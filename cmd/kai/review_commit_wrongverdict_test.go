package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Validation of the verdict contract against the PRESERVED wrong-verdict run:
// the end-to-end GLM-5.2 review of the #429 scratch repo that concluded the
// JSON.stringify quoting was safe. testdata/pr429/wrong-verdict-run.json was
// extracted from that run's bundle by script (see its "provenance" field), not
// retyped. It carries the allegation text, GLM's verdict and reason, and the
// complete record of the experiment GLM cited: a fidelity-mode run on
// /tmp/test"dir — a double quote, which JSON.stringify escapes — with all three
// assertions passed.
//
// Under the four rules, that record can carry NO verdict on this allegation:
// it observed conformance (intended behavior asserted, all passed), so it
// cannot support the defect; and it did not cover the alleged inputs ($,
// backticks), so it cannot refute it. The wrong conclusion is unreachable from
// that evidence whatever the model asserts — except by declaring, on the
// record, that a double-quote test covered the alleged $/backtick inputs. That
// false declaration would be visible in the bundle.
type rcWrongVerdictRun struct {
	Provenance       string             `json:"provenance"`
	Issue            string             `json:"issue"`
	GLMStatus        string             `json:"glmStatus"`
	GLMReason        string             `json:"glmReason"`
	ExperimentSource int                `json:"experimentSource"`
	Experiment       rcExperimentRecord `json:"experiment"`
}

func rcLoadWrongVerdictRun(t *testing.T) rcWrongVerdictRun {
	t.Helper()
	raw, err := os.ReadFile("testdata/pr429/wrong-verdict-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var run rcWrongVerdictRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatal(err)
	}
	// The preserved record predates the outcome field; it did run (it has a
	// generated command, exit status, and evaluated assertions), so it is
	// completed. Stated here rather than hidden in the fixture.
	if run.Experiment.Outcome == "" {
		run.Experiment.Outcome = rcOutcomeCompleted
	}
	return run
}

func TestWrongVerdictRunFixtureIsTheOneWeThink(t *testing.T) {
	run := rcLoadWrongVerdictRun(t)
	if run.GLMStatus != "supported" || !strings.Contains(run.GLMReason, "properly escapes") {
		t.Fatalf("fixture is not the preserved wrong-verdict run: status=%q reason=%q", run.GLMStatus, run.GLMReason)
	}
	e := run.Experiment
	// The tested PATH is the double-quote one and contains no $ or backtick.
	// (The generated command does contain a $ — GLM's own `$(pwd)` in the
	// echo — which is not part of the input under test.)
	tested := ""
	for _, a := range e.Assertions {
		if a.Kind == "pwd" {
			tested = a.Value
		}
	}
	if e.Mode != "construct" || !e.AllPassed || len(e.Assertions) != 3 || tested != `/tmp/test"dir` || strings.ContainsAny(tested, "$`") || !strings.HasPrefix(e.GeneratedCommand, `cd "/tmp/test\"dir"`) {
		t.Fatalf("fixture experiment is not the double-quote, all-passed record: tested=%q %+v", tested, e)
	}
}

func TestWrongVerdictRunCannotProduceTheWrongConclusion(t *testing.T) {
	run := rcLoadWrongVerdictRun(t)
	// Sources 1..4 as in that run; only source 4 (the experiment) matters here.
	sources := []string{"the diff", "exploration 1", "exploration 2", run.Experiment.render("node@sha256:c610…")}
	experiments := map[int]*rcExperimentRecord{run.ExperimentSource: &run.Experiment}
	issues := []string{run.Issue}
	answer := func(verdict string, ev rcCheckEvidence) string {
		ev.Source, ev.LineStart, ev.LineEnd = run.ExperimentSource, 13, 24 // the lines GLM cited
		return rcTestAnswer(t, rcChallengeAnswer{
			Scope: []string{"frontend/dist/app.js lines 9-10"}, IntentMatch: "partial", MergeReady: 3,
			Checks: []rcIssueCheck{{Issue: run.Issue, Verdict: verdict, RequiresRuntime: rcBool(true), Reason: run.GLMReason,
				Finding: "f", Remedy: "No code change needed for JSON.stringify safety.", Evidence: []rcCheckEvidence{ev}}},
		})
	}
	yes, no := rcBool(true), rcBool(false)
	honest := rcCheckEvidence{AddressesAllegation: yes, CoversAllegedInputs: no, Expectation: "intended", Tested: `a path containing a double quote`}

	// What GLM actually submitted — "supported" — with an honest connection:
	// unresolved. The experiment observed conformance, so it cannot support the
	// defect; its remedy ("no code change needed") is withheld, not published.
	res, err := rcValidateChallenge(answer("supported", honest), issues, nil, sources, experiments)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Allegations[0]
	if r.Status != "unresolved" || !strings.Contains(r.Reason, "observed the alleged violation; none did") || r.Remedy != "" || r.WithheldRemedy == "" || r.Evidence[0].Observed != rcObservedConformance {
		t.Fatalf("the preserved run's verdict survived the new rules: %+v", r)
	}
	if !res.Incomplete {
		t.Fatal("review not marked incomplete")
	}
	// The conclusion GLM's reasoning actually expresses — "the quoting is
	// safe", i.e. refuted — with an honest connection: unresolved, because a
	// double-quote example does not cover the alleged $/backtick inputs.
	res, err = rcValidateChallenge(answer("refuted", honest), issues, nil, sources, experiments)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Allegations[0]; r.Status != "unresolved" || !strings.Contains(r.Reason, "on the alleged inputs themselves") || !strings.Contains(r.Reason, "double quote") {
		t.Fatalf("a double-quote example refuted a $/backtick allegation: %+v", r)
	}
	// Even a DISHONEST covers_alleged_inputs=true cannot turn the record into
	// support for the defect: no violation was observed.
	dishonest := honest
	dishonest.CoversAllegedInputs = yes
	res, err = rcValidateChallenge(answer("supported", dishonest), issues, nil, sources, experiments)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Allegations[0]; r.Status != "unresolved" {
		t.Fatalf("a dishonest covers flag produced support without an observed violation: %+v", r)
	}
	// The one path to the wrong "safe" conclusion that remains is a false
	// declaration that the double-quote test covered $ and backticks. It is a
	// model judgment the gate cannot check; it is recorded on the citation so
	// a reader can see exactly what was claimed about what was tested.
	res, err = rcValidateChallenge(answer("refuted", dishonest), issues, nil, sources, experiments)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Allegations[0]; r.Status != "refuted" || !r.Evidence[0].Covers || r.Evidence[0].Tested != honest.Tested {
		t.Fatalf("expected the false-cover path to be recorded, not hidden: %+v", r)
	}
}
