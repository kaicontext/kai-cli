package agent

import (
	"os"
	"path/filepath"
	"testing"

	"kai/internal/projects"
)

// TestFilterExistingPaths pins the phantom-file filter. Drove the
// 2026-05-12 dogfood fix: the graph carried stale File nodes for
// package.json / index.js (from an earlier capture cycle in a
// since-deleted scaffolding) and the injector listed them in
// "Files in scope (kai graph)" every turn. The agent dutifully
// reported them to the user as real, which read as a hallucination
// even though the input was technically grounded in the (wrong)
// graph data.
func TestFilterExistingPaths_DropsPhantoms(t *testing.T) {
	ws := t.TempDir()
	// Create one real file; leave the other paths as phantoms.
	real := filepath.Join(ws, "real.go")
	if err := os.WriteFile(real, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := []string{"real.go", "package.json", "index.js", "kai.modules.yaml"}
	got := filterExistingPaths(input, ws, nil)

	if len(got) != 1 || got[0] != "real.go" {
		t.Errorf("expected only real.go to survive, got %v", got)
	}
}

// TestFilterExistingPaths_ResolvesAcrossProjects covers the multi-
// root case. The graph stores project-relative paths; a path that
// doesn't exist under the workspace root may still resolve under a
// sibling project. The filter must try every project before giving
// up — otherwise legitimate sibling-project files would get dropped.
func TestFilterExistingPaths_ResolvesAcrossProjects(t *testing.T) {
	root := t.TempDir()
	siblingDir := filepath.Join(root, "sibling")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(siblingDir, "owned.go")
	if err := os.WriteFile(siblingFile, []byte("package y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	set := projects.New(root, []*projects.Project{
		{Name: "primary", Path: filepath.Join(root, "primary")},
		{Name: "sibling", Path: siblingDir},
	})

	// owned.go isn't under the primary path; it IS under sibling.
	// The filter must keep it.
	input := []string{"owned.go", "phantom.go"}
	got := filterExistingPaths(input, filepath.Join(root, "primary"), set)

	if len(got) != 1 || got[0] != "owned.go" {
		t.Errorf("expected owned.go to survive (resolves under sibling project), got %v", got)
	}
}

// TestFilterExistingPaths_EmptyInputPassthrough: the filter is on
// the hot path of every agent turn; an empty input must not allocate
// or os.Stat — just return the same slice.
func TestFilterExistingPaths_EmptyInputPassthrough(t *testing.T) {
	got := filterExistingPaths(nil, "/tmp", nil)
	if got != nil {
		t.Errorf("expected nil for nil input, got %v", got)
	}
}
