package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
	"github.com/kaicontext/kai-engine/workspace"
)

// TestBridgeImport_IgnoresCheckedOutWorkspace: a git commit made while a
// workspace is checked out is trunk history. The import must advance
// snap.latest and map git.<sha> to the new snapshot, and must not stage the
// commit into the workspace. Before the fix the commit was absorbed into the
// workspace (its head became the commit's tree) and git.<sha> pointed at the
// snapshot from BEFORE the commit.
func TestBridgeImport_IgnoresCheckedOutWorkspace(t *testing.T) {
	setupBridgeRepo(t)
	first := gitHeadSHA(t)
	if err := runBridgeImport(nil, []string{first}); err != nil {
		t.Fatalf("import first: %v", err)
	}

	db, err := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	refs := ref.NewRefManager(db)
	base, err := refs.Get("snap.latest")
	if err != nil || base == nil {
		db.Close()
		t.Fatalf("no snap.latest after first import: %v", err)
	}
	ws, err := workspace.NewManager(db).Create("feat/x", base.TargetID, "")
	if err != nil {
		db.Close()
		t.Fatalf("create workspace: %v", err)
	}
	wsHead := ws.HeadSnapshot
	db.Close()
	if err := setCurrentWorkspace("feat/x"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile("hello.txt", []byte("hello, trunk\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"git", "commit", "-qam", "trunk change"}} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	second := gitHeadSHA(t)
	if err := runBridgeImport(nil, []string{second}); err != nil {
		t.Fatalf("import second: %v", err)
	}

	db, err = graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	refs = ref.NewRefManager(db)

	latest, _ := refs.Get("snap.latest")
	if latest == nil || bytes.Equal(latest.TargetID, base.TargetID) {
		t.Fatal("snap.latest did not advance for a commit made with a workspace checked out")
	}
	mapped, _ := refs.Get("git." + second[:12])
	if mapped == nil || !bytes.Equal(mapped.TargetID, latest.TargetID) {
		t.Fatalf("git.%s does not point at the new snap.latest", second[:12])
	}
	got, err := workspace.NewManager(db).Get("feat/x")
	if err != nil || got == nil {
		t.Fatalf("workspace lookup: %v", err)
	}
	if !bytes.Equal(got.HeadSnapshot, wsHead) || len(got.OpenChangeSets) != 0 {
		t.Fatalf("trunk commit was staged into workspace feat/x (head moved or changesets recorded: %d)", len(got.OpenChangeSets))
	}
	if cur, _ := getCurrentWorkspace(); cur != "feat/x" {
		t.Fatalf("current workspace changed to %q", cur)
	}
}

// TestBridgeImport_IncludesLiveSyncedPeerEdits: default capture reverts
// ledgered live-sync peer contributions out of the snapshot, but a bridge
// import records a git commit and must contain exactly what was committed.
// Before the fix a commit whose file matched a ledgered peer version was
// recorded with the file's pre-peer content.
func TestBridgeImport_IncludesLiveSyncedPeerEdits(t *testing.T) {
	setupBridgeRepo(t)
	if err := runBridgeImport(nil, []string{gitHeadSHA(t)}); err != nil {
		t.Fatalf("import first: %v", err)
	}

	committed := []byte("hello, from a live-synced peer\n")
	db, err := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	baseDigest, err := db.WriteObject([]byte("hello\n"))
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	peerDigest, err := db.WriteObject(committed)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	ledger := `{"hello.txt":{"base_digest":"` + baseDigest + `","peer_digest":"` + peerDigest + `"}}`
	if err := os.WriteFile(filepath.Join(".kai", "live-synced.json"), []byte(ledger), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile("hello.txt", committed, 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "commit", "-qam", "take the peer's edit").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	sha := gitHeadSHA(t)
	if err := runBridgeImport(nil, []string{sha}); err != nil {
		t.Fatalf("import second: %v", err)
	}

	db, err = graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	mapped, _ := ref.NewRefManager(db).Get("git." + sha[:12])
	if mapped == nil {
		t.Fatalf("no git.%s ref", sha[:12])
	}
	edges, err := db.GetEdges(mapped.TargetID, graph.EdgeHasFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		node, err := db.GetNode(e.Dst)
		if err != nil || node == nil || node.Payload["path"] != "hello.txt" {
			continue
		}
		if got, _ := node.Payload["digest"].(string); got != peerDigest {
			t.Fatalf("git.%s records hello.txt as %.12s, want the committed content %.12s", sha[:12], got, peerDigest)
		}
		return
	}
	t.Fatalf("hello.txt missing from git.%s", sha[:12])
}
