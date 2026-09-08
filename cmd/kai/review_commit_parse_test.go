package main

import (
	"slices"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

func TestRCParseReviewOutputIntentFormatting(t *testing.T) {
	tests := []struct {
		name string
		line string
		want finding.Match
	}{
		{"canonical", "INTENT_MATCH: verified", finding.MatchVerified},
		{"lowercase", "intent_match: partial", finding.MatchPartial},
		{"bold label", "**INTENT_MATCH:** verified", finding.MatchVerified},
		{"backticked line", "`INTENT_MATCH: diverges`", finding.MatchDiverges},
		{"heading label", "### INTENT_MATCH: partial", finding.MatchPartial},
		{"period", "INTENT_MATCH: verified.", finding.MatchVerified},
		{"explanation", "INTENT_MATCH: partial — one gap remains", finding.MatchPartial},
		{"prompt synonym", "INTENT_MATCH: matches", finding.MatchVerified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := "review prose\n===REVIEW-DATA===\n" + tt.line + "\nMERGE_READY: 4\nSUMMARY: useful\nISSUES:\n"
			prose, risks, decisions, got, readiness, note := rcParseReviewOutput(raw)
			if got != tt.want {
				t.Fatalf("match = %q, want %q", got, tt.want)
			}
			if prose != "review prose" || len(risks) != 0 || len(decisions) != 0 || readiness != finding.ReadinessDecideThenMerge || note != "useful" {
				t.Fatalf("other fields regressed: prose=%q risks=%v decisions=%v readiness=%v note=%q", prose, risks, decisions, readiness, note)
			}
		})
	}
}

func TestRCParseReviewOutputDoesNotInventIntent(t *testing.T) {
	for _, line := range []string{
		"INTENT_MATCH: not verified",
		"INTENT_MATCH: cannot be verified",
		"INTENT_MATCH: probably",
		"INTENT MATCH: verified",
		"SUMMARY: the implementation matches the intent",
	} {
		t.Run(line, func(t *testing.T) {
			_, _, _, got, _, _ := rcParseReviewOutput("===REVIEW-DATA===\n" + line)
			if got != finding.MatchUnknown {
				t.Fatalf("match = %q, want unknown", got)
			}
		})
	}
}

func TestRCParseReviewOutputConflictingIntentStaysUnknown(t *testing.T) {
	raw := "===REVIEW-DATA===\nINTENT_MATCH: verified\nINTENT_MATCH: diverges\nSUMMARY: contradictory"
	_, _, _, got, _, _ := rcParseReviewOutput(raw)
	if got != finding.MatchUnknown {
		t.Fatalf("match = %q, want unknown", got)
	}
}

func TestRCParseReviewOutputDecoratedCodaPreservesOtherFields(t *testing.T) {
	raw := "Human review.\n\n===REVIEW-DATA===\n**INTENT_MATCH:** verified\n**MERGE_READY:** 3\n**SUMMARY:** keep `a*b` and trailing `code`\n**ISSUES:**\n**- a.go:4 — broken `thing`**\n**DECISIONS:**\n**- enable the policy for all users**"
	prose, risks, decisions, match, readiness, note := rcParseReviewOutput(raw)
	if prose != "Human review." {
		t.Errorf("prose = %q", prose)
	}
	if match != finding.MatchVerified {
		t.Errorf("match = %q", match)
	}
	if readiness != finding.ReadinessSmallFixes {
		t.Errorf("readiness = %v", readiness)
	}
	if note != "keep `a*b` and trailing `code`" {
		t.Errorf("note = %q", note)
	}
	if !slices.Equal(risks, []string{"a.go:4 — broken `thing`"}) {
		t.Errorf("risks = %q", risks)
	}
	if !slices.Equal(decisions, []string{"enable the policy for all users"}) {
		t.Errorf("decisions = %q", decisions)
	}
}

func TestRCParseReviewOutputLegacyBlockStillWorks(t *testing.T) {
	raw := "Context first.\nFINDINGS:\n- old.go:2 — bug\nINTENT_MATCH: matches\nNOTE: legacy summary"
	prose, risks, _, match, _, note := rcParseReviewOutput(raw)
	if prose != "Context first." {
		t.Errorf("prose = %q", prose)
	}
	if !slices.Equal(risks, []string{"old.go:2 — bug"}) {
		t.Errorf("risks = %q", risks)
	}
	if match != finding.MatchVerified {
		t.Errorf("match = %q", match)
	}
	if note != "legacy summary" {
		t.Errorf("note = %q", note)
	}
}

func TestRCParseReviewOutputRepeatedFindingsSupersedesEarlierBlock(t *testing.T) {
	raw := "The fixture contains:\nFINDINGS:\n- quoted.go:1 — example\nINTENT_MATCH: diverges\nMERGE_READY: 1\nNOTE: quoted note\nDECISIONS:\n- quoted policy decision\n\nActual review:\nFINDINGS:\n- real.go:2 — actual issue\nINTENT_MATCH: verified\nNOTE: actual note"
	_, risks, decisions, match, readiness, note := rcParseReviewOutput(raw)
	if !slices.Equal(risks, []string{"real.go:2 — actual issue"}) {
		t.Errorf("risks = %q", risks)
	}
	if len(decisions) != 0 {
		t.Errorf("decisions = %q, want none from the superseded block", decisions)
	}
	if match != finding.MatchVerified {
		t.Errorf("match = %q, want verified", match)
	}
	if readiness != finding.ReadinessUnknown {
		t.Errorf("readiness = %v, want unknown after superseding the quoted score", readiness)
	}
	if note != "actual note" {
		t.Errorf("note = %q", note)
	}
}

func TestRCParseReviewOutputFirstFindingsPreservesEarlierVerdict(t *testing.T) {
	raw := "INTENT_MATCH: verified\nNOTE: stated first\nFINDINGS:\n- real.go:2 — issue"
	_, _, _, match, _, note := rcParseReviewOutput(raw)
	if match != finding.MatchVerified {
		t.Errorf("match = %q, want verified", match)
	}
	if note != "stated first" {
		t.Errorf("note = %q", note)
	}
}
