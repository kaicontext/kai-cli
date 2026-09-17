package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// Evidence is cited by location; the system extracts the text. Out-of-range
// sources and out-of-bounds line ranges are reported unusable so the caller can
// drop that one citation rather than trust a fabricated excerpt.
func TestReviewCitationExtractsByLocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ev     rcCheckEvidence
		want   string
		wantOK bool
	}{
		{"first line", rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 1}, "cd /tmp && pwd", true},
		{"line range", rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 2}, "cd /tmp && pwd\npwd", true},
		{"single line source", rcCheckEvidence{Source: 2, LineStart: 1, LineEnd: 1}, `cd "$HOME"`, true},
		{"source out of range", rcCheckEvidence{Source: 9, LineStart: 1, LineEnd: 1}, "", false},
		{"source zero", rcCheckEvidence{Source: 0, LineStart: 1, LineEnd: 1}, "", false},
		{"line past end", rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 9}, "", false},
		{"inverted range", rcCheckEvidence{Source: 1, LineStart: 2, LineEnd: 1}, "", false},
		{"line zero", rcCheckEvidence{Source: 1, LineStart: 0, LineEnd: 1}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rcExtractCitation(rcCDSources, tc.ev)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("extract=%q,%v want %q,%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	if n := rcNumberedSource(1, rcCDSource); !strings.Contains(n, "    1| cd /tmp && pwd") || !strings.Contains(n, "    2| pwd") || strings.Contains(n, "    3|") {
		t.Fatalf("numbered source miscounts lines: %q", n)
	}
}

// One unusable citation drops that citation, not the finding: a supported check
// with any valid citation survives, and the review is not marked incomplete.
func TestReviewChallengeToleratesOneBadCitation(t *testing.T) {
	a := rcCDChecks()
	a.Checks[1].Evidence = append(a.Checks[1].Evidence, rcCheckEvidence{Source: 2, LineStart: 9, LineEnd: 9}) // unusable, dropped
	res, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allegations[1].Status != "supported" || len(res.Allegations[1].Evidence) != 1 || res.Incomplete {
		t.Fatalf("a valid citation lost its finding or the review was marked incomplete: %+v", res.Allegations[1])
	}
}

// REGRESSION (observed failure): one invalid citation leaves THAT finding
// unresolved while an independently supported finding remains available — in
// the published text and in the structured record.
func TestReviewChallengeOneInvalidCitationLeavesOnlyThatFindingUnresolved(t *testing.T) {
	a := rcChallengeAnswer{
		Scope:       []string{"both allegations against the sources"},
		IntentMatch: "partial",
		MergeReady:  3,
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "real", Finding: "The cd is mishandled.", Remedy: "Fix the cd handling.", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "claimed", Finding: "Expansion.", Remedy: "Fix the escaping.", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 5, LineEnd: 9}}},
		},
	}
	res, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatalf("one bad citation withheld the whole review: %v", err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(res.Review)
	if len(issues) != 1 || issues[0] != rcFalseCDIssue {
		t.Fatalf("supported finding not available alongside the unresolved one: %v", issues)
	}
	if res.Allegations[0].Status != "supported" || res.Allegations[0].Remedy != "Fix the cd handling." {
		t.Fatalf("supported finding's remedy not actionable: %+v", res.Allegations[0])
	}
	u := res.Allegations[1]
	if u.Status != "unresolved" || !strings.Contains(u.Reason, "no usable citation") || u.Remedy != "" || u.WithheldRemedy != "Fix the escaping." {
		t.Fatalf("invalid-citation finding not unresolved with its remedy withheld: %+v", u)
	}
	if !res.Incomplete || len(res.Unresolved) != 1 || res.Unresolved[0] != rcEscapeIssue {
		t.Fatalf("incomplete status not derived from the final results: %+v", res)
	}
	if strings.Contains(res.Review, "Fix the escaping.") {
		t.Fatalf("withheld remedy leaked into the published review: %s", res.Review)
	}
}

// REGRESSION (observed failure): a model claiming it ran an experiment that is
// not present is downgraded, and its remedy is withheld. Two forms: citing a
// source number that does not exist (a phantom experiment), and citing a real
// non-experiment source while asserting a runtime verdict.
func TestReviewChallengeAbsentExperimentIsDowngradedAndRemedyWithheld(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   rcCheckEvidence
	}{
		{"phantom experiment source", rcCheckEvidence{Source: 3, LineStart: 1, LineEnd: 1}},
		{"non-experiment source", rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcChallengeAnswer{
				Scope:       []string{"the cd behavior"},
				IntentMatch: "partial",
				MergeReady:  3,
				Checks: []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "supported", RequiresRuntime: rcBool(true), Reason: "I ran it and saw the failure",
					Finding: "Later lines run in the wrong directory.", Remedy: "Wrap the command in braces so the cd applies to every line.", Evidence: []rcCheckEvidence{tc.ev}}},
			}
			res, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue}, nil, rcCDSources, nil) // no experiments ran
			if err != nil {
				t.Fatal(err)
			}
			r := res.Allegations[0]
			if r.Status != "unresolved" || r.Remedy != "" || r.WithheldRemedy == "" {
				t.Fatalf("absent experiment not downgraded with remedy withheld: %+v", r)
			}
			if !res.Incomplete || strings.Contains(res.Review, "Wrap the command in braces") {
				t.Fatalf("brace-wrapping advice published for an unresolved runtime claim: incomplete=%v\n%s", res.Incomplete, res.Review)
			}
		})
	}
}

// With an experiment from THIS challenge backing it, the same runtime claim
// publishes with its remedy actionable.
func TestReviewChallengeRuntimeClaimPublishesWithExperiment(t *testing.T) {
	a := rcChallengeAnswer{
		Scope:       []string{"the escaping behavior"},
		IntentMatch: "partial",
		MergeReady:  3,
		Checks:      []rcIssueCheck{{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(true), Reason: "runtime", Finding: "Expands.", Remedy: "Escape the path.", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}}},
	}
	res, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcEscapeIssue}, nil, []string{`cd "$HOME"`, "expansion observed"}, map[int]bool{2: true})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Allegations[0]
	if r.Status != "supported" || r.Remedy != "Escape the path." || res.Incomplete || !r.Evidence[0].Experiment {
		t.Fatalf("experiment-backed runtime claim not published as actionable: %+v", r)
	}
}

// REGRESSION (observed failure): an unresolved cd allegation cannot publish
// brace-wrapping advice under Decisions. The remedy is withheld on the
// allegation, and a decision the draft never made is dropped — a different
// heading does not bypass the evidence requirement.
func TestReviewChallengeUnresolvedCDCannotPublishBraceAdviceUnderDecisions(t *testing.T) {
	a := rcChallengeAnswer{
		Scope:       []string{"the cd behavior"},
		IntentMatch: "partial",
		MergeReady:  4,
		Checks: []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "could not run a shell",
			Remedy: "Wrap the command in braces so the cd applies to every line."}},
		// The draft made no decisions; the model tries to smuggle the fix in here.
		Decisions: []rcDecisionCheck{{Decision: "Wrap the command in braces so the cd applies to every line.", Verdict: "supported", Reason: "cleaner", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}}},
	}
	res, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue}, nil, rcCDSources, nil)
	if err != nil {
		t.Fatalf("smuggled decision sank the review instead of being dropped: %v", err)
	}
	if strings.Contains(res.Review, "Wrap the command in braces") || strings.Contains(res.Review, "DECISIONS:") {
		t.Fatalf("brace-wrapping advice published under Decisions for an unresolved allegation:\n%s", res.Review)
	}
	if len(res.Decisions) != 0 || res.Allegations[0].Status != "unresolved" || res.Allegations[0].WithheldRemedy == "" || !res.Incomplete {
		t.Fatalf("record wrong: decisions=%+v allegation=%+v incomplete=%v", res.Decisions, res.Allegations[0], res.Incomplete)
	}
}

// A decision the DRAFT made is preserved when assessed with evidence, and
// dropped to unresolved without it — the same requirement as an allegation.
func TestReviewChallengeAssessesDraftDecisions(t *testing.T) {
	const decision = "Keep the new getter public — it is now part of the panel API."
	base := func(verdict string, ev []rcCheckEvidence) rcChallengeAnswer {
		a := rcCDChecks()
		a.Decisions = []rcDecisionCheck{{Decision: decision, Verdict: verdict, Reason: "it is exported and used", Evidence: ev}}
		return a
	}
	res, err := rcValidateChallenge(rcTestAnswer(t, base("supported", []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}})), rcCDIssues, []string{decision}, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, decisions, _, _, _ := rcParseReviewOutput(res.Review)
	if len(decisions) != 1 || decisions[0] != decision || res.Decisions[0].Status != "supported" {
		t.Fatalf("genuine draft decision not preserved: %v %+v", decisions, res.Decisions)
	}
	res, err = rcValidateChallenge(rcTestAnswer(t, base("supported", nil)), rcCDIssues, []string{decision}, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, decisions, _, _, _ = rcParseReviewOutput(res.Review)
	if len(decisions) != 0 || res.Decisions[0].Status != "unresolved" {
		t.Fatalf("decision without evidence was published: %v %+v", decisions, res.Decisions)
	}
}

// Readiness is made coherent with the FINAL statuses by clamping, always toward
// caution, instead of failing closed. Live GLM-5.2 on #418 submitted a
// merge_ready that contradicted its own verdicts; failing closed withheld the
// supported finding too. The finding must survive and the score must be sane.
func TestReviewChallengeClampsIncoherentReadinessInsteadOfWithholding(t *testing.T) {
	// A confirmed defect proposed as ready-to-merge: clamped to small-fixes.
	a := rcCDChecks()
	a.MergeReady = 5
	res, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatalf("incoherent readiness withheld the review: %v", err)
	}
	if _, issues, _, _, readiness, _ := rcParseReviewOutput(res.Review); len(issues) != 1 || int(readiness) != 3 {
		t.Fatalf("supported finding lost or readiness not clamped: issues=%v readiness=%d", issues, readiness)
	}
	// Nothing found and nothing open, proposed as needs-work: LEFT ALONE. The
	// system never raises a score — a contradictory answer must never become a
	// more permissive merge recommendation. Too cautious is not a defect.
	b := rcCDChecks()
	b.Checks[1].Verdict = "refuted" // now both refuted
	b.MergeReady = 2
	res, err = rcValidateChallenge(rcTestAnswer(t, b), rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, readiness, _ := rcParseReviewOutput(res.Review); int(readiness) != 2 {
		t.Fatalf("a cautious score was raised to a more permissive one: %d", readiness)
	}
	// A supported draft decision with a proposed clean merge: held at "your call".
	const decision = "Keep the getter public."
	c := rcCDChecks()
	c.Checks[1].Verdict = "refuted"
	c.MergeReady = 5
	c.Decisions = []rcDecisionCheck{{Decision: decision, Verdict: "supported", Reason: "exported", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}}}
	res, err = rcValidateChallenge(rcTestAnswer(t, c), rcCDIssues, []string{decision}, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, decisions, _, readiness, _ := rcParseReviewOutput(res.Review); len(decisions) != 1 || int(readiness) != 4 {
		t.Fatalf("open decision allowed a clean-merge score: decisions=%v readiness=%d", decisions, readiness)
	}
}

// The SUMMARY is derived from the validated counts; there is no summary or
// assessment field, so a rejected allegation has no channel to be restated —
// not verbatim and not paraphrased (the reviewer's own specimen).
func TestReviewChallengeSummaryIsDerivedNotModelAuthored(t *testing.T) {
	raw := strings.TrimSuffix(rcTestAnswer(t, rcCDChecks()), "}") +
		`,"summary":"Multiline commands execute in the wrong directory.","assessment":"Later lines run outside the workspace."}`
	res, err := rcValidateChallenge(raw, rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Review, "wrong directory") || strings.Contains(res.Review, "Later lines run outside") || strings.Contains(res.Review, rcFalseCDIssue) {
		t.Fatalf("model-authored text or a refuted allegation leaked into the published review: %s", res.Review)
	}
	_, _, _, _, _, note := rcParseReviewOutput(res.Review)
	if !strings.Contains(note, "1 confirmed finding") || !strings.Contains(note, "1 refuted") {
		t.Fatalf("SUMMARY not derived from the validated counts: %q", note)
	}
}

// Each unresolved allegation is reported with its ACTUAL reason, and the log
// records the FINAL validated verdict, not the model's original "supported".
func TestReviewChallengeRecordsFinalVerdictAndPerClaimReason(t *testing.T) {
	a := rcChallengeAnswer{
		Scope:       []string{"both allegations"},
		IntentMatch: "partial",
		MergeReady:  4,
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "x", Finding: "f", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 9, LineEnd: 9}}}, // bad citation
			{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(true), Reason: "y", Finding: "f", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}},   // runtime, no experiment
		},
	}
	res, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allegations[0].Status != "unresolved" || !strings.Contains(res.Allegations[0].Reason, "no usable citation") {
		t.Fatalf("citation-invalid claim not recorded with its real reason: %+v", res.Allegations[0])
	}
	if res.Allegations[1].Status != "unresolved" || !strings.Contains(res.Allegations[1].Reason, "runtime experiment") {
		t.Fatalf("runtime claim not recorded with its real reason: %+v", res.Allegations[1])
	}
	if !strings.Contains(res.Review, "no usable citation") || !strings.Contains(res.Review, "runtime experiment") {
		t.Fatalf("banner does not give per-claim reasons: %s", res.Review)
	}
}

// The unverified path runs end-to-end through the provider and returns the
// structured, incomplete result.
func TestReviewChallengeUnverifiedThroughProvider(t *testing.T) {
	a := rcChallengeAnswer{
		Scope:       []string{"the diff"},
		IntentMatch: "partial",
		MergeReady:  4,
		Checks:      []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "needs a shell"}},
	}
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestAnswer(t, a)}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil)
	if err != nil {
		t.Fatalf("unverified allegation withheld the review: %v", err)
	}
	if !res.Incomplete || len(res.Unresolved) != 1 || !strings.Contains(res.Review, "This review is incomplete") {
		t.Fatalf("expected an incomplete structured result: %+v", res)
	}
}

// REGRESSION (observed failure): partial results reach the ACTUAL emitted
// bundle as incomplete, with the structured record attached — not merely a
// warning in prose. This marshals the exact struct the JSON branch of
// runReviewCommit emits.
func TestPartialResultsReachEmittedBundleAsIncomplete(t *testing.T) {
	a := rcChallengeAnswer{
		Scope:       []string{"both allegations"},
		IntentMatch: "partial",
		MergeReady:  3,
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "real", Finding: "f", Remedy: "fix it", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "needs a shell", Remedy: "escape it"},
		},
	}
	res, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror runReviewCommit's decision and shape exactly.
	incomplete := res.Incomplete
	out, err := json.Marshal(struct {
		Review     string             `json:"review,omitempty"`
		Incomplete bool               `json:"incomplete,omitempty"`
		Challenge  *rcChallengeResult `json:"challenge,omitempty"`
	}{res.Review, incomplete, res})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"incomplete":true`, `"status":"supported"`, `"status":"unresolved"`, `"remedy":"fix it"`, `"withheldRemedy":"escape it"`, `"unresolved":["` + rcEscapeIssue + `"]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("emitted bundle missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"remedy":"escape it"`) {
		t.Fatalf("withheld remedy published as actionable in the bundle:\n%s", s)
	}
	// A complete review's bundle carries no incomplete flag.
	done, _ := json.Marshal(struct {
		Incomplete bool `json:"incomplete,omitempty"`
	}{false})
	if strings.Contains(string(done), "incomplete") {
		t.Fatalf("a complete review's bundle mentions incomplete: %s", done)
	}
}
