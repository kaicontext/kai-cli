package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

func TestSweepIssuesKeepOnlyGroundedBulletsOnChangedFiles(t *testing.T) {
	changed := map[string]bool{"a/b.go": true, "t/b_test.go": true}
	out := `Some preamble the model should not write.
ISSUES:
- a/b.go:12 — compares the email case-sensitively while the insert lowercases it
- t/b_test.go:40 — the test asserts 6 for group_2 but the data packet sends 10
- other.go:3 — not a file in this change
- a/b.go — no line number
- a/b.go:52 — the error is only logged on the error path, so this line is fine — no defect here. (Correction: no defect.)
- (none)`
	got := rcSweepIssues(out, changed)
	if len(got) != 2 || !strings.HasPrefix(got[0], "a/b.go:12") || !strings.HasPrefix(got[1], "t/b_test.go:40") {
		t.Fatalf("issues = %q", got)
	}
}

func TestSweepChunksSkipGeneratedFilesAndPutTestsLast(t *testing.T) {
	order := []string{"pkg/a_test.go", "go.sum", "pkg/a.go", "web/app.min.js", "pkg/b.go"}
	patches := map[string]string{}
	for _, p := range order {
		patches[p] = strings.Repeat("x", 100)
	}
	chunks := rcSweepChunks(order, patches)
	var flat []string
	for _, c := range chunks {
		flat = append(flat, c...)
	}
	if strings.Join(flat, ",") != "pkg/a.go,pkg/b.go,pkg/a_test.go" {
		t.Fatalf("files = %v", flat)
	}
}

func TestDraftWithSweepAppendsNewDefectsOnly(t *testing.T) {
	draft := "Prose.\n\n" + rcReviewDataMarker + "\nINTENT_MATCH: verified\nMERGE_READY: 4\nSUMMARY: ok\nISSUES:\n- a.go:10 — the session user is dereferenced without a nil check after expiry\nDECISIONS:\n- a decision\n"
	got := rcDraftWithSweep(draft, []string{
		"a.go:10 — dereferences the session user after expiry without a nil check", // the same defect, reworded
		"a.go:10 — the log message misspells receivedAt as recievedAt",             // a different defect on that line
		"b.go:3 — a new defect",
	})
	issues := rcIssuesOf(got)
	if len(issues) != 3 || !strings.Contains(issues[0], "session user is dereferenced") || !strings.Contains(issues[1], "recievedAt") || !strings.HasPrefix(issues[2], "b.go:3") {
		t.Fatalf("issues = %q\n%s", issues, got)
	}
	if !strings.Contains(got, "DECISIONS:\n- a decision") {
		t.Fatalf("decisions lost:\n%s", got)
	}
	// A coda with no ISSUES list gets one, before DECISIONS.
	noIssues := "Prose.\n\n" + rcReviewDataMarker + "\nINTENT_MATCH: verified\nMERGE_READY: 5\nSUMMARY: ok\nDECISIONS:\n- d\n"
	if got := rcIssuesOf(rcDraftWithSweep(noIssues, []string{"c.go:1 — x"})); len(got) != 1 {
		t.Fatalf("issues = %q", got)
	}
	// "(none)" is replaced, not kept beside a real issue.
	none := "P\n\n" + rcReviewDataMarker + "\nINTENT_MATCH: verified\nMERGE_READY: 5\nSUMMARY: ok\nISSUES:\n- (none)\n"
	if got := rcDraftWithSweep(none, []string{"c.go:1 — x"}); strings.Contains(got, "(none)") {
		t.Fatalf("(none) kept:\n%s", got)
	}
	// No coda: nothing to merge into.
	if got := rcDraftWithSweep("just prose", []string{"c.go:1 — x"}); got != "just prose" {
		t.Fatalf("draft without a coda changed: %q", got)
	}
}

func TestMergeBatchesConfinesAFailureToItsBatch(t *testing.T) {
	batches := [][]string{{"a.go:1 — one", "a.go:2 — two"}, {"b.go:1 — three"}}
	ok := &rcChallengeResult{
		Allegations: []rcAllegationResult{
			{Issue: "a.go:1 — one", Status: rcStatusSupported, Finding: "one is wrong"},
			{Issue: "a.go:2 — two", Status: rcStatusRefuted},
		},
		Decisions: []rcDecisionResult{{Decision: "d", Status: rcStatusSupported}},
		match:     finding.MatchPartial,
		proposed:  finding.ReadinessMerge,
	}
	res, err := rcMergeBatches(batches, []string{"d"}, []*rcChallengeResult{ok, nil}, []error{nil, errors.New("deadline")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Allegations) != 3 || res.Allegations[2].Status != rcStatusUnresolved || !strings.Contains(res.Allegations[2].Reason, "deadline") {
		t.Fatalf("allegations = %+v", res.Allegations)
	}
	_, issues, decisions, _, readiness, _ := rcParseReviewOutput(res.Review)
	if len(issues) != 1 || len(decisions) != 1 || readiness > finding.ReadinessSmallFixes {
		t.Fatalf("issues=%q decisions=%q readiness=%d\n%s", issues, decisions, readiness, res.Review)
	}
	if !strings.Contains(res.Review, "## Could not verify") {
		t.Fatalf("the failed batch's allegation is not listed:\n%s", res.Review)
	}
	// Every batch failing withholds the draft, as a single failed check does.
	if _, err := rcMergeBatches(batches, nil, []*rcChallengeResult{nil, nil}, []error{errors.New("x"), errors.New("y")}); err == nil {
		t.Fatal("all batches failed but a review was assembled")
	}
}

func TestSplitDiffKeysPatchesByNewPath(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/gone.go b/gone.go\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-c\n"
	order, patches := rcSplitDiff(diff)
	if strings.Join(order, ",") != "x.go,gone.go" || !strings.Contains(patches["x.go"], "+b") || !strings.Contains(patches["gone.go"], "-c") {
		t.Fatalf("order=%v patches=%v", order, patches)
	}
}

// Without the first batch — the one that saw the decisions — the intent
// verdict must not be the most optimistic survivor's.
func TestMergeBatchesTakesTheWorstIntentWhenTheFirstBatchFails(t *testing.T) {
	batches := [][]string{{"a.go:1 — one"}, {"b.go:1 — two"}, {"c.go:1 — three"}}
	verified := &rcChallengeResult{Allegations: []rcAllegationResult{{Issue: "b.go:1 — two", Status: rcStatusRefuted}}, match: finding.MatchVerified, proposed: finding.ReadinessMerge}
	partial := &rcChallengeResult{Allegations: []rcAllegationResult{{Issue: "c.go:1 — three", Status: rcStatusRefuted}}, match: finding.MatchPartial, proposed: finding.ReadinessMerge}
	res, err := rcMergeBatches(batches, []string{"d"}, []*rcChallengeResult{nil, verified, partial}, []error{errors.New("timeout"), nil, nil})
	if err != nil {
		t.Fatal(err)
	}
	if res.match != finding.MatchPartial {
		t.Fatalf("match = %q, want the least favourable surviving verdict", res.match)
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Status != rcStatusUnresolved {
		t.Fatalf("decisions = %+v", res.Decisions)
	}
}

func TestChallengeModelOverride(t *testing.T) {
	t.Setenv("KAI_CHALLENGE_MODEL", "")
	if got := rcChallengeModel("z-ai/glm-5.2"); got != "z-ai/glm-5.2" {
		t.Fatalf("unset: %q, want the review model", got)
	}
	t.Setenv("KAI_CHALLENGE_MODEL", " openai/gpt-5.4-mini ")
	if got := rcChallengeModel("z-ai/glm-5.2"); got != "openai/gpt-5.4-mini" {
		t.Fatalf("set: %q", got)
	}
}

// An unsettled sweep proposal is withheld; the reviewer's own unsettled point
// is still listed under "Could not verify".
func TestUnsettledSweepProposalsAreWithheld(t *testing.T) {
	res := &rcChallengeResult{
		Allegations: []rcAllegationResult{
			{ID: 1, Issue: "a.go:1 — reviewer's own doubt", Status: rcStatusUnresolved, Reason: "no source"},
			{ID: 2, Issue: "b.go:2 — sweep guess", Status: rcStatusUnresolved, Reason: "the challenge did not assess this allegation"},
			{ID: 3, Issue: "c.go:3 — sweep hit", Status: rcStatusSupported, Finding: "c is wrong"},
		},
		match: finding.MatchPartial, proposed: finding.ReadinessSmallFixes,
	}
	if n := rcWithholdUnsettledSweep(res, []string{"`b.go:2 — sweep guess`", "c.go:3 — sweep hit"}); n != 1 {
		t.Fatalf("withheld %d, want 1", n)
	}
	cnv := couldNotVerifySection(res.Review)
	if !strings.Contains(cnv, "reviewer's own doubt") || strings.Contains(res.Review, "sweep guess") {
		t.Fatalf("review:\n%s", res.Review)
	}
	if !strings.Contains(res.Review, "c.go:3 — sweep hit") {
		t.Fatalf("the supported sweep finding was lost:\n%s", res.Review)
	}
}
