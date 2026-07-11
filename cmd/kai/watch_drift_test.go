package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/drift"
)

func TestGitPathRelevant(t *testing.T) {
	gitDir := "/repo/.git"
	cases := []struct {
		path string
		want bool
	}{
		{"/repo/.git/HEAD", true},
		{"/repo/.git/ORIG_HEAD", true},
		{"/repo/.git/packed-refs", true},
		{"/repo/.git/refs/heads/main", true},
		{"/repo/.git/refs/heads/feat/x", true},
		{"/repo/.git/refs/tags/v1", true},
		{"/repo/.git/index", false},
		{"/repo/.git/objects/ab/cdef", false},
		{"/repo/.git/COMMIT_EDITMSG", false},
		{"/repo/.git/hooks/post-commit", false},
		{"/elsewhere/HEAD", false},
	}
	for _, tc := range cases {
		if got := gitPathRelevant(gitDir, tc.path); got != tc.want {
			t.Errorf("gitPathRelevant(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestDriftCatchUpPassConverges(t *testing.T) {
	c1 := setupCatchupRepo(t)
	repo, _ := os.Getwd()

	// The pass opens the DB via openDB(), which resolves from the global
	// kaiDir — make sure a graph DB exists there.
	if err := os.MkdirAll(kaiDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := drift.Pin(kaiDir, "refs/heads/main", c1, time.Now()); err != nil {
		t.Fatal(err)
	}
	addCommit(t, repo, "w.go", "package cu\n", "w1")

	var buf bytes.Buffer
	driftCatchUpPass(&buf)

	if !strings.Contains(buf.String(), "caught up 1 commit") {
		t.Errorf("pass output: %q", buf.String())
	}
	rep, err := drift.Compute(repo, kaiDir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Relationship != drift.RelSynced {
		t.Errorf("after pass: %+v", rep)
	}

	// Synced pass is silent (only resyncs the manifest).
	buf.Reset()
	driftCatchUpPass(&buf)
	if buf.Len() != 0 {
		t.Errorf("synced pass should be silent, got %q", buf.String())
	}
	man, err := drift.LoadManifest(kaiDir)
	if err != nil || len(man.Commits) != 0 {
		t.Errorf("manifest after synced pass: %+v, %v", man, err)
	}
}
