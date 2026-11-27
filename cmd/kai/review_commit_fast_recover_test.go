package main

import (
	"strings"
	"testing"
)

// The shape GPT-5.4 mini returned on an OpenAI key: each concern a paragraph
// written like a bullet, and a coda that stops before ISSUES.
const rcFastDraftWithoutList = `Local changes on main, not pushed; this is a fast pass over the diff only.

The patch flips the cap check in cmd/kai/review_local.go from a sum to a difference.

cmd/kai/review_local.go:312 — The cap guard now subtracts reservedUSD instead of adding it, so a run can admit calls past its cap. (severity: high)

cmd/kai/review_local.go:314 — The error message still reports the sum, which is no longer what the guard tests. (severity: low)

INTENT_MATCH: partial
MERGE_READY: 2`

func TestFastRecoverIssues(t *testing.T) {
	paths := []string{"cmd/kai/review_local.go"}

	got, n := rcFastRecoverIssues(rcFastDraftWithoutList, paths)
	if n != 2 {
		t.Fatalf("recovered %d issues, want 2\n%s", n, got)
	}
	_, issues, _, _, readiness, _ := rcParseReviewOutput(got)
	if len(issues) != 2 || !strings.HasPrefix(issues[0], "cmd/kai/review_local.go:312") || !strings.HasPrefix(issues[1], "cmd/kai/review_local.go:314") {
		t.Errorf("parsed issues = %q", issues)
	}
	if _, _, _, _, before, _ := rcParseReviewOutput(rcFastDraftWithoutList); readiness != before {
		t.Errorf("readiness changed from %v to %v", before, readiness)
	}

	// A draft that already has its list is left exactly as it is.
	withList := rcFastDraftWithoutList + "\nISSUES:\n- cmd/kai/review_local.go:312 — The cap guard subtracts. (severity: high)"
	if d, n := rcFastRecoverIssues(withList, paths); n != 0 || d != withList {
		t.Errorf("a draft with an ISSUES list was changed (n=%d)", n)
	}

	// A line for a file that is not in the diff, a line with no severity
	// label, and ordinary prose are not issues.
	for _, draft := range []string{
		"Fine.\n\nother/file.go:10 — Something is wrong here. (severity: high)\n\nINTENT_MATCH: verified\nMERGE_READY: 4",
		"Fine.\n\ncmd/kai/review_local.go:312 — the guard changed, which looks deliberate.\n\nINTENT_MATCH: verified\nMERGE_READY: 4",
		"Looks right to me.\n\nINTENT_MATCH: verified\nMERGE_READY: 4",
	} {
		if d, n := rcFastRecoverIssues(draft, paths); n != 0 || d != draft {
			t.Errorf("recovered %d issue(s) from a draft with none:\n%s", n, draft)
		}
	}

	// Bulleted, backticked and repeated lines are each taken once.
	loose := "Summary.\n\n- `cmd/kai/review_local.go:312` - The guard subtracts. (severity: high)\n- `cmd/kai/review_local.go:312` - The guard subtracts. (severity: high)\n\nMERGE_READY: 2"
	d, n := rcFastRecoverIssues(loose, paths)
	if n != 1 || !strings.HasSuffix(d, "ISSUES:\n- cmd/kai/review_local.go:312 — The guard subtracts. (severity: high)") {
		t.Errorf("a bulleted, backticked, repeated line gave %d issues:\n%s", n, d)
	}
}

// A renamed file's entry in the changed paths is a fragment of numstat's
// "{old => new}" form; a concern written against its new path is still
// recovered, from the diff's file headers.
func TestFastRecoverIssuesOnARenamedFile(t *testing.T) {
	diff := "diff --git a/cmd/kai/old.go b/cmd/kai/new.go\nsimilarity index 90%\nrename from cmd/kai/old.go\nrename to cmd/kai/new.go\n--- a/cmd/kai/old.go\n+++ b/cmd/kai/new.go\n@@ -1 +1 @@\n-a\n+b\n"
	numstat := []string{"cmd/kai/{old.go"}
	draft := "Renamed and changed.\n\ncmd/kai/new.go:1 — The value is now wrong. (severity: high)\n\nMERGE_READY: 2"
	if _, n := rcFastRecoverIssues(draft, numstat); n != 0 {
		t.Fatalf("the numstat spelling alone matched (n=%d): the test no longer shows the gap", n)
	}
	if _, n := rcFastRecoverIssues(draft, rcFastDiffPaths(diff, numstat)); n != 1 {
		t.Errorf("a concern about a renamed file was not recovered (n=%d)", n)
	}
}

func TestFastDiffPaths(t *testing.T) {
	has := func(paths []string, want string) bool {
		for _, p := range paths {
			if p == want {
				return true
			}
		}
		return false
	}
	// A pure rename and a mode change have no "+++ b/" line.
	diff := "diff --git a/a/old.go b/a/new.go\nsimilarity index 100%\nrename from a/old.go\nrename to a/new.go\n" +
		"diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\n" +
		// An added line of content that reads like a header names no file.
		"diff --git a/doc.md b/doc.md\n--- a/doc.md\n+++ b/doc.md\n@@ -1 +1,2 @@\n text\n+++ b/not/in/the/change.go\n+diff --git a/x b/also/not.go\n"
	got := rcFastDiffPaths(diff, nil)
	for _, want := range []string{"a/new.go", "run.sh", "doc.md"} {
		if !has(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	for _, not := range []string{"not/in/the/change.go", "also/not.go"} {
		if has(got, not) {
			t.Errorf("%s was read out of file content: %v", not, got)
		}
	}
}
