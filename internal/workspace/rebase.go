package workspace

import (
	"fmt"

	"kai/internal/graph"
	"kai/internal/ref"
)

// RebaseResult contains the result of rebasing changesets onto a new base.
type RebaseResult struct {
	BaseSnapshot   []byte
	Applied        [][]byte
	Conflicts      []Conflict
	ResultSnapshot []byte
}

// Rebase applies a list of changesets onto a target snapshot in order.
func (m *Manager) Rebase(changeSetIDs [][]byte, targetSnapshotID []byte) (*RebaseResult, error) {
	if len(changeSetIDs) == 0 {
		return nil, fmt.Errorf("no changesets provided")
	}
	targetSnap, err := m.db.GetNode(targetSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("getting target snapshot: %w", err)
	}
	if targetSnap == nil || targetSnap.Kind != graph.KindSnapshot {
		return nil, fmt.Errorf("target snapshot not found")
	}

	current := targetSnapshotID
	var applied [][]byte
	for _, csID := range changeSetIDs {
		result, err := m.CherryPick(csID, current)
		if err != nil {
			return nil, err
		}
		if len(result.Conflicts) > 0 {
			return &RebaseResult{
				BaseSnapshot:   targetSnapshotID,
				Applied:        applied,
				Conflicts:      result.Conflicts,
				ResultSnapshot: current,
			}, nil
		}
		applied = append(applied, csID)
		current = result.ResultSnapshot
	}

	return &RebaseResult{
		BaseSnapshot:   targetSnapshotID,
		Applied:        applied,
		ResultSnapshot: current,
	}, nil
}

// ResolveChangesetChain resolves a list of changeset selectors to IDs.
func ResolveChangesetChain(db *graph.DB, selectors []string) ([][]byte, error) {
	var ids [][]byte
	resolver := ref.NewResolver(db)
	kind := ref.KindChangeSet
	for _, sel := range selectors {
		result, err := resolver.Resolve(sel, &kind)
		if err != nil {
			return nil, err
		}
		ids = append(ids, result.ID)
	}
	return ids, nil
}
