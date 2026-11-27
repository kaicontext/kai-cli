package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

func rcCutFiles(paths ...string) []finding.DiffFile {
	out := make([]finding.DiffFile, 0, len(paths))
	for _, p := range paths {
		out = append(out, finding.DiffFile{Path: p, Action: "modified", Added: 10})
	}
	return out
}

// No limit, nothing is cut; the threshold is the limit itself unless a higher
// one is given, and a value that is not a positive number is no limit.
func TestRiskiestLimits(t *testing.T) {
	for _, c := range []struct {
		files, over string
		keep, wantO int
	}{
		{"", "", 0, 0}, {"", "30", 0, 0}, {"5", "", 5, 5}, {"5", "30", 5, 30},
		{"5", "2", 5, 5}, {"0", "30", 0, 0}, {"five", "30", 0, 0}, {"5", "many", 5, 5},
	} {
		t.Setenv("KAI_REVIEW_RISKIEST_FILES", c.files)
		t.Setenv("KAI_REVIEW_RISKIEST_OVER", c.over)
		if keep, over := rcRiskiestLimits(); keep != c.keep || over != c.wantO {
			t.Errorf("FILES=%q OVER=%q: keep %d over %d, want %d and %d", c.files, c.over, keep, over, c.keep, c.wantO)
		}
	}
}

// A change at or under the threshold is reviewed whole.
func TestRiskiestCutLeavesSmallChangesWhole(t *testing.T) {
	files := rcCutFiles("a.go", "b.go", "c.go")
	if cut := rcRiskiestCut(files, rcPathsOf(files), 0, 0); cut != nil {
		t.Errorf("no limit: cut %+v", cut)
	}
	if cut := rcRiskiestCut(files, rcPathsOf(files), 2, 3); cut != nil {
		t.Errorf("3 files, threshold 3: cut %+v", cut)
	}
	if cut := rcRiskiestCut(files, rcPathsOf(files), 3, 1); cut != nil {
		t.Errorf("3 files, keep 3: cut %+v", cut)
	}
}

// The ranking: code in a sensitive area, then other code by size, with
// configuration ahead of code its size; tests and docs last however large;
// lockfiles and generated files never counted or kept.
func TestRiskiestCutRanksFiles(t *testing.T) {
	files := []finding.DiffFile{
		{Path: "docs/guide.md", Added: 900},
		{Path: "internal/api/list_test.go", Added: 400},
		{Path: "internal/api/list.go", Added: 40},
		{Path: "internal/billing/charge.go", Added: 3},
		{Path: "db/0012_add_index.sql", Added: 2},
		{Path: "internal/api/big.go", Added: 300, Removed: 100},
		{Path: ".github/workflows/ci.yml", Added: 40},
		{Path: "go.sum", Added: 2000},
		{Path: "internal/api/gen.pb.go", Added: 5000},
		{Path: "internal/api/tiny.go", Added: 1},
	}
	cut := rcRiskiestCut(files, rcPathsOf(files), 5, 5)
	if cut == nil {
		t.Fatal("no cut")
	}
	want := []string{"internal/billing/charge.go", "db/0012_add_index.sql", "internal/api/big.go", ".github/workflows/ci.yml", "internal/api/list.go"}
	if !reflect.DeepEqual(cut.Kept, want) || cut.Of != 8 {
		t.Errorf("kept %v of %d\nwant %v of 8", cut.Kept, cut.Of, want)
	}
	// The same files in another order rank the same.
	rev := make([]finding.DiffFile, len(files))
	for i, f := range files {
		rev[len(files)-1-i] = f
	}
	if again := rcRiskiestCut(rev, rcPathsOf(rev), 5, 5); !reflect.DeepEqual(again.Kept, want) {
		t.Errorf("reordered: kept %v", again.Kept)
	}
}

// A re-review's focus is what is cut, not the whole change.
func TestRiskiestCutOfAFocus(t *testing.T) {
	files := rcCutFiles("a.go", "b.go", "c.go", "auth/login.go", "d.go", "e.go")
	if cut := rcRiskiestCut(files, []string{"a.go", "b.go"}, 2, 3); cut != nil {
		t.Errorf("a two-file focus under the threshold: cut %+v", cut)
	}
	cut := rcRiskiestCut(files, []string{"a.go", "b.go", "c.go", "d.go"}, 2, 3)
	if cut == nil || cut.Of != 4 || !reflect.DeepEqual(cut.Kept, []string{"a.go", "b.go"}) {
		t.Errorf("a four-file focus: %+v, want a.go and b.go of 4 (auth/login.go is outside the focus)", cut)
	}
}

// The reviewer's diff keeps the kept files whole and names the rest with
// their size, and the budgets follow the kept files.
func TestCutDiff(t *testing.T) {
	sec := func(p, body string) string {
		return fmt.Sprintf("diff --git a/%s b/%s\nindex 1..2 100644\n--- a/%s\n+++ b/%s\n@@ -1,2 +1,2 @@\n ctx\n%s", p, p, p, p, body)
	}
	raw := sec("a.go", "-old a\n+new a\n") + sec("b.go", "-old b\n+new b\n+more b\n") + sec("c.go", "+new c\n")
	got := rcCutDiff(raw, []string{"b.go"}, 1<<20)
	for _, want := range []string{"+new b\n+more b\n", "diff --git a/a.go b/a.go\n(not part of this review: 2 changed line(s), not shown)\n",
		"diff --git a/c.go b/c.go\n(not part of this review: 1 changed line(s), not shown)\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("cut diff lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"new a", "new c"} {
		if strings.Contains(got, not) {
			t.Errorf("cut diff still shows %q:\n%s", not, got)
		}
	}
	if long := rcCutDiff(raw, []string{"a.go", "b.go", "c.go"}, 40); !strings.HasSuffix(long, "... (diff truncated)\n") || len(long) > 40+len("\n... (diff truncated)\n") {
		t.Errorf("over the byte limit: %q", long)
	}
	twenty := make([]string, 20)
	for i := range twenty {
		twenty[i] = fmt.Sprintf("f%d.go", i)
	}
	if got, want := rcReviewMaxTurns(len(rcReviewablePaths(twenty[:5], twenty))), rcReviewMaxTurns(5); got != want {
		t.Errorf("turns for 5 kept of 20 = %d, want %d", got, want)
	}
}

func TestFileCutBlock(t *testing.T) {
	if b := rcFileCutBlock(nil); b != "" {
		t.Errorf("no cut: %q", b)
	}
	b := rcFileCutBlock(&rcFileCut{Kept: []string{"a.go", "b.go"}, Of: 40})
	for _, want := range []string{"40 reviewable files", "the 2 ranked riskiest: a.go, b.go", "do not report findings in them"} {
		if !strings.Contains(b, want) {
			t.Errorf("block lacks %q: %s", want, b)
		}
	}
}

// End to end: with a limit of one file on a two-file change, the reviewer is
// sent one file's diff, told so, and the bundle names the cut. Without the
// limit the bundle has no cut.
func TestLargeChangeIsReviewedOnItsRiskiestFiles(t *testing.T) {
	run := func(t *testing.T, limit string) (rcFakeBundle, *rcFakeModelServer) {
		f := newRcFakeModelServer(t, rcFakeScenario{Main: "review"})
		f.root = rcFakeRepo(t)
		rcFakeReviewEnv(t, f, "fake-main", nil)
		t.Setenv("KAI_REVIEW_RISKIEST_FILES", limit)
		t.Setenv("KAI_REVIEW_RISKIEST_OVER", "")
		out, err := rcFakeRunReview(t)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		var b rcFakeBundle
		if jerr := json.Unmarshal([]byte(out), &b); jerr != nil {
			t.Fatalf("bundle: %v\n%s", jerr, out)
		}
		return b, f
	}
	t.Run("no limit", func(t *testing.T) {
		b, _ := run(t, "")
		if b.Run == nil || b.Run.FileCut != nil {
			t.Fatalf("run %+v, want no file cut", b.Run)
		}
	})
	t.Run("limit of one", func(t *testing.T) {
		b, f := run(t, "1")
		if b.Run == nil || b.Run.FileCut == nil || b.Run.FileCut.Of != 2 || len(b.Run.FileCut.Kept) != 1 {
			t.Fatalf("run %+v, want one file kept of 2", b.Run)
		}
		kept := b.Run.FileCut.Kept[0]
		told := false
		for _, c := range f.Calls() {
			if strings.Contains(c.Prompt, "LARGE CHANGE: this change has 2 reviewable files") {
				told = true
				if !strings.Contains(c.Prompt, "(not part of this review: ") || !strings.Contains(c.Prompt, "+++ b/"+kept) {
					t.Errorf("the reviewer's prompt does not show the cut diff:\n%s", c.Prompt)
				}
			}
		}
		if !told {
			t.Error("no prompt told the reviewer about the cut")
		}
	})
}
