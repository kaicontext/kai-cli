package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
	"github.com/kaicontext/kai-engine/remote"
)

func openKaiDB(t *testing.T) *graph.DB {
	t.Helper()
	db, err := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestIsHistoryRef(t *testing.T) {
	for name, want := range map[string]bool{
		"tag.v0.1":                 true,
		"git.6421f1d5fd34":         true,
		"snap.20261021T081052.275": true,
		"git.HEAD":                 false,
		"snap.latest":              false,
		"snap.main":                false,
		"cs.latest":                false,
		"ws.feat.head":             false,
	} {
		if got := isHistoryRef(name); got != want {
			t.Errorf("isHistoryRef(%q) = %v, want %v", name, got, want)
		}
	}
}

// Fetched history becomes local refs (visible to kai log / kai tag list),
// without moving any ref that already exists.
func TestAdoptRemoteHistory(t *testing.T) {
	setupBridgeRepo(t)
	if err := runBridgeImport(nil, []string{gitHeadSHA(t)}); err != nil {
		t.Fatal(err)
	}
	db := openKaiDB(t)
	defer db.Close()
	refs := ref.NewRefManager(db)
	latest, _ := refs.Get("snap.latest")
	other, err := db.InsertNodeDirect(graph.KindSnapshot, map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := refs.Set("tag.mine", latest.TargetID, ref.KindSnapshot); err != nil {
		t.Fatal(err)
	}
	n := adoptRemoteHistory(refs, []*remote.RefEntry{
		{Name: "tag.v0.1", Target: other},
		{Name: "tag.mine", Target: other},
		{Name: "snap.20261021T081052.275", Target: other},
		{Name: "snap.latest", Target: other},
		{Name: "git.HEAD", Target: other},
	})
	if n != 2 {
		t.Fatalf("adopted %d refs, want 2 (tag.v0.1 and the history snap)", n)
	}
	if r, _ := refs.Get("tag.v0.1"); r == nil || !bytes.Equal(r.TargetID, other) {
		t.Error("tag.v0.1 not adopted")
	}
	if r, _ := refs.Get("tag.mine"); !bytes.Equal(r.TargetID, latest.TargetID) {
		t.Error("existing local tag was moved")
	}
	if r, _ := refs.Get("snap.latest"); !bytes.Equal(r.TargetID, latest.TargetID) {
		t.Error("snap.latest was moved by adoption")
	}
}

// In a clone the snapshot log is empty, so @snap:last used to resolve to the
// snapshot a capture had just written and the capture reported "no
// changes" (and recorded no changeset) after a real edit.
func TestCaptureSummaryComparesAgainstSnapLatestWithEmptyLog(t *testing.T) {
	setupBridgeRepo(t)
	t.Setenv("KAI_NO_AUTOSYNC", "1")
	if err := runCapture(nil, nil); err != nil {
		t.Fatal(err)
	}
	db := openKaiDB(t)
	if _, err := db.Exec(`DELETE FROM logs`); err != nil { // as after `kai clone`
		t.Fatal(err)
	}
	before := len(changesetIDs(t, db))
	db.Close()

	if err := os.WriteFile("hello.txt", []byte("hello, edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runCapture(nil, nil); err != nil {
		t.Fatal(err)
	}
	db = openKaiDB(t)
	defer db.Close()
	if after := len(changesetIDs(t, db)); after != before+1 {
		t.Fatalf("changesets %d -> %d: the edit was not detected as a change", before, after)
	}
}

func changesetIDs(t *testing.T, db *graph.DB) [][]byte {
	t.Helper()
	nodes, err := db.GetNodesByKind(graph.KindChangeSet)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([][]byte, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

func TestInitGitignoreSeedsOnlyWhenMissing(t *testing.T) {
	chdirTemp(t)
	initGitignore(filepath.Join(".git", "kai"))
	got, _ := os.ReadFile(".gitignore")
	for _, want := range []string{"__pycache__/", "*.pyc", "node_modules/", ".venv/"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("seeded .gitignore lacks %q:\n%s", want, got)
		}
	}

	chdirTemp(t)
	if err := os.WriteFile(".gitignore", []byte("vendor/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	initGitignore(filepath.Join(".git", "kai"))
	if got, _ := os.ReadFile(".gitignore"); string(got) != "vendor/\n" {
		t.Errorf("existing .gitignore was changed: %q", got)
	}
}

func TestPurgeMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{".env", ".env", true},
		{"__pycache__/**", "__pycache__/kv.cpython-312.pyc", true},
		{"**/*.pem", "certs/a/b.pem", true},
		{"*.pyc", "kv.pyc", true},
		{"__pycache__/**", "src/__pycache__x.py", false},
		{".env", "app/.env.example", false},
	} {
		if got := purgeMatch([]string{tc.pattern}, tc.path); got != tc.want {
			t.Errorf("purgeMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// Re-importing history from git (kai init, kai import --git) must not bring
// back files `kai purge` removed.
func TestGitImportSkipsPurgedFiles(t *testing.T) {
	setupBridgeRepo(t)
	if err := os.WriteFile("secret.env", []byte("TOKEN=x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := exec.Command("git", "commit", "-qm", "leak").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := recordPurgePatterns(".kai", []string{"secret.env"}); err != nil {
		t.Fatal(err)
	}
	if got := loadPurgePatterns(".kai"); len(got) != 1 || got[0] != "secret.env" {
		t.Fatalf("recorded patterns = %v", got)
	}
	db := openKaiDB(t)
	defer db.Close()
	initMode = true
	defer func() { initMode = false }()
	if err := runGitImport(db); err != nil {
		t.Fatal(err)
	}
	r, _ := ref.NewRefManager(db).Get("git.HEAD")
	edges, _ := db.GetEdges(r.TargetID, graph.EdgeHasFile)
	for _, e := range edges {
		if n, _ := db.GetNode(e.Dst); n != nil && n.Payload["path"] == "secret.env" {
			t.Fatal("purged secret.env was re-imported from git")
		}
	}
	if len(edges) == 0 {
		t.Fatal("import produced an empty snapshot")
	}
}
