package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
)

// TestCheckRepoDBAlignment_SameDirOK is the happy path: when the
// caller opens the db from <mainRepo>/.kai/ and passes mainRepo
// alongside, the alignment check returns nil. Verified by the
// "actually run a real Open against a real tempdir" route so the
// canonicalPath / EvalSymlinks path gets exercised end-to-end.
func TestCheckRepoDBAlignment_SameDirOK(t *testing.T) {
	repo := t.TempDir()
	kaiDir := filepath.Join(repo, ".kai")
	if err := os.MkdirAll(filepath.Join(kaiDir, "objects"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := graph.Open(filepath.Join(kaiDir, "db.sqlite"), filepath.Join(kaiDir, "objects"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := checkRepoDBAlignment(repo, db); err != nil {
		t.Errorf("expected nil for matching mainRepo+db, got: %v", err)
	}
}

// TestCheckRepoDBAlignment_DivergedReturnsHint pins the failure
// mode that motivates this guard: mainRepo points at parent/, db
// was opened from parent/inner/.kai/. Without the guard, downstream
// `kai capture` would write to parent/.kai while
// resolveLatestSnap reads from parent/inner/.kai — and the user
// sees "no such table: refs". With the guard, Execute fails
// up-front with both paths printed.
func TestCheckRepoDBAlignment_DivergedReturnsHint(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	inner := filepath.Join(parent, "inner")
	parentKai := filepath.Join(parent, ".kai")
	innerKai := filepath.Join(inner, ".kai")
	for _, d := range []string{
		filepath.Join(parentKai, "objects"),
		filepath.Join(innerKai, "objects"),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Open the inner db. Pass parent as mainRepo — the divergence.
	db, err := graph.Open(filepath.Join(innerKai, "db.sqlite"), filepath.Join(innerKai, "objects"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	err = checkRepoDBAlignment(parent, db)
	if err == nil {
		t.Fatal("expected divergence error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"don't agree",
		parent,
		inner,
		"pinned: true",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

// TestCheckRepoDBAlignment_NilDBSkipped pins that the guard is a
// no-op for nil-db / empty-mainRepo inputs — those failure modes
// have their own dedicated checks earlier in Execute and the
// alignment guard shouldn't double-error.
func TestCheckRepoDBAlignment_NilDBSkipped(t *testing.T) {
	if err := checkRepoDBAlignment("", nil); err != nil {
		t.Errorf("nil inputs should pass: %v", err)
	}
	if err := checkRepoDBAlignment("/some/path", nil); err != nil {
		t.Errorf("nil db should pass: %v", err)
	}
}

// TestCheckRepoDBAlignment_GitKaiLayout tolerates the .git/kai
// layout that fresh inits use in a git repo. kaipath.Resolve
// returns <repo>/.git/kai when .git is a real directory, and the
// guard must agree with that resolution rather than insisting on
// a literal .kai/.
func TestCheckRepoDBAlignment_GitKaiLayout(t *testing.T) {
	repo := t.TempDir()
	gitKai := filepath.Join(repo, ".git", "kai")
	if err := os.MkdirAll(filepath.Join(gitKai, "objects"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := graph.Open(filepath.Join(gitKai, "db.sqlite"), filepath.Join(gitKai, "objects"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := checkRepoDBAlignment(repo, db); err != nil {
		t.Errorf("git/kai layout should align: %v", err)
	}
}
