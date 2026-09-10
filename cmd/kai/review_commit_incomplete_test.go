package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	// Empty root: nothing resolves, so nothing is confirmed a directory and
	// every named path is kept. Directory filtering has its own test below.
	got := rcFilesRead(tr, "")
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

// The prose alone is not enough for the renderer downstream: kai-server picks
// the review's headline from the finding's COUNTS, and an incomplete bundle's
// counts are indistinguishable from a clean review's (no risks, no decisions,
// unknown intent). So the bundle has to carry the fact structurally. Without
// this field the server has only a literal sentence to match on, and two
// timed-out reviews shipped with "Nothing jumped out" as their opening line
// (kai-desktop#304, kai-server#186, 2026-09-08).
func TestIncompleteBundleCarriesTheFlag(t *testing.T) {
	type bundle struct {
		Review     string `json:"review,omitempty"`
		Depth      string `json:"depth,omitempty"`
		Incomplete bool   `json:"incomplete,omitempty"`
	}

	// The shape the JSON branch of runReviewCommit marshals when the run
	// stopped: salvaged prose, and the flag that says it is not a review.
	out, err := json.Marshal(bundle{Review: rcIncompleteProse(&rcIncomplete{
		FinishReason: string(message.FinishReasonTimeBudget),
		Elapsed:      9*time.Minute + 59*time.Second,
		Turns:        27,
	}), Depth: "grounded", Incomplete: true})
	if err != nil {
		t.Fatalf("marshaling bundle: %v", err)
	}
	if !strings.Contains(string(out), `"incomplete":true`) {
		t.Errorf("bundle does not carry the incomplete flag:\n%s", out)
	}

	// omitempty keeps a finished review's bundle byte-identical to what it
	// was before this field existed — the flag appears only when it is true,
	// so a complete review can never be read as an incomplete one.
	done, err := json.Marshal(bundle{Review: "a real review", Depth: "grounded"})
	if err != nil {
		t.Fatalf("marshaling complete bundle: %v", err)
	}
	if strings.Contains(string(done), "incomplete") {
		t.Errorf("a complete review's bundle mentions incomplete:\n%s", done)
	}
}

// The run's account of itself was already gathered on every grounded review and
// thrown away unless the review died. Now it always ships, so a reader can tell
// a clean verdict that read the whole change from a clean verdict that read two
// files.
func TestCoverageShipsOnEveryGroundedRun(t *testing.T) {
	got := rcCoverageOf(&rcIncomplete{
		Elapsed:   4*time.Minute + 19*time.Second,
		Turns:     27,
		FilesRead: []string{"a.go", "b.go"},
	})
	if got == nil {
		t.Fatal("rcCoverageOf dropped a run's manifest")
	}
	if got.Seconds != 259 {
		t.Errorf("Seconds = %d, want 259", got.Seconds)
	}
	if got.Turns != 27 || len(got.FilesRead) != 2 {
		t.Errorf("coverage = %+v, want the run's turns and files", got)
	}

	// The fast pass opens nothing and has no manifest to publish. Nil out
	// keeps `omitempty` dropping the field, rather than publishing a record
	// that claims zero files were read.
	if rcCoverageOf(nil) != nil {
		t.Error("rcCoverageOf(nil) invented a manifest for a pass that opened nothing")
	}
}

// "What I opened — 6 files" listed `frontend` and `frontend/dist`
// (kai-desktop#314); "4 files" listed `cmd/kai` (kai-cli#99). A directory
// listing passes the same "path" argument a file read does, so the manifest
// counted directories as files it had read. A coverage report that miscounts
// its own coverage is the one thing this feature cannot afford.
func TestFilesReadDropsDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "frontend", "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frontend", "dist", "app.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := []message.Message{{
		Role: message.RoleAssistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "kai_grep", Input: `{"path":"frontend"}`},
			message.ToolCall{ID: "2", Name: "kai_grep", Input: `{"path":"frontend/dist"}`},
			message.ToolCall{ID: "3", Name: "kai_view", Input: `{"file_path":"frontend/dist/app.js"}`},
			// Never existed here at all: unresolvable, so it is kept. Dropping
			// it would make changedFilesNotListed accuse the review of
			// skipping a file it opened.
			message.ToolCall{ID: "4", Name: "kai_view", Input: `{"file_path":"/elsewhere/vendored.go"}`},
		},
	}}
	got := rcFilesRead(tr, root)
	want := []string{"/elsewhere/vendored.go", "frontend/dist/app.js"}
	if len(got) != len(want) {
		t.Fatalf("rcFilesRead = %v, want %v — directories must not count as files", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rcFilesRead = %v, want %v", got, want)
			break
		}
	}
}
