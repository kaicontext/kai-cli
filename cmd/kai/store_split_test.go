package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/kaipath"
)

// The session-ingest hook wrote <root>/.kai/loops beside a .git/kai store,
// which flipped kaipath onto an empty second store (riddle, 2026-10-22).
// Loop records go into the store kaipath resolves instead.
func TestLoopRecordLandsInTheResolvedStore(t *testing.T) {
	t.Setenv("KAI_DIR", "")
	root := t.TempDir()
	store := filepath.Join(root, ".git", "kai")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, kaipath.StoreFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	writeLoopRecord(&loopRecord{ID: "s1", Cwd: root})

	if _, err := os.Stat(filepath.Join(store, "loops", "s1.json")); err != nil {
		t.Fatalf("loop record not in the .git/kai store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".kai")); !os.IsNotExist(err) {
		t.Fatalf("ingest created <root>/.kai beside a .git/kai store (err=%v)", err)
	}
}

func TestSplitStoreMessageNamesBothStores(t *testing.T) {
	root := t.TempDir()
	stores := []string{filepath.Join(root, ".kai"), filepath.Join(root, ".git", "kai")}
	msg := splitStoreMessage(stores)
	for _, s := range stores {
		if !strings.Contains(msg, filepath.Join(s, kaipath.StoreFile)) {
			t.Errorf("message does not name %s:\n%s", s, msg)
		}
	}
	if !strings.Contains(msg, "mv "+stores[1]) {
		t.Errorf("message gives no way out:\n%s", msg)
	}
}

func TestSplitStoreExemptKeepsDiagnosisAvailable(t *testing.T) {
	for _, name := range []string{"doctor", "version", "ingest"} {
		if !splitStoreExempt[name] {
			t.Errorf("%s must run on a split project", name)
		}
	}
	for _, name := range []string{"capture", "push", "status", "checkpoint"} {
		if splitStoreExempt[name] {
			t.Errorf("%s must refuse on a split project", name)
		}
	}
}
