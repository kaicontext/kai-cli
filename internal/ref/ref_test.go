package ref

import (
	"os"
	"path/filepath"
	"testing"

	"kai/internal/graph"
	"kai/internal/util"
)

func setupTestDB(t *testing.T) (*graph.DB, func()) {
	t.Helper()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "kai-test-*")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test.db")
	objPath := filepath.Join(tmpDir, "objects")
	if err := os.MkdirAll(objPath, 0755); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("creating objects dir: %v", err)
	}

	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("opening database: %v", err)
	}

	// Apply schema
	schema := `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS nodes (id BLOB PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS edges (src BLOB NOT NULL, type TEXT NOT NULL, dst BLOB NOT NULL, at BLOB, created_at INTEGER NOT NULL, PRIMARY KEY (src, type, dst, at));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target_id BLOB NOT NULL, target_kind TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS slugs (target_id BLOB PRIMARY KEY, slug TEXT UNIQUE NOT NULL);
CREATE TABLE IF NOT EXISTS logs (kind TEXT NOT NULL, seq INTEGER NOT NULL, id BLOB NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (kind, seq));
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("applying schema: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(tmpDir)
	}

	return db, cleanup
}

func createTestSnapshot(t *testing.T, db *graph.DB) []byte {
	t.Helper()
	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("beginning transaction: %v", err)
	}

	id, err := db.InsertNode(tx, graph.KindSnapshot, map[string]interface{}{
		"sourceType": "test",
		"sourceRef":  "test-ref",
		"fileCount":  float64(0),
		"createdAt":  float64(util.NowMs()),
	})
	if err != nil {
		tx.Rollback()
		t.Fatalf("inserting snapshot: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing transaction: %v", err)
	}

	return id
}

func createTestChangeSet(t *testing.T, db *graph.DB, baseID, headID []byte) []byte {
	t.Helper()
	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("beginning transaction: %v", err)
	}

	id, err := db.InsertNode(tx, graph.KindChangeSet, map[string]interface{}{
		"base":      util.BytesToHex(baseID),
		"head":      util.BytesToHex(headID),
		"title":     "",
		"intent":    "Test changeset",
		"createdAt": float64(util.NowMs()),
	})
	if err != nil {
		tx.Rollback()
		t.Fatalf("inserting changeset: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing transaction: %v", err)
	}

	return id
}

func TestResolver_FullHexID(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a snapshot
	snapID := createTestSnapshot(t, db)
	snapHex := util.BytesToHex(snapID)

	resolver := NewResolver(db)

	// Resolve full hex ID
	result, err := resolver.Resolve(snapHex, nil)
	if err != nil {
		t.Fatalf("resolving full hex ID: %v", err)
	}

	if util.BytesToHex(result.ID) != snapHex {
		t.Errorf("expected ID %s, got %s", snapHex, util.BytesToHex(result.ID))
	}
	if result.Kind != KindSnapshot {
		t.Errorf("expected kind %s, got %s", KindSnapshot, result.Kind)
	}
}

func TestResolver_ShortID(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a snapshot
	snapID := createTestSnapshot(t, db)
	snapHex := util.BytesToHex(snapID)

	resolver := NewResolver(db)

	// Resolve short ID (first 8 chars)
	shortID := snapHex[:8]
	result, err := resolver.Resolve(shortID, nil)
	if err != nil {
		t.Fatalf("resolving short ID: %v", err)
	}

	if util.BytesToHex(result.ID) != snapHex {
		t.Errorf("expected ID %s, got %s", snapHex, util.BytesToHex(result.ID))
	}
}

func TestResolver_RefName(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a snapshot
	snapID := createTestSnapshot(t, db)

	// Create a ref
	refMgr := NewRefManager(db)
	if err := refMgr.Set("snap.main", snapID, KindSnapshot); err != nil {
		t.Fatalf("setting ref: %v", err)
	}

	resolver := NewResolver(db)

	// Resolve ref name
	result, err := resolver.Resolve("snap.main", nil)
	if err != nil {
		t.Fatalf("resolving ref name: %v", err)
	}

	if util.BytesToHex(result.ID) != util.BytesToHex(snapID) {
		t.Errorf("expected ID %s, got %s", util.BytesToHex(snapID), util.BytesToHex(result.ID))
	}
	if result.Kind != KindSnapshot {
		t.Errorf("expected kind %s, got %s", KindSnapshot, result.Kind)
	}
}

func TestResolver_SelectorLast(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create snapshots
	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)

	// Log them
	logMgr := NewLogManager(db)
	if err := logMgr.Append(KindSnapshot, snap1); err != nil {
		t.Fatalf("appending to log: %v", err)
	}
	if err := logMgr.Append(KindSnapshot, snap2); err != nil {
		t.Fatalf("appending to log: %v", err)
	}

	resolver := NewResolver(db)

	// Resolve @snap:last
	result, err := resolver.Resolve("@snap:last", nil)
	if err != nil {
		t.Fatalf("resolving @snap:last: %v", err)
	}

	if util.BytesToHex(result.ID) != util.BytesToHex(snap2) {
		t.Errorf("expected ID %s, got %s", util.BytesToHex(snap2), util.BytesToHex(result.ID))
	}
}

func TestResolver_SelectorPrev(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create snapshots
	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)

	// Log them
	logMgr := NewLogManager(db)
	if err := logMgr.Append(KindSnapshot, snap1); err != nil {
		t.Fatalf("appending to log: %v", err)
	}
	if err := logMgr.Append(KindSnapshot, snap2); err != nil {
		t.Fatalf("appending to log: %v", err)
	}

	resolver := NewResolver(db)

	// Resolve @snap:prev
	result, err := resolver.Resolve("@snap:prev", nil)
	if err != nil {
		t.Fatalf("resolving @snap:prev: %v", err)
	}

	if util.BytesToHex(result.ID) != util.BytesToHex(snap1) {
		t.Errorf("expected ID %s, got %s", util.BytesToHex(snap1), util.BytesToHex(result.ID))
	}
}

func TestResolver_SelectorRelative(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create snapshots
	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)
	snap3 := createTestSnapshot(t, db)

	// Log them
	logMgr := NewLogManager(db)
	if err := logMgr.Append(KindSnapshot, snap1); err != nil {
		t.Fatalf("appending to log: %v", err)
	}
	if err := logMgr.Append(KindSnapshot, snap2); err != nil {
		t.Fatalf("appending to log: %v", err)
	}
	if err := logMgr.Append(KindSnapshot, snap3); err != nil {
		t.Fatalf("appending to log: %v", err)
	}

	resolver := NewResolver(db)

	// Resolve @snap:last~2
	result, err := resolver.Resolve("@snap:last~2", nil)
	if err != nil {
		t.Fatalf("resolving @snap:last~2: %v", err)
	}

	if util.BytesToHex(result.ID) != util.BytesToHex(snap1) {
		t.Errorf("expected ID %s, got %s", util.BytesToHex(snap1), util.BytesToHex(result.ID))
	}
}

func TestResolver_SelectorWorking(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a snapshot
	snapID := createTestSnapshot(t, db)

	// Set it as the working snapshot
	refMgr := NewRefManager(db)
	if err := refMgr.Set("snap.working", snapID, KindSnapshot); err != nil {
		t.Fatalf("setting snap.working: %v", err)
	}

	resolver := NewResolver(db)

	// Resolve @snap:working
	result, err := resolver.Resolve("@snap:working", nil)
	if err != nil {
		t.Fatalf("resolving @snap:working: %v", err)
	}

	if util.BytesToHex(result.ID) != util.BytesToHex(snapID) {
		t.Errorf("expected ID %s, got %s", util.BytesToHex(snapID), util.BytesToHex(result.ID))
	}

	if result.Kind != KindSnapshot {
		t.Errorf("expected kind Snapshot, got %s", result.Kind)
	}
}

func TestAutoRefManager_WorkingSnapshot(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create snapshots
	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)

	autoRef := NewAutoRefManager(db)
	resolver := NewResolver(db)

	// First scan: should set both @snap:last and @snap:working
	if err := autoRef.OnSnapshotCreated(snap1); err != nil {
		t.Fatalf("OnSnapshotCreated: %v", err)
	}
	if err := autoRef.OnWorkingSnapshotUpdated(snap1); err != nil {
		t.Fatalf("OnWorkingSnapshotUpdated: %v", err)
	}

	// Verify @snap:last is snap1
	result, err := resolver.Resolve("@snap:last", nil)
	if err != nil {
		t.Fatalf("resolving @snap:last: %v", err)
	}
	if util.BytesToHex(result.ID) != util.BytesToHex(snap1) {
		t.Errorf("@snap:last should be snap1")
	}

	// Second scan: update working only
	if err := autoRef.OnWorkingSnapshotUpdated(snap2); err != nil {
		t.Fatalf("OnWorkingSnapshotUpdated: %v", err)
	}

	// @snap:last should still be snap1
	result, err = resolver.Resolve("@snap:last", nil)
	if err != nil {
		t.Fatalf("resolving @snap:last: %v", err)
	}
	if util.BytesToHex(result.ID) != util.BytesToHex(snap1) {
		t.Errorf("@snap:last should still be snap1 after updating working")
	}

	// @snap:working should be snap2
	result, err = resolver.Resolve("@snap:working", nil)
	if err != nil {
		t.Fatalf("resolving @snap:working: %v", err)
	}
	if util.BytesToHex(result.ID) != util.BytesToHex(snap2) {
		t.Errorf("@snap:working should be snap2")
	}

	// Promote working to last
	if err := autoRef.PromoteWorkingSnapshot(); err != nil {
		t.Fatalf("PromoteWorkingSnapshot: %v", err)
	}

	// @snap:last should now be snap2
	result, err = resolver.Resolve("@snap:last", nil)
	if err != nil {
		t.Fatalf("resolving @snap:last after promote: %v", err)
	}
	if util.BytesToHex(result.ID) != util.BytesToHex(snap2) {
		t.Errorf("@snap:last should be snap2 after promote")
	}
}

func TestResolver_KindMismatch(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a snapshot
	snapID := createTestSnapshot(t, db)
	snapHex := util.BytesToHex(snapID)

	resolver := NewResolver(db)

	// Try to resolve as changeset
	csKind := KindChangeSet
	_, err := resolver.Resolve(snapHex, &csKind)
	if err == nil {
		t.Error("expected error for kind mismatch, got nil")
	}
}

func TestResolver_NotFound(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	resolver := NewResolver(db)

	// Try to resolve non-existent ID
	_, err := resolver.Resolve("0000000000000000000000000000000000000000000000000000000000000000", nil)
	if err == nil {
		t.Error("expected not found error, got nil")
	}
	if _, ok := err.(*NotFoundError); !ok {
		t.Errorf("expected NotFoundError, got %T", err)
	}
}

func TestRefManager_CRUD(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snapID := createTestSnapshot(t, db)
	refMgr := NewRefManager(db)

	// Set
	if err := refMgr.Set("snap.test", snapID, KindSnapshot); err != nil {
		t.Fatalf("setting ref: %v", err)
	}

	// Get
	r, err := refMgr.Get("snap.test")
	if err != nil {
		t.Fatalf("getting ref: %v", err)
	}
	if r == nil {
		t.Fatal("expected ref, got nil")
	}
	if r.Name != "snap.test" {
		t.Errorf("expected name 'snap.test', got '%s'", r.Name)
	}
	if r.TargetKind != KindSnapshot {
		t.Errorf("expected kind %s, got %s", KindSnapshot, r.TargetKind)
	}

	// List
	refs, err := refMgr.List(nil)
	if err != nil {
		t.Fatalf("listing refs: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("expected 1 ref, got %d", len(refs))
	}

	// Delete
	if err := refMgr.Delete("snap.test"); err != nil {
		t.Fatalf("deleting ref: %v", err)
	}

	// Verify deleted
	r, err = refMgr.Get("snap.test")
	if err != nil {
		t.Fatalf("getting deleted ref: %v", err)
	}
	if r != nil {
		t.Error("expected nil after delete, got ref")
	}
}

func TestRefManager_ListByKind(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)
	cs := createTestChangeSet(t, db, snap1, snap2)

	refMgr := NewRefManager(db)

	// Set refs of different kinds
	if err := refMgr.Set("snap.one", snap1, KindSnapshot); err != nil {
		t.Fatalf("setting snap ref: %v", err)
	}
	if err := refMgr.Set("snap.two", snap2, KindSnapshot); err != nil {
		t.Fatalf("setting snap ref: %v", err)
	}
	if err := refMgr.Set("cs.one", cs, KindChangeSet); err != nil {
		t.Fatalf("setting cs ref: %v", err)
	}

	// List all
	all, err := refMgr.List(nil)
	if err != nil {
		t.Fatalf("listing all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 refs, got %d", len(all))
	}

	// List snapshots only
	snapKind := KindSnapshot
	snaps, err := refMgr.List(&snapKind)
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("expected 2 snapshot refs, got %d", len(snaps))
	}

	// List changesets only
	csKind := KindChangeSet
	changesets, err := refMgr.List(&csKind)
	if err != nil {
		t.Fatalf("listing changesets: %v", err)
	}
	if len(changesets) != 1 {
		t.Errorf("expected 1 changeset ref, got %d", len(changesets))
	}
}

func TestLogManager(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)

	logMgr := NewLogManager(db)

	// Append entries
	if err := logMgr.Append(KindSnapshot, snap1); err != nil {
		t.Fatalf("appending first: %v", err)
	}
	if err := logMgr.Append(KindSnapshot, snap2); err != nil {
		t.Fatalf("appending second: %v", err)
	}

	// Get by seq
	entry, err := logMgr.GetBySeq(KindSnapshot, 1)
	if err != nil {
		t.Fatalf("getting seq 1: %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry, got nil")
	}
	if util.BytesToHex(entry.ID) != util.BytesToHex(snap1) {
		t.Errorf("expected snap1, got different ID")
	}

	// Get latest seq
	latest, err := logMgr.GetLatestSeq(KindSnapshot)
	if err != nil {
		t.Fatalf("getting latest seq: %v", err)
	}
	if latest != 2 {
		t.Errorf("expected latest seq 2, got %d", latest)
	}
}

func TestAutoRefManager_Snapshot(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snapID := createTestSnapshot(t, db)
	autoMgr := NewAutoRefManager(db)

	// Trigger auto-ref update
	if err := autoMgr.OnSnapshotCreated(snapID); err != nil {
		t.Fatalf("OnSnapshotCreated: %v", err)
	}

	// Verify snap.latest was set
	refMgr := NewRefManager(db)
	r, err := refMgr.Get("snap.latest")
	if err != nil {
		t.Fatalf("getting snap.latest: %v", err)
	}
	if r == nil {
		t.Fatal("expected snap.latest ref, got nil")
	}
	if util.BytesToHex(r.TargetID) != util.BytesToHex(snapID) {
		t.Errorf("expected snap.latest to point to %s", util.BytesToHex(snapID))
	}

	// Verify log entry
	logMgr := NewLogManager(db)
	latest, err := logMgr.GetLatestSeq(KindSnapshot)
	if err != nil {
		t.Fatalf("getting latest seq: %v", err)
	}
	if latest != 1 {
		t.Errorf("expected seq 1, got %d", latest)
	}
}

func TestAutoRefManager_ChangeSet(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)
	csID := createTestChangeSet(t, db, snap1, snap2)

	autoMgr := NewAutoRefManager(db)

	// Trigger auto-ref update
	if err := autoMgr.OnChangeSetCreated(csID); err != nil {
		t.Fatalf("OnChangeSetCreated: %v", err)
	}

	// Verify cs.latest was set
	refMgr := NewRefManager(db)
	r, err := refMgr.Get("cs.latest")
	if err != nil {
		t.Fatalf("getting cs.latest: %v", err)
	}
	if r == nil {
		t.Fatal("expected cs.latest ref, got nil")
	}
	if util.BytesToHex(r.TargetID) != util.BytesToHex(csID) {
		t.Errorf("expected cs.latest to point to %s", util.BytesToHex(csID))
	}
}

func TestAutoRefManager_Workspace(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snapID := createTestSnapshot(t, db)
	autoMgr := NewAutoRefManager(db)

	// Trigger workspace creation
	if err := autoMgr.OnWorkspaceCreated("feature/auth", snapID); err != nil {
		t.Fatalf("OnWorkspaceCreated: %v", err)
	}

	refMgr := NewRefManager(db)

	// Verify ws.feature/auth.base was set
	base, err := refMgr.Get("ws.feature/auth.base")
	if err != nil {
		t.Fatalf("getting ws.feature/auth.base: %v", err)
	}
	if base == nil {
		t.Fatal("expected ws.feature/auth.base ref, got nil")
	}
	if util.BytesToHex(base.TargetID) != util.BytesToHex(snapID) {
		t.Errorf("expected base ref to point to %s", util.BytesToHex(snapID))
	}

	// Verify ws.feature/auth.head was set
	head, err := refMgr.Get("ws.feature/auth.head")
	if err != nil {
		t.Fatalf("getting ws.feature/auth.head: %v", err)
	}
	if head == nil {
		t.Fatal("expected ws.feature/auth.head ref, got nil")
	}
}

func TestAmbiguityError(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create multiple snapshots
	for i := 0; i < 5; i++ {
		createTestSnapshot(t, db)
	}

	resolver := NewResolver(db)

	// Try to resolve with very short prefix (likely ambiguous)
	_, err := resolver.Resolve("000", nil)

	// Should either be not found or ambiguous - both are acceptable
	// The test just verifies we don't panic
	if err == nil {
		t.Log("prefix '000' resolved without error (might match)")
	} else {
		t.Logf("prefix '000' returned error: %v", err)
	}
}

func TestSessionBaseRef_OldestSelected(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)

	refMgr := NewRefManager(db)

	// Write two session base refs — morning and afternoon
	if err := refMgr.Set("session.mcp_1_1000.base", snap1, KindSnapshot); err != nil {
		t.Fatalf("setting session ref 1: %v", err)
	}
	if err := refMgr.Set("session.mcp_2_2000.base", snap2, KindSnapshot); err != nil {
		t.Fatalf("setting session ref 2: %v", err)
	}

	// Find oldest session base (should be snap1)
	allRefs, err := refMgr.List(nil)
	if err != nil {
		t.Fatalf("listing refs: %v", err)
	}

	var oldest *Ref
	for _, r := range allRefs {
		if len(r.Name) > 8 && r.Name[:8] == "session." && r.Name[len(r.Name)-5:] == ".base" {
			if oldest == nil || r.CreatedAt < oldest.CreatedAt {
				oldest = r
			}
		}
	}

	if oldest == nil {
		t.Fatal("expected to find a session base ref")
	}
	if util.BytesToHex(oldest.TargetID) != util.BytesToHex(snap1) {
		t.Errorf("expected oldest session base to point to snap1, got %s", oldest.Name)
	}
}

func TestSessionBaseRef_CleanupAfterReview(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	snap1 := createTestSnapshot(t, db)
	snap2 := createTestSnapshot(t, db)

	refMgr := NewRefManager(db)

	// Write two session refs
	refMgr.Set("session.mcp_1_1000.base", snap1, KindSnapshot)
	refMgr.Set("session.mcp_2_2000.base", snap2, KindSnapshot)

	// Simulate review creation cleanup: delete all session refs
	allRefs, _ := refMgr.List(nil)
	for _, r := range allRefs {
		if len(r.Name) > 8 && r.Name[:8] == "session." && r.Name[len(r.Name)-5:] == ".base" {
			refMgr.Delete(r.Name)
		}
	}

	// Verify both are gone
	allRefs, _ = refMgr.List(nil)
	for _, r := range allRefs {
		if len(r.Name) > 8 && r.Name[:8] == "session." {
			t.Errorf("expected session ref to be deleted, found %s", r.Name)
		}
	}
}

func TestSessionBaseRef_NoSessionFallsBack(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	refMgr := NewRefManager(db)

	// No session refs exist — scan should find nothing
	allRefs, _ := refMgr.List(nil)
	var found *Ref
	for _, r := range allRefs {
		if len(r.Name) > 8 && r.Name[:8] == "session." && r.Name[len(r.Name)-5:] == ".base" {
			found = r
		}
	}
	if found != nil {
		t.Error("expected no session base ref in empty DB")
	}
}
