package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/agent/message"
	"kai/internal/graph"
)

// TestExtractFilePaths covers the regex-based path extraction.
// Tool-result content is the most common source (view tool dumps
// a path, the agent narrates it back) — make sure both text and
// tool-result branches contribute.
func TestExtractFilePaths(t *testing.T) {
	hist := []message.Message{
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.TextContent{Text: "fix the bug in src/auth.py affecting api/routes.go"},
			},
		},
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.ToolResult{Content: "wrote 412 bytes to src/auth.py"},
			},
		},
	}
	got := extractFilePaths(hist)
	want := []string{"src/auth.py", "api/routes.go"}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

// TestExtractFilePaths_IgnoresProse: a sentence mentioning a verb
// like "auto.go" shouldn't fire (it doesn't), but neither should
// non-extension words. Regex is intentionally conservative.
func TestExtractFilePaths_IgnoresProse(t *testing.T) {
	hist := []message.Message{
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.TextContent{Text: "the function takes a long time to run"},
			},
		},
	}
	if got := extractFilePaths(hist); len(got) != 0 {
		t.Errorf("expected no matches, got %v", got)
	}
}

// TestLatestSlice trims to the most recent user/tool turn so we
// don't re-inject context from earlier turns we already injected.
func TestLatestSlice(t *testing.T) {
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "first"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "ok"}}},
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "second"}}},
	}
	got := latestSlice(hist)
	if len(got) != 1 {
		t.Fatalf("expected 1 message in latest slice, got %d", len(got))
	}
	if t1 := got[0].Parts[0].(message.TextContent).Text; t1 != "second" {
		t.Errorf("wrong message returned: %q", t1)
	}
}

// TestIsProtected covers exact-glob and recursive `/**` matching
// without instantiating the gate package.
func TestIsProtected(t *testing.T) {
	cases := []struct {
		path    string
		patts   []string
		want    bool
	}{
		{"internal/auth/middleware.go", []string{"internal/auth/**"}, true},
		{"internal/auth/middleware.go", []string{"internal/db/**"}, false},
		{"go.mod", []string{"go.mod"}, true},
		{"main.go", nil, false},
	}
	for _, c := range cases {
		if got := isProtected(c.path, c.patts); got != c.want {
			t.Errorf("isProtected(%q, %v) = %v, want %v", c.path, c.patts, got, c.want)
		}
	}
}

// TestInjector_NilGraph: nil graph DB means no injector — calls
// short-circuit to "" so the runner just sends the system prompt
// unchanged. Avoids needing a graph fixture for tests that don't
// care about graph context.
func TestInjector_NilGraph(t *testing.T) {
	gc := newGraphContextInjector(nil, "", ModeCoding)
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "edit auth.py"},
		}},
	}
	if got := gc.buildBlock(hist, nil); got != "" {
		t.Errorf("expected empty block from nil graph, got %q", got)
	}
}

// TestInjector_NoNewFiles: once a file has been injected, a later
// turn that mentions the same file produces no new block. Critical
// to avoid spamming the model with the same callers list every
// turn.
func TestInjector_NoNewFiles(t *testing.T) {
	gc := &graphContextInjector{
		// No graph (calls short-circuit), but we still want to
		// exercise the injected-set bookkeeping.
		injected: map[string]bool{"auth.py": true},
	}
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "now also fix auth.py timeout"},
		}},
	}
	if got := gc.buildBlock(hist, nil); !strings.HasPrefix(got, "") || got != "" {
		t.Errorf("re-mentioning already-injected file should produce no block, got %q", got)
	}
}

// openTestGraph spins up an isolated graph.DB on disk for trace-test
// fixtures. The schema-bootstrap mirrors the graph package's own
// test setup; we re-create it here to avoid coupling the agent
// tests to the graph package's internal test helpers.
func openTestGraph(t *testing.T) *graph.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	objPath := filepath.Join(dir, "objects")
	if err := os.MkdirAll(objPath, 0o755); err != nil {
		t.Fatalf("mkdir objects: %v", err)
	}
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		t.Fatalf("graph.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS nodes (id BLOB PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS edges (src BLOB NOT NULL, type TEXT NOT NULL, dst BLOB NOT NULL, at BLOB, created_at INTEGER NOT NULL, PRIMARY KEY (src, type, dst, at));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target_id BLOB NOT NULL, target_kind TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS slugs (target_id BLOB PRIMARY KEY, slug TEXT UNIQUE NOT NULL);
CREATE TABLE IF NOT EXISTS logs (kind TEXT NOT NULL, seq INTEGER NOT NULL, id BLOB NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (kind, seq));`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

// addFile inserts a File-kind node whose payload.path is the given
// workspace-relative path. Returns the node id.
func addFile(t *testing.T, db *graph.DB, path string) []byte {
	t.Helper()
	id, err := db.InsertNodeDirect(graph.KindFile, map[string]interface{}{
		"name": path, "path": path,
	})
	if err != nil {
		t.Fatalf("InsertNodeDirect %q: %v", path, err)
	}
	return id
}

// addCall wires a File→File EdgeCalls edge from src to dst. The
// graph injector reads inbound edges via GetEdgesToByPath and
// resolves src→path through GetNode, so all nodes here are Files.
// Production graphs use Symbol-level edges; the injector tolerates
// both because it only reads payload.path.
func addCall(t *testing.T, db *graph.DB, src, dst []byte) {
	t.Helper()
	if err := db.InsertEdgeDirect(src, graph.EdgeCalls, dst, nil); err != nil {
		t.Fatalf("InsertEdgeDirect: %v", err)
	}
}

// TestSummarizeTrace_HubDropsBeforeSpecific builds the canonical
// pathological case for the Debug heuristic: many distance-1 callers
// of the broken file, half of them low-degree (specific to one
// place) and half high-degree (called by many other files). The cap
// forces the truncation to choose; specifics must win, hubs must lose.
//
// Without this guarantee, Debug mode would surface logging/utility
// hubs as the "trace" — which is the noise problem the spec is
// trying to prevent.
func TestSummarizeTrace_HubDropsBeforeSpecific(t *testing.T) {
	db := openTestGraph(t)

	broken := addFile(t, db, "broken.go")

	// 30 specific callers — degree 0 (nothing calls them).
	const specifics = 30
	specificPaths := make([]string, 0, specifics)
	for i := 0; i < specifics; i++ {
		path := fmt.Sprintf("specific_%02d.go", i)
		specificPaths = append(specificPaths, path)
		id := addFile(t, db, path)
		addCall(t, db, id, broken)
	}

	// 30 hub callers — each called by 5 random caller files, so each
	// hub has degree 5. Hubs share the same 5 caller files to keep
	// the fixture compact.
	const hubs = 30
	hubPaths := make([]string, 0, hubs)
	hubIDs := make([][]byte, 0, hubs)
	for i := 0; i < hubs; i++ {
		path := fmt.Sprintf("hub_%02d.go", i)
		hubPaths = append(hubPaths, path)
		id := addFile(t, db, path)
		hubIDs = append(hubIDs, id)
		addCall(t, db, id, broken)
	}
	// 5 caller files that call all 30 hubs.
	const callerCount = 5
	for j := 0; j < callerCount; j++ {
		callerID := addFile(t, db, fmt.Sprintf("caller_%d.go", j))
		for _, h := range hubIDs {
			addCall(t, db, callerID, h)
		}
	}

	gc := newGraphContextInjector(db, "/ws", ModeDebug)
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "panic: nil pointer at broken.go:47"},
		}},
	}
	block := gc.buildBlock(hist, nil)
	if block == "" {
		t.Fatal("expected non-empty trace block")
	}

	// Total nodes: 30 specifics + 30 hubs at distance 1, plus 5
	// caller files at distance 2 (each caller calls all 30 hubs and
	// thus reaches broken via depth 2). 65 total, cap 50, so 15
	// drop out: hub_20..hub_29 (10 hubs lose to lower-degree
	// specifics within distance 1) and the 5 distance-2 callers
	// (which sort after all distance-1 nodes).
	if !strings.Contains(block, "15 nodes dropped (hubs + far) to fit 50-node cap") {
		t.Errorf("expected 15-node drop notice in block:\n%s", block)
	}
	for _, sp := range specificPaths {
		if !strings.Contains(block, sp) {
			t.Errorf("specific caller %q should be kept, missing from block", sp)
		}
	}
	// Exactly 20 of 30 hubs should appear (sorted alphabetically by
	// path among ties on degree, hub_00..hub_19 keep, hub_20..hub_29
	// drop). Verify hub_20 is NOT present and hub_19 IS.
	if strings.Contains(block, "hub_29.go") {
		t.Error("hub_29 should have been dropped")
	}
	if !strings.Contains(block, "hub_19.go") {
		t.Error("hub_19 should have been kept")
	}
	// Sanity: header reflects Trace scope.
	if !strings.Contains(block, "Trace (depth 2") {
		t.Errorf("expected trace header, got block:\n%s", block)
	}
}

// TestSummarizeTrace_NoCallersReturnsEmpty: a seed file with zero
// inbound edges produces no trace block — there's nothing to trace,
// and emitting an empty "callers (depth 1):" header would just
// pollute the prompt.
func TestSummarizeTrace_NoCallersReturnsEmpty(t *testing.T) {
	db := openTestGraph(t)
	addFile(t, db, "lonely.go")

	gc := newGraphContextInjector(db, "/ws", ModeDebug)
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "Error: lonely.go:1 something"},
		}},
	}
	block := gc.buildBlock(hist, nil)
	// Block can still contain the project overview from the
	// workspace, but no Trace section should appear.
	if strings.Contains(block, "Trace (depth 2") {
		t.Errorf("orphan file should not produce a trace section:\n%s", block)
	}
}

// TestSummarizeTrace_RendersDepth2WithVia confirms callers-of-callers
// surface with the via path so the model can read the chain.
func TestSummarizeTrace_RendersDepth2WithVia(t *testing.T) {
	db := openTestGraph(t)
	broken := addFile(t, db, "broken.go")
	a := addFile(t, db, "a.go")
	x := addFile(t, db, "x.go")
	addCall(t, db, a, broken)
	addCall(t, db, x, a)

	gc := newGraphContextInjector(db, "/ws", ModeDebug)
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "panic at broken.go:1"},
		}},
	}
	block := gc.buildBlock(hist, nil)
	if !strings.Contains(block, "callers (depth 1):") {
		t.Errorf("missing depth-1 section:\n%s", block)
	}
	if !strings.Contains(block, "callers-of-callers (depth 2):") {
		t.Errorf("missing depth-2 section:\n%s", block)
	}
	if !strings.Contains(block, "via a.go") {
		t.Errorf("depth-2 entry should show via path:\n%s", block)
	}
}

// TestSummarizeBroad_PlanningEmitsAllCallers ensures Planning mode
// produces uncapped caller lists. Coding's default summary truncates
// at 5 names with "+N more"; Planning needs the full set so the
// model can decide work splits.
func TestSummarizeBroad_PlanningEmitsAllCallers(t *testing.T) {
	db := openTestGraph(t)
	target := addFile(t, db, "target.go")
	const callers = 10
	for i := 0; i < callers; i++ {
		c := addFile(t, db, fmt.Sprintf("caller_%02d.go", i))
		addCall(t, db, c, target)
	}

	gc := newGraphContextInjector(db, "/ws", ModePlanning)
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "plan changes to target.go"},
		}},
	}
	block := gc.buildBlock(hist, nil)
	if !strings.Contains(block, "full caller chains") {
		t.Errorf("expected Planning header in block:\n%s", block)
	}
	for i := 0; i < callers; i++ {
		want := fmt.Sprintf("caller_%02d.go", i)
		if !strings.Contains(block, want) {
			t.Errorf("Planning broad block missing caller %q:\n%s", want, block)
		}
	}
	// Coding's "+N more" cap marker must NOT appear.
	if strings.Contains(block, "+5 more") || strings.Contains(block, "+4 more") {
		t.Errorf("Planning should not truncate callers:\n%s", block)
	}
}

// TestSummarizeNarrow_CodingTruncatesCallers is the inverse: Coding
// keeps the existing 5-name cap so the prompt stays focused.
func TestSummarizeNarrow_CodingTruncatesCallers(t *testing.T) {
	db := openTestGraph(t)
	target := addFile(t, db, "narrow.go")
	const callers = 10
	for i := 0; i < callers; i++ {
		c := addFile(t, db, fmt.Sprintf("nc_%02d.go", i))
		addCall(t, db, c, target)
	}
	gc := newGraphContextInjector(db, "/ws", ModeCoding)
	hist := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "edit narrow.go"},
		}},
	}
	block := gc.buildBlock(hist, nil)
	if !strings.Contains(block, "Files in scope (kai graph):") {
		t.Errorf("Coding should use default header:\n%s", block)
	}
	if !strings.Contains(block, "+5 more") {
		t.Errorf("Coding should cap at 5 with '+N more':\n%s", block)
	}
	// Trace header must not appear in Coding mode.
	if strings.Contains(block, "Trace (depth 2") {
		t.Errorf("Coding should not produce trace header:\n%s", block)
	}
}
