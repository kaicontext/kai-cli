package main

import (
	"strings"
	"testing"
)

// TestReachableClauseIsVerbatim guards the invariant rcChallengeSystemHeadText
// relies on: the default clause must be an exact substring of the shipped head,
// or the KAI_REVIEW_LATENT_DEFECTS swap would silently no-op.
func TestReachableClauseIsVerbatim(t *testing.T) {
	if !strings.Contains(rcChallengeSystemHead, rcReachableClauseDefault) {
		t.Fatal("rcReachableClauseDefault is no longer a verbatim substring of rcChallengeSystemHead")
	}
	if strings.Contains(rcChallengeSystemHead, rcReachableClauseLatent) {
		t.Fatal("rcReachableClauseLatent should not already be in the shipped head")
	}
	// Finder prompts carry the same contract; their default clauses must also
	// stay verbatim substrings or the swap no-ops.
	for _, c := range []struct {
		name, prompt, clause string
	}{
		{"deep trigger", rcReviewSystem, rcReviewTriggerClauseDefault},
		{"deep score", rcReviewSystem, rcReviewScoreClauseDefault},
		{"fast trigger", rcFastReviewSystem, rcFastTriggerClauseDefault},
	} {
		if !strings.Contains(c.prompt, c.clause) {
			t.Fatalf("%s clause is no longer a verbatim substring of its prompt", c.name)
		}
	}
}

// TestFinderPromptsSwapUnderFlag: the finder prompts relax only when the flag is set.
func TestFinderPromptsSwapUnderFlag(t *testing.T) {
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "")
	if rcReviewSystemText() != rcReviewSystem || rcFastReviewSystemText() != rcFastReviewSystem {
		t.Fatal("finder prompts must be unchanged when the flag is off")
	}
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "on")
	if strings.Contains(rcReviewSystemText(), rcReviewTriggerClauseDefault) {
		t.Fatal("deep finder must drop the default trigger clause when the flag is on")
	}
	if !strings.Contains(rcReviewSystemText(), rcReviewTriggerClauseLatent) {
		t.Fatal("deep finder must contain the latent trigger clause when the flag is on")
	}
	if !strings.Contains(rcFastReviewSystemText(), rcFastTriggerClauseLatent) {
		t.Fatal("fast finder must contain the latent trigger clause when the flag is on")
	}
}

// TestLatentDefectsFlagOffUnchanged: with the flag unset the gate prompt is
// byte-identical to the shipped head and the speculative drop still runs.
func TestLatentDefectsFlagOffUnchanged(t *testing.T) {
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "")
	if rcLatentDefectsEnabled() {
		t.Fatal("flag should be off when unset")
	}
	if rcChallengeSystemHeadText() != rcChallengeSystemHead {
		t.Fatal("default head must equal the shipped const")
	}
	if !strings.Contains(rcChallengeSystemHeadText(), rcReachableClauseDefault) {
		t.Fatal("default head must contain the default reachability clause")
	}
}

// TestLatentDefectsFlagOnSwaps: with the flag on the clause is swapped and the
// pre-gate speculative drop becomes a no-op (the gate adjudicates instead).
func TestLatentDefectsFlagOnSwaps(t *testing.T) {
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "1")
	if !rcLatentDefectsEnabled() {
		t.Fatal("flag should be on for \"1\"")
	}
	head := rcChallengeSystemHeadText()
	if strings.Contains(head, rcReachableClauseDefault) {
		t.Fatal("latent head must not contain the default clause")
	}
	if !strings.Contains(head, rcReachableClauseLatent) {
		t.Fatal("latent head must contain the latent clause")
	}

	// A draft whose only ISSUE carries a future-change phrase would be dropped
	// with the flag off; with it on, rcWithoutSpeculativeIssues passes it through.
	draft := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — breaks if someone later adds a caller\n"
	out, dropped := rcWithoutSpeculativeIssues(draft)
	if out != draft || dropped != nil {
		t.Fatalf("flag on: expected pass-through, got dropped=%d", len(dropped))
	}
}
