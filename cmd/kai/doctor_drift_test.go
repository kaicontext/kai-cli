package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/drift"
)

func driftHealthOutput(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	checkDriftHealth(&buf, "OK", "WARN", "BAD")
	return buf.String()
}

func TestCheckDriftHealthSynced(t *testing.T) {
	c1 := setupCatchupRepo(t)
	if err := drift.Pin(kaiDir, "refs/heads/main", c1, time.Now()); err != nil {
		t.Fatal(err)
	}
	syncDriftManifest()

	out := driftHealthOutput(t)
	if !strings.Contains(out, "OK graph drift: in sync") {
		t.Errorf("synced output:\n%s", out)
	}
	if !strings.Contains(out, "OK drift manifest:") {
		t.Errorf("manifest line missing:\n%s", out)
	}
}

func TestCheckDriftHealthBehindWithStaleManifest(t *testing.T) {
	c1 := setupCatchupRepo(t)
	repo, _ := os.Getwd()
	if err := drift.Pin(kaiDir, "refs/heads/main", c1, time.Now()); err != nil {
		t.Fatal(err)
	}
	syncDriftManifest() // manifest keyed to the synced state
	addCommit(t, repo, "x.go", "package cu\n", "d1")

	out := driftHealthOutput(t)
	if !strings.Contains(out, "WARN graph drift: 1 commit behind") {
		t.Errorf("behind line missing:\n%s", out)
	}
	if !strings.Contains(out, "WARN drift manifest: stale") {
		t.Errorf("stale manifest not flagged:\n%s", out)
	}

	// --fix resyncs the manifest.
	oldFix := doctorFix
	doctorFix = true
	defer func() { doctorFix = oldFix }()
	out = driftHealthOutput(t)
	if !strings.Contains(out, "resynced") {
		t.Errorf("--fix did not resync:\n%s", out)
	}
	man, err := drift.LoadManifest(kaiDir)
	if err != nil || len(man.Commits) != 1 {
		t.Errorf("manifest after fix: %+v, %v", man, err)
	}
}

func TestCheckDriftHealthCorruptManifest(t *testing.T) {
	c1 := setupCatchupRepo(t)
	if err := drift.Pin(kaiDir, "refs/heads/main", c1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(kaiDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kaiDir, drift.ManifestFile), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	out := driftHealthOutput(t)
	if !strings.Contains(out, "BAD drift manifest: corrupt") {
		t.Errorf("corrupt manifest not flagged:\n%s", out)
	}

	oldFix := doctorFix
	doctorFix = true
	defer func() { doctorFix = oldFix }()
	out = driftHealthOutput(t)
	if !strings.Contains(out, "rebuilt") {
		t.Errorf("--fix did not rebuild:\n%s", out)
	}
	if _, err := drift.LoadManifest(kaiDir); err != nil {
		t.Errorf("manifest still corrupt after fix: %v", err)
	}
}

func TestCheckDriftHealthCorruptRefs(t *testing.T) {
	setupCatchupRepo(t)
	if err := os.MkdirAll(kaiDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kaiDir, drift.RefsFile), []byte("]["), 0644); err != nil {
		t.Fatal(err)
	}
	out := driftHealthOutput(t)
	if !strings.Contains(out, "BAD graph refs") {
		t.Errorf("corrupt refs not flagged:\n%s", out)
	}
}

func TestHookUninstallRemovesAllKaiHooks(t *testing.T) {
	setupCatchupRepo(t)
	hooks := filepath.Join(".git", "hooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}
	// Two kai-managed hooks, one foreign.
	kaiHook := "#!/bin/sh\n" + kaiHookMarker + " " + kaiHookVersion + "\nexit 0\n"
	for _, name := range []string{"post-commit", "post-rewrite"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(kaiHook), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\n# user hook\n"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := runHookUninstall(nil, nil); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	for _, name := range []string{"post-commit", "post-rewrite"} {
		if _, err := os.Stat(filepath.Join(hooks, name)); !os.IsNotExist(err) {
			t.Errorf("%s hook not removed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(hooks, "pre-push")); err != nil {
		t.Errorf("foreign pre-push hook was touched: %v", err)
	}
}
