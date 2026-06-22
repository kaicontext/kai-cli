package graph

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFileText_IndexAndSearch is the round-trip smoke test for the
// FTS5 integration. Confirms three things in one shot:
//  1. modernc.org/sqlite ships with FTS5 (no build-tag dance needed)
//  2. IndexFile / SearchText round-trip a query
//  3. snippet() returns the «match» markers we rely on for rendering
//
// If any of these fail, the whole semantic-rg story is dead — better
// to find out here than in the agent tool path where the failure
// mode would be "search returns nothing" with no obvious cause.
func TestFileText_IndexAndSearch(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "graph.db"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	files := map[string]string{
		"src/handler.go":   "func handleAuth() { reasoning_format := \"hidden\" }",
		"src/router.go":    "// routes auth requests to handleAuth",
		"src/middleware.go": "func authMiddleware() {}",
		"docs/auth.md":     "# Auth\nThe auth flow uses bcrypt for password hashing.",
	}
	for path, body := range files {
		if err := db.IndexFile("kai-cli", path, body); err != nil {
			t.Fatalf("IndexFile %s: %v", path, err)
		}
	}

	if got := db.FileTextCount(); got != len(files) {
		t.Errorf("FileTextCount = %d, want %d", got, len(files))
	}

	hits, err := db.SearchText("reasoning_format", "", 10)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchText returned zero hits for 'reasoning_format' — FTS5 may not be enabled in this build")
	}
	if hits[0].Path != "src/handler.go" {
		t.Errorf("top hit = %s, want src/handler.go", hits[0].Path)
	}
	if !strings.Contains(hits[0].Snippet, "«") || !strings.Contains(hits[0].Snippet, "»") {
		t.Errorf("snippet missing match markers: %q", hits[0].Snippet)
	}
}

// TestFileText_ReplaceIsIdempotent pins that IndexFile twice with
// different bodies leaves only one row — and the latest body wins.
// Without this guarantee, the backfill path would double-index every
// file on every run and SearchText would return phantom matches from
// stale bodies.
func TestFileText_ReplaceIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "graph.db"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.IndexFile("p", "a.go", "needle_v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexFile("p", "a.go", "needle_v2"); err != nil {
		t.Fatal(err)
	}

	if got := db.FileTextCount(); got != 1 {
		t.Errorf("FileTextCount = %d, want 1 after replace", got)
	}
	hits, err := db.SearchText("needle_v1", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("stale body still matched: %+v", hits)
	}
	hits, err = db.SearchText("needle_v2", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("new body should match, got %d hits", len(hits))
	}
}

// TestFileText_ProjectFilter confirms the multi-root scoping: a
// query restricted to one project doesn't leak hits from another.
// Multi-root is the whole reason this layer exists alongside rg —
// rg would happily return cross-project matches that the agent
// then has to filter out of its mental model.
func TestFileText_ProjectFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "graph.db"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_ = db.IndexFile("kai-cli", "x.go", "the_thing is here")
	_ = db.IndexFile("kai-server", "y.go", "the_thing is also here")

	hits, err := db.SearchText("the_thing", "kai-cli", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Project != "kai-cli" {
		t.Errorf("project filter leaked: got %+v", hits)
	}
}
