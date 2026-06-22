package workspace

import (
	"sort"
	"testing"

	"kai/internal/graph"
	"kai/internal/ref"
	"kai/internal/safetygate"
	"kai/internal/util"
)

// TestGateE2E_BlockListApproveCycle exercises the full safety-gate
// lifecycle: an agent edits a protected file, integrate produces a
// Block verdict, the held snapshot is discoverable by the same query
// `kai gate list` runs, and approval (SkipGate=true) advances the
// team-visible ref to the previously-held snapshot.
//
// This is the integration test that closes the loop on tasks 1–5.
func TestGateE2E_BlockListApproveCycle(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)

	// 1. Set up: a snapshot containing a protected file, plus a
	//    workspace branched off it.
	baseSnap := insertSnapshot(t, db, "base", map[string]string{
		"pkg/auth/login.go": "v1",
	})
	ws, err := mgr.Create("feat", baseSnap, "e2e gate cycle")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Agent edits the protected file.
	headSnap := insertSnapshot(t, db, "head", map[string]string{
		"pkg/auth/login.go": "v2",
	})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// snap.latest currently points at baseSnap.
	refMgr := ref.NewRefManager(db)
	if err := refMgr.Set("snap.latest", baseSnap, ref.KindSnapshot); err != nil {
		t.Fatalf("set ref: %v", err)
	}

	// 2. Integrate with a config that protects the touched directory.
	cfg := safetygate.Config{
		AutoThreshold:  100,
		BlockThreshold: 1000,
		Protected:      []string{"pkg/auth/**"},
	}
	result, err := mgr.IntegrateWithOptions("feat", baseSnap, IntegrateOptions{GateConfig: &cfg})
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if result.Decision == nil || result.Decision.Verdict != "block" {
		t.Fatalf("expected Block verdict, got %+v", result.Decision)
	}
	heldSnapID := result.ResultSnapshot

	// PublishToRef must hold the change.
	wsObj, _ := mgr.Get("feat")
	report, err := mgr.PublishToRef(wsObj, result, "snap.latest", PublishOptions{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !report.HeldByGate || len(report.AdvancedRefs) != 0 {
		t.Fatalf("Block verdict must hold; got report=%+v", report)
	}

	// snap.latest must still be at baseSnap.
	r, _ := refMgr.Get("snap.latest")
	if util.BytesToHex(r.TargetID) != util.BytesToHex(baseSnap) {
		t.Fatal("snap.latest moved despite Block verdict")
	}

	// 3. The held snapshot must be discoverable by the listing filter
	//    (same logic kai gate list uses).
	held := findHeldSnapshots(t, db)
	if !containsHex(held, heldSnapID) {
		t.Fatalf("held snapshot %s missing from gate list", util.BytesToHex(heldSnapID)[:12])
	}

	// 4. Reject path: dismissing removes from the list. Use a second
	//    integration so we can prove dismissal filters correctly without
	//    affecting the snapshot we want to approve.
	dismissedSnapID := makeAnotherHeldSnapshot(t, db, mgr, baseSnap, "feat2", "pkg/auth/oauth.go", cfg)
	dismissNode, err := db.GetNode(dismissedSnapID)
	if err != nil || dismissNode == nil {
		t.Fatalf("get dismissed node: %v", err)
	}
	dismissNode.Payload["dismissed"] = true
	if err := db.UpdateNodePayload(dismissedSnapID, dismissNode.Payload); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	held = findHeldSnapshots(t, db)
	if containsHex(held, dismissedSnapID) {
		t.Fatal("dismissed snapshot must be filtered out of gate list")
	}
	if !containsHex(held, heldSnapID) {
		t.Fatal("non-dismissed held snapshot must still be in gate list")
	}

	// 5. Approve path: publish the held snapshot with SkipGate=true,
	//    same call kai gate approve makes. snap.latest must advance.
	approvedReport, err := mgr.PublishAtTarget(
		wsObj,
		&IntegrateResult{ResultSnapshot: heldSnapID},
		baseSnap,
		PublishOptions{SkipGate: true},
	)
	if err != nil {
		t.Fatalf("approve publish: %v", err)
	}
	if approvedReport.HeldByGate {
		t.Fatal("SkipGate must override the hold")
	}
	if len(approvedReport.AdvancedRefs) == 0 {
		t.Fatal("expected at least one ref to advance on approve")
	}

	// snap.latest now points at the previously-held snapshot.
	r, _ = refMgr.Get("snap.latest")
	if util.BytesToHex(r.TargetID) != util.BytesToHex(heldSnapID) {
		t.Fatalf("snap.latest did not advance to held snapshot after approve")
	}
}

// findHeldSnapshots replicates the filter cmd/kai/gate.go uses, kept
// in the test file rather than imported because gate.go lives in
// package main.
func findHeldSnapshots(t *testing.T, db *graph.DB) [][]byte {
	t.Helper()
	all, err := db.GetNodesByKind(graph.KindSnapshot)
	if err != nil {
		t.Fatalf("scan snapshots: %v", err)
	}
	var out [][]byte
	for _, n := range all {
		if n == nil || n.Payload == nil {
			continue
		}
		v, _ := n.Payload["gateVerdict"].(string)
		if v != "review" && v != "block" {
			continue
		}
		if dismissed, _ := n.Payload["dismissed"].(bool); dismissed {
			continue
		}
		out = append(out, n.ID)
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i]) < string(out[j]) })
	return out
}

func containsHex(ids [][]byte, target []byte) bool {
	tgt := util.BytesToHex(target)
	for _, id := range ids {
		if util.BytesToHex(id) == tgt {
			return true
		}
	}
	return false
}

// makeAnotherHeldSnapshot spins up a second workspace+integrate so the
// dismissal test has a distinct held snapshot to operate on without
// affecting the primary one under test.
func makeAnotherHeldSnapshot(t *testing.T, db *graph.DB, mgr *Manager, baseSnap []byte, wsName, filePath string, cfg safetygate.Config) []byte {
	t.Helper()
	w, err := mgr.Create(wsName, baseSnap, "second held")
	if err != nil {
		t.Fatalf("create %s: %v", wsName, err)
	}
	head := insertSnapshot(t, db, wsName+"-head", map[string]string{filePath: "v2"})
	if err := mgr.UpdateHead(w.ID, head); err != nil {
		t.Fatalf("update head %s: %v", wsName, err)
	}
	cs := insertChangeSet(t, db, baseSnap, head)
	if err := mgr.AddChangeSet(w.ID, cs); err != nil {
		t.Fatalf("add changeset %s: %v", wsName, err)
	}
	res, err := mgr.IntegrateWithOptions(wsName, baseSnap, IntegrateOptions{GateConfig: &cfg})
	if err != nil {
		t.Fatalf("integrate %s: %v", wsName, err)
	}
	if res.Decision == nil || res.Decision.Verdict != "block" {
		t.Fatalf("expected Block for %s, got %+v", wsName, res.Decision)
	}
	return res.ResultSnapshot
}
