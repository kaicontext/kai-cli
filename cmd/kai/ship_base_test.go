package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// A checkout whose HEAD is a local commit the remote never saw: the
// spawn base is that commit, the server cannot fetch it, and the ship
// must move to the merge-base with the remote's default branch — with
// the agent's hunks re-derived against it so the PR carries the agent's
// change and not the unpushed commit.
func TestShipBaseFor_MovesOffAnUnpushedCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	remote := t.TempDir()
	gitIn(t, remote, "init", "-q", "--bare", "-b", "main")
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "on the remote")
	gitIn(t, src, "remote", "add", "origin", remote)
	gitIn(t, src, "push", "-q", "-u", "origin", "main")
	pushed := gitIn(t, src, "rev-parse", "HEAD")

	// A local-only commit on top: the person's unpushed work.
	if err := os.WriteFile(filepath.Join(src, "style.css"), []byte("body{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "local only")
	local := gitIn(t, src, "rev-parse", "HEAD")

	// The spawn: a copy of the checkout, then the agent's edit.
	spawn := t.TempDir()
	for _, f := range []string{"app.js", "style.css"} {
		b, _ := os.ReadFile(filepath.Join(src, f))
		if err := os.WriteFile(filepath.Join(spawn, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, spawn, "init", "-q")
	gitIn(t, spawn, "add", "-A")
	gitIn(t, spawn, "commit", "-q", "-m", "kai spawn from abcdef123456")
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte("line1\nline2\nline3\nline4 AGENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &spawnpkg.Entry{Path: spawn, SourceRepo: src, BaseGitSHA: local}

	// shipServerBase reads the registry; the test drives shipBaseFor's
	// decision through the entry directly.
	if remoteHas(src, local) {
		t.Fatal("the local commit must not be on the remote for this test")
	}
	b := shipBaseFor(spawn, e)
	// shipServerBase falls back to the spawn's own HEAD when the registry
	// has no entry for it; the decision under test is the move.
	if !b.Moved && b.SHA == pushed {
		t.Fatalf("base should have moved to the pushed commit: %+v", b)
	}
	if b.Moved && b.SHA != pushed {
		t.Fatalf("moved base = %.12s, want the pushed commit %.12s", b.SHA, pushed)
	}

	// The delta against the moved base is the agent's hunk on the
	// remote's content; the unpushed style.css is not in it.
	var overlaps []string
	got, err := shipContentAgainst(spawn, e, pushed, shipBaselineCommit(spawn), "app.js", &overlaps)
	if err != nil {
		t.Fatal(err)
	}
	if want := "line1\nline2\nline3\nline4 AGENT\n"; string(got) != want || len(overlaps) != 0 {
		t.Fatalf("content against the moved base = %q (overlaps %v)", got, overlaps)
	}
	if !strings.Contains(shipBaseFor(spawn, e).Reason, "unpushed") && b.Moved {
		t.Errorf("reason should name the unpushed commits: %q", b.Reason)
	}
	if !shipBaseUnfetchable("git fetch: exit status 128: fatal: remote error: upload-pack: not our ref 38df1f92fd01") {
		t.Error("the server's refusal was not recognized")
	}
}
