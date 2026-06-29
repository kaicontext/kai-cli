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

// TestRefresh_AggregatesAcrossWorkspace pins the 2026-06-11 fix: the
// gate is workspace-wide, so Refresh() reports held integrations from
// EVERY project, not just the primary. The old primary-only scope
// silently hid sibling held work (`/gate` in kai/ showed "nothing held"
// while kai-tui had 4). Each item also carries its project tag + a
// manager bound to that project's DB so approve/reject hit the right
// store.
func TestRefresh_AggregatesAcrossWorkspace(t *testing.T) {
	heldDB := gateTestDB(t, true)   // 1 held integration
	emptyDB := gateTestDB(t, false) // 0 held

	// Primary empty, sibling held → the sibling's held item now COUNTS
	// (workspace-wide), and it's tagged with its project name.
	primaryEmpty := &projects.Project{Path: "/ws/a", Name: "a", DB: emptyDB}
	siblingHeld := &projects.Project{Path: "/ws/b", Name: "b", DB: heldDB}
	g := Gate{set: projects.New("/ws", []*projects.Project{primaryEmpty, siblingHeld})}
	cmd := g.Refresh()
	msg := cmd().(GateRefreshedMsg)
	if len(msg.items) != 1 {
		t.Fatalf("held count = %d, want 1 — Refresh must aggregate sibling projects", len(msg.items))
	}
	if msg.items[0].project != "b" {
		t.Errorf("held item project tag = %q, want %q (multi-root rows are tagged)", msg.items[0].project, "b")
	}
	if msg.items[0].mgr == nil {
		t.Errorf("held item must carry a manager bound to its own project DB for approve/reject")
	}

	// Both projects holding → both count.
	primaryHeld := &projects.Project{Path: "/ws/a", Name: "a", DB: heldDB}
	siblingHeld2 := &projects.Project{Path: "/ws/b", Name: "b", DB: gateTestDB(t, true)}
	g = Gate{set: projects.New("/ws", []*projects.Project{primaryHeld, siblingHeld2})}
	if n := refreshCount(t, g); n != 2 {
		t.Errorf("held count = %d, want 2 — both projects' held items should count", n)
	}
}
