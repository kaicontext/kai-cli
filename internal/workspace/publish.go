package workspace

import (
	"fmt"
	"strings"

	"kai/internal/ref"
	"kai/internal/util"
)

// PublishOptions controls how an integrated result becomes team-visible.
type PublishOptions struct {
	// SkipGate, when true, advances the named ref(s) regardless of the
	// gate verdict. Used by the human-approval path in `kai review` and
	// by tests that intentionally bypass classification.
	SkipGate bool
}

// PublishReport summarizes which refs Publish actually advanced. Useful
// for callers that want to print results to the user; never required.
type PublishReport struct {
	// AdvancedRefs lists the ref names whose target was moved to the
	// integrated snapshot. Empty when the gate held the change.
	AdvancedRefs []string
	// WorkspaceHeadAdvanced is true if ws.<name>.head was moved (always
	// done; this is local-only state, not team-visible).
	WorkspaceHeadAdvanced bool
	// HeldByGate is true when a non-Auto verdict caused the team-visible
	// refs to stay put. Callers should surface this to the user.
	HeldByGate bool
}

// PublishToRef advances a single named ref to the integrated snapshot,
// honoring the gate verdict on the result. Used by `kai integrate`,
// where the user explicitly named the target ref via --into.
//
// The workspace's own head ref (ws.<name>.head) is always advanced —
// that ref is local context, not team-visible state.
func (m *Manager) PublishToRef(ws *Workspace, result *IntegrateResult, refName string, opts PublishOptions) (*PublishReport, error) {
	if ws == nil || result == nil {
		return nil, fmt.Errorf("publish: nil workspace or result")
	}
	if len(result.ResultSnapshot) == 0 {
		return nil, fmt.Errorf("publish: result has no snapshot to advance to")
	}

	report := &PublishReport{}
	allowPublish := opts.SkipGate || gateAllows(result)
	if !allowPublish {
		report.HeldByGate = true
	}

	if allowPublish && refName != "" {
		refMgr := ref.NewRefManager(m.db)
		if existing, _ := refMgr.Get(refName); existing != nil {
			if err := refMgr.Set(refName, result.ResultSnapshot, ref.KindSnapshot); err != nil {
				return report, fmt.Errorf("advancing ref %s: %w", refName, err)
			}
			report.AdvancedRefs = append(report.AdvancedRefs, refName)
		}
	}

	// Always advance the workspace's own head — local-only.
	if err := ref.NewAutoRefManager(m.db).OnWorkspaceHeadChanged(ws.Name, result.ResultSnapshot); err == nil {
		report.WorkspaceHeadAdvanced = true
	}

	return report, nil
}

// PublishAtTarget advances every user-named (non-ws.*) ref currently
// pointing at oldTargetID to the integrated snapshot, honoring the gate
// verdict. Used by `kai resolve`, which knows the snapshot it merged
// against but not which named ref(s) the user originally pointed at.
//
// Workspace head is always advanced.
func (m *Manager) PublishAtTarget(ws *Workspace, result *IntegrateResult, oldTargetID []byte, opts PublishOptions) (*PublishReport, error) {
	if ws == nil || result == nil {
		return nil, fmt.Errorf("publish: nil workspace or result")
	}
	if len(result.ResultSnapshot) == 0 {
		return nil, fmt.Errorf("publish: result has no snapshot to advance to")
	}

	report := &PublishReport{}
	allowPublish := opts.SkipGate || gateAllows(result)
	if !allowPublish {
		report.HeldByGate = true
	}

	if allowPublish && len(oldTargetID) > 0 {
		refMgr := ref.NewRefManager(m.db)
		refs, err := refMgr.List(nil)
		if err != nil {
			return report, fmt.Errorf("listing refs: %w", err)
		}
		oldHex := util.BytesToHex(oldTargetID)
		for _, r := range refs {
			// Skip workspace auto-refs (ws.<name>.head/.base) — those
			// are private to other workspaces and must not be
			// piggy-backed by an unrelated integrate.
			if strings.HasPrefix(r.Name, "ws.") {
				continue
			}
			if util.BytesToHex(r.TargetID) != oldHex {
				continue
			}
			if err := refMgr.Set(r.Name, result.ResultSnapshot, ref.KindSnapshot); err != nil {
				continue
			}
			report.AdvancedRefs = append(report.AdvancedRefs, r.Name)
		}
	}

	if err := ref.NewAutoRefManager(m.db).OnWorkspaceHeadChanged(ws.Name, result.ResultSnapshot); err == nil {
		report.WorkspaceHeadAdvanced = true
	}

	return report, nil
}

// gateAllows is the single point where the verdict is consulted. Until
// the gate is wired into integrateInternal (task 3), result.Decision is
// always nil and this returns true — preserving today's behavior.
//
// Once tasks 3 and 4 land, a Review or Block verdict will return false
// here and Publish will skip the team-visible ref advance.
func gateAllows(result *IntegrateResult) bool {
	if result.Decision == nil {
		return true
	}
	return result.Decision.Verdict == "auto"
}
