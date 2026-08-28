package main

import (
	"os"
	"path/filepath"
	"testing"

	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// Durable session spawns must not share mutable dependency dirs with
// the source checkout: an agent's install through a symlink would
// mutate the user's repo and every other session. Ephemeral spawns
// keep the near-free symlink.
func TestProvisionDeps(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "node_modules", "leftpad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "node_modules", "leftpad", "index.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ephemeral: symlink.
	eph := t.TempDir()
	provisionDeps(src, eph, false, spawnpkg.Resolved("full"))
	if fi, err := os.Lstat(filepath.Join(eph, "node_modules")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("ephemeral deps should be a symlink (err=%v)", err)
	}

	// Durable: a real, independent copy.
	dur := t.TempDir()
	provisionDeps(src, dur, true, spawnpkg.Resolved("full"))
	fi, err := os.Lstat(filepath.Join(dur, "node_modules"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Fatalf("durable deps should be a real dir (err=%v, mode=%v)", err, fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(dur, "node_modules", "leftpad", "index.js")); err != nil {
		t.Fatalf("copied content missing: %v", err)
	}
	// Independence: writing into the durable copy must not touch src.
	if err := os.WriteFile(filepath.Join(dur, "node_modules", "installed.js"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "node_modules", "installed.js")); err == nil {
		t.Fatal("durable dep write leaked into the source repo — the exact hazard this exists to prevent")
	}

	// Existing dst is never clobbered.
	pre := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pre, "node_modules", "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	provisionDeps(src, pre, true, spawnpkg.Resolved("full"))
	if _, err := os.Stat(filepath.Join(pre, "node_modules", "mine")); err != nil {
		t.Fatal("pre-existing deps dir was clobbered")
	}
}
