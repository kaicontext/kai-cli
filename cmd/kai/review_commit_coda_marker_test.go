package main

import (
	"os"
	"strings"
	"testing"
)

// A real finished answer (regression case calcom-14943, trimmed to its last
// sections): a whole review and a whole coda, with a horizontal rule where
// the marker line should be. It used to be discarded as "ended without a
// conclusion".
func TestRestoreCodaMarkerRescuesARealAnswer(t *testing.T) {
	b, err := os.ReadFile("testdata/review-coda/no-marker-calcom-14943.txt")
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(string(b))
	if rcUsableCoda(raw) {
		t.Fatal("fixture should reproduce the bug: no marker, so not usable as-is")
	}
	got := rcRestoreCodaMarker(raw)
	if !rcUsableCoda(got) || rcNeedsConclusion(got) {
		t.Fatal("a complete coda without its marker line must count as a conclusion")
	}
	prose, risks, _, match, readiness, _ := rcParseReviewOutput(got)
	if strings.Contains(prose, "INTENT_MATCH:") || strings.HasSuffix(prose, "---") {
		t.Errorf("prose should end before the coda and drop the rule that stood in for the marker:\n%s", prose[len(prose)-200:])
	}
	if len(risks) != 1 || !strings.Contains(risks[0], "scheduleSMSReminders.ts:184") {
		t.Errorf("risks = %q, want the one retryCount issue", risks)
	}
	if match == "" || readiness == 0 {
		t.Errorf("match=%q readiness=%d, want both parsed", match, readiness)
	}
}

func TestRestoreCodaMarkerLeavesOtherAnswersAlone(t *testing.T) {
	withMarker := "Prose.\n\n" + rcReviewDataMarker + "\nINTENT_MATCH: verified\nSUMMARY: ok\n"
	if got := rcRestoreCodaMarker(withMarker); got != withMarker {
		t.Error("an answer that already has its marker must pass through unchanged")
	}
	for _, raw := range []string{
		"Looks fine, nothing to add.",
		"The field INTENT_MATCH: is what the parser reads.",
		"Stopped mid-write.\n\nINTENT_MATCH: partial",
	} {
		if got := rcRestoreCodaMarker(raw); got != raw {
			t.Errorf("no coda to restore, but %q became %q", raw, got)
		}
	}
	// A quoted example block earlier on must not become the coda.
	raw := "Example:\nINTENT_MATCH: verified\nSUMMARY: x\n\nReal prose.\n\nINTENT_MATCH: partial\nMERGE_READY: 3\nSUMMARY: real\n"
	got := rcRestoreCodaMarker(raw)
	if i := strings.Index(got, rcReviewDataMarker); i < 0 || !strings.HasPrefix(got[i+len(rcReviewDataMarker):], "\nINTENT_MATCH: partial") {
		t.Errorf("marker should sit above the LAST coda block:\n%s", got)
	}
}

// The coverage gate's rewrite was thrown away for the same reason: its answer
// had no marker, so the first review stood and then went to the fallback.
func TestMergeGateAdoptsASecondAnswerWithoutItsMarker(t *testing.T) {
	first := "First.\n\nINTENT_MATCH: verified\nMERGE_READY: 5\nSUMMARY: fine\n"
	second := "With the skipped files.\n\n---\n\nINTENT_MATCH: partial\nMERGE_READY: 3\nSUMMARY: one gap\nISSUES:\n- a.go:1 — x\n"
	raw, _, adopted := rcMergeGate(first, second, "end_turn")
	if !adopted || !strings.Contains(raw, "one gap") || !rcUsableCoda(raw) {
		t.Errorf("the gate's whole review should be adopted, got adopted=%v:\n%s", adopted, raw)
	}
}
