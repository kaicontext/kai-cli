package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The provider carries an HTTP status only in its error text, in the shapes
// kai-engine's providers write. Each must land in its own category, or the
// server learns "fact_check_failed" and nothing about why
// (falkordb/falkordb#3049, 2026-10-03).
func TestFailureCategoryReadsUpstreamStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("challenge call: %w", errors.New("kailab provider: 429: slow down")), "rate_limited"},
		{fmt.Errorf("challenge call: %w", errors.New("openrouter provider: 503: overloaded")), "upstream_5xx"},
		{errors.New("kailab provider (openai stream): 500: boom"), "upstream_5xx"},
		{errors.New("kailab provider (openai): 400: bad request"), "upstream_4xx"},
		{errors.New("openai provider: 413: too big"), "context_overflow"},
		{fmt.Errorf("challenge call: %w", context.DeadlineExceeded), "timeout"},
		{fmt.Errorf("challenge call: %w", context.Canceled), "canceled"},
		{errors.New("kailab provider: parsing response: unexpected EOF"), "unparseable"},
		{nil, ""},
		{errors.New("something nobody anticipated"), "other"},
	} {
		if got := rcFailureCategory(tc.err); got != tc.want {
			t.Errorf("rcFailureCategory(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// The gate's own failures are placed by their wording, so the wording is held
// here: changing a message in review_commit_challenge.go without this table
// would quietly turn its category into "other".
func TestFailureCategoryKnowsTheGatesOwnFailures(t *testing.T) {
	src, err := os.ReadFile("review_commit_challenge.go")
	if err != nil {
		t.Fatal(err)
	}
	for msg, want := range map[string]string{
		"challenge answer was truncated at":                  "truncated",
		"challenge evidence exceeds":                         "evidence_too_large",
		"challenge kept requesting lookups past its limit":   "tool_loop",
		"challenge kept requesting unavailable tool":         "tool_loop",
		"challenge submitted before its pending experiments": "tool_loop",
		"challenge ended without a complete answer":          "no_answer",
	} {
		if !strings.Contains(string(src), msg) {
			t.Errorf("review_commit_challenge.go no longer says %q; update rcFailureCategory", msg)
		}
		if got := rcFailureCategory(errors.New(msg + " (detail)")); got != want {
			t.Errorf("rcFailureCategory(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestIncompleteReasonNamesTheStageThatStopped(t *testing.T) {
	for _, tc := range []struct {
		name string
		inc  *rcIncomplete
		want rcIncompleteReason
	}{
		{"challenge", &rcIncomplete{
			Model: "review-m", FinishReason: "end_turn",
			ChallengeFailure: "challenge call: kailab provider: 429: x", ChallengeCategory: "rate_limited", ChallengeModel: "gate-m",
		}, rcIncompleteReason{Stage: "challenge", Category: "rate_limited", Model: "gate-m", FinishReason: "end_turn"}},
		{"conclusion", &rcIncomplete{
			Model: "review-m", FinishReason: "time_budget",
			ConclusionCategory: "timeout", ConclusionModel: "concl-m",
		}, rcIncompleteReason{Stage: "conclusion", Category: "timeout", Model: "concl-m", FinishReason: "time_budget"}},
		{"review", &rcIncomplete{Model: "review-m", FinishReason: "max_turns"},
			rcIncompleteReason{Stage: "review", Model: "review-m", FinishReason: "max_turns"}},
	} {
		got := rcIncompleteReasonOf(tc.inc)
		if got == nil || *got != tc.want {
			t.Errorf("%s: rcIncompleteReasonOf = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if rcIncompleteReasonOf(nil) != nil {
		t.Error("a run with no facts about its ending has no reason")
	}
}

// The reason is stored and logged by the server, so it must never carry the
// error's text, which can quote the provider's response body.
func TestIncompleteReasonCarriesNoErrorText(t *testing.T) {
	r := rcIncompleteReasonOf(&rcIncomplete{
		ChallengeFailure:  "challenge call: kailab provider: 500: SECRET-BODY",
		ChallengeCategory: "upstream_5xx",
		ChallengeModel:    "gate-m",
	})
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "SECRET-BODY") {
		t.Errorf("incompleteReason leaks the error text: %s", out)
	}
}
