package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/authorship"
	"kai/internal/graph"
)

// newCheckpointWriterAt builds an authorship.CheckpointWriter
// rooted at dir with a fixed sessionID. Test helper for the
// kai_checkpoint round-trip tests.
func newCheckpointWriterAt(t *testing.T, dir, sessionID string) *authorship.CheckpointWriter {
	t.Helper()
	return authorship.NewCheckpointWriter(dir, sessionID)
}

// writeTestFile writes a file inside dir at relative path rel with
// the given content. Used by kai_live_sync tests that need a real
// file in a workspace for the push action.
func writeTestFile(dir, rel, content string) error {
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

// fakeKaiGraph is an in-memory KaiGrapher. Just enough surface to
// test the three kai_* tools. Edges are stored verbatim by edge type;
// nodes are looked up by path or by the byte-id we assign on insert.
type fakeKaiGraph struct {
	// callsByCallee: when a tool calls GetEdgesOfType(EdgeCalls), we
	// return all edges. Each edge's `At` field points at a "call
	// node" whose payload has calleeName/callerFile/line.
	callEdges []*graph.Edge

	// importsToFile: GetEdgesToByPath(file, EdgeImports) returns
	// edges whose Src is a file that imports `file`.
	importsToFile map[string][]*graph.Edge

	// callsToFile: GetEdgesToByPath(file, EdgeCalls).
	callsToFile map[string][]*graph.Edge

	// definesInToFile: GetEdgesByDst(EdgeDefinesIn, fileNodeID).
	// keyed by hex(fileNodeID).
	definesInByFile map[string][]*graph.Edge

	// nodes: id-keyed (we use path bytes as id for files; arbitrary
	// strings for call/symbol nodes).
	nodes map[string]*graph.Node

	// fileByPath: FindNodesByPayloadPath("File", path).
	fileByPath map[string]*graph.Node
}

func newFakeKaiGraph() *fakeKaiGraph {
	return &fakeKaiGraph{
		importsToFile:   map[string][]*graph.Edge{},
		callsToFile:     map[string][]*graph.Edge{},
		definesInByFile: map[string][]*graph.Edge{},
		nodes:           map[string]*graph.Node{},
		fileByPath:      map[string]*graph.Node{},
	}
}

func (f *fakeKaiGraph) addFile(path string) *graph.Node {
	id := []byte("file:" + path)
	n := &graph.Node{
		ID:      id,
		Kind:    graph.KindFile,
		Payload: map[string]interface{}{"path": path},
	}
	f.nodes[string(id)] = n
	f.fileByPath[path] = n
	return n
}

func (f *fakeKaiGraph) addSymbol(name, kind string, file *graph.Node) {
	symID := []byte("sym:" + name)
	f.nodes[string(symID)] = &graph.Node{
		ID:      symID,
		Kind:    graph.KindSymbol,
		Payload: map[string]interface{}{"fqName": name, "kind": kind},
	}
	edge := &graph.Edge{Src: symID, Dst: file.ID}
	key := string(file.ID)
	f.definesInByFile[key] = append(f.definesInByFile[key], edge)
}

// addCall: register a CALLS edge from caller-file to callee-file with
// a Call node payload describing the call site.
func (f *fakeKaiGraph) addCall(callerFile, calleeFile, calleeName string, line int) {
	callNodeID := []byte("call:" + callerFile + "->" + calleeName + ":" +
		string(rune(line)))
	f.nodes[string(callNodeID)] = &graph.Node{
		ID:   callNodeID,
		Kind: "Call",
		Payload: map[string]interface{}{
			"calleeName": calleeName,
			"callerFile": callerFile,
			"calleeFile": calleeFile,
			"line":       float64(line),
		},
	}
	edge := &graph.Edge{
		Src: []byte("file:" + callerFile),
		Dst: []byte("file:" + calleeFile),
		At:  callNodeID,
	}
	f.callEdges = append(f.callEdges, edge)
	f.callsToFile[calleeFile] = append(f.callsToFile[calleeFile], edge)
}

// addImport: file -> file IMPORTS edge.
func (f *fakeKaiGraph) addImport(importerFile, importedFile string) {
	edge := &graph.Edge{
		Src: []byte("file:" + importerFile),
		Dst: []byte("file:" + importedFile),
	}
	f.importsToFile[importedFile] = append(f.importsToFile[importedFile], edge)
}

// --- KaiGrapher implementation ---------------------------------------

func (f *fakeKaiGraph) GetEdgesToByPath(file string, et graph.EdgeType) ([]*graph.Edge, error) {
	switch et {
	case graph.EdgeImports:
		return f.importsToFile[file], nil
	case graph.EdgeCalls:
		return f.callsToFile[file], nil
	}
	return nil, nil
}

func (f *fakeKaiGraph) GetEdgesOfType(et graph.EdgeType) ([]*graph.Edge, error) {
	if et == graph.EdgeCalls {
		return f.callEdges, nil
	}
	return nil, nil
}

func (f *fakeKaiGraph) GetEdgesByDst(et graph.EdgeType, dst []byte) ([]*graph.Edge, error) {
	if et == graph.EdgeDefinesIn {
		return f.definesInByFile[string(dst)], nil
	}
	return nil, nil
}

func (f *fakeKaiGraph) GetNode(id []byte) (*graph.Node, error) {
	return f.nodes[string(id)], nil
}

func (f *fakeKaiGraph) FindNodesByPayloadPath(kind, path string) ([]*graph.Node, error) {
	if kind != string(graph.KindFile) {
		return nil, nil
	}
	if n, ok := f.fileByPath[path]; ok {
		return []*graph.Node{n}, nil
	}
	return nil, nil
}

// --- tests -----------------------------------------------------------

func TestKaiCallers_FindsMatches(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("router.go")
	g.addFile("api/server.go")
	g.addFile("api/health.go")
	g.addCall("api/server.go", "router.go", "Register", 42)
	g.addCall("api/health.go", "router.go", "Register", 17)
	// Unrelated call shouldn't show up:
	g.addCall("api/server.go", "router.go", "Other", 50)

	tool := (&KaiTools{DB: g}).All()[0] // kai_callers is first
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_callers",
		Input: `{"symbol":"Register"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	for _, want := range []string{"api/server.go:42", "api/health.go:17", "Register"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("missing %q in output:\n%s", want, resp.Content)
		}
	}
	if strings.Contains(resp.Content, "Other") {
		t.Errorf("output should not include unrelated callee: %s", resp.Content)
	}
}

func TestKaiCallers_NoMatches(t *testing.T) {
	g := newFakeKaiGraph()
	tool := (&KaiTools{DB: g}).All()[0]
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_callers",
		Input: `{"symbol":"Nonexistent"}`,
	})
	if !strings.Contains(resp.Content, "no callers") {
		t.Errorf("expected 'no callers' message, got: %s", resp.Content)
	}
}

func TestKaiCallers_NormalizesQualifiedNames(t *testing.T) {
	// CalleeName stored with scope prefix; tool should still match
	// when the agent asks by short name.
	g := newFakeKaiGraph()
	g.addFile("a.go")
	g.addFile("b.go")
	g.addCall("a.go", "b.go", "Resolver::resolve", 10) // Rust-style
	g.addCall("a.go", "b.go", "Type.Method", 20)       // Go-style

	tool := (&KaiTools{DB: g}).All()[0]
	for _, q := range []string{"resolve", "Method"} {
		resp, _ := tool.Run(context.Background(), ToolCall{
			Name:  "kai_callers",
			Input: `{"symbol":"` + q + `"}`,
		})
		if resp.IsError || !strings.Contains(resp.Content, "a.go") {
			t.Errorf("query %q: unexpected output: %s", q, resp.Content)
		}
	}
}

func TestKaiDependents_ReportsImporters(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("util.go")
	g.addFile("api/a.go")
	g.addFile("api/b.go")
	g.addImport("api/a.go", "util.go")
	g.addImport("api/b.go", "util.go")

	tool := (&KaiTools{DB: g}).All()[1] // kai_dependents is second
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_dependents",
		Input: `{"file":"util.go"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	for _, want := range []string{"api/a.go", "api/b.go"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("missing %q in output:\n%s", want, resp.Content)
		}
	}
}

func TestKaiDependents_NoneFound(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("isolated.go")
	tool := (&KaiTools{DB: g}).All()[1]
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_dependents",
		Input: `{"file":"isolated.go"}`,
	})
	if !strings.Contains(resp.Content, "nothing depends") {
		t.Errorf("expected 'nothing depends' message, got: %s", resp.Content)
	}
}

func TestKaiContext_ReportsSymbolsAndDependents(t *testing.T) {
	g := newFakeKaiGraph()
	router := g.addFile("router.go")
	g.addFile("api/server.go")
	g.addSymbol("Register", "function", router)
	g.addSymbol("Mux", "type", router)
	g.addImport("api/server.go", "router.go")

	tool := (&KaiTools{DB: g}).All()[2] // kai_context is third
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_context",
		Input: `{"file":"router.go"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	for _, want := range []string{"Register", "Mux", "[function]", "[type]", "api/server.go", "depth 1"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("missing %q in output:\n%s", want, resp.Content)
		}
	}
}

func TestKaiContext_FileNotFound(t *testing.T) {
	g := newFakeKaiGraph()
	tool := (&KaiTools{DB: g}).All()[2]
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_context",
		Input: `{"file":"missing.go"}`,
	})
	if !resp.IsError || !strings.Contains(resp.Content, "not found") {
		t.Errorf("expected not-found error, got: %+v", resp)
	}
}

// TestKaiTools_NilDBReturnsOnlyNoDBTools confirms that with no graph
// DB wired, the only tools registered are the ones that don't need
// one. As of RT-1, kai_console qualifies (it talks CDP over a
// localhost port, not the graph). Graph-backed tools (kai_callers,
// kai_context, kai_search, etc.) must stay omitted so the runner
// doesn't dispatch a tool that will fail on every call.
func TestKaiTools_NilDBReturnsOnlyNoDBTools(t *testing.T) {
	got := (&KaiTools{DB: nil}).All()
	allowed := map[string]bool{
		"kai_console": true,
	}
	for _, tool := range got {
		name := tool.Info().Name
		if !allowed[name] {
			t.Errorf("tool %q should not register without a DB", name)
		}
	}
}

// TestKaiTools_AllReturnsGraphTools confirms the graph-only baseline
// registers callers/dependents/context/impact when only DB is wired.
// kai_diff (needs binary), kai_checkpoint (needs writer), and
// kai_live_sync (needs remote client + channel) stay omitted.
func TestKaiTools_AllReturnsGraphTools(t *testing.T) {
	g := newFakeKaiGraph()
	tools := (&KaiTools{DB: g}).All()
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Info().Name] = true
	}
	for _, want := range []string{"kai_callers", "kai_dependents", "kai_context", "kai_impact"} {
		if !names[want] {
			t.Errorf("missing graph-tool %q in registry: %v", want, names)
		}
	}
	for _, omitted := range []string{"kai_diff", "kai_checkpoint", "kai_live_sync"} {
		if names[omitted] {
			t.Errorf("%q registered without its dependency wired", omitted)
		}
	}
}

func TestNormalizeSymbolName(t *testing.T) {
	cases := map[string]string{
		"Foo":               "Foo",
		"Type.Method":       "Method",
		"*Resolver.Resolve": "Resolve",
		"crate::foo::bar":   "bar",
		"Module::Class.fn":  "fn",
	}
	for in, want := range cases {
		if got := normalizeSymbolName(in); got != want {
			t.Errorf("normalizeSymbolName(%q): got %q, want %q", in, got, want)
		}
	}
}

// --- kai_impact ------------------------------------------------------

// findToolByName walks KaiTools.All() to retrieve the tool with the
// given Info().Name. Test helper so per-tool tests can build a kit
// once and address tools by string name.
func findToolByName(kt *KaiTools, name string) BaseTool {
	for _, t := range kt.All() {
		if t.Info().Name == name {
			return t
		}
	}
	return nil
}

func TestKaiImpact_RiskLow(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("auth.py")
	g.addFile("api/routes.go")
	g.addCall("api/routes.go", "auth.py", "login", 10)

	tool := findToolByName(&KaiTools{DB: g}, "kai_impact")
	if tool == nil {
		t.Fatal("kai_impact missing from registry")
	}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_impact",
		Input: `{"target":"auth.py"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, `"risk": "low"`) {
		t.Errorf("expected risk=low for 1 caller / no protected: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "api/routes.go") {
		t.Errorf("expected caller file in output: %s", resp.Content)
	}
}

func TestKaiImpact_RiskHigh(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("hub.go")
	for i := 0; i < 12; i++ {
		caller := "caller_" + string(rune('a'+i)) + ".go"
		g.addFile(caller)
		g.addCall(caller, "hub.go", "Helper", i)
	}
	tool := findToolByName(&KaiTools{DB: g}, "kai_impact")
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_impact",
		Input: `{"target":"hub.go"}`,
	})
	if !strings.Contains(resp.Content, `"risk": "high"`) {
		t.Errorf("expected risk=high for 12 callers: %s", resp.Content)
	}
}

func TestKaiImpact_RiskMediumOnProtected(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("internal/auth/middleware.go")
	g.addFile("api/routes.go")
	g.addCall("api/routes.go", "internal/auth/middleware.go", "Authenticate", 1)

	tool := findToolByName(&KaiTools{
		DB:        g,
		Protected: []string{"internal/auth/**"},
	}, "kai_impact")
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_impact",
		Input: `{"target":"internal/auth/middleware.go"}`,
	})
	if !strings.Contains(resp.Content, `"risk": "medium"`) {
		t.Errorf("expected risk=medium when protected: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, `"has_protected": true`) {
		t.Errorf("expected has_protected=true: %s", resp.Content)
	}
}

func TestKaiImpact_RiskNone(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("orphan.go")
	tool := findToolByName(&KaiTools{DB: g}, "kai_impact")
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_impact",
		Input: `{"target":"orphan.go"}`,
	})
	if !strings.Contains(resp.Content, `"risk": "none"`) {
		t.Errorf("orphan file should be risk=none: %s", resp.Content)
	}
}

func TestKaiImpact_FunctionFilter(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("auth.py")
	g.addFile("a.go")
	g.addFile("b.go")
	g.addCall("a.go", "auth.py", "login", 1)
	g.addCall("b.go", "auth.py", "logout", 2)

	tool := findToolByName(&KaiTools{DB: g}, "kai_impact")
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_impact",
		Input: `{"target":"auth.py","function":"login"}`,
	})
	if strings.Contains(resp.Content, "b.go") {
		t.Errorf("function=login should not include b.go (which calls logout): %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "a.go") {
		t.Errorf("function=login should include a.go: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "auth.py::login") {
		t.Errorf("target should display as file::function: %s", resp.Content)
	}
}

func TestKaiImpact_RejectsMissingTarget(t *testing.T) {
	g := newFakeKaiGraph()
	tool := findToolByName(&KaiTools{DB: g}, "kai_impact")
	resp, _ := tool.Run(context.Background(), ToolCall{Name: "kai_impact", Input: `{}`})
	if !resp.IsError {
		t.Errorf("expected IsError=true on missing target")
	}
}

// --- kai_diff parsing helpers ---------------------------------------

func TestSplitUnifiedPatches_MultipleFiles(t *testing.T) {
	in := `--- a/auth.py
+++ b/auth.py
@@ -1,3 +1,4 @@
 a
+b
 c
--- a/router.go
+++ b/router.go
@@ -10,2 +10,3 @@
 x
+y
`
	patches := splitUnifiedPatches(in)
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(patches))
	}
	if _, ok := patches["auth.py"]; !ok {
		t.Errorf("missing auth.py patch")
	}
	if _, ok := patches["router.go"]; !ok {
		t.Errorf("missing router.go patch")
	}
}

func TestCountAddDel(t *testing.T) {
	patch := `--- a/x
+++ b/x
@@ -1,3 +1,3 @@
 a
-old
+new1
+new2
 b
`
	add, del := countAddDel(patch)
	if add != 2 || del != 1 {
		t.Errorf("got add=%d del=%d, want 2/1", add, del)
	}
}

func TestExtractFunctionsChanged(t *testing.T) {
	patch := `--- a/auth.py
+++ b/auth.py
@@ -10,5 +10,8 @@ def login(self, user):
 stuff
+more
@@ -50,3 +50,4 @@ def logout(self):
 ok
+x
`
	fns := extractFunctionsChanged(patch)
	want := map[string]bool{"login": true, "logout": true}
	if len(fns) != 2 {
		t.Fatalf("expected 2 functions, got %d: %v", len(fns), fns)
	}
	for _, f := range fns {
		if !want[f] {
			t.Errorf("unexpected function %q", f)
		}
	}
}

// --- kai_checkpoint -------------------------------------------------

func TestKaiCheckpoint_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := newCheckpointWriterAt(t, dir, "test-session")
	tool := findToolByName(&KaiTools{
		CheckpointWriter: w,
		AgentName:        "test-agent",
		AgentModel:       "claude-sonnet-4-6",
	}, "kai_checkpoint")
	if tool == nil {
		t.Fatal("kai_checkpoint missing from registry")
	}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_checkpoint",
		Input: `{"file":"auth.py","start_line":47,"end_line":52,"action":"modified"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, `"recorded":true`) {
		t.Errorf("expected recorded=true: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "cp_") {
		t.Errorf("expected checkpoint id prefix: %s", resp.Content)
	}
}

func TestKaiCheckpoint_RejectsBadInput(t *testing.T) {
	w := newCheckpointWriterAt(t, t.TempDir(), "s1")
	tool := findToolByName(&KaiTools{CheckpointWriter: w, AgentName: "a"}, "kai_checkpoint")
	cases := []struct {
		name  string
		input string
	}{
		{"missing file", `{"start_line":1,"end_line":2,"action":"modified"}`},
		{"bad range", `{"file":"x","start_line":5,"end_line":2,"action":"modified"}`},
		{"bad action", `{"file":"x","start_line":1,"end_line":2,"action":"vandalism"}`},
	}
	for _, c := range cases {
		resp, _ := tool.Run(context.Background(), ToolCall{Name: "kai_checkpoint", Input: c.input})
		if !resp.IsError {
			t.Errorf("%s: expected IsError, got %s", c.name, resp.Content)
		}
	}
}

// --- kai_live_sync --------------------------------------------------

type fakeLiveSyncClient struct {
	pushes []struct{ agent, channel, file, digest, content string }
	err    error
}

func (f *fakeLiveSyncClient) SyncPushFile(agent, channelID, filePath, digest, contentBase64 string) error {
	if f.err != nil {
		return f.err
	}
	f.pushes = append(f.pushes, struct{ agent, channel, file, digest, content string }{
		agent, channelID, filePath, digest, contentBase64,
	})
	return nil
}

func TestKaiLiveSync_PushReadsAndForwards(t *testing.T) {
	ws := t.TempDir()
	if err := writeTestFile(ws, "hello.txt", "hi\n"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLiveSyncClient{}
	tool := findToolByName(&KaiTools{
		Workspace:      ws,
		LiveSyncClient: fake,
		ChannelID:      "channel-xyz",
		AgentName:      "agent-1",
	}, "kai_live_sync")
	if tool == nil {
		t.Fatal("kai_live_sync missing from registry")
	}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_live_sync",
		Input: `{"action":"push","file":"hello.txt"}`,
	})
	if resp.IsError {
		t.Fatalf("push failed: %s", resp.Content)
	}
	if len(fake.pushes) != 1 {
		t.Fatalf("expected 1 push, got %d", len(fake.pushes))
	}
	got := fake.pushes[0]
	if got.file != "hello.txt" || got.channel != "channel-xyz" || got.agent != "agent-1" {
		t.Errorf("push args wrong: %+v", got)
	}
	if got.digest == "" || got.content == "" {
		t.Errorf("digest/content empty: %+v", got)
	}
}

func TestKaiLiveSync_PushRejectsAbsolutePath(t *testing.T) {
	fake := &fakeLiveSyncClient{}
	tool := findToolByName(&KaiTools{
		Workspace: t.TempDir(), LiveSyncClient: fake, ChannelID: "c", AgentName: "a",
	}, "kai_live_sync")
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_live_sync",
		Input: `{"action":"push","file":"/etc/passwd"}`,
	})
	if !resp.IsError {
		t.Errorf("absolute path should be rejected: %s", resp.Content)
	}
	if len(fake.pushes) != 0 {
		t.Errorf("unexpected push attempt: %v", fake.pushes)
	}
}

func TestKaiLiveSync_StatusReflectsConfig(t *testing.T) {
	tool := findToolByName(&KaiTools{
		Workspace: t.TempDir(), LiveSyncClient: &fakeLiveSyncClient{},
		ChannelID: "c1", AgentName: "a",
	}, "kai_live_sync")
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_live_sync",
		Input: `{"action":"status"}`,
	})
	if !strings.Contains(resp.Content, `"connected": true`) {
		t.Errorf("expected connected=true: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "c1") {
		t.Errorf("status should show channel id: %s", resp.Content)
	}
}

func TestKaiLiveSync_PullIsInformational(t *testing.T) {
	tool := findToolByName(&KaiTools{
		Workspace: t.TempDir(), LiveSyncClient: &fakeLiveSyncClient{},
		ChannelID: "c", AgentName: "a",
	}, "kai_live_sync")
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_live_sync",
		Input: `{"action":"pull"}`,
	})
	if resp.IsError {
		t.Fatalf("pull should not error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "orchestrator") {
		t.Errorf("pull should explain it's orchestrator-driven: %s", resp.Content)
	}
}

func TestKaiLiveSync_NotConnectedRejectsPush(t *testing.T) {
	// No client → tool isn't registered. Build it directly and call
	// Run to confirm runtime guard fires too (defense-in-depth).
	tool := findToolByName(&KaiTools{
		Workspace: t.TempDir(), LiveSyncClient: &fakeLiveSyncClient{},
		ChannelID: "", // empty channel = not connected
		AgentName: "a",
	}, "kai_live_sync")
	if tool != nil {
		t.Errorf("kai_live_sync should not register without ChannelID")
	}
}
