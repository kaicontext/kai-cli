package graph

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-core/cas"
)

// Re-export for test convenience
var (
	Blake3Hash    = cas.Blake3Hash
	CanonicalJSON = cas.CanonicalJSON
)

func setupTestDB(t *testing.T) (*DB, func()) {
	t.Helper()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "kai-graph-test-*")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test.db")
	objPath := filepath.Join(tmpDir, "objects")
	if err := os.MkdirAll(objPath, 0755); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("creating objects dir: %v", err)
	}

	db, err := Open(dbPath, objPath)
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

func TestOpen_Close(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "kai-graph-test-*")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	objPath := filepath.Join(tmpDir, "objects")
	os.MkdirAll(objPath, 0755)

	db, err := Open(dbPath, objPath)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("closing database: %v", err)
	}

	// Verify database file was created
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("database file was not created")
	}
}

func TestOpen_InvalidPath(t *testing.T) {
	// Try to open database in non-existent directory
	_, err := Open("/nonexistent/path/test.db", "/tmp/objects")
	if err == nil {
		t.Error("expected error for invalid path, got nil")
	}
}

func TestInsertNode_GetNode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("beginning transaction: %v", err)
	}

	payload := map[string]interface{}{
		"name":      "test-file.go",
		"path":      "/src/test-file.go",
		"createdAt": float64(1234567890),
	}

	id, err := db.InsertNode(tx, KindFile, payload)
	if err != nil {
		tx.Rollback()
		t.Fatalf("inserting node: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing transaction: %v", err)
	}

	// Verify node was inserted
	node, err := db.GetNode(id)
	if err != nil {
		t.Fatalf("getting node: %v", err)
	}
	if node == nil {
		t.Fatal("expected node, got nil")
	}

	if node.Kind != KindFile {
		t.Errorf("expected kind %s, got %s", KindFile, node.Kind)
	}
	if node.Payload["name"] != "test-file.go" {
		t.Errorf("expected name 'test-file.go', got '%v'", node.Payload["name"])
	}
}

func TestInsertNode_Idempotent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	payload := map[string]interface{}{
		"name": "duplicate-test",
	}

	// Insert same node twice
	tx1, _ := db.BeginTx()
	id1, err := db.InsertNode(tx1, KindFile, payload)
	if err != nil {
		tx1.Rollback()
		t.Fatalf("first insert: %v", err)
	}
	tx1.Commit()

	tx2, _ := db.BeginTx()
	id2, err := db.InsertNode(tx2, KindFile, payload)
	if err != nil {
		tx2.Rollback()
		t.Fatalf("second insert: %v", err)
	}
	tx2.Commit()

	// Should have same ID (content-addressed)
	if !bytes.Equal(id1, id2) {
		t.Error("expected same ID for duplicate insert")
	}
}

func TestInsertNodeDirect(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	payload := map[string]interface{}{
		"name": "direct-insert-test",
	}

	id, err := db.InsertNodeDirect(KindModule, payload)
	if err != nil {
		t.Fatalf("inserting node directly: %v", err)
	}

	node, err := db.GetNode(id)
	if err != nil {
		t.Fatalf("getting node: %v", err)
	}
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if node.Kind != KindModule {
		t.Errorf("expected kind %s, got %s", KindModule, node.Kind)
	}
}

func TestGetNode_NotFound(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	node, err := db.GetNode([]byte("nonexistent-id"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != nil {
		t.Error("expected nil for non-existent node")
	}
}

func TestHasNode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	payload := map[string]interface{}{"name": "test"}
	id, err := db.InsertNodeDirect(KindFile, payload)
	if err != nil {
		t.Fatalf("inserting node: %v", err)
	}

	// Should exist
	exists, err := db.HasNode(id)
	if err != nil {
		t.Fatalf("checking node: %v", err)
	}
	if !exists {
		t.Error("expected node to exist")
	}

	// Should not exist
	exists, err = db.HasNode([]byte("nonexistent"))
	if err != nil {
		t.Fatalf("checking nonexistent node: %v", err)
	}
	if exists {
		t.Error("expected node to not exist")
	}
}

func TestGetNodesByKind(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert nodes of different kinds
	db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "file1"})
	db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "file2"})
	db.InsertNodeDirect(KindModule, map[string]interface{}{"name": "module1"})

	// Get files
	files, err := db.GetNodesByKind(KindFile)
	if err != nil {
		t.Fatalf("getting files: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d", len(files))
	}

	// Get modules
	modules, err := db.GetNodesByKind(KindModule)
	if err != nil {
		t.Fatalf("getting modules: %v", err)
	}
	if len(modules) != 1 {
		t.Errorf("expected 1 module, got %d", len(modules))
	}

	// Get symbols (none exist)
	symbols, err := db.GetNodesByKind(KindSymbol)
	if err != nil {
		t.Fatalf("getting symbols: %v", err)
	}
	if len(symbols) != 0 {
		t.Errorf("expected 0 symbols, got %d", len(symbols))
	}
}

func TestInsertEdge_GetEdges(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create source and destination nodes
	srcID, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "src"})
	dstID, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "dst"})

	// Insert edge
	tx, _ := db.BeginTx()
	if err := db.InsertEdge(tx, srcID, EdgeHasFile, dstID, nil); err != nil {
		tx.Rollback()
		t.Fatalf("inserting edge: %v", err)
	}
	tx.Commit()

	// Get edges
	edges, err := db.GetEdges(srcID, EdgeHasFile)
	if err != nil {
		t.Fatalf("getting edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}

	edge := edges[0]
	if !bytes.Equal(edge.Src, srcID) {
		t.Error("edge source mismatch")
	}
	if !bytes.Equal(edge.Dst, dstID) {
		t.Error("edge destination mismatch")
	}
	if edge.Type != EdgeHasFile {
		t.Errorf("expected edge type %s, got %s", EdgeHasFile, edge.Type)
	}
}

func TestInsertEdge_WithContext(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	srcID, _ := db.InsertNodeDirect(KindChangeSet, map[string]interface{}{"name": "cs"})
	dstID, _ := db.InsertNodeDirect(KindSymbol, map[string]interface{}{"name": "sym"})
	contextID, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "context"})

	tx, _ := db.BeginTx()
	if err := db.InsertEdge(tx, srcID, EdgeAffects, dstID, contextID); err != nil {
		tx.Rollback()
		t.Fatalf("inserting edge with context: %v", err)
	}
	tx.Commit()

	// Get edges by context
	edges, err := db.GetEdgesByContext(contextID, EdgeAffects)
	if err != nil {
		t.Fatalf("getting edges by context: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if !bytes.Equal(edges[0].At, contextID) {
		t.Error("edge context mismatch")
	}
}

func TestInsertEdgeDirect(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	srcID, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "src"})
	dstID, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "dst"})

	if err := db.InsertEdgeDirect(srcID, EdgeContains, dstID, nil); err != nil {
		t.Fatalf("inserting edge directly: %v", err)
	}

	edges, err := db.GetEdges(srcID, EdgeContains)
	if err != nil {
		t.Fatalf("getting edges: %v", err)
	}
	if len(edges) != 1 {
		t.Errorf("expected 1 edge, got %d", len(edges))
	}
}

func TestGetEdgeCount(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Empty graph should have zero edges.
	count, err := db.EdgeCount()
	if err != nil {
		t.Fatalf("EdgeCount on empty: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 edges, got %d", count)
	}

	// Insert nodes so we can add edges.
	srcID, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "A"})
	dstID, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "B"})

	// Add one edge.
	if err := db.InsertEdgeDirect(srcID, EdgeImports, dstID, nil); err != nil {
		t.Fatal(err)
	}
	count, err = db.EdgeCount()
	if err != nil {
		t.Fatalf("EdgeCount after 1 edge: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 edge, got %d", count)
	}

	// Add a second edge of a different type.
	if err := db.InsertEdgeDirect(srcID, EdgeContains, dstID, nil); err != nil {
		t.Fatal(err)
	}
	count, err = db.EdgeCount()
	if err != nil {
		t.Fatalf("EdgeCount after 2 edges: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 edges, got %d", count)
	}
}

func TestGetEdgesOfType(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	src1, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "src1"})
	src2, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "src2"})
	dst1, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "dst1"})
	dst2, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "dst2"})

	db.InsertEdgeDirect(src1, EdgeHasFile, dst1, nil)
	db.InsertEdgeDirect(src2, EdgeHasFile, dst2, nil)
	db.InsertEdgeDirect(src1, EdgeContains, dst1, nil)

	// Get all HAS_FILE edges
	edges, err := db.GetEdgesOfType(EdgeHasFile)
	if err != nil {
		t.Fatalf("getting edges of type: %v", err)
	}
	if len(edges) != 2 {
		t.Errorf("expected 2 HAS_FILE edges, got %d", len(edges))
	}

	// Get all CONTAINS edges
	containsEdges, err := db.GetEdgesOfType(EdgeContains)
	if err != nil {
		t.Fatalf("getting CONTAINS edges: %v", err)
	}
	if len(containsEdges) != 1 {
		t.Errorf("expected 1 CONTAINS edge, got %d", len(containsEdges))
	}
}

func TestGetEdgesByContextAndDst(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	contextID, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "ctx"})
	src1, _ := db.InsertNodeDirect(KindChangeSet, map[string]interface{}{"name": "cs1"})
	src2, _ := db.InsertNodeDirect(KindChangeSet, map[string]interface{}{"name": "cs2"})
	dst, _ := db.InsertNodeDirect(KindSymbol, map[string]interface{}{"name": "sym"})

	db.InsertEdgeDirect(src1, EdgeAffects, dst, contextID)
	db.InsertEdgeDirect(src2, EdgeAffects, dst, contextID)

	edges, err := db.GetEdgesByContextAndDst(contextID, EdgeAffects, dst)
	if err != nil {
		t.Fatalf("getting edges: %v", err)
	}
	if len(edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(edges))
	}
}

func TestGetEdgesTo(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	src1, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "src1"})
	src2, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "src2"})
	dst, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "dst"})

	db.InsertEdgeDirect(src1, EdgeHasFile, dst, nil)
	db.InsertEdgeDirect(src2, EdgeHasFile, dst, nil)

	edges, err := db.GetEdgesTo(dst, EdgeHasFile)
	if err != nil {
		t.Fatalf("getting edges to: %v", err)
	}
	if len(edges) != 2 {
		t.Errorf("expected 2 edges pointing to dst, got %d", len(edges))
	}
}

func TestUpdateNodePayload(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	id, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{
		"name":   "original",
		"status": "pending",
	})

	// Re-storing identical content is a no-op and must succeed (id unchanged).
	if err := db.UpdateNodePayload(id, map[string]interface{}{
		"name":   "original",
		"status": "pending",
	}); err != nil {
		t.Fatalf("no-op re-store should succeed: %v", err)
	}

	// Mutating a content-addressed node's content in place must be REFUSED:
	// it would break id == blake3(payload) and silently corrupt the store
	// (the root cause of the snapshot-corruption incident).
	if err := db.UpdateNodePayload(id, map[string]interface{}{
		"name":   "updated",
		"status": "complete",
	}); err == nil {
		t.Fatal("expected in-place content mutation of a File node to be refused")
	}

	// The node must be unchanged.
	node, _ := db.GetNode(id)
	if node.Payload["name"] != "original" {
		t.Errorf("content changed despite refusal: name=%v", node.Payload["name"])
	}
}

func TestUpdateNodePayload_NotFound(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	err := db.UpdateNodePayload([]byte("nonexistent"), map[string]interface{}{"foo": "bar"})
	if err == nil {
		t.Error("expected error for non-existent node")
	}
}

func TestWriteObject_ReadObject(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	content := []byte("Hello, this is test content for object storage!")

	digest, err := db.WriteObject(content)
	if err != nil {
		t.Fatalf("writing object: %v", err)
	}

	if digest == "" {
		t.Error("expected non-empty digest")
	}

	// Read back
	readContent, err := db.ReadObject(digest)
	if err != nil {
		t.Fatalf("reading object: %v", err)
	}

	if !bytes.Equal(content, readContent) {
		t.Error("content mismatch after read")
	}
}

func TestWriteObject_Idempotent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	content := []byte("duplicate content test")

	digest1, _ := db.WriteObject(content)
	digest2, _ := db.WriteObject(content)

	if digest1 != digest2 {
		t.Error("expected same digest for duplicate content")
	}
}

func TestReadObject_NotFound(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	_, err := db.ReadObject("nonexistent-digest")
	if err == nil {
		t.Error("expected error for non-existent object")
	}
}

func TestInsertWorkspace(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	wsID := []byte("workspace-uuid-12345678")
	payload := map[string]interface{}{
		"name":         "feature/test",
		"baseSnapshot": "abc123",
		"headSnapshot": "abc123",
		"status":       "active",
	}

	tx, _ := db.BeginTx()
	if err := db.InsertWorkspace(tx, wsID, payload); err != nil {
		tx.Rollback()
		t.Fatalf("inserting workspace: %v", err)
	}
	tx.Commit()

	// Verify
	node, err := db.GetNode(wsID)
	if err != nil {
		t.Fatalf("getting workspace: %v", err)
	}
	if node == nil {
		t.Fatal("expected workspace node, got nil")
	}
	if node.Kind != KindWorkspace {
		t.Errorf("expected kind %s, got %s", KindWorkspace, node.Kind)
	}
}

func TestInsertReview(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	reviewID := []byte("review-uuid-87654321")
	payload := map[string]interface{}{
		"title":   "Fix authentication bug",
		"state":   "draft",
		"author":  "alice",
	}

	tx, _ := db.BeginTx()
	if err := db.InsertReview(tx, reviewID, payload); err != nil {
		tx.Rollback()
		t.Fatalf("inserting review: %v", err)
	}
	tx.Commit()

	// Verify
	node, err := db.GetNode(reviewID)
	if err != nil {
		t.Fatalf("getting review: %v", err)
	}
	if node == nil {
		t.Fatal("expected review node, got nil")
	}
	if node.Kind != KindReview {
		t.Errorf("expected kind %s, got %s", KindReview, node.Kind)
	}
	if node.Payload["title"] != "Fix authentication bug" {
		t.Errorf("unexpected title: %v", node.Payload["title"])
	}
}

func TestGetWorkspaceByName(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert workspace
	wsID := []byte("workspace-find-test")
	payload := map[string]interface{}{
		"name":   "feature/auth",
		"status": "active",
	}

	tx, _ := db.BeginTx()
	db.InsertWorkspace(tx, wsID, payload)
	tx.Commit()

	// Find by name
	node, err := db.GetWorkspaceByName("feature/auth")
	if err != nil {
		t.Fatalf("finding workspace: %v", err)
	}
	if node == nil {
		t.Fatal("expected workspace, got nil")
	}
	if !bytes.Equal(node.ID, wsID) {
		t.Error("workspace ID mismatch")
	}

	// Not found
	node, err = db.GetWorkspaceByName("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != nil {
		t.Error("expected nil for non-existent workspace")
	}
}

func TestDeleteEdge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	srcID, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "src"})
	dstID, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "dst"})

	db.InsertEdgeDirect(srcID, EdgeHasFile, dstID, nil)

	// Verify edge exists
	edges, _ := db.GetEdges(srcID, EdgeHasFile)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge before delete, got %d", len(edges))
	}

	// Delete edge
	tx, _ := db.BeginTx()
	if err := db.DeleteEdge(tx, srcID, EdgeHasFile, dstID); err != nil {
		tx.Rollback()
		t.Fatalf("deleting edge: %v", err)
	}
	tx.Commit()

	// Verify edge was deleted
	edges, _ = db.GetEdges(srcID, EdgeHasFile)
	if len(edges) != 0 {
		t.Errorf("expected 0 edges after delete, got %d", len(edges))
	}
}

func TestDeleteEdgeAt(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	srcID, _ := db.InsertNodeDirect(KindChangeSet, map[string]interface{}{"name": "cs"})
	dst1, _ := db.InsertNodeDirect(KindSymbol, map[string]interface{}{"name": "sym1"})
	dst2, _ := db.InsertNodeDirect(KindSymbol, map[string]interface{}{"name": "sym2"})
	ctx1, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "ctx1"})
	ctx2, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"name": "ctx2"})

	// Insert edges with different (src,type,dst) tuples and different contexts.
	// PK is (src, type, dst), so each needs a unique dst.
	db.InsertEdgeDirect(srcID, EdgeAffects, dst1, ctx1)
	db.InsertEdgeDirect(srcID, EdgeAffects, dst2, ctx2)

	// Delete only the edge with ctx1
	tx, _ := db.BeginTx()
	if err := db.DeleteEdgeAt(tx, srcID, EdgeAffects, dst1, ctx1); err != nil {
		tx.Rollback()
		t.Fatalf("deleting edge at context: %v", err)
	}
	tx.Commit()

	// Verify only ctx2 edge remains
	edges1, _ := db.GetEdgesByContext(ctx1, EdgeAffects)
	edges2, _ := db.GetEdgesByContext(ctx2, EdgeAffects)

	if len(edges1) != 0 {
		t.Errorf("expected 0 edges for ctx1, got %d", len(edges1))
	}
	if len(edges2) != 1 {
		t.Errorf("expected 1 edge for ctx2, got %d", len(edges2))
	}
}

func TestGetAllNodesAndEdgesForChangeSet(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a changeset with related nodes
	csID, _ := db.InsertNodeDirect(KindChangeSet, map[string]interface{}{
		"title":  "Test changeset",
		"intent": "Testing",
	})
	fileID, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"name": "file.go"})
	symID, _ := db.InsertNodeDirect(KindSymbol, map[string]interface{}{"name": "TestFunc"})

	db.InsertEdgeDirect(csID, EdgeModifies, fileID, nil)
	db.InsertEdgeDirect(csID, EdgeAffects, symID, nil)

	result, err := db.GetAllNodesAndEdgesForChangeSet(csID)
	if err != nil {
		t.Fatalf("getting changeset data: %v", err)
	}

	if result["changeset"] == nil {
		t.Error("expected changeset in result")
	}

	nodes, ok := result["nodes"].([]map[string]interface{})
	if !ok {
		t.Fatal("expected nodes array in result")
	}
	if len(nodes) != 2 {
		t.Errorf("expected 2 related nodes, got %d", len(nodes))
	}

	edges, ok := result["edges"].([]map[string]interface{})
	if !ok {
		t.Fatal("expected edges array in result")
	}
	if len(edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(edges))
	}
}

func TestGetAllNodesAndEdgesForChangeSet_NotFound(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	_, err := db.GetAllNodesAndEdgesForChangeSet([]byte("nonexistent"))
	if err == nil {
		t.Error("expected error for non-existent changeset")
	}
}

func TestBeginTxCtx(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := t.Context()
	tx, err := db.BeginTxCtx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning transaction with context: %v", err)
	}
	defer tx.Rollback()

	// Insert a node within transaction
	_, err = db.InsertNode(tx, KindFile, map[string]interface{}{"name": "ctx-test"})
	if err != nil {
		t.Fatalf("inserting node in ctx tx: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing ctx transaction: %v", err)
	}
}

func TestQuery_QueryRow_Exec(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Test Exec
	_, err := db.Exec("INSERT INTO nodes (id, kind, payload, created_at) VALUES (?, ?, ?, ?)",
		[]byte("test-id"), "File", "{}", 123456)
	if err != nil {
		t.Fatalf("exec insert: %v", err)
	}

	// Test QueryRow
	var kind string
	err = db.QueryRow("SELECT kind FROM nodes WHERE id = ?", []byte("test-id")).Scan(&kind)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}
	if kind != "File" {
		t.Errorf("expected kind 'File', got '%s'", kind)
	}

	// Test Query
	rows, err := db.Query("SELECT id FROM nodes")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestNodeKindConstants(t *testing.T) {
	// Verify all node kind constants are accessible
	kinds := []NodeKind{
		KindFile,
		KindModule,
		KindSymbol,
		KindSnapshot,
		KindChangeSet,
		KindChangeType,
		KindWorkspace,
		KindReview,
		KindReviewComment,
	}

	for _, k := range kinds {
		if k == "" {
			t.Error("found empty node kind constant")
		}
	}
}

func TestEdgeTypeConstants(t *testing.T) {
	// Verify all edge type constants are accessible
	types := []EdgeType{
		EdgeContains,
		EdgeDefinesIn,
		EdgeHasFile,
		EdgeModifies,
		EdgeHas,
		EdgeAffects,
		EdgeBasedOn,
		EdgeHeadAt,
		EdgeHasChangeSet,
		EdgeReviewOf,
		EdgeHasComment,
		EdgeAnchorsTo,
		EdgeSupersedes,
	}

	for _, e := range types {
		if e == "" {
			t.Error("found empty edge type constant")
		}
	}
}

func TestGetNodeRawPayload(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a node with specific payload
	payload := map[string]interface{}{
		"path":      "src/main.ts",
		"digest":    "abc123",
		"size":      int64(1024),
		"createdAt": int64(1234567890000),
	}

	nodeID, err := db.InsertNodeDirect(KindFile, payload)
	if err != nil {
		t.Fatalf("inserting node: %v", err)
	}

	// Get raw payload
	kind, rawPayloadJSON, err := db.GetNodeRawPayload(nodeID)
	if err != nil {
		t.Fatalf("GetNodeRawPayload failed: %v", err)
	}

	if kind != KindFile {
		t.Errorf("expected kind File, got %s", kind)
	}

	if rawPayloadJSON == nil {
		t.Fatal("expected non-nil raw payload")
	}

	// Verify the raw payload can be used to compute the same node ID
	content := append([]byte(string(kind)+"\n"), rawPayloadJSON...)
	computedID := Blake3Hash(content)

	if !bytes.Equal(computedID, nodeID) {
		t.Errorf("computed ID mismatch: expected %x, got %x", nodeID, computedID)
	}
}

func TestGetNodeRawPayload_NotFound(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Try to get non-existent node
	kind, rawPayloadJSON, err := db.GetNodeRawPayload([]byte("nonexistent"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != "" {
		t.Errorf("expected empty kind, got %s", kind)
	}
	if rawPayloadJSON != nil {
		t.Error("expected nil raw payload for non-existent node")
	}
}

func TestGetNodeRawPayload_PreservesExactJSON(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create payload and get its canonical JSON
	payload := map[string]interface{}{
		"name": "test",
		"num":  int64(42),
	}

	// Insert the node
	nodeID, _ := db.InsertNodeDirect(KindSymbol, payload)

	// Get raw payload
	_, rawPayloadJSON, _ := db.GetNodeRawPayload(nodeID)

	// The raw payload should be canonical JSON
	expectedJSON, _ := CanonicalJSON(payload)
	if string(rawPayloadJSON) != string(expectedJSON) {
		t.Errorf("raw payload differs from expected:\n  got: %s\n  want: %s", rawPayloadJSON, expectedJSON)
	}
}

// TestGC_EphemeralRefsNotRoots verifies that snap.working and snap.latest are not GC roots
func TestGC_EphemeralRefsNotRoots(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a snapshot node
	snapPayload := map[string]interface{}{"path": "."}
	snapID, err := db.InsertNodeDirect(KindSnapshot, snapPayload)
	if err != nil {
		t.Fatalf("inserting snapshot: %v", err)
	}

	// Set snap.working to point to snapshot (ephemeral - NOT a root)
	_, err = db.Exec(`INSERT INTO refs (name, target_id, target_kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"snap.working", snapID, "Snapshot", 1000, 1000)
	if err != nil {
		t.Fatalf("inserting snap.working ref: %v", err)
	}

	// Set snap.latest to point to same snapshot (ephemeral - NOT a root)
	_, err = db.Exec(`INSERT INTO refs (name, target_id, target_kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"snap.latest", snapID, "Snapshot", 1000, 1000)
	if err != nil {
		t.Fatalf("inserting snap.latest ref: %v", err)
	}

	// Build GC plan - snapshot should be a candidate since only ephemeral refs point to it
	plan, err := db.BuildGCPlan(GCOptions{})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	// Snapshot should be in candidates (since snap.working and snap.latest are not roots)
	found := false
	for _, node := range plan.NodesToDelete {
		if bytes.Equal(node.ID, snapID) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected snapshot to be a GC candidate when only ephemeral refs point to it")
	}
}

// TestGC_NamedRefIsRoot verifies that named refs (like snap.main) ARE roots
func TestGC_NamedRefIsRoot(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a snapshot node
	snapPayload := map[string]interface{}{"path": "."}
	snapID, err := db.InsertNodeDirect(KindSnapshot, snapPayload)
	if err != nil {
		t.Fatalf("inserting snapshot: %v", err)
	}

	// Set snap.main to point to snapshot (named ref - IS a root)
	_, err = db.Exec(`INSERT INTO refs (name, target_id, target_kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"snap.main", snapID, "Snapshot", 1000, 1000)
	if err != nil {
		t.Fatalf("inserting snap.main ref: %v", err)
	}

	// Build GC plan - snapshot should NOT be a candidate
	plan, err := db.BuildGCPlan(GCOptions{})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	// Snapshot should NOT be in candidates
	for _, node := range plan.NodesToDelete {
		if bytes.Equal(node.ID, snapID) {
			t.Error("snapshot with named ref should NOT be a GC candidate")
		}
	}
}

// TestGC_ReviewKeepsTargetAlive verifies that review nodes keep their targets alive
func TestGC_ReviewKeepsTargetAlive(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a changeset node
	csPayload := map[string]interface{}{"title": "test changeset"}
	csID, err := db.InsertNodeDirect(KindChangeSet, csPayload)
	if err != nil {
		t.Fatalf("inserting changeset: %v", err)
	}

	// Create a review node pointing to the changeset
	reviewPayload := map[string]interface{}{
		"title":      "test review",
		"state":      "draft",
		"targetId":   string(csID),
		"targetKind": "ChangeSet",
	}
	reviewID, err := db.InsertNodeDirect(KindReview, reviewPayload)
	if err != nil {
		t.Fatalf("inserting review: %v", err)
	}

	// Create REVIEW_OF edge
	tx, _ := db.BeginTx()
	db.InsertEdge(tx, reviewID, EdgeReviewOf, csID, nil)
	tx.Commit()

	// Build GC plan - review and changeset should NOT be candidates
	plan, err := db.BuildGCPlan(GCOptions{})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	// Neither should be candidates
	for _, node := range plan.NodesToDelete {
		if bytes.Equal(node.ID, reviewID) {
			t.Error("review node should NOT be a GC candidate")
		}
		if bytes.Equal(node.ID, csID) {
			t.Error("changeset targeted by review should NOT be a GC candidate")
		}
	}
}

// TestGC_KeepFilter verifies that --keep patterns preserve matching nodes
func TestGC_KeepFilter(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create file nodes
	srcFile, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"path": "src/main.js"})
	testFile, _ := db.InsertNodeDirect(KindFile, map[string]interface{}{"path": "test/main.test.js"})

	// Build GC plan without keep filter - both should be candidates
	plan1, err := db.BuildGCPlan(GCOptions{})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	srcFound := false
	testFound := false
	for _, node := range plan1.NodesToDelete {
		if bytes.Equal(node.ID, srcFile) {
			srcFound = true
		}
		if bytes.Equal(node.ID, testFile) {
			testFound = true
		}
	}
	if !srcFound || !testFound {
		t.Error("without keep filter, both files should be candidates")
	}

	// Build GC plan with keep filter for src/**
	plan2, err := db.BuildGCPlan(GCOptions{Keep: []string{"src/**"}})
	if err != nil {
		t.Fatalf("building GC plan with keep: %v", err)
	}

	srcFoundWithKeep := false
	testFoundWithKeep := false
	for _, node := range plan2.NodesToDelete {
		if bytes.Equal(node.ID, srcFile) {
			srcFoundWithKeep = true
		}
		if bytes.Equal(node.ID, testFile) {
			testFoundWithKeep = true
		}
	}
	if srcFoundWithKeep {
		t.Error("src file should be preserved by keep filter")
	}
	if !testFoundWithKeep {
		t.Error("test file should still be a candidate (not matching keep)")
	}
}

// TestGC_SinceFilter verifies that --since filter only deletes old content
func TestGC_SinceFilter(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create an old snapshot (created a very long time ago)
	// Since we're using absolute timestamps, let's use a fixed old timestamp
	_, err := db.Exec(`INSERT INTO nodes (id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		[]byte("old-snap"), "Snapshot", `{"path":"."}`, 1000) // Very old
	if err != nil {
		t.Fatalf("inserting old snapshot: %v", err)
	}

	// Create a new snapshot (created now)
	newPayload := map[string]interface{}{"path": "."}
	newSnapID, err := db.InsertNodeDirect(KindSnapshot, newPayload)
	if err != nil {
		t.Fatalf("inserting new snapshot: %v", err)
	}

	// Build GC plan with since=7 days - should only include old snapshot
	plan, err := db.BuildGCPlan(GCOptions{SinceDays: 7})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	oldFound := false
	newFound := false
	for _, node := range plan.NodesToDelete {
		if string(node.ID) == "old-snap" {
			oldFound = true
		}
		if bytes.Equal(node.ID, newSnapID) {
			newFound = true
		}
	}

	// Old snapshot should be a candidate (older than 7 days)
	if !oldFound {
		t.Error("old snapshot should be a GC candidate with since filter")
	}
	// New snapshot should NOT be a candidate (too recent)
	if newFound {
		t.Error("new snapshot should NOT be a GC candidate with since filter")
	}
}

// TestGC_WorkspaceKeepsContentAlive verifies workspaces keep their snapshots/changesets alive
func TestGC_WorkspaceKeepsContentAlive(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create base and head snapshots
	baseSnap, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"path": "."})
	headSnap, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"path": "."})

	// Create a changeset
	csPayload := map[string]interface{}{
		"title":        "test changeset",
		"baseSnapshot": string(baseSnap),
		"headSnapshot": string(headSnap),
	}
	csID, _ := db.InsertNodeDirect(KindChangeSet, csPayload)

	// Create workspace with base, head, and changeset
	wsPayload := map[string]interface{}{
		"name":           "feature-branch",
		"baseSnapshot":   bytesToHex(baseSnap),
		"headSnapshot":   bytesToHex(headSnap),
		"openChangeSets": []interface{}{bytesToHex(csID)},
	}
	wsID, _ := db.InsertNodeDirect(KindWorkspace, wsPayload)

	// Build GC plan - workspace and all its content should be preserved
	plan, err := db.BuildGCPlan(GCOptions{})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	// Check none of our content is in candidates
	for _, node := range plan.NodesToDelete {
		if bytes.Equal(node.ID, wsID) {
			t.Error("workspace should NOT be a GC candidate")
		}
		if bytes.Equal(node.ID, baseSnap) {
			t.Error("workspace base snapshot should NOT be a GC candidate")
		}
		if bytes.Equal(node.ID, headSnap) {
			t.Error("workspace head snapshot should NOT be a GC candidate")
		}
		if bytes.Equal(node.ID, csID) {
			t.Error("workspace changeset should NOT be a GC candidate")
		}
	}
}

// TestGC_WorkspaceAndReviewCoexistence verifies that workspace with review keeps all content alive
func TestGC_WorkspaceAndReviewCoexistence(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create base and head snapshots
	baseSnap, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"path": "."})
	headSnap, _ := db.InsertNodeDirect(KindSnapshot, map[string]interface{}{"path": "."})

	// Create a changeset
	csPayload := map[string]interface{}{
		"title":        "test changeset",
		"baseSnapshot": string(baseSnap),
		"headSnapshot": string(headSnap),
	}
	csID, _ := db.InsertNodeDirect(KindChangeSet, csPayload)

	// Create workspace with changeset
	wsPayload := map[string]interface{}{
		"name":           "feature-branch",
		"baseSnapshot":   bytesToHex(baseSnap),
		"headSnapshot":   bytesToHex(headSnap),
		"openChangeSets": []interface{}{bytesToHex(csID)},
	}
	wsID, _ := db.InsertNodeDirect(KindWorkspace, wsPayload)

	// Create a review targeting the workspace
	reviewPayload := map[string]interface{}{
		"title":      "PR #123",
		"state":      "draft",
		"targetId":   bytesToHex(wsID),
		"targetKind": "Workspace",
	}
	reviewID, _ := db.InsertNodeDirect(KindReview, reviewPayload)

	// Create REVIEW_OF edge
	tx, _ := db.BeginTx()
	db.InsertEdge(tx, reviewID, EdgeReviewOf, wsID, nil)
	tx.Commit()

	// Build GC plan
	plan, err := db.BuildGCPlan(GCOptions{})
	if err != nil {
		t.Fatalf("building GC plan: %v", err)
	}

	// All content should be preserved
	contentIDs := [][]byte{wsID, baseSnap, headSnap, csID, reviewID}
	for _, node := range plan.NodesToDelete {
		for _, contentID := range contentIDs {
			if bytes.Equal(node.ID, contentID) {
				t.Errorf("content %x should NOT be a GC candidate when workspace has review", contentID[:8])
			}
		}
	}
}

// bytesToHex is a helper for tests
func bytesToHex(b []byte) string {
	const hex = "0123456789abcdef"
	result := make([]byte, len(b)*2)
	for i, v := range b {
		result[i*2] = hex[v>>4]
		result[i*2+1] = hex[v&0x0f]
	}
	return string(result)
}

func TestHasCIRunEdge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	tx, err := db.BeginTx()
	if err != nil {
		t.Fatal(err)
	}

	// Create a snapshot node
	snapID, err := db.InsertNode(tx, KindSnapshot, map[string]interface{}{
		"sourceType": "test",
		"fileCount":  float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a CI run ID (simulated)
	ciRunID := Blake3Hash([]byte("cirun:test-run-123"))

	// Write HAS_CI_RUN edge
	if err := db.InsertEdge(tx, snapID, EdgeHasCIRun, ciRunID, nil); err != nil {
		t.Fatalf("inserting HAS_CI_RUN edge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Query the edge back
	edges, err := db.GetEdges(snapID, EdgeHasCIRun)
	if err != nil {
		t.Fatalf("getting edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 HAS_CI_RUN edge, got %d", len(edges))
	}
	if !bytes.Equal(edges[0].Dst, ciRunID) {
		t.Error("edge destination doesn't match CI run ID")
	}
}
