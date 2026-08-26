package main

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

// The reviewer's normal output: human prose, then the ===REVIEW-DATA=== coda.
func TestParseReviewOutputCoda(t *testing.T) {
	raw := `This change adds a token-bucket limiter to the webhook handler. The
shape is right, but two things would bite in production.

server/limiter.go:42 — the refill math divides before multiplying, so any
interval under a second refills zero tokens and the bucket starves. Compute
tokens as elapsed*rate/interval instead.

server/handler.go:88 — the map of buckets is read and written from every
request goroutine with no lock; that's a data race under load. Guard it with
a sync.Mutex or use sync.Map.

===REVIEW-DATA===
INTENT_MATCH: partial
SUMMARY: Right approach, but the refill math and an unsynchronized map need fixing before this is safe.
ISSUES:
- server/limiter.go:42 — refill math divides before multiplying, starving the bucket at sub-second intervals
- server/handler.go:88 — bucket map accessed from request goroutines without a lock (data race)
`
	prose, risks, match, note := rcParseReviewOutput(raw)
	if !strings.HasPrefix(prose, "This change adds a token-bucket limiter") {
		t.Fatalf("prose lost: %q", prose)
	}
	if strings.Contains(prose, "REVIEW-DATA") || strings.Contains(prose, "INTENT_MATCH") {
		t.Fatalf("coda leaked into prose: %q", prose)
	}
	if len(risks) != 2 {
		t.Fatalf("risks = %d, want 2: %v", len(risks), risks)
	}
	if match != finding.MatchPartial {
		t.Fatalf("match = %v, want partial", match)
	}
	if !strings.HasPrefix(note, "Right approach") {
		t.Fatalf("note = %q", note)
	}
}

// Legacy strict-block output (pre-harness reviewer) must still parse.
func TestParseReviewOutputLegacyBlock(t *testing.T) {
	raw := `FINDINGS:
- [concurrency] pkg/a.go:10 — map written without lock
INTENT_MATCH: diverges
NOTE: The change does something materially different.
`
	prose, risks, match, note := rcParseReviewOutput(raw)
	if prose != "" {
		t.Fatalf("legacy block should have no prose, got %q", prose)
	}
	if len(risks) != 1 || !strings.Contains(risks[0], "pkg/a.go:10") {
		t.Fatalf("risks = %v", risks)
	}
	if match != finding.MatchDiverges {
		t.Fatalf("match = %v, want diverges", match)
	}
	if note != "The change does something materially different." {
		t.Fatalf("note = %q", note)
	}
}

// A run that produced prose but no coda at all: keep the prose, leave the
// structured fields at their zero values rather than dropping the review.
func TestParseReviewOutputProseOnly(t *testing.T) {
	raw := "Looks solid. The retry loop is bounded and the error paths all wrap."
	prose, risks, match, note := rcParseReviewOutput(raw)
	if prose != raw {
		t.Fatalf("prose = %q", prose)
	}
	if len(risks) != 0 || note != "" {
		t.Fatalf("unexpected structured fields: %v %q", risks, note)
	}
	if match != finding.MatchUnknown {
		t.Fatalf("match = %v, want unknown", match)
	}
}

// A one-word verdict with trailing commentary keeps only the verdict.
func TestParseReviewOutputVerdictWithCommentary(t *testing.T) {
	raw := "===REVIEW-DATA===\nINTENT_MATCH: verified — matches the stated goal\nSUMMARY: Ship it.\n"
	_, _, match, note := rcParseReviewOutput(raw)
	if match != finding.MatchVerified {
		t.Fatalf("match = %v, want verified", match)
	}
	if note != "Ship it." {
		t.Fatalf("note = %q", note)
	}
}
