package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// Every stage of the local profile is a Claude model the direct provider
// serves, or off: nothing in it needs a vendor an Anthropic key cannot reach.
func TestLocalProfileIsClaudeOnly(t *testing.T) {
	p := rcLocalProfile()
	for _, stage := range rcProfileStages {
		s, ok := p.Stages[stage]
		if !ok {
			t.Errorf("stage %s is not set: it would fall back to a server default", stage)
			continue
		}
		if s.Model == rcEffortOff {
			continue
		}
		if _, err := provider.AnthropicLocalReviewModel(s.Model); err != nil {
			t.Errorf("stage %s: %v", stage, err)
		}
		if s.Effort != "" {
			t.Errorf("stage %s sets effort %q; a bare claude- id carries none", stage, s.Effort)
		}
	}
	old := rcActiveProfile
	rcActiveProfile = p
	defer func() { rcActiveProfile = old }()
	for _, stage := range []string{rcStageFinder2, rcStageSweep2, rcStageRank, rcStageHumanize, rcStageSpecLogic, rcStageSpecSecurity, rcStageSpecContracts} {
		if m := rcEnsembleModel(stage); m != "" {
			t.Errorf("ensemble stage %s resolves to %q on a local review, want off", stage, m)
		}
	}
	if got := rcFastModel("z-ai/glm-5.2", provider.KindAnthropicLocalReview); got != rcLocalFastModel {
		t.Errorf("fast draft model = %q, want %q", got, rcLocalFastModel)
	}
	if got := rcFastChallengeModel("z-ai/glm-5.2"); got != rcLocalFastModel {
		t.Errorf("fast fact-check model = %q, want %q", got, rcLocalFastModel)
	}
}

type rcLocalFakeProvider struct {
	costUSD float64
	asked   []string
}

func (f *rcLocalFakeProvider) Send(_ context.Context, req provider.Request) (provider.Response, error) {
	f.asked = append(f.asked, req.Model)
	return provider.Response{
		Parts:            []message.ContentPart{message.TextContent{Text: rcTestReview()}},
		EstimatedCostUSD: f.costUSD,
	}, nil
}

func rcLocalReq(model string) provider.Request {
	return provider.Request{Model: model, MaxTokens: 10}
}

// The meter is the one place a local review's money goes through: it refuses
// a model the run did not start with (a dearer Claude model included), adds
// up what answers cost, and stops at the cap.
func TestLocalMeter(t *testing.T) {
	fake := &rcLocalFakeProvider{costUSD: 0.30}
	m := &rcLocalMeter{inner: fake, allowed: map[string]bool{"claude-haiku-4-5": true}, capUSD: 0.50}
	ctx := context.Background()

	for _, other := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "z-ai/glm-5.2", "claude-mystery-9000"} {
		if _, err := m.Send(ctx, rcLocalReq(other)); err == nil {
			t.Errorf("a call to %s went through", other)
		}
	}
	if len(fake.asked) != 0 {
		t.Fatalf("refused models reached the provider: %v", fake.asked)
	}
	// The OpenRouter spelling of the same model is the same model.
	if _, err := m.Send(ctx, rcLocalReq("anthropic/claude-haiku-4.5")); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := m.Send(ctx, rcLocalReq("claude-haiku-4-5")); err != nil {
		t.Fatalf("second call (spent 0.30 of 0.50): %v", err)
	}
	_, err := m.Send(ctx, rcLocalReq("claude-haiku-4-5"))
	if err == nil || !strings.Contains(err.Error(), "the cap for this run") {
		t.Fatalf("third call (spent 0.60 of 0.50) = %v, want a stop at the cap", err)
	}
	if spent, calls := m.Spent(); calls != 2 || spent < 0.59 || spent > 0.61 {
		t.Errorf("spent $%.2f over %d calls, want $0.60 over 2", spent, calls)
	}
}

// The key comes from ANTHROPIC_API_KEY only. A subscription sign-in is
// refused wherever it is put, and the variables that hold one are not read.
func TestLocalProviderKey(t *testing.T) {
	old := rcLocalNewProvider
	built := ""
	rcLocalNewProvider = func(key string) (provider.Provider, error) {
		built = key
		return provider.NewAnthropicLocalReview(key)
	}
	defer func() { rcLocalNewProvider = old }()
	models := []string{rcLocalFastModel}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-oat01-plan")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-plan")
	if _, err := rcLocalProvider(models, 1); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("no API key, a plan token in the other variables: %v", err)
	}
	if built != "" {
		t.Errorf("a provider was built from %q", built)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-oat01-plan")
	if _, err := rcLocalProvider(models, 1); err == nil || !strings.Contains(err.Error(), "subscription sign-in token") {
		t.Errorf("a plan token as the API key: %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-ok")
	if _, err := rcLocalProvider(models, 1); err != nil || built != "sk-ant-api03-ok" {
		t.Errorf("an API key: err=%v built=%q", err, built)
	}
	if _, err := rcLocalProvider([]string{"z-ai/glm-5.2"}, 1); err == nil {
		t.Error("a non-Claude model was accepted for a local run")
	}
}

// The estimate is shown first; a deep review needs a yes; an estimate over
// the cap does not start.
func TestLocalConfirm(t *testing.T) {
	oldTTY, oldYes := rcStdinIsTerminal, reviewCommitYes
	rcStdinIsTerminal = func() bool { return false }
	defer func() { rcStdinIsTerminal, reviewCommitYes = oldTTY, oldYes }()

	if err := rcLocalConfirm(false, 0.04, 0.50, []string{rcLocalFastModel}); err != nil {
		t.Errorf("fast review under its cap: %v", err)
	}
	if err := rcLocalConfirm(false, 0.80, 0.50, []string{rcLocalFastModel}); err == nil || !strings.Contains(err.Error(), "over this run's cap") {
		t.Errorf("estimate over the cap: %v", err)
	}
	reviewCommitYes = false
	if err := rcLocalConfirm(true, 1.20, 5, []string{rcLocalDeepModel}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("deep review, nobody to ask, no --yes: %v", err)
	}
	reviewCommitYes = true
	if err := rcLocalConfirm(true, 1.20, 5, []string{rcLocalDeepModel}); err != nil {
		t.Errorf("deep review with --yes: %v", err)
	}
}

func TestLocalEstimateAndCap(t *testing.T) {
	fast := rcLocalEstimate(false, 30_000, 5)
	deep := rcLocalEstimate(true, 30_000, 5)
	if fast <= 0 || deep <= fast {
		t.Errorf("estimates fast=$%.3f deep=$%.3f: both must be positive and deep the larger", fast, deep)
	}
	if fast > rcLocalFastCapUSD {
		t.Errorf("an ordinary 30KB diff estimates $%.2f fast, over the default cap", fast)
	}
	old := reviewCommitMaxCost
	defer func() { reviewCommitMaxCost = old }()
	reviewCommitMaxCost = 0
	if rcLocalCap(false) != rcLocalFastCapUSD || rcLocalCap(true) != rcLocalDeepCapUSD {
		t.Error("default caps")
	}
	reviewCommitMaxCost = 1.25
	if rcLocalCap(false) != 1.25 || rcLocalCap(true) != 1.25 {
		t.Error("--max-cost did not set the cap")
	}
}

func rcLocalGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// rcLocalRepo is a clone with one pushed commit on main, one unpushed commit,
// an uncommitted edit, an untracked file and an ignored one.
func rcLocalRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	dir := filepath.Join(root, "work")
	rcLocalGit(t, root, "init", "-q", "--bare", origin)
	rcLocalGit(t, root, "init", "-q", "-b", "main", dir)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	write(".gitignore", "secret.env\n")
	rcLocalGit(t, dir, "add", "-A")
	rcLocalGit(t, dir, "commit", "-qm", "Add calc")
	rcLocalGit(t, dir, "remote", "add", "origin", origin)
	rcLocalGit(t, dir, "push", "-q", "origin", "main")
	write("calc.go", "package calc\n\nfunc Add(a, b int) int { return a - b }\n")
	rcLocalGit(t, dir, "commit", "-qam", "Rework Add")
	write("calc.go", "package calc\n\nfunc Add(a, b int) int { return a - b - 1 }\n")
	write("total.go", "package calc\n\nfunc Total() int { return 0 }\n")
	write("secret.env", "TOKEN=1\n")
	return dir
}

// With no commit named, the review covers unpushed commits and uncommitted
// work, including new files, without touching the index, the branch or the
// stash; ignored files stay out.
func TestLocalTargetIsTheWorkingTree(t *testing.T) {
	dir := rcLocalRepo(t)
	t.Chdir(dir)
	headBefore := rcLocalGit(t, dir, "rev-parse", "HEAD")
	statusBefore := rcLocalGit(t, dir, "status", "--porcelain")

	ref, base, made, err := rcLocalTarget(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if base != "origin/main" || !made {
		t.Errorf("base=%q made=%v, want origin/main and a working-tree commit", base, made)
	}
	diff := rcLocalGit(t, dir, "diff", base+"..."+ref)
	for _, want := range []string{"return a - b - 1", "func Total()"} {
		if !strings.Contains(diff, want) {
			t.Errorf("reviewed diff is missing %q", want)
		}
	}
	if strings.Contains(diff, "TOKEN=1") {
		t.Error("an ignored file is in the reviewed diff")
	}
	if msg := rcLocalGit(t, dir, "log", "-1", "--format=%B", ref); !strings.Contains(msg, "Rework Add") {
		t.Errorf("the commit message does not name the unpushed commit:\n%s", msg)
	}
	if got := rcLocalGit(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Error("HEAD moved")
	}
	if got := rcLocalGit(t, dir, "status", "--porcelain"); got != statusBefore {
		t.Errorf("the working tree or index changed:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}
	if got := rcLocalGit(t, dir, "stash", "list"); got != "" {
		t.Errorf("the stash was used: %s", got)
	}

	// A clean tree reviews HEAD itself; a named commit is taken as given.
	rcLocalGit(t, dir, "add", "-A")
	rcLocalGit(t, dir, "commit", "-qm", "Finish")
	head := rcLocalGit(t, dir, "rev-parse", "HEAD")
	if ref, _, made, err := rcLocalTarget(nil, ""); err != nil || made || ref != head {
		t.Errorf("clean tree: ref=%q made=%v err=%v, want HEAD", ref, made, err)
	}
	if ref, base, made, err := rcLocalTarget([]string{"HEAD~1"}, ""); err != nil || made || ref != "HEAD~1" || base != "" {
		t.Errorf("named commit: ref=%q base=%q made=%v err=%v", ref, base, made, err)
	}
}

// One local fast review, end to end, on a stand-in for the API: it runs on
// the local profile's model, calls nothing else, and the bundle says what it
// was allowed and what it spent.
func TestLocalReviewEndToEnd(t *testing.T) {
	dir := rcLocalRepo(t)
	t.Chdir(dir)

	fake := &rcLocalFakeProvider{costUSD: 0.02}
	oldNew := rcLocalNewProvider
	rcLocalNewProvider = func(string) (provider.Provider, error) { return fake, nil }
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-test")
	// A job's settings for some other review must not reach this one.
	t.Setenv("KAI_REVIEW_MODEL", "z-ai/glm-5.2")
	t.Setenv("KAI_CHALLENGE_MODEL", "openai/gpt-5.6-sol")
	t.Setenv("KAI_FAST_MODEL", "z-ai/glm-5.2")
	t.Setenv("KAI_PROVIDER", "openrouter")

	oldKaiDir, oldFormat, oldBase, oldProfile := kaiDir, reviewCommitFormat, reviewCommitBase, rcActiveProfile
	kaiDir, reviewCommitFormat, reviewCommitLocal = t.TempDir(), "json", true
	var stdout bytes.Buffer
	reviewCommitCmd.SetOut(&stdout)
	t.Cleanup(func() {
		rcLocalNewProvider = oldNew
		kaiDir, reviewCommitFormat, reviewCommitBase, rcActiveProfile = oldKaiDir, oldFormat, oldBase, oldProfile
		reviewCommitLocal, rcLocalRun = false, nil
		reviewCommitCmd.SetOut(nil)
	})

	if err := runReviewCommit(reviewCommitCmd, nil); err != nil {
		t.Fatalf("local review: %v", err)
	}
	for _, m := range fake.asked {
		if m != rcLocalFastModel {
			t.Errorf("the review called %s", m)
		}
	}
	if len(fake.asked) == 0 {
		t.Fatal("the review made no model call")
	}
	var bundle struct {
		Review      string         `json:"review"`
		Depth       string         `json:"depth"`
		Description string         `json:"description"`
		Local       *rcLocalReport `json:"local"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &bundle); err != nil {
		t.Fatalf("bundle: %v\n%s", err, stdout.String())
	}
	l := bundle.Local
	if l == nil {
		t.Fatalf("bundle has no local object:\n%s", stdout.String())
	}
	if l.Provider != string(provider.KindAnthropicLocalReview) || len(l.Models) != 1 || l.Models[0] != rcLocalFastModel {
		t.Errorf("local.provider=%q models=%v", l.Provider, l.Models)
	}
	if l.Calls != len(fake.asked) || l.SpentUSD <= 0 || l.CapUSD != rcLocalFastCapUSD {
		t.Errorf("local.calls=%d spent=%v cap=%v with %d calls made", l.Calls, l.SpentUSD, l.CapUSD, len(fake.asked))
	}
	if l.Base != "origin/main" || !l.WorkingTree {
		t.Errorf("local.base=%q workingTree=%v", l.Base, l.WorkingTree)
	}
	if bundle.Depth != "fast" || strings.TrimSpace(bundle.Review) == "" {
		t.Errorf("depth=%q review=%q", bundle.Depth, bundle.Review)
	}
	if bundle.Description != "" {
		t.Error("a local review asked for a pull request description")
	}
}

// Without --local nothing about the command changes: it still wants exactly
// one commit, and the local-only flags are refused.
func TestLocalFlagsNeedLocal(t *testing.T) {
	if err := rcReviewCommitArgs(reviewCommitCmd, nil); err == nil {
		t.Error("review-commit with no commit and no --local was accepted")
	}
	reviewCommitLocal = true
	if err := rcReviewCommitArgs(reviewCommitCmd, nil); err != nil {
		t.Errorf("--local with no commit: %v", err)
	}
	if err := rcReviewCommitArgs(reviewCommitCmd, []string{"a", "b"}); err == nil {
		t.Error("--local with two commits was accepted")
	}
	reviewCommitLocal = false
	reviewCommitYes = true
	defer func() { reviewCommitYes = false }()
	err := runReviewCommit(reviewCommitCmd, []string{"HEAD"})
	if err == nil || !strings.Contains(err.Error(), "add --local") {
		t.Errorf("--yes without --local = %v", err)
	}
	var none *rcLocalState
	if none.report() != nil {
		t.Error("a review that is not local reported a local object")
	}
}
