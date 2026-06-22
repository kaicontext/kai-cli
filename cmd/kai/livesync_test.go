package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSetupLiveSync_DisabledReturnsNil verifies the most common path:
// when `kai live on` hasn't been run (no sync-state.json), the helper
// returns (nil, nil) so the TUI cleanly skips live sync rather than
// blocking on a missing-file error.
func TestSetupLiveSync_DisabledReturnsNil(t *testing.T) {
	dir := t.TempDir()
	w, err := setupLiveSync(dir)
	if err != nil {
		t.Errorf("expected no error for missing state, got %v", err)
	}
	if w != nil {
		t.Errorf("expected nil wiring, got %+v", w)
	}
}

// TestSetupLiveSync_FileExistsButDisabled covers the case where
// `kai live off` was run — file exists with Enabled=false. Same
// behavior as missing file.
func TestSetupLiveSync_FileExistsButDisabled(t *testing.T) {
	dir := t.TempDir()
	data, _ := json.Marshal(liveSyncState{Enabled: false})
	if err := os.WriteFile(filepath.Join(dir, "sync-state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := setupLiveSync(dir)
	if err != nil || w != nil {
		t.Errorf("expected (nil, nil) for disabled state, got %v / %+v", err, w)
	}
}

// TestSetupLiveSync_MalformedFileTreatedAsDisabled — a corrupt state
// file shouldn't surface as an error to the user. Treat it as
// "live sync is off" and move on.
func TestSetupLiveSync_MalformedFileTreatedAsDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sync-state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := setupLiveSync(dir)
	if err != nil || w != nil {
		t.Errorf("expected (nil, nil) for malformed state, got %v / %+v", err, w)
	}
}

// TestOrchLiveSync_NilWiring confirms the adapter returns nil so the
// orchestrator's nil-check at the hook site routes file writes only
// to the local OnFileChange callback.
func TestOrchLiveSync_NilWiring(t *testing.T) {
	if got := orchLiveSync(nil); got != nil {
		t.Errorf("expected nil func for nil wiring, got non-nil")
	}
}

// TestOrchLiveSync_PassesThrough confirms that when wiring is set,
// orchLiveSync hands back its Broadcast field unchanged.
func TestOrchLiveSync_PassesThrough(t *testing.T) {
	calls := 0
	w := &liveSyncWiring{
		Broadcast: func(_, _, _ string) { calls++ },
		Stop:      func() {},
	}
	fn := orchLiveSync(w)
	if fn == nil {
		t.Fatal("expected non-nil func")
	}
	fn("a", "b", "c")
	if calls != 1 {
		t.Errorf("Broadcast not invoked: calls=%d", calls)
	}
}
