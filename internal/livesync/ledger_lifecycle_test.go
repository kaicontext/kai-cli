package livesync

import (
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
)

// A cleared ledger must stay cleared: the engine used to cache the ledger in
// memory and rewrite the whole cached map on its next peer apply, bringing
// back entries another process (workspace switch, `kai capture --all`) had
// cleared.
func TestRecordPeerContributionDoesNotResurrectClearedEntries(t *testing.T) {
	dir := t.TempDir()
	db, err := graph.Open(filepath.Join(dir, "db.sqlite"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := &Engine{kaiDir: dir, db: db}

	e.recordPeerContribution("main.go", []byte("mine\n"), []byte("peer\n"))
	if m, ok := LoadSyncedLedger(dir); !ok || len(m) != 1 {
		t.Fatalf("ledger after first apply = %v, want main.go", m)
	}
	if err := ClearSyncedLedger(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadSyncedLedger(dir); ok {
		t.Fatal("ledger still present after ClearSyncedLedger")
	}

	e.recordPeerContribution("scan.go", []byte("mine\n"), []byte("peer\n"))
	m, ok := LoadSyncedLedger(dir)
	if !ok || len(m) != 1 {
		t.Fatalf("ledger = %v, want only scan.go", m)
	}
	if _, back := m["main.go"]; back {
		t.Fatal("cleared main.go entry was resurrected by the next peer apply")
	}
	if err := ClearSyncedLedger(dir); err != nil {
		t.Fatalf("clearing an already-cleared ledger: %v", err)
	}
	if err := ClearSyncedLedger(dir); err != nil {
		t.Fatalf("clearing a missing ledger must be a no-op: %v", err)
	}
}
