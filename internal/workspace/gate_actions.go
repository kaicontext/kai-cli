package workspace

import (
	"fmt"
	"strings"

	"kai/internal/ref"
	"github.com/kaicontext/kai-engine/util"
)

// ApproveHeld advances every non-ws.* ref currently pointing at the
// held snapshot's original target, effectively publishing a previously-
// gated integration after human review.
//
// Refuses if the original target ref has moved past the integrate
// point — re-running `kai integrate` against the current target is the
// safer fix in that case (otherwise we'd regress newer auto-promoted
// changes).
//
// Returns the names of refs that were advanced.
//
// This is the engine-level operation behind both `kai gate approve`
// and the TUI's gate pane "approve" hotkey.
func (m *Manager) ApproveHeld(snapID []byte) ([]string, error) {
	snap, err := m.db.GetNode(snapID)
	if err != nil {
		return nil, fmt.Errorf("getting snapshot: %w", err)
	}
	if snap == nil {
		return nil, fmt.Errorf("snapshot not found")
	}
	if dismissed, _ := snap.Payload["dismissed"].(bool); dismissed {
		return nil, fmt.Errorf("snapshot is dismissed; cannot approve")
	}

	targetHex, _ := snap.Payload["targetSnapshot"].(string)
	if targetHex == "" {
		return nil, fmt.Errorf("snapshot has no targetSnapshot in payload")
	}
	targetID, err := util.HexToBytes(targetHex)
	if err != nil {
		return nil, fmt.Errorf("decoding target hex: %w", err)
	}

	wsHex, _ := snap.Payload["integratedFrom"].(string)
	orchAgent, _ := snap.Payload["orchestratorAgent"].(string)
	if wsHex == "" && orchAgent == "" {
		return nil, fmt.Errorf("snapshot has neither integratedFrom workspace nor orchestratorAgent — cannot determine approval path")
	}

	// Confirm at least one user-named ref still points at the original
	// target. If none, the world has moved on — advancing now would
	// leak old changes back in front of newer ones. Same check applies
	// to both producer paths.
	refMgr := ref.NewRefManager(m.db)
	refs, err := refMgr.List(nil)
	if err != nil {
		return nil, fmt.Errorf("listing refs: %w", err)
	}
	stillAtTarget := false
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "ws.") {
			continue
		}
		if util.BytesToHex(r.TargetID) == targetHex {
			stillAtTarget = true
			break
		}
	}
	if !stillAtTarget {
		return nil, fmt.Errorf("no team-visible ref still points at the original target %s; "+
			"re-run `kai integrate` to refresh the merge against the current target", targetHex[:12])
	}

	var advancedRefs []string
	if wsHex != "" {
		// Workspace path: PublishAtTarget walks the workspace's
		// ws.* refs and advances any team-visible ref that points
		// at the integrate target.
		ws, err := m.Get(wsHex)
		if err != nil || ws == nil {
			return nil, fmt.Errorf("workspace %s not found: %v", wsHex[:12], err)
		}
		report, err := m.PublishAtTarget(
			ws,
			&IntegrateResult{ResultSnapshot: snap.ID},
			targetID,
			PublishOptions{SkipGate: true},
		)
		if err != nil {
			return nil, err
		}
		advancedRefs = report.AdvancedRefs
	} else {
		// Orchestrator path: there's no workspace. The held
		// snapshot was produced by `kai code`'s orchestrator
		// integrating directly into mainRepo, so the only thing
		// to advance is snap.latest itself. Set it to the held
		// snap ID; that's exactly what the orchestrator would
		// have done if the gate had returned Auto in the first
		// place.
		if err := refMgr.Set("snap.latest", snap.ID, ref.KindSnapshot); err != nil {
			return nil, fmt.Errorf("advance snap.latest: %w", err)
		}
		advancedRefs = []string{"snap.latest"}
	}

	// Mark the snapshot as resolved so safetygate.IsHeld no longer
	// returns it. Without this, the held list keeps showing the
	// snapshot after approval — TUI status bar still says "Gate: N
	// held", `kai gate list` still lists it, and re-launching kai
	// re-shows the same items in /gate. We use the existing
	// `dismissed` flag (same field RejectHeld sets) since IsHeld
	// already filters on it; `approvedAt` distinguishes approves
	// from rejects in audit. Best-effort: a write failure here
	// doesn't undo the ref advance (which already succeeded), but
	// it does mean the held list will still show this snap until
	// the next manual cleanup. Surfaced as an error so the caller
	// can flag it.
	// Mutable status lives in the snapshot_meta side table, NOT the hashed
	// payload — writing it into the payload would break the snapshot's
	// content-address (id != blake3(payload)) and corrupt the store.
	if err := m.db.MergeSnapshotMeta(snap.ID, map[string]interface{}{
		"dismissed":  true,
		"approvedAt": util.NowMs(),
	}); err != nil {
		return advancedRefs, fmt.Errorf("refs advanced but marking resolved failed: %w", err)
	}
	return advancedRefs, nil
}

// RejectHeld marks a held snapshot as dismissed. The snapshot is not
// deleted — keeping it preserves the audit trail and lets the agent's
// later work supersede it organically (a fresh integrate produces a
// new snapshot with a fresh verdict).
func (m *Manager) RejectHeld(snapID []byte) error {
	snap, err := m.db.GetNode(snapID)
	if err != nil {
		return fmt.Errorf("getting snapshot: %w", err)
	}
	if snap == nil {
		return fmt.Errorf("snapshot not found")
	}
	if dismissed, _ := snap.Payload["dismissed"].(bool); dismissed {
		return nil // idempotent — already dismissed
	}
	if err := m.db.MergeSnapshotMeta(snap.ID, map[string]interface{}{
		"dismissed":   true,
		"dismissedAt": util.NowMs(),
	}); err != nil {
		return fmt.Errorf("marking dismissed: %w", err)
	}
	return nil
}

