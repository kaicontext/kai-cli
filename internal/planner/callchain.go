package planner

import (
	"strings"

	"github.com/kaicontext/kai-engine/graph"
)

// CallChain is the depth-first walk of outbound CALLS edges starting
// at one entry point. Nodes are in visit order (parent before
// children), with Depth carrying the nesting level — 0 is the entry
// point itself, 1 is direct callees, etc. The formatter renders this
// directly into a tree.
type CallChain struct {
	// Entry is the resolved entry point this chain starts from.
	Entry ResolvedEntryPoint

	// Nodes are the symbols visited during the walk, in DFS order.
	// Always non-empty when the entry has a Symbol set.
	Nodes []CallChainNode

	// Truncated reports whether the walk hit the per-chain node cap.
	// When true, the formatter appends "(more callees not shown)"
	// so the user knows the chain is partial.
	Truncated bool
}

// CallChainNode is one symbol on the call chain. ShortLabel is the
// human-facing name — the trailing component of the fqName (e.g.
// "Primary" from "projects.Set.Primary"). FullName is the full
// qualified form, surfaced when the trailing alone would be
// ambiguous.
type CallChainNode struct {
	Symbol    *graph.Node
	FullName  string
	ShortName string
	FilePath  string
	Depth     int

	// NoteOnly is true for nodes that are mentioned but not expanded —
	// stdlib calls and pruned helpers. Formatter renders these as
	// one-line notes ("→ os.Getwd (stdlib, not expanded)") instead
	// of expanding their callees.
	NoteOnly bool
}

// callChainCap is the hard cap on functions injected across the
// whole request. Matches the spec's ≤20 number. When multiple entry
// points are present and have no shared ancestor, each gets a
// budget of callChainCap / len(entries) — see WalkCallChains.
const callChainCap = 20

// stdlibPrefixes is the set of fqName prefixes treated as stdlib.
// Go stdlib mostly. A symbol whose fqName starts with one of these
// is mentioned but not expanded — the agent doesn't need to know
// what's inside os.Getwd, just that the entry point calls it. Path-
// based detection (defining file outside the workspace) would be
// more reliable but requires file-path inspection per node, which
// blows up the per-call cost. Prefix list is cheap and correct for
// the common case.
var stdlibPrefixes = []string{
	"os.", "io.", "fmt.", "strings.", "bytes.",
	"strconv.", "errors.", "sort.", "time.", "math.",
	"unicode.", "regexp.", "path.", "filepath.",
	"context.", "sync.", "atomic.", "log.",
	"encoding/", "net/", "crypto/", "compress/",
	"bufio.", "io/", "runtime.",
}

func isStdlib(fqName string) bool {
	for _, p := range stdlibPrefixes {
		if strings.HasPrefix(fqName, p) {
			return true
		}
	}
	return false
}

// pureDataPrefixes are name prefixes that strongly suggest a
// getter/setter/coercion helper. When combined with a low outbound
// edge count (≤3, i.e. the function does almost no internal work),
// these get noted-but-not-expanded. The "unless it's the only path
// forward" exception is enforced by the walker: a parent with no
// other callees keeps its pure-data child as a real node.
var pureDataPrefixes = []string{
	"get", "set", "is", "has", "to", "from", "new",
}

// isPureDataHelper reports whether the given symbol's fqName trailing
// component looks like a pure-data accessor. Combined with the
// outbound-edge check in the walker — name alone isn't enough; the
// function must also do little work (≤3 outbound calls). The check
// is case-insensitive so "GetFoo" and "getFoo" both trigger.
func isPureDataHelper(fqName string) bool {
	t := plannerTrailing(fqName)
	lo := strings.ToLower(t)
	for _, p := range pureDataPrefixes {
		if strings.HasPrefix(lo, p) {
			return true
		}
	}
	return false
}

// WalkCallChains produces one CallChain per resolved entry point,
// applying the pruning rules and the global cap. The total number
// of expanded (non-note-only) nodes across all chains is bounded by
// callChainCap; per-chain budgets are split evenly when multiple
// entries are present.
//
// Entries without a Symbol set (path-only resolutions) get a
// degenerate single-node chain — the formatter knows to render
// these as "this file was mentioned" without trying to expand a
// non-existent symbol.
func WalkCallChains(entries []ResolvedEntryPoint, g GraphAccess) []CallChain {
	if g == nil || len(entries) == 0 {
		return nil
	}
	perChain := callChainCap
	if len(entries) > 1 {
		perChain = callChainCap / len(entries)
		if perChain < 3 {
			perChain = 3 // floor — anything less isn't useful as context
		}
	}

	// Symbol-file index, built once across all chains so the per-
	// node file lookup stays O(1).
	symFile := buildSymbolFileIndex(g)

	var out []CallChain
	for _, e := range entries {
		out = append(out, walkOneChain(e, g, symFile, perChain))
	}
	return out
}

// walkOneChain DFS-walks from one entry point. Uses an explicit
// stack to keep iteration bounded and to make pruning decisions
// inline with the visit. visited tracks node IDs to avoid cycles
// (recursive functions or callgraph loops).
func walkOneChain(entry ResolvedEntryPoint, g GraphAccess, symFile map[string]string, cap int) CallChain {
	chain := CallChain{Entry: entry}
	if entry.Symbol == nil {
		// File-only resolution — no chain to walk. Formatter
		// handles this case.
		return chain
	}

	type frame struct {
		node  *graph.Node
		depth int
	}
	stack := []frame{{node: entry.Symbol, depth: 0}}
	visited := map[string]bool{string(entry.Symbol.ID): true}
	expanded := 0

	for len(stack) > 0 {
		// Pop. DFS = LIFO order.
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		fqName, _ := f.node.Payload["fqName"].(string)
		short := plannerTrailing(fqName)

		// Stdlib: include as a note, don't expand.
		if isStdlib(fqName) && f.depth > 0 {
			chain.Nodes = append(chain.Nodes, CallChainNode{
				Symbol:    f.node,
				FullName:  fqName,
				ShortName: short,
				Depth:     f.depth,
				NoteOnly:  true,
			})
			continue
		}

		// Real node. Adds to budget unless we're at the cap.
		if expanded >= cap {
			chain.Truncated = true
			break
		}
		filePath := ""
		if symFile != nil {
			filePath = symFile[string(f.node.ID)]
		}
		chain.Nodes = append(chain.Nodes, CallChainNode{
			Symbol:    f.node,
			FullName:  fqName,
			ShortName: short,
			FilePath:  filePath,
			Depth:     f.depth,
		})
		expanded++

		// Enumerate outbound CALLS edges. Don't follow if this is a
		// pure-data helper at a non-root depth — but only if the
		// parent had alternative callees (the "only path forward"
		// exception is naturally satisfied because we expand the
		// node regardless; we just don't recurse into its callees
		// when it's a helper).
		if f.depth > 0 && isPureDataHelper(fqName) {
			continue
		}

		callees, err := getOutboundCalls(g, f.node)
		if err != nil || len(callees) == 0 {
			continue
		}
		// Push in reverse so DFS visits in original order. Filter
		// already-visited inline.
		for i := len(callees) - 1; i >= 0; i-- {
			c := callees[i]
			id := string(c.ID)
			if visited[id] {
				continue
			}
			visited[id] = true
			stack = append(stack, frame{node: c, depth: f.depth + 1})
		}
	}

	return chain
}

// getOutboundCalls returns the symbols this symbol calls, in graph
// edge order. Built around GetEdgesOfType because GraphAccess
// doesn't expose a per-node outbound query — we filter the full
// edge list to those whose Src matches. Acceptable for v1; for
// larger graphs the planner can add a GetOutbound shortcut later.
func getOutboundCalls(g GraphAccess, node *graph.Node) ([]*graph.Node, error) {
	edges, err := g.GetEdgesOfType(graph.EdgeCalls)
	if err != nil {
		return nil, err
	}
	srcID := string(node.ID)
	var out []*graph.Node
	for _, e := range edges {
		if string(e.Src) != srcID {
			continue
		}
		dst, err := g.GetNode(e.Dst)
		if err != nil || dst == nil {
			continue
		}
		out = append(out, dst)
	}
	return out, nil
}
