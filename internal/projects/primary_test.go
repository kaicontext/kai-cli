package projects

import (
	"path/filepath"
	"testing"
)

// TestPrimary_RoutesByInvokedFrom verifies the bug fix: a Set built
// from a subdir of a multi-root workspace returns the project the
// user is actually sitting in, not projects[0].
//
// Reproduction: a yaml at /workspace/kai.projects.yaml lists Kai
// (kai-cli) first and Kai-Server second. User runs `kai code` from
// /workspace/kai-server. Without InvokedFrom-aware Primary, this
// silently lands them in kai-cli — the exact case we hit on 2026-05-11.
func TestPrimary_RoutesByInvokedFrom(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "kai")
	server := filepath.Join(root, "kai-server")

	set := &Set{
		DiscoveryRoot: root,
		InvokedFrom:   server,
		projects: []*Project{
			{Path: cli, Name: "Kai"},
			{Path: server, Name: "Kai Server"},
		},
	}

	got := set.Primary()
	if got == nil {
		t.Fatal("Primary returned nil")
	}
	if got.Path != server {
		t.Errorf("Primary.Path = %q, want %q (the dir the user invoked from)", got.Path, server)
	}
}

// TestPrimary_LongestPrefixWins covers nested layouts: if a project
// at /repo and another at /repo/sub both contain the invocation dir,
// the deeper one wins — same rule ProjectFor uses for path routing.
func TestPrimary_LongestPrefixWins(t *testing.T) {
	root := t.TempDir()
	outer := root
	inner := filepath.Join(root, "sub")
	invokedAt := filepath.Join(inner, "pkg")

	set := &Set{
		DiscoveryRoot: root,
		InvokedFrom:   invokedAt,
		projects: []*Project{
			{Path: outer, Name: "outer"},
			{Path: inner, Name: "inner"},
		},
	}
	got := set.Primary()
	if got == nil || got.Path != inner {
		t.Errorf("Primary = %v, want inner (%s)", got, inner)
	}
}

// TestPrimary_FallsBackWhenInvokedFromUnowned verifies the safety
// net: if InvokedFrom is outside every project, return projects[0]
// rather than nil. This is what happens when the user runs from the
// discovery root itself (no project contains it).
func TestPrimary_FallsBackWhenInvokedFromUnowned(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")

	set := &Set{
		DiscoveryRoot: root,
		InvokedFrom:   root, // outside both a and b
		projects: []*Project{
			{Path: a, Name: "alpha"},
			{Path: b, Name: "beta"},
		},
	}
	got := set.Primary()
	if got == nil || got.Path != a {
		t.Errorf("Primary = %v, want alpha fallback", got)
	}
}

// TestPrimary_NoInvokedFromUsesFirst preserves the legacy behavior
// for callers that don't pass InvokedFrom (Single, New, tests).
func TestPrimary_NoInvokedFromUsesFirst(t *testing.T) {
	set := &Set{
		projects: []*Project{
			{Path: "/a", Name: "first"},
			{Path: "/b", Name: "second"},
		},
	}
	got := set.Primary()
	if got == nil || got.Name != "first" {
		t.Errorf("Primary without InvokedFrom should return projects[0], got %v", got)
	}
}

// TestPrimary_ExactPathMatch verifies that InvokedFrom equal to a
// project's Path (not a subdir of it) still selects that project —
// the user's cwd IS the project root.
func TestPrimary_ExactPathMatch(t *testing.T) {
	set := &Set{
		InvokedFrom: "/repo/b",
		projects: []*Project{
			{Path: "/repo/a", Name: "alpha"},
			{Path: "/repo/b", Name: "beta"},
		},
	}
	got := set.Primary()
	if got == nil || got.Name != "beta" {
		t.Errorf("Primary = %v, want beta", got)
	}
}

// TestHasPrefixDir guards the helper against the
// strings.HasPrefix("/a/bc", "/a/b") false-positive.
func TestHasPrefixDir(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		sub, parent string
		want        bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a/b", "/a/b", false}, // equal, not "inside"
		{"/a", "/a/b", false},
		{sep + "x" + sep + "y", sep + "x", true},
	}
	for _, c := range cases {
		got := hasPrefixDir(c.sub, c.parent)
		if got != c.want {
			t.Errorf("hasPrefixDir(%q, %q) = %v, want %v", c.sub, c.parent, got, c.want)
		}
	}
}

// TestByName_BasenameFallback pins the 2026-05-11 fix: SmartName
// often produces human-friendly Names with spaces (README H1
// "Kai Server" for a directory named kai-server). The agent
// overwhelmingly references projects by directory name in paths,
// so ByName must accept the directory basename as a second-chance
// match. Without this, "view kai-server/foo" silently fails to
// route through the multi-root prefix and the agent has to guess
// the README-derived display name.
func TestByName_BasenameFallback(t *testing.T) {
	root := t.TempDir()
	set := &Set{
		DiscoveryRoot: root,
		projects: []*Project{
			{Path: filepath.Join(root, "kai-server"), Name: "Kai Server"},
			{Path: filepath.Join(root, "kai-cli"), Name: "Kai"},
		},
	}
	if got := set.ByName("Kai Server"); got == nil || got.Name != "Kai Server" {
		t.Errorf("ByName(human Name) failed: %+v", got)
	}
	if got := set.ByName("kai-server"); got == nil || got.Name != "Kai Server" {
		t.Errorf("ByName(dir basename) failed — should fall back to basename match: %+v", got)
	}
	if got := set.ByName("kai-cli"); got == nil || got.Name != "Kai" {
		t.Errorf("ByName(dir basename) failed for second project: %+v", got)
	}
	if got := set.ByName("nonexistent"); got != nil {
		t.Errorf("ByName(unknown) should return nil, got %+v", got)
	}
}
