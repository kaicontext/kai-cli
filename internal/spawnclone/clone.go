package spawnclone

import (
	"crypto/rand"
	"fmt"
	"path/filepath"

	"kai/internal/graph"
	"kai/internal/ref"
	"kai/internal/workspace"
)

// RewriteClonedWorkspace opens the graph DB inside `clonedKaiDir` (the
// .kai/ inside a CoW-cloned spawn dir), finds the single Workspace node
// that came along for the ride, regenerates its ID, and rewrites its
// name + agent name. Edges (BASED_ON, HEAD_AT, HAS_CHANGESET) are
// re-pointed at the new ID. Old auto-refs (ws.<oldname>.{base,head})
// are deleted; new ones are written under newName.
//
// This is the surgery that makes "CoW-clone the whole .kai/, then
// claim it as a fresh workspace" work. We assume exactly one workspace
// node lives in the cloned DB (which is the case for a freshly-spawned
// workspace 1 that hasn't yet been used to create more workspaces).
func RewriteClonedWorkspace(clonedKaiDir, newName, newAgent string) error {
	dbPath := filepath.Join(clonedKaiDir, "db.sqlite")
	objPath := filepath.Join(clonedKaiDir, "objects")
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		return fmt.Errorf("opening cloned db: %w", err)
	}
	defer db.Close()

	nodes, err := db.GetNodesByKind(graph.KindWorkspace)
	if err != nil {
		return fmt.Errorf("listing workspaces in clone: %w", err)
	}
	if len(nodes) != 1 {
		return fmt.Errorf("clone has %d workspace nodes, expected 1 — is this a fresh spawn?", len(nodes))
	}
	old := nodes[0]
	oldName, _ := old.Payload["name"].(string)

	// Generate fresh 16-byte workspace ID.
	newID := make([]byte, 16)
	if _, err := rand.Read(newID); err != nil {
		return fmt.Errorf("generating workspace id: %w", err)
	}

	// Snapshot edges before we touch anything. Each is (oldID → dstID).
	headEdges, _ := db.GetEdges(old.ID, graph.EdgeHeadAt)
	baseEdges, _ := db.GetEdges(old.ID, graph.EdgeBasedOn)
	csEdges, _ := db.GetEdges(old.ID, graph.EdgeHasChangeSet)

	// Build the rewritten payload.
	payload := make(map[string]interface{}, len(old.Payload))
	for k, v := range old.Payload {
		payload[k] = v
	}
	payload["name"] = newName
	payload["agentName"] = newAgent

	tx, err := db.BeginTx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert new workspace at new ID.
	if err := db.InsertWorkspace(tx, newID, payload); err != nil {
		return fmt.Errorf("inserting rewritten workspace: %w", err)
	}

	// Re-point edges.
	for _, e := range headEdges {
		if err := db.InsertEdge(tx, newID, graph.EdgeHeadAt, e.Dst, nil); err != nil {
			return fmt.Errorf("re-inserting HEAD_AT: %w", err)
		}
		if err := db.DeleteEdge(tx, old.ID, graph.EdgeHeadAt, e.Dst); err != nil {
			return fmt.Errorf("deleting old HEAD_AT: %w", err)
		}
	}
	for _, e := range baseEdges {
		if err := db.InsertEdge(tx, newID, graph.EdgeBasedOn, e.Dst, nil); err != nil {
			return fmt.Errorf("re-inserting BASED_ON: %w", err)
		}
		if err := db.DeleteEdge(tx, old.ID, graph.EdgeBasedOn, e.Dst); err != nil {
			return fmt.Errorf("deleting old BASED_ON: %w", err)
		}
	}
	for _, e := range csEdges {
		if err := db.InsertEdge(tx, newID, graph.EdgeHasChangeSet, e.Dst, nil); err != nil {
			return fmt.Errorf("re-inserting HAS_CHANGESET: %w", err)
		}
		if err := db.DeleteEdge(tx, old.ID, graph.EdgeHasChangeSet, e.Dst); err != nil {
			return fmt.Errorf("deleting old HAS_CHANGESET: %w", err)
		}
	}

	// Delete the old workspace node.
	if _, err := tx.Exec(`DELETE FROM nodes WHERE id = ? AND kind = ?`, old.ID, string(graph.KindWorkspace)); err != nil {
		return fmt.Errorf("deleting old workspace node: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Rewrite auto-refs: delete ws.<oldname>.{base,head}, create
	// ws.<newname>.{base,head} pointing at the same base snapshot.
	refMgr := ref.NewRefManager(db)
	autoRefs := ref.NewAutoRefManager(db)
	if oldName != "" {
		_ = refMgr.Delete(fmt.Sprintf("ws.%s.base", oldName))
		_ = refMgr.Delete(fmt.Sprintf("ws.%s.head", oldName))
	}
	// Look up base snapshot from the rewritten payload to create new refs.
	mgr := workspace.NewManager(db)
	ws, err := mgr.Get(newName)
	if err != nil || ws == nil {
		return fmt.Errorf("could not re-fetch rewritten workspace: %w", err)
	}
	if err := autoRefs.OnWorkspaceCreated(newName, ws.BaseSnapshot); err != nil {
		return fmt.Errorf("rewriting auto-refs: %w", err)
	}
	return nil
}
