package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/gitio"
	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// TestShipPromote_StripsCheckoutEdits verifies the delta-stripping logic
// works end-to-end through promote: the checkout had an uncommitted edit,
// the spawn's baseline is a copy of that dirty tree, the agent adds a line
// — promote ships HEAD's file plus the agent's line, leaving the checkout
// edit behind, as a new branch + commit in the source repo.
func TestShipPromote_StripsCheckoutEdits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	base := "line1\nline2\nline3\nline4\n"
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "init", "-q")
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "base")
	baseSHA := gitIn(t, src, "rev-parse", "HEAD")

	// The user's uncommitted edit in src (NOT committed yet).
	userTree := strings.Replace(base, "line1\n", "line1 USER-EDIT\n", 1)
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte(userTree), 0o644); err != nil {
		t.Fatal(err)
	}

	// The spawn: a copy of src's working tree (WITH the uncommitted edit),
	// committed as the baseline.
	spawn := t.TempDir()
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(userTree), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, spawn, "init", "-q")
	gitIn(t, spawn, "add", "-A")
	gitIn(t, spawn, "commit", "-q", "-m", "kai spawn from test123")

	// The agent's edit in spawn: a new line at the end.
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(userTree+"line5 AGENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Commit the user's edit in src so src is clean.
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "user edit")

	// Isolate the spawn registry so this test doesn't touch the host's.
	t.Setenv("HOME", t.TempDir())
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SourceRepo: src, BaseGitSHA: baseSHA, SessionID: "sid-1"}); err != nil {
		t.Fatal(err)
	}

	shipPromote = true
	shipPush = false
	defer func() { shipPromote = false; shipPush = true }()

	if err := runShipPromote(spawn, "kai/promote-test", "sid-1"); err != nil {
		t.Fatalf("runShipPromote: %v", err)
	}

	if got, err := gitio.CurrentBranch(src); err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	} else if got != "kai/promote-test" {
		t.Fatalf("src should be on kai/promote-test, got %s", got)
	}

	want := "line1\nline2\nline3\nline4\nline5 AGENT\n"
	// gitIn trims its output, so read the committed blob directly to
	// preserve the trailing newline.
	blobSHA := gitIn(t, src, "rev-parse", "HEAD:app.js")
	out, err := exec.Command("git", "-C", src, "cat-file", "-p", blobSHA).Output()
	if err != nil {
		t.Fatalf("cat-file: %v", err)
	}
	if got := string(out); got != want {
		t.Fatalf("promoted app.js:\nwant %q\ngot  %q", want, got)
	}
}

// TestShipPromote_RefusesDirtySourceRepo verifies promote refuses to
// clobber uncommitted work in the source repo before any mutation.
func TestShipPromote_RefusesDirtySourceRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	base := "line1\nline2\nline3\nline4\n"
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "init", "-q")
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "base")
	baseSHA := gitIn(t, src, "rev-parse", "HEAD")

	// Make src dirty (uncommitted edit).
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte(base+"DIRTY\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A spawn with a committed delta to promote.
	spawn := t.TempDir()
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(base+"AGENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, spawn, "init", "-q")
	gitIn(t, spawn, "add", "-A")
	gitIn(t, spawn, "commit", "-q", "-m", "kai spawn from test123")

	t.Setenv("HOME", t.TempDir())
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SourceRepo: src, BaseGitSHA: baseSHA, SessionID: "sid-2"}); err != nil {
		t.Fatal(err)
	}

	originalBranch, err := gitio.CurrentBranch(src)
	if err != nil {
		t.Fatal(err)
	}

	shipPromote = true
	shipPush = false
	defer func() { shipPromote = false; shipPush = true }()

	err = runShipPromote(spawn, "kai/promote-test", "sid-2")
	if err == nil {
		t.Fatal("expected an error from a dirty source repo, got nil")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error should mention uncommitted changes, got: %v", err)
	}

	if got, err := gitio.CurrentBranch(src); err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	} else if got != originalBranch {
		t.Fatalf("src should still be on %s, got %s — promote mutated the source repo despite refusing", originalBranch, got)
	}
}

// TestShipPromote_MutexWithServer verifies --promote and --server are
// mutually exclusive. The guard sits before shipUseServer resolves so
// runShip returns the mutex error rather than routing into runShipServer.
func TestShipPromote_MutexWithServer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	spawn := t.TempDir()
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, spawn, "init", "-q")
	gitIn(t, spawn, "add", "-A")
	gitIn(t, spawn, "commit", "-q", "-m", "kai spawn from test123")

	t.Setenv("HOME", t.TempDir())
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SourceRepo: t.TempDir(), SessionID: "sid-3"}); err != nil {
		t.Fatal(err)
	}

	shipPromote = true
	shipServer = true
	defer func() { shipPromote = false; shipServer = false }()

	// runShip starts with os.Getwd(); chdir into the spawn so it resolves
	// as a registered spawn. Restore the original dir afterwards.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(spawn); err != nil {
		t.Fatal(err)
	}

	err = runShip(nil, nil)
	if err == nil {
		t.Fatal("expected a mutual-exclusion error, got nil")
	}
	if !strings.Contains(err.Error(), "--promote") || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("error should mention both --promote and --server, got: %v", err)
	}
}

// TestShipPromote_RoutesThroughRunShip verifies the routing fix: a bare
// `kai ship --promote` inside a spawned workspace (the flag's only valid
// use case) reaches runShipPromote, NOT runShipServer. A spawn with no
// explicit --server/--local defaults useServer=true (shipUseServer returns
// true because shipSpawnEntry(cwd) != nil), so the old code returned from
// the useServer arm before the promote check — silently routing --promote
// to the server path. The fix short-circuits promote before shipUseServer.
//
// The assertion is behavioral: the promote path creates and checks out a
// kai/ branch in the SOURCE repo; the server path never touches the source
// repo's working tree. So if runShip leaves src on a kai/ branch, it routed
// through runShipPromote. On the unfixed code, runShip routes to
// runShipServer, which returns a not-logged-in error and leaves src on its
// original branch — the test fails.
func TestShipPromote_RoutesThroughRunShip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	base := "line1\nline2\nline3\nline4\n"
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "init", "-q")
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "base")
	baseSHA := gitIn(t, src, "rev-parse", "HEAD")

	// The spawn: baseline matches src, then the agent's edit sits as a
	// dirty working-tree change (the delta shipDeltaNames measures
	// against the "kai spawn from" baseline commit).
	spawn := t.TempDir()
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, spawn, "init", "-q")
	gitIn(t, spawn, "add", "-A")
	gitIn(t, spawn, "commit", "-q", "-m", "kai spawn from test123")
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(base+"line5 AGENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", t.TempDir())
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SourceRepo: src, BaseGitSHA: baseSHA, SessionID: "sid-route"}); err != nil {
		t.Fatal(err)
	}

	// The default flags a bare `kai ship --promote` would have: promote on,
	// no explicit --server/--local, push on by default (set false so no push).
	shipPromote = true
	shipServer = false
	shipLocal = false
	shipPush = false
	shipBranch = "kai/route-test"
	defer func() {
		shipPromote = false
		shipServer = false
		shipLocal = false
		shipPush = true
		shipBranch = ""
	}()

	// runShip starts with os.Getwd(); chdir into the spawn so it resolves
	// as a registered spawn (same pattern as TestShipPromote_MutexWithServer).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(spawn); err != nil {
		t.Fatal(err)
	}

	if err := runShip(nil, nil); err != nil {
		t.Fatalf("runShip routed to a path that errored (expected runShipPromote to succeed): %v", err)
	}

	// The promote path checks out the kai/ branch in the source repo.
	// runShipServer never touches the source repo's working tree, so this
	// assertion distinguishes the two routes.
	got, err := gitio.CurrentBranch(src)
	if err != nil {
		t.Fatalf("CurrentBranch(src): %v", err)
	}
	if got != "kai/route-test" {
		t.Fatalf("runShip did not route through runShipPromote: src should be on kai/route-test, got %s", got)
	}
}