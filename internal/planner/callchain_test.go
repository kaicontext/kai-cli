package planner

import (
	"testing"

	"github.com/kaicontext/kai-engine/graph"
)

// addCalls is a fake-graph helper for wiring up CALLS edges in
// tests. Mutates the receiver's calls slice and the dst->src
// reverse index isn't needed — getOutboundCalls just filters the
// edge list.
func (g *epGraph) addCalls(srcID, dstID string) {
	g.calls = append(g.calls, &graph.Edge{
		Src: []byte(srcID),
		Dst: []byte(dstID),
	})
}

// TestWalkCallChains_KaiCodeScenario is the v1 success criterion:
// starting from runCodeTUI, the walker should produce a chain that
// includes Discover, locateProjectsFileRoot, and Set.Primary —
// exactly what a developer reading the source would see.
func TestWalkCallChains_KaiCodeScenario(t *testing.T) {
	g := newEPGraph()
	runCode := g.addSymbol("sym:runCodeTUI", "runCodeTUI")
	g.addSymbol("sym:Discover", "projects.Discover")
	g.addSymbol("sym:locateProjectsFileRoot", "projects.locateProjectsFileRoot")
	g.addSymbol("sym:Primary", "projects.Set.Primary")
	g.addSymbol("sym:Getwd", "os.Getwd")

	g.addFile("cmd/kai/tui.go")
	g.addFile("internal/projects/discover.go")
	g.addFile("internal/projects/set.go")
	g.addDefines("sym:runCodeTUI", "cmd/kai/tui.go")
	g.addDefines("sym:Discover", "internal/projects/discover.go")
	g.addDefines("sym:locateProjectsFileRoot", "internal/projects/discover.go")
	g.addDefines("sym:Primary", "internal/projects/set.go")

	g.addCalls("sym:runCodeTUI", "sym:Getwd")
	g.addCalls("sym:runCodeTUI", "sym:Discover")
	g.addCalls("sym:runCodeTUI", "sym:Primary")
	g.addCalls("sym:Discover", "sym:locateProjectsFileRoot")

	entries := []ResolvedEntryPoint{{
		Token:    "kai code",
		Symbol:   runCode,
		FilePath: "cmd/kai/tui.go",
		Stage:    StageCommand,
	}}
	chains := WalkCallChains(entries, g)
	if len(chains) != 1 {
		t.Fatalf("got %d chains, want 1", len(chains))
	}
	chain := chains[0]

	// Build a name set for assertion. NoteOnly nodes count too —
	// the agent benefits from knowing os.Getwd is called.
	names := map[string]bool{}
	for _, n := range chain.Nodes {
		names[n.ShortName] = true
	}
	for _, want := range []string{"runCodeTUI", "Discover", "locateProjectsFileRoot", "Primary"} {
		if !names[want] {
			t.Errorf("chain missing expected node %q; got nodes: %v", want, names)
		}
	}

	// os.Getwd is stdlib and should be NoteOnly.
	for _, n := range chain.Nodes {
		if n.ShortName == "Getwd" && !n.NoteOnly {
			t.Errorf("os.Getwd should be NoteOnly (stdlib), got expanded: %+v", n)
		}
	}
}

// TestWalkCallChains_StdlibSkippedAsNote: a stdlib callee at any
// depth gets noted but not expanded — even if it has callees of its
// own, we don't recurse into the stdlib.
func TestWalkCallChains_StdlibSkippedAsNote(t *testing.T) {
	g := newEPGraph()
	root := g.addSymbol("sym:root", "root")
	g.addSymbol("sym:Println", "fmt.Println")
	g.addSymbol("sym:doFprintln", "fmt.doFprintln")

	g.addCalls("sym:root", "sym:Println")
	g.addCalls("sym:Println", "sym:doFprintln")

	chains := WalkCallChains([]ResolvedEntryPoint{{Symbol: root}}, g)
	for _, n := range chains[0].Nodes {
		if n.ShortName == "doFprintln" {
			t.Errorf("fmt.doFprintln should not appear — stdlib callee shouldn't recurse")
		}
	}
}

// TestWalkCallChains_PureDataHelperNotExpanded: helpers named
// getX/setX/isX/etc. are listed but their callees aren't walked.
// Reduces noise from accessor cascades in real codebases.
func TestWalkCallChains_PureDataHelperNotExpanded(t *testing.T) {
	g := newEPGraph()
	root := g.addSymbol("sym:root", "root")
	g.addSymbol("sym:getThing", "getThing")
	g.addSymbol("sym:innerCall", "innerCall")

	g.addCalls("sym:root", "sym:getThing")
	g.addCalls("sym:getThing", "sym:innerCall")

	chains := WalkCallChains([]ResolvedEntryPoint{{Symbol: root}}, g)
	for _, n := range chains[0].Nodes {
		if n.ShortName == "innerCall" {
			t.Errorf("innerCall should not appear — parent is a pure-data helper, callees pruned")
		}
	}
	// getThing itself should be present.
	found := false
	for _, n := range chains[0].Nodes {
		if n.ShortName == "getThing" {
			found = true
		}
	}
	if !found {
		t.Error("getThing should appear as a node — only its callees are pruned")
	}
}

// TestWalkCallChains_RespectsCap: a chain with >20 callees gets
// truncated at the cap and the Truncated flag is set so the
// formatter can append a hint.
func TestWalkCallChains_RespectsCap(t *testing.T) {
	g := newEPGraph()
	root := g.addSymbol("sym:root", "rootFunc")
	for i := 0; i < 30; i++ {
		id := "sym:child" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		g.addSymbol(id, "childFunc"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		g.addCalls("sym:root", id)
	}

	chains := WalkCallChains([]ResolvedEntryPoint{{Symbol: root}}, g)
	if !chains[0].Truncated {
		t.Error("expected Truncated=true when callee count exceeds cap")
	}
	if len(chains[0].Nodes) > callChainCap {
		t.Errorf("got %d nodes, exceeds cap of %d", len(chains[0].Nodes), callChainCap)
	}
}

// TestWalkCallChains_BudgetSplitsAcrossEntries: two entries with no
// shared ancestor each get half the budget.
func TestWalkCallChains_BudgetSplitsAcrossEntries(t *testing.T) {
	g := newEPGraph()
	rootA := g.addSymbol("sym:rootA", "rootA")
	rootB := g.addSymbol("sym:rootB", "rootB")
	for i := 0; i < 15; i++ {
		idA := "sym:Achild" + string(rune('a'+i))
		idB := "sym:Bchild" + string(rune('a'+i))
		g.addSymbol(idA, "achild"+string(rune('a'+i)))
		g.addSymbol(idB, "bchild"+string(rune('a'+i)))
		g.addCalls("sym:rootA", idA)
		g.addCalls("sym:rootB", idB)
	}

	chains := WalkCallChains([]ResolvedEntryPoint{
		{Symbol: rootA},
		{Symbol: rootB},
	}, g)
	if len(chains) != 2 {
		t.Fatalf("got %d chains, want 2", len(chains))
	}
	// Each chain gets cap/2 = 10. Each has 15 children + 1 root, so
	// both should truncate.
	if !chains[0].Truncated || !chains[1].Truncated {
		t.Errorf("both chains should truncate at cap/2 = 10; got truncated=(%v, %v)",
			chains[0].Truncated, chains[1].Truncated)
	}
}

// TestWalkCallChains_FileOnlyEntry: an entry with no Symbol
// produces a degenerate empty chain — formatter handles it
// elsewhere.
func TestWalkCallChains_FileOnlyEntry(t *testing.T) {
	g := newEPGraph()
	g.addFile("set.go")

	chains := WalkCallChains([]ResolvedEntryPoint{
		{Token: "set.go", FilePath: "set.go", Stage: StageFile},
	}, g)
	if len(chains) != 1 {
		t.Fatalf("got %d chains, want 1", len(chains))
	}
	if len(chains[0].Nodes) != 0 {
		t.Errorf("expected empty chain for file-only entry, got %d nodes", len(chains[0].Nodes))
	}
}
