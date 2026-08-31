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
	prose, risks, decisions, match, note := rcParseReviewOutput(raw)
	if !strings.HasPrefix(prose, "This change adds a token-bucket limiter") {
		t.Fatalf("prose lost: %q", prose)
	}
	if strings.Contains(prose, "REVIEW-DATA") || strings.Contains(prose, "INTENT_MATCH") {
		t.Fatalf("coda leaked into prose: %q", prose)
	}
	if len(risks) != 2 {
		t.Fatalf("risks = %d, want 2: %v", len(risks), risks)
	}
	if len(decisions) != 0 {
		t.Fatalf("decisions = %v, want none", decisions)
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
	prose, risks, decisions, match, note := rcParseReviewOutput(raw)
	if prose != "" {
		t.Fatalf("legacy block should have no prose, got %q", prose)
	}
	if len(risks) != 1 || !strings.Contains(risks[0], "pkg/a.go:10") {
		t.Fatalf("risks = %v", risks)
	}
	if len(decisions) != 0 {
		t.Fatalf("decisions = %v, want none", decisions)
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
	prose, risks, decisions, match, note := rcParseReviewOutput(raw)
	if prose != raw {
		t.Fatalf("prose = %q", prose)
	}
	if len(risks) != 0 || len(decisions) != 0 || note != "" {
		t.Fatalf("unexpected structured fields: %v %v %q", risks, decisions, note)
	}
	if match != finding.MatchUnknown {
		t.Fatalf("match = %v, want unknown", match)
	}
}

// A one-word verdict with trailing commentary keeps only the verdict.
func TestParseReviewOutputVerdictWithCommentary(t *testing.T) {
	raw := "===REVIEW-DATA===\nINTENT_MATCH: verified — matches the stated goal\nSUMMARY: Ship it.\n"
	_, _, _, match, note := rcParseReviewOutput(raw)
	if match != finding.MatchVerified {
		t.Fatalf("match = %v, want verified", match)
	}
	if note != "Ship it." {
		t.Fatalf("note = %q", note)
	}
}

// The kaicontext/kai-server#126 shape (2026-08-31): the change is correct, the
// intent matches, there are no defects — and it still moves customer money, so
// the reviewer files a DECISION. Decisions must parse into their own list: they
// carry no path:line, and they must NOT be mistaken for defects or suppress the
// verified verdict. The caller folds them into the risk-tagged claims so the PR
// badge stops saying "Ready to merge — nothing flagged".
func TestParseReviewOutputDecisions(t *testing.T) {
	raw := `Scope: kai-server at HEAD; I could not read kai-engine, which also sets EstimatedCostUSD.

The surcharge itself is applied correctly on both cost surfaces.

===REVIEW-DATA===
INTENT_MATCH: verified
SUMMARY: Correctly implemented, but it is a pricing change, not only an accounting one.
DECISIONS:
- The 5% reaches DrawdownUserCredits, so every OpenRouter request now debits 5% more from a customer's prepaid balance and shrinks daily/monthly cap headroom by ~4.8%.
`
	prose, risks, decisions, match, note := rcParseReviewOutput(raw)
	if !strings.HasPrefix(prose, "Scope: kai-server at HEAD") {
		t.Fatalf("prose lost: %q", prose)
	}
	if len(risks) != 0 {
		t.Fatalf("risks = %v, want none (a decision is not a defect)", risks)
	}
	if len(decisions) != 1 || !strings.Contains(decisions[0], "prepaid balance") {
		t.Fatalf("decisions = %v", decisions)
	}
	if match != finding.MatchVerified {
		t.Fatalf("match = %v, want verified — a decision must not lower the verdict", match)
	}
	if !strings.HasPrefix(note, "Correctly implemented") {
		t.Fatalf("note = %q", note)
	}
}

// Both lists in one coda, in either order, must stay separated.
func TestParseReviewOutputIssuesAndDecisions(t *testing.T) {
	raw := `===REVIEW-DATA===
INTENT_MATCH: partial
SUMMARY: One real bug, one call for you.
DECISIONS:
- Raising the free-tier cap changes what every unpaid org can spend.
ISSUES:
- internal/usage/pricing.go:147 — branches on ProviderCostUSD > 0 but the comment claims it means the OpenRouter path
`
	_, risks, decisions, match, _ := rcParseReviewOutput(raw)
	if len(risks) != 1 || !strings.Contains(risks[0], "pricing.go:147") {
		t.Fatalf("risks = %v", risks)
	}
	if len(decisions) != 1 || !strings.Contains(decisions[0], "free-tier cap") {
		t.Fatalf("decisions = %v", decisions)
	}
	if match != finding.MatchPartial {
		t.Fatalf("match = %v", match)
	}
}
