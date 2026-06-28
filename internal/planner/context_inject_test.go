package planner

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
)

// TestFormatCallChains_RendersKaiCodeChain is the formatter side of
// the v1 regression: given the resolved kai-code call chain, the
// rendered text should contain the soft-escape clause and each
// expected function name in tree form, with file paths.
func TestFormatCallChains_RendersKaiCodeChain(t *testing.T) {
	chain := CallChain{
		Entry: ResolvedEntryPoint{
			Token:       "kai code",
			HandlerName: "runCodeTUI",
			FilePath:    "cmd/kai/tui.go",
			Stage:       StageCommand,
		},
		Nodes: []CallChainNode{
			{ShortName: "runCodeTUI", FullName: "runCodeTUI", FilePath: "cmd/kai/tui.go", Depth: 0},
			{ShortName: "Discover", FullName: "projects.Discover", FilePath: "internal/projects/discover.go", Depth: 1},
			{ShortName: "locateProjectsFileRoot", FullName: "projects.locateProjectsFileRoot", FilePath: "internal/projects/discover.go", Depth: 2},
			{ShortName: "Primary", FullName: "projects.Set.Primary", FilePath: "internal/projects/set.go", Depth: 1},
			{ShortName: "Getwd", FullName: "os.Getwd", Depth: 1, NoteOnly: true},
		},
	}

	out := FormatCallChains([]CallChain{chain})

	// Soft-escape clause is present (top of every injection).
	if !strings.Contains(out, "starting hint, not a constraint") {
		t.Errorf("output missing soft-escape clause:\n%s", out)
	}

	for _, want := range []string{
		"kai code",
		"runCodeTUI",
		"projects.Discover",
		"projects.locateProjectsFileRoot",
		"projects.Set.Primary",
		"os.Getwd",
		"stdlib, not expanded",
		"cmd/kai/tui.go",
		"internal/projects/discover.go",
		"internal/projects/set.go",
		"via command index",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing expected token %q:\n%s", want, out)
		}
	}
}

// TestFormatCallChains_EmptyChains: zero input produces zero output.
// Callers use this to decide whether to skip injection entirely.
func TestFormatCallChains_EmptyChains(t *testing.T) {
	if got := FormatCallChains(nil); got != "" {
		t.Errorf("nil chains should format to empty, got: %q", got)
	}
	if got := FormatCallChains([]CallChain{}); got != "" {
		t.Errorf("empty chains should format to empty, got: %q", got)
	}
}

// TestFormatCallChains_FileOnlyEntry: an entry with no Symbol gets
// a "file:" line rather than an empty tree.
func TestFormatCallChains_FileOnlyEntry(t *testing.T) {
	chain := CallChain{
		Entry: ResolvedEntryPoint{
			Token:    "set.go",
			FilePath: "internal/projects/set.go",
			Stage:    StageFile,
		},
	}
	out := FormatCallChains([]CallChain{chain})
	if !strings.Contains(out, "file: internal/projects/set.go") {
		t.Errorf("file-only entry should render with file: line, got:\n%s", out)
	}
}

// TestFormatCallChains_TruncatedHint: chains marked Truncated get a
// "more callees not shown" footer so the user knows the output is
// partial.
func TestFormatCallChains_TruncatedHint(t *testing.T) {
	chain := CallChain{
		Entry: ResolvedEntryPoint{Token: "x", Stage: StageSymbol},
		Nodes: []CallChainNode{
			{ShortName: "x", FullName: "x", Depth: 0},
		},
		Truncated: true,
	}
	out := FormatCallChains([]CallChain{chain})
	if !strings.Contains(out, "truncated at the per-request cap") {
		t.Errorf("truncated chain should surface the cap message, got:\n%s", out)
	}
}

// TestBuildInjectedContext_FullPipelineKaiCode is the regression
// criterion for v1: given the user's exact request from the bug
// report, the end-to-end pipeline produces text containing the
// expected chain.
func TestBuildInjectedContext_FullPipelineKaiCode(t *testing.T) {
	g := newEPGraph()
	runCode := g.addSymbol("sym:runCodeTUI", "runCodeTUI")
	g.addSymbol("sym:Discover", "projects.Discover")
	g.addSymbol("sym:Primary", "projects.Set.Primary")
	_ = runCode

	g.addFile("cmd/kai/tui.go")
	g.addFile("internal/projects/discover.go")
	g.addFile("internal/projects/set.go")
	g.addDefines("sym:runCodeTUI", "cmd/kai/tui.go")
	g.addDefines("sym:Discover", "internal/projects/discover.go")
	g.addDefines("sym:Primary", "internal/projects/set.go")

	g.addCalls("sym:runCodeTUI", "sym:Discover")
	g.addCalls("sym:runCodeTUI", "sym:Primary")

	cmds := &CommandIndex{
		commands:     map[string]string{"code": "runCodeTUI"},
		handlerFiles: map[string]string{"runCodeTUI": "cmd/kai/tui.go"},
	}

	request := "I ran `kai code` from kai-server and it opened the wrong project"
	got := BuildInjectedContext(request, g, cmds)

	for _, want := range []string{
		"kai code",
		"runCodeTUI",
		"projects.Discover",
		"projects.Set.Primary",
		"via command index",
		"starting hint, not a constraint",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("injection missing %q. Full output:\n%s", want, got)
		}
	}
}

// TestBuildInjectedContext_NoMatchesReturnsEmpty: a request with no
// code-shaped tokens (or with tokens that resolve nowhere) produces
// empty output so the caller skips injection.
func TestBuildInjectedContext_NoMatchesReturnsEmpty(t *testing.T) {
	g := newEPGraph()
	cmds := &CommandIndex{}

	cases := []string{
		"the page is broken",
		"please fix the homepage",
		"",
		"thingThatIsNotInGraph somewhere",
	}
	for _, req := range cases {
		if got := BuildInjectedContext(req, g, cmds); got != "" {
			t.Errorf("BuildInjectedContext(%q) = %q, want empty", req, got)
		}
	}
}

// epGraphForFormat is a sanity check that the GraphAccess interface
// is satisfied by the test fake — guards against silently breaking
// the test harness when GraphAccess gets a new method.
var _ GraphAccess = (*epGraph)(nil)

// silence unused-import warning if `graph` is removed from this
// file in the future.
var _ = graph.KindSymbol
