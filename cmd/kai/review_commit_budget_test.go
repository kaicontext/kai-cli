package main

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

// The turn budget is the one that binds, so it has to be the one that scales.
//
// A flat 20 was set on 2026-07-07 and never revisited. Measured across 85 live
// reviews: median 90 seconds against a 540-second soft budget, 6% coming near
// it, and on PRs of nine files or more 8.4 changed files left unopened. Twenty
// turns simply cannot read a twelve-file change and still sweep it.
func TestReviewMaxTurnsScalesWithTheChange(t *testing.T) {
	if got := rcReviewMaxTurns(1); got < rcReviewBaseTurns {
		t.Errorf("a one-file review must not get less room than before: got %d, want >= %d", got, rcReviewBaseTurns)
	}
	if got := rcReviewMaxTurns(0); got != rcReviewBaseTurns {
		t.Errorf("an empty diff should get the base budget: got %d, want %d", got, rcReviewBaseTurns)
	}
	twelve, three := rcReviewMaxTurns(12), rcReviewMaxTurns(3)
	if twelve <= three {
		t.Errorf("a twelve-file review must get more turns than a three-file one: %d vs %d", twelve, three)
	}
	// The ceiling exists so the WALL CLOCK becomes the binding limit again.
	// At the observed 9.3s/turn the cap must stay inside the 9-minute soft
	// budget, or this change trades a turn-starved review for a timed-out one.
	if secs := float64(rcReviewMaxTurns(500)) * 9.3; secs >= rcReviewSoftBudget.Seconds() {
		t.Errorf("the turn ceiling costs %.0fs at the measured pace, which is past the %.0fs soft budget",
			secs, rcReviewSoftBudget.Seconds())
	}
}

func TestDiffPathsListsWhatTheChangeTouches(t *testing.T) {
	diff := `diff --git a/api/ci.go b/api/ci.go
--- a/api/ci.go
+++ b/api/ci.go
@@ -1,3 +1,4 @@
+// new
diff --git a/db/secrets.go b/db/secrets.go
--- a/db/secrets.go
+++ b/db/secrets.go
@@ -9,2 +9,3 @@
+x
diff --git a/gone.go b/gone.go
--- a/gone.go
+++ /dev/null
`
	got := rcDiffPaths(diff)
	want := []string{"api/ci.go", "db/secrets.go"}
	if len(got) != len(want) {
		t.Fatalf("rcDiffPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rcDiffPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The gate's whole job: name the changed files the run never opened. This is
// the same comparison the server renders under the coverage manifest, so the
// two can never disagree about which files were skipped.
func TestUnopenedChangedNamesTheSkippedFiles(t *testing.T) {
	changed := []string{"api/ci.go", "db/secrets.go", "cmd/kai/do_budget.go"}
	read := []string{"api/ci.go", "/tmp/tmp.aBc/db/secrets.go"}
	got := rcUnopenedChanged(changed, read)
	if len(got) != 1 || got[0] != "cmd/kai/do_budget.go" {
		t.Errorf("rcUnopenedChanged = %v, want [cmd/kai/do_budget.go]", got)
	}
	if n := len(rcUnopenedChanged(changed, changed)); n != 0 {
		t.Errorf("a run that opened everything has nothing to answer for, got %d", n)
	}
	if n := len(rcUnopenedChanged(nil, nil)); n != 0 {
		t.Errorf("no diff, no gate; got %d", n)
	}
}

// kai-tui#108 is the specimen: the manifest said "2 of the 4 changed files
// don't appear below: do_budget.go, do_budget_test.go", and the defect a
// competitor found was in do_budget.go. The prompt must name the file rather
// than ask the model to work out what it skipped.
func TestCoverageGatePromptNamesTheFilesAndAsksForTheWholeReview(t *testing.T) {
	got := rcCoverageGatePrompt([]string{"cmd/kit/do_budget.go", "cmd/kit/do_budget_test.go"})
	for _, want := range []string{
		"cmd/kit/do_budget.go",
		"cmd/kit/do_budget_test.go",
		"did not open",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("gate prompt is missing %q:\n%s", want, got)
		}
	}
	// The coda is what the pipeline parses; a supplement would have to be
	// merged with the first answer by hand.
	if !strings.Contains(got, "in full") || !strings.Contains(got, "coda") {
		t.Errorf("the gate must ask for the whole review again, with its coda:\n%s", got)
	}
}

func TestCoverageGateTurnsStayBounded(t *testing.T) {
	if rcCoverageGateTurns(1) < 2 {
		t.Error("one skipped file still needs a turn to read and a turn to answer")
	}
	if got := rcCoverageGateTurns(400); got > 16 {
		t.Errorf("the gate is a patch, not a second review: got %d turns", got)
	}
	if rcCoverageGateTurns(6) <= rcCoverageGateTurns(2) {
		t.Error("more skipped files need more turns")
	}
}

// The manifest published len(transcript) as "turns". That is the MESSAGE
// count — prompt, assistant turn, tool result — so every review told its
// reader it had run about twice as long as it had.
func TestTurnsCountsTurnsNotMessages(t *testing.T) {
	tr := []message.Message{
		{Role: message.RoleUser},
		{Role: message.RoleAssistant},
		{Role: message.RoleTool},
		{Role: message.RoleAssistant},
		{Role: message.RoleTool},
		{Role: message.RoleAssistant},
	}
	if got := rcTurns(tr); got != 3 {
		t.Errorf("rcTurns = %d, want 3 (got the message count %d?)", got, len(tr))
	}
}

func TestMergeFilesReadUnionsWithoutRepeats(t *testing.T) {
	got := rcMergeFilesRead([]string{"b.go", "a.go"}, []string{"a.go", "c.go", ""})
	if len(got) != 3 {
		t.Fatalf("rcMergeFilesRead = %v, want three distinct paths", got)
	}
	for i, want := range []string{"a.go", "b.go", "c.go"} {
		if got[i] != want {
			t.Errorf("rcMergeFilesRead[%d] = %q, want %q", i, got[i], want)
		}
	}
}
