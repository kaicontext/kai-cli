package main

import (
	"os"
	"strings"
	"testing"
)

func rcHintsFor(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/review-hints/" + name + ".diff")
	if err != nil {
		t.Fatal(err)
	}
	return rcReviewHints(string(b))
}

// Every golden defect the reviewer missed on the benchmark and the first two
// nightly corpus runs has a hint that points at its line, in the real diff.
func TestReviewHintsPointAtTheMissedDefects(t *testing.T) {
	for _, tc := range []struct {
		diff, why string
		want      []string
	}{
		{"calcom-14943", "non-atomic retry increment",
			[]string{"TWO REQUESTS AT ONCE", "scheduleSMSReminders.ts:184  retryCount: reminder.retryCount + 1", "scheduleSMSReminders.ts:195"}},
		{"calcom-10600", "backup code checked then written back",
			[]string{"both pass the check before either write lands", "next-auth-options.ts:144  const index = backupCodes.indexOf("}},
		{"calcom-11059", "credentialId passed where every other call passes credential.userId",
			[]string{`zoho-bigin/lib/CalendarService.ts:85  refreshOAuthTokens(…) passes "credentialId" as argument 3 where the other 7 calls pass "credential.userId"`}},
		{"calcom-11059", "jsforce connection built around a refresh",
			[]string{"salesforce/lib/CalendarService.ts:101  return new jsforce.Connection({"}},
		{"keycloak-36880", "resource looked up by the wrong kind of key",
			[]string{"KIND the record was stored under", "ClientPermissionsV2.java:214  Resource resource =  resourceStore.findByName(server, client.getId(), server.getId());"}},
		{"keycloak-36880", "names returned where callers expect client ids",
			[]string{"ClientPermissionsV2.java:140  granted.add(resource.getName());"}},
		{"keycloak-37038", "resource ids returned where callers expect group ids",
			[]string{"GroupPermissionsV2.java:123  granted.add(groupResource.getId());", "GroupPermissionsV2.java:141"}},
	} {
		got := rcHintsFor(t, tc.diff)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s (%s): hints miss %q\n%s", tc.diff, tc.why, w, got)
			}
		}
	}
}

// The block is context for the reviewer, never a finding list, and it says so.
func TestReviewHintsAreFramedAsChecksNotFindings(t *testing.T) {
	got := rcHintsFor(t, "calcom-14943")
	if !strings.HasPrefix(got, "REVIEW HINTS (") || !strings.Contains(got, "These are NOT findings") ||
		!strings.Contains(got, "reachable trigger") {
		t.Errorf("hint block is not framed as checks:\n%s", got)
	}
}

func TestReviewHintsStayQuietOnOrdinaryCode(t *testing.T) {
	diff := `diff --git a/internal/api/users.go b/internal/api/users.go
--- a/internal/api/users.go
+++ b/internal/api/users.go
@@ -1,3 +1,6 @@
 package api
+func greet(name string) string {
+	count := len(name) + 1
+	return fmt.Sprintf("hello %s (%d)", name, count)
+}
`
	if got := rcReviewHints(diff); got != "" {
		t.Errorf("hints on ordinary code:\n%s", got)
	}
	// Tests are not where these defects live.
	test := strings.ReplaceAll(`diff --git a/internal/api/users_test.go b/internal/api/users_test.go
--- a/internal/api/users_test.go
+++ b/internal/api/users_test.go
@@ -1,1 +1,2 @@
 package api
+	row.retryCount = row.retryCount + 1
`, "\t", "")
	if got := rcReviewHints(test); got != "" {
		t.Errorf("hints on a test file:\n%s", got)
	}
}

func TestOutlierNeedsAClearMajority(t *testing.T) {
	c := func(args ...string) rcCall { return rcCall{Name: "f", Args: args} }
	if got := rcOutlierCalls([]rcCall{c("a", "userId"), c("b", "userId"), c("c", "userId"), c("d", "credentialId")}); len(got) != 1 || got[0].Odd != "credentialId" {
		t.Errorf("clear outlier not reported: %+v", got)
	}
	if got := rcOutlierCalls([]rcCall{c("a", "x"), c("b", "y"), c("c", "z"), c("d", "w")}); len(got) != 0 {
		t.Errorf("reported an outlier where every call differs: %+v", got)
	}
	if got := rcOutlierCalls([]rcCall{c("a", "userId"), c("b", "userId"), c("d", "credentialId")}); len(got) != 0 {
		t.Errorf("reported an outlier from too few calls: %+v", got)
	}
}

func TestSplitArgsHandlesNestingAndStrings(t *testing.T) {
	args, ok := rcSplitArgs(`async () => await fetch("a,b", { method: "POST" }), "zoho-bigin", credentialId)`)
	if !ok || len(args) != 3 || args[2] != "credentialId" || args[1] != `"zoho-bigin"` {
		t.Errorf("args = %q ok=%v", args, ok)
	}
	if _, ok := rcSplitArgs(`a, b`); ok {
		t.Error("an unclosed call was reported complete")
	}
}

func TestPromptsCarryTheHints(t *testing.T) {
	// Both passes put the block in the model's input; the builders are
	// checked by their call sites compiling, the wording here.
	if !strings.Contains(rcReviewHints("diff --git a/x.ts b/x.ts\n--- a/x.ts\n+++ b/x.ts\n@@ -1,1 +1,2 @@\n a\n+n: row.n + 1,\n"), "TWO REQUESTS AT ONCE") {
		t.Error("read-modify-write hint does not name the rule it serves")
	}
}
