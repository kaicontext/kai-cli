package main

import (
	"strings"
	"testing"
)

// The reviewer prompt is the only place these rules live, and each block
// in it was bought with a real incident. A rule that gets dropped in an
// edit fails silently — the reviewer simply stops looking, and nothing
// says so. Pin the ones added after the 2026-09-03 review, by the
// behaviour they demand rather than by exact wording, so rephrasing is
// free and deletion is not.
func TestReviewSystemPrompt_KeepsTheHardWonRules(t *testing.T) {
	rules := []struct {
		name  string
		needs []string
	}{
		{
			// A fix whose test also passes on the old code proves nothing.
			name:  "a test must fail without the fix",
			needs: []string{"FAILS with the fix removed", "unverified"},
		},
		{
			// Asserting a mechanism was configured is not asserting the
			// behaviour it was meant to buy.
			name:  "mechanism configured is not behaviour verified",
			needs: []string{"MECHANISM was configured", "BEHAVIOUR"},
		},
		{
			// A test that skips on the machine that runs it runs nowhere.
			name:  "a skipped test is not a passing test",
			needs: []string{"SKIPS for an environmental reason"},
		},
		{
			name:  "environment assumptions get named",
			needs: []string{"THE ENVIRONMENT IS NOT CLEAN", "failure mode is the finding"},
		},
		{
			name:  "config read from the wrong place",
			needs: []string{"CONFIG READ FROM THE WRONG PLACE", "gitconfig"},
		},
		{
			name:  "a deadline alone does not bound an exec",
			needs: []string{"SUBPROCESSES THAT NEVER RETURN", "WaitDelay"},
		},
		{
			name:  "hot-path cost",
			needs: []string{"COST ON A HOT PATH"},
		},
		{
			name:  "an optimisation must not be fatal",
			needs: []string{"A NICETY THAT CAN BE FATAL"},
		},
		{
			name:  "unrequested writes get named",
			needs: []string{"WRITES NOBODY ASKED FOR", "opt-out"},
		},
	}

	for _, r := range rules {
		for _, need := range r.needs {
			if !strings.Contains(rcReviewSystem, need) {
				t.Errorf("the reviewer stopped enforcing %q: %q is gone from the prompt", r.name, need)
			}
		}
	}
}

// Each rule cites the incident that produced it, which is what stops a
// later reader deleting it as boilerplate. Keep the citations.
func TestReviewSystemPrompt_RulesCiteTheirIncident(t *testing.T) {
	for _, cite := range []string{"kai-engine, 2026-09-03", "kai-cli, 2026-09-03", "v0.6.46", "v0.6.47"} {
		if !strings.Contains(rcReviewSystem, cite) {
			t.Errorf("a rule lost the incident that bought it: %q", cite)
		}
	}
}
