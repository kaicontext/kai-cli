package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/drift"
)

// setupCatchupRepo builds a real git repo in a temp dir, chdirs into it,
// points the global kaiDir at it, and returns the first commit's SHA.
func setupCatchupRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	makeFixtureLayout(t, repo, map[string]string{
		"go.mod":         "module example.com/cu\n\ngo 1.25\n",
		"pkg/foo/foo.go": "package foo\n\nfunc Foo() int { return 1 }\n",
	})
	initGitRepo(t, repo)
	t.Chdir(repo)

	oldKaiDir := kaiDir
	kaiDir = filepath.Join(repo, ".kai")
	t.Cleanup(func() { kaiDir = oldKaiDir })

	return gitSHA(t, repo)
}

func gitSHA(t *testing.T, repo string) string {
	t.Helper()
	out, err := gitCmdOutput("-C", repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return out
}

func addCommit(t *testing.T, repo, path, content, msg string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, repo, msg)
	return gitSHA(t, repo)
}

func TestCatchUpDriftCheckpointsEachCommit(t *testing.T) {
	c1 := setupCatchupRepo(t)
	repo, _ := os.Getwd()
	db := newTestDB(t)

	if err := drift.Pin(kaiDir, "refs/heads/main", c1, time.Now()); err != nil {
		t.Fatal(err)
	}
	addCommit(t, repo, "pkg/foo/foo.go", "package foo\n\nfunc Foo() int { return 2 }\n", "c2")
	c3 := addCommit(t, repo, "pkg/bar/bar.go", "package bar\n\nfunc Bar() int { return 3 }\n", "c3")

	res, err := catchUpDrift(db, 0, nil)
	if err != nil {
		t.Fatalf("catchUpDrift: %v", err)
	}
	if res.Processed != 2 || res.Remaining != 0 || res.BudgetHit {
		t.Fatalf("result = %+v, want 2 processed", res)
	}

	rep, err := drift.Compute(repo, kaiDir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Relationship != drift.RelSynced || rep.GraphState != c3 {
		t.Errorf("after catch-up: %+v, want synced at %s", rep, c3)
	}
	man, err := drift.LoadManifest(kaiDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Commits) != 0 {
		t.Errorf("manifest not retired: %+v", man.Commits)
	}
}

func TestCatchUpDriftBudgetAndResume(t *testing.T) {
	c1 := setupCatchupRepo(t)
	repo, _ := os.Getwd()
	db := newTestDB(t)

	if err := drift.Pin(kaiDir, "refs/heads/main", c1, time.Now()); err != nil {
		t.Fatal(err)
	}
	addCommit(t, repo, "a.go", "package cu\n", "c2")
	tip := addCommit(t, repo, "b.go", "package cu\n", "c3")

	// An already-elapsed budget starts no commit: the graph stays exactly at
	// the last checkpoint, honestly reported.
	res, err := catchUpDrift(db, time.Nanosecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BudgetHit || res.Processed != 0 || res.Remaining != 2 {
		t.Fatalf("budget run = %+v, want 0 processed, budget hit", res)
	}
	rep, _ := drift.Compute(repo, kaiDir)
	if rep.GraphState != c1 {
		t.Errorf("graph moved without a completed checkpoint: %+v", rep)
	}

	// Resume unbounded: picks up from the pin and finishes.
	res, err = catchUpDrift(db, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 2 {
		t.Fatalf("resume = %+v, want 2 processed", res)
	}
	rep, _ = drift.Compute(repo, kaiDir)
	if rep.Relationship != drift.RelSynced || rep.GraphState != tip {
		t.Errorf("after resume: %+v", rep)
	}
}

func TestCatchUpDriftNoopWhenSyncedOrUnpinned(t *testing.T) {
	c1 := setupCatchupRepo(t)
	db := newTestDB(t)

	// Unpinned: nothing to advance from.
	res, err := catchUpDrift(db, 0, nil)
	if err != nil || res.Processed != 0 {
		t.Fatalf("unpinned run = %+v, %v", res, err)
	}

	// Synced: nothing to do.
	if err := drift.Pin(kaiDir, "refs/heads/main", c1, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err = catchUpDrift(db, 0, nil)
	if err != nil || res.Processed != 0 {
		t.Fatalf("synced run = %+v, %v", res, err)
	}
}
