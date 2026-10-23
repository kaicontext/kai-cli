package main

import (
	"strings"
	"testing"
)

// TestReachableClauseIsVerbatim guards the invariant the swaps rely on: each
// default clause must be an exact substring of its shipped prompt, or the
// KAI_REVIEW_LATENT_DEFECTS replace would silently no-op. The fast pass is
// deliberately NOT relaxed, so only the gate and the deep finder are checked.
func TestReachableClauseIsVerbatim(t *testing.T) {
	if !strings.Contains(rcChallengeSystemHead, rcReachableClauseDefault) {
		t.Fatal("rcReachableClauseDefault is no longer a verbatim substring of rcChallengeSystemHead")
	}
	if strings.Contains(rcChallengeSystemHead, rcReachableClauseLatent) {
		t.Fatal("rcReachableClauseLatent should not already be in the shipped head")
	}
	for _, c := range []struct {
		name, prompt, clause string
	}{
		{"deep trigger", rcReviewSystem, rcReviewTriggerClauseDefault},
		{"deep score", rcReviewSystem, rcReviewScoreClauseDefault},
	} {
		if !strings.Contains(c.prompt, c.clause) {
			t.Fatalf("%s clause is no longer a verbatim substring of its prompt", c.name)
		}
	}
	// rcFutureRewritePhrases must be a strict subset of the full list, or the
	// flag-on filter would drop phrases the flag-off path never did.
	full := map[string]bool{}
	for _, p := range rcSpeculativePhrases {
		full[p] = true
	}
	for _, p := range rcFutureRewritePhrases {
		if !full[p] {
			t.Fatalf("rcFutureRewritePhrases has %q, not in rcSpeculativePhrases", p)
		}
	}
}

// TestDeepFinderSwapsUnderFlag: the deep finder prompt relaxes only when set;
// the fast finder is never touched.
func TestDeepFinderSwapsUnderFlag(t *testing.T) {
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "")
	if rcReviewSystemText() != rcReviewSystem {
		t.Fatal("deep finder must be unchanged when the flag is off")
	}
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "on")
	if strings.Contains(rcReviewSystemText(), rcReviewTriggerClauseDefault) {
		t.Fatal("deep finder must drop the default trigger clause when the flag is on")
	}
	if !strings.Contains(rcReviewSystemText(), rcReviewTriggerClauseLatent) {
		t.Fatal("deep finder must contain the latent trigger clause when the flag is on")
	}
}

// TestLatentDefectsFlagOffUnchanged: with the flag unset the gate prompt is
// byte-identical to the shipped head.
func TestLatentDefectsFlagOffUnchanged(t *testing.T) {
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "")
	if rcLatentDefectsEnabled() {
		t.Fatal("flag should be off when unset")
	}
	if rcChallengeSystemHeadText() != rcChallengeSystemHead {
		t.Fatal("default head must equal the shipped const")
	}
}

// TestGateClauseSwapsUnderFlag: with the flag on the gate's reachability clause
// is swapped for the latent variant.
func TestGateClauseSwapsUnderFlag(t *testing.T) {
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
}

// TestSpeculativeDropNarrowsUnderFlag: the pre-gate filter and the gate agree.
// Off, both the wrong-as-written dormant case and the pure footgun are dropped.
// On, the dormant case (Cal.com #14943 wording) flows to the gate while the
// pure future-rewrite footgun (Cal.com #11059 "future strict") is still dropped.
func TestSpeculativeDropNarrowsUnderFlag(t *testing.T) {
	dormant := rcSpeculativeBenchmarkIssues[0] // "...dormant today... if retry-incrementing is ever added..."
	footgun := rcSpeculativeBenchmarkIssues[1] // "...latent footgun for future strict comparisons."

	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "")
	if _, ok := rcSpeculativePhrase(dormant); !ok {
		t.Fatal("flag off: dormant case should be recognised by the full list")
	}
	if _, ok := rcSpeculativePhrase(footgun); !ok {
		t.Fatal("flag off: footgun case should be recognised by the full list")
	}

	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "1")
	if _, ok := rcSpeculativePhrase(dormant); ok {
		t.Fatal("flag on: wrong-as-written dormant case must flow to the gate, not be dropped")
	}
	if _, ok := rcSpeculativePhrase(footgun); !ok {
		t.Fatal("flag on: pure future-rewrite footgun must still be dropped pre-gate")
	}

	// End to end through rcWithoutSpeculativeIssues: flag on keeps the dormant
	// issue in the draft and still removes the footgun.
	draft := rcTestReview(dormant, footgun)
	out, dropped := rcWithoutSpeculativeIssues(draft)
	_, issues, _, _, _, _ := rcParseReviewOutput(out)
	if len(issues) != 1 || issues[0] != dormant {
		t.Fatalf("flag on: issues after filter = %q, want only the dormant case kept", issues)
	}
	if len(dropped) != 1 || dropped[0].Issue != footgun {
		t.Fatalf("flag on: dropped = %+v, want only the footgun", dropped)
	}
}

// TestRankCapRaisedUnderFlag: the publish cap grows by 2 under the flag so the
// recovered latent defects can actually be published, and is unchanged off.
// The b130168 revert of this bump was undone after measuring run42's dropped
// tail: it was only ~65% FP, so cutting it shed ~11 points of recall and pulled
// F1 down ~2. Precision is handled by the gate drop-list, not the cap.
func TestRankCapRaisedUnderFlag(t *testing.T) {
	for _, n := range []int{1, 3, 8, 20, 40} {
		t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "")
		off := rcRankCap(n)
		t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "1")
		on := rcRankCap(n)
		if on != off+2 {
			t.Fatalf("changedFiles=%d: cap off=%d on=%d, want on=off+2", n, off, on)
		}
	}
}

// TestLatentClauseRaisesTheBar: the latent gate clause carries the explicit
// drop-list that keeps the flag's recall from flooding false positives, plus
// the severity guardrail that keeps a high/critical latent defect supportable
// on an unverified consequence so the drop-list does not also cost recall.
func TestLatentClauseRaisesTheBar(t *testing.T) {
	for _, want := range []string{
		"higher bar",
		"drop-list",
		"high or critical",
		// guardrail: an unverified consequence refutes only a low/medium latent
		// defect; a high/critical one may still be supported.
		"even when you could not fully verify",
		"unverified consequence is a refutation",
	} {
		if !strings.Contains(rcReachableClauseLatent, want) {
			t.Fatalf("latent gate clause missing precision condition %q", want)
		}
	}
}
