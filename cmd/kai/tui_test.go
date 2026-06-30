package main

import (
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/projects"
)

// TestNarrowToInvokedProject_InsideSubprojectNarrows reproduces the TUI
// regression: cwd is a sub-project (kai-tui) with its own .kai/, but a
// parent holds kai.projects.yaml so Discover returns the full multi-root
// Set (4 projects) with DiscoveryRoot at the parent and InvokedFrom at
// the sub-project. Launching from inside a sub-project must collapse the
// Set to ONLY that project, otherwise the planner emits workspace-
// relative, project-prefixed paths (`cd kai-tui && go test`,
// `kai-tui/internal/...`) that the executor/verify — rooted at the
// sub-project — can't resolve. One project rooted at cwd proves the
// multi-root path-emission branches (len(Projects())>1) switch off.
func TestNarrowToInvokedProject_InsideSubprojectNarrows(t *testing.T) {
	root := filepath.FromSlash("/ws")
	tui := filepath.FromSlash("/ws/kai-tui")
	set := &projects.Set{DiscoveryRoot: root, InvokedFrom: tui}
	set.SetProjectsForTest([]*projects.Project{
		{Path: filepath.FromSlash("/ws/kai-cli"), Name: "kai-cli"},
		{Path: filepath.FromSlash("/ws/kai-core"), Name: "kai-core"},
		{Path: filepath.FromSlash("/ws/kai-engine"), Name: "kai-engine"},
		{Path: tui, Name: "kai-tui"},
	})

	got, err := narrowToInvokedProject(set)
	if err != nil {
		t.Fatalf("narrowToInvokedProject: %v", err)
	}
	if n := len(got.Projects()); n != 1 {
		t.Fatalf("narrowed set should be single-root, got %d projects", n)
	}
	if p := got.Projects()[0].Path; p != tui {
		t.Errorf("narrowed project path = %q, want %q", p, tui)
	}
	if got.DiscoveryRoot != tui {
		t.Errorf("narrowed DiscoveryRoot = %q, want %q (repo-relative paths)", got.DiscoveryRoot, tui)
	}
	if p := got.Primary().Path; p != tui {
		t.Errorf("narrowed Primary = %q, want %q", p, tui)
	}
}

// TestNarrowToInvokedProject_ContainerRootPreservesMultiRoot guards the
// intentional cross-project mode: when `kai code` is launched from the
// container root itself (InvokedFrom == DiscoveryRoot, the dir holding
// kai.projects.yaml), the full multi-root Set must pass through untouched
// so the "multi-root workspace" banner + cross-project planning still
// work.
func TestNarrowToInvokedProject_ContainerRootPreservesMultiRoot(t *testing.T) {
	root := filepath.FromSlash("/ws")
	set := &projects.Set{DiscoveryRoot: root, InvokedFrom: root}
	ps := []*projects.Project{
		{Path: filepath.FromSlash("/ws/kai-cli"), Name: "kai-cli"},
		{Path: filepath.FromSlash("/ws/kai-core"), Name: "kai-core"},
		{Path: filepath.FromSlash("/ws/kai-engine"), Name: "kai-engine"},
		{Path: filepath.FromSlash("/ws/kai-tui"), Name: "kai-tui"},
	}
	set.SetProjectsForTest(ps)

	got, err := narrowToInvokedProject(set)
	if err != nil {
		t.Fatalf("narrowToInvokedProject: %v", err)
	}
	if got != set {
		t.Errorf("container-root launch should return the Set unchanged")
	}
	if n := len(got.Projects()); n != 4 {
		t.Errorf("multi-root Set should keep all 4 projects, got %d", n)
	}
}

// TestNarrowToInvokedProject_SingleRootIsNoOp keeps a genuine single-root
// workspace (InvokedFrom == DiscoveryRoot == the one project) a pass-
// through.
func TestNarrowToInvokedProject_SingleRootIsNoOp(t *testing.T) {
	repo := filepath.FromSlash("/repo")
	set := &projects.Set{DiscoveryRoot: repo, InvokedFrom: repo}
	set.SetProjectsForTest([]*projects.Project{{Path: repo, Name: "repo"}})

	got, err := narrowToInvokedProject(set)
	if err != nil {
		t.Fatalf("narrowToInvokedProject: %v", err)
	}
	if got != set {
		t.Errorf("single-root set should be returned unchanged")
	}
}
