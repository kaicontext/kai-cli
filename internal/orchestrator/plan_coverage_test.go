package orchestrator

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/planner"
)

func TestExtractSymbols(t *testing.T) {
	prompt := "Add `planDetailsExpanded bool` and call `formatPlanDetails(r.pendingPlan)`. " +
		"Reset in `dispatchPlanChoice`. The `?` key triggers it."
	got := extractSymbols(prompt)
	want := map[string]bool{
		"planDetailsExpanded": true,
		"formatPlanDetails":   true,
		"pendingPlan":         true,
		"dispatchPlanChoice":  true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d symbols (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected symbol %q", s)
		}
	}
}

func TestExtractSymbolsIgnoresPlainWords(t *testing.T) {
	// Inside-backtick English words without internal capitals/underscores
	// must not be treated as code identifiers — they would false-flag
	// every prompt.
	prompt := "Set `details` to the value and `render` it."
	got := extractSymbols(prompt)
	if len(got) != 0 {
		t.Fatalf("plain English in backticks should not extract: got %v", got)
	}
}

func TestExtractFiles(t *testing.T) {
	prompt := "Edit kai-cli/internal/tui/views/repl.go and look at planner_dispatch.go too."
	got := extractFiles(prompt)
	if len(got) != 2 {
		t.Fatalf("got %d files, want 2: %v", len(got), got)
	}
	if got[0] != "kai-cli/internal/tui/views/repl.go" && got[1] != "kai-cli/internal/tui/views/repl.go" {
		t.Errorf("missing main path in %v", got)
	}
}

func TestUnderCovered(t *testing.T) {
	tests := []struct {
		name string
		c    coverageResult
		want bool
	}{
		{"too few signals", coverageResult{TotalSignals: 2, Matched: 0}, false},
		{"fully covered", coverageResult{TotalSignals: 5, Matched: 5}, false},
		{"half covered", coverageResult{TotalSignals: 6, Matched: 3}, false},
		{"under covered", coverageResult{TotalSignals: 6, Matched: 2}, true},
		{"round-12 shape", coverageResult{TotalSignals: 6, Matched: 1}, true},
		{"barely missing", coverageResult{TotalSignals: 5, Matched: 2}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.UnderCovered(); got != tc.want {
				t.Errorf("UnderCovered() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReasonShape(t *testing.T) {
	c := coverageResult{
		TotalSignals:   6,
		Matched:        1,
		MissingSymbols: []string{"pendingPlan", "formatPlanDetails", "dispatchPlanChoice", "pendingPrints"},
		MissingFiles:   nil,
	}
	r := c.Reason()
	if !strings.Contains(r, "1/6") {
		t.Errorf("reason missing ratio: %q", r)
	}
	if !strings.Contains(r, "pendingPlan") || !strings.Contains(r, "formatPlanDetails") {
		t.Errorf("reason missing identifiers: %q", r)
	}
}

func TestExtractPlanSignals_Round14Shape(t *testing.T) {
	// Round-14 dogfood (2026-05-13): planner emitted this prompt;
	// worker produced 0 changes; we needed the signals to decide
	// whether to treat the no-op as failure. Confirm the extractor
	// pulls ≥3 signals from this shape so the zero-edits guard fires.
	prompt := "In kai-cli/internal/tui/views/repl.go around line 1040, the `?` " +
		"key handler calls `r.write(details)`. Add `planDetailsShown bool` to " +
		"the REPL struct. Reset in `dispatchPlanChoice` and the esc handler. " +
		"`clearTransient` is already used by the cancel path. `formatPlanDetails` " +
		"is called from gate_review.go, app.go, tui.go."
	syms, files := extractPlanSignals(prompt)
	if len(syms)+len(files) < 3 {
		t.Fatalf("expected ≥3 signals from round-14 prompt, got %d (syms=%v files=%v)",
			len(syms)+len(files), syms, files)
	}
	// Verify the key identifiers landed.
	want := map[string]bool{
		"planDetailsShown":   true,
		"dispatchPlanChoice": true,
		"clearTransient":     true,
		"formatPlanDetails":  true,
	}
	got := map[string]bool{}
	for _, s := range syms {
		got[s] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing expected symbol %q in %v", k, syms)
		}
	}
}

func TestReasonCaps(t *testing.T) {
	// >6 missing → must elide.
	c := coverageResult{
		TotalSignals:   10,
		Matched:        2,
		MissingSymbols: []string{"a1B", "c2D", "e3F", "g4H", "i5J", "k6L", "m7N", "o8P"},
	}
	r := c.Reason()
	if !strings.Contains(r, "…") {
		t.Errorf("reason should elide >6 missing: %q", r)
	}
}

// TestCheckPlanCoverage_MultiRootPathsMatch pins the 2026-05-21 fix:
// the planner names target files with multi-root prefixes
// ("kai-server/foo.go") and the orchestrator's `changed` slice uses
// the same convention. The check must compare them directly and
// produce 100% coverage when every named file appears in the
// changed list.
func TestCheckPlanCoverage_MultiRootPathsMatch(t *testing.T) {
	task := mustTask([]string{
		"kai-server/kailab-control/internal/api/search.go",
		"kai-server/kailab-control/internal/cfg/config.go",
		"kai/kai-cli/internal/agent/tools/kai.go",
	})
	changed := []string{
		"kai-server/kailab-control/internal/api/search.go",
		"kai-server/kailab-control/internal/cfg/config.go",
		"kai/kai-cli/internal/agent/tools/kai.go",
	}
	got := checkPlanCoverage(task, changed)
	if got.TotalSignals != 3 || got.Matched != 3 {
		t.Errorf("got %d/%d matched, want 3/3", got.Matched, got.TotalSignals)
	}
	if got.UnderCovered() {
		t.Errorf("full match should not under-cover")
	}
}

// TestCheckPlanCoverage_NormalizationStripsPrefix confirms the
// defensive normalization: planner says "kai-server/foo.go" but
// the diff has "foo.go" (or vice-versa). Strip-and-compare absorbs
// the format mismatch instead of false-positive holding.
func TestCheckPlanCoverage_NormalizationStripsPrefix(t *testing.T) {
	task := mustTask([]string{
		"kai-server/foo.go", // planner-prefixed
		"bar.go",            // unprefixed
	})
	changed := []string{
		"foo.go",            // diff side: unprefixed
		"kai-server/bar.go", // diff side: prefixed
	}
	got := checkPlanCoverage(task, changed)
	if got.Matched != 2 {
		t.Errorf("strip-and-compare should match both ways, got %d/2 (missing=%v)",
			got.Matched, got.MissingFiles)
	}
}

// TestCheckPlanCoverage_EmptyTaskFilesNoUnderCover confirms that
// vague plans (no files named by the planner) don't trigger
// under-coverage — there's no target to verify against. Different
// from "no diff at all"; this is "planner gave us nothing to
// check," so the right behavior is silent.
func TestCheckPlanCoverage_EmptyTaskFilesNoUnderCover(t *testing.T) {
	task := mustTask(nil)
	changed := []string{"kai-server/foo.go", "kai-server/bar.go"}
	got := checkPlanCoverage(task, changed)
	if got.TotalSignals != 0 || got.UnderCovered() {
		t.Errorf("empty task.Files should produce zero-signal non-under-cover; got %+v", got)
	}
}

// TestCheckPlanCoverage_FullMiss confirms the under-cover failure
// fires when none of the planner's named files appear in the diff.
// This is the round-12 dogfood shape this guard exists to catch.
func TestCheckPlanCoverage_FullMiss(t *testing.T) {
	task := mustTask([]string{
		"kai/A.go", "kai/B.go", "kai/C.go", "kai/D.go",
	})
	changed := []string{"kai/unrelated.go"}
	got := checkPlanCoverage(task, changed)
	if got.Matched != 0 || got.TotalSignals != 4 {
		t.Errorf("full miss: got %d/%d; expected 0/4", got.Matched, got.TotalSignals)
	}
	if !got.UnderCovered() {
		t.Errorf("0/4 must be under-covered (4 signals, 0 matched)")
	}
	if len(got.MissingFiles) != 4 {
		t.Errorf("missing files count: got %d, want 4", len(got.MissingFiles))
	}
}

// TestCheckPlanCoverage_ExemplarFilesNotCounted is the v0.31.8 fix
// for the exemplar-vs-target conflation. Old behavior pulled any
// file mentioned in the prompt prose ("matches the pattern in
// repos.go and webhooks.go") into the signal list, producing
// false-positive holds when the agent legitimately only needed
// to edit the target file. New behavior: only task.Files counts.
func TestCheckPlanCoverage_ExemplarFilesNotCounted(t *testing.T) {
	task := mustTask([]string{
		"kai-server/kailab-control/internal/api/search.go", // only target
	})
	task.Prompt = "Add the search handler. See how repos.go and webhooks.go " +
		"register routes — match that pattern. The new handler follows " +
		"the same shape as llm_completions.go's WithAuth wrapper."
	changed := []string{
		"kai-server/kailab-control/internal/api/search.go",
	}
	got := checkPlanCoverage(task, changed)
	if got.TotalSignals != 1 || got.Matched != 1 {
		t.Errorf("only task.Files should count as signal; got %d/%d with missing=%v",
			got.Matched, got.TotalSignals, got.MissingFiles)
	}
	if got.UnderCovered() {
		t.Errorf("legitimate single-target edit should not under-cover; got reason=%q", got.Reason())
	}
}

func mustTask(files []string) planner.AgentTask {
	return planner.AgentTask{
		Name:  "test",
		Files: files,
	}
}
