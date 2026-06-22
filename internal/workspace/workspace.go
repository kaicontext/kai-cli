// Package workspace provides workspace (branching) operations for IVCS.
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"kai/internal/graph"
	"kai/internal/util"
)

// Status represents the state of a workspace.
type Status string

const (
	StatusActive  Status = "active"
	StatusShelved Status = "shelved"
	StatusClosed  Status = "closed"
)

// Workspace represents a mutable overlay on immutable snapshots.
type Workspace struct {
	ID             []byte
	Name           string
	BaseSnapshot   []byte
	HeadSnapshot   []byte
	Status         Status
	OpenChangeSets [][]byte
	Description    string
	CreatedAt      int64
	AgentName      string
}

// Manager handles workspace operations.
type Manager struct {
	db *graph.DB
}

// NewManager creates a new workspace manager.
func NewManager(db *graph.DB) *Manager {
	return &Manager{db: db}
}

// Create creates a new workspace based on a snapshot.
func (m *Manager) Create(name string, baseSnapshotID []byte, description string) (*Workspace, error) {
	// Check if workspace with same name already exists
	existing, err := m.db.GetWorkspaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("checking existing workspace: %w", err)
	}
	if existing != nil {
		return nil, fmt.Errorf("workspace %q already exists", name)
	}

	// Verify base snapshot exists
	baseSnap, err := m.db.GetNode(baseSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("getting base snapshot: %w", err)
	}
	if baseSnap == nil {
		return nil, fmt.Errorf("base snapshot not found")
	}
	if baseSnap.Kind != graph.KindSnapshot {
		return nil, fmt.Errorf("base must be a snapshot, got %s", baseSnap.Kind)
	}

	// Generate UUID-like ID for workspace
	wsID := make([]byte, 16)
	if _, err := rand.Read(wsID); err != nil {
		return nil, fmt.Errorf("generating workspace ID: %w", err)
	}

	now := util.NowMs()
	payload := map[string]interface{}{
		"name":           name,
		"baseSnapshot":   util.BytesToHex(baseSnapshotID),
		"headSnapshot":   util.BytesToHex(baseSnapshotID), // starts as base
		"status":         string(StatusActive),
		"openChangeSets": []interface{}{},
		"description":    description,
		"createdAt":      now,
		"agentName":      "",
	}

	tx, err := m.db.BeginTx()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert workspace node
	if err := m.db.InsertWorkspace(tx, wsID, payload); err != nil {
		return nil, fmt.Errorf("inserting workspace: %w", err)
	}

	// Create edges
	if err := m.db.InsertEdge(tx, wsID, graph.EdgeBasedOn, baseSnapshotID, nil); err != nil {
		return nil, fmt.Errorf("inserting BASED_ON edge: %w", err)
	}
	if err := m.db.InsertEdge(tx, wsID, graph.EdgeHeadAt, baseSnapshotID, nil); err != nil {
		return nil, fmt.Errorf("inserting HEAD_AT edge: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return &Workspace{
		ID:             wsID,
		Name:           name,
		BaseSnapshot:   baseSnapshotID,
		HeadSnapshot:   baseSnapshotID,
		Status:         StatusActive,
		OpenChangeSets: nil,
		Description:    description,
		CreatedAt:      now,
	}, nil
}

// Get retrieves a workspace by name or ID.
func (m *Manager) Get(nameOrID string) (*Workspace, error) {
	// Try by name first
	node, err := m.db.GetWorkspaceByName(nameOrID)
	if err != nil {
		return nil, err
	}

	// If not found by name, try by ID
	if node == nil {
		id, err := util.HexToBytes(nameOrID)
		if err == nil {
			node, err = m.db.GetNode(id)
			if err != nil {
				return nil, err
			}
			if node != nil && node.Kind != graph.KindWorkspace {
				node = nil
			}
		}
	}

	if node == nil {
		return nil, nil
	}

	return nodeToWorkspace(node)
}

// List returns all workspaces.
func (m *Manager) List() ([]*Workspace, error) {
	nodes, err := m.db.GetNodesByKind(graph.KindWorkspace)
	if err != nil {
		return nil, err
	}

	workspaces := make([]*Workspace, 0, len(nodes))
	for _, node := range nodes {
		ws, err := nodeToWorkspace(node)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, ws)
	}

	return workspaces, nil
}

// Shelve freezes a workspace (no new staging).
func (m *Manager) Shelve(nameOrID string) error {
	return m.setStatus(nameOrID, StatusShelved, StatusActive)
}

// Unshelve returns a workspace to active state.
func (m *Manager) Unshelve(nameOrID string) error {
	return m.setStatus(nameOrID, StatusActive, StatusShelved)
}

// Close permanently closes a workspace.
func (m *Manager) Close(nameOrID string) error {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("workspace not found: %s", nameOrID)
	}
	if ws.Status == StatusClosed {
		return fmt.Errorf("workspace already closed")
	}

	return m.updateStatus(ws.ID, StatusClosed)
}

// DeletePlan contains information about what will be deleted.
type DeletePlan struct {
	WorkspaceID     []byte
	WorkspaceName   string
	EdgesRemoved    int
	RefsRemoved     []string
	OrphanedCSCount int
	OrphanedSnaps   int
}

// PlanDelete computes what would be deleted without actually deleting.
func (m *Manager) PlanDelete(nameOrID string) (*DeletePlan, error) {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", nameOrID)
	}

	plan := &DeletePlan{
		WorkspaceID:   ws.ID,
		WorkspaceName: ws.Name,
	}

	// Count edges
	headEdges, _ := m.db.GetEdges(ws.ID, graph.EdgeHeadAt)
	baseEdges, _ := m.db.GetEdges(ws.ID, graph.EdgeBasedOn)
	csEdges, _ := m.db.GetEdges(ws.ID, graph.EdgeHasChangeSet)
	plan.EdgesRemoved = len(headEdges) + len(baseEdges) + len(csEdges)

	// Workspace refs that would be removed
	plan.RefsRemoved = []string{
		"ws." + ws.Name + ".base",
		"ws." + ws.Name + ".head",
	}

	// Count orphaned changesets (those only referenced by this workspace)
	plan.OrphanedCSCount = len(ws.OpenChangeSets)

	// Orphaned snapshots (simplified - actual GC will do proper reachability)
	plan.OrphanedSnaps = 0
	if len(headEdges) > 0 {
		plan.OrphanedSnaps++
	}

	return plan, nil
}

// Delete permanently removes a workspace node, its edges, and related refs.
// Content (snapshots, changesets) is NOT deleted - use prune for that.
func (m *Manager) Delete(nameOrID string, keepRefs bool) error {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("workspace not found: %s", nameOrID)
	}

	tx, err := m.db.BeginTx()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Delete edges: HEAD_AT, BASED_ON, HAS_CHANGESET
	headEdges, _ := m.db.GetEdges(ws.ID, graph.EdgeHeadAt)
	for _, e := range headEdges {
		if err := m.db.DeleteEdge(tx, ws.ID, graph.EdgeHeadAt, e.Dst); err != nil {
			return fmt.Errorf("deleting HEAD_AT edge: %w", err)
		}
	}

	baseEdges, _ := m.db.GetEdges(ws.ID, graph.EdgeBasedOn)
	for _, e := range baseEdges {
		if err := m.db.DeleteEdge(tx, ws.ID, graph.EdgeBasedOn, e.Dst); err != nil {
			return fmt.Errorf("deleting BASED_ON edge: %w", err)
		}
	}

	csEdges, _ := m.db.GetEdges(ws.ID, graph.EdgeHasChangeSet)
	for _, e := range csEdges {
		if err := m.db.DeleteEdge(tx, ws.ID, graph.EdgeHasChangeSet, e.Dst); err != nil {
			return fmt.Errorf("deleting HAS_CHANGESET edge: %w", err)
		}
	}

	// Delete the workspace node
	_, err = tx.Exec(`DELETE FROM nodes WHERE id = ? AND kind = ?`, ws.ID, string(graph.KindWorkspace))
	if err != nil {
		return fmt.Errorf("deleting workspace node: %w", err)
	}

	// Delete refs unless --keep-refs
	if !keepRefs {
		refNames := []string{
			"ws." + ws.Name + ".base",
			"ws." + ws.Name + ".head",
		}
		for _, refName := range refNames {
			_, err = tx.Exec(`DELETE FROM refs WHERE name = ?`, refName)
			if err != nil {
				return fmt.Errorf("deleting ref %s: %w", refName, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// setStatus changes workspace status with validation.
func (m *Manager) setStatus(nameOrID string, newStatus, expectedCurrent Status) error {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("workspace not found: %s", nameOrID)
	}
	if ws.Status == StatusClosed {
		return fmt.Errorf("workspace is closed")
	}
	if ws.Status != expectedCurrent {
		return fmt.Errorf("workspace is %s, expected %s", ws.Status, expectedCurrent)
	}

	return m.updateStatus(ws.ID, newStatus)
}

// updateStatus updates the status in the database.
func (m *Manager) updateStatus(wsID []byte, status Status) error {
	node, err := m.db.GetNode(wsID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("workspace not found")
	}

	node.Payload["status"] = string(status)
	return m.db.UpdateNodePayload(wsID, node.Payload)
}

// UpdateHead updates the head snapshot of a workspace.
func (m *Manager) UpdateHead(wsID, newHeadID []byte) error {
	node, err := m.db.GetNode(wsID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("workspace not found")
	}

	// Get current head to delete old edge
	currentHeadHex, _ := node.Payload["headSnapshot"].(string)
	currentHead, _ := util.HexToBytes(currentHeadHex)

	tx, err := m.db.BeginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete old HEAD_AT edge
	if len(currentHead) > 0 {
		if err := m.db.DeleteEdge(tx, wsID, graph.EdgeHeadAt, currentHead); err != nil {
			return err
		}
	}

	// Insert new HEAD_AT edge
	if err := m.db.InsertEdge(tx, wsID, graph.EdgeHeadAt, newHeadID, nil); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Update payload
	node.Payload["headSnapshot"] = util.BytesToHex(newHeadID)
	return m.db.UpdateNodePayload(wsID, node.Payload)
}

// AddChangeSet adds a changeset to the workspace's open changesets.
func (m *Manager) AddChangeSet(wsID, changeSetID []byte) error {
	node, err := m.db.GetNode(wsID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("workspace not found")
	}

	// Get current changeset list
	openCSs, _ := node.Payload["openChangeSets"].([]interface{})
	openCSs = append(openCSs, util.BytesToHex(changeSetID))
	node.Payload["openChangeSets"] = openCSs

	// Create edge
	if err := m.db.InsertEdgeDirect(wsID, graph.EdgeHasChangeSet, changeSetID, nil); err != nil {
		return err
	}

	return m.db.UpdateNodePayload(wsID, node.Payload)
}

// GetLog returns the changesets in a workspace in order.
func (m *Manager) GetLog(nameOrID string) ([]*graph.Node, error) {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", nameOrID)
	}

	var changesets []*graph.Node
	for _, csID := range ws.OpenChangeSets {
		node, err := m.db.GetNode(csID)
		if err != nil {
			return nil, err
		}
		if node != nil {
			changesets = append(changesets, node)
		}
	}

	return changesets, nil
}

// nodeToWorkspace converts a graph node to a Workspace struct.
func nodeToWorkspace(node *graph.Node) (*Workspace, error) {
	name, _ := node.Payload["name"].(string)
	status, _ := node.Payload["status"].(string)
	description, _ := node.Payload["description"].(string)
	createdAt, _ := node.Payload["createdAt"].(float64)
	agentName, _ := node.Payload["agentName"].(string)

	baseHex, _ := node.Payload["baseSnapshot"].(string)
	headHex, _ := node.Payload["headSnapshot"].(string)

	baseID, _ := util.HexToBytes(baseHex)
	headID, _ := util.HexToBytes(headHex)

	// Parse open changesets
	var openCSs [][]byte
	if csArr, ok := node.Payload["openChangeSets"].([]interface{}); ok {
		for _, csHex := range csArr {
			if hexStr, ok := csHex.(string); ok {
				if csID, err := util.HexToBytes(hexStr); err == nil {
					openCSs = append(openCSs, csID)
				}
			}
		}
	}

	return &Workspace{
		ID:             node.ID,
		Name:           name,
		BaseSnapshot:   baseID,
		HeadSnapshot:   headID,
		Status:         Status(status),
		OpenChangeSets: openCSs,
		Description:    description,
		CreatedAt:      int64(createdAt),
		AgentName:      agentName,
	}, nil
}

// SetAgentName updates the agent name stored in workspace metadata.
// Used by `kai spawn` to pre-register an agent so `kai checkpoint` can
// fall back to this when no --agent flag is passed.
func (m *Manager) SetAgentName(nameOrID, agentName string) error {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("workspace not found: %s", nameOrID)
	}
	node, err := m.db.GetNode(ws.ID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("workspace node missing")
	}
	node.Payload["agentName"] = agentName
	return m.db.UpdateNodePayload(ws.ID, node.Payload)
}

// IDToHex converts workspace ID to hex string.
func IDToHex(id []byte) string {
	return hex.EncodeToString(id)
}
