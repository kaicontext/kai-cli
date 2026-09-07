package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rcTreeFiles must list the WHOLE tree no matter which directory the command
// was invoked from. Without --full-tree, git scopes ls-tree to the cwd and
// spells paths relative to it, so every claim fails to resolve and is held —
// RiskCount 0, green badge, real defects shipped. This builds a throwaway repo
// with a nested file and reads the tree from the subdirectory.
func TestTreeFilesIsNotScopedToTheCurrentDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v — %s", strings.Join(args, " "), err, out)
		}
	}
	run(repo, "init", "-q")
	sub := filepath.Join(repo, "frontend", "dist")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "panel.js"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "top.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", ".")
	run(repo, "commit", "-qm", "seed")

	hashOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(string(hashOut))

	// Read the tree from the NESTED directory, which is what breaks it.
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	tree := rcTreeFiles(hash)

	want := map[string]bool{"frontend/dist/panel.js": false, "top.txt": false}
	for _, p := range tree {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, found := range want {
		if !found {
			t.Errorf("tree read from frontend/dist is missing %q; got %v", p, tree)
		}
	}

	// And the whole point: a claim naming the full path must resolve.
	if got, n := rcResolvePath("frontend/dist/panel.js", tree); got == "" {
		t.Errorf("rcResolvePath(frontend/dist/panel.js) did not resolve (matches=%d) — the claim would be HELD and stop counting as a risk", n)
	}
}
