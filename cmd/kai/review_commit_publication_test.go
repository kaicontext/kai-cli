package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// These tests cover PUBLICATION MECHANICS: that what is published follows the
// challenger's per-item verdicts. They say nothing about whether those verdicts
// are right — a wrong "supported" is published exactly as faithfully as a
// correct one.

const rcTestDecision = `The play button now changes the terminal's working directory for every command it runs.`

func rcMustValidate(t *testing.T, a rcChallengeAnswer, decisions []string) *rcChallengeResult {
	t.Helper()
	res, _, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, decisions, rcCDSources)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return res
}

// findingsSection is the part of the assembled review where actionable
// findings and remedies are published.
func findingsSection(review string) string {
	i := strings.Index(review, "## Findings")
	if i < 0 {
		return ""
	}
	rest := review[i:]
	for _, end := range []string{"\n## Could not verify", "\n## Limitations", "\n## Decisions", "\n" + rcReviewDataMarker} {
		if j := strings.Index(rest, end); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

// couldNotVerifySection is the part of the assembled review that lists the
// items the check could neither confirm nor rule out.
func couldNotVerifySection(review string) string {
	i := strings.Index(review, "\n## Could not verify\n")
	if i < 0 {
		return ""
	}
	rest := review[i+1:]
	for _, end := range []string{"\n## Limitations", "\n## Decisions", "\n" + rcReviewDataMarker} {
		if j := strings.Index(rest, end); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

// A supported finding survives alongside a refuted one; the refuted
// allegation, its description and its remedy are not published.
func TestPublicationSupportedSurvivesRefuted(t *testing.T) {
	res := rcMustValidate(t, rcCDChecks(), nil)
	f := findingsSection(res.Review)
	for _, want := range []string{"### " + rcPublishedIssue(res.Allegations[1]), rcEscapeFinding, "**Remedy:** " + rcEscapeRemedy} {
		if !strings.Contains(f, want) {
			t.Fatalf("supported finding not published (%q missing):\n%s", want, res.Review)
		}
	}
	for _, banned := range []string{rcFalseCDIssue, rcFalseCDRemedy} {
		if strings.Contains(res.Review, banned) {
			t.Fatalf("refuted allegation or its remedy was published (%q):\n%s", banned, res.Review)
		}
	}
	// The record keeps what was proposed, marked as withheld, never as advice.
	if a := res.Allegations[0]; a.Status != rcStatusRefuted || a.Remedy != "" || a.WithheldRemedy != rcFalseCDRemedy || a.Finding != "" {
		t.Fatalf("refuted allegation record: %+v", a)
	}
	if a := res.Allegations[1]; a.Status != rcStatusSupported || a.Remedy != rcEscapeRemedy || a.Finding != rcEscapeFinding || len(a.Evidence) != 1 {
		t.Fatalf("supported allegation record: %+v", a)
	}
}

// A supported finding survives alongside an unresolved one; the review is
// published and complete, lists the unresolved item under "Could not verify",
// and proposes no fix for it. One open item does not discard the verdict.
func TestPublicationSupportedSurvivesUnresolved(t *testing.T) {
	a := rcCDChecks()
	a.MergeReady = 5
	a.Checks[0].Verdict, a.Checks[0].Reason, a.Checks[0].Evidence = "unverified", "needs a shell to settle", nil
	res := rcMustValidate(t, a, nil)
	if _, _, _, _, readiness, _ := rcParseReviewOutput(res.Review); readiness > finding.ReadinessDecideThenMerge {
		t.Fatalf("readiness %d with an open item", readiness)
	}
	if f := findingsSection(res.Review); !strings.Contains(f, rcEscapeFinding) || !strings.Contains(f, rcEscapeRemedy) || strings.Contains(f, rcFalseCDIssue) {
		t.Fatalf("findings section wrong:\n%s", res.Review)
	}
	if !strings.Contains(res.Review, "\n## Could not verify\n") || !strings.Contains(res.Review, "- "+rcFalseCDIssue+" — needs a shell to settle") {
		t.Fatalf("unresolved allegation not listed with its reason:\n%s", res.Review)
	}
	if strings.Contains(res.Review, rcFalseCDRemedy) {
		t.Fatalf("repair advice published for an unresolved allegation:\n%s", res.Review)
	}
	if got := res.unresolved(); len(got) != 1 || got[0] != rcFalseCDIssue {
		t.Fatalf("unresolved(): %v", got)
	}
	// The emitted bundle, shaped exactly as runReviewCommit shapes it.
	out, err := json.Marshal(struct {
		Review     string             `json:"review,omitempty"`
		Incomplete bool               `json:"incomplete,omitempty"`
		Challenge  *rcChallengeResult `json:"challenge,omitempty"`
	}{res.Review, false, res})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"incomplete"`) {
		t.Fatalf("an open item marked the emitted bundle incomplete:\n%s", s)
	}
	for _, want := range []string{`"status":"supported"`, `"status":"unresolved"`, `"remedy":"` + rcEscapeRemedy + `"`, `"withheldRemedy":"` + rcFalseCDRemedy + `"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("emitted bundle missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"remedy":"`+rcFalseCDRemedy) {
		t.Fatalf("withheld remedy published as actionable in the bundle:\n%s", s)
	}
}

// An unresolved decision cannot silently disappear — whether the challenger
// marked it unverified or never assessed it at all. It is withheld from the
// published decisions, listed under "Could not verify" with its reason, and
// caps readiness, without marking the whole review incomplete.
func TestPublicationUnresolvedDecisionIsWithheld(t *testing.T) {
	ev := []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}
	for _, tc := range []struct {
		name           string
		decisions      []rcDecisionCheck
		wantUnresolved bool
		wantPublished  bool
		wantReason     string
	}{
		{"unverified", []rcDecisionCheck{{Decision: rcTestDecision, Verdict: "unverified", Reason: "the terminal panel is outside this repo"}}, true, false, "the terminal panel is outside this repo"},
		{"never assessed", nil, true, false, "the challenge did not assess this decision"},
		{"supported", []rcDecisionCheck{{Decision: rcTestDecision, Verdict: "supported", Reason: "the diff prefixes cd", Evidence: ev}}, false, true, ""},
		{"refuted", []rcDecisionCheck{{Decision: rcTestDecision, Verdict: "refuted", Reason: "the change does no such thing", Evidence: ev}}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			a.MergeReady = 5
			a.Checks[1].Verdict, a.Checks[1].Finding = "refuted", "" // no confirmed defect: only the decision is in play
			a.Decisions = tc.decisions
			res := rcMustValidate(t, a, []string{rcTestDecision})
			if (len(res.unresolved()) > 0) != tc.wantUnresolved {
				t.Fatalf("unresolved()=%v, want open=%v:\n%s", res.unresolved(), tc.wantUnresolved, res.Review)
			}
			published := strings.Contains(res.Review, "## Decisions (need your call)\n- "+rcTestDecision)
			if published != tc.wantPublished {
				t.Fatalf("decision published=%v, want %v:\n%s", published, tc.wantPublished, res.Review)
			}
			if tc.wantUnresolved {
				if !strings.Contains(res.Review, "\n## Could not verify\n") || !strings.Contains(res.Review, "- Decision: "+rcTestDecision+" — "+tc.wantReason) {
					t.Fatalf("unresolved decision not listed with its reason:\n%s", res.Review)
				}
				if got := res.unresolved(); len(got) != 1 || got[0] != "decision: "+rcTestDecision {
					t.Fatalf("unresolved(): %v", got)
				}
			}
			_, _, decisions, _, readiness, _ := rcParseReviewOutput(res.Review)
			if (len(decisions) == 1) != tc.wantPublished {
				t.Fatalf("coda DECISIONS=%v, want published=%v", decisions, tc.wantPublished)
			}
			// Proposed 5: an open or unsettled decision is never a clean merge.
			if (tc.wantUnresolved || tc.wantPublished) && readiness > finding.ReadinessDecideThenMerge {
				t.Fatalf("readiness %d with an open or unsettled decision", readiness)
			}
		})
	}
	// A decision the draft never made cannot be introduced by the challenger.
	a := rcCDChecks()
	a.Decisions = []rcDecisionCheck{{Decision: "rewrite the terminal panel", Verdict: "supported", Reason: "r", Evidence: ev}}
	if res := rcMustValidate(t, a, nil); strings.Contains(res.Review, "rewrite the terminal panel") || len(res.Decisions) != 0 {
		t.Fatalf("challenger-invented decision published:\n%s", res.Review)
	}
}

// The summary, the findings, the counts and the coda derive from the same
// results, so they agree in every combination.
func TestPublicationSummaryFindingsAndCodaAgree(t *testing.T) {
	for _, verdicts := range [][2]string{
		{"supported", "supported"}, {"supported", "refuted"}, {"refuted", "refuted"},
		{"unverified", "supported"}, {"refuted", "unverified"}, {"unverified", "unverified"},
	} {
		t.Run(verdicts[0]+"+"+verdicts[1], func(t *testing.T) {
			a := rcCDChecks()
			for i, v := range verdicts {
				a.Checks[i].Verdict, a.Checks[i].Finding = v, "description "+a.Checks[i].Issue
			}
			res := rcMustValidate(t, a, nil)
			var supported []string
			refuted, unresolved := 0, 0
			for _, r := range res.Allegations {
				switch r.Status {
				case rcStatusSupported:
					supported = append(supported, rcPublishedIssue(r))
				case rcStatusRefuted:
					refuted++
				default:
					unresolved++
				}
			}
			prose, issues, _, match, readiness, summary := rcParseReviewOutput(res.Review)
			if strings.Join(issues, "\n") != strings.Join(supported, "\n") {
				t.Fatalf("coda ISSUES %v != supported results %v", issues, supported)
			}
			if want := rcDeriveSummary(len(supported), refuted, unresolved, 0, match, readiness); summary != want {
				t.Fatalf("summary %q != derived %q", summary, want)
			}
			if got := strings.Count(prose, "\n### "); got != len(supported) {
				t.Fatalf("%d finding sections for %d supported results:\n%s", got, len(supported), prose)
			}
			for _, r := range res.Allegations {
				inFindings := strings.Contains(findingsSection(res.Review), "### "+rcPublishedIssue(r))
				if inFindings != (r.Status == rcStatusSupported) {
					t.Fatalf("allegation %q status=%s but published-as-finding=%v", r.Issue, r.Status, inFindings)
				}
			}
			if len(res.unresolved()) != unresolved || strings.Contains(summary, "could not be verified") != (unresolved > 0) || strings.Contains(prose, "\n## Could not verify\n") != (unresolved > 0) {
				t.Fatalf("unresolved=%d summary=%q", unresolved, summary)
			}
			if len(supported) == 0 && unresolved == 0 && !strings.Contains(prose, "No proposed defect was confirmed") {
				t.Fatalf("an all-refuted review does not say so:\n%s", prose)
			}
		})
	}
}

// Readiness is only ever capped from the challenger's proposal, never raised,
// and an incomplete review is never scored as a clean merge.
func TestPublicationReadinessIsOnlyCapped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verdicts [2]string
		proposed int
		want     finding.Readiness
	}{
		{"confirmed defect caps 5 to small-fixes", [2]string{"refuted", "supported"}, 5, finding.ReadinessSmallFixes},
		{"unresolved caps 5 to decide-then-merge", [2]string{"unverified", "refuted"}, 5, finding.ReadinessDecideThenMerge},
		{"all refuted keeps the proposal", [2]string{"refuted", "refuted"}, 5, finding.Readiness(5)},
		{"a cautious proposal is never raised", [2]string{"refuted", "refuted"}, 2, finding.Readiness(2)},
		{"a cautious proposal with a defect is never raised", [2]string{"refuted", "supported"}, 1, finding.Readiness(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			a.MergeReady = tc.proposed
			for i, v := range tc.verdicts {
				a.Checks[i].Verdict, a.Checks[i].Finding = v, "d"
			}
			_, _, _, _, readiness, _ := rcParseReviewOutput(rcMustValidate(t, a, nil).Review)
			if readiness != tc.want {
				t.Fatalf("readiness %d, want %d", readiness, tc.want)
			}
		})
	}
}

// A supported verdict with no separate description publishes the allegation
// itself as the finding: it is the reviewer's own text, the exact claim the
// check confirmed with evidence, so nothing is invented and a confirmed defect
// is not demoted to an open item.
func TestPublicationSupportedWithoutFindingPublishesTheAllegation(t *testing.T) {
	a := rcCDChecks()
	a.Checks = append([]rcIssueCheck(nil), a.Checks...)
	a.Checks[0] = rcIssueCheck{Issue: rcFalseCDIssue, Verdict: "supported", Reason: "real", Remedy: rcFalseCDRemedy, Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}}
	res := rcMustValidate(t, a, nil)
	if got := res.Allegations[0]; got.Status != rcStatusSupported || got.Finding != rcFalseCDIssue || got.Remedy != rcFalseCDRemedy || got.WithheldRemedy != "" {
		t.Fatalf("supported-without-finding: %+v", got)
	}
	f := findingsSection(res.Review)
	if len(res.unresolved()) != 0 || !strings.Contains(f, "### "+rcFalseCDIssue) || !strings.Contains(f, "**Remedy:** "+rcFalseCDRemedy) || !strings.Contains(f, rcEscapeFinding) {
		t.Fatalf("allegation not published as a finding, or the other finding lost:\n%s", res.Review)
	}
}

// The fast path publishes a partial result, with the open item listed under
// "Could not verify", instead of returning an error and nothing.
func TestFastReviewReportsUnresolved(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls == 1 {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestReview(rcFalseCDIssue)}}}, nil
		}
		a := rcChallengeAnswer{IntentMatch: "partial", MergeReady: 4,
			Checks: []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", Reason: "needs a shell"}}}
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestAnswer(t, a)}}}, nil
	}}
	got, res, err := rcRunFastReview(context.Background(), p, "test", "test", "", "", "test", "", rcCDSource, nil)
	if err != nil {
		t.Fatalf("fast review withheld a publishable-but-incomplete result: %v", err)
	}
	if res == nil || len(res.unresolved()) != 1 {
		t.Fatalf("fast review did not report the unresolved allegation: %+v", res)
	}
	if !strings.Contains(couldNotVerifySection(got), "- "+rcFalseCDIssue) || strings.Contains(got, "## Findings") {
		t.Fatalf("fast review body: %s", got)
	}
}

// Replay of a CAPTURED rewrite inconsistency, using the challenger's original
// per-allegation judgments byte for byte.
//
// testdata/rewrite-inconsistency/smoke-429-fast.json is a real GLM-5.2
// submission (2026-09-17, the #429 change, the code merged as kai-cli#118). Its
// one per-allegation judgment was valid — "refuted", two resolving location
// citations — but its rewritten review began with the REVIEW-DATA marker, so
// main rejected it ("challenge did not produce a complete revised review") and
// withheld the whole review (the marker opened the text and no SUMMARY /
// INTENT_MATCH / MERGE_READY followed). The production failure of the same class on
// kai-desktop#429 ("revised review added an unchecked or rejected allegation",
// run 89bdabb8) logged only its error line; its payload was never captured, and
// eight captured attempts with main's binary on the same change did not
// reproduce that exact error (see testdata/rewrite-inconsistency/PROVENANCE), so
// this is the captured specimen of the class.
//
// The captured payload predates intent_match/merge_ready as top-level fields
// (they lived inside the rewritten review). The test supplies them from the
// DRAFT's own coda via the production parser, and changes nothing else: the
// checks — issue, verdict, reason, evidence — are the captured ones.
func TestPublicationReplaysCapturedRewriteInconsistency(t *testing.T) {
	raw, err := os.ReadFile("testdata/rewrite-inconsistency/smoke-429-fast.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		MainError string   `json:"mainError"`
		Draft     string   `json:"draft"`
		Issues    []string `json:"issues"`
		Sources   []string `json:"sources"`
		Submitted string   `json:"submitted"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var captured map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fx.Submitted), &captured); err != nil {
		t.Fatal(err)
	}
	// The specimen is the one we think it is: the rewrite is what main
	// rejected, and the per-allegation judgment underneath it is sound.
	var rewritten string
	if err := json.Unmarshal(captured["review"], &rewritten); err != nil {
		t.Fatal(err)
	}
	// main's condition for "did not produce a complete revised review", verbatim:
	// exactly one marker, non-empty prose, a summary, a known intent verdict and
	// a valid readiness. The captured rewrite opens with the marker and carries
	// no coda fields after it, so the last three fail.
	prose, _, _, rwMatch, rwReadiness, rwSummary := rcParseReviewOutput(rewritten)
	mainRejects := strings.Count(rewritten, rcReviewDataMarker) != 1 || prose == "" || rwSummary == "" || rwMatch == finding.MatchUnknown || !rwReadiness.Valid()
	if !mainRejects || !strings.HasPrefix(strings.TrimSpace(rewritten), rcReviewDataMarker) {
		t.Fatalf("the captured rewrite is not the one main rejected: summary=%q match=%q readiness=%d", rwSummary, rwMatch, rwReadiness)
	}
	var checks []rcIssueCheck
	if err := json.Unmarshal(captured["checks"], &checks); err != nil {
		t.Fatal(err)
	}
	if len(fx.Issues) != 1 || len(checks) != 1 || checks[0].Issue != fx.Issues[0] || checks[0].Verdict != "refuted" || len(checks[0].Evidence) != 2 {
		t.Fatalf("captured judgments: %+v", checks)
	}

	// Replay: the captured checks, untouched, plus the draft's own proposal.
	_, draftIssues, draftDecisions, match, readiness, _ := rcParseReviewOutput(fx.Draft)
	if len(draftIssues) != 1 || draftIssues[0] != fx.Issues[0] || len(draftDecisions) != 0 {
		t.Fatalf("draft coda: issues=%v decisions=%v", draftIssues, draftDecisions)
	}
	captured["intent_match"], _ = json.Marshal(string(match))
	captured["merge_ready"], _ = json.Marshal(int(readiness))
	replay, err := json.Marshal(captured)
	if err != nil {
		t.Fatal(err)
	}
	var srcs []rcSource
	for _, s := range fx.Sources {
		srcs = append(srcs, rcRowSource(s))
	}
	res, _, err := rcValidateChallenge(string(replay), fx.Issues, draftDecisions, srcs)
	if err != nil {
		t.Fatalf("the captured judgments no longer publish: %v", err)
	}
	if len(res.Allegations) != 1 || res.Allegations[0].Status != rcStatusRefuted || len(res.Allegations[0].Evidence) != 2 {
		t.Fatalf("result: %+v", res)
	}
	_, issues, _, _, got, summary := rcParseReviewOutput(res.Review)
	if len(issues) != 0 || strings.Contains(res.Review, "## Findings") || strings.Contains(res.Review, fx.Issues[0]) {
		t.Fatalf("the refuted allegation was published:\n%s", res.Review)
	}
	if !strings.Contains(summary, "0 confirmed findings, 1 refuted") || got > readiness {
		t.Fatalf("summary %q readiness %d (draft proposed %d)", summary, got, readiness)
	}
}
