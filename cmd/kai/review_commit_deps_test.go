package main

import (
	"strings"
	"testing"
)

// The TUI#92 diff, reduced to the parts that matter. That review spent most of
// its output speculating about kaipath.UserPath — a 17-line function in another
// module — and escalated one guess to "a real defect". The commit holding the
// answer was sitting in the pseudo-version below the whole time.
const tui92Diff = `diff --git a/go.mod b/go.mod
index 1111111..2222222 100644
--- a/go.mod
+++ b/go.mod
@@ -67,7 +67,7 @@ require (
 	github.com/charmbracelet/bubbletea/v2 v2.0.0
-	github.com/kaicontext/kai-engine v0.6.58
+	github.com/kaicontext/kai-engine v0.6.59-0.20260908192613-5ab1b102fc2f
 	github.com/spf13/cobra v1.8.0
diff --git a/go.sum b/go.sum
--- a/go.sum
+++ b/go.sum
+github.com/kaicontext/kai-engine v0.6.59-0.20260908192613-5ab1b102fc2f h1:deadbeef=
diff --git a/internal/tui/app.go b/internal/tui/app.go
--- a/internal/tui/app.go
+++ b/internal/tui/app.go
@@ -12,6 +12,7 @@ import (
 	"os"
+	"github.com/kaicontext/kai-engine/kaipath"
 )
@@ -1360,7 +1361,7 @@ func logTUIPanic() {
-	dir := filepath.Join(home, ".kai")
+	dir := kaipath.UserPath(home)
`

func TestChangedDepsFindsThePinnedCommit(t *testing.T) {
	deps := rcChangedDeps(tui92Diff)
	if len(deps) != 1 {
		t.Fatalf("got %d deps, want exactly kai-engine: %+v", len(deps), deps)
	}
	d := deps[0]
	if d.Module != "github.com/kaicontext/kai-engine" {
		t.Errorf("Module = %q", d.Module)
	}
	if d.From != "v0.6.58" || d.To != "v0.6.59-0.20260908192613-5ab1b102fc2f" {
		t.Errorf("versions = %q -> %q", d.From, d.To)
	}
	// The whole point: the source the reviewer needed is identified by a
	// commit the diff already carried.
	if d.Commit != "5ab1b102fc2f" {
		t.Errorf("Commit = %q, want the pseudo-version's commit", d.Commit)
	}
	if len(d.Pkgs) != 1 || d.Pkgs[0] != "kaipath" {
		t.Errorf("Pkgs = %v, want [kaipath]", d.Pkgs)
	}
}

// go.sum restates every version and would double each entry while adding
// nothing. Only go.mod is read.
func TestChangedDepsReadsOnlyGoMod(t *testing.T) {
	onlySum := `diff --git a/go.sum b/go.sum
--- a/go.sum
+++ b/go.sum
+github.com/kaicontext/kai-engine v0.6.59 h1:deadbeef=
`
	if deps := rcChangedDeps(onlySum); len(deps) != 0 {
		t.Errorf("go.sum alone produced %+v, want nothing", deps)
	}
}

// A module that only appears on "-" lines was removed, and a removed
// dependency is not a contract this change rests on.
func TestChangedDepsSkipsRemovedAndUnchanged(t *testing.T) {
	d := `diff --git a/go.mod b/go.mod
--- a/go.mod
+++ b/go.mod
-	github.com/old/gone v1.0.0
-	github.com/same/pinned v2.0.0
+	github.com/same/pinned v2.0.0
`
	if deps := rcChangedDeps(d); len(deps) != 0 {
		t.Errorf("got %+v, want nothing: one removal and one no-op", deps)
	}
}

func TestPseudoCommitOnlyAcceptsARealOne(t *testing.T) {
	if got := rcPseudoCommit("v0.6.59-0.20260908192613-5ab1b102fc2f"); got != "5ab1b102fc2f" {
		t.Errorf("pseudo-version commit = %q", got)
	}
	for _, v := range []string{"v0.6.58", "v1.2.3-rc1", "v0.0.0-20260101010101-nothexdigits"} {
		if got := rcPseudoCommit(v); got != "" {
			t.Errorf("rcPseudoCommit(%q) = %q, want empty", v, got)
		}
	}
}

// The instruction is the load-bearing half. Without it the reviewer produces a
// concern per call site, each genuinely unverified, and a reader cannot tell
// that speculation from the findings around it.
func TestDepLimitsBlockSaysItOnceOrNotAtAll(t *testing.T) {
	if got := rcDepLimitsBlock(nil); got != "" {
		t.Errorf("no dependency changes should print nothing, got %q", got)
	}
	// Matched against whitespace-collapsed text: these assert what the block
	// SAYS, and should not break the day a sentence rewraps.
	got := strings.Join(strings.Fields(rcDepLimitsBlock(rcChangedDeps(tui92Diff))), " ")
	for _, want := range []string{
		"github.com/kaicontext/kai-engine",
		"v0.6.58 -> v0.6.59-0.20260908192613-5ab1b102fc2f",
		"commit 5ab1b102fc2f",
		"imported here: kaipath",
		"LIMITATION, not a defect",
		"Say it ONCE",
		"do not call it a defect",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q:\n%s", want, got)
		}
	}
}
