package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// rcRequestText is every text part of a request's messages, joined.
func rcRequestText(req provider.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if t, ok := p.(message.TextContent); ok {
				b.WriteString(t.Text)
			}
		}
	}
	return b.String()
}

// Kai's three findings for one cause on Cal.com #11059 (2026-09-24), in the
// order it published them, between two unrelated ones.
var rc11059Issues = []string{
	"packages/app-store/googlecalendar/lib/CalendarService.ts:97 — `parseRefreshTokenResponse` returns a `{success,data}` wrapper; Google stores it directly as `key`, corrupting the credential on every refresh.",
	"packages/app-store/hubspot/lib/CalendarService.ts:177 — `refreshOAuthTokens` returns a `fetch Response` in sync mode; HubSpot treats it as a `HubspotToken` and stores the `Response` object as the credential.",
	"packages/app-store/zoho-bigin/lib/CalendarService.ts:96 — same Axios/Response mismatch; sync mode stores `Response.body`-derived garbage with `NaN` expiry.",
	"packages/app-store/zohocrm/lib/CalendarService.ts:217 — same Axios/Response mismatch; sync mode persists `Response` data as a `ZohoToken`.",
	"packages/app-store/_utils/oauth/parseRefreshTokenResponse.ts:25 — missing `refresh_token` is replaced with the literal `\"refresh_token\"`, which Zoom then persists, permanently breaking refresh.",
}

func TestRepeatedCauseBecomesOneIssueWithEveryLocation(t *testing.T) {
	out, notes := rcMergeDuplicateIssues(rcTestReview(rc11059Issues...))
	_, issues, _, _, _, _ := rcParseReviewOutput(out)
	if len(issues) != 3 {
		t.Fatalf("issues = %d %q, want 3 (Google wrapper, HubSpot+Zoho x2, Zoom literal)", len(issues), issues)
	}
	hub := issues[1]
	if !strings.HasPrefix(hub, rc11059Issues[1][:len("packages/app-store/hubspot/lib/CalendarService.ts:177")]) {
		t.Errorf("the merged issue must keep HubSpot's location first: %s", hub)
	}
	for _, loc := range []string{"packages/app-store/zoho-bigin/lib/CalendarService.ts:96", "packages/app-store/zohocrm/lib/CalendarService.ts:217"} {
		if !strings.Contains(hub, loc) {
			t.Errorf("merged issue is missing %s: %s", loc, hub)
		}
	}
	if !strings.Contains(hub, "(also: ") {
		t.Errorf("merged issue does not list its other places: %s", hub)
	}
	if issues[0] != rc11059Issues[0] || issues[2] != rc11059Issues[4] {
		t.Errorf("unrelated issues changed: %q", issues)
	}
	if len(notes) != 2 {
		t.Errorf("notes = %q, want one per fold", notes)
	}
	// The merged bullet still grounds at its first location.
	if p, l, ok := rcIssueLocation(hub); !ok || p != "packages/app-store/hubspot/lib/CalendarService.ts" || l != 177 {
		t.Errorf("merged issue grounds at %s:%d (%v)", p, l, ok)
	}
}

func TestIdenticalSentencesAtDifferentPlacesAreOneIssue(t *testing.T) {
	a := "internal/api/a.go:10 — the webhook secret is compared with `!=`, a timing side channel."
	b := "internal/api/b.go:22 — the webhook secret is compared with !=, a timing side channel."
	out, _ := rcMergeDuplicateIssues(rcTestReview(a, b))
	_, issues, _, _, _, _ := rcParseReviewOutput(out)
	if len(issues) != 1 || !strings.Contains(issues[0], "(also: internal/api/b.go:22)") {
		t.Fatalf("issues = %q, want one with b.go in its also-list", issues)
	}
}

// Different defects that share words stay separate: merging is only for a
// bullet that says it repeats, or one that repeats word for word.
func TestRepeatOpenersCoverPunctuationAndSynonyms(t *testing.T) {
	for _, s := range []string{"Same. Zoho CRM stores it too.", "Similarly, Zoho CRM stores the Response.", "Same — Zoho CRM stores it too.", "As in HubSpot, the Response is stored."} {
		if !rcIsRepeat(s) {
			t.Errorf("not recognised as a repeat: %q", s)
		}
	}
	for _, s := range []string{"Samesite is not set on the cookie.", "Sameness of ids is never checked before the merge."} {
		if rcIsRepeat(s) {
			t.Errorf("a sentence that merely starts with 'same' was taken for a repeat: %q", s)
		}
	}
}

// A terse sentence repeated at two places is a pattern, not proof of one
// cause, and is left for the reviewer.
// A repeat folds into the cause it names, not into an unrelated bullet that
// happens to sit between them; one that names nothing earlier is left alone.
func TestRepeatFoldsIntoTheCauseItNames(t *testing.T) {
	unrelated := "apps/web/pages/api/webhook/app-credential.ts:24 — the webhook secret is compared with !==, a timing side channel."
	in := rcTestReview(rc11059Issues[1], unrelated, rc11059Issues[2])
	out, _ := rcMergeDuplicateIssues(in)
	_, issues, _, _, _, _ := rcParseReviewOutput(out)
	if len(issues) != 2 || !strings.Contains(issues[0], "zoho-bigin/lib/CalendarService.ts:96") || strings.Contains(issues[1], "(also:") {
		t.Fatalf("issues = %q, want the Zoho repeat on HubSpot's bullet, not on the webhook one", issues)
	}
	orphan := rcTestReview(unrelated, "internal/a.go:3 — same problem here.")
	if got, _ := rcMergeDuplicateIssues(orphan); got != orphan {
		t.Error("a repeat that names nothing earlier was folded")
	}
}

func TestAlsoListKeepsTheBulletsFullStop(t *testing.T) {
	if got := rcWithAlso("a.go:1 — the token is stale.", []string{"b.go:2"}); got != "a.go:1 — the token is stale (also: b.go:2)." {
		t.Errorf("got %q", got)
	}
	if got := rcWithAlso("a.go:1 — the token is stale", []string{"b.go:2"}); got != "a.go:1 — the token is stale (also: b.go:2)." {
		t.Errorf("got %q", got)
	}
}

func TestTerseRepeatsAreNotFolded(t *testing.T) {
	in := rcTestReview("internal/a.go:10 — missing error check.", "internal/b.go:20 — missing error check.")
	if out, notes := rcMergeDuplicateIssues(in); out != in || notes != nil {
		t.Fatalf("terse repeats were folded: %q", notes)
	}
}

func TestDistinctDefectsAreNotMerged(t *testing.T) {
	distinct := []string{
		"services/src/main/java/org/keycloak/services/resources/admin/permissions/GroupPermissionsV2.java:70 — canManage() checks VIEW and MANAGE, so view-only callers pass it.",
		"services/src/main/java/org/keycloak/services/resources/admin/permissions/GroupPermissionsV2.java:147 — getResourceTypeResource can return null and findByResource then throws a NullPointerException.",
		"services/src/main/java/org/keycloak/services/resources/admin/permissions/AdminPermissions.java:74 — the V1 guard around listener registration disables group-removal cleanup in V2-only deployments.",
		"src/lib/sameSite.ts:12 — samesite=none is set without secure, so browsers reject the cookie.",
	}
	in := rcTestReview(distinct...)
	out, notes := rcMergeDuplicateIssues(in)
	if out != in || notes != nil {
		t.Fatalf("distinct defects were merged: %q", notes)
	}
}

// The gate is asked to check one allegation per cause.
func TestTheGateSeesOneAllegationPerCause(t *testing.T) {
	var asked string
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		asked = rcRequestText(req)
		return provider.Response{}, context.Canceled
	}}
	_, _ = rcChallengeReview(context.Background(), p, "test", rcTestReview(rc11059Issues...), nil, nil)
	if n := strings.Count(asked, "same Axios/Response mismatch"); n != 0 {
		t.Errorf("the gate was sent %d repeat bullet(s) as allegations of their own", n)
	}
	if !strings.Contains(asked, "(also: packages/app-store/zoho-bigin/lib/CalendarService.ts:96, packages/app-store/zohocrm/lib/CalendarService.ts:217)") {
		t.Error("the gate did not receive the merged allegation")
	}
}

func TestPromptsAskForOneIssuePerCause(t *testing.T) {
	for name, prompt := range map[string]string{"review": rcReviewSystem, "fast": rcFastReviewSystem} {
		if !strings.Contains(prompt, "(also: ") {
			t.Errorf("%s prompt does not show the also-list", name)
		}
	}
	if !strings.Contains(rcReviewSystem, "ONE ROOT CAUSE, ONE ISSUE") || !strings.Contains(rcReviewSystem, "#11059") {
		t.Error("review prompt is missing the one-cause rule or its incident")
	}
	if !strings.Contains(rcChallengeSystemHead, "one allegation, and one check") {
		t.Error("challenge prompt does not treat also-locations as one allegation")
	}
}
