package kaipath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Run("env override wins over filesystem", func(t *testing.T) {
		t.Setenv("KAI_DIR", "/tmp/custom-kai")
		got := Resolve(t.TempDir())
		if got != "/tmp/custom-kai" {
			t.Errorf("expected env override, got %q", got)
		}
	})

	t.Run("existing .kai is preferred (backward compat)", func(t *testing.T) {
		t.Setenv("KAI_DIR", "")
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".kai"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := Resolve(root)
		want := filepath.Join(root, ".kai")
		if got != want {
			t.Errorf("expected existing .kai to win over .git, got %q", got)
		}
	})

	t.Run("git repo without .kai uses .git/kai", func(t *testing.T) {
		t.Setenv("KAI_DIR", "")
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := Resolve(root)
		want := filepath.Join(root, ".git", "kai")
		if got != want {
			t.Errorf("expected .git/kai, got %q", got)
		}
	})

	t.Run("non-git project falls back to .kai", func(t *testing.T) {
		t.Setenv("KAI_DIR", "")
		root := t.TempDir()
		got := Resolve(root)
		want := filepath.Join(root, ".kai")
		if got != want {
			t.Errorf("expected .kai fallback, got %q", got)
		}
	})

	t.Run(".git as a file (worktree) falls back to .kai", func(t *testing.T) {
		t.Setenv("KAI_DIR", "")
		root := t.TempDir()
		// Mimic a git worktree: .git is a file containing "gitdir: ..."
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := Resolve(root)
		want := filepath.Join(root, ".kai")
		if got != want {
			t.Errorf("expected .kai fallback for gitfile worktree, got %q", got)
		}
	})
}

func TestNeedsGitignore(t *testing.T) {
	cases := []struct {
		resolved string
		want     bool
	}{
		{".kai", true},
		{filepath.Join(".git", "kai"), false},
		{filepath.Join("/abs/path/.git", "kai"), false},
		{"/some/custom/kaidir", true},
		{filepath.Join("/abs/proj", ".kai"), true},
	}
	for _, c := range cases {
		if got := NeedsGitignore(c.resolved); got != c.want {
			t.Errorf("NeedsGitignore(%q) = %v, want %v", c.resolved, got, c.want)
		}
	}
}
