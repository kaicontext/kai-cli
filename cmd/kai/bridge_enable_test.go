package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bridge used to be reachable only at `kai init --git-bridge` time,
// so an already-initialized repo could never turn it on. Enable has to
// work at any point in a repo's life, and "enabled" has to mean the
// hooks are actually there — a sentinel with no post-commit hook is a
// bridge that silently does nothing.
func TestBridgeEnableInstallsHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	run := exec.Command("git", "init", "-q", "-b", "main")
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// A kai project that exists but was never bridged — the case the
	// enable command is for.
	if err := os.MkdirAll(filepath.Join(dir, ".kai"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldKaiDir := kaiDir
	kaiDir = ".kai"
	t.Cleanup(func() { kaiDir = oldKaiDir })

	if bridgeEnabled() {
		t.Fatal("bridge reported enabled before it was")
	}
	oldInit := initMode
	initMode = true // silence the install chatter
	t.Cleanup(func() { initMode = oldInit })

	if err := runBridgeEnable(bridgeEnableCmd, nil); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !bridgeEnabled() {
		t.Fatal("bridge still reports disabled after enable")
	}
	for _, name := range []string{"post-commit", "post-merge", "post-checkout", "post-rewrite"} {
		data, err := os.ReadFile(filepath.Join(dir, ".git", "hooks", name))
		if err != nil {
			t.Fatalf("%s hook not installed: %v", name, err)
		}
		if !strings.Contains(string(data), kaiHookMarker) {
			t.Fatalf("%s hook is not kai-managed", name)
		}
	}

	// Re-running is safe — this is the shape of `kai bridge enable` on a
	// repo someone already bridged.
	if err := runBridgeEnable(bridgeEnableCmd, nil); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	// Disable drops the milestone direction and leaves the hooks, so the
	// import direction keeps the graph from drifting.
	if err := bridgeDisableCmd.RunE(bridgeDisableCmd, nil); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if bridgeEnabled() {
		t.Fatal("bridge still enabled after disable")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "post-commit")); err != nil {
		t.Fatalf("disable removed the import hook: %v", err)
	}
}

// Without git there is nothing to bridge TO, and the failure has to say
// so rather than writing a sentinel that can never do anything.
func TestBridgeEnableRequiresGit(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	if err := runBridgeEnable(bridgeEnableCmd, nil); err == nil ||
		!strings.Contains(err.Error(), "git repository") {
		t.Fatalf("enable outside git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kai", "bridge-enabled")); err == nil {
		t.Fatal("a sentinel was written for a repo the bridge cannot work in")
	}
}
