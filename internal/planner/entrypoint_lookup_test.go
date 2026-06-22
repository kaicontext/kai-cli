package planner

import (
	"testing"

	"kai/internal/graph"
)

// epGraph is the minimal GraphAccess fake for entry-point lookup
// tests. Provides KindSymbol via GetNodesByKind, KindFile via the
// same, and DEFINES_IN edges so the symbol→file index builds.
type epGraph struct {
	symbols  []*graph.Node
	files    []*graph.Node
	defines  []*graph.Edge
	calls    []*graph.Edge
	nodeByID map[string]*graph.Node
}

func newEPGraph() *epGraph {
	return &epGraph{nodeByID: map[string]*graph.Node{}}
}

func (g *epGraph) addSymbol(id, fqName string) *graph.Node {
	n := &graph.Node{
		ID:      []byte(id),
		Kind:    graph.KindSymbol,
		Payload: map[string]interface{}{"fqName": fqName},
	}
	g.symbols = append(g.symbols, n)
	g.nodeByID[id] = n
	return n
}

func (g *epGraph) addFile(path string) *graph.Node {
	n := &graph.Node{
		ID:      []byte(path),
		Kind:    graph.KindFile,
		Payload: map[string]interface{}{"path": path},
	}
	g.files = append(g.files, n)
	g.nodeByID[path] = n
	return n
}

func (g *epGraph) addDefines(symbolID, filePath string) {
	g.defines = append(g.defines, &graph.Edge{
		Src: []byte(symbolID),
		Dst: []byte(filePath),
	})
}

func (g *epGraph) GetNodesByKind(kind graph.NodeKind) ([]*graph.Node, error) {
	switch kind {
	case graph.KindSymbol:
		return g.symbols, nil
	case graph.KindFile:
		return g.files, nil
	}
	return nil, nil
}

func (g *epGraph) GetEdgesOfType(t graph.EdgeType) ([]*graph.Edge, error) {
	switch t {
	case graph.EdgeDefinesIn:
		return g.defines, nil
	case graph.EdgeCalls:
		return g.calls, nil
	}
	return nil, nil
}

func (g *epGraph) GetEdgesToByPath(string, graph.EdgeType) ([]*graph.Edge, error) {
	return nil, nil
}

func (g *epGraph) GetNode(id []byte) (*graph.Node, error) {
	return g.nodeByID[string(id)], nil
}

// TestResolveEntryPoints_CommandStage: the kai-code regression. A
// backticked `kai code` token, paired with a command index that
// maps "code" → "runCodeTUI" and a graph that defines runCodeTUI in
// tui.go, must produce a StageCommand resolution pointing at the
// runCodeTUI symbol.
func TestResolveEntryPoints_CommandStage(t *testing.T) {
	g := newEPGraph()
	sym := g.addSymbol("sym:runCodeTUI", "runCodeTUI")
	g.addFile("cmd/kai/tui.go")
	g.addDefines("sym:runCodeTUI", "cmd/kai/tui.go")

	cmds := &CommandIndex{
		commands:     map[string]string{"code": "runCodeTUI"},
		handlerFiles: map[string]string{"runCodeTUI": "cmd/kai/tui.go"},
	}

	tokens := []EntryPointToken{{Raw: "kai code", Origin: OriginBacktick}}
	got := ResolveEntryPoints(tokens, g, cmds)
	if len(got) != 1 {
		t.Fatalf("got %d resolutions, want 1", len(got))
	}
	if got[0].Stage != StageCommand {
		t.Errorf("Stage = %v, want StageCommand", got[0].Stage)
	}
	if got[0].HandlerName != "runCodeTUI" {
		t.Errorf("HandlerName = %q, want runCodeTUI", got[0].HandlerName)
	}
	if got[0].Symbol != sym {
		t.Errorf("Symbol = %v, want the runCodeTUI node", got[0].Symbol)
	}
	if got[0].FilePath != "cmd/kai/tui.go" {
		t.Errorf("FilePath = %q, want cmd/kai/tui.go", got[0].FilePath)
	}
}

// TestResolveEntryPoints_SymbolStage covers the direct symbol path:
// a CamelCase token that matches a symbol's fqName resolves at the
// symbol stage. Command index doesn't fire because the token isn't
// a registered command.
func TestResolveEntryPoints_SymbolStage(t *testing.T) {
	g := newEPGraph()
	sym := g.addSymbol("sym:Primary", "projects.Set.Primary")
	g.addFile("internal/projects/set.go")
	g.addDefines("sym:Primary", "internal/projects/set.go")

	tokens := []EntryPointToken{{Raw: "Primary", Origin: OriginBacktick}}
	got := ResolveEntryPoints(tokens, g, &CommandIndex{})
	if len(got) != 1 {
		t.Fatalf("got %d resolutions, want 1", len(got))
	}
	if got[0].Stage != StageSymbol {
		t.Errorf("Stage = %v, want StageSymbol", got[0].Stage)
	}
	if got[0].Symbol != sym {
		t.Errorf("Symbol = %v, want Primary", got[0].Symbol)
	}
	if got[0].FilePath != "internal/projects/set.go" {
		t.Errorf("FilePath = %q, want set.go path", got[0].FilePath)
	}
}

// TestResolveEntryPoints_FileStage: a path-shaped token that
// matches a file node falls through to the file stage. No symbol
// is attached.
func TestResolveEntryPoints_FileStage(t *testing.T) {
	g := newEPGraph()
	g.addFile("internal/projects/set.go")

	tokens := []EntryPointToken{{Raw: "set.go", Origin: OriginPath}}
	got := ResolveEntryPoints(tokens, g, &CommandIndex{})
	if len(got) != 1 {
		t.Fatalf("got %d resolutions, want 1", len(got))
	}
	if got[0].Stage != StageFile {
		t.Errorf("Stage = %v, want StageFile", got[0].Stage)
	}
	if got[0].Symbol != nil {
		t.Errorf("Symbol = %v, want nil for file-only resolution", got[0].Symbol)
	}
	if got[0].FilePath != "internal/projects/set.go" {
		t.Errorf("FilePath = %q, want set.go path", got[0].FilePath)
	}
}

// TestResolveEntryPoints_UnresolvedTokensDropped: tokens that don't
// match any stage are silently dropped — callers shouldn't have to
// filter them out.
func TestResolveEntryPoints_UnresolvedTokensDropped(t *testing.T) {
	g := newEPGraph()
	g.addSymbol("sym:knownThing", "knownThing")
	g.addFile("known.go")
	g.addDefines("sym:knownThing", "known.go")

	tokens := []EntryPointToken{
		{Raw: "knownThing", Origin: OriginCamelCase},
		{Raw: "nothingMatches", Origin: OriginCamelCase},
		{Raw: "kaicontext.com", Origin: OriginPath},
	}
	got := ResolveEntryPoints(tokens, g, &CommandIndex{})
	if len(got) != 1 {
		t.Fatalf("got %d resolutions, want 1 (only knownThing)", len(got))
	}
	if got[0].Token != "knownThing" {
		t.Errorf("resolution = %+v, want token knownThing", got[0])
	}
}

// TestResolveEntryPoints_FirstStageWins: a token that COULD match
// at multiple stages stops at the first hit. Command beats symbol,
// symbol beats file.
func TestResolveEntryPoints_FirstStageWins(t *testing.T) {
	g := newEPGraph()
	// A symbol AND a file both literally named "code".
	g.addSymbol("sym:code", "code")
	g.addFile("code")
	g.addDefines("sym:code", "code")

	// Command index also has "code" → "runCodeTUI" (separate symbol).
	g.addSymbol("sym:runCodeTUI", "runCodeTUI")
	g.addFile("cmd/kai/tui.go")
	g.addDefines("sym:runCodeTUI", "cmd/kai/tui.go")
	cmds := &CommandIndex{
		commands:     map[string]string{"code": "runCodeTUI"},
		handlerFiles: map[string]string{"runCodeTUI": "cmd/kai/tui.go"},
	}

	tokens := []EntryPointToken{{Raw: "code", Origin: OriginBacktick}}
	got := ResolveEntryPoints(tokens, g, cmds)
	if len(got) != 1 {
		t.Fatalf("got %d resolutions, want 1", len(got))
	}
	if got[0].Stage != StageCommand {
		t.Errorf("Stage = %v, want StageCommand (command wins over symbol/file)", got[0].Stage)
	}
}
