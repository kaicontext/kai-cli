package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyProjectTree_FaithfulCopy verifies the spawn-materialization
// copy reproduces source files byte-for-byte while skipping the dirs
// `kai capture` ignores. This is the fix for the 2026-05-15 spawn
// failures: a copy of the live tree cannot be a mixed-era
// reconstruction the way a snapshot checkout could.
func TestCopyProjectTree_FaithfulCopy(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "spawn")

	// Source layout: real code + dirs that must be excluded.
	writeTreeFile(t, src, "main.go", "package main\n")
	writeTreeFile(t, src, "internal/x/x.go", "package x\n")
	writeTreeFile(t, src, ".git/config", "[core]\n")
	writeTreeFile(t, src, ".kai/db.sqlite", "binary")
	writeTreeFile(t, src, "node_modules/dep/index.js", "module.exports={}")

	if err := copyProjectTree(src, dst); err != nil {
		t.Fatalf("copyProjectTree: %v", err)
	}

	// Code files copied with identical content.
	if got := readTreeFile(t, dst, "main.go"); got != "package main\n" {
		t.Errorf("main.go content = %q, want faithful copy", got)
	}
	if got := readTreeFile(t, dst, "internal/x/x.go"); got != "package x\n" {
		t.Errorf("nested file content = %q, want faithful copy", got)
	}

	// Excluded dirs must NOT be copied — copying .kai/.git/node_modules
	// is what made spawns huge and (for .kai) self-referential.
	for _, excluded := range []string{".git/config", ".kai/db.sqlite", "node_modules/dep/index.js"} {
		if _, err := os.Stat(filepath.Join(dst, excluded)); !os.IsNotExist(err) {
			t.Errorf("%s should have been excluded from the spawn copy", excluded)
		}
	}
}

// TestCopyProjectTree_PreservesExecutableBit confirms mode bits
// survive the copy — a spawn whose build script lost +x would fail
// VERIFY for a reason unrelated to the agent's edits.
func TestCopyProjectTree_PreservesExecutableBit(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "spawn")

	scriptPath := filepath.Join(src, "build.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyProjectTree(src, dst); err != nil {
		t.Fatalf("copyProjectTree: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "build.sh"))
	if err != nil {
		t.Fatalf("stat copied script: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("executable bit lost: mode = %v", info.Mode())
	}
}

func writeTreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTreeFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
