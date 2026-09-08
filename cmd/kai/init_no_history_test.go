package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
)

// initTestRepo builds a throwaway git repo with n commits and chdirs into it.
func initTestRepo(t *testing.T, commits int) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "--quiet")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test")
	for i := 0; i < commits; i++ {
		name := filepath.Join(dir, "f"+string(rune('a'+i))+".go")
		if err := os.WriteFile(name, []byte("package main\n\nfunc F"+string(rune('a'+i))+"() {}\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		run("git", "add", "-A")
		run("git", "commit", "--quiet", "-m", "commit")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	return dir
}

// snapshotCount reports how many Snapshot nodes the repo's graph holds.
func snapshotCount(t *testing.T, dir string) int {
	t.Helper()
	db, err := graph.Open(filepath.Join(dir, kaiDir, dbFile), filepath.Join(dir, kaiDir, objectsDir))
	if err != nil {
		t.Fatalf("open graph: %v", err)
	}
	defer db.Close()
	nodes, err := db.GetNodesByKind(graph.KindSnapshot)
	if err != nil {
		t.Fatalf("nodes by kind: %v", err)
	}
	return len(nodes)
}

// runInitInRepo runs a local-only init in the current directory.
func runInitInRepo(t *testing.T, noHistory bool) {
	t.Helper()
	oldKaiDir, oldRemote, oldYes, oldHist := kaiDir, initNoRemote, initAssumeYes, initNoHistory
	kaiDir = ".kai"
	initNoRemote, initAssumeYes, initNoHistory = true, true, noHistory
	t.Cleanup(func() {
		kaiDir, initNoRemote, initAssumeYes, initNoHistory = oldKaiDir, oldRemote, oldYes, oldHist
	})
	if err := runInit(nil, nil); err != nil {
		t.Fatalf("runInit(noHistory=%v): %v", noHistory, err)
	}
}

// The history import is nearly all of init's wall clock and a review pod never
// reads what it produces, so --no-history must actually skip it — and must
// still leave the working tree's own graph behind, which is the thing the
// review does read.
func TestInitNoHistorySkipsTheImport(t *testing.T) {
	dir := initTestRepo(t, 3)
	runInitInRepo(t, true)

	if got := snapshotCount(t, dir); got != 1 {
		t.Fatalf("--no-history should leave only the working-tree capture, got %d snapshots", got)
	}
}

// The default is unchanged: a person running `kai init` still gets the history
// that makes log, blame and bisect worth having.
func TestInitImportsHistoryByDefault(t *testing.T) {
	dir := initTestRepo(t, 3)
	runInitInRepo(t, false)

	if got := snapshotCount(t, dir); got <= 1 {
		t.Fatalf("default init should import history, got %d snapshots", got)
	}
}
