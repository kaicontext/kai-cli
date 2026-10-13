package main

import (
	"strings"
	"testing"
)

// Real decisions: the first three were raised on kai-cli#171 and
// kai-server#444 about how the reviewer itself behaves — no stake for anyone
// outside. The rest reach people.
func TestDecisionsMustNameAStake(t *testing.T) {
	none := []string{
		"Recovery promotes located prose bullets to ISSUES when the coda has no ISSUES header, so some non-defect discussion bullets reach the fact-check, for every review published to PR authors.",
		"The fact-check prompt now supports a finding when the code defect is established even if a further consequence is unverified, so more findings reach PR authors.",
		"A point someone has answered is no longer posted as a new inline comment, even reworded or on a different line; it is moved to a folded section.",
	}
	stake := []string{
		"[money] Every run is now debited 5% more from the customer's prepaid balance.",
		"[contract] The /v1/findings response drops the `verdict` field.",
		"Retries now cost the customer one extra call, which is billed.",
		"Admins can now read every member's API keys.",
		"Deleted repositories are purged after 48 hours instead of 30 days.",
		"Users who uninstall get a goodbye email.",
		"[Access] Members can now see the admin page.",
		"**[contract]** The export format adds a column.",
		"`[Limit]` Free orgs get 20 reviews a day instead of 50.",
	}
	for _, d := range none {
		if rcHasStake(d) {
			t.Errorf("no stake, but kept: %q", d)
		}
	}
	for _, d := range stake {
		if !rcHasStake(d) {
			t.Errorf("names a stake, but dropped: %q", d)
		}
	}
}

func TestStakelessDecisionsLeaveTheDraftBeforeTheCheck(t *testing.T) {
	raw := "Prose.\n" + rcReviewDataMarker + "\nMERGE_READY: 4\nISSUES:\n- a.go:3 — `x` is nil\nDECISIONS:\n" +
		"- The reviewer now folds nits into one section.\n" +
		"- [money] Every run is debited 5% more from the customer's balance.\n"
	got, n := rcDropStakelessDecisions(raw)
	_, issues, decisions, _, _, _ := rcParseReviewOutput(got)
	if n != 1 || len(decisions) != 1 || !strings.HasPrefix(decisions[0], "[money]") || len(issues) != 1 {
		t.Errorf("dropped %d; issues %q decisions %q", n, issues, decisions)
	}
	for _, prompt := range []string{rcReviewSystem, rcFastReviewSystem} {
		if !strings.Contains(prompt, "A DECISION MUST NAME ITS STAKE") || !strings.Contains(prompt, "[money|limit|sends|deletes|access|contract]") {
			t.Error("a reviewer prompt lacks the stake rule or the tagged DECISIONS format")
		}
	}
	if !strings.Contains(rcChallengeSystemTail, "REFUTE a decision whose consequence does not reach real people") {
		t.Error("the fact-check prompt lacks the stake rule")
	}
}
