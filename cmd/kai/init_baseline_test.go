package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRevParseHead runs `git -C dir rev-parse HEAD` and returns the trimmed
// SHA plus any error (non-nil when HEAD does not resolve, e.g. no commits).
func gitRevParseHead(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	return string(out), err
}

// gitInitMain runs `git init -q -b main` in dir, failing the test on error.
func gitInitMain(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}
}

// TestInitBaselineCreatesCommitWhenNone verifies ensureGitBaselineCommit
// creates an initial commit in a freshly-init'd repo with no commits.
func TestInitBaselineCreatesCommitWhenNone(t *testing.T) {
	dir := t.TempDir()
	gitInitMain(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	created, err := ensureGitBaselineCommit(dir)
	if err != nil {
		t.Fatalf("ensureGitBaselineCommit: %v", err)
	}
	if !created {
		t.Errorf("expected created=true, got false")
	}
	if _, err := gitRevParseHead(t, dir); err != nil {
		t.Errorf("git rev-parse HEAD after baseline commit: %v", err)
	}
}

// TestInitBaselineNoopWhenCommitExists verifies ensureGitBaselineCommit is
// a no-op when a commit already exists (HEAD unchanged).
func TestInitBaselineNoopWhenCommitExists(t *testing.T) {
	dir := t.TempDir()
	gitInitMain(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// Configure identity and make an initial commit ourselves.
	for _, args := range [][]string{
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "add", "-A"},
		{"git", "commit", "-q", "-m", "initial"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}

	before, err := gitRevParseHead(t, dir)
	if err != nil {
		t.Fatalf("git rev-parse HEAD before: %v", err)
	}

	created, err := ensureGitBaselineCommit(dir)
	if err != nil {
		t.Fatalf("ensureGitBaselineCommit: %v", err)
	}
	if created {
		t.Errorf("expected created=false, got true")
	}

	after, err := gitRevParseHead(t, dir)
	if err != nil {
		t.Fatalf("git rev-parse HEAD after: %v", err)
	}
	if before != after {
		t.Errorf("HEAD changed: before=%q after=%q", before, after)
	}
}

// TestInitBaselineNoopWhenNotGitRepo verifies ensureGitBaselineCommit returns
// (false, nil) when dir is not a git repository.
func TestInitBaselineNoopWhenNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	// No git init.

	created, err := ensureGitBaselineCommit(dir)
	if err != nil {
		t.Fatalf("ensureGitBaselineCommit: %v", err)
	}
	if created {
		t.Errorf("expected created=false, got true")
	}
}

// TestInitBaselineWorksWithEmptyTree verifies ensureGitBaselineCommit creates
// a baseline commit via the --allow-empty path even when no files are staged.
func TestInitBaselineWorksWithEmptyTree(t *testing.T) {
	dir := t.TempDir()
	gitInitMain(t, dir)
	// No files added — exercise the --allow-empty path.

	created, err := ensureGitBaselineCommit(dir)
	if err != nil {
		t.Fatalf("ensureGitBaselineCommit: %v", err)
	}
	if !created {
		t.Errorf("expected created=true, got false")
	}
	if _, err := gitRevParseHead(t, dir); err != nil {
		t.Errorf("git rev-parse HEAD after empty-tree baseline commit: %v", err)
	}
}
