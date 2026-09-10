package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/finding"

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
	if secs := float64(rcReviewMaxTurns(500)) * rcObservedSecondsPerTurn; secs >= rcReviewSoftBudget.Seconds() {
		t.Errorf("the turn ceiling costs %.0fs at %.1fs/turn, which is past the %.0fs soft budget — "+
			"re-measure the pace or lower the ceiling",
			secs, rcObservedSecondsPerTurn, rcReviewSoftBudget.Seconds())
	}
}

// Changed-file discovery must not read the PROMPT's diff. That text is
// truncated at maxReviewCommitDiffBytes, a deletion's hunk ends at
// `+++ /dev/null`, and a binary or mode-only change carries no `+++` header at
// all — so a `+++ b/` scan silently omits files on exactly the large changes
// the turn budget and the coverage gate exist for. The diff STAT has none of
// those holes, and it is also what the server compares the manifest against.
func TestChangedPathsComeFromTheDiffStat(t *testing.T) {
	files := []finding.DiffFile{
		{Path: "api/ci.go", Action: "modified"},
		{Path: "gone.go", Action: "removed"},          // no `+++ b/` header at all
		{Path: "assets/logo.png", Action: "modified"}, // binary: no hunks
		{Path: "api/ci.go", Action: "modified"},       // duplicate
		{Path: "  ", Action: "modified"},              // junk
	}
	got := rcPathsOf(files)
	want := []string{"api/ci.go", "gone.go", "assets/logo.png"}
	if len(got) != len(want) {
		t.Fatalf("rcPathsOf = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rcPathsOf[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// One answer may only replace another when it is a WHOLE review. A pass that
// ran out of turns mid-write emits the marker and stops; swapping that in
// would turn a complete first review into an incomplete finding.
func TestUsableCodaNeedsMoreThanTheMarker(t *testing.T) {
	if rcUsableCoda("Looks fine, nothing to add.") {
		t.Error("prose with no coda is not a review the pipeline can read")
	}
	if rcUsableCoda("Ran out of road.\n\n" + rcReviewDataMarker) {
		t.Error("the marker alone must not count — that is a pass that stopped mid-write")
	}
	if rcUsableCoda(rcReviewDataMarker + "\nISSUES:\n- a.go:1 — x") {
		t.Error("a coda with neither INTENT_MATCH nor SUMMARY is not parseable")
	}
	if !rcUsableCoda("Prose.\n\n" + rcReviewDataMarker + "\nINTENT_MATCH: verified\nSUMMARY: ok\n") {
		t.Error("a complete coda must be usable")
	}
	if !rcUsableCoda("Prose.\n\n" + rcReviewDataMarker + "\nSUMMARY: ok\n") {
		t.Error("SUMMARY alone is enough for the parser")
	}
}

// The gate runs after a review that already has an answer, so it never gets to
// be the thing that makes the run late. It must not inherit a hard deadline
// that has been ticking since the first pass started.
func TestGateHeadroomNeverOutlivesTheReview(t *testing.T) {
	now := time.Now()
	if got := rcGateHeadroom(now); got != rcGateMaxBudget {
		t.Errorf("a review that just started should give the gate its full budget, got %s", got)
	}
	// A first pass that spent almost the whole hard deadline leaves nothing
	// worth asking for — and asking anyway is how a clean first pass ends up
	// reported as one that ran out of time.
	if got := rcGateHeadroom(now.Add(-rcReviewHardDeadline + 10*time.Second)); got != 0 {
		t.Errorf("headroom = %s, want 0 — too little clock to start a second pass", got)
	}
	if got := rcGateHeadroom(now.Add(-rcReviewHardDeadline)); got != 0 {
		t.Errorf("an exhausted deadline must yield no gate, got %s", got)
	}
	mid := rcGateHeadroom(now.Add(-rcReviewHardDeadline + 2*time.Minute))
	if mid <= 0 || mid > rcGateMaxBudget {
		t.Errorf("headroom = %s, want a bounded positive budget", mid)
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

// Two changed files sharing a tail, only the shallower one opened. The suffix
// match must not let the deeper one out of the gate.
//
// The mirror test — "the changed path ends in the read path" — declared
// `pkg/db/secrets.go` opened because `db/secrets.go` had been, and dropped the
// one file the gate exists to name. Silent, and not exotic in a repo with
// parallel package trees.
func TestUnopenedChangedIsNotFooledByASharedTail(t *testing.T) {
	changed := []string{"db/secrets.go", "pkg/db/secrets.go"}
	read := []string{"db/secrets.go"}
	got := rcUnopenedChanged(changed, read)
	if len(got) != 1 || got[0] != "pkg/db/secrets.go" {
		t.Errorf("rcUnopenedChanged = %v, want [pkg/db/secrets.go] — a shared tail is not a read", got)
	}

	// The direction that IS sound: the run works in a mktemp checkout, so a
	// manifest entry is often the absolute path of a changed file.
	abs := rcUnopenedChanged([]string{"db/secrets.go"}, []string{"/tmp/tmp.aBc123/db/secrets.go"})
	if len(abs) != 0 {
		t.Errorf("an absolute path to the changed file counts as opened, got %v", abs)
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

// AUTHOR CONTEXT is what the reviewer tests the code against, and it has always
// been built from commit messages. On a pull request that is the wrong
// document — it is what the author wrote about a commit, not about the change.
func TestPullRequestDescriptionLeadsTheAuthorContext(t *testing.T) {
	t.Setenv("KAI_PR_TITLE", "Seats pool into one org bucket")
	t.Setenv("KAI_PR_BODY", "Three $50 seats become one $150/month bucket.")
	commits := "db: rekey daily_usage\n\nthe migration takes ACCESS EXCLUSIVE"

	got := rcWithPRDescription(commits)
	for _, want := range []string{
		"Seats pool into one org bucket",
		"one $150/month bucket",
		"db: rekey daily_usage",
		"ACCESS EXCLUSIVE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("author context is missing %q:\n%s", want, got)
		}
	}
	// The claim being tested comes first; the commits are supporting detail,
	// so truncation drops them rather than the goal.
	if strings.Index(got, "$150/month") > strings.Index(got, "db: rekey") {
		t.Error("the pull request description must lead, not trail the commits")
	}
}

// A local review-commit, a push that is not a pull request, or an older
// control plane sets neither variable, and the author context must be exactly
// what it is today.
func TestNoPullRequestDescriptionChangesNothing(t *testing.T) {
	t.Setenv("KAI_PR_TITLE", "")
	t.Setenv("KAI_PR_BODY", "")
	commits := "Subj\n\nBody"
	if got := rcWithPRDescription(commits); got != commits {
		t.Errorf("rcWithPRDescription = %q, want the commits unchanged", got)
	}
}

// A long description must not crowd the commits out of the context entirely.
func TestPullRequestDescriptionIsBounded(t *testing.T) {
	t.Setenv("KAI_PR_TITLE", "big")
	t.Setenv("KAI_PR_BODY", strings.Repeat("x", rcMaxPRDescriptionBytes*3))
	got := rcWithPRDescription("db: a commit that must survive")
	if !strings.Contains(got, "db: a commit that must survive") {
		t.Error("the commits were crowded out by the description")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("a truncated description must say so")
	}
}
