package planner

import (
	"strings"

	"github.com/kaicontext/kai-engine/graph"
)

// ResolvedEntryPoint is one successful lookup against the graph.
// Symbol is the node the call-chain walker will trace forward from
// (nil only if File was resolved without a symbol — a path-only
// mention with no obvious entry function). FilePath is the file the
// symbol lives in (or the file itself, for path-only resolutions).
// Origin echoes how the token was classified by ExtractEntryPointTokens
// — useful for debug formatting and for tests that need to verify
// which stage matched.
type ResolvedEntryPoint struct {
	// Token is the original code-shaped substring from the user.
	Token string

	// Symbol is the entry-point node in the semantic graph. May be
	// nil for path-only resolutions (the file matched but no
	// specific function was identified).
	Symbol *graph.Node

	// FilePath is the path of the file containing Symbol, or the
	// path of the file itself when Symbol is nil.
	FilePath string

	// HandlerName is set for command-index resolutions: the cobra
	// handler function name (e.g. "runCodeTUI"). Empty for direct
	// symbol or file resolutions.
	HandlerName string

	// Stage records which lookup stage matched. Useful for the
	// formatter's "via command index" / "via symbol index" labels.
	Stage LookupStage
}

// LookupStage identifies which stage of the pipeline produced a
// resolution. The pipeline tries stages in order; the first match
// wins. Recorded on each ResolvedEntryPoint for the formatter and
// for stage-coverage metrics.
type LookupStage int

const (
	StageCommand LookupStage = iota
	StageSymbol
	StageFile
)

// ResolveEntryPoints runs the three-stage lookup pipeline against
// the supplied graph and command index. Returns one entry per
// successfully-resolved token, in token order. Tokens that resolve
// at multiple stages (e.g. "code" hits both a cobra command and a
// function named "code") use the first-match stage and don't double-
// emit. Tokens that resolve nowhere are silently dropped — they
// pass through as plain text in the formatted output upstream.
//
// The pipeline:
//
//  1. Command index — most specific. Backtick tokens get priority
//     here because they're the user's explicit signal of "this is a
//     thing in the system."
//  2. Symbol index — fqName or trailing-component match. Same rule
//     as resolveSymbolFiles in plan.go.
//  3. File index — substring match against file paths. Last resort.
//
// All three stages can run for any origin — origin just biases the
// order. A backtick token can still resolve at the symbol stage if
// the command index misses, and a CamelCase token can resolve at
// the file stage if no symbol matches (unusual but possible).
func ResolveEntryPoints(tokens []EntryPointToken, g GraphAccess, cmds *CommandIndex) []ResolvedEntryPoint {
	if g == nil {
		return nil
	}
	var out []ResolvedEntryPoint
	seenSymbol := map[string]bool{}

	// Build lazy lookups for the symbol and file passes. Cached
	// across tokens because the same graph queries fan out across
	// every token; computing them once per Resolve call keeps the
	// per-token cost in O(1) amortized.
	symbolByName := buildSymbolNameIndex(g)
	symbolFile := buildSymbolFileIndex(g)
	fileByPath := buildFileIndex(g)

	for _, tok := range tokens {
		raw := strings.TrimSpace(tok.Raw)
		if raw == "" {
			continue
		}

		// Stage 1: command index. Always tried first — false
		// positives here are bounded (the command must literally
		// match a cobra Use field) so it's cheap to attempt for
		// every token.
		if handler, file := cmds.LookupCommand(raw); handler != "" {
			sym := symbolByName[handler]
			if sym == nil {
				sym = symbolByName[strings.ToLower(handler)]
			}
			if sym != nil && !seenSymbol[handler] {
				seenSymbol[handler] = true
				resolvedFile := file
				if symbolFile != nil {
					if f, ok := symbolFile[string(sym.ID)]; ok && f != "" {
						resolvedFile = f
					}
				}
				out = append(out, ResolvedEntryPoint{
					Token:       tok.Raw,
					Symbol:      sym,
					FilePath:    resolvedFile,
					HandlerName: handler,
					Stage:       StageCommand,
				})
				continue
			}
			// Command matched but symbol node missing from the
			// graph — emit a file-only resolution so the formatter
			// can still surface "command X handled in file Y."
			if file != "" {
				out = append(out, ResolvedEntryPoint{
					Token:       tok.Raw,
					FilePath:    file,
					HandlerName: handler,
					Stage:       StageCommand,
				})
				continue
			}
		}

		// Stage 2: symbol index. fqName or trailing component
		// match. Lowercased so case differences between user's
		// prose and code don't break the match.
		key := strings.ToLower(raw)
		if sym := symbolByName[key]; sym != nil {
			fq, _ := sym.Payload["fqName"].(string)
			if seenSymbol[fq] {
				continue
			}
			seenSymbol[fq] = true
			file := ""
			if symbolFile != nil {
				file = symbolFile[string(sym.ID)]
			}
			out = append(out, ResolvedEntryPoint{
				Token:    tok.Raw,
				Symbol:   sym,
				FilePath: file,
				Stage:    StageSymbol,
			})
			continue
		}

		// Stage 3: file index. Path-shaped tokens prefer this
		// stage; non-path tokens may still hit it if the token
		// happens to be a filename (e.g. "discover" matching
		// "discover.go" by basename).
		if file := lookupFile(fileByPath, raw); file != nil {
			path, _ := file.Payload["path"].(string)
			out = append(out, ResolvedEntryPoint{
				Token:    tok.Raw,
				FilePath: path,
				Stage:    StageFile,
			})
			continue
		}
	}
	return out
}

// buildSymbolNameIndex enumerates KindSymbol nodes and indexes them
// by fqName (lowercased) and by trailing component. Returns a map
// suitable for O(1) lookup per token. Returns nil if the graph has
// no symbols — caller falls through to the file stage.
func buildSymbolNameIndex(g GraphAccess) map[string]*graph.Node {
	nodes, err := g.GetNodesByKind(graph.KindSymbol)
	if err != nil || len(nodes) == 0 {
		return nil
	}
	out := make(map[string]*graph.Node, len(nodes)*2)
	for _, n := range nodes {
		fq, _ := n.Payload["fqName"].(string)
		if fq == "" {
			continue
		}
		lo := strings.ToLower(fq)
		if _, exists := out[lo]; !exists {
			out[lo] = n
		}
		trailing := plannerTrailing(fq)
		if trailing != fq {
			tlo := strings.ToLower(trailing)
			if _, exists := out[tlo]; !exists {
				out[tlo] = n
			}
		}
		// Also index by the original-case forms so the cobra-handler
		// lookup can find the symbol by its handler name verbatim
		// (e.g. "runCodeTUI"). Original-case write doesn't overwrite
		// lowercase entries.
		if _, exists := out[fq]; !exists {
			out[fq] = n
		}
		if trailing != fq {
			if _, exists := out[trailing]; !exists {
				out[trailing] = n
			}
		}
	}
	return out
}

// buildSymbolFileIndex maps symbol-node-id → defining-file-path via
// the DEFINES_IN edges. Built once per ResolveEntryPoints call.
func buildSymbolFileIndex(g GraphAccess) map[string]string {
	edges, err := g.GetEdgesOfType(graph.EdgeDefinesIn)
	if err != nil || len(edges) == 0 {
		return nil
	}
	out := make(map[string]string, len(edges))
	for _, e := range edges {
		dst, err := g.GetNode(e.Dst)
		if err != nil || dst == nil {
			continue
		}
		path, _ := dst.Payload["path"].(string)
		if path == "" {
			continue
		}
		out[string(e.Src)] = path
	}
	return out
}

// buildFileIndex collects file nodes keyed by their path. Used by
// stage 3 for path-shaped lookups.
func buildFileIndex(g GraphAccess) map[string]*graph.Node {
	nodes, err := g.GetNodesByKind(graph.KindFile)
	if err != nil || len(nodes) == 0 {
		return nil
	}
	out := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		p, _ := n.Payload["path"].(string)
		if p == "" {
			continue
		}
		out[p] = n
	}
	return out
}

// lookupFile resolves a token against the file index. Tries exact
// path match first, then basename match (so "set.go" hits any file
// whose basename is "set.go"). Returns nil on miss.
func lookupFile(idx map[string]*graph.Node, token string) *graph.Node {
	if idx == nil {
		return nil
	}
	if n, ok := idx[token]; ok {
		return n
	}
	// Basename pass — slow when basenames collide across packages,
	// but that's rare enough in practice that we don't bother
	// pre-indexing the basenames. First match wins; for ambiguous
	// basenames the user should be more specific.
	for path, n := range idx {
		base := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			base = path[i+1:]
		}
		if base == token {
			return n
		}
	}
	return nil
}
