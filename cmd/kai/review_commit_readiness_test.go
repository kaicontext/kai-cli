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

// The specimen. On 2026-09-06, the first review to run on an image that
// carried the MERGE_READY prompt answered in PROSE and emitted no coda
// line at all — this is its closing paragraph, verbatim. The score was
// right and unreadable: the parser saw nothing, the bundle carried no
// readiness, and the surface built to render it rendered nothing.
const rcProseScoreSpecimen = "The backend validation, the known-project path check, and the error " +
	"plumbing are all solid.\n\n" +
	"**Merge readiness:** small fixes first — the placeholder bug is real and " +
	"quick to fix, and the rest is your call on the 300ms tradeoff.\n"

func TestScoreIsReadWhereverTheModelPutIt(t *testing.T) {
	raw := rcProseScoreSpecimen + "\n===REVIEW-DATA===\nINTENT_MATCH: verified\nSUMMARY: fine\n"
	_, _, _, _, got, _ := rcParseReviewOutput(raw)
	if got != finding.ReadinessSmallFixes {
		t.Fatalf("the score the reviewer actually gave was dropped: got %d, want %d",
			got, finding.ReadinessSmallFixes)
	}
}

func TestProseScoreFormsThatMustParse(t *testing.T) {
	// The model was shown Label()'s vocabulary in the prompt, so these
	// are the words it reaches for when it writes a sentence instead of
	// a coda line.
	for _, c := range []struct {
		line string
		want finding.Readiness
		why  string
	}{
		{"**Merge readiness:** small fixes first", finding.ReadinessSmallFixes, "the specimen's shape"},
		{"Merge readiness: ready to merge", finding.ReadinessMerge, "bare, no markdown"},
		{"MERGE READINESS: do not merge", finding.ReadinessBlocked, "shouted, spaced"},
		{"Merge-readiness: needs work", finding.ReadinessNeedsWork, "hyphenated"},
		{"Readiness: your call, then merge", finding.ReadinessDecideThenMerge, "the short key"},
		{"`Merge readiness:` 2", finding.ReadinessNeedsWork, "a digit still wins in prose form"},
		{"### Merge readiness: 5", finding.ReadinessMerge, "written as a heading"},
	} {
		raw := "Prose.\n\n" + c.line + "\n\n===REVIEW-DATA===\nSUMMARY: x\n"
		if _, _, _, _, got, _ := rcParseReviewOutput(raw); got != c.want {
			t.Errorf("%q → %d, want %d (%s)", c.line, got, c.want, c.why)
		}
	}
}

// The prose fallback must not manufacture a score out of a sentence.
// Only a LABELLED line counts: the reviewer says "ready to merge" in
// ordinary prose all the time, and reading that as a 5 would invent a
// verdict nobody gave — the same mistake, pointed the other way.
func TestProseScoreDoesNotInventAVerdict(t *testing.T) {
	for _, prose := range []string{
		"Once the placeholder bug is fixed this is ready to merge.",
		"I do not merge things I cannot run, and I could not run this.",
		"The concurrency comment needs work before a reader trusts it.",
		"Your call, then merge — but the 300ms is a real cost.",
		"readiness is not something I can judge from the diff alone",
	} {
		raw := prose + "\n\n===REVIEW-DATA===\nSUMMARY: x\n"
		if _, _, _, _, got, _ := rcParseReviewOutput(raw); got != finding.ReadinessUnknown {
			t.Errorf("%q → %d, want UNKNOWN — a sentence is not a score", prose, got)
		}
	}
}

// The label must sit at the START of the value. A substring match reads
// the negation of a label as the label: "not ready to merge" contains
// "ready to merge", and scoring that a 5 would turn the reviewer's
// clearest refusal into its opposite.
func TestProseScoreLabelIsAnchored(t *testing.T) {
	for _, line := range []string{
		"Merge readiness: this is not ready to merge without the placeholder fix",
		"Merge readiness: I would not merge it, though nothing here is unsafe",
		"Merge readiness: the placeholder bug means this needs work",
	} {
		raw := "Prose.\n\n" + line + "\n\n===REVIEW-DATA===\nSUMMARY: x\n"
		if _, _, _, _, got, _ := rcParseReviewOutput(raw); got != finding.ReadinessUnknown {
			t.Errorf("%q → %d, want UNKNOWN — a label buried in a sentence is not the score, "+
				"and half of these say the OPPOSITE of the label they contain", line, got)
		}
	}
}

// The coda still wins. It is where the score is asked for, and a model
// that answers there and then muses in prose must not be overridden by
// its own musing.
func TestCodaBeatsProse(t *testing.T) {
	raw := "**Merge readiness:** ready to merge\n\n===REVIEW-DATA===\nMERGE_READY: 2\nSUMMARY: x\n"
	if _, _, _, _, got, _ := rcParseReviewOutput(raw); got != finding.ReadinessNeedsWork {
		t.Errorf("got %d, want the coda's 2 — the prose is a restatement, not a second opinion", got)
	}
}
