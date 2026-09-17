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
		Checks: []rcIssueCheck{{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(true), Reason: "runtime", Finding: "Expands.", Remedy: "Escape the path.", Evidence: []rcCheckEvidence{
			{Source: 2, LineStart: 1, LineEnd: 1, AddressesAllegation: rcBool(true), CoversAllegedInputs: rcBool(true), Expectation: "defect", Tested: "a path containing $HOME"},
		}}},
	}
	// The experiment asserted the DEFECT (pwd_not the intended dir) and it
	// passed: the alleged violation was observed.
	passing := map[int]*rcExperimentRecord{2: {Outcome: rcOutcomeCompleted, Mode: "construct", HasAssertions: true, AllPassed: true,
		Assertions: []rcAssertionResult{{Kind: "pwd_not", Value: "/tmp/a$HOME", Passed: true, Observed: "/tmp/a"}}}}
	res, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcEscapeIssue}, nil, []string{`cd "$HOME"`, "expansion observed"}, passing)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Allegations[0]
	if r.Status != "supported" || r.Remedy != "Escape the path." || res.Incomplete || !r.Evidence[0].Experiment {
		t.Fatalf("experiment-backed runtime claim not published as actionable: %+v", r)
	}
	if len(res.Experiments) != 1 || res.Experiments[0].Source != 2 || !res.Experiments[0].Record.AllPassed {
		t.Fatalf("complete experiment record not preserved on the result: %+v", res.Experiments)
	}
}

// The verdict is connected to what was tested and what was observed. Four
// rules: an observed violation by a relevant experiment can SUPPORT the
// defect; a passing example establishes behavior for that example only and
// REFUTES the allegation only if it covered the alleged inputs (and never when
// a violation was observed); an experiment that does not address the
// allegation leaves it unresolved; an experiment that could not run supplies
// no runtime conclusion. A verdict is never flipped — one the observations do
// not carry becomes unresolved, with the reason recorded.
func TestReviewChallengeVerdictConnectsTestedAndObserved(t *testing.T) {
	sources := []string{`cd "$HOME"`, "experiment output"}
	answer := func(verdict string, ev rcCheckEvidence) rcChallengeAnswer {
		ev.Source, ev.LineStart, ev.LineEnd = 2, 1, 1
		return rcChallengeAnswer{
			Scope: []string{"quoting"}, IntentMatch: "partial", MergeReady: 4,
			Checks: []rcIssueCheck{{Issue: rcEscapeIssue, Verdict: verdict, RequiresRuntime: rcBool(true), Reason: "ran it", Finding: "f", Remedy: "fix", Evidence: []rcCheckEvidence{ev}}},
		}
	}
	run := func(verdict string, ev rcCheckEvidence, rec *rcExperimentRecord) rcAllegationResult {
		t.Helper()
		res, err := rcValidateChallenge(rcTestAnswer(t, answer(verdict, ev)), []string{rcEscapeIssue}, nil, sources, map[int]*rcExperimentRecord{2: rec})
		if err != nil {
			t.Fatal(err)
		}
		return res.Allegations[0]
	}
	yes, no := rcBool(true), rcBool(false)
	// Records: the intended-behavior assertion FAILED on the alleged input
	// (violation observed); the intended-behavior assertion PASSED on a benign
	// input (conformance for that input).
	violation := &rcExperimentRecord{Outcome: rcOutcomeCompleted, Mode: "construct", HasAssertions: true, AllPassed: false,
		Assertions: []rcAssertionResult{{Kind: "pwd", Value: "/tmp/test$dir", Passed: false, Observed: "/tmp"}}}
	conformance := &rcExperimentRecord{Outcome: rcOutcomeCompleted, Mode: "construct", HasAssertions: true, AllPassed: true,
		Assertions: []rcAssertionResult{{Kind: "pwd", Value: `/tmp/test"dir`, Passed: true, Observed: `/tmp/test"dir`}}}

	// Rule 1: an observed violation supports the defect — the failed assertion
	// is the evidence, not a reason to discard it.
	if r := run("supported", rcCheckEvidence{AddressesAllegation: yes, CoversAllegedInputs: yes, Expectation: "intended", Tested: "$dir"}, violation); r.Status != "supported" || r.Remedy != "fix" || r.Evidence[0].Observed != rcObservedViolation {
		t.Fatalf("observed violation did not support the defect: %+v", r)
	}
	// The same violation cannot be REFUTED: the observation contradicts it.
	if r := run("refuted", rcCheckEvidence{AddressesAllegation: yes, CoversAllegedInputs: yes, Expectation: "intended"}, violation); r.Status != "unresolved" || !strings.Contains(r.Reason, "observed the alleged violation") {
		t.Fatalf("a refutation survived an observed violation: %+v", r)
	}
	// Rule 2: a passing example on a benign input refutes nothing — this is
	// the preserved wrong-verdict mechanism (a double quote, not $ or backtick).
	if r := run("refuted", rcCheckEvidence{AddressesAllegation: yes, CoversAllegedInputs: no, Expectation: "intended", Tested: `a path containing "`}, conformance); r.Status != "unresolved" || !strings.Contains(r.Reason, "on the alleged inputs themselves") || !strings.Contains(r.Reason, `tested: a path containing "`) {
		t.Fatalf("a passing example on the wrong input refuted the allegation: %+v", r)
	}
	// …and it cannot SUPPORT the defect either: no violation was observed.
	if r := run("supported", rcCheckEvidence{AddressesAllegation: yes, CoversAllegedInputs: no, Expectation: "intended"}, conformance); r.Status != "unresolved" || !strings.Contains(r.Reason, "observed the alleged violation; none did") || r.Remedy != "" || r.WithheldRemedy != "fix" {
		t.Fatalf("conformance supported a defect, or remedy not withheld: %+v", r)
	}
	// Conformance ON the alleged inputs may refute (a model judgment, recorded).
	if r := run("refuted", rcCheckEvidence{AddressesAllegation: yes, CoversAllegedInputs: yes, Expectation: "intended", Tested: "literal $ and backtick paths"}, conformance); r.Status != "refuted" || !r.Evidence[0].Covers {
		t.Fatalf("covering conformance did not refute: %+v", r)
	}
	// Rule 3: an experiment the model says does not address the allegation, or
	// one it never connected, leaves it unresolved.
	if r := run("supported", rcCheckEvidence{AddressesAllegation: no, Expectation: "intended"}, violation); r.Status != "unresolved" || !strings.Contains(r.Reason, "no cited experiment addresses the allegation") {
		t.Fatalf("a non-addressing experiment backed a verdict: %+v", r)
	}
	if r := run("supported", rcCheckEvidence{Expectation: "intended"}, violation); r.Status != "unresolved" || !strings.Contains(r.Reason, "did not state whether the experiment addresses") {
		t.Fatalf("an unconnected experiment backed a verdict: %+v", r)
	}
	// No declared expectation: no observation can be derived.
	if r := run("supported", rcCheckEvidence{AddressesAllegation: yes}, violation); r.Status != "unresolved" || !strings.Contains(r.Reason, "intended behavior or the alleged defect") {
		t.Fatalf("an observation was derived without a declared expectation: %+v", r)
	}
	// Rule 4: an experiment that could not run supplies no runtime conclusion.
	if r := run("supported", rcCheckEvidence{AddressesAllegation: yes, Expectation: "intended"}, &rcExperimentRecord{Outcome: rcOutcomeNotRun, Error: "node: not found"}); r.Status != "unresolved" || !strings.Contains(r.Reason, "did not run: node: not found") {
		t.Fatalf("a not-run experiment backed a verdict: %+v", r)
	}
	// A bare printout: no assertions, no observation.
	if r := run("refuted", rcCheckEvidence{AddressesAllegation: yes, CoversAllegedInputs: yes, Expectation: "intended"}, &rcExperimentRecord{Outcome: rcOutcomeCompleted, Mode: "script"}); r.Status != "unresolved" || !strings.Contains(r.Reason, "declared no assertions") {
		t.Fatalf("an unasserted printout backed a refutation: %+v", r)
	}
}

// Item 1 of the evidence contract: every experiment attempt is recorded, and
// "could not run" is distinct from "ran and the expected behavior failed".
// A not-run attempt has no observation and is never a citable source; a
// completed attempt whose assertion failed IS an observation — expected vs
// observed — and is preserved as such. The verdict rule is unchanged here.
func TestExperimentRecordDistinguishesNotRunFromFailedAssertion(t *testing.T) {
	// Could not run: parameter validation fails before any container is used.
	sb := &rcShellSandbox{image: "x@sha256:" + strings.Repeat("0", 64)}
	rec, err := sb.runExperiment(context.Background(), `{"script":"pwd","assertions":[{"kind":"exit","value":"0"}]}`)
	if err == nil || rec == nil || rec.Outcome != rcOutcomeNotRun || rec.Error == "" || rec.completed() {
		t.Fatalf("not-run attempt not recorded as such: rec=%+v err=%v", rec, err)
	}
	if obs, why := rec.observation("intended"); obs != "" || !strings.Contains(why, "did not run") || !strings.Contains(rec.render(sb.image), "could not run") {
		t.Fatalf("not-run record yielded an observation or does not say so: obs=%q why=%q", obs, why)
	}
	// Ran, expectation failed: a valid observation, distinct from the above —
	// with the intended behavior asserted, the failure IS the violation observed.
	failed := &rcExperimentRecord{Outcome: rcOutcomeCompleted, Mode: "construct", ObservedPWD: "/tmp", ExitCode: 2}
	failed.evaluate([]rcAssertion{{Kind: "pwd", Value: "/tmp/test$dir"}})
	if !failed.completed() || failed.Assertions[0].Passed || failed.Assertions[0].Observed != "/tmp" {
		t.Fatalf("completed-but-failed experiment not recorded as an observation: %+v", failed)
	}
	if obs, _ := failed.observation("intended"); obs != rcObservedViolation {
		t.Fatalf("failed intended-behavior assertion not derived as a violation: %q", obs)
	}
	if !strings.Contains(failed.render("img"), `FAIL pwd "/tmp/test$dir" (observed "/tmp")`) {
		t.Fatalf("failed assertion not rendered as expected-vs-observed: %s", failed.render("img"))
	}
	// Through the challenge: a not-run attempt is preserved on the result at
	// source 0 (uncitable), and does not sink the review.
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls == 1 {
			return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "e1", Name: "review_shell", Input: `{"script":"pwd","assertions":[{"kind":"exit","value":"0"}]}`}}}, nil
		}
		last := req.Messages[len(req.Messages)-1].Parts[0].(message.ToolResult)
		if !last.IsError || !strings.Contains(last.Content, "could not run") {
			t.Fatalf("model not told the experiment produced no observation: %+v", last)
		}
		a := rcChallengeAnswer{Scope: []string{"x"}, IntentMatch: "partial", MergeReady: 4,
			Checks: []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "experiment could not run"}}}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, a)}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, sb)
	if err != nil {
		t.Fatalf("a not-run experiment sank the review: %v", err)
	}
	if len(res.Experiments) != 1 || res.Experiments[0].Source != 0 || res.Experiments[0].Record.Outcome != rcOutcomeNotRun {
		t.Fatalf("not-run attempt not preserved on the result at source 0: %+v", res.Experiments)
	}
	if !res.Incomplete || res.Allegations[0].Status != "unresolved" {
		t.Fatalf("expected unresolved + incomplete: %+v", res.Allegations[0])
	}
}

// Pure tests of the fidelity-mode record: parsing the driver's output and
// evaluating assertions against it.
func TestExperimentRecordParsesConstructOutputAndEvaluates(t *testing.T) {
	out := rcGenBegin + "\n" + `cd "/tmp/test$dir" && pwd && echo REACHED` + "\n" + rcGenEnd + "\n" +
		rcRunBegin + "\n" + "sh: cd: can't cd\n" + rcExitMarker + "2\n" + rcPWDMarker + "/tmp\n"
	rec := &rcExperimentRecord{Outcome: rcOutcomeCompleted, Mode: "construct"}
	if err := rec.parseConstructOutput(out); err != nil {
		t.Fatal(err)
	}
	if rec.GeneratedCommand != `cd "/tmp/test$dir" && pwd && echo REACHED` || rec.ExitCode != 2 || rec.ObservedPWD != "/tmp" || !strings.Contains(rec.Stdout, "can't cd") {
		t.Fatalf("record parsed wrong: %+v", rec)
	}
	rec.evaluate([]rcAssertion{{Kind: "pwd", Value: "/tmp/test$dir"}, {Kind: "exit", Value: "2"}, {Kind: "stdout_not_contains", Value: "REACHED"}})
	if !rec.HasAssertions || rec.AllPassed || rec.Assertions[0].Passed || !rec.Assertions[1].Passed || !rec.Assertions[2].Passed {
		t.Fatalf("assertions evaluated wrong: %+v", rec.Assertions)
	}
	if obs, why := rec.observation("intended"); obs != rcObservedViolation || !strings.Contains(why, `pwd "/tmp/test$dir", observed "/tmp"`) {
		t.Fatalf("derived observation does not name the failed expectation: obs=%q why=%q", obs, why)
	}
	if obs, _ := rec.observation("defect"); obs != rcObservedConformance {
		t.Fatalf("with the defect asserted and not all passing, observation should be conformance: %q", obs)
	}
	if err := (&rcExperimentRecord{}).parseConstructOutput("no markers"); err == nil {
		t.Fatal("incomplete record accepted")
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
	// A runtime claim citing only a source (no experiment): no runtime
	// conclusion can be drawn, and the reason says exactly that.
	if res.Allegations[1].Status != "unresolved" || !strings.Contains(res.Allegations[1].Reason, "no cited experiment addresses the allegation") {
		t.Fatalf("runtime claim not recorded with its real reason: %+v", res.Allegations[1])
	}
	if !strings.Contains(res.Review, "no usable citation") || !strings.Contains(res.Review, "no runtime conclusion") {
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
