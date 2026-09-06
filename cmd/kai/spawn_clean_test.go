package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDurableSpawnRequestsCleanSource(t *testing.T) {
	if !cleanSpawnRequested(false, true) {
		t.Fatal("durable session spawn did not request a clean source")
	}
	if cleanSpawnRequested(false, false) {
		t.Fatal("ordinary snapshot spawn unexpectedly requested a clean source")
	}
}

func TestCleanSpawnToGitHeadDropsDirtySourceFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Kai Test", "GIT_AUTHOR_EMAIL=kai@test", "GIT_COMMITTER_NAME=Kai Test", "GIT_COMMITTER_EMAIL=kai@test")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit(src, "init", "-q")
	mustWrite := func(root, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(src, "tracked.txt", "committed\n")
	mustWrite(src, "deleted.txt", "committed\n")
	runGit(src, "add", "-A")
	runGit(src, "commit", "-qm", "base")
	mustWrite(src, "tracked.txt", "dirty\n")
	if err := os.Remove(filepath.Join(src, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(src, "untracked.txt", "dirty\n")
	mustWrite(src, "staged.txt", "dirty\n")
	runGit(src, "add", "staged.txt")
	mustWrite(src, "node_modules/package/installed", "source dependency\n")

	// Model the Kai snapshot that was materialized before cleaning.
	mustWrite(dst, "tracked.txt", "dirty\n")
	mustWrite(dst, "untracked.txt", "dirty\n")
	mustWrite(dst, "staged.txt", "dirty\n")
	mustWrite(dst, "node_modules/keep", "dependency\n")
	if err := cleanSpawnToGitHead(src, dst); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"tracked.txt": "committed\n", "deleted.txt": "committed\n", "node_modules/keep": "dependency\n"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked file survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "staged.txt")); !os.IsNotExist(err) {
		t.Fatalf("staged-only file survived: %v", err)
	}
}
