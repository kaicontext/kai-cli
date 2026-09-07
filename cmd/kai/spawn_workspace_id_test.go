package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/kaipath"
	"github.com/kaicontext/kai-engine/workspace"
)

func TestLookupWorkspaceIDResolvesGitKaiDirectory(t *testing.T) {
	t.Setenv("KAI_DIR", "")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	kaiData := kaipath.Resolve(root)
	if want := filepath.Join(root, ".git", "kai"); kaiData != want {
		t.Fatalf("resolved kai dir = %q, want %q", kaiData, want)
	}
	if err := os.MkdirAll(kaiData, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := graph.Open(filepath.Join(kaiData, dbFile), filepath.Join(kaiData, objectsDir))
	if err != nil {
		t.Fatal(err)
	}
	base, err := db.InsertNodeDirect(graph.KindSnapshot, map[string]interface{}{"source": "test"})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	mgr := workspace.NewManager(db)
	ws, err := mgr.Create("s-sibling", base, "test sibling")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := lookupWorkspaceID(root, "s-sibling")
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(ws.ID); got != want {
		t.Fatalf("workspace id = %q, want %q", got, want)
	}
}
