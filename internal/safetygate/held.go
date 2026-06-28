package safetygate

import (
	"sort"
	"strings"

	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/util"
)

// HeldStore is the subset of *graph.DB needed to enumerate held
// snapshots. Defined as an interface so tests and TUI views can
// substitute lightweight fakes.
type HeldStore interface {
	GetNodesByKind(kind graph.NodeKind) ([]*graph.Node, error)
}

// ListHeld returns Snapshot nodes whose gate verdict is non-Auto
// (review or block) and that have not been dismissed. Newest first.
//
// Both `kai gate list` and the TUI's gate pane call this; keeping it
// here means there is one definition of "held" across surfaces.
func ListHeld(db HeldStore) ([]*graph.Node, error) {
	all, err := db.GetNodesByKind(graph.KindSnapshot)
	if err != nil {
		return nil, err
	}
	var held []*graph.Node
	for _, n := range all {
		if !IsHeld(n) {
			continue
		}
		held = append(held, n)
	}
	sort.Slice(held, func(i, j int) bool {
		ai, _ := held[i].Payload["createdAt"].(float64)
		aj, _ := held[j].Payload["createdAt"].(float64)
		return ai > aj
	})
	return held, nil
}

// IsHeld reports whether a single Snapshot node is currently held by
// the gate (non-Auto verdict, not dismissed). Exported so callers can
// reuse the predicate without re-listing — e.g. the TUI checking
// whether a freshly-integrated snapshot needs the user's attention.
func IsHeld(n *graph.Node) bool {
	if n == nil || n.Payload == nil {
		return false
	}
	v, _ := n.Payload["gateVerdict"].(string)
	if v != string(Review) && v != string(Block) {
		return false
	}
	if dismissed, _ := n.Payload["dismissed"].(bool); dismissed {
		return false
	}
	// A snapshot whose targetSnapshot is its own ID is degenerate:
	// the integration recorded a hold with no change to review. Its
	// diff against the target is necessarily empty and it can never
	// be approved (HeldSnapshotDiff sees identical content; approve
	// has no integratedFrom). Observed when a multi-root integration
	// holds a project the agent never touched. Never surface it as
	// held — it is permanent, un-resolvable noise otherwise.
	if tgt, _ := n.Payload["targetSnapshot"].(string); tgt != "" {
		if strings.EqualFold(tgt, util.BytesToHex(n.ID)) {
			return false
		}
	}
	return true
}
