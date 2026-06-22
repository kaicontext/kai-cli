package views

import (
	"os"
	"path/filepath"
	"testing"

	"kai/api/graph"
	"kai/api/projects"
)

// gateTestDB opens a temp graph DB with the minimal schema. When held
// is true it also inserts one review-verdict Snapshot node, so the DB
// reports exactly one held integration.
func gateTestDB(t *testing.T, held bool) *graph.DB {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := graph.OpenDB(filepath.Join(dir, "test.db"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatalf("graph.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(
		`CREATE TABLE IF NOT EXISTS nodes (id BLOB PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL);`,
	); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if held {
		if _, err := db.InsertNodeDirect(graph.KindSnapshot, map[string]interface{}{
			"gateVerdict": "review",
		}); err != nil {
			t.Fatalf("insert held snapshot: %v", err)
		}
	}
	return db
}

// refreshCount runs Gate.Refresh() to completion and returns how many
// held items the resulting GateRefreshedMsg carried.
func refreshCount(t *testing.T, g Gate) int {
	t.Helper()
	cmd := g.Refresh()
	if cmd == nil {
		t.Fatal("Refresh() returned nil cmd")
	}
	msg, ok := cmd().(GateRefreshedMsg)
	if !ok {
		t.Fatalf("Refresh cmd produced %T, want GateRefreshedMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("GateRefreshedMsg.err: %v", msg.err)
	}
	return len(msg.items)
}

// TestRefresh_ScopesToPrimaryProject is the regression test for the
// multi-root gate-count bug: Refresh() must report only the PRIMARY
// project's held integrations, not aggregate across the whole set. A
// held item in a sibling project the user isn't in must not inflate
// the count (it made the status bar say "1 held" while `/gate list`,
// primary-scoped, showed nothing).
func TestRefresh_ScopesToPrimaryProject(t *testing.T) {
	heldDB := gateTestDB(t, true)   // 1 held integration
	emptyDB := gateTestDB(t, false) // 0 held

	// Primary has nothing held; a sibling project does. The count
	// must follow the primary — i.e. 0, not 1.
	primaryEmpty := &projects.Project{Path: "/ws/a", Name: "a", DB: emptyDB}
	siblingHeld := &projects.Project{Path: "/ws/b", Name: "b", DB: heldDB}
	g := Gate{set: projects.New("/ws", []*projects.Project{primaryEmpty, siblingHeld})}
	if n := refreshCount(t, g); n != 0 {
		t.Errorf("held count = %d, want 0 — Refresh aggregated a sibling project instead of scoping to primary", n)
	}

	// Flip it: when the PRIMARY itself has the held item, it counts.
	primaryHeld := &projects.Project{Path: "/ws/a", Name: "a", DB: heldDB}
	siblingEmpty := &projects.Project{Path: "/ws/b", Name: "b", DB: emptyDB}
	g = Gate{set: projects.New("/ws", []*projects.Project{primaryHeld, siblingEmpty})}
	if n := refreshCount(t, g); n != 1 {
		t.Errorf("held count = %d, want 1 — the primary project's held item should count", n)
	}
}
