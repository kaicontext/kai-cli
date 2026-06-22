package projects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckContainerInvariant_ConflictReturnsError pins the failure
// shape that prompted this guard: a directory that has both
// kai.projects.yaml (says "I'm a container") and .kai/ (says "I'm
// a project"). The two contradict each other and produce silent
// cross-DB errors downstream. Confirmed May 2026 against the user's
// session: orchestrator wrote to one .kai/, queried another, and
// surfaced "no such table: refs" three layers down.
func TestCheckContainerInvariant_ConflictReturnsError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ProjectsFileName), []byte("projects: []\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".kai"), 0755); err != nil {
		t.Fatalf("mkdir kai: %v", err)
	}
	err := CheckContainerInvariant(root)
	if err == nil {
		t.Fatal("expected error when both yaml and .kai/ coexist")
	}
	msg := err.Error()
	for _, want := range []string{
		"misconfig",
		"yaml at:",
		".kai at:",
		"delete the .kai/",
		"delete kai.projects.yaml",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
}

// TestCheckContainerInvariant_OnlyYAMLOK verifies the legitimate
// "container" shape: kai.projects.yaml declaring sub-projects, no
// .kai/ at this level. Should pass without error.
func TestCheckContainerInvariant_OnlyYAMLOK(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ProjectsFileName), []byte("projects: []\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := CheckContainerInvariant(root); err != nil {
		t.Errorf("yaml-only should be valid: %v", err)
	}
}

// TestCheckContainerInvariant_OnlyKaiDirOK verifies the legitimate
// "single project" shape: .kai/ at this level, no
// kai.projects.yaml. Should pass without error.
func TestCheckContainerInvariant_OnlyKaiDirOK(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".kai"), 0755); err != nil {
		t.Fatalf("mkdir kai: %v", err)
	}
	if err := CheckContainerInvariant(root); err != nil {
		t.Errorf("kai-only should be valid: %v", err)
	}
}

// TestCheckContainerInvariant_NeitherOK is the brand-new directory
// case: nothing kai-related yet. The invariant doesn't fire — there's
// no conflict to flag. Other paths (Discover, init prompts) handle
// the "this isn't a project yet" case.
func TestCheckContainerInvariant_NeitherOK(t *testing.T) {
	root := t.TempDir()
	if err := CheckContainerInvariant(root); err != nil {
		t.Errorf("empty dir should be valid: %v", err)
	}
}

// TestCheckContainerInvariant_EmptyRootSkipped pins that an empty
// root string returns nil rather than crashing on filepath.Clean
// of "". Defensive: callers shouldn't pass empty but if they do, no
// harm done.
func TestCheckContainerInvariant_EmptyRootSkipped(t *testing.T) {
	if err := CheckContainerInvariant(""); err != nil {
		t.Errorf("empty root should be a no-op: %v", err)
	}
}

// TestCheckContainerInvariant_KaiDirAsFileNotDirSkipped pins that a
// FILE named .kai (rather than a directory) doesn't trigger the
// invariant. .kai-the-file isn't claiming to be a project — only
// .kai/-the-directory does. We check IsDir() to avoid false
// positives.
func TestCheckContainerInvariant_KaiDirAsFileNotDirSkipped(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ProjectsFileName), []byte(""), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	// A file (not a dir) named .kai — unusual but legal on disk.
	if err := os.WriteFile(filepath.Join(root, ".kai"), []byte(""), 0644); err != nil {
		t.Fatalf("write .kai file: %v", err)
	}
	if err := CheckContainerInvariant(root); err != nil {
		t.Errorf(".kai-as-file should not trigger invariant: %v", err)
	}
}
