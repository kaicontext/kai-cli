package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/util"
	"github.com/kaicontext/kai-engine/workspace"
)

// TestLiveSyncScope_NewWorkspaceSameNameGetsNewScope: live sync used to be
// scoped by workspace name, so a workspace re-created under an old name (or
// created independently elsewhere) replayed the earlier workspace's live
// edits onto its files. The scope now carries the workspace ID.
func TestLiveSyncScope_NewWorkspaceSameNameGetsNewScope(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(filepath.Join(".kai", "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base, err := db.InsertNodeDirect(graph.KindSnapshot, map[string]interface{}{"fileCount": 0})
	if err != nil {
		t.Fatal(err)
	}
	mgr := workspace.NewManager(db)

	first, err := mgr.Create("feat/json", base, "")
	if err != nil {
		t.Fatal(err)
	}
	s1 := liveSyncScope(db, "feat/json")
	if s1 != "feat/json#"+util.BytesToHex(first.ID) {
		t.Fatalf("scope = %q, want feat/json#<id>", s1)
	}
	if err := mgr.Delete("feat/json", false); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Create("feat/json", base, ""); err != nil {
		t.Fatal(err)
	}
	s2 := liveSyncScope(db, "feat/json")
	if s2 == s1 || !strings.HasPrefix(s2, "feat/json#") {
		t.Fatalf("re-created workspace scope = %q, previous %q — must differ", s2, s1)
	}

	if got := liveSyncScope(db, ""); got != "" {
		t.Fatalf("repo-wide scope = %q, want empty", got)
	}
	if got := liveSyncScope(db, "nope"); got != "nope" {
		t.Fatalf("unknown workspace scope = %q, want the bare name", got)
	}
}
