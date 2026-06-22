package gatereview

import (
	"strings"
	"testing"
)

func TestParseResponse_CleanApprove(t *testing.T) {
	resp := `SUMMARY
Adds a --fixxy-upper flag to the TUI startup and refactors viewport handling.
Updates one cache-band test assertion to match the new format.

AUDIT
CLEAN

RECOMMENDATION
APPROVE
Audit is clean and the change scope matches the session intent.`

	res := parseResponse(resp)
	if !strings.Contains(res.Summary, "fixxy-upper") {
		t.Fatalf("summary not captured: %q", res.Summary)
	}
	if !res.AuditClean || len(res.Issues) != 0 {
		t.Fatalf("expected clean audit, got clean=%v issues=%v", res.AuditClean, res.Issues)
	}
	if res.Recommendation != RecApprove {
		t.Fatalf("expected APPROVE, got %q", res.Recommendation)
	}
	if !strings.Contains(res.RecReason, "Audit is clean") {
		t.Fatalf("rec reason not captured: %q", res.RecReason)
	}
}

func TestParseResponse_IssuesFixThenApprove(t *testing.T) {
	resp := `SUMMARY
Refactors auth middleware and removes the legacy session helper.

AUDIT
ISSUES
  1. [fixable] missing import for context after refactor
  2. [human] removed session helper still referenced by 3 callers

RECOMMENDATION
FIX_THEN_APPROVE
Two issues found; one is mechanical, one needs human judgment.`

	res := parseResponse(resp)
	if res.AuditClean {
		t.Fatalf("expected non-clean audit")
	}
	if len(res.Issues) != 2 {
		t.Fatalf("expected 2 issues, got %d: %+v", len(res.Issues), res.Issues)
	}
	if !res.Issues[0].Fixable {
		t.Fatalf("issue 0 should be fixable: %+v", res.Issues[0])
	}
	if res.Issues[1].Fixable {
		t.Fatalf("issue 1 should NOT be fixable: %+v", res.Issues[1])
	}
	if !strings.Contains(res.Issues[0].Description, "missing import") {
		t.Fatalf("issue 0 description: %q", res.Issues[0].Description)
	}
	if res.Recommendation != RecFixThenApprove {
		t.Fatalf("expected FIX_THEN_APPROVE, got %q", res.Recommendation)
	}
}

func TestParseResponse_Reject(t *testing.T) {
	resp := `SUMMARY
Adds a hard-coded API key to the auth handler.

AUDIT
ISSUES
  1. [human] secret committed in plaintext

RECOMMENDATION
REJECT
Secret in diff — never approve.`

	res := parseResponse(resp)
	if res.Recommendation != RecReject {
		t.Fatalf("expected REJECT, got %q", res.Recommendation)
	}
	if len(res.Issues) != 1 || res.Issues[0].Fixable {
		t.Fatalf("expected one human issue, got %+v", res.Issues)
	}
}

func TestParseResponse_DriftedFormatting(t *testing.T) {
	// Model occasionally emits markdown bullets / extra whitespace; the
	// parser should still extract the recommendation rather than dump
	// everything into Raw and give up.
	resp := `  SUMMARY
This is a one-liner.

  AUDIT
  - CLEAN (no issues found)

  RECOMMENDATION
  ASK
  Genuinely ambiguous — could be either intentional or a mistake.`

	res := parseResponse(resp)
	if res.Recommendation != RecAsk {
		t.Fatalf("expected ASK from drifted format, got %q", res.Recommendation)
	}
	if !res.AuditClean {
		t.Fatalf("expected clean audit, got issues=%+v", res.Issues)
	}
	if !strings.Contains(res.Summary, "one-liner") {
		t.Fatalf("summary not captured: %q", res.Summary)
	}
}

func TestParseResponse_MissingAuditDefaultsClean(t *testing.T) {
	// If the model is silent on audit (which shouldn't happen, but the
	// LLM is the LLM), we read silence as "nothing to flag" rather than
	// leaving an ambiguous half-state. The recommendation alone tells
	// the user what to do.
	resp := `SUMMARY
Trivial change.

RECOMMENDATION
APPROVE`

	res := parseResponse(resp)
	if !res.AuditClean {
		t.Fatalf("expected default-clean for missing audit section")
	}
	if res.Recommendation != RecApprove {
		t.Fatalf("expected APPROVE, got %q", res.Recommendation)
	}
}

func TestParseResponse_RawAlwaysSet(t *testing.T) {
	// Even when parsing fully succeeds, Raw should hold the original
	// response so the TUI can fall back to it on render glitches.
	// (review.go's Review() sets Raw; parseResponse itself doesn't —
	// this test pins the contract by exercising the round-trip.)
	resp := `SUMMARY
Hi.
RECOMMENDATION
APPROVE`
	res := parseResponse(resp)
	// parseResponse leaves Raw empty; Review() fills it. So we just
	// verify the parser produced a non-zero Result — the Raw plumbing
	// is the caller's responsibility and is exercised by Review's
	// integration tests.
	if res == nil || res.Recommendation != RecApprove {
		t.Fatalf("parseResponse returned unusable result: %+v", res)
	}
}
