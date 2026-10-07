package main

import (
	"context"
	"encoding/json"
	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
	"os"
	"strings"
	"testing"
)

func TestReviewOutputContract(t *testing.T) {
	for _, raw := range []string{
		`{"intent_match":"partial","merge_ready":2,"summary":"check","findings":[{"file":"a.go","line":1,"claim":"bad","evidence":["caller passes nil"]}],"decisions":[],"limitations":[]}`,
		`{"intent_match":"partial","merge_ready":2,"summary":"check","findings":["a.go:1 — bad"],"decisions":[],"limitations":[]}`,
		"**intent_match:** partial\n**merge_ready:** 2\n**summary:** check\n**findings:**\n- a.go:1 — bad\n",
		"INTENT_MATCH: partial\nMERGE_READY: 2\nSUMMARY: check\nISSUES:\n- a.go:1 — bad\n",
	} {
		o, err := rcDecodeReview(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(o.Findings) != 1 || !rcUsableCoda(raw) || rcNeedsConclusion(raw) {
			t.Fatalf("lost findings: %+v", o)
		}
		_, issues, _, _, _, _ := rcParseReviewOutput(o.draft())
		if len(issues) != 1 {
			t.Fatal(issues)
		}
	}
	for _, raw := range []string{"", "I found nothing.", "===REVIEW-DATA===", "INTENT_MATCH: verified\nSUMMARY: clean", `{"intent_match":"verified","merge_ready":5}`, "```json\nnot JSON\n```"} {
		if _, err := rcDecodeReview(raw); err == nil {
			t.Fatalf("incomplete output accepted: %q", raw)
		}
	}
	clean := `{"intent_match":"verified","merge_ready":5,"summary":"clean","findings":[],"decisions":[],"limitations":[]}`
	if _, err := rcDecodeReview(clean); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRequiresEvidenceBackedVerification(t *testing.T) {
	raw := rcReviewOutput{IntentMatch: "partial", MergeReady: 2, Findings: []string{"a.go:1 — wrong draft"}}.draft()
	if rcPublicationReady(raw, nil) {
		t.Fatal("missing verification passed")
	}
	r := &rcChallengeResult{Allegations: []rcAllegationResult{{Issue: "a.go:1 — wrong draft", Finding: "corrected explanation", Status: rcStatusSupported, Evidence: []rcCitationRef{{Source: 1, LineStart: 1, LineEnd: 1}}}}}
	if rcPublicationReady(raw, r) {
		t.Fatal("draft text passed despite correction")
	}
	raw = rcReviewOutput{IntentMatch: "partial", MergeReady: 2, Findings: []string{rcPublishedIssue(r.Allegations[0])}}.draft()
	if !rcPublicationReady(raw, r) {
		t.Fatal("verified finding rejected")
	}
	r.Allegations[0].Unchecked = true
	if rcPublicationReady(raw, r) {
		t.Fatal("unchecked finding passed")
	}
}

func TestChallengeFindingIDsDoNotRequireEchoedText(t *testing.T) {
	a := rcCDChecks()
	for i := range a.Checks {
		a.Checks[i].FindingID = rcFindingKey(a.Checks[i].Issue)
		a.Checks[i].Issue = "a paraphrase that cannot match"
	}
	data, _ := json.Marshal(a)
	r, _, err := rcValidateChallenge(string(data), rcCDIssues, nil, rcCDSources)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Allegations) != len(rcCDIssues) {
		t.Fatal("lost check")
	}
	a.Checks[0].FindingID = "unknown"
	data, _ = json.Marshal(a)
	if _, _, err = rcValidateChallenge(string(data), rcCDIssues, nil, rcCDSources); err == nil {
		t.Fatal("unknown ID accepted")
	}
	a = rcCDChecks()
	a.Checks[0].FindingID = rcFindingKey(rcCDIssues[0])
	a.Checks[1].FindingID = a.Checks[0].FindingID
	data, _ = json.Marshal(a)
	if _, _, err = rcValidateChallenge(string(data), rcCDIssues, nil, rcCDSources); err == nil {
		t.Fatal("duplicate ID accepted")
	}
}

func TestVerifiedHeadingDoesNotRetainDraft(t *testing.T) {
	a := rcCDChecks()
	a.Checks[1].Finding = "SafeParse returns a wrapper; the caller must use its data."
	r := rcMustValidate(t, a, nil)
	body := findingsSection(r.Review)
	if strings.Contains(body, rcEscapeIssue) || !strings.Contains(body, a.Checks[1].Finding) {
		t.Fatal(body)
	}
}

func TestRun28SavedPublicationRegressions(t *testing.T) {
	data, err := os.ReadFile("testdata/review-output/run28-cal11059-allegation.json")
	if err != nil {
		t.Fatal(err)
	}
	var allegation rcAllegationResult
	if err = json.Unmarshal(data, &allegation); err != nil {
		t.Fatal(err)
	}
	body := rcAssembleReview(nil, nil, []rcAllegationResult{allegation}, nil, finding.MatchPartial, finding.Readiness(2), "One verified finding.")
	if strings.Contains(body, "so it validates the wrong object") || !strings.Contains(body, allegation.Finding) {
		t.Fatal(body)
	}
	if !rcPublicationReady(body, &rcChallengeResult{Allegations: []rcAllegationResult{allegation}}) {
		t.Fatal("verified finding rejected")
	}
	data, err = os.ReadFile("testdata/review-output/run28-cal8330-published.json")
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Review string `json:"review"`
	}
	if err = json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if rcPublicationReady(old.Review, nil) {
		t.Fatal("run28's unverified publication accepted")
	}
}

func TestStructuredDiscoveryMergesSweepWithoutDroppingFindings(t *testing.T) {
	raw := `{"intent_match":"partial","merge_ready":2,"summary":"check","findings":[{"file":"a.go","line":1,"claim":"nil dereference","evidence":[]}],"decisions":[],"limitations":[]}`
	merged := rcDraftWithSweep(raw, []string{"b.go:2 — missing authorization"})
	output, err := rcDecodeReview(merged)
	if err != nil || len(output.Findings) != 2 {
		t.Fatalf("merged discovery: %+v %v", output, err)
	}
}

func TestFastOutputRepairThenVerification(t *testing.T) {
	calls := 0
	answer := rcCDChecks()
	for i := range answer.Checks {
		answer.Checks[i].FindingID = rcFindingKey(answer.Checks[i].Issue)
		answer.Checks[i].Issue = ""
		answer.Checks[i].Evidence = []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}
	}
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls <= 2 && req.OutputJSONSchema == nil {
			t.Fatal("discovery/repair missing schema")
		}
		if calls == 1 {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: "unstructured draft"}}}, nil
		}
		if calls == 2 {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestReview(rcCDIssues...)}}}, nil
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, answer)}}}, nil
	}}
	raw, r, err := rcRunFastReview(context.Background(), p, "draft", "verifier", "", "", "test", "", rcCDSource, nil)
	if err != nil || calls != 3 || !rcPublicationReady(raw, r) {
		t.Fatalf("calls=%d result=%v err=%v", calls, r, err)
	}
	if len(r.OutputAttempts) != 2 || r.OutputAttempts[0].Error == "" {
		t.Fatal("repair diagnostics lost")
	}
}

func TestFastFailedRepairNeverReachesVerification(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if req.Model == "verifier" {
			t.Fatal("verification reached with invalid discovery")
		}
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: "still malformed"}}}, nil
	}}
	raw, r, err := rcRunFastReview(context.Background(), p, "draft", "verifier", "", "", "test", "", rcCDSource, nil)
	if err == nil || raw != "" || r != nil || calls != 2 {
		t.Fatalf("invalid output escaped: calls=%d %q %v %v", calls, raw, r, err)
	}
	failure, ok := err.(*rcDiscoveryError)
	if !ok || len(failure.Attempts) != 2 {
		t.Fatal("failed response diagnostics lost")
	}
}
