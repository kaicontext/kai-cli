package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

func TestStatedIntentAndTitleForMergeCommit(t *testing.T) {
	subs := []string{
		"Add referrals API: GET /api/v1/referrals/me",
		"Fix referral API review issues: entropy, atomicity, convention, comment",
		"Add referral credit grant flow: signup attribution + Pro conversion",
	}
	merge := "Merge origin/main into kai/s-94c6bf29"

	if got := rcStatedIntent(merge, false, subs); got != merge {
		t.Errorf("non-merge keeps its subject, got %q", got)
	}
	if got := rcStatedIntent(merge, true, nil); got != merge {
		t.Errorf("merge with no range falls back to subject, got %q", got)
	}
	got := rcStatedIntent(merge, true, subs)
	if strings.Contains(got, "Merge origin/main") {
		t.Errorf("merge intent must not be the merge line: %q", got)
	}
	for _, s := range subs {
		if !strings.Contains(got, "- "+s) {
			t.Errorf("stated intent missing %q:\n%s", s, got)
		}
	}

	if got := rcTitle(merge, true, subs); got != "Add referral credit grant flow: signup attribution + Pro conversion (+2 commits)" {
		t.Errorf("title = %q", got)
	}
	if got := rcTitle(merge, true, subs[:1]); got != subs[0] {
		t.Errorf("single-commit title = %q", got)
	}
	if got := rcTitle("Squash", false, subs); got != "Squash" {
		t.Errorf("non-merge title = %q", got)
	}

	ctx := rcAuthorContext(merge, "", true, subs, []string{"body one", "", "body three"})
	if !strings.Contains(ctx, "3 commit(s)") || !strings.Contains(ctx, "## "+subs[1]) || !strings.Contains(ctx, "body three") {
		t.Errorf("author context for merge:\n%s", ctx)
	}
	if got := rcAuthorContext("Subj", "Body", false, nil, nil); got != "Subj\n\nBody" {
		t.Errorf("plain author context = %q", got)
	}
}

// CI reviews `$HEAD_SHA --base $BASE_REF`, and a pull request's head is an
// ordinary commit, so isMerge was false on every review a customer ever got —
// and rcAuthorContext threw the branch's commits away and handed the reviewer
// the LAST commit's message as the author's account of the whole change.
// rcRangeCommits had already collected them one line above the call.
func TestAuthorContextUsesEveryCommitOnANonMergeHead(t *testing.T) {
	subs := []string{
		"db: pool seats into one org budget",
		"api: charge the org bucket before the seat",
		"console: show the pooled balance",
	}
	bodies := []string{"the migration rekeys daily_usage", "", "and the 429 body loses its dollars"}

	got := rcAuthorContext(subs[2], bodies[2], false, subs, bodies)
	for _, want := range append(append([]string{}, subs...), "3 commit(s)", "the migration rekeys daily_usage") {
		if !strings.Contains(got, want) {
			t.Errorf("author context for a multi-commit branch is missing %q:\n%s", want, got)
		}
	}
	// A merge still reads as a merge — that wording is load-bearing, because
	// "Merge pull request #N" is not a goal and the listing is what replaces it.
	if strings.Contains(got, "a merge bringing in") {
		t.Error("a branch head is not a merge and must not be described as one")
	}

	// One commit states itself; nothing changes for the common case.
	if g := rcAuthorContext("Subj", "Body", false, []string{"Subj"}, []string{"Body"}); g != "Subj\n\nBody" {
		t.Errorf("single-commit author context = %q", g)
	}
}

func TestParseReviewOutputEmptyListBullets(t *testing.T) {
	raw := "Looks fine.\n\n===REVIEW-DATA===\nINTENT_MATCH: verified\nSUMMARY: ok\nISSUES:\n- (none)\nDECISIONS:\n- None.\n- Decision: the cap moves from 5 to 7 for every org\n"
	_, risks, decisions, match, _, _ := rcParseReviewOutput(raw)
	if len(risks) != 0 {
		t.Errorf("\"(none)\" must not become an issue, got %v", risks)
	}
	if len(decisions) != 1 || !strings.HasPrefix(decisions[0], "Decision: the cap") {
		t.Errorf("\"None.\" must be dropped and the real decision kept, got %v", decisions)
	}
	if match != finding.MatchVerified {
		t.Errorf("match = %v", match)
	}
	for _, s := range []string{"(none)", "None.", "n/a", "No issues", "**none**", ""} {
		if !rcIsEmptyListItem(s) {
			t.Errorf("%q should read as an empty list", s)
		}
	}
	for _, s := range []string{"none of the callers check the error", "billing.go:12 — nothing guards the nil"} {
		if rcIsEmptyListItem(s) {
			t.Errorf("%q is a real item", s)
		}
	}
}

func TestBranchNamePrecedence(t *testing.T) {
	t.Setenv("GITHUB_HEAD_REF", "feat/from-pr")
	t.Setenv("GITHUB_REF_NAME", "123/merge")
	if got := rcBranchName("explicit"); got != "explicit" {
		t.Errorf("flag wins, got %q", got)
	}
	if got := rcBranchName(""); got != "feat/from-pr" {
		t.Errorf("GITHUB_HEAD_REF next, got %q", got)
	}
	t.Setenv("GITHUB_HEAD_REF", "")
	if got := rcBranchName(""); got != "123/merge" {
		t.Errorf("GITHUB_REF_NAME next, got %q", got)
	}
}

func TestIssueLocation(t *testing.T) {
	cases := map[string]struct {
		path string
		line int
		ok   bool
	}{
		"kailab-control/internal/api/billing.go:960-987 — reward grant runs after the flip":   {"kailab-control/internal/api/billing.go", 960, true},
		"kailab-control/internal/api/billing.go:978,983 — the rewardRef does not deduplicate": {"kailab-control/internal/api/billing.go", 978, true},
		"internal/db/referrals.go:139-155 & schema/0052 — no unique constraint":               {"internal/db/referrals.go", 139, true},
		"`server/handler.go:88` — bucket map accessed without a lock":                         {"server/handler.go", 88, true},
		"Decision: the reward is $10 by default":                                              {"", 0, false},
		"schema/0052 — no line given":                                                         {"", 0, false},
	}
	for item, want := range cases {
		path, line, ok := rcIssueLocation(item)
		if path != want.path || line != want.line || ok != want.ok {
			t.Errorf("%q → (%q, %d, %v), want (%q, %d, %v)", item, path, line, ok, want.path, want.line, want.ok)
		}
	}
}

func TestGroundIssue(t *testing.T) {
	files := map[string][]string{
		"internal/api/billing.go": {"package api", "", "func checkReferralConversion(orgID string) {", "\trows, cErr := h.db.MarkReferralConverted(org.OwnerID)", "}"},
	}
	read := func(_ string, path string) ([]string, bool) {
		l, ok := files[path]
		return l, ok
	}
	hash := "01b3cf323d89eed7aaaaaaaaaaaaaaaaaaaaaaaa"

	c := rcGroundIssue(hash, "internal/api/billing.go:4 — reward grant runs after the flip", nil, read)
	if !c.Resolved || !c.Verified || c.Tag != finding.TagRisk {
		t.Errorf("resolvable location must be a grounded risk: %+v", c)
	}
	if !strings.Contains(c.Lookup, "internal/api/billing.go:4 @ 01b3cf323d89") || !strings.Contains(c.Lookup, "MarkReferralConverted") {
		t.Errorf("lookup must quote the source line: %q", c.Lookup)
	}

	c = rcGroundIssue(hash, "internal/api/billing.go:40 — beyond the end", nil, read)
	if c.Resolved || c.Verified || !strings.Contains(c.Lookup, "has 5 lines") {
		t.Errorf("line past EOF must be held with a reason: %+v", c)
	}

	c = rcGroundIssue(hash, "internal/api/nope.go:1 — missing file", nil, read)
	if c.Resolved || c.Verified || !strings.Contains(c.Lookup, "does not exist at 01b3cf323d89") {
		t.Errorf("missing file must be held with a reason: %+v", c)
	}

	c = rcGroundIssue(hash, "the map is unlocked somewhere", nil, read)
	if c.Resolved || c.Verified || !strings.Contains(c.Lookup, "no path:line") {
		t.Errorf("unlocated issue must be held: %+v", c)
	}

	// A bare file name resolves through the tree when it is unique there;
	// an ambiguous or absent one is held with the reason.
	tree := []string{"frontend/dist/panel-changes.js", "internal/api/billing.go", "a/util.go", "b/util.go"}
	files["frontend/dist/panel-changes.js"] = []string{"// panel", "reviewFor = function () {}"}
	c = rcGroundIssue(hash, "panel-changes.js:2 — reviewFor skips the session check", tree, read)
	if !c.Resolved || !strings.Contains(c.Lookup, "frontend/dist/panel-changes.js:2 @") || !strings.Contains(c.Lookup, "reviewFor") {
		t.Errorf("bare file name must resolve through the tree: %+v", c)
	}
	c = rcGroundIssue(hash, "util.go:1 — ambiguous", tree, read)
	if c.Resolved || !strings.Contains(c.Lookup, "matches 2 files") {
		t.Errorf("ambiguous name must be held with the count: %+v", c)
	}
	c = rcGroundIssue(hash, "ghost.go:1 — absent", tree, read)
	if c.Resolved || !strings.Contains(c.Lookup, "does not exist") {
		t.Errorf("absent name must be held: %+v", c)
	}
	if p, n := rcResolvePath("internal/api/billing.go", tree); p != "internal/api/billing.go" || n != 1 {
		t.Errorf("exact path: got (%q, %d)", p, n)
	}

	d := rcDecisionClaim("the reward is $10 in credits to the referrer")
	if !strings.HasPrefix(d.Statement, "Decision: ") || !d.Resolved || !d.Verified || d.Lookup == "" {
		t.Errorf("decision claim: %+v", d)
	}
}

const rcHostDiff = `diff --git a/internal/cfg/config.go b/internal/cfg/config.go
index 1..2 100644
--- a/internal/cfg/config.go
+++ b/internal/cfg/config.go
@@ -360,6 +360,8 @@ func FromEnv() *Config {
 		KitReleaseRepo:         getEnv("KLC_KIT_RELEASE_REPO", "kaicontext/kai-tui"),
 		BraveAPIKey:            getEnv("BRAVE_API_KEY", ""),
+		// See https://docs.kai.dev/referrals for the link format.
+		ReferralBaseURL:        getEnv("KLC_REFERRAL_BASE_URL", "https://kai.dev/r/"),
+		StripeDocs:             "https://stripe.com/docs/webhooks",
 	}

diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1,2 +1,3 @@
 # Kai
+Share links look like https://kai.dev/r/<code>.
diff --git a/internal/api/oauth.go b/internal/api/oauth.go
--- a/internal/api/oauth.go
+++ b/internal/api/oauth.go
@@ -10,3 +10,4 @@
 	q := url.Values{}
-	old := "https://gone.example.io"
+	q.Set("redirect_uri", "https://login.partner-idp.net/callback")
+	q.Set("docs", "https://api.github.com/x")
`

func TestAddedURLHosts(t *testing.T) {
	hosts := rcAddedURLHosts(rcHostDiff)
	var got []string
	for _, h := range hosts {
		got = append(got, h.Host+"@"+h.Path+":"+itoa(h.Line))
	}
	want := []string{
		"kai.dev@internal/cfg/config.go:363",
		"login.partner-idp.net@internal/api/oauth.go:11",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("hosts = %v, want %v", got, want)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestNewHosts(t *testing.T) {
	// kai.dev appears only in the changed file → listed; the partner IdP host
	// is mentioned by an untouched file → not listed.
	mention := func(_ string, host string) []string {
		switch host {
		case "kai.dev":
			return []string{"internal/cfg/config.go"}
		case "login.partner-idp.net":
			return []string{"internal/api/oauth.go", "docs/sso.txt"}
		}
		return nil
	}
	hosts := rcNewHosts("01b3cf323d89eed7", rcHostDiff, []string{"internal/cfg/config.go", "README.md", "internal/api/oauth.go"}, mention)
	if len(hosts) != 1 || hosts[0].Host != "kai.dev" || hosts[0].Path != "internal/cfg/config.go" || hosts[0].Line != 363 {
		t.Fatalf("hosts = %+v, want only kai.dev at internal/cfg/config.go:363", hosts)
	}
}

// The benchmark's three bogus findings (2026-09-24): a test fixture, a
// developer script and an OAuth authority, each a host no other file
// mentioned. They reach the reviewer as context with the bar for reporting
// them — never as findings of their own.
func TestNewHostsAreContextNotFindings(t *testing.T) {
	diff := `diff --git a/services/tests/src/test/java/RedirectTest.java b/services/tests/src/test/java/RedirectTest.java
--- a/services/tests/src/test/java/RedirectTest.java
+++ b/services/tests/src/test/java/RedirectTest.java
@@ -40,2 +40,3 @@
 	@Test
+	String evil = "https://malicious.com/steal";
 	void redirect() {}
diff --git a/scripts/dev-tunnel.sh b/scripts/dev-tunnel.sh
--- a/scripts/dev-tunnel.sh
+++ b/scripts/dev-tunnel.sh
@@ -1,1 +1,2 @@
 set -e
+open "https://dashboard.tunnelmole.com"
diff --git a/packages/app-store/office365/api/add.ts b/packages/app-store/office365/api/add.ts
--- a/packages/app-store/office365/api/add.ts
+++ b/packages/app-store/office365/api/add.ts
@@ -5,1 +5,2 @@
 const scopes = ["Calendars.ReadWrite"];
+const authority = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize";
`
	nobodyElse := func(string, string) []string { return nil }
	hosts := rcNewHosts("abc1234", diff, nil, nobodyElse)
	block := rcNewHostsBlock(hosts)
	for _, want := range []string{
		"HOSTS THIS CHANGE INTRODUCES (context, not findings",
		"services/tests/src/test/java/RedirectTest.java:41 malicious.com",
		"scripts/dev-tunnel.sh:2 dashboard.tunnelmole.com",
		"packages/app-store/office365/api/add.ts:6 login.microsoftonline.com",
		"A host being new to the repository is not a defect.",
		"concrete incorrect URL or a code path that fails",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block is missing %q:\n%s", want, block)
		}
	}
	if rcNewHostsBlock(nil) != "" {
		t.Error("no new hosts must add nothing to the reviewer's input")
	}
}

// The reviewer is told the same bar, and no longer told the pipeline files
// new hosts as risks on its behalf.
func TestReviewPromptRequiresEvidenceForHosts(t *testing.T) {
	for _, want := range []string{
		"a host being new to the repository is NOT a defect by itself",
		"concrete incorrect URL",
		"a code path that fails because of it",
		"test fixture",
		"it is context, not a finding list",
	} {
		if !strings.Contains(rcReviewSystem, want) {
			t.Errorf("review prompt is missing %q", want)
		}
	}
	if strings.Contains(rcReviewSystem, "files them as risks") {
		t.Error("the prompt still says the pipeline files new hosts as risks")
	}
}

func TestHostIsKnown(t *testing.T) {
	for _, h := range []string{"api.github.com", "stripe.com", "js.stripe.com", "localhost", "fonts.googleapis.com"} {
		if !rcHostIsKnown(h) {
			t.Errorf("%s should be known", h)
		}
	}
	for _, h := range []string{"kai.dev", "notgithub.com", "stripe.com.evil.io"} {
		if rcHostIsKnown(h) {
			t.Errorf("%s should not be known", h)
		}
	}
}
