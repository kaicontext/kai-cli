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

// Each kind of hint points at the line that has its shape, in a synthetic
// diff written for the test (testdata/review-hints/PROVENANCE).
func TestReviewHintsPointAtTheMissedDefects(t *testing.T) {
	for _, tc := range []struct {
		diff, why string
		want      []string
	}{
		{"lost-update", "a counter written back from the value just read",
			[]string{"TWO REQUESTS AT ONCE", "src/jobs/worker.ts:8  data: { attempts: job.attempts + 1 },"}},
		{"check-then-write", "a membership check on a collection the same code writes back",
			[]string{"both pass the check before either write lands", "src/invites/redeem.ts:6  const index = codes.indexOf(code);"}},
		{"argument-outlier", "one call passes a different value where every other call agrees",
			[]string{`src/billing/notify.ts:16  notify(…) passes "org.name" as argument 3 where the other 4 calls pass "org.billingEmail"`}},
		{"client-around-refresh", "a client built from credentials read before the refresh",
			[]string{"src/storage/client.ts:8  return new StorageClient({", "src/storage/client.ts:9  token: creds.accessToken,"}},
		{"lookup-collect", "a lookup by one kind of key, and names collected for callers",
			[]string{"KIND the record was stored under", "ProjectPermissions.java:6  for (Entry entry : store.findByOwner(org, org.getId())) {", "ProjectPermissions.java:7  granted.add(entry.getName());"}},
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
	got := rcHintsFor(t, "lost-update")
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
