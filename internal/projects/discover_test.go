package projects

import (
	"os"
	"path/filepath"
	"testing"
)

// touchFile creates an empty file at path, MkdirAll'ing parents.
// Helper for fixturing project markers without bothering with
// content.
func touchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("touch %s: %v", path, err)
	}
}

// initKai puts the minimal layout for `projectAt` to recognize an
// initialized project: kaipath.Resolve(dir) must contain db.sqlite.
// We use the .kai variant rather than .git/kai because it doesn't
// require faking a real git directory layout.
func initKai(t *testing.T, dir string) {
	t.Helper()
	kaiDir := filepath.Join(dir, ".kai")
	if err := os.MkdirAll(kaiDir, 0o755); err != nil {
		t.Fatalf("mkdir kai: %v", err)
	}
	touchFile(t, filepath.Join(kaiDir, "db.sqlite"))
}

func TestDiscover_RootsFoundAtCwd(t *testing.T) {
	dir := t.TempDir()
	initKai(t, dir)

	set, outcome := Discover(dir)
	if outcome != OutcomeRootsFound {
		t.Fatalf("outcome = %v, want roots-found", outcome)
	}
	if len(set.Projects()) != 1 {
		t.Fatalf("projects = %d, want 1", len(set.Projects()))
	}
	if set.Projects()[0].Path != dir {
		t.Errorf("project path = %q, want %q", set.Projects()[0].Path, dir)
	}
}

func TestDiscover_RootsFoundInChildren(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "alpha")
	b := filepath.Join(root, "beta")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	initKai(t, a)
	initKai(t, b)

	set, outcome := Discover(root)
	if outcome != OutcomeRootsFound {
		t.Fatalf("outcome = %v, want roots-found", outcome)
	}
	if len(set.Projects()) != 2 {
		t.Fatalf("projects = %d, want 2", len(set.Projects()))
	}
}

func TestDiscover_ContainerByName(t *testing.T) {
	// A directory literally named "projects" with no kai children
	// should be classified as a container even if it has subdirs.
	root := t.TempDir()
	container := filepath.Join(root, "projects")
	if err := os.MkdirAll(filepath.Join(container, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, outcome := Discover(container)
	if outcome != OutcomeContainer {
		t.Fatalf("outcome = %v, want container", outcome)
	}
}

func TestDiscover_ContainerBySiblingMarkers(t *testing.T) {
	// cwd has no markers itself, but 3+ children each look like a
	// project (have .git). That's a container.
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		child := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(child, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, outcome := Discover(root)
	if outcome != OutcomeContainer {
		t.Fatalf("outcome = %v, want container", outcome)
	}
}

// TestDiscover_ContainerByGitChildOne pins the strong-marker rule:
// even ONE child with its own .git/ is enough to flag cwd as a
// container. The May-2026 footgun: a user's holding directory with
// 1-2 sibling git repos would previously fall under the 3+ weak-
// marker threshold, miss container detection, and auto-init at the
// parent — slurping the sibling repos' source into one big
// snapshot. With this rule, a single git sub-repo is enough to
// refuse auto-init.
func TestDiscover_ContainerByGitChildOne(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "only-one")
	if err := os.MkdirAll(filepath.Join(child, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, outcome := Discover(root)
	if outcome != OutcomeContainer {
		t.Fatalf("outcome = %v, want container (single git sub-repo should suffice)", outcome)
	}
}

// TestDiscover_ContainerByGitChildTwo confirms the rule still
// holds for the realistic "two sibling repos" case the user
// described: ~/projects/kai/ with kai/ and kai-server/ as
// independent git repos. Without the strong-marker rule this would
// have fallen through to OutcomeEmpty and auto-init'd at the parent.
func TestDiscover_ContainerByGitChildTwo(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"kai", "kai-server"} {
		child := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(child, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, outcome := Discover(root)
	if outcome != OutcomeContainer {
		t.Fatalf("outcome = %v, want container", outcome)
	}
}

// TestDiscover_NotContainerWhenCwdIsAGitRepo guards the inverse:
// if cwd ITSELF is a git repo, having a git submodule child
// shouldn't make us classify the whole thing as a container.
// hasProjectMarkers(cwd) returns true → isContainer's early
// return blocks the strong-marker rule from firing here.
func TestDiscover_NotContainerWhenCwdIsAGitRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Add a sub-dir with its own .git/ — would trigger strong-marker
	// rule if not for the cwd-is-project guard above it.
	if err := os.MkdirAll(filepath.Join(root, "sub", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, outcome := Discover(root)
	if outcome == OutcomeContainer {
		t.Fatal("a git repo with a git child should NOT be classified as a container")
	}
}

func TestDiscover_UninitProject(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, filepath.Join(dir, "go.mod"))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	set, outcome := Discover(dir)
	if outcome != OutcomeUninitProject {
		t.Fatalf("outcome = %v, want uninit-project", outcome)
	}
	if len(set.Projects()) != 1 {
		t.Fatalf("projects = %d, want 1 (the uninit one)", len(set.Projects()))
	}
	if set.Projects()[0].Name != "foo" {
		t.Errorf("name = %q, want foo", set.Projects()[0].Name)
	}
}

func TestDiscover_Empty(t *testing.T) {
	dir := t.TempDir()
	_, outcome := Discover(dir)
	if outcome != OutcomeEmpty {
		t.Fatalf("outcome = %v, want empty", outcome)
	}
}

func TestProjectFor_LongestPrefix(t *testing.T) {
	// Two projects, one nested under the other; the inner should
	// win for paths inside it.
	outer := "/repo"
	inner := "/repo/sub"
	set := New("/repo", []*Project{
		{Path: outer, Name: "outer"},
		{Path: inner, Name: "inner"},
	})

	got := set.ProjectFor("/repo/sub/file.go")
	if got == nil || got.Name != "inner" {
		t.Fatalf("ProjectFor(/repo/sub/file.go) = %v, want inner", got)
	}
	got = set.ProjectFor("/repo/file.go")
	if got == nil || got.Name != "outer" {
		t.Fatalf("ProjectFor(/repo/file.go) = %v, want outer", got)
	}
	got = set.ProjectFor("/elsewhere/file.go")
	if got != nil {
		t.Fatalf("ProjectFor outside any root = %v, want nil", got)
	}
}

func TestProjectFor_RelativePathResolvesToDiscoveryRoot(t *testing.T) {
	set := New("/repo", []*Project{
		{Path: "/repo/api", Name: "api"},
	})
	got := set.ProjectFor("api/handler.go")
	if got == nil || got.Name != "api" {
		t.Fatalf("relative path routing failed: got %v", got)
	}
}
