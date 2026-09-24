package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

// The review of kaicontext/kai-server#302 (finding rc-f04ff39763255f72,
// 2026-09-24) is the specimen these tests are cut from. Its challenger was
// handed three allegations and, holding only supported/refuted/unverified,
// marked all three supported — two of them with the remedy "No fix needed".
// The status is what every reader acts on, so both notes became 🐞 comments on
// the diff and two thirds of a 3/5. The strings below are the bundle's own.

const (
	rc302Autopilot = `executor.go:287-289 — the fix's correctness depends on GKE Autopilot clamping the ephemeral-storage limit down to the request at admission; this repo cannot confirm it, and the eviction evidence in the PR is the only support.`
	rc302Reactor   = `pod_resources_test.go:27-31 — the test's non-hanging behavior depends on fake-clientset reactor internals (storing the mutated object); correct given existing patterns, but load-bearing on fake-client behavior rather than the unit under test.`
	rc302Invariant = `pod_resources_test.go:60-64 — the test pins the invariant (request==limit, limit>workspace cap) but not the 10Gi value, so any equal request/limit above 5Gi would pass; intended, noted for completeness.`
	rc302Decision  = `Raising the job pod ephemeral-storage request from 1Gi to 10Gi increases every CI review pod's billable ephemeral storage tenfold for its lifetime (Autopilot bills by request); affects whoever pays the GKE bill, and the author has already flagged the cost.`

	rc302AutopilotFinding = "The fix's correctness depends entirely on GKE Autopilot clamping the ephemeral-storage limit down to the request at admission — an external behavioral fact that cannot be confirmed from this repository."
	rc302AutopilotRemedy  = "Confirm against current GKE Autopilot admission documentation that the ephemeral-storage limit is rewritten down to the request before merge."
	rc302ReactorFinding   = "The test's non-hanging behavior depends on the fake clientset storing the pod object that the create reactor mutated in-place (setting Status.Phase to PodRunning). The pattern is consistent with existing tests in pod_lifecycle_test.go."
	rc302ReactorRemedy    = "No fix needed — this is a correct and standard use of the fake clientset reactor pattern. The observation is noted for completeness."
	rc302InvariantFinding = "The test asserts request==limit and limit>5Gi (the workspace cap) but does not pin the 10Gi value. This is an intentional design choice to test the invariant rather than a specific number."
	rc302InvariantRemedy  = "No fix needed — testing the invariant rather than the magic number is the correct approach for a regression test."
)

var rc302Issues = []string{rc302Autopilot, rc302Reactor, rc302Invariant}

var rc302Sources = []rcSource{
	rcRowSource("// Ephemeral storage: the request MUST equal the limit. GKE Autopilot\n// clamps the limit down to the request at admission.\n"),
	rcRowSource(`client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {` + "\n"),
	rcRowSource("if req.Cmp(lim) != 0 {\n"),
}

func rc302Cite(source int) []rcCheckEvidence {
	return []rcCheckEvidence{{Source: source, LineStart: 1, LineEnd: 1}}
}

// rc302Answer is the challenger's submission for #302 as it was actually
// made: every allegation supported, two of them with a remedy that says
// there is nothing to fix.
func rc302Answer() rcChallengeAnswer {
	return rcChallengeAnswer{
		Scope:       []string{"executor.go, pod_resources_test.go"},
		IntentMatch: "verified",
		MergeReady:  3,
		Checks: []rcIssueCheck{
			{Issue: rc302Autopilot, Verdict: "supported", Reason: "The only code change raises the request; the limit was already 10Gi.", Finding: rc302AutopilotFinding, Remedy: rc302AutopilotRemedy, Evidence: rc302Cite(1)},
			{Issue: rc302Reactor, Verdict: "supported", Reason: "The fake clientset stores the mutated object.", Finding: rc302ReactorFinding, Remedy: rc302ReactorRemedy, Evidence: rc302Cite(2)},
			{Issue: rc302Invariant, Verdict: "supported", Reason: "6Gi/6Gi passes both assertions.", Finding: rc302InvariantFinding, Remedy: rc302InvariantRemedy, Evidence: rc302Cite(3)},
		},
		Decisions: []rcDecisionCheck{
			{Decision: rc302Decision, Verdict: "supported", Reason: "The request is what Autopilot bills.", Evidence: rc302Cite(1)},
		},
	}
}

func rc302Validate(t *testing.T, a rcChallengeAnswer) *rcChallengeResult {
	t.Helper()
	res, problems, err := rcValidateChallenge(rcTestAnswer(t, a), rc302Issues, []string{rc302Decision}, rc302Sources)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("citation problems on a submission that cites real lines: %v", problems)
	}
	return res
}

// codaIssues reads the ISSUES bullets back out of the assembled review the
// way runReviewCommit does — the list that becomes risk claims and comments.
func codaIssues(t *testing.T, review string) []string {
	t.Helper()
	_, risks, _, _, _, _ := rcParseReviewOutput(review)
	return risks
}

func notesSection(review string) string {
	i := strings.Index(review, "## Notes (nothing to fix)")
	if i < 0 {
		return ""
	}
	rest := review[i:]
	for _, end := range []string{"\n**This review is incomplete.**", "\n## Limitations", "\n## Decisions", "\n" + rcReviewDataMarker} {
		if j := strings.Index(rest, end); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

// assert302Shape holds the two publication outcomes #302 should have had,
// whichever way the challenger expressed them: one finding, two notes.
func assert302Shape(t *testing.T, res *rcChallengeResult) {
	t.Helper()
	if res.Incomplete {
		t.Fatalf("nothing is unresolved, yet the review is incomplete: %+v", res)
	}
	want := map[string]string{rc302Autopilot: rcStatusSupported, rc302Reactor: rcStatusObservation, rc302Invariant: rcStatusObservation}
	for _, a := range res.Allegations {
		if a.Status != want[a.Issue] {
			t.Errorf("%.40q: status %q, want %q", a.Issue, a.Status, want[a.Issue])
		}
	}

	// The ISSUES coda — what becomes a risk claim and a comment on a line —
	// carries the one real finding and nothing else.
	if got := codaIssues(t, res.Review); len(got) != 1 || got[0] != rc302Autopilot {
		t.Fatalf("ISSUES coda = %q, want only the Autopilot finding", got)
	}

	f := findingsSection(res.Review)
	if strings.Count(f, "\n### ") != 1 || !strings.Contains(f, rc302AutopilotFinding) || !strings.Contains(f, "**Remedy:** "+rc302AutopilotRemedy) {
		t.Errorf("findings section should hold exactly the Autopilot finding with its remedy:\n%s", f)
	}
	for _, note := range []string{rc302Reactor, rc302Invariant, rc302ReactorFinding, rc302InvariantFinding} {
		if strings.Contains(f, note) {
			t.Errorf("a note was published as a finding (%.50q):\n%s", note, f)
		}
	}

	n := notesSection(res.Review)
	if strings.Count(n, "\n### ") != 2 || !strings.Contains(n, rc302ReactorFinding) || !strings.Contains(n, rc302InvariantFinding) {
		t.Errorf("notes section should hold both observations:\n%s", n)
	}
	// "No fix needed" is not advice, and a note carries no remedy line.
	if strings.Contains(res.Review, "**Remedy:** No fix needed") || strings.Contains(n, "**Remedy:**") {
		t.Errorf("a no-fix remedy was published as a remedy:\n%s", res.Review)
	}

	if !strings.Contains(res.Review, "SUMMARY: 1 confirmed finding, 2 observations (nothing to fix).") {
		t.Errorf("summary should count the notes as notes:\n%s", res.Review)
	}
	// One real defect remains, so 3/5 stands — the score is not raised
	// merely because two of the three were notes.
	if !strings.Contains(res.Review, "MERGE_READY: 3\n") {
		t.Errorf("readiness should stay 3 with a confirmed finding:\n%s", res.Review)
	}
}

// The backstop: the submission exactly as #302's challenger made it, with the
// old vocabulary. A "supported" allegation whose remedy opens with "No fix
// needed" is an observation by its own account.
func TestASupportedAllegationWithNothingToFixIsAnObservation(t *testing.T) {
	res := rc302Validate(t, rc302Answer())
	assert302Shape(t, res)

	// The record keeps the model's own "no fix needed" as withheld, so the
	// reclassification can be audited, and never as a published remedy.
	for _, a := range res.Allegations[1:] {
		if a.Remedy != "" || a.WithheldRemedy == "" || !strings.HasPrefix(a.WithheldRemedy, "No fix needed") {
			t.Errorf("reclassified allegation record: %+v", a)
		}
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Count(s, `"status":"observation"`) != 2 || strings.Contains(s, `"remedy":"No fix needed`) {
		t.Errorf("emitted challenge record:\n%s", s)
	}
}

// The source fix: the same review with the challenger using the class that
// now exists for it. Same publication, no reclassification needed.
func TestAnObservationVerdictIsPublishedAsANote(t *testing.T) {
	a := rc302Answer()
	a.Checks[1].Verdict, a.Checks[1].Remedy = "observation", ""
	a.Checks[2].Verdict, a.Checks[2].Remedy = "observation", ""
	res := rc302Validate(t, a)
	assert302Shape(t, res)
	if a := res.Allegations[1]; a.WithheldRemedy != "" || a.Remedy != "" || len(a.Evidence) != 1 {
		t.Errorf("observation record: %+v", a)
	}
}

// An observation asserts nothing is wrong, so it has no evidence bar: none
// cited is fine, and a citation that points nowhere costs the note its
// citation, not its place, and does not send the review round a correction.
func TestAnObservationNeedsNoEvidence(t *testing.T) {
	a := rc302Answer()
	a.Checks[1].Verdict, a.Checks[1].Remedy, a.Checks[1].Evidence = "observation", "", nil
	a.Checks[2].Verdict, a.Checks[2].Remedy, a.Checks[2].Evidence = "observation", "", []rcCheckEvidence{{Source: 9, LineStart: 1, LineEnd: 1}}
	res, problems, err := rcValidateChallenge(rcTestAnswer(t, a), rc302Issues, []string{rc302Decision}, rc302Sources)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("an observation's bad citation was raised as a problem: %v", problems)
	}
	assert302Shape(t, res)
	if len(res.Allegations[1].Evidence) != 0 || len(res.Allegations[2].Evidence) != 0 {
		t.Errorf("evidence on the observations: %+v", res.Allegations[1:])
	}
	// With no finding text, the reasoning is the note rather than nothing.
	a.Checks[1].Finding = ""
	res = rc302Validate(t, a)
	if got := res.Allegations[1]; got.Status != rcStatusObservation || got.Finding != a.Checks[1].Reason {
		t.Errorf("observation with no finding text: %+v", got)
	}
}

// Observations never lower readiness. When every allegation is one, the
// results hold the branch at nothing, and the score says so — 4 with a
// decision still open, 5 without — even if the challenger counted the notes.
func TestObservationsNeverLowerReadiness(t *testing.T) {
	all := func() rcChallengeAnswer {
		a := rc302Answer()
		for i := range a.Checks {
			a.Checks[i].Verdict, a.Checks[i].Remedy = "observation", ""
		}
		return a
	}
	for _, tc := range []struct {
		name string
		mut  func(*rcChallengeAnswer)
		want finding.Readiness
	}{
		{"decision open", func(*rcChallengeAnswer) {}, finding.ReadinessDecideThenMerge},
		{"decision refuted", func(a *rcChallengeAnswer) { a.Decisions[0].Verdict = "refuted" }, finding.ReadinessMerge},
		// The old vocabulary reaches the same score through the backstop.
		{"old vocabulary", func(a *rcChallengeAnswer) {
			for i := range a.Checks {
				a.Checks[i].Verdict, a.Checks[i].Remedy = "supported", "No fix needed."
			}
		}, finding.ReadinessDecideThenMerge},
		// The floor is not a raise for its own sake: a partial intent is a
		// reason to hold a branch that the results say nothing about, an
		// unsettled item keeps the review incomplete, and a confirmed
		// defect is a 3 whatever else is on the list.
		{"partial intent", func(a *rcChallengeAnswer) { a.IntentMatch = "partial" }, finding.ReadinessSmallFixes},
		{"unresolved item", func(a *rcChallengeAnswer) { a.Checks[0].Verdict, a.Checks[0].Evidence = "unverified", nil }, finding.ReadinessSmallFixes},
		{"one real defect", func(a *rcChallengeAnswer) {
			a.Checks[0].Verdict, a.Checks[0].Remedy = "supported", rc302AutopilotRemedy
		}, finding.ReadinessSmallFixes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := all()
			tc.mut(&a)
			res, _, err := rcValidateChallenge(rcTestAnswer(t, a), rc302Issues, []string{rc302Decision}, rc302Sources)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			_, _, _, _, got, _ := rcParseReviewOutput(res.Review)
			if got != tc.want {
				t.Errorf("readiness = %d, want %d:\n%s", got, tc.want, res.Review)
			}
		})
	}
}

// The backstop reads the shape of the remedy, not its vocabulary: a remedy
// that mentions no-fix language while proposing a fix is still a fix.
func TestNoFixRemedyReadsTheOpeningOnly(t *testing.T) {
	for remedy, want := range map[string]bool{
		"":                                   false,
		rc302ReactorRemedy:                   true,
		rc302InvariantRemedy:                 true,
		"No fix needed.":                     true,
		"none":                               true,
		"None.":                              true,
		"N/A":                                true,
		"**No change required** — intended.": true,
		"Nothing to fix; noted.":             true,
		"Correct as written.":                true,
		"Noted for completeness only.":       true,
		"No action is required here.":        true,
		rc302AutopilotRemedy:                 false,
		"Pin the 10Gi value; no fix needed elsewhere.":             false,
		"None of the callers check the error — wrap it.":           false,
		"Nothing in the test asserts the value; add an assertion.": false,
		"Not needed: the guard, so remove it.":                     true, // opens with "not needed"; the model's own words say so
	} {
		if got := rcNoFixRemedy(remedy); got != want {
			t.Errorf("rcNoFixRemedy(%q) = %v, want %v", remedy, got, want)
		}
	}
}

// What the bundle carries for a note: an info claim, grounded to its line
// exactly as a risk would be, so the inbox can show where it is without the
// server counting it, posting it, or letting it move the verdict.
func TestObservationClaimsAreInfoAndGrounded(t *testing.T) {
	if got := rcObservationClaims("abc", nil, nil, nil); got != nil {
		t.Fatalf("nil challenge produced claims: %+v", got)
	}
	res := rc302Validate(t, rc302Answer())
	read := func(hash, path string) ([]string, bool) {
		lines := make([]string, 80)
		lines[26], lines[59] = "\tclient.PrependReactor(", "\tif req.Cmp(lim) != 0 {"
		return lines, true
	}
	tree := []string{"kailab-control/internal/runner/executor.go", "kailab-control/internal/runner/pod_resources_test.go"}
	claims := rcObservationClaims("f04ff3976325", res, tree, read)
	if len(claims) != 2 {
		t.Fatalf("claims = %d, want the two observations: %+v", len(claims), claims)
	}
	for i, c := range claims {
		if c.Tag != finding.TagInfo {
			t.Errorf("claim %d tagged %q, want info", i, c.Tag)
		}
		if !c.Resolved || !c.Verified || !strings.HasPrefix(c.Lookup, "kailab-control/internal/runner/pod_resources_test.go:") {
			t.Errorf("claim %d not grounded to its line: %+v", i, c)
		}
		if c.Statement != rc302Issues[i+1] {
			t.Errorf("claim %d statement = %q", i, c.Statement)
		}
	}
	// The supported allegation is not among them; it goes the risk route.
	for _, c := range claims {
		if c.Statement == rc302Autopilot {
			t.Errorf("the confirmed finding was emitted as an observation")
		}
	}
}
