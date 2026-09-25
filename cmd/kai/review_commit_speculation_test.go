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

// The draft was written about its speculative issues: prose, SUMMARY and
// score. Once they are gone none of that may be published.
func TestAllSpeculativeDraftPublishesNoTraceOfTheRefutedConcerns(t *testing.T) {
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		t.Fatal("the gate was called")
		return provider.Response{}, nil
	}}
	draft := "Two defects: the SMS-only deletion branch and the credential flag.\n" + rcReviewDataMarker +
		"\nINTENT_MATCH: partial\nMERGE_READY: 3\nSUMMARY: 2 defects need small fixes before merge.\nISSUES:\n" +
		rcTestBullets(rcSpeculativeBenchmarkIssues[:2])
	res, err := rcChallengeReview(context.Background(), p, "test", draft, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"Two defects", "2 defects need small fixes", "SMS-only deletion branch", "dormant today"} {
		if strings.Contains(res.Review, stale) {
			t.Errorf("published review still carries %q:\n%s", stale, res.Review)
		}
	}
	_, issues, _, _, r, note := rcParseReviewOutput(res.Review)
	if len(issues) != 0 || int(r) != 4 || !strings.Contains(note, "No defect survived review") {
		t.Errorf("issues=%q readiness=%d summary=%q, want none, 4, and a summary saying nothing survived", issues, int(r), note)
	}
	if len(res.Allegations) != 2 || res.Allegations[0].Status != rcStatusRefuted {
		t.Errorf("allegations = %+v, want both recorded as refuted", res.Allegations)
	}
}

// A draft the reviewer scored 5 over speculative concerns is not published
// as 5 either: nothing re-examined the change once they were gone.
func TestAllSpeculativeDraftScoredFiveIsPublishedAsFour(t *testing.T) {
	draft := strings.Replace(rcTestReview(rcSpeculativeBenchmarkIssues[0]), "MERGE_READY: 3", "MERGE_READY: 5", 1)
	out, spec := rcWithoutSpeculativeIssues(draft)
	review, ok := rcReviewWithoutSpeculation(out, spec)
	if !ok {
		t.Fatal("not assembled")
	}
	if _, _, _, _, r, _ := rcParseReviewOutput(review); int(r) != 4 {
		t.Errorf("readiness = %d, want 4", int(r))
	}
}

// A draft that keeps a real issue goes through the gate with the reviewer's
// own prose and score.
func TestDraftWithARealIssueStillGoesToTheGate(t *testing.T) {
	out, spec := rcWithoutSpeculativeIssues(rcTestReview(rcSpeculativeBenchmarkIssues[0], rcRealBenchmarkIssues[0]))
	if _, ok := rcReviewWithoutSpeculation(out, spec); ok {
		t.Error("a draft with a real issue left was assembled without the gate")
	}
}
