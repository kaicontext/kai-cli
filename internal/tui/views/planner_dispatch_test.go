package views

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/planner"
)

func TestCaptureHeadline(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "chat: agent edits"},
		{"whitespace only", "   \n\t  ", "chat: agent edits"},
		{"trims trailing period", "fix the bug.", "fix the bug"},
		{"trims many trailing puncts", "do the thing!!??...", "do the thing"},
		{"trims trailing comma/semicolon/colon", "do x,;:", "do x"},
		{"keeps internal punctuation", "fix x.y and z", "fix x.y and z"},
		{"collapses newlines to spaces", "line one\nline two", "line one line two"},
		{"collapses CR to space", "a\rb", "a b"},
		{"keeps verb-starting sentence", "Add a new feature", "Add a new feature"},
		{"noun phrase passed through (no verb prefix)", "user authentication", "user authentication"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureHeadline(tc.in)
			if got != tc.want {
				t.Fatalf("captureHeadline(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCaptureHeadline_LongInputTruncatedWithEllipsis(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := captureHeadline(long)
	if len(got) != 120 {
		t.Fatalf("captureHeadline length = %d, want 120", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected trailing ellipsis on truncation, got %q", got)
	}
	// The first 117 chars should be the original prefix.
	if got[:117] != strings.Repeat("a", 117) {
		t.Fatalf("truncation prefix wrong: %q", got)
	}
}

func TestCaptureHeadline_ShortInputNotEllipsized(t *testing.T) {
	got := captureHeadline("short")
	if strings.Contains(got, "...") {
		t.Fatalf("short input should not be ellipsized, got %q", got)
	}
}

func TestComposeCaptureMessage_NoEdits(t *testing.T) {
	got := composeCaptureMessage("fix the bug", nil, 0)
	if got != "fix the bug" {
		t.Fatalf("composeCaptureMessage with nil edits = %q, want headline only", got)
	}
}

func TestComposeCaptureMessage_EmptyEdits(t *testing.T) {
	got := composeCaptureMessage("fix the bug", []editSummary{}, 0)
	if got != "fix the bug" {
		t.Fatalf("composeCaptureMessage with empty edits = %q, want headline only", got)
	}
}

func TestComposeCaptureMessage_WithMixedOps(t *testing.T) {
	edits := []editSummary{
		{Path: "a.go", Op: "M", Added: 12, Removed: 3},
		{Path: "new.go", Op: "A", Added: 40, Removed: 0},
		{Path: "gone.go", Op: "D", Added: 0, Removed: 22},
	}
	got := composeCaptureMessage("refactor parser", edits, len(edits))

	lines := strings.Split(got, "\n")
	if lines[0] != "refactor parser" {
		t.Fatalf("first line = %q, want headline", lines[0])
	}
	if lines[1] != "" {
		t.Fatalf("second line = %q, want blank", lines[1])
	}
	if lines[2] != "Files:" {
		t.Fatalf("third line = %q, want \"Files:\"", lines[2])
	}

	for _, want := range []string{
		"  M a.go (+12 -3)",
		"  A new.go (+40)",
		"  D gone.go (-22)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("composed message missing %q\nfull output:\n%s", want, got)
		}
	}

	// No overflow line when total == len(edits) and <= maxShown.
	if strings.Contains(got, "more") {
		t.Errorf("did not expect overflow line, got:\n%s", got)
	}
}

func TestComposeCaptureMessage_TruncatesAtSix(t *testing.T) {
	edits := []editSummary{
		{Path: "a.go", Op: "M", Added: 1, Removed: 0},
		{Path: "b.go", Op: "M", Added: 1, Removed: 0},
		{Path: "c.go", Op: "M", Added: 1, Removed: 0},
		{Path: "d.go", Op: "M", Added: 1, Removed: 0},
		{Path: "e.go", Op: "M", Added: 1, Removed: 0},
		{Path: "f.go", Op: "M", Added: 1, Removed: 0},
		{Path: "g.go", Op: "M", Added: 1, Removed: 0},
		{Path: "h.go", Op: "M", Added: 1, Removed: 0},
	}
	got := composeCaptureMessage("touch a bunch", edits, len(edits))

	for _, p := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"} {
		if !strings.Contains(got, p) {
			t.Errorf("expected %q in output, missing\n%s", p, got)
		}
	}
	for _, p := range []string{"g.go", "h.go"} {
		if strings.Contains(got, " "+p+" ") || strings.Contains(got, " "+p+"\n") {
			t.Errorf("did not expect %q in listed entries\n%s", p, got)
		}
	}
	if !strings.Contains(got, "and 2 more") {
		t.Errorf("expected \"and 2 more\" overflow line, got:\n%s", got)
	}
}

func TestComposeCaptureMessage_OverflowFromTotal(t *testing.T) {
	// Even when only 2 edits were captured, if total > len(edits)
	// the overflow line should reflect the true total.
	edits := []editSummary{
		{Path: "a.go", Op: "M", Added: 1, Removed: 0},
		{Path: "b.go", Op: "M", Added: 1, Removed: 0},
	}
	got := composeCaptureMessage("partial capture", edits, 5)
	if !strings.Contains(got, "and 3 more") {
		t.Errorf("expected \"and 3 more\" based on total=5 shown=2, got:\n%s", got)
	}
}

func TestComposeCaptureMessage_NoOverflowAtExactlySix(t *testing.T) {
	edits := []editSummary{
		{Path: "a.go", Op: "M", Added: 1, Removed: 0},
		{Path: "b.go", Op: "M", Added: 1, Removed: 0},
		{Path: "c.go", Op: "M", Added: 1, Removed: 0},
		{Path: "d.go", Op: "M", Added: 1, Removed: 0},
		{Path: "e.go", Op: "M", Added: 1, Removed: 0},
		{Path: "f.go", Op: "M", Added: 1, Removed: 0},
	}
	got := composeCaptureMessage("six edits", edits, 6)
	if strings.Contains(got, "more") {
		t.Errorf("did not expect overflow line for exactly 6 edits:\n%s", got)
	}
}

func TestComposeCaptureMessage_AddOnlyOmitsRemoved(t *testing.T) {
	got := composeCaptureMessage("add file", []editSummary{
		{Path: "new.go", Op: "A", Added: 40, Removed: 0},
	}, 1)
	if !strings.Contains(got, "(+40)") {
		t.Errorf("expected (+40) for add-only edit, got:\n%s", got)
	}
	if strings.Contains(got, "-0") {
		t.Errorf("did not expect -0 in add-only edit, got:\n%s", got)
	}
}

func TestComposeCaptureMessage_DeleteOnlyOmitsAdded(t *testing.T) {
	got := composeCaptureMessage("drop file", []editSummary{
		{Path: "gone.go", Op: "D", Added: 0, Removed: 22},
	}, 1)
	if !strings.Contains(got, "(-22)") {
		t.Errorf("expected (-22) for delete-only edit, got:\n%s", got)
	}
	if strings.Contains(got, "+0") {
		t.Errorf("did not expect +0 in delete-only edit, got:\n%s", got)
	}
}

func TestComposeCaptureMessage_ZeroCountsOmitParens(t *testing.T) {
	got := composeCaptureMessage("touch", []editSummary{
		{Path: "x.go", Op: "M", Added: 0, Removed: 0},
	}, 1)
	// When both counts are zero, no (+/-) suffix should be emitted.
	if strings.Contains(got, "(") || strings.Contains(got, ")") {
		t.Errorf("did not expect parens for zero-count edit, got:\n%s", got)
	}
	if !strings.Contains(got, "  M x.go") {
		t.Errorf("expected raw \"  M x.go\" line, got:\n%s", got)
	}
}

func TestComposeCaptureMessage_EmptyOpDefaultsToM(t *testing.T) {
	got := composeCaptureMessage("h", []editSummary{
		{Path: "x.go", Op: "", Added: 1, Removed: 1},
	}, 1)
	if !strings.Contains(got, "  M x.go") {
		t.Errorf("expected empty op to default to M, got:\n%s", got)
	}
}

// TestUnverifiedNeverRendersConfidentDone pins the 2026-06-10 fix: when
// an "already done" verdict's end-to-end verification could not complete
// (stall/cancel), the planner loop sets WorkPlan.Unverified, and the
// renderer must degrade to explicit uncertainty — never the confident
// "✓ Already done" that the verify guard exists to prevent.
func TestUnverifiedNeverRendersConfidentDone(t *testing.T) {
	p := &planner.WorkPlan{
		Summary:    "Already implemented — the three tiers exist across config and billing.",
		Unverified: true,
	}
	out := formatEmptyPlan(p)
	if strings.Contains(out, "✓ Already done") {
		t.Errorf("unverified plan must NOT render confident done:\n%s", out)
	}
	if !strings.Contains(out, "Couldn't verify") {
		t.Errorf("expected 'Couldn't verify' headline, got:\n%s", out)
	}
	// Sanity: without the taint, the same summary still reads as done.
	if ok := formatEmptyPlan(&planner.WorkPlan{Summary: p.Summary}); !strings.Contains(ok, "Already done") {
		t.Errorf("baseline (untainted) should still say 'Already done', got:\n%s", ok)
	}
}
