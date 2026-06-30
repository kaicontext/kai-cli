package main

import (
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/projects"
)

// TestNarrowToOwner_MultiRootCollapsesToInvokedProject reproduces the
// autofix divergence: cwd is a sub-project (kai-tui) that has its own
// .kai/, but a parent holds kai.projects.yaml so Discover returns the
// full multi-root Set (4 projects). A single-repo run must collapse that
// to ONLY the project that owns cwd, otherwise the planner emits
// workspace-relative, project-prefixed paths (`cd kai-tui && go test`,
// `kai-tui/internal/...`) that the executor/verify — rooted at the
// sub-project — can't resolve. Proving the narrowed Set has exactly one
// project rooted at cwd proves the multi-root path-emission branches
// (len(Projects())>1 in planner/graph_context) all switch off.
func TestNarrowToOwner_MultiRootCollapsesToInvokedProject(t *testing.T) {
	root := filepath.FromSlash("/ws")
	tui := filepath.FromSlash("/ws/kai-tui")
	set := &projects.Set{DiscoveryRoot: root, InvokedFrom: tui}
	set.SetProjectsForTest([]*projects.Project{
		{Path: filepath.FromSlash("/ws/kai-cli"), Name: "kai-cli"},
		{Path: filepath.FromSlash("/ws/kai-core"), Name: "kai-core"},
		{Path: filepath.FromSlash("/ws/kai-engine"), Name: "kai-engine"},
		{Path: tui, Name: "kai-tui"},
	})

	got, err := narrowToOwner(set, tui)
	if err != nil {
		t.Fatalf("narrowToOwner: %v", err)
	}
	if n := len(got.Projects()); n != 1 {
		t.Fatalf("narrowed set should be single-root, got %d projects", n)
	}
	if p := got.Projects()[0].Path; p != tui {
		t.Errorf("narrowed project path = %q, want %q", p, tui)
	}
	if got.DiscoveryRoot != tui {
		t.Errorf("narrowed DiscoveryRoot = %q, want %q (the sub-project, so paths are repo-relative)", got.DiscoveryRoot, tui)
	}
	if p := got.Primary().Path; p != tui {
		t.Errorf("narrowed Primary = %q, want %q", p, tui)
	}
}

// TestNarrowToOwner_AlreadySingleRootIsUnchanged keeps the common case a
// no-op: a genuine single-root workspace must pass straight through.
func TestNarrowToOwner_AlreadySingleRootIsUnchanged(t *testing.T) {
	repo := filepath.FromSlash("/repo")
	set := &projects.Set{DiscoveryRoot: repo, InvokedFrom: repo}
	set.SetProjectsForTest([]*projects.Project{{Path: repo, Name: "repo"}})

	got, err := narrowToOwner(set, repo)
	if err != nil {
		t.Fatalf("narrowToOwner: %v", err)
	}
	if got != set {
		t.Errorf("single-root set should be returned unchanged")
	}
}

// TestNarrowToOwner_EmptySetErrors guards the container case: cwd in no
// project at all has no owner to root on, so a single-repo run can't proceed.
func TestNarrowToOwner_EmptySetErrors(t *testing.T) {
	set := &projects.Set{DiscoveryRoot: filepath.FromSlash("/ws")}
	if _, err := narrowToOwner(set, filepath.FromSlash("/ws")); err == nil {
		t.Fatalf("expected error for a set with no projects")
	}
}
