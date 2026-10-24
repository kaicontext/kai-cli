package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/projects"
)

// A checkout with no db.sqlite under a workspace kai.projects.yaml that does
// not list it — the git-worktree-next-to-its-repo layout — makes Discover
// report roots while the primary's DB stays nil after Open. The fast pass
// used to hand that nil DB to BlastFor and panic after printing its review.
func TestRCBlast_RootsFoundButNoGraphDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "listed"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "unlisted-worktree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "projects:\n    - path: listed\n      name: listed\n      pinned: true\n"
	if err := os.WriteFile(filepath.Join(root, "kai.projects.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	set, outcome := projects.Discover(wt)
	if outcome != projects.OutcomeRootsFound {
		t.Fatalf("Discover outcome = %v, want roots found (the case that reached BlastFor)", outcome)
	}
	if err := set.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer set.Close()
	if set.Primary().DB != nil {
		t.Fatal("precondition: primary should have no graph DB")
	}

	blast := rcBlast(context.Background(), set, []string{"pricing/prices.yaml"})
	if len(blast.Nodes) != 0 || len(blast.ShouldTouch) != 0 {
		t.Fatalf("blast with no graph = %+v, want an empty panel", blast)
	}
}

func TestRCBlast_NoSet(t *testing.T) {
	blast := rcBlast(context.Background(), nil, []string{"a.go"})
	if len(blast.Nodes) != 0 {
		t.Fatalf("blast with no set = %+v, want empty", blast)
	}
}
