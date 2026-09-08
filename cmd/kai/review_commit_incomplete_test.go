package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
)

// A review that ran for real and never wrote itself down must still be able to
// say so. Before this, rcParseReviewOutput saw nothing and the command returned
// an error having emitted NOTHING — and because the CI step runs this CLI under
// set -eu, the job died before the ingest and the PR was told the review could
// not be finished (kai-server#184, 2026-09-08: 12m7s, 39-file diff, nothing
// posted).
func TestIncompleteProseReportsHowFarTheRunGot(t *testing.T) {
	got := rcIncompleteProse(&rcIncomplete{
		FinishReason: string(message.FinishReasonTimeBudget),
		Elapsed:      12*time.Minute + 7*time.Second,
		Turns:        41,
		FilesRead:    []string{"api/ci.go", "db/ci.go"},
	})
	for _, want := range []string{
		"did not finish",
		"12m7s",
		"41 turns",
		"ran out of time",
		"api/ci.go",
		"db/ci.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("incomplete report is missing %q:\n%s", want, got)
		}
	}
	// It must never read as a clean review.
	if strings.Contains(strings.ToLower(got), "looks good") {
		t.Error("an unfinished review must not read as an approval")
	}
	if !strings.Contains(got, `"not reviewed"`) {
		t.Error("the report must say explicitly that this is not a verdict on the change")
	}
}

// No facts at all (the agent run itself failed before producing a transcript)
// is the genuinely-empty case: no salvage, and the caller keeps the hard error.
func TestIncompleteProseIsEmptyWithoutFacts(t *testing.T) {
	if got := rcIncompleteProse(nil); got != "" {
		t.Errorf("rcIncompleteProse(nil) = %q, want empty so the caller still fails hard", got)
	}
}

// A run that opened nothing says so rather than rendering an empty bullet list.
func TestIncompleteProseHandlesNoFilesRead(t *testing.T) {
	got := rcIncompleteProse(&rcIncomplete{FinishReason: "end_turn", Elapsed: time.Minute, Turns: 3})
	if !strings.Contains(got, "had not opened any files") {
		t.Errorf("expected an explicit no-files line:\n%s", got)
	}
	if strings.Contains(got, "- \n") {
		t.Error("rendered an empty bullet")
	}
}

// The files list comes off the run's own tool calls, deduped and sorted, from
// either spelling of the argument.
func TestFilesReadDedupesAcrossToolCallSpellings(t *testing.T) {
	tr := []message.Message{{
		Role: message.RoleAssistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "kai_view", Input: `{"file_path":"b.go"}`},
			message.ToolCall{ID: "2", Name: "kai_grep", Input: `{"path":"a.go"}`},
			message.ToolCall{ID: "3", Name: "kai_view", Input: `{"file_path":"b.go"}`},
			message.ToolCall{ID: "4", Name: "kai_impact", Input: `{"symbol":"Foo"}`},
			message.ToolCall{ID: "5", Name: "kai_view", Input: `not json`},
		},
	}}
	got := rcFilesRead(tr)
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Errorf("rcFilesRead = %v, want [a.go b.go] (deduped, sorted, non-file calls ignored)", got)
	}
}

// The sentinel is what lets the command emit a bundle and still exit non-zero.
func TestIncompleteReviewErrorSaysItDidNotFinish(t *testing.T) {
	if rcErrIncompleteReview == nil {
		t.Fatal("no sentinel: the command cannot both deliver a partial and fail")
	}
	if !strings.Contains(rcErrIncompleteReview.Error(), "did not finish") {
		t.Errorf("sentinel message should name the real problem, got %q", rcErrIncompleteReview)
	}
}
