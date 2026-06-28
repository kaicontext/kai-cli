package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/filesource"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/snapshot"
	"github.com/kaicontext/kai-engine/util"
)

// memSource is a tiny in-memory FileSource for tests — a fixed set of files
// with no filesystem or git backing — so snapshot creation needs no network or
// working tree of its own.
type memSource struct {
	files []*filesource.FileInfo
	id    string
}

func (m *memSource) GetFiles() ([]*filesource.FileInfo, error) { return m.files, nil }
func (m *memSource) GetFile(path string) (*filesource.FileInfo, error) {
	for _, f := range m.files {
		if f.Path == path {
			return f, nil
		}
	}
	return nil, os.ErrNotExist
}
func (m *memSource) Identifier() string { return m.id }
func (m *memSource) SourceType() string { return "directory" }

// newTestDB opens a throwaway graph DB under t.TempDir().
func newTestDB(t *testing.T) *graph.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := graph.Open(filepath.Join(dir, "db.sqlite"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatalf("graph.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestOnDiskContentMatches covers the blob-skip decision pull uses when the
// remote can't supply a blob: skip only when the file on disk is already exactly
// the target content.
func TestOnDiskContentMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.js")
	content := []byte("function f() { return 3; }\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	if !onDiskContentMatches(path, util.Blake3HashHex(content)) {
		t.Errorf("matching content: got false, want true")
	}
	if onDiskContentMatches(path, util.Blake3HashHex([]byte("something else"))) {
		t.Errorf("differing content: got true, want false")
	}
	if onDiskContentMatches(filepath.Join(dir, "missing.js"), util.Blake3HashHex(content)) {
		t.Errorf("missing file: got true, want false")
	}
}

// TestMaterializeWorkingTree is the F-10 regression: materializing a pulled
// snapshot whose file content differs from disk must overwrite the working-tree
// file, and must be a no-op once the tree already matches the snapshot.
func TestMaterializeWorkingTree(t *testing.T) {
	db := newTestDB(t)

	// Build a snapshot containing a.js = v3.
	v3 := []byte("function f() { return 3; }\n")
	snapID, err := snapshot.NewCreator(db, nil).CreateSnapshot(&memSource{
		id:    "test-v3",
		files: []*filesource.FileInfo{{Path: "a.js", Content: v3, Lang: "js"}},
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// A stale working tree (a.js = v1) — the F-10 starting state after a pull
	// advanced the graph but never touched the files.
	work := t.TempDir()
	aPath := filepath.Join(work, "a.js")
	if err := os.WriteFile(aPath, []byte("function f() { return 1; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// First materialize: rewrites the stale file to the snapshot content.
	written, err := materializeWorkingTree(db, snapID, work)
	if err != nil {
		t.Fatalf("materializeWorkingTree: %v", err)
	}
	if written != 1 {
		t.Errorf("first materialize wrote %d file(s), want 1", written)
	}
	if got, _ := os.ReadFile(aPath); string(got) != string(v3) {
		t.Errorf("a.js after materialize = %q, want %q", got, v3)
	}

	// Second materialize: tree already current -> no writes (idempotent).
	written, err = materializeWorkingTree(db, snapID, work)
	if err != nil {
		t.Fatalf("materializeWorkingTree (idempotent): %v", err)
	}
	if written != 0 {
		t.Errorf("second materialize wrote %d file(s), want 0 (already current)", written)
	}
}
