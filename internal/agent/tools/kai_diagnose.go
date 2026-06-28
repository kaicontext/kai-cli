package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kaicontext/kai-engine/graph"
)

// kaiDiagnoseTool is a single-call replacement for "read 30 files chasing
// a runtime error." The agent passes the error message (and optionally a
// file:line from a stack trace) and the tool returns a graph-grounded
// report: which symbol the error names, where that symbol is defined
// (or "nowhere — that's the bug"), what the named file imports, what
// imports the named file, and a one-line hypothesis the agent can use
// to pick its next action.
//
// Why this exists: agents seeing "X is not defined" tend to read the
// whole project before forming a hypothesis. The graph already knows
// where every symbol lives — one query collapses 30 view calls into
// a structured answer. The tool is the chokepoint that makes "what
// does the codebase actually say about this error" a single tool
// dispatch instead of an open-ended exploration.
type kaiDiagnoseTool struct {
	db kaiDiagnoseGrapher
}

// kaiDiagnoseGrapher is the slice of *graph.DB this tool needs. Aliased
// to KaiGrapher for clarity at the call site — same surface as the
// other graph-backed tools in this package.
type kaiDiagnoseGrapher = KaiGrapher

type kaiDiagnoseParams struct {
	// Error is the full error message, ideally including the symbol
	// name. Examples: "ReferenceError: getCollection is not defined",
	// "TypeError: foo.bar is not a function", "ImportError: cannot
	// import name 'baz'". Required.
	Error string `json:"error"`
	// File, when set, is the workspace-relative path the error names
	// (the top frame of a stack trace, the failing test's file). When
	// the agent can extract this from the error or the user's paste,
	// the diagnosis includes that file's imports + dependents — turns
	// a one-symbol question into a graph-anchored one.
	File string `json:"file"`
	// Line is optional — the line number from the stack trace. Not
	// used directly today; reserved so callers can pass everything
	// they have without the schema rejecting it.
	Line int `json:"line"`
}

func (t *kaiDiagnoseTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_diagnose",
		Description: "Diagnose a runtime error against the kai graph. Pass the error " +
			"message (and the file from the stack trace if you have it) and get back: " +
			"the symbol the error names, where it's defined (or that it isn't), what " +
			"the named file imports, what imports the named file, and a hypothesis. " +
			"Use this BEFORE reading source files when you have an error — it answers " +
			"'where does this come from' in one call instead of 20 view dispatches.",
		Parameters: map[string]any{
			"error": map[string]any{
				"type":        "string",
				"description": "The full error message. Include the symbol name if visible (e.g. \"ReferenceError: getCollection is not defined\").",
			},
			"file": map[string]any{
				"type":        "string",
				"description": "Optional workspace-relative file from the stack trace (e.g. \"apps/web/pages/index.astro\").",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "Optional line number from the stack trace. Unused today; pass it for forward-compat.",
			},
		},
		Required: []string{"error"},
	}
}

func (t *kaiDiagnoseTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiDiagnoseParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_diagnose: invalid input json: " + err.Error()), nil
		}
	}
	if strings.TrimSpace(p.Error) == "" {
		return NewTextErrorResponse("kai_diagnose: error message required"), nil
	}

	symbol := extractSymbol(p.Error)
	var b strings.Builder
	fmt.Fprintf(&b, "kai_diagnose:\n")
	fmt.Fprintf(&b, "  error: %s\n", oneLine(p.Error))

	// Symbol lookup. If we couldn't extract one, say so explicitly —
	// the agent should know whether the rest of the report is about
	// "X" specifically or just file-scoped context.
	if symbol == "" {
		b.WriteString("  symbol: <couldn't extract from error message>\n")
	} else {
		fmt.Fprintf(&b, "  symbol: %s\n", symbol)
		defs := findSymbolDefinitions(t.db, symbol)
		switch len(defs) {
		case 0:
			fmt.Fprintf(&b, "  defined: NOWHERE in the indexed codebase — this is likely the bug. Either an import is missing, or the symbol was deleted/renamed.\n")
		case 1:
			fmt.Fprintf(&b, "  defined: %s\n", defs[0])
		default:
			fmt.Fprintf(&b, "  defined: %d locations:\n", len(defs))
			for _, d := range defs {
				fmt.Fprintf(&b, "    - %s\n", d)
			}
		}
	}

	// File-scoped context. Even if the symbol lookup turned up
	// nothing, naming the file's imports + dependents helps the
	// agent pick its next move (read THIS file, not 30 others).
	if strings.TrimSpace(p.File) != "" {
		fmt.Fprintf(&b, "  file: %s\n", p.File)
		imports := importsOfFile(t.db, p.File)
		if len(imports) == 0 {
			b.WriteString("    imports: (none indexed — file may not be parsed, or really imports nothing)\n")
		} else {
			fmt.Fprintf(&b, "    imports (%d):\n", len(imports))
			for _, imp := range imports {
				fmt.Fprintf(&b, "      - %s\n", imp)
			}
		}
		dependents, _ := dependentsOfFile(t.db, p.File)
		if len(dependents) > 0 {
			fmt.Fprintf(&b, "    imported by (%d):\n", len(dependents))
			for _, d := range dependents {
				fmt.Fprintf(&b, "      - %s\n", d)
			}
		}
	}

	// Hypothesis line. Cheap rule-based synthesis from what we
	// learned above — gives the agent a starting point so it doesn't
	// re-derive the same conclusion. Conservative wording: "likely",
	// not "is", because the graph has blind spots (dynamic imports,
	// CommonJS require, etc.) and we don't want the agent treating
	// the hypothesis as proof.
	hypothesis := buildHypothesis(t.db, symbol, p.File, p.Error)
	if hypothesis != "" {
		fmt.Fprintf(&b, "  hypothesis: %s\n", hypothesis)
	}

	return NewTextResponse(b.String()), nil
}

// extractSymbol pulls the offending identifier out of common runtime
// error shapes. Returns "" when nothing matches — better than guessing
// since downstream logic switches on empty.
//
// Patterns covered (ordered most specific → most general):
//
//	ReferenceError: X is not defined           → X
//	X is not defined                           → X
//	X is not a function                        → X (last token of dotted chain)
//	TypeError: Cannot read properties of undefined (reading 'X')  → X
//	ImportError: cannot import name 'X'        → X
//	NameError: name 'X' is not defined         → X (Python)
//	cannot find symbol X                       → X (Java/Go-style)
//	undefined: X                               → X (Go)
//	undefined reference to `X'                 → X (linker)
func extractSymbol(msg string) string {
	for _, re := range symbolExtractRegexes {
		if m := re.FindStringSubmatch(msg); len(m) > 1 {
			s := strings.TrimSpace(m[1])
			// "foo.bar is not a function" → return "bar" (the
			// missing/wrong leaf). The full chain isn't queryable
			// in the symbol graph; the leaf usually is.
			if i := strings.LastIndex(s, "."); i >= 0 {
				s = s[i+1:]
			}
			return s
		}
	}
	return ""
}

var symbolExtractRegexes = []*regexp.Regexp{
	// "X is not defined" — JS ReferenceError.
	regexp.MustCompile(`\b([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s+is\s+not\s+defined\b`),
	// "X is not a function" — JS TypeError on call site.
	regexp.MustCompile(`\b([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s+is\s+not\s+a\s+function\b`),
	// "Cannot read properties of undefined (reading 'X')" / older
	// "Cannot read property 'X' of undefined".
	regexp.MustCompile(`reading\s+['"]([\w$]+)['"]`),
	regexp.MustCompile(`Cannot\s+read\s+property\s+['"]([\w$]+)['"]`),
	// Python-flavored.
	regexp.MustCompile(`name\s+['"]([\w]+)['"]\s+is\s+not\s+defined`),
	regexp.MustCompile(`cannot\s+import\s+name\s+['"]([\w]+)['"]`),
	// Java / generic compile.
	regexp.MustCompile(`cannot\s+find\s+symbol[:\s]+([A-Za-z_][\w]*)`),
	// Go: "undefined: X".
	regexp.MustCompile(`undefined:\s*([A-Za-z_][\w.]*)`),
	// Linker: "undefined reference to `X'".
	regexp.MustCompile(`undefined\s+reference\s+to\s+` + "`" + `([\w:]+)`),
}

// findSymbolDefinitions walks the graph for any symbol node whose
// fqName ends with the requested name. Returns "file:line" strings
// for each definition site, sorted for stable output.
//
// Why fqName-suffix matching: the graph stores symbols with their
// containing-package qualifier ("foo.bar.Baz"); the error message
// usually has just the leaf ("Baz"). Suffix-match handles both.
func findSymbolDefinitions(db KaiGrapher, symbol string) []string {
	if symbol == "" {
		return nil
	}
	target := normalizeSymbolName(symbol)
	// FindNodesByPayloadPath is keyed on the literal payload value;
	// we don't have a fqName index. So scan all DEFINES_IN edges and
	// filter by payload — bounded by the graph size, fine for
	// interactive use. If this gets hot, add a name index.
	edges, err := db.GetEdgesOfType(graph.EdgeDefinesIn)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range edges {
		sym, err := db.GetNode(e.Src)
		if err != nil || sym == nil {
			continue
		}
		name, _ := sym.Payload["fqName"].(string)
		leaf := name
		if i := strings.LastIndex(leaf, "."); i >= 0 {
			leaf = leaf[i+1:]
		}
		if normalizeSymbolName(leaf) != target {
			continue
		}
		fileNode, err := db.GetNode(e.Dst)
		if err != nil || fileNode == nil {
			continue
		}
		filePath, _ := fileNode.Payload["path"].(string)
		if filePath == "" {
			continue
		}
		line := 0
		if l, ok := sym.Payload["line"].(float64); ok {
			line = int(l)
		}
		key := fmt.Sprintf("%s:%d", filePath, line)
		if seen[key] {
			continue
		}
		seen[key] = true
		if line > 0 {
			out = append(out, fmt.Sprintf("%s:%d (%s)", filePath, line, name))
		} else {
			out = append(out, fmt.Sprintf("%s (%s)", filePath, name))
		}
	}
	sort.Strings(out)
	return out
}

// importsOfFile returns the workspace-relative paths the given file
// imports. Mirrors callersOfFile but on the outbound edge direction.
// Returns nil when the file isn't indexed.
func importsOfFile(db KaiGrapher, filePath string) []string {
	if filePath == "" {
		return nil
	}
	fileNodes, err := db.FindNodesByPayloadPath(string(graph.KindFile), filePath)
	if err != nil || len(fileNodes) == 0 {
		return nil
	}
	// We want outbound IMPORTS edges from this file. The store's
	// GetEdgesByDst lookups are by destination, not source — so we
	// pull all IMPORTS and filter to ones whose Src is our file.
	all, err := db.GetEdgesOfType(graph.EdgeImports)
	if err != nil {
		return nil
	}
	srcID := fileNodes[0].ID
	out := map[string]bool{}
	for _, e := range all {
		if !bytesEqual(e.Src, srcID) {
			continue
		}
		dst, err := db.GetNode(e.Dst)
		if err != nil || dst == nil {
			continue
		}
		p, _ := dst.Payload["path"].(string)
		if p != "" && p != filePath {
			out[p] = true
		}
	}
	res := make([]string, 0, len(out))
	for k := range out {
		res = append(res, k)
	}
	sort.Strings(res)
	return res
}

// buildHypothesis composes a one-line plain-language guess from what
// the graph and the error tell us. Conservative — every clause uses
// "likely" / "appears" because the graph has blind spots (dynamic
// imports, runtime require, generated code) and we don't want the
// agent to treat the hypothesis as a proof.
func buildHypothesis(db KaiGrapher, symbol, file, errMsg string) string {
	low := strings.ToLower(errMsg)
	switch {
	case symbol != "":
		defs := findSymbolDefinitions(db, symbol)
		if len(defs) == 0 {
			return fmt.Sprintf("%q isn't defined anywhere in the indexed codebase. Likely either (a) a missing import in %s, (b) the symbol comes from a third-party package not in the graph, or (c) it was deleted/renamed and the call site wasn't updated.", symbol, fallback(file, "the failing file"))
		}
		if file != "" {
			imports := importsOfFile(db, file)
			defFiles := defFilesFromDefinitions(defs)
			if !anyImportSatisfies(imports, defFiles) {
				return fmt.Sprintf("%q is defined at %s, but %s does NOT appear to import any of those files. Likely a missing import — add the import statement and the error should resolve.", symbol, joinTop(defs, 3), file)
			}
		}
		return fmt.Sprintf("%q is defined at %s. The import is in place; the bug is likely in HOW it's called (wrong arguments, wrong this-binding, async-not-awaited).", symbol, joinTop(defs, 3))
	case strings.Contains(low, "module not found") || strings.Contains(low, "cannot find module"):
		return "Module-resolution failure. Likely a typo in the import path, a package not yet installed, or a tsconfig/jsconfig path alias that isn't wired in the bundler. Check package.json, then the tsconfig 'paths' field."
	case strings.Contains(low, "syntaxerror"):
		return "Syntax error — the runtime is rejecting the source as unparseable. The file:line in the trace IS the answer; read just that line."
	}
	return ""
}

// defFilesFromDefinitions strips the "(symbol)" tail and "line" suffix
// from the strings findSymbolDefinitions emits, leaving just the file
// path. Used for the "imports satisfy definition" check.
func defFilesFromDefinitions(defs []string) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		// Format is "path:line (fqName)" or "path (fqName)".
		s := d
		if i := strings.Index(s, " ("); i >= 0 {
			s = s[:i]
		}
		if i := strings.LastIndex(s, ":"); i >= 0 {
			// Strip ":<digits>" only — paths can contain ":" on Windows
			// but the index walker normalizes those already.
			tail := s[i+1:]
			isNum := tail != ""
			for _, r := range tail {
				if r < '0' || r > '9' {
					isNum = false
					break
				}
			}
			if isNum {
				s = s[:i]
			}
		}
		out = append(out, s)
	}
	return out
}

// anyImportSatisfies is a coarse "does the importer reach any of the
// definers" check. We don't normalize package-relative imports against
// alias roots — too much project-specific config — so this is a hint,
// not a proof. False negatives are fine; the hypothesis just won't
// fire and the report still shows imports/defs side-by-side.
func anyImportSatisfies(imports, defFiles []string) bool {
	if len(imports) == 0 || len(defFiles) == 0 {
		return false
	}
	for _, def := range defFiles {
		for _, imp := range imports {
			if imp == def {
				return true
			}
			// Tolerate extension differences (foo vs foo.ts) and
			// trailing /index — the kai indexer normalizes most
			// of this already, but bundler-style imports often
			// drop the extension.
			if strings.HasPrefix(imp, def) || strings.HasPrefix(def, imp) {
				return true
			}
		}
	}
	return false
}

func joinTop(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" (+%d more)", len(items)-n)
}

func fallback(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

// bytesEqual avoids importing "bytes" just for one comparison. Same
// shape as bytes.Equal; kept local because graph IDs are short and
// we compare in a hot loop.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
