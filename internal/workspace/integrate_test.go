package workspace

import (
	"bytes"
	"testing"

	"kai/internal/util"
)

// --- Integrate: fast-forward ---

func TestIntegrateFastForward(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "test workspace")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Simulate a staged change: new head with modified file
	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// Integrate into the same base (fast-forward)
	result, err := mgr.Integrate("feat", baseSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}
	// FF now produces a fresh tagged snapshot (not ws.HeadSnapshot
	// directly) so every integration carries gate metadata. The
	// result's HAS_FILE edges point at the same file nodes as
	// ws.HeadSnapshot, but the snapshot id itself is new.
	if len(result.ResultSnapshot) == 0 {
		t.Fatalf("fast-forward should produce a result snapshot")
	}
	if bytes.Equal(result.ResultSnapshot, headSnap) {
		t.Fatalf("fast-forward should mint a new snapshot id, not reuse ws head")
	}
	if result.Decision == nil {
		t.Fatalf("fast-forward should populate a gate Decision")
	}
	if len(result.AppliedChangeSets) != 1 {
		t.Fatalf("expected 1 applied changeset, got %d", len(result.AppliedChangeSets))
	}
}

// --- Integrate: diverged, no overlap ---

func TestIntegrateDivergedNoConflict(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)

	// Base has a.txt and b.txt
	baseSnap := insertSnapshot(t, db, "base", map[string]string{
		"a.txt": "v1",
		"b.txt": "v1",
	})

	ws, err := mgr.Create("feat", baseSnap, "diverged test")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Workspace modifies a.txt
	headSnap := insertSnapshot(t, db, "head", map[string]string{
		"a.txt": "v2",
		"b.txt": "v1",
	})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// Target modifies b.txt (no overlap with workspace changes)
	targetSnap := insertSnapshot(t, db, "target", map[string]string{
		"a.txt": "v1",
		"b.txt": "v3",
	})

	result, err := mgr.Integrate("feat", targetSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}
	if result.ResultSnapshot == nil {
		t.Fatal("expected result snapshot")
	}

	// Verify merged file set
	files, err := mgr.getSnapshotFileMap(result.ResultSnapshot)
	if err != nil {
		t.Fatalf("result files: %v", err)
	}
	if files["a.txt"] != "v2" {
		t.Errorf("a.txt: expected v2 (workspace change), got %q", files["a.txt"])
	}
	if files["b.txt"] != "v3" {
		t.Errorf("b.txt: expected v3 (target change), got %q", files["b.txt"])
	}
}

// --- Integrate: diverged, overlapping files -> conflict ---

func TestIntegrateDivergedConflict(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "conflict test")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Workspace modifies a.txt to v2
	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// Target also modifies a.txt to v3
	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v3"})

	result, err := mgr.Integrate("feat", targetSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatal("expected conflicts")
	}
	if result.ResultSnapshot != nil {
		t.Fatal("expected no result snapshot on conflict")
	}

	// Verify conflict path
	found := false
	for _, c := range result.Conflicts {
		if c.Path == "a.txt" {
			found = true
			if c.BaseDigest != "v1" || c.HeadDigest != "v2" || c.NewDigest != "v3" {
				t.Errorf("unexpected conflict digests: base=%q head=%q new=%q", c.BaseDigest, c.HeadDigest, c.NewDigest)
			}
		}
	}
	if !found {
		t.Error("expected conflict on a.txt")
	}
}

// --- Integrate: workspace with no changes ---

func TestIntegrateNoChanges(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Create("empty", baseSnap, "no changes")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// No changes staged — workspace has no open changesets
	_, err = mgr.Integrate("empty", baseSnap)
	if err == nil {
		t.Fatal("expected error for workspace with no changes")
	}
}

// --- Integrate: closed workspace ---

func TestIntegrateClosedWorkspace(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Create("closed", baseSnap, "will close")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := mgr.Close("closed"); err != nil {
		t.Fatalf("close workspace: %v", err)
	}

	_, err = mgr.Integrate("closed", baseSnap)
	if err == nil {
		t.Fatal("expected error for closed workspace")
	}
}

// --- Integrate: workspace not found ---

func TestIntegrateNotFound(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Integrate("nonexistent", baseSnap)
	if err == nil {
		t.Fatal("expected error for nonexistent workspace")
	}
}

// --- Integrate: file added in workspace, target unchanged ---

func TestIntegrateNewFileInWorkspace(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "new file")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Workspace adds b.txt
	headSnap := insertSnapshot(t, db, "head", map[string]string{
		"a.txt": "v1",
		"b.txt": "new",
	})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// Target is the same as base (fast-forward)
	result, err := mgr.Integrate("feat", baseSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}

	files, err := mgr.getSnapshotFileMap(result.ResultSnapshot)
	if err != nil {
		t.Fatalf("result files: %v", err)
	}
	if files["b.txt"] != "new" {
		t.Errorf("b.txt: expected 'new', got %q", files["b.txt"])
	}
}

// --- Integrate: file deleted in workspace ---

func TestIntegrateFileDeletedInWorkspace(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{
		"a.txt": "v1",
		"b.txt": "v1",
	})

	ws, err := mgr.Create("feat", baseSnap, "delete file")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Workspace deletes b.txt
	headSnap := insertSnapshot(t, db, "head", map[string]string{
		"a.txt": "v1",
	})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	result, err := mgr.Integrate("feat", baseSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}

	files, err := mgr.getSnapshotFileMap(result.ResultSnapshot)
	if err != nil {
		t.Fatalf("result files: %v", err)
	}
	if _, exists := files["b.txt"]; exists {
		t.Error("b.txt should be deleted in result")
	}
	if files["a.txt"] != "v1" {
		t.Errorf("a.txt should be unchanged, got %q", files["a.txt"])
	}
}

// --- Conflict state persistence ---

func TestConflictStateSaveGetClear(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})
	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v3"})

	ws, err := mgr.Create("feat", baseSnap, "conflict state test")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	conflicts := []Conflict{
		{
			Path:        "a.txt",
			Description: "both sides modified",
			BaseDigest:  "v1",
			HeadDigest:  "v2",
			NewDigest:   "v3",
		},
	}

	// Save
	if err := mgr.SaveConflictState(ws.ID, targetSnap, conflicts); err != nil {
		t.Fatalf("save conflict state: %v", err)
	}

	// Get
	state, err := mgr.GetConflictState("feat")
	if err != nil {
		t.Fatalf("get conflict state: %v", err)
	}
	if state == nil {
		t.Fatal("expected conflict state")
	}
	if state.TargetSnapshot != util.BytesToHex(targetSnap) {
		t.Errorf("target snapshot: expected %s, got %s", util.BytesToHex(targetSnap), state.TargetSnapshot)
	}
	if len(state.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(state.Conflicts))
	}
	if state.Conflicts[0].Path != "a.txt" {
		t.Errorf("conflict path: expected a.txt, got %s", state.Conflicts[0].Path)
	}
	if state.Conflicts[0].BaseDigest != "v1" {
		t.Errorf("conflict base digest: expected v1, got %s", state.Conflicts[0].BaseDigest)
	}

	// Clear
	if err := mgr.ClearConflictState("feat"); err != nil {
		t.Fatalf("clear conflict state: %v", err)
	}

	state, err = mgr.GetConflictState("feat")
	if err != nil {
		t.Fatalf("get conflict state after clear: %v", err)
	}
	if state != nil {
		t.Fatal("expected nil conflict state after clear")
	}
}

func TestGetConflictStateNoConflicts(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Create("feat", baseSnap, "no conflicts")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// No conflicts saved — should return nil
	state, err := mgr.GetConflictState("feat")
	if err != nil {
		t.Fatalf("get conflict state: %v", err)
	}
	if state != nil {
		t.Fatal("expected nil conflict state for workspace with no conflicts")
	}
}

func TestGetConflictStateWorkspaceNotFound(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	_, err := mgr.GetConflictState("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent workspace")
	}
}

// --- Integrate saves conflict state automatically ---

func TestIntegrateSavesConflictState(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "auto-save conflicts")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v3"})

	// Integrate should fail with conflicts
	result, err := mgr.Integrate("feat", targetSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatal("expected conflicts")
	}

	// Conflict state should be saved automatically
	state, err := mgr.GetConflictState("feat")
	if err != nil {
		t.Fatalf("get conflict state: %v", err)
	}
	if state == nil {
		t.Fatal("expected conflict state to be saved automatically")
	}
	if len(state.Conflicts) != len(result.Conflicts) {
		t.Errorf("saved conflicts: expected %d, got %d", len(result.Conflicts), len(state.Conflicts))
	}
	if state.TargetSnapshot != util.BytesToHex(targetSnap) {
		t.Error("saved target snapshot doesn't match")
	}
}

// --- IntegrateWithResolutions ---

func TestIntegrateWithResolutionsAllResolved(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "resolve test")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v3"})

	// Provide resolution for a.txt
	resolutions := map[string][]byte{
		"a.txt": []byte("resolved content"),
	}

	result, err := mgr.IntegrateWithResolutions("feat", targetSnap, resolutions)
	if err != nil {
		t.Fatalf("integrate with resolutions: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts after resolution, got %d", len(result.Conflicts))
	}
	if result.ResultSnapshot == nil {
		t.Fatal("expected result snapshot")
	}
	if result.AutoResolved < 1 {
		t.Errorf("expected at least 1 auto-resolved, got %d", result.AutoResolved)
	}
}

func TestIntegrateWithResolutionsPartial(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{
		"a.txt": "v1",
		"b.txt": "v1",
	})

	ws, err := mgr.Create("feat", baseSnap, "partial resolve")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Workspace modifies both files
	headSnap := insertSnapshot(t, db, "head", map[string]string{
		"a.txt": "v2",
		"b.txt": "v2",
	})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// Target also modifies both files
	targetSnap := insertSnapshot(t, db, "target", map[string]string{
		"a.txt": "v3",
		"b.txt": "v3",
	})

	// Only resolve a.txt, leave b.txt unresolved
	resolutions := map[string][]byte{
		"a.txt": []byte("resolved a"),
	}

	result, err := mgr.IntegrateWithResolutions("feat", targetSnap, resolutions)
	if err != nil {
		t.Fatalf("integrate with partial resolutions: %v", err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatal("expected remaining conflicts for b.txt")
	}

	// b.txt should still be conflicted
	found := false
	for _, c := range result.Conflicts {
		if c.Path == "b.txt" {
			found = true
		}
		if c.Path == "a.txt" {
			t.Error("a.txt should be resolved, not in conflicts")
		}
	}
	if !found {
		t.Error("expected b.txt to remain in conflicts")
	}
}

func TestIntegrateWithResolutionsNilMap(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "nil resolutions")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	targetSnap := insertSnapshot(t, db, "target", map[string]string{"a.txt": "v3"})

	// IntegrateWithResolutions with nil map should behave like Integrate
	result, err := mgr.IntegrateWithResolutions("feat", targetSnap, nil)
	if err != nil {
		t.Fatalf("integrate with nil resolutions: %v", err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatal("expected conflicts when no resolutions provided")
	}
}

// --- Full workspace lifecycle ---

func TestWorkspaceLifecycle(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)

	// 1. Create base snapshot (the "main" state)
	baseSnap := insertSnapshot(t, db, "main", map[string]string{
		"main.go":   "package main",
		"README.md": "# Project",
	})

	// 2. Create workspace
	ws, err := mgr.Create("feature/auth", baseSnap, "Add authentication")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if ws.Name != "feature/auth" {
		t.Errorf("workspace name: expected feature/auth, got %s", ws.Name)
	}
	if ws.Status != StatusActive {
		t.Errorf("workspace status: expected active, got %s", ws.Status)
	}
	if !bytes.Equal(ws.BaseSnapshot, baseSnap) {
		t.Error("workspace base should equal base snapshot")
	}
	if !bytes.Equal(ws.HeadSnapshot, baseSnap) {
		t.Error("workspace head should start at base snapshot")
	}

	// 3. Stage changes (simulate — create new snapshot and update head)
	headSnap := insertSnapshot(t, db, "staged", map[string]string{
		"main.go":   "package main\nfunc auth() {}",
		"README.md": "# Project",
		"auth.go":   "package auth",
	})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// 4. Verify workspace state
	ws, err = mgr.Get("feature/auth")
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if !bytes.Equal(ws.HeadSnapshot, headSnap) {
		t.Error("head should be updated after staging")
	}
	if len(ws.OpenChangeSets) != 1 {
		t.Errorf("expected 1 changeset, got %d", len(ws.OpenChangeSets))
	}

	// 5. Integrate into base (fast-forward since base hasn't moved)
	result, err := mgr.Integrate("feature/auth", baseSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}

	// 6. Verify result snapshot has all files
	files, err := mgr.getSnapshotFileMap(result.ResultSnapshot)
	if err != nil {
		t.Fatalf("result files: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("expected 3 files in result, got %d", len(files))
	}
	if _, ok := files["auth.go"]; !ok {
		t.Error("auth.go should be in result")
	}

	// 7. Close workspace
	if err := mgr.Close("feature/auth"); err != nil {
		t.Fatalf("close workspace: %v", err)
	}
	ws, err = mgr.Get("feature/auth")
	if err != nil {
		t.Fatalf("get workspace after close: %v", err)
	}
	if ws.Status != StatusClosed {
		t.Errorf("workspace status after close: expected closed, got %s", ws.Status)
	}
}

// --- Full conflict resolution lifecycle ---

func TestConflictResolutionLifecycle(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)

	// 1. Create base
	baseSnap := insertSnapshot(t, db, "main", map[string]string{
		"config.json": "v1",
		"app.go":      "v1",
	})

	// 2. Create workspace and make changes
	ws, err := mgr.Create("feat", baseSnap, "feature work")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	headSnap := insertSnapshot(t, db, "head", map[string]string{
		"config.json": "v2-ws",
		"app.go":      "v2-ws",
	})
	if err := mgr.UpdateHead(ws.ID, headSnap); err != nil {
		t.Fatalf("update head: %v", err)
	}
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	// 3. Meanwhile, target has diverged (both files modified)
	targetSnap := insertSnapshot(t, db, "target", map[string]string{
		"config.json": "v2-target",
		"app.go":      "v2-target",
	})

	// 4. First integrate attempt — conflicts
	result, err := mgr.Integrate("feat", targetSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatal("expected conflicts")
	}

	// 5. Verify conflict state was saved
	state, err := mgr.GetConflictState("feat")
	if err != nil {
		t.Fatalf("get conflict state: %v", err)
	}
	if state == nil {
		t.Fatal("expected saved conflict state")
	}
	if len(state.Conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(state.Conflicts))
	}

	// 6. Resolve both files
	resolutions := map[string][]byte{
		"config.json": []byte("merged config"),
		"app.go":      []byte("merged app"),
	}

	targetID, err := util.HexToBytes(state.TargetSnapshot)
	if err != nil {
		t.Fatalf("parse target snapshot: %v", err)
	}

	result, err = mgr.IntegrateWithResolutions("feat", targetID, resolutions)
	if err != nil {
		t.Fatalf("integrate with resolutions: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no remaining conflicts, got %d", len(result.Conflicts))
	}
	if result.ResultSnapshot == nil {
		t.Fatal("expected result snapshot after resolution")
	}

	// 7. Clear conflict state
	if err := mgr.ClearConflictState("feat"); err != nil {
		t.Fatalf("clear conflict state: %v", err)
	}
	state, err = mgr.GetConflictState("feat")
	if err != nil {
		t.Fatalf("get conflict state after clear: %v", err)
	}
	if state != nil {
		t.Fatal("conflict state should be cleared")
	}
}

// --- Workspace CRUD operations ---

func TestWorkspaceCreateDuplicate(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Create("feat", baseSnap, "first")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = mgr.Create("feat", baseSnap, "duplicate")
	if err == nil {
		t.Fatal("expected error for duplicate workspace name")
	}
}

func TestWorkspaceList(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Create("ws1", baseSnap, "first")
	if err != nil {
		t.Fatalf("create ws1: %v", err)
	}
	_, err = mgr.Create("ws2", baseSnap, "second")
	if err != nil {
		t.Fatalf("create ws2: %v", err)
	}

	workspaces, err := mgr.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(workspaces))
	}
}

func TestWorkspaceShelveUnshelve(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Create("feat", baseSnap, "shelve test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Shelve
	if err := mgr.Shelve("feat"); err != nil {
		t.Fatalf("shelve: %v", err)
	}
	ws, _ := mgr.Get("feat")
	if ws.Status != StatusShelved {
		t.Errorf("expected shelved, got %s", ws.Status)
	}

	// Can't shelve again
	if err := mgr.Shelve("feat"); err == nil {
		t.Fatal("expected error shelving already-shelved workspace")
	}

	// Unshelve
	if err := mgr.Unshelve("feat"); err != nil {
		t.Fatalf("unshelve: %v", err)
	}
	ws, _ = mgr.Get("feat")
	if ws.Status != StatusActive {
		t.Errorf("expected active after unshelve, got %s", ws.Status)
	}
}

func TestWorkspaceDelete(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	_, err := mgr.Create("feat", baseSnap, "delete test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := mgr.Delete("feat", false); err != nil {
		t.Fatalf("delete: %v", err)
	}

	ws, err := mgr.Get("feat")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if ws != nil {
		t.Fatal("workspace should be nil after delete")
	}
}

func TestWorkspaceGetLog(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "log test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	headSnap := insertSnapshot(t, db, "head", map[string]string{"a.txt": "v2"})
	csID := insertChangeSet(t, db, baseSnap, headSnap)
	if err := mgr.AddChangeSet(ws.ID, csID); err != nil {
		t.Fatalf("add changeset: %v", err)
	}

	log, err := mgr.GetLog("feat")
	if err != nil {
		t.Fatalf("get log: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(log))
	}
}

// --- Multiple changesets ---

func TestIntegrateMultipleChangeSets(t *testing.T) {
	db, cleanup := setupWorkspaceTestDB(t)
	defer cleanup()

	mgr := NewManager(db)
	baseSnap := insertSnapshot(t, db, "base", map[string]string{"a.txt": "v1"})

	ws, err := mgr.Create("feat", baseSnap, "multi-changeset")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// First changeset: modify a.txt
	snap1 := insertSnapshot(t, db, "snap1", map[string]string{"a.txt": "v2"})
	if err := mgr.UpdateHead(ws.ID, snap1); err != nil {
		t.Fatalf("update head 1: %v", err)
	}
	cs1 := insertChangeSet(t, db, baseSnap, snap1)
	if err := mgr.AddChangeSet(ws.ID, cs1); err != nil {
		t.Fatalf("add changeset 1: %v", err)
	}

	// Second changeset: add b.txt
	snap2 := insertSnapshot(t, db, "snap2", map[string]string{
		"a.txt": "v2",
		"b.txt": "new",
	})
	if err := mgr.UpdateHead(ws.ID, snap2); err != nil {
		t.Fatalf("update head 2: %v", err)
	}
	cs2 := insertChangeSet(t, db, snap1, snap2)
	if err := mgr.AddChangeSet(ws.ID, cs2); err != nil {
		t.Fatalf("add changeset 2: %v", err)
	}

	// Integrate — fast-forward since base hasn't moved
	result, err := mgr.Integrate("feat", baseSnap)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(result.Conflicts))
	}
	if len(result.AppliedChangeSets) != 2 {
		t.Errorf("expected 2 applied changesets, got %d", len(result.AppliedChangeSets))
	}

	files, err := mgr.getSnapshotFileMap(result.ResultSnapshot)
	if err != nil {
		t.Fatalf("result files: %v", err)
	}
	if files["a.txt"] != "v2" {
		t.Errorf("a.txt: expected v2, got %q", files["a.txt"])
	}
	if files["b.txt"] != "new" {
		t.Errorf("b.txt: expected 'new', got %q", files["b.txt"])
	}
}
