package intent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"kai/internal/graph"
	"kai/internal/util"
)

func setupTestDB(t *testing.T) (*graph.DB, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "kai-intent-test-*")
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

func createTestChangeSet(t *testing.T, db *graph.DB) []byte {
	t.Helper()

	payload := map[string]interface{}{
		"base":        "abc123",
		"head":        "def456",
		"workspaceId": "ws123",
		"title":       "",
		"description": "",
		"intent":      "",
		"createdAt":   int64(1234567890000),
	}

	id, err := db.InsertNodeDirect(graph.KindChangeSet, payload)
	if err != nil {
		t.Fatalf("creating changeset: %v", err)
	}
	return id
}

func TestUpdateChangeSetIntent_CreatesIntentNode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	csID := createTestChangeSet(t, db)
	gen := NewGenerator(db)

	// Update intent
	intentText := "Add new authentication feature"
	err := gen.UpdateChangeSetIntent(csID, intentText)
	if err != nil {
		t.Fatalf("updating intent: %v", err)
	}

	// Verify Intent node was created
	edges, err := db.GetEdges(csID, graph.EdgeHasIntent)
	if err != nil {
		t.Fatalf("getting edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 HAS_INTENT edge, got %d", len(edges))
	}

	// Get the Intent node
	intentNode, err := db.GetNode(edges[0].Dst)
	if err != nil {
		t.Fatalf("getting intent node: %v", err)
	}
	if intentNode == nil {
		t.Fatal("intent node not found")
	}
	if intentNode.Kind != graph.KindIntent {
		t.Errorf("expected kind %s, got %s", graph.KindIntent, intentNode.Kind)
	}

	// Verify intent text
	text, ok := intentNode.Payload["text"].(string)
	if !ok {
		t.Fatal("intent text not found in payload")
	}
	if text != intentText {
		t.Errorf("expected intent %q, got %q", intentText, text)
	}

	// Verify changeSetID is stored
	csIDHex, ok := intentNode.Payload["changeSetID"].(string)
	if !ok {
		t.Fatal("changeSetID not found in payload")
	}
	if csIDHex != util.BytesToHex(csID) {
		t.Errorf("expected changeSetID %s, got %s", util.BytesToHex(csID), csIDHex)
	}
}

func TestGetChangeSetIntent_RetrievesViaEdge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	csID := createTestChangeSet(t, db)
	gen := NewGenerator(db)

	// Initially no intent
	intent, err := gen.GetChangeSetIntent(csID)
	if err != nil {
		t.Fatalf("getting intent: %v", err)
	}
	if intent != "" {
		t.Errorf("expected empty intent, got %q", intent)
	}

	// Set intent
	intentText := "Fix critical bug in payment processing"
	err = gen.UpdateChangeSetIntent(csID, intentText)
	if err != nil {
		t.Fatalf("updating intent: %v", err)
	}

	// Retrieve intent
	intent, err = gen.GetChangeSetIntent(csID)
	if err != nil {
		t.Fatalf("getting intent: %v", err)
	}
	if intent != intentText {
		t.Errorf("expected intent %q, got %q", intentText, intent)
	}
}

func TestUpdateChangeSetIntent_ReplacesOldIntent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	csID := createTestChangeSet(t, db)
	gen := NewGenerator(db)

	// Set first intent
	err := gen.UpdateChangeSetIntent(csID, "First intent")
	if err != nil {
		t.Fatalf("updating intent: %v", err)
	}

	// Get first intent node ID
	edges1, _ := db.GetEdges(csID, graph.EdgeHasIntent)
	firstIntentID := edges1[0].Dst

	// Set second intent
	err = gen.UpdateChangeSetIntent(csID, "Second intent")
	if err != nil {
		t.Fatalf("updating intent: %v", err)
	}

	// Verify only one edge exists
	edges2, err := db.GetEdges(csID, graph.EdgeHasIntent)
	if err != nil {
		t.Fatalf("getting edges: %v", err)
	}
	if len(edges2) != 1 {
		t.Fatalf("expected 1 HAS_INTENT edge after update, got %d", len(edges2))
	}

	// Verify it's a different intent node
	secondIntentID := edges2[0].Dst
	if bytes.Equal(firstIntentID, secondIntentID) {
		t.Error("expected different intent node after update")
	}

	// Verify intent text is updated
	intent, _ := gen.GetChangeSetIntent(csID)
	if intent != "Second intent" {
		t.Errorf("expected 'Second intent', got %q", intent)
	}
}

func TestIntentNode_IsContentAddressed(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	csID := createTestChangeSet(t, db)
	gen := NewGenerator(db)

	intentText := "Refactor database layer"
	err := gen.UpdateChangeSetIntent(csID, intentText)
	if err != nil {
		t.Fatalf("updating intent: %v", err)
	}

	// Get the Intent node
	edges, _ := db.GetEdges(csID, graph.EdgeHasIntent)
	intentNode, _ := db.GetNode(edges[0].Dst)

	// Get raw payload and verify ID can be recomputed
	kind, rawPayloadJSON, err := db.GetNodeRawPayload(intentNode.ID)
	if err != nil {
		t.Fatalf("getting raw payload: %v", err)
	}
	if kind != graph.KindIntent {
		t.Errorf("expected kind %s, got %s", graph.KindIntent, kind)
	}

	// Recompute ID from content
	content := append([]byte(string(kind)+"\n"), rawPayloadJSON...)
	computedID := util.Blake3Hash(content)

	if !bytes.Equal(computedID, intentNode.ID) {
		t.Errorf("content-addressing broken: computed ID %x != stored ID %x",
			computedID[:8], intentNode.ID[:8])
	}
}

func TestChangeSetPayload_NotModifiedByIntent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	csID := createTestChangeSet(t, db)

	// Get original raw payload
	_, originalPayload, err := db.GetNodeRawPayload(csID)
	if err != nil {
		t.Fatalf("getting original payload: %v", err)
	}

	// Update intent
	gen := NewGenerator(db)
	err = gen.UpdateChangeSetIntent(csID, "New intent")
	if err != nil {
		t.Fatalf("updating intent: %v", err)
	}

	// Get payload after intent update
	_, afterPayload, err := db.GetNodeRawPayload(csID)
	if err != nil {
		t.Fatalf("getting payload after update: %v", err)
	}

	// Verify changeset payload was NOT modified
	if !bytes.Equal(originalPayload, afterPayload) {
		t.Error("changeset payload was modified by intent update - this breaks content-addressing!")
		t.Logf("Original: %s", originalPayload)
		t.Logf("After: %s", afterPayload)
	}
}

func TestRenderIntent_UsesIntentNode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	csID := createTestChangeSet(t, db)
	gen := NewGenerator(db)

	// Render with edit text
	editText := "Custom intent description"
	result, err := gen.RenderIntent(csID, editText, false)
	if err != nil {
		t.Fatalf("rendering intent: %v", err)
	}
	if result != editText {
		t.Errorf("expected %q, got %q", editText, result)
	}

	// Verify intent is stored in separate node
	edges, _ := db.GetEdges(csID, graph.EdgeHasIntent)
	if len(edges) != 1 {
		t.Fatalf("expected 1 HAS_INTENT edge, got %d", len(edges))
	}

	// Render again without edit - should return stored intent
	result2, err := gen.RenderIntent(csID, "", false)
	if err != nil {
		t.Fatalf("rendering intent: %v", err)
	}
	if result2 != editText {
		t.Errorf("expected stored intent %q, got %q", editText, result2)
	}
}
