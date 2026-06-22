package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"kai/internal/graph"
	"kai/internal/util"
)

func setupWorkspaceTestDB(t *testing.T) (*graph.DB, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "kai-workspace-test-*")
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

func insertSnapshot(t *testing.T, db *graph.DB, source string, files map[string]string) []byte {
	t.Helper()

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	fileIDs := make([][]byte, 0, len(files))
	for path, digest := range files {
		payload := map[string]interface{}{
			"path":   path,
			"digest": digest,
		}
		fileID, err := db.InsertNode(tx, graph.KindFile, payload)
		if err != nil {
			t.Fatalf("insert file: %v", err)
		}
		fileIDs = append(fileIDs, fileID)
	}

	snapPayload := map[string]interface{}{
		"sourceType": source,
		"sourceRef":  source,
		"fileCount":  len(files),
		"createdAt":  util.NowMs(),
	}
	snapID, err := db.InsertNode(tx, graph.KindSnapshot, snapPayload)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	for _, fileID := range fileIDs {
		if err := db.InsertEdge(tx, snapID, graph.EdgeHasFile, fileID, nil); err != nil {
			t.Fatalf("insert HAS_FILE edge: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit snapshot: %v", err)
	}

	return snapID
}

func insertChangeSet(t *testing.T, db *graph.DB, baseID, headID []byte) []byte {
	t.Helper()

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	payload := map[string]interface{}{
		"base":      util.BytesToHex(baseID),
		"head":      util.BytesToHex(headID),
		"title":     "",
		"createdAt": util.NowMs(),
	}
	csID, err := db.InsertNode(tx, graph.KindChangeSet, payload)
	if err != nil {
		t.Fatalf("insert changeset: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit changeset: %v", err)
	}
	return csID
}

func TestRebaseAppliesChangeSet(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})
	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v1"})

	csID := insertChangeSet(t, db, baseSnap, headSnap)

	mgr := NewManager(db)
	result, err := mgr.Rebase([][]byte{csID}, targetSnap)
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}
	if len(result.Applied) != 1 {
		t.Fatalf("expected 1 applied changeset, got %d", len(result.Applied))
	}

	files, err := mgr.getSnapshotFileMap(result.ResultSnapshot)
	if err != nil {
		t.Fatalf("result files: %v", err)
	}
	if got := files["a.txt"]; got != "v2" {
		t.Fatalf("expected rebased file digest v2, got %q", got)
	}
}

func TestRebaseStopsOnConflict(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})
	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v3"})

	csID := insertChangeSet(t, db, baseSnap, headSnap)

	mgr := NewManager(db)
	result, err := mgr.Rebase([][]byte{csID}, targetSnap)
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatalf("expected conflicts")
	}
	if len(result.Applied) != 0 {
		t.Fatalf("expected no applied changesets, got %d", len(result.Applied))
	}
}

func TestRebaseEmptyChangeSets(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})
	mgr := NewManager(db)
	if _, err := mgr.Rebase(nil, baseSnap); err == nil {
		t.Fatalf("expected error for empty changeset list")
	}
}
