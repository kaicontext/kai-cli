package gitio

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestGitSource_IncludesGoFiles(t *testing.T) {
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	if _, err := worktree.Add("main.go"); err != nil {
		t.Fatalf("add main.go: %v", err)
	}

	commitHash, err := worktree.Commit("add main", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	gs, err := OpenSource(dir, commitHash.String())
	if err != nil {
		t.Fatalf("open source: %v", err)
	}

	files, err := gs.GetFiles()
	if err != nil {
		t.Fatalf("get files: %v", err)
	}

	found := false
	for _, f := range files {
		if f.Path == "main.go" && f.Lang == "go" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected main.go to be included in git snapshot")
	}
}
