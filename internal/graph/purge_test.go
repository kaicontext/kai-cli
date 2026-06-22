package graph

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Helper: create a file node with path and optional content object on disk.
func createFileNode(t *testing.T, db *DB, path, content string) []byte {
	t.Helper()

	// Write object to disk if content provided
	var digest, contentDigest string
	if content != "" {
		var err error
		contentDigest, err = db.WriteObject([]byte(content))
		if err != nil {
			t.Fatalf("writing object: %v", err)
		}
		digest = contentDigest
	}

	payload := map[string]interface{}{
		"path":          path,
		"lang":          "typescript",
		"digest":        digest,
		"contentDigest": contentDigest,
	}

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("beginning tx: %v", err)
	}
	id, err := db.InsertNode(tx, KindFile, payload)
	if err != nil {
		tx.Rollback()
		t.Fatalf("inserting file node: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("committing: %v", err)
	}
	return id
}

// Helper: create a snapshot node that references a set of file IDs.
func createSnapshotWithFiles(t *testing.T, db *DB, fileIDs [][]byte, filePaths []string) []byte {
	t.Helper()

	fileDigests := make([]interface{}, len(fileIDs))
	filesMetadata := make([]interface{}, len(fileIDs))
	for i, id := range fileIDs {
		fileDigests[i] = hex.EncodeToString(id)
		filesMetadata[i] = map[string]interface{}{
			"path":   filePaths[i],
			"lang":   "typescript",
			"digest": hex.EncodeToString(id),
		}
	}

	payload := map[string]interface{}{
		"sourceType":  "test",
		"sourceRef":   "test-ref",
		"fileCount":   float64(len(fileIDs)),
		"fileDigests": fileDigests,
		"files":       filesMetadata,
	}

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("beginning tx: %v", err)
	}
	snapID, err := db.InsertNode(tx, KindSnapshot, payload)
	if err != nil {
		tx.Rollback()
		t.Fatalf("inserting snapshot: %v", err)
	}

	// Create HAS_FILE edges
	for _, fid := range fileIDs {
		if err := db.InsertEdge(tx, snapID, EdgeHasFile, fid, nil); err != nil {
			tx.Rollback()
			t.Fatalf("inserting HAS_FILE edge: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing: %v", err)
	}
	return snapID
}

// Helper: create a symbol node defined in a file.
func createSymbolInFile(t *testing.T, db *DB, name string, fileID []byte, snapID []byte) []byte {
	t.Helper()

	payload := map[string]interface{}{
		"name": name,
		"kind": "function",
	}

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("beginning tx: %v", err)
	}
	symID, err := db.InsertNode(tx, KindSymbol, payload)
	if err != nil {
		tx.Rollback()
		t.Fatalf("inserting symbol: %v", err)
	}

	// DEFINES_IN: symbol -> file
	if err := db.InsertEdge(tx, symID, EdgeDefinesIn, fileID, snapID); err != nil {
		tx.Rollback()
		t.Fatalf("inserting DEFINES_IN edge: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing: %v", err)
	}
	return symID
}

// Helper: add an IMPORTS edge between two files.
func createImportEdge(t *testing.T, db *DB, fromFile, toFile, snapID []byte) {
	t.Helper()
	if err := db.InsertEdgeDirect(fromFile, EdgeImports, toFile, snapID); err != nil {
		t.Fatalf("inserting IMPORTS edge: %v", err)
	}
}

// Helper: add a TESTS edge between two files.
func createTestsEdge(t *testing.T, db *DB, testFile, srcFile, snapID []byte) {
	t.Helper()
	if err := db.InsertEdgeDirect(testFile, EdgeTests, srcFile, snapID); err != nil {
		t.Fatalf("inserting TESTS edge: %v", err)
	}
}

// Helper: insert authorship ranges for a file.
func createAuthorship(t *testing.T, db *DB, snapID []byte, filePath string) {
	t.Helper()
	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("beginning tx: %v", err)
	}
	db.InsertAuthorshipRange(tx, snapID, AuthorshipRange{
		FilePath: filePath, StartLine: 1, EndLine: 10,
		AuthorType: "human", CreatedAt: 1,
	})
	db.InsertAuthorshipRange(tx, snapID, AuthorshipRange{
		FilePath: filePath, StartLine: 11, EndLine: 20,
		AuthorType: "ai", Agent: "claude", Model: "opus", SessionID: "sess-123", CreatedAt: 2,
	})
	tx.Commit()
}

// Helper: count nodes of a given kind.
func countNodes(t *testing.T, db *DB, kind NodeKind) int {
	t.Helper()
	nodes, err := db.GetNodesByKind(kind)
	if err != nil {
		t.Fatalf("counting nodes: %v", err)
	}
	return len(nodes)
}

// Helper: count edges of a given type.
func countEdges(t *testing.T, db *DB, edgeType EdgeType) int {
	t.Helper()
	edges, err := db.GetEdgesOfType(edgeType)
	if err != nil {
		t.Fatalf("counting edges: %v", err)
	}
	return len(edges)
}

// Helper: count authorship ranges for a file path.
func countAuthorship(t *testing.T, db *DB, filePath string) int {
	t.Helper()
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM authorship_ranges WHERE file_path = ?", filePath).Scan(&count)
	if err != nil {
		t.Fatalf("counting authorship: %v", err)
	}
	return count
}

// Helper: check if an object file exists on disk.
func objectExists(db *DB, digest string) bool {
	_, err := os.Stat(filepath.Join(db.objectsDir, digest))
	return err == nil
}

// ─── Tests ───────────────────────────────────────────────────────────────────

func TestPurge_NoMatch(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	fid := createFileNode(t, db, "src/app.ts", "hello")
	createSnapshotWithFiles(t, db, [][]byte{fid}, []string{"src/app.ts"})

	plan, err := db.BuildPurgePlan([]string{".env"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.FileCount != 0 {
		t.Errorf("expected 0 files, got %d", plan.FileCount)
	}
	if plan.SnapshotCount != 0 {
		t.Errorf("expected 0 snapshots, got %d", plan.SnapshotCount)
	}
}

func TestPurge_ExactPath(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	envID := createFileNode(t, db, ".env", "SECRET=abc")
	appID := createFileNode(t, db, "src/app.ts", "export function main() {}")

	snapID := createSnapshotWithFiles(t, db, [][]byte{envID, appID}, []string{".env", "src/app.ts"})

	// Verify preconditions
	if n := countNodes(t, db, KindFile); n != 2 {
		t.Fatalf("expected 2 file nodes, got %d", n)
	}

	plan, err := db.BuildPurgePlan([]string{".env"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.FileCount != 1 {
		t.Errorf("expected 1 file, got %d", plan.FileCount)
	}
	if plan.Paths[0] != ".env" {
		t.Errorf("expected path '.env', got '%s'", plan.Paths[0])
	}
	if plan.SnapshotCount != 1 {
		t.Errorf("expected 1 snapshot, got %d", plan.SnapshotCount)
	}

	// Execute
	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// File node gone
	if n := countNodes(t, db, KindFile); n != 1 {
		t.Errorf("expected 1 file node remaining, got %d", n)
	}

	// HAS_FILE edges: only the surviving file's edge should remain
	edges, _ := db.getAllEdgesFrom(snapID)
	hasFileCount := 0
	for _, e := range edges {
		if e.Type == EdgeHasFile {
			hasFileCount++
		}
	}
	if hasFileCount != 1 {
		t.Errorf("expected 1 HAS_FILE edge remaining, got %d", hasFileCount)
	}

	// Snapshot manifest is content-addressed, so purge must NOT rewrite it in
	// place (that was the corruption bug). The purged file's blob/node/edges
	// are gone, but the manifest is preserved so its digest stays valid.
	snap, _ := db.GetNode(snapID)
	fc, _ := snap.Payload["fileCount"].(float64)
	if int(fc) != 2 {
		t.Errorf("manifest must be preserved (fileCount=2), got %v", fc)
	}
	files, _ := snap.Payload["files"].([]interface{})
	if len(files) != 2 {
		t.Errorf("manifest must be preserved (2 files), got %d", len(files))
	}
}

func TestPurge_GlobPattern(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	f1 := createFileNode(t, db, "src/auth/secrets.ts", "secret1")
	f2 := createFileNode(t, db, "src/db/config.ts", "config")
	f3 := createFileNode(t, db, "src/auth/login.ts", "login")
	f4 := createFileNode(t, db, "README.md", "readme")

	createSnapshotWithFiles(t, db,
		[][]byte{f1, f2, f3, f4},
		[]string{"src/auth/secrets.ts", "src/db/config.ts", "src/auth/login.ts", "README.md"},
	)

	// Purge all .ts files under src/auth/
	plan, err := db.BuildPurgePlan([]string{"src/auth/*.ts"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.FileCount != 2 {
		t.Errorf("expected 2 files matched, got %d", plan.FileCount)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	if n := countNodes(t, db, KindFile); n != 2 {
		t.Errorf("expected 2 file nodes remaining, got %d", n)
	}
}

func TestPurge_DoubleStarGlob(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	f1 := createFileNode(t, db, "src/auth/secrets.pem", "cert1")
	f2 := createFileNode(t, db, "deploy/tls/server.pem", "cert2")
	f3 := createFileNode(t, db, "src/app.ts", "code")

	createSnapshotWithFiles(t, db,
		[][]byte{f1, f2, f3},
		[]string{"src/auth/secrets.pem", "deploy/tls/server.pem", "src/app.ts"},
	)

	plan, err := db.BuildPurgePlan([]string{"**/*.pem"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.FileCount != 2 {
		t.Errorf("expected 2 .pem files, got %d", plan.FileCount)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	if n := countNodes(t, db, KindFile); n != 1 {
		t.Errorf("expected 1 file remaining, got %d", n)
	}
}

func TestPurge_MultiplePatterns(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	f1 := createFileNode(t, db, ".env", "secret")
	f2 := createFileNode(t, db, "credentials.json", "creds")
	f3 := createFileNode(t, db, "src/app.ts", "code")

	createSnapshotWithFiles(t, db,
		[][]byte{f1, f2, f3},
		[]string{".env", "credentials.json", "src/app.ts"},
	)

	plan, err := db.BuildPurgePlan([]string{".env", "credentials.json"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", plan.FileCount)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	if n := countNodes(t, db, KindFile); n != 1 {
		t.Errorf("expected 1 file remaining, got %d", n)
	}
}

func TestPurge_SymbolsDeleted(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	fileID := createFileNode(t, db, "src/auth/validate.ts", "export function validateToken() {}")
	otherID := createFileNode(t, db, "src/app.ts", "export function main() {}")

	snapID := createSnapshotWithFiles(t, db, [][]byte{fileID, otherID}, []string{"src/auth/validate.ts", "src/app.ts"})

	sym1 := createSymbolInFile(t, db, "validateToken", fileID, snapID)
	sym2 := createSymbolInFile(t, db, "tokenTTL", fileID, snapID)
	createSymbolInFile(t, db, "main", otherID, snapID) // should survive

	// Verify preconditions
	if n := countNodes(t, db, KindSymbol); n != 3 {
		t.Fatalf("expected 3 symbols, got %d", n)
	}

	plan, err := db.BuildPurgePlan([]string{"src/auth/validate.ts"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.SymbolCount != 2 {
		t.Errorf("expected 2 symbols in plan, got %d", plan.SymbolCount)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// Symbols for validate.ts should be gone
	if n := countNodes(t, db, KindSymbol); n != 1 {
		t.Errorf("expected 1 symbol remaining, got %d", n)
	}

	// The surviving symbol should be "main"
	node1, _ := db.GetNode(sym1)
	if node1 != nil {
		t.Error("expected validateToken symbol to be deleted")
	}
	node2, _ := db.GetNode(sym2)
	if node2 != nil {
		t.Error("expected tokenTTL symbol to be deleted")
	}
}

func TestPurge_EdgesCleanedUp(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	secretFile := createFileNode(t, db, "src/secrets.ts", "export const KEY = 'abc';")
	appFile := createFileNode(t, db, "src/app.ts", "import { KEY } from './secrets';")
	testFile := createFileNode(t, db, "tests/secrets.test.ts", "import { KEY } from '../src/secrets';")

	snapID := createSnapshotWithFiles(t, db,
		[][]byte{secretFile, appFile, testFile},
		[]string{"src/secrets.ts", "src/app.ts", "tests/secrets.test.ts"},
	)

	// app.ts imports secrets.ts
	createImportEdge(t, db, appFile, secretFile, snapID)
	// test file tests secrets.ts
	createTestsEdge(t, db, testFile, secretFile, snapID)

	// Verify preconditions
	if n := countEdges(t, db, EdgeImports); n != 1 {
		t.Fatalf("expected 1 IMPORTS edge, got %d", n)
	}
	if n := countEdges(t, db, EdgeTests); n != 1 {
		t.Fatalf("expected 1 TESTS edge, got %d", n)
	}

	plan, err := db.BuildPurgePlan([]string{"src/secrets.ts"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// IMPORTS edge (app -> secrets) should be deleted because secrets is dst
	if n := countEdges(t, db, EdgeImports); n != 0 {
		t.Errorf("expected 0 IMPORTS edges after purge, got %d", n)
	}

	// TESTS edge (test -> secrets) should be deleted because secrets is dst
	if n := countEdges(t, db, EdgeTests); n != 0 {
		t.Errorf("expected 0 TESTS edges after purge, got %d", n)
	}

	// HAS_FILE edges should only have 2 (for surviving files)
	if n := countEdges(t, db, EdgeHasFile); n != 2 {
		t.Errorf("expected 2 HAS_FILE edges remaining, got %d", n)
	}
}

func TestPurge_ObjectsDeletedFromDisk(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	secretContent := "SUPER_SECRET_KEY=do_not_leak"
	appContent := "export function main() {}"

	secretID := createFileNode(t, db, ".env", secretContent)
	appID := createFileNode(t, db, "src/app.ts", appContent)

	createSnapshotWithFiles(t, db, [][]byte{secretID, appID}, []string{".env", "src/app.ts"})

	// Get the digests to check on disk
	secretNode, _ := db.GetNode(secretID)
	secretDigest := secretNode.Payload["contentDigest"].(string)
	appNode, _ := db.GetNode(appID)
	appDigest := appNode.Payload["contentDigest"].(string)

	// Both objects should exist
	if !objectExists(db, secretDigest) {
		t.Fatal("secret object should exist before purge")
	}
	if !objectExists(db, appDigest) {
		t.Fatal("app object should exist before purge")
	}

	plan, err := db.BuildPurgePlan([]string{".env"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.BytesReclaimed <= 0 {
		t.Error("expected BytesReclaimed > 0")
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// Secret object gone
	if objectExists(db, secretDigest) {
		t.Error("secret object should be deleted after purge")
	}

	// App object still there
	if !objectExists(db, appDigest) {
		t.Error("app object should survive purge")
	}
}

func TestPurge_AuthorshipDeleted(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	secretID := createFileNode(t, db, "src/secrets.ts", "secret")
	appID := createFileNode(t, db, "src/app.ts", "app")

	snapID := createSnapshotWithFiles(t, db, [][]byte{secretID, appID}, []string{"src/secrets.ts", "src/app.ts"})

	createAuthorship(t, db, snapID, "src/secrets.ts")
	createAuthorship(t, db, snapID, "src/app.ts")

	// Verify preconditions
	if n := countAuthorship(t, db, "src/secrets.ts"); n != 2 {
		t.Fatalf("expected 2 authorship ranges for secrets.ts, got %d", n)
	}

	plan, err := db.BuildPurgePlan([]string{"src/secrets.ts"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// Authorship for secrets.ts gone
	if n := countAuthorship(t, db, "src/secrets.ts"); n != 0 {
		t.Errorf("expected 0 authorship ranges for secrets.ts, got %d", n)
	}

	// Authorship for app.ts untouched
	if n := countAuthorship(t, db, "src/app.ts"); n != 2 {
		t.Errorf("expected 2 authorship ranges for app.ts, got %d", n)
	}
}

func TestPurge_MultipleSnapshots(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Same file appears in two different snapshots
	envV1 := createFileNode(t, db, ".env", "SECRET=v1")
	envV2 := createFileNode(t, db, ".env", "SECRET=v2")
	appID := createFileNode(t, db, "src/app.ts", "code")

	createSnapshotWithFiles(t, db, [][]byte{envV1, appID}, []string{".env", "src/app.ts"})
	createSnapshotWithFiles(t, db, [][]byte{envV2, appID}, []string{".env", "src/app.ts"})

	plan, err := db.BuildPurgePlan([]string{".env"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	// Should match both versions of .env
	if plan.FileCount != 2 {
		t.Errorf("expected 2 file nodes (two versions), got %d", plan.FileCount)
	}

	// Both snapshots should need updates
	if plan.SnapshotCount != 2 {
		t.Errorf("expected 2 snapshots to update, got %d", plan.SnapshotCount)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// Only app.ts file node should remain
	if n := countNodes(t, db, KindFile); n != 1 {
		t.Errorf("expected 1 file node remaining, got %d", n)
	}

	// Both snapshots should still exist
	if n := countNodes(t, db, KindSnapshot); n != 2 {
		t.Errorf("expected 2 snapshot nodes remaining, got %d", n)
	}
}

func TestPurge_SharedDigestDeletedOnce(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Two file nodes with the same content (different paths, same digest)
	content := "shared secret content"
	f1 := createFileNode(t, db, ".env", content)
	f2 := createFileNode(t, db, ".env.backup", content)
	f3 := createFileNode(t, db, "src/app.ts", "other content")

	createSnapshotWithFiles(t, db,
		[][]byte{f1, f2, f3},
		[]string{".env", ".env.backup", "src/app.ts"},
	)

	plan, err := db.BuildPurgePlan([]string{".env", ".env.backup"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	if plan.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", plan.FileCount)
	}

	// Objects should be deduplicated — same content = 1 digest
	if len(plan.ObjectsToDelete) != 1 {
		t.Errorf("expected 1 unique object to delete (shared digest), got %d", len(plan.ObjectsToDelete))
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	if n := countNodes(t, db, KindFile); n != 1 {
		t.Errorf("expected 1 file remaining, got %d", n)
	}
}

func TestPurge_Idempotent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	envID := createFileNode(t, db, ".env", "SECRET=abc")
	appID := createFileNode(t, db, "src/app.ts", "code")
	createSnapshotWithFiles(t, db, [][]byte{envID, appID}, []string{".env", "src/app.ts"})

	// First purge
	plan1, _ := db.BuildPurgePlan([]string{".env"})
	if err := db.ExecutePurge(plan1); err != nil {
		t.Fatalf("first purge: %v", err)
	}

	// Second purge — should find nothing
	plan2, err := db.BuildPurgePlan([]string{".env"})
	if err != nil {
		t.Fatalf("building second plan: %v", err)
	}

	if plan2.FileCount != 0 {
		t.Errorf("expected 0 files on second purge, got %d", plan2.FileCount)
	}
}

func TestPurge_SnapshotsRemainNavigable(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	envID := createFileNode(t, db, ".env", "SECRET")
	appID := createFileNode(t, db, "src/app.ts", "code")
	configID := createFileNode(t, db, "config.json", "{}")

	snapID := createSnapshotWithFiles(t, db,
		[][]byte{envID, appID, configID},
		[]string{".env", "src/app.ts", "config.json"},
	)

	plan, _ := db.BuildPurgePlan([]string{".env"})
	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// Snapshot node should still be retrievable
	snap, err := db.GetNode(snapID)
	if err != nil {
		t.Fatalf("getting snapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot node should still exist")
	}

	// Manifests are content-addressed, so purge does NOT rewrite them in
	// place — that was the corruption bug. The .env blob/node are deleted
	// (content scrubbed), but the manifest is preserved intact so the
	// snapshot stays navigable and its digest valid.
	fc, _ := snap.Payload["fileCount"].(float64)
	if int(fc) != 3 {
		t.Errorf("manifest must be preserved (fileCount=3), got %v", fc)
	}
	digestsArr, _ := snap.Payload["fileDigests"].([]interface{})
	if len(digestsArr) != 3 {
		t.Errorf("manifest must be preserved (3 fileDigests), got %d", len(digestsArr))
	}

	// The purged file's NODE is gone (blob scrubbed) even though the manifest
	// still references it.
	if n, _ := db.GetNode(envID); n != nil {
		t.Error("purged .env file node should be deleted")
	}
}

func TestPurge_EmptyPlanNoOp(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	appID := createFileNode(t, db, "src/app.ts", "code")
	createSnapshotWithFiles(t, db, [][]byte{appID}, []string{"src/app.ts"})

	plan, err := db.BuildPurgePlan([]string{"nonexistent.file"})
	if err != nil {
		t.Fatalf("building plan: %v", err)
	}

	// ExecutePurge on empty plan should be a no-op
	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing empty purge: %v", err)
	}

	if n := countNodes(t, db, KindFile); n != 1 {
		t.Errorf("file should survive no-op purge, got %d", n)
	}
}

func TestPurge_PurgeAllFiles(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	f1 := createFileNode(t, db, "a.ts", "a")
	f2 := createFileNode(t, db, "b.ts", "b")

	snapID := createSnapshotWithFiles(t, db, [][]byte{f1, f2}, []string{"a.ts", "b.ts"})

	plan, _ := db.BuildPurgePlan([]string{"a.ts", "b.ts"})
	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// No file nodes left
	if n := countNodes(t, db, KindFile); n != 0 {
		t.Errorf("expected 0 files, got %d", n)
	}

	// Snapshot still exists, manifest preserved (content-addressed — not
	// rewritten in place). The file blobs/nodes are deleted; the manifest
	// keeps its entries so its digest stays valid.
	snap, _ := db.GetNode(snapID)
	if snap == nil {
		t.Fatal("snapshot should still exist")
	}
	fc, _ := snap.Payload["fileCount"].(float64)
	if int(fc) != 2 {
		t.Errorf("manifest must be preserved (fileCount=2), got %v", fc)
	}
}

func TestPurge_DoesNotAffectUnrelatedSnapshots(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	envID := createFileNode(t, db, ".env", "secret")
	appID := createFileNode(t, db, "src/app.ts", "code")
	readmeID := createFileNode(t, db, "README.md", "readme")

	// Snapshot 1 has .env and app.ts
	createSnapshotWithFiles(t, db, [][]byte{envID, appID}, []string{".env", "src/app.ts"})
	// Snapshot 2 has only README.md — no .env
	snap2ID := createSnapshotWithFiles(t, db, [][]byte{readmeID}, []string{"README.md"})

	plan, _ := db.BuildPurgePlan([]string{".env"})

	// Only 1 snapshot should be affected
	if plan.SnapshotCount != 1 {
		t.Errorf("expected 1 snapshot affected, got %d", plan.SnapshotCount)
	}

	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// Snapshot 2 should be completely untouched
	snap2, _ := db.GetNode(snap2ID)
	fc, _ := snap2.Payload["fileCount"].(float64)
	if int(fc) != 1 {
		t.Errorf("snapshot 2 fileCount should still be 1, got %v", fc)
	}
}

func TestPurge_SymbolEdgesFullyRemoved(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	fileID := createFileNode(t, db, "src/auth.ts", "code")
	otherFile := createFileNode(t, db, "src/app.ts", "code2")
	snapID := createSnapshotWithFiles(t, db, [][]byte{fileID, otherFile}, []string{"src/auth.ts", "src/app.ts"})

	symID := createSymbolInFile(t, db, "login", fileID, snapID)

	// Add a CALLS edge from another symbol to this one
	otherSym := createSymbolInFile(t, db, "main", otherFile, snapID)
	db.InsertEdgeDirect(otherSym, EdgeCalls, symID, snapID)

	// Verify CALLS edge exists
	callEdges, _ := db.GetEdges(otherSym, EdgeCalls)
	if len(callEdges) != 1 {
		t.Fatalf("expected 1 CALLS edge, got %d", len(callEdges))
	}

	plan, _ := db.BuildPurgePlan([]string{"src/auth.ts"})
	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// The CALLS edge pointing to the deleted symbol should be gone
	// (because the symbol is deleted and its incoming edges are cleaned)
	deletedSym, _ := db.GetNode(symID)
	if deletedSym != nil {
		t.Error("login symbol should be deleted")
	}

	// DEFINES_IN edges for purged file should be gone
	defEdges, _ := db.GetEdgesByDst(EdgeDefinesIn, fileID)
	if len(defEdges) != 0 {
		t.Errorf("expected 0 DEFINES_IN edges to purged file, got %d", len(defEdges))
	}
}

func TestPurge_ThenPruneIsClean(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	envID := createFileNode(t, db, ".env", "secret")
	appID := createFileNode(t, db, "src/app.ts", "code")
	snapID := createSnapshotWithFiles(t, db, [][]byte{envID, appID}, []string{".env", "src/app.ts"})

	// Make the snapshot a root by creating a named ref (not snap.latest which is ephemeral)
	db.Exec("INSERT INTO refs (name, target_id, target_kind, created_at, updated_at) VALUES (?, ?, 'Snapshot', 1, 1)",
		"snap.main", snapID)

	plan, _ := db.BuildPurgePlan([]string{".env"})
	if err := db.ExecutePurge(plan); err != nil {
		t.Fatalf("executing purge: %v", err)
	}

	// The purged file should NOT show up in a GC plan (it's already gone).
	// Use non-aggressive mode to only look for snapshots/changesets/files.
	gcPlan, err := db.BuildGCPlan(GCOptions{})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	// No snapshot or file nodes should be orphaned
	for _, node := range gcPlan.NodesToDelete {
		if node.Kind == KindFile {
			path, _ := node.Payload["path"].(string)
			t.Errorf("purged file %q should not appear in GC plan", path)
		}
		if node.Kind == KindSnapshot {
			t.Errorf("snapshot should not be orphaned after purge")
		}
	}
	if len(gcPlan.ObjectsToDelete) != 0 {
		t.Errorf("expected no orphaned objects after purge, got %d", len(gcPlan.ObjectsToDelete))
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	tests := []struct {
		patterns []string
		path     string
		want     bool
	}{
		{[]string{".env"}, ".env", true},
		{[]string{".env"}, ".env.local", false},
		{[]string{"*.ts"}, "app.ts", true},
		{[]string{"*.ts"}, "src/app.ts", false}, // filepath.Match doesn't cross /
		{[]string{"**/*.ts"}, "src/app.ts", true},
		{[]string{"**/*.ts"}, "src/deep/nested/app.ts", true},
		{[]string{"**/*.pem"}, "certs/server.pem", true},
		{[]string{"src/**"}, "src/app.ts", true},
		{[]string{"src/**"}, "src/deep/nested.ts", true},
		{[]string{"src/**"}, "other/app.ts", false},
		{[]string{".env", "*.pem"}, ".env", true},
		{[]string{".env", "*.pem"}, "server.pem", true},
		{[]string{".env", "*.pem"}, "app.ts", false},
	}

	for _, tt := range tests {
		got := matchesAnyPattern(tt.patterns, tt.path)
		if got != tt.want {
			t.Errorf("matchesAnyPattern(%v, %q) = %v, want %v", tt.patterns, tt.path, got, tt.want)
		}
	}
}
