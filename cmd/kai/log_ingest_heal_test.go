package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsTempPath(t *testing.T) {
	cases := map[string]bool{
		"/tmp/claude-501/x/scratchpad/kai":         true,
		"/private/tmp/claude-501/y/scratchpad/kai": true,
		"/var/folders/ab/cd/T/kai":                 true,
		"/Users/jacobschatz/.kai/bin/kai":          false,
		"/usr/local/bin/kai":                       false,
	}
	for p, want := range cases {
		if got := isTempPath(p); got != want {
			t.Errorf("isTempPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// reconcileHookEvents must prune a stale ingest entry (dead temp path), install
// the current one, and never touch an unrelated hook in the same event array.
func TestReconcileHookEventsPrunesStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.local.json")

	stale := `"/tmp/claude-501/dead/scratchpad/kai" log ingest`
	unrelated := "some-other-tool --notify"
	seed := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": stale, "timeout": 30},
					map[string]any{"type": "command", "command": unrelated, "timeout": 5},
				}},
			},
		},
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	want := `"/Users/jacobschatz/.kai/bin/kai" log ingest`
	reconcileHookEvents(path, []string{"Stop", "SessionEnd"}, want)

	if got := ingestHookCommands(path, []string{"Stop"}); len(got) != 1 || got[0] != want {
		t.Fatalf("Stop ingest hooks after reconcile = %v, want exactly [%q]", got, want)
	}
	// SessionEnd had no entry — reconcile should have added the current one.
	if se := ingestHookCommands(path, []string{"SessionEnd"}); len(se) != 1 || se[0] != want {
		t.Fatalf("SessionEnd ingest = %v, want [%q]", se, want)
	}
	// The unrelated hook must survive.
	raw, _ := os.ReadFile(path)
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	found := false
	for _, g := range root["hooks"].(map[string]any)["Stop"].([]any) {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			if h.(map[string]any)["command"] == unrelated {
				found = true
			}
		}
	}
	if !found {
		t.Error("unrelated hook was dropped by reconcile")
	}
}

// A second reconcile with the same desired command must be a no-op (idempotent):
// mtime should not change on the second call.
func TestReconcileHookEventsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	want := `"/Users/jacobschatz/.kai/bin/kai" log ingest`

	reconcileHookEvents(path, []string{"Stop"}, want)
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reconcileHookEvents(path, []string{"Stop"}, want)
	fi2, _ := os.Stat(path)
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Error("reconcile rewrote an already-correct file (not idempotent)")
	}
}
