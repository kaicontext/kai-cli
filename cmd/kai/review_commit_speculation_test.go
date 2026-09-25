package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/provider"
)

// Kai's own published findings from the 2026-09-24 benchmark whose trigger is
// a change nobody made.
var rcSpeculativeBenchmarkIssues = []string{
	"packages/features/ee/workflows/api/scheduleSMSReminders.ts:38 — New `retryCount > 1` deletion branch omits the `method: SMS` guard the original branch had; dormant today (only SMS writes retryCount in this repo) but will silently delete email/whatsapp reminders if retry-incrementing is ever added to those paths.",
	"packages/lib/constants.ts:103 — `APP_CREDENTIAL_SHARING_ENABLED` holds the encryption-key string, not a boolean; latent footgun for future strict comparisons.",
	"src/auth/guard.ts:40 — the null check only works because it runs first; a future guard reorder would dereference session before it is checked.",
	// The baseline corpus run of 2026-09-25 (kai-cli v0.35.89) on Cal.com #10600.
	"apps/web/pages/api/auth/two-factor/totp/disable.ts:58-66 — disable path relies on terminal-wipe for single-use; a future non-terminal step would break the \"exactly once\" invariant.",
}

// Golden defects from the same benchmark: real triggers, stated plainly. None
// of them may be mistaken for speculation.
var rcRealBenchmarkIssues = []string{
	"packages/features/ee/workflows/api/scheduleSMSReminders.ts:30 — retryCount: reminder.retryCount + 1 reads a possibly stale value and can lose increments under concurrency; use an atomic increment.",
	"packages/features/auth/lib/next-auth-options.ts:144 — two concurrent logins with the same backup code can both pass the check before either writes back, so a one-time code is accepted more than once.",
	"packages/app-store/_utils/oauth/parseRefreshTokenResponse.ts:25 — a missing refresh_token is replaced with the literal string 'refresh_token', which is then persisted and breaks every later refresh.",
	"services/src/main/java/org/keycloak/services/resources/admin/permissions/ClientPermissionsV2.java:120 — findByName(server, client.getId(), ...) never finds the per-client resource, which getOrCreateResource creates with the owner set to resourceServer.getClientId().",
	"services/src/main/java/org/keycloak/services/resources/admin/permissions/GroupPermissionsV2.java:70 — canManage() falls back to VIEW and MANAGE, so a view-only caller passes it and can create top-level groups.",
	"apps/web/pages/api/webhook/app-credential.ts:24 — the webhook secret is compared with !==, a timing side channel on an authorization boundary.",
	"internal/cache/cache.go:88 — if the entry is ever evicted between Get and Set, the second writer overwrites the first; two requests hit this on every cold start.",
	"apps/web/lib/booking.ts:40 — a booking date in the future is compared as a string, so 2026-10-01 sorts before 2026-9-30 and the slot is rejected.",
	"packages/features/bookings/lib/handleNewBooking.ts:210 — if a future request arrives before the first commits, both pass the availability check and the slot is double-booked.",
}

func TestSpeculativePhraseCatchesTheBenchmarkCases(t *testing.T) {
	for _, it := range rcSpeculativeBenchmarkIssues {
		if _, ok := rcSpeculativePhrase(it); !ok {
			t.Errorf("not recognised as speculative: %s", it)
		}
	}
}

func TestSpeculativePhraseLeavesRealDefectsAlone(t *testing.T) {
	for _, it := range rcRealBenchmarkIssues {
		if p, ok := rcSpeculativePhrase(it); ok {
			t.Errorf("real defect dropped as speculative (%q): %s", p, it)
		}
	}
}

func TestSpeculativeIssuesLeaveTheDraftBeforeTheGate(t *testing.T) {
	draft := rcTestReview(rcSpeculativeBenchmarkIssues[0], rcRealBenchmarkIssues[0]) +
		"DECISIONS:\n- a future change to billing is the author's call\n"
	out, dropped := rcWithoutSpeculativeIssues(draft)

	_, issues, decisions, _, _, _ := rcParseReviewOutput(out)
	if len(issues) != 1 || issues[0] != rcRealBenchmarkIssues[0] {
		t.Fatalf("issues after the filter = %q, want only the real defect", issues)
	}
	// DECISIONS are not issues: "a future change" there is not a defect claim.
	if len(decisions) != 1 {
		t.Errorf("decisions = %q, want the draft's one decision untouched", decisions)
	}
	if !strings.HasPrefix(out, "Review within the supplied scope.") {
		t.Errorf("prose before the coda changed:\n%s", out)
	}
	if len(dropped) != 1 || dropped[0].Status != rcStatusRefuted || !strings.Contains(dropped[0].Reason, "hypothetical trigger") ||
		dropped[0].Issue != rcSpeculativeBenchmarkIssues[0] {
		t.Fatalf("dropped = %+v, want the speculative issue recorded as refuted", dropped)
	}

	if same, none := rcWithoutSpeculativeIssues(rcTestReview(rcRealBenchmarkIssues...)); none != nil || same != rcTestReview(rcRealBenchmarkIssues...) {
		t.Error("a draft with no speculative issue must pass through unchanged")
	}
}

// Speculation never reaches the challenger, and never the published review.
// A draft whose only issues are speculative has nothing left to challenge.
func TestSpeculativeIssuesAreNeverPublished(t *testing.T) {
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		t.Fatal("the gate was asked to check a speculative allegation")
		return provider.Response{}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcSpeculativeBenchmarkIssues...), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, issues, _, _, _, _ := rcParseReviewOutput(res.Review); len(issues) != 0 {
		t.Errorf("published issues = %q, want none", issues)
	}
	if len(res.Allegations) != len(rcSpeculativeBenchmarkIssues) || res.Incomplete {
		t.Fatalf("result = %+v, want every speculative issue recorded as refuted and the review complete", res)
	}
	for i, a := range res.Allegations {
		if a.Status != rcStatusRefuted || a.ID != i+1 {
			t.Errorf("allegation %d = %+v, want refuted with id %d", i, a, i+1)
		}
	}
}

// The gate and the fast pass carry the same bar as the deep review.
func TestGateAndFastPassRefuteFutureTriggers(t *testing.T) {
	for _, want := range []string{"REFUTE one whose failure needs a future change", "trigger is hypothetical", "supported only when a source establishes"} {
		if !strings.Contains(rcChallengeSystemHead, want) {
			t.Errorf("challenge prompt is missing %q", want)
		}
	}
	if !strings.Contains(rcFastReviewSystem, "The trigger must exist today") {
		t.Error("fast-pass prompt is missing the trigger-today rule")
	}
}
