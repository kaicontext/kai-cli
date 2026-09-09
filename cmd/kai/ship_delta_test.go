package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The checkout has an uncommitted edit; the spawn's baseline is a copy of
// that working tree; the agent adds a line elsewhere in the same file.
// What ships is HEAD's file plus the agent's line — the checkout's
// uncommitted edit stays home.
func TestShipContentFor_StripsCheckoutEdits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	base := "line1\nline2\nline3\nline4\nline5\nline6\n"
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "init", "-q")
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "base")
	baseSHA := gitIn(t, src, "rev-parse", "HEAD")
	// The user's uncommitted edit: line1 changes.
	userTree := strings.Replace(base, "line1\n", "line1 USER-EDIT\n", 1)
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte(userTree), 0o644); err != nil {
		t.Fatal(err)
	}
	// The spawn: a copy of the working tree, committed as the baseline.
	spawn := t.TempDir()
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(userTree), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, spawn, "init", "-q")
	gitIn(t, spawn, "add", "-A")
	gitIn(t, spawn, "commit", "-q", "-m", "kai spawn from abcdef123456")
	// The agent's edit: a new line at the end.
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(userTree+"line7 AGENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &spawnpkg.Entry{Path: spawn, SourceRepo: src, BaseGitSHA: baseSHA}
	var overlaps []string
	got, err := shipContentFor(spawn, e, shipBaselineCommit(spawn), "app.js", &overlaps)
	if err != nil {
		t.Fatal(err)
	}
	if want := base + "line7 AGENT\n"; string(got) != want {
		t.Fatalf("shipped content should be HEAD + the agent's line:\n%s", got)
	}
	if len(overlaps) != 0 {
		t.Fatalf("no overlap expected: %v", overlaps)
	}

	// Overlap: the agent edits the very line the user edited — the full
	// file ships and the file is reported.
	if err := os.WriteFile(filepath.Join(spawn, "app.js"), []byte(strings.Replace(userTree, "line1 USER-EDIT\n", "line1 USER-EDIT AGENT\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	overlaps = nil
	got, _ = shipContentFor(spawn, e, shipBaselineCommit(spawn), "app.js", &overlaps)
	if !strings.Contains(string(got), "USER-EDIT AGENT") || len(overlaps) != 1 {
		t.Fatalf("overlap should ship the full file and be reported: %q %v", got, overlaps)
	}

	// A warm sync after the baseline moves the baseline to that commit.
	if err := os.WriteFile(filepath.Join(spawn, "b.txt"), []byte("synced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, spawn, "add", "-A")
	gitIn(t, spawn, "commit", "-q", "-m", "kai warm sync")
	if bl := shipBaselineCommit(spawn); bl != gitIn(t, spawn, "rev-parse", "HEAD") {
		t.Fatalf("baseline should be the warm-sync commit, got %s", bl)
	}
}
