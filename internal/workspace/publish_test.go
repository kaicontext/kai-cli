package workspace

import (
	"testing"

	"kai/internal/ref"
	"kai/internal/safetygate"
)

// TestPublishHonorsAutoVerdict verifies that an Auto verdict (the default
// when blast radius == 0) advances the named target ref. This is the
// happy path: agent's change is isolated, gate says Auto, ref moves.
func TestPublishHonorsAutoVerdict(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "auto verdict test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// Set a named ref pointing at base so PublishToRef has something to advance.
	refMgr := ref.NewRefManager(db)
	if err := refMgr.Set("snap.latest", baseSnap, ref.KindSnapshot); err != nil {
		t.Fatalf("set ref: %v", err)
	}

	result, err := mgr.Integrate("feat", baseSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if result.Decision == nil || result.Decision.Verdict != "auto" {
		t.Fatalf("expected Auto verdict, got %+v", result.Decision)
	}

	wsObj, _ := mgr.Get("feat")
	report, err := mgr.PublishToRef(wsObj, result, "snap.latest", PublishOptions{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if report.HeldByGate {
		t.Fatal("Auto verdict must not hold")
	}
	if len(report.AdvancedRefs) != 1 || report.AdvancedRefs[0] != "snap.latest" {
		t.Fatalf("expected snap.latest advanced, got %v", report.AdvancedRefs)
	}
}

// TestPublishBlocksOnProtectedPath verifies that touching a protected
// path produces a Block verdict and PublishToRef refuses to advance
// the named ref. The merged snapshot still exists in the DB (refuse-
// to-promote), but the team-visible ref doesn't move.
func TestPublishBlocksOnProtectedPath(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"pkg/auth/login.go": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "protected path test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	headSnap := insertSnapshot(t, db, "head", map[string]string{"pkg/auth/login.go": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	refMgr := ref.NewRefManager(db)
	if err := refMgr.Set("snap.latest", baseSnap, ref.KindSnapshot); err != nil {
		t.Fatalf("set ref: %v", err)
	}

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
	// Result snapshot must still exist — refuse-to-promote, not roll-back.
	if len(result.ResultSnapshot) == 0 {
		t.Fatal("blocked integration must still produce a snapshot for review")
	}

	wsObj, _ := mgr.Get("feat")
	report, err := mgr.PublishToRef(wsObj, result, "snap.latest", PublishOptions{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !report.HeldByGate {
		t.Fatal("Block verdict must hold the publish")
	}
	if len(report.AdvancedRefs) != 0 {
		t.Fatalf("Block verdict must not advance any team-visible ref, got %v", report.AdvancedRefs)
	}

	// snap.latest must still point at baseSnap, not the new merged snapshot.
	r, err := refMgr.Get("snap.latest")
	if err != nil {
		t.Fatalf("get ref: %v", err)
	}
	if string(r.TargetID) != string(baseSnap) {
		t.Fatalf("snap.latest moved despite Block verdict")
	}
}

// TestPublishSkipGateBypassesBlock verifies that the human-approval
// path (SkipGate) advances the ref even when the underlying integration
// would have blocked. This is what `kai review approve` will use.
func TestPublishSkipGateBypassesBlock(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"pkg/auth/login.go": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "skip gate test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	headSnap := insertSnapshot(t, db, "head", map[string]string{"pkg/auth/login.go": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	refMgr := ref.NewRefManager(db)
	if err := refMgr.Set("snap.latest", baseSnap, ref.KindSnapshot); err != nil {
		t.Fatalf("set ref: %v", err)
	}

	cfg := safetygate.Config{Protected: []string{"pkg/auth/**"}, BlockThreshold: 1000}
	// First integrate produces Block.
	result, err := mgr.IntegrateWithOptions("feat", baseSnap, IntegrateOptions{GateConfig: &cfg})
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if result.Decision.Verdict != "block" {
		t.Fatalf("expected Block, got %s", result.Decision.Verdict)
	}

	// Human-approval path: re-publish with SkipGate. The decision on
	// the result is still Block, but PublishOptions.SkipGate forces
	// the advance.
	wsObj, _ := mgr.Get("feat")
	report, err := mgr.PublishToRef(wsObj, result, "snap.latest", PublishOptions{SkipGate: true})
	if err != nil {
		t.Fatalf("publish skip-gate: %v", err)
	}
	if report.HeldByGate {
		t.Fatal("SkipGate must override the held verdict")
	}
	if len(report.AdvancedRefs) != 1 {
		t.Fatalf("expected ref advance, got %v", report.AdvancedRefs)
	}
}
