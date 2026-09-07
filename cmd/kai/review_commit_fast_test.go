package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/provider"
)

// A fast pass read no callers, so it may not clear a change. The prompt says
// so and this is the enforcement; the test fails on code that trusts the
// prompt alone.
func TestCapFastReadinessBlocksTheGreenCheck(t *testing.T) {
	if got := rcCapFastReadiness(finding.ReadinessMerge); got != finding.ReadinessDecideThenMerge {
		t.Errorf("readiness 5 from a fast pass = %v, want %v (a diff-only pass cannot say 'merge it')",
			got, finding.ReadinessDecideThenMerge)
	}
	// Everything else passes through untouched — including Unknown, which is
	// "the reviewer did not say" and must not be invented into a score.
	for _, r := range []finding.Readiness{
		finding.ReadinessUnknown,
		finding.ReadinessBlocked,
		finding.ReadinessNeedsWork,
		finding.ReadinessSmallFixes,
		finding.ReadinessDecideThenMerge,
	} {
		if got := rcCapFastReadiness(r); got != r {
			t.Errorf("rcCapFastReadiness(%v) = %v, want it unchanged", r, got)
		}
	}
}

// The four bullets below are verbatim from the shapes the first live fast run
// produced on kai-desktop 94dbbe8 — the model narrating its uncertainty into
// the defect list, where each one would have become a risk-tagged claim and
// flipped the PR badge to "review before merging".
func TestFilterFastIssuesDropsHedges(t *testing.T) {
	in := []string{
		"panel-changes.js:2200 — box.removeChild(body) throws when body is null on the deferred path",
		"panel-changes.js:2123 — this appears intentional but worth confirming the budget-exhaustion behavior",
		"panel-changes.js:1644 — the caching could diverge, assuming the tree is mutated elsewhere",
		"panel-changes.js:592 — lastFindingsJSON is never cleared on workspace switch",
	}
	got := rcFilterFastIssues(in)
	if len(got) != 2 {
		t.Fatalf("kept %d issues %q, want the 2 unhedged ones", len(got), got)
	}
	for _, g := range got {
		if h, hedged := rcHedgePhrase(g); hedged {
			t.Errorf("kept a hedged issue (%q): %s", h, g)
		}
	}
	// Order is preserved, so the cap below keeps the most confident bullets.
	if !strings.Contains(got[0], "removeChild") || !strings.Contains(got[1], "lastFindingsJSON") {
		t.Errorf("filter reordered the issues: %q", got)
	}
}

// Hedges are dropped BEFORE the count is capped. Capping first would keep
// three maybes and discard the certainty sitting behind them.
func TestFilterFastIssuesCapsAfterDroppingHedges(t *testing.T) {
	in := []string{
		"a.go:1 — seems correct but check it",
		"b.go:2 — it appears to be fine",
		"c.go:3 — worth confirming this one",
		"d.go:4 — the mutex is never unlocked on the error path",
		"e.go:5 — the caller in this diff was not updated",
		"f.go:6 — nil dereference on the empty slice",
		"g.go:7 — the ticker is never stopped",
	}
	got := rcFilterFastIssues(in)
	if len(got) != rcFastMaxIssues {
		t.Fatalf("kept %d issues, want the %d cap: %q", len(got), rcFastMaxIssues, got)
	}
	if !strings.Contains(got[0], "mutex") {
		t.Errorf("cap kept the hedges instead of the real defects: %q", got)
	}
	for _, g := range got {
		if h, hedged := rcHedgePhrase(g); hedged {
			t.Errorf("kept a hedged issue (%q): %s", h, g)
		}
	}
}

// "could" and "may" are deliberately NOT hedge markers: they appear in real
// defects as often as in guesses, and dropping them would silently eat the
// findings the fast pass exists to surface.
func TestFilterFastIssuesKeepsAssertiveBullets(t *testing.T) {
	in := []string{
		"a.go:1 — this could panic when the map is nil",
		"b.go:2 — a large rebuild here may stall the UI thread",
	}
	if got := rcFilterFastIssues(in); len(got) != 2 {
		t.Errorf("dropped an assertive bullet: kept %q, want both", got)
	}
}

// A reasoning model's hidden chain-of-thought is what blew the first fast run's
// budget (glm-5.2: 3m58s on one call). Selection must swap it out, and must
// honor an explicit override.
func TestFastModelAvoidsReasoningModels(t *testing.T) {
	t.Setenv("KAI_FAST_MODEL", "")
	os.Unsetenv("KAI_FAST_MODEL")
	if got := rcFastModel("z-ai/glm-5.2", provider.KindKailab); got != rcFastDefaultModel {
		t.Errorf("rcFastModel(glm-5.2) = %q, want the non-reasoning default %q", got, rcFastDefaultModel)
	}
	if got := rcFastModel("anthropic/claude-haiku-4-5", provider.KindKailab); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("rcFastModel kept-model = %q, want it unchanged", got)
	}
	t.Setenv("KAI_FAST_MODEL", "some/other-model")
	if got := rcFastModel("z-ai/glm-5.2", provider.KindKailab); got != "some/other-model" {
		t.Errorf("KAI_FAST_MODEL override = %q, want it to win outright", got)
	}
}

// KindOpenAI is every OpenAI-COMPATIBLE endpoint — Together, Groq, Ollama,
// vLLM, LM Studio — and each serves its own model namespace. Substituting an
// "anthropic/..." id there fails the request, and since the fast pass is the
// DEFAULT review that is no review at all rather than a slow one.
func TestFastModelDoesNotSubstituteOnForeignProviders(t *testing.T) {
	t.Setenv("KAI_FAST_MODEL", "")
	os.Unsetenv("KAI_FAST_MODEL")
	for _, kind := range []provider.Kind{provider.KindOpenAI, provider.KindAnthropic} {
		if got := rcFastModel("z-ai/glm-5.2", kind); got != "z-ai/glm-5.2" {
			t.Errorf("rcFastModel(glm-5.2, %s) = %q, want the configured model kept — %q is not in that provider's namespace",
				kind, got, rcFastDefaultModel)
		}
	}
	// OpenRouter serves the namespaced id, so the swap is legal there.
	if got := rcFastModel("z-ai/glm-5.2", provider.KindOpenRouter); got != rcFastDefaultModel {
		t.Errorf("rcFastModel(glm-5.2, openrouter) = %q, want %q", got, rcFastDefaultModel)
	}
	// An explicit override still wins everywhere: the user named the model.
	t.Setenv("KAI_FAST_MODEL", "local/fast")
	if got := rcFastModel("z-ai/glm-5.2", provider.KindOpenAI); got != "local/fast" {
		t.Errorf("KAI_FAST_MODEL on a foreign provider = %q, want it to win", got)
	}
}

// The identifier lookups must grep the same tree the diff was taken from. A
// run from a subdirectory of an uncaptured repo would otherwise scope the grep
// to that subtree while reviewing the whole change.
func TestWorktreeRootNormalizesASubdirectory(t *testing.T) {
	// `go test` runs in the package directory, so "." is cmd/kai — itself a
	// subdirectory of the worktree — and ".." is cmd. Both must resolve to
	// the same root, and neither may resolve to itself.
	here := rcWorktreeRoot(".")
	up := rcWorktreeRoot("..")
	if here == "" || up == "" {
		t.Fatal("rcWorktreeRoot returned empty")
	}
	if here != up {
		t.Errorf("rcWorktreeRoot(\".\") = %q but rcWorktreeRoot(\"..\") = %q; both are inside one worktree", here, up)
	}
	if here == "." {
		t.Error("rcWorktreeRoot fell back to the cwd inside a real repo — the lookups would grep a subtree of the diff")
	}
	if !filepath.IsAbs(here) {
		t.Errorf("rcWorktreeRoot = %q, want the absolute worktree root", here)
	}
	// Not a repo (and no parent repo): fall back to the directory itself.
	tmp := t.TempDir()
	if got := rcWorktreeRoot(tmp); got != tmp && !strings.HasSuffix(got, tmp) {
		t.Logf("rcWorktreeRoot(%q) = %q — acceptable if the temp dir sits inside a repo", tmp, got)
	}
}

func TestFastBudgetOverride(t *testing.T) {
	t.Setenv("KAI_FAST_BUDGET", "45s")
	if got := rcFastBudget(); got.String() != "45s" {
		t.Errorf("KAI_FAST_BUDGET=45s → %v, want 45s", got)
	}
	t.Setenv("KAI_FAST_BUDGET", "not-a-duration")
	if got := rcFastBudget(); got != 100*1000*1000*1000 {
		t.Errorf("unparseable budget → %v, want the 100s default", got)
	}
}

// The fast prompt is a different contract, not a shortened one: it must forbid
// the green check and cap its own score, because a shallow pass that renders
// like a grounded one is worse than no pass at all.
func TestFastPromptForbidsClearingAChange(t *testing.T) {
	for _, want := range []string{
		"NEVER GREEN-CHECK",
		"CANNOT be 5",
		"AT MOST THREE ISSUES",
		rcReviewDataMarker,
	} {
		if !strings.Contains(rcFastReviewSystem, want) {
			t.Errorf("fast prompt is missing %q", want)
		}
	}
	// The coda must stay parseable by the SAME parser the grounded path uses;
	// a fast finding that cannot be read is not a faster finding.
	if !strings.Contains(rcFastReviewSystem, "MERGE_READY: 1|2|3|4") {
		t.Error("fast prompt must offer 1-4 only, not the grounded 1-5 scale")
	}
}

// The fast pass is the DEFAULT review; --deep opts into the grounded one.
// These pin the flag surface, because the default is the whole behaviour
// change and a silent flip back would be invisible in every other test.
func TestReviewCommitFlagDefaults(t *testing.T) {
	fastFlag := reviewCommitCmd.Flags().Lookup("fast")
	deepFlag := reviewCommitCmd.Flags().Lookup("deep")
	if fastFlag == nil || deepFlag == nil {
		t.Fatal("review-commit must offer both --fast and --deep")
	}
	// Both default to false: neither flag given means the fast pass runs,
	// which is what `fast := !reviewCommitDeep` expresses.
	if fastFlag.DefValue != "false" || deepFlag.DefValue != "false" {
		t.Errorf("flag defaults = fast:%s deep:%s, want both false", fastFlag.DefValue, deepFlag.DefValue)
	}
	// --fast survives as the explicit spelling of the default: CI pods and
	// the built-in review workflow pass it, and removing it would break them
	// on the next image bump for no gain.
	if !strings.Contains(reviewCommitCmd.Long, "--deep is the grounded review") {
		t.Error("help must say what --deep buys; it is no longer the default")
	}
	if !strings.Contains(reviewCommitCmd.Long, "DEFAULT: the fast pass") {
		t.Error("help must state that the fast pass is the default")
	}
}

// Asking for both depths is a contradiction, and picking one silently is how
// a CI job reviews at the wrong depth for a month without anyone noticing.
func TestReviewCommitRejectsBothDepths(t *testing.T) {
	reviewCommitFast, reviewCommitDeep = true, true
	defer func() { reviewCommitFast, reviewCommitDeep = false, false }()
	err := runReviewCommit(reviewCommitCmd, []string{"HEAD"})
	if err == nil || !strings.Contains(err.Error(), "opposites") {
		t.Errorf("--fast --deep together = %v, want a refusal naming the contradiction", err)
	}
}
