package workspace

import (
	"encoding/json"
	"fmt"

	"kai/internal/util"
)

// ConflictState represents pending conflicts for a workspace integration.
type ConflictState struct {
	WorkspaceID    string     `json:"workspaceId"`
	TargetSnapshot string     `json:"targetSnapshot"`
	Conflicts      []Conflict `json:"conflicts"`
	CreatedAt      int64      `json:"createdAt"`
}

// SaveConflictState persists conflict state in the workspace payload.
func (m *Manager) SaveConflictState(wsID []byte, targetSnapshotID []byte, conflicts []Conflict) error {
	node, err := m.db.GetNode(wsID)
	if err != nil {
		return fmt.Errorf("getting workspace node: %w", err)
	}
	if node == nil {
		return fmt.Errorf("workspace not found")
	}

	state := ConflictState{
		WorkspaceID:    util.BytesToHex(wsID),
		TargetSnapshot: util.BytesToHex(targetSnapshotID),
		Conflicts:      conflicts,
		CreatedAt:      util.NowMs(),
	}

	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling conflict state: %w", err)
	}

	// Store as a raw JSON string in the payload map
	var stateMap map[string]interface{}
	if err := json.Unmarshal(stateJSON, &stateMap); err != nil {
		return fmt.Errorf("converting conflict state: %w", err)
	}

	node.Payload["pendingConflicts"] = stateMap
	return m.db.UpdateNodePayload(wsID, node.Payload)
}

// GetConflictState reads pending conflict state from a workspace.
func (m *Manager) GetConflictState(nameOrID string) (*ConflictState, error) {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", nameOrID)
	}

	node, err := m.db.GetNode(ws.ID)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, nil
	}

	pending, ok := node.Payload["pendingConflicts"]
	if !ok || pending == nil {
		return nil, nil
	}

	// Re-marshal and unmarshal to get a typed struct
	raw, err := json.Marshal(pending)
	if err != nil {
		return nil, fmt.Errorf("marshaling pending conflicts: %w", err)
	}

	var state ConflictState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("unmarshaling conflict state: %w", err)
	}

	return &state, nil
}

// ClearConflictState removes pending conflict state from a workspace.
func (m *Manager) ClearConflictState(nameOrID string) error {
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
		return nil
	}

	delete(node.Payload, "pendingConflicts")
	return m.db.UpdateNodePayload(ws.ID, node.Payload)
}
