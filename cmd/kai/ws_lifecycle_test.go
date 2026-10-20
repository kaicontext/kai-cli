package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
	"github.com/kaicontext/kai-engine/workspace"
)

func writeLedger(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(".kai", "live-synced.json"),
		[]byte(`{"hello.txt":{"base_digest":"","peer_digest":"00"}}`), 0644); err != nil {
		t.Fatal(err)
	}
}

func ledgerExists() bool {
	_, err := os.Stat(filepath.Join(".kai", "live-synced.json"))
	return err == nil
}

// setupWsRepo: a bridge repo with one imported snapshot and two workspaces,
// "feat/x" checked out.
func setupWsRepo(t *testing.T) {
	t.Helper()
	setupBridgeRepo(t)
	if err := runBridgeImport(nil, []string{gitHeadSHA(t)}); err != nil {
		t.Fatal(err)
	}
	db, err := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base, err := ref.NewRefManager(db).Get("snap.latest")
	if err != nil || base == nil {
		t.Fatalf("no snap.latest: %v", err)
	}
	for _, name := range []string{"feat/x", "feat/y"} {
		if _, err := workspace.NewManager(db).Create(name, base.TargetID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := setCurrentWorkspace("feat/x"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAI_NO_AUTOSYNC", "1")
	wsName = ""
}

func TestResolveWsArg(t *testing.T) {
	setupWsRepo(t)
	if got, err := resolveWsArg([]string{"feat/y"}, false); err != nil || got != "feat/y" {
		t.Fatalf("positional = %q, %v", got, err)
	}
	wsName = "feat/y"
	if got, _ := resolveWsArg(nil, true); got != "feat/y" {
		t.Fatalf("--ws = %q, want feat/y", got)
	}
	wsName = ""
	if got, _ := resolveWsArg(nil, true); got != "feat/x" {
		t.Fatalf("default = %q, want the checked-out feat/x", got)
	}
	if _, err := resolveWsArg(nil, false); err == nil {
		t.Fatal("destructive subcommands must not default to the checked-out workspace")
	}
}

// Closing the checked-out workspace used to leave .kai/workspace pointing at
// it (the pre-commit hook's capture then tried to stage into a closed
// workspace) and its live-sync ledger in place.
func TestWsCloseCurrentReleasesMarkerAndLedger(t *testing.T) {
	setupWsRepo(t)
	writeLedger(t)

	if err := runWsClose(nil, []string{"feat/y"}); err != nil {
		t.Fatalf("close feat/y: %v", err)
	}
	if cur, _ := getCurrentWorkspace(); cur != "feat/x" || !ledgerExists() {
		t.Fatalf("closing another workspace changed state: current=%q ledger=%v", cur, ledgerExists())
	}

	if err := runWsClose(nil, []string{"feat/x"}); err != nil {
		t.Fatalf("close feat/x: %v", err)
	}
	if cur, _ := getCurrentWorkspace(); cur != "" {
		t.Fatalf("current workspace = %q after closing it, want none", cur)
	}
	if ledgerExists() {
		t.Fatal("live-synced ledger survived closing the checked-out workspace")
	}
}

func TestWsDeleteCurrentReleasesMarkerAndLedger(t *testing.T) {
	setupWsRepo(t)
	writeLedger(t)
	if err := runWsDelete(nil, []string{"feat/x"}); err != nil {
		t.Fatalf("delete feat/x: %v", err)
	}
	if cur, _ := getCurrentWorkspace(); cur != "" {
		t.Fatalf("current workspace = %q after deleting it, want none", cur)
	}
	if ledgerExists() {
		t.Fatal("live-synced ledger survived deleting the checked-out workspace")
	}
}

// A capture that keeps peer content (--all) records it as the user's own,
// so the ledger is dropped; a default capture keeps it.
func TestCaptureAllClearsLedger(t *testing.T) {
	setupBridgeRepo(t)
	t.Setenv("KAI_NO_AUTOSYNC", "1")
	writeLedger(t)
	captureAll = false
	if err := runCapture(nil, nil); err != nil {
		t.Fatalf("default capture: %v", err)
	}
	if !ledgerExists() {
		t.Fatal("default capture must keep the ledger")
	}
	captureAll = true
	defer func() { captureAll = false }()
	if err := runCapture(nil, nil); err != nil {
		t.Fatalf("capture --all: %v", err)
	}
	if ledgerExists() {
		t.Fatal("capture --all must clear the ledger")
	}
}
