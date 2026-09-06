package main

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

// MERGE_READY is the reviewer answering what should happen to the
// branch next. It exists because the review loop had no way to say
// "done": a fix cycle stops when it runs out of risk claims or spends
// its budget, both of which are silences. On 2026-09-06 the reviewer
// wrote "I found no correctness bug" in prose while nothing downstream
// could read it, so the findings sat awaiting a verdict and the loop
// stayed armed.

func codaWith(lines string) string {
	return "The change reads fine.\n\n===REVIEW-DATA===\n" + lines + "\nSUMMARY: fine\n"
}

func TestParsesMergeReady(t *testing.T) {
	for _, c := range []struct {
		line string
		want finding.Readiness
		why  string
	}{
		{"MERGE_READY: 5", finding.ReadinessMerge, "plain value"},
		{"MERGE_READY: 1", finding.ReadinessBlocked, "the bottom of the scale is a real score"},
		{"MERGE_READY: 4 — pending your call on the 220ms", finding.ReadinessDecideThenMerge,
			"first field only; the rest is commentary, as with INTENT_MATCH"},
		{"merge_ready: 3", finding.ReadinessSmallFixes, "the key is matched case-insensitively"},
		{"MERGE_READY: 4.", finding.ReadinessDecideThenMerge, "a trailing period is punctuation, not a decimal"},
	} {
		_, _, _, _, got, _ := rcParseReviewOutput(codaWith(c.line))
		if got != c.want {
			t.Errorf("%q → %d, want %d (%s)", c.line, got, c.want, c.why)
		}
	}
}

// Anything the reviewer did not actually say must stay UNKNOWN. A
// missing or malformed score is silence, and silence must never render
// as "do not merge" — that would block work nobody objected to.
func TestUnparseableMergeReadyStaysUnknown(t *testing.T) {
	for _, line := range []string{
		"MERGE_READY: 0",         // below the scale
		"MERGE_READY: 6",         // above it
		"MERGE_READY: -2",        // nonsense
		"MERGE_READY: high",      // a word, not a score
		"MERGE_READY:",           // the key with nothing after it
		"INTENT_MATCH: verified", // no MERGE_READY line at all
	} {
		_, _, _, _, got, _ := rcParseReviewOutput(codaWith(line))
		if got != finding.ReadinessUnknown {
			t.Errorf("%q → %d, want ReadinessUnknown — an unsaid score must not become a verdict", line, got)
		}
	}
}

// A review written before this field existed must still parse, with
// every other field intact.
func TestMergeReadyIsOptionalForOlderReviews(t *testing.T) {
	raw := "Looks good.\n\n===REVIEW-DATA===\nINTENT_MATCH: verified\nSUMMARY: solid\nISSUES:\n- a.go:1 — something\n"
	prose, risks, _, match, readiness, note := rcParseReviewOutput(raw)
	if readiness != finding.ReadinessUnknown {
		t.Errorf("readiness = %d, want unknown", readiness)
	}
	if match != finding.MatchVerified || note != "solid" || len(risks) != 1 || !strings.Contains(prose, "Looks good") {
		t.Errorf("the rest of the coda must be unaffected: match=%v note=%q risks=%d", match, note, len(risks))
	}
}

// The coda line must not leak into the human review, same as every
// other machine field.
func TestMergeReadyDoesNotLeakIntoProse(t *testing.T) {
	prose, _, _, _, r, _ := rcParseReviewOutput(codaWith("MERGE_READY: 5"))
	if strings.Contains(prose, "MERGE_READY") {
		t.Errorf("the machine coda leaked into the review the author reads:\n%s", prose)
	}
	if r != finding.ReadinessMerge {
		t.Errorf("readiness = %d, want 5", r)
	}
}
