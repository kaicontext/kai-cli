package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/projects"
)

// fakeSearcher is an in-memory stand-in for the FTS5 layer. Lets
// the tool tests run without spinning up SQLite + walking real
// disk paths — same trade-off as KaiGrapher in the rest of the
// tool tests.
type fakeSearcher struct {
	mu      sync.Mutex
	indexed map[string]map[string]string // project → path → body
}

func (f *fakeSearcher) IndexFile(project, path, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.indexed == nil {
		f.indexed = map[string]map[string]string{}
	}
	if f.indexed[project] == nil {
		f.indexed[project] = map[string]string{}
	}
	f.indexed[project][path] = body
	return nil
}

func (f *fakeSearcher) FileTextCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, files := range f.indexed {
		n += len(files)
	}
	return n
}

func (f *fakeSearcher) CountFileTextForProject(project string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.indexed[project])
}

func (f *fakeSearcher) ClearFileTextForProject(project string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.indexed, project)
	return nil
}

func (f *fakeSearcher) RemoveFile(project, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.indexed[project] != nil {
		delete(f.indexed[project], path)
	}
	return nil
}

func (f *fakeSearcher) SearchText(query, project string, limit int) ([]graph.SearchHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []graph.SearchHit
	for proj, files := range f.indexed {
		if project != "" && project != proj {
			continue
		}
		for path, body := range files {
			if strings.Contains(body, query) {
				hits = append(hits, graph.SearchHit{
					Project: proj,
					Path:    path,
					Snippet: "«" + query + "» context",
				})
			}
		}
	}
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// TestKaiSearch_BackfillsThenSearches confirms the end-to-end shape:
// first call walks disk, populates the index, returns results with
// the "indexed N files" header so the user knows why the first
// search felt heavier. Second call skips backfill (the index is
// already populated) and answers directly.
func TestKaiSearch_BackfillsThenSearches(t *testing.T) {
	ws := t.TempDir()
	for _, f := range []struct{ path, body string }{
		{"src/handler.go", "package main\nfunc handleAuth() {}"},
		{"src/router.go", "// routes auth\nfunc init() {}"},
		{"empty/.gitkeep", ""},
	} {
		full := filepath.Join(ws, f.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := &fakeSearcher{}
	tool := &kaiSearchTool{
		workspace: ws,
		set:       projects.Single(ws),
		db:        fs,
	}

	resp, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"query":"handleAuth"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "indexed") {
		t.Errorf("expected backfill note on first call, got:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "handler.go") {
		t.Errorf("expected handler.go in results, got:\n%s", resp.Content)
	}

	// Second call: no backfill note, results only.
	resp2, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"query":"handleAuth"}`,
	})
	if strings.Contains(resp2.Content, "indexed") {
		t.Errorf("second call should not re-index; got:\n%s", resp2.Content)
	}
}

// TestKaiSearch_SkipsBinaryAndOversize verifies the indexer doesn't
// burn cycles on files outside the text-extension allowlist or on
// pathologically large files (vendored bundles, lockfiles). Without
// this guard the FTS5 token budget gets spent on noise.
func TestKaiSearch_SkipsBinaryAndOversize(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "code.go"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "image.png"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.go"), make([]byte, maxIndexableSize+1), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := &fakeSearcher{}
	tool := &kaiSearchTool{workspace: ws, set: projects.Single(ws), db: fs}
	_, _ = tool.Run(context.Background(), ToolCall{Input: `{"query":"needle"}`})

	if got := fs.FileTextCount(); got != 1 {
		t.Errorf("indexed %d files, want 1 (code.go only)", got)
	}
}

// TestKaiSearch_EmptyQueryRejected pins the input-validation path:
// an empty query string is a user error (the agent is probably
// constructing a malformed call), and we return a clear message
// rather than burn an FTS round-trip on garbage.
func TestKaiSearch_EmptyQueryRejected(t *testing.T) {
	tool := &kaiSearchTool{workspace: t.TempDir(), db: &fakeSearcher{}}
	resp, _ := tool.Run(context.Background(), ToolCall{Input: `{"query":""}`})
	if !resp.IsError {
		t.Errorf("expected error for empty query")
	}
}

// TestKaiSearch_EnrichesWithOwningSymbol pins the differentiator
// versus rg: each FTS hit must carry the enclosing function name
// and the match line, joined from the graph. Without this kai_search
// is "rg with a different backend"; with it, results read like
// "in handleAuth():12" instead of "src/handler.go:12:match" and the
// agent can act semantically (call kai_callers on the symbol, etc.)
// without an extra round-trip.
func TestKaiSearch_EnrichesWithOwningSymbol(t *testing.T) {
	ws := t.TempDir()
	body := strings.Join([]string{
		"package main",             // line 1
		"",                         // line 2
		"func unrelated() {}",      // line 3
		"",                         // line 4
		"func handleAuth() {",      // line 5
		"    needle := \"hidden\"", // line 6 — match here
		"    _ = needle",           // line 7
		"}",                        // line 8
	}, "\n")
	if err := os.WriteFile(filepath.Join(ws, "h.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	fg := newFakeKaiGraph()
	fileNode := fg.addFile("h.go")
	// handleAuth lives on lines 5..8 (0-based: 4..7).
	addSymbolWithRange(fg, "handleAuth", "function", fileNode, 4, 7)
	addSymbolWithRange(fg, "unrelated", "function", fileNode, 2, 2)

	fs := &fakeSearcher{}
	_ = fs.IndexFile(filepath.Base(ws), "h.go", body)
	tool := &kaiSearchTool{
		workspace: ws,
		set:       projects.Single(ws),
		db:        fs,
		grapher:   fg,
	}

	resp, _ := tool.Run(context.Background(), ToolCall{Input: `{"query":"needle"}`})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "in handleAuth") {
		t.Errorf("expected enrichment to surface handleAuth, got:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "h.go:6") {
		t.Errorf("expected match line 6 in output, got:\n%s", resp.Content)
	}
}

// addSymbolWithRange extends the test helper so we can pin enrichment
// to specific line ranges. The base addSymbol in kai_test.go omits
// the range because the older symbol-aware tools didn't filter by
// it; kai_search does.
func addSymbolWithRange(f *fakeKaiGraph, name, kind string, file *graph.Node, startLine, endLine int) {
	symID := []byte("sym:" + name)
	f.nodes[string(symID)] = &graph.Node{
		ID:   symID,
		Kind: graph.KindSymbol,
		Payload: map[string]interface{}{
			"fqName": name,
			"kind":   kind,
			"range": map[string]interface{}{
				"start": []interface{}{float64(startLine), float64(0)},
				"end":   []interface{}{float64(endLine), float64(0)},
			},
		},
	}
	edge := &graph.Edge{Src: symID, Dst: file.ID}
	key := string(file.ID)
	f.definesInByFile[key] = append(f.definesInByFile[key], edge)
}

// TestKaiSearch_MultiRootLazyBackfillsNewProjects pins the 2026-05-21
// fix: when a workspace already has FTS rows for the primary (from
// an earlier single-root session) AND a new secondary project gets
// added, the next search must backfill the secondary's files rather
// than short-circuiting on "total count > 0". Before the fix, the
// legacy once.Do + FileTextCount > 0 path bailed out, leaving
// secondary content invisible to FTS until a manual re-index.
func TestKaiSearch_MultiRootLazyBackfillsNewProjects(t *testing.T) {
	parent := t.TempDir()
	// Two project roots with one go file each.
	kaiDir := filepath.Join(parent, "kai")
	serverDir := filepath.Join(parent, "kai-server")
	for path, body := range map[string]string{
		filepath.Join(kaiDir, "client.go"):     "package main\nfunc fetchSearch() {}",
		filepath.Join(serverDir, "handler.go"): "package main\n// proxies search\nfunc Search() {}",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate the bug's initial state: kai is already indexed but
	// kai-server isn't. (As if a previous single-root session
	// populated the primary, then the workspace gained a sibling.)
	fs := &fakeSearcher{}
	_ = fs.IndexFile("kai", "client.go", "package main\nfunc fetchSearch() {}")
	if fs.CountFileTextForProject("kai") != 1 {
		t.Fatalf("test setup: kai should have 1 indexed file, got %d", fs.CountFileTextForProject("kai"))
	}
	if fs.CountFileTextForProject("kai-server") != 0 {
		t.Fatalf("test setup: kai-server should start at 0 indexed files, got %d", fs.CountFileTextForProject("kai-server"))
	}

	set := projects.New(parent, []*projects.Project{
		{Name: "kai", Path: kaiDir},
		{Name: "kai-server", Path: serverDir},
	})
	tool := &kaiSearchTool{
		workspace: kaiDir,
		set:       set,
		db:        fs,
	}

	// A search that targets kai-server content. Before the fix, this
	// would return zero hits (kai-server's file isn't in the index)
	// and the backfill would skip because the total row count was
	// already > 0 (kai's row).
	resp, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"query":"Search"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	// After the fix: ensureProjectsBackfilled should have detected
	// kai-server's zero count and indexed it. The search should
	// surface handler.go.
	if !strings.Contains(resp.Content, "handler.go") {
		t.Errorf("expected handler.go in results after lazy backfill, got:\n%s", resp.Content)
	}
	// Indexing note should mention the newly-indexed file.
	if !strings.Contains(resp.Content, "indexed") {
		t.Errorf("expected an 'indexed N files' note when lazy backfill ran, got:\n%s", resp.Content)
	}
	// And kai-server should now have its file in the index.
	if got := fs.CountFileTextForProject("kai-server"); got == 0 {
		t.Errorf("kai-server should have non-zero indexed files after lazy backfill, got %d", got)
	}
}

// TestKaiSearch_MultiRootSkipsAlreadyIndexedProjects confirms the
// other half of the fix: ensureProjectsBackfilled doesn't re-walk
// projects that already have rows. The legacy bug was "first backfill
// covers nothing new"; the inverse bug would be "every search
// re-walks everything." Verify only the missing project gets walked.
func TestKaiSearch_MultiRootSkipsAlreadyIndexedProjects(t *testing.T) {
	parent := t.TempDir()
	kaiDir := filepath.Join(parent, "kai")
	serverDir := filepath.Join(parent, "kai-server")
	for _, p := range []string{kaiDir, serverDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "f.go"), []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := &fakeSearcher{}
	// Both projects already covered.
	_ = fs.IndexFile("kai", "f.go", "package x")
	_ = fs.IndexFile("kai-server", "f.go", "package x")
	set := projects.New(parent, []*projects.Project{
		{Name: "kai", Path: kaiDir},
		{Name: "kai-server", Path: serverDir},
	})
	tool := &kaiSearchTool{workspace: kaiDir, set: set, db: fs}

	resp, _ := tool.Run(context.Background(), ToolCall{Input: `{"query":"package"}`})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	// Note should be absent because no backfill ran.
	if strings.Contains(resp.Content, "indexed") {
		t.Errorf("no backfill should have run; got 'indexed' note in: %s", resp.Content)
	}
}

func TestTruncateSnippet(t *testing.T) {
	if got := truncateSnippet("short", 240); got != "short" {
		t.Errorf("under cap should be unchanged, got %q", got)
	}
	long := strings.Repeat("a", 500)
	got := truncateSnippet(long, 240)
	if len(got) > 244 { // 240 + ellipsis (3 bytes) + slack
		t.Errorf("over cap not trimmed: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("trimmed snippet should end with ellipsis, got %q", got[len(got)-5:])
	}
	// Multi-byte safety: cutting mid-rune must not corrupt output.
	multi := strings.Repeat("é", 200) // 2 bytes each
	out := truncateSnippet(multi, 51)
	if !utf8.ValidString(out) {
		t.Errorf("truncation split a multi-byte rune: %q", out)
	}
}

// TestKaiSearch_ResultByteCap: a search returning many hits must be
// bounded by maxSearchResultBytes with a "…N more hit(s)" suffix, not
// dumped verbatim into the model's context.
func TestKaiSearch_ResultByteCap(t *testing.T) {
	ws := t.TempDir()
	proj := filepath.Base(ws)
	fs := &fakeSearcher{}
	const n = 80
	for i := 0; i < n; i++ {
		// Long path so a modest hit count clears the 4 KB cap.
		p := fmt.Sprintf("internal/pkg/subsystem/component/very/long/path/file_%03d_handler.go", i)
		full := filepath.Join(ws, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("needle here"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = fs.IndexFile(proj, p, "needle here")
	}
	tool := &kaiSearchTool{workspace: ws, set: projects.Single(ws), db: fs}
	resp, _ := tool.Run(context.Background(), ToolCall{Input: `{"query":"needle","limit":200}`})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "more hit(s) not shown") {
		t.Errorf("expected a truncation suffix; output not capped:\n%.300s", resp.Content)
	}
	if len(resp.Content) > maxSearchResultBytes+400 { // header + suffix slack
		t.Errorf("result exceeded byte cap: len=%d (cap=%d)", len(resp.Content), maxSearchResultBytes)
	}
}
