package workspace

import (
	"testing"

	"kai/internal/graph"
	"kai/internal/util"
)

func TestCherryPickAppliesChangeSet(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})
	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v1"})

	csID := insertChangeSet(t, db, baseSnap, headSnap)
	mgr := NewManager(db)

	result, err := mgr.CherryPick(csID, targetSnap)
	if err != nil {
		t.Fatalf("cherry-pick: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}
	if result.ResultSnapshot == nil || result.ResultChangeSet == nil {
		t.Fatalf("expected result snapshot and changeset")
	}
	if result.AppliedFiles != 1 {
		t.Fatalf("expected 1 applied file, got %d", result.AppliedFiles)
	}

	files, err := mgr.getSnapshotFileMap(result.ResultSnapshot)
	if err != nil {
		t.Fatalf("result files: %v", err)
	}
	if got := files["a.txt"]; got != "v2" {
		t.Fatalf("expected merged file digest v2, got %q", got)
	}
}

func TestCherryPickDetectsConflict(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})
	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v3"})

	csID := insertChangeSet(t, db, baseSnap, headSnap)
	mgr := NewManager(db)

	result, err := mgr.CherryPick(csID, targetSnap)
	if err != nil {
		t.Fatalf("cherry-pick: %v", err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatalf("expected conflicts")
	}
	if result.ResultSnapshot != nil {
		t.Fatalf("expected no result snapshot on conflict")
	}
}

func TestCherryPickMissingChangeSet(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v1"})

	if _, err := mgr.CherryPick([]byte("missing"), targetSnap); err == nil {
		t.Fatalf("expected error for missing changeset")
	}
}

func TestCherryPickMissingBaseHead(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	payload := map[string]interface{}{
		"title":     "bad",
		"createdAt": util.NowMs(),
	}
	csID, err := db.InsertNode(tx, graph.KindChangeSet, payload)
	if err != nil {
		t.Fatalf("insert changeset: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit changeset: %v", err)
	}

	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v1"})
	mgr := NewManager(db)
	if _, err := mgr.CherryPick(csID, targetSnap); err == nil {
		t.Fatalf("expected error for missing base/head")
	}
}
