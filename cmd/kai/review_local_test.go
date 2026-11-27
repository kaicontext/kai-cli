package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
		if s.Effort != rcEffortOff {
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

// Calls that overlap cannot each be admitted against money none of them has
// been billed for yet: a call in flight holds its ceiling against the cap.
func TestLocalMeterCapHoldsUnderConcurrency(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	slow := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		started <- struct{}{}
		<-release
		return provider.Response{EstimatedCostUSD: 0.05}, nil
	}}
	// One call's ceiling: Haiku 4.5 output is $5 per million tokens, so
	// 20,000 allowed tokens is $0.10.
	req := provider.Request{Model: "claude-haiku-4-5", MaxTokens: 20_000}
	m := &rcLocalMeter{backend: rcLocalBackends[0], inner: slow, allowed: map[string]bool{"claude-haiku-4-5": true}, capUSD: 0.25}
	done := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() { _, err := m.Send(context.Background(), req); done <- err }()
		<-started
	}
	// Three in flight have committed about $0.30 of a $0.25 cap, and nothing
	// has been billed. A fourth must not start.
	fourth := make(chan error, 1)
	go func() { _, err := m.Send(context.Background(), req); fourth <- err }()
	select {
	case err := <-fourth:
		if err == nil || !strings.Contains(err.Error(), "the cap for this run") {
			t.Fatalf("fourth overlapping call = %v, want a stop at the cap", err)
		}
	case <-started:
		t.Fatal("a fourth overlapping call reached the provider past the cap")
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatalf("an admitted call failed: %v", err)
		}
	}
	// Settled at what the answers cost ($0.15), the reservations are gone
	// and the run has room again.
	if spent, calls := m.Spent(); calls != 3 || spent < 0.149 || spent > 0.151 {
		t.Errorf("spent $%.3f over %d calls, want $0.150 over 3", spent, calls)
	}
	if m.reservedUSD != 0 {
		t.Errorf("reservations left after every call settled: $%.3f", m.reservedUSD)
	}
	if _, err := m.Send(context.Background(), req); err != nil {
		t.Errorf("a call after the others settled: %v", err)
	}
}

// A call that fails gives its reservation back and is not counted.
func TestLocalMeterFailedCallReleasesItsReservation(t *testing.T) {
	failing := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{}, context.DeadlineExceeded
	}}
	m := &rcLocalMeter{backend: rcLocalBackends[0], inner: failing, allowed: map[string]bool{"claude-haiku-4-5": true}, capUSD: 0.25}
	for i := 0; i < 5; i++ {
		if _, err := m.Send(context.Background(), provider.Request{Model: "claude-haiku-4-5", MaxTokens: 20_000}); err != context.DeadlineExceeded {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if spent, calls := m.Spent(); spent != 0 || calls != 0 || m.reservedUSD != 0 {
		t.Errorf("after five failed calls: spent=%v calls=%d reserved=%v", spent, calls, m.reservedUSD)
	}
}

func rcLocalReq(model string) provider.Request {
	return provider.Request{Model: model, MaxTokens: 10}
}

// The meter is the one place a local review's money goes through: it refuses
// a model the run did not start with (a dearer Claude model included), adds
// up what answers cost, and stops at the cap.
func TestLocalMeter(t *testing.T) {
	fake := &rcLocalFakeProvider{costUSD: 0.30}
	m := &rcLocalMeter{backend: rcLocalBackends[0], inner: fake, allowed: map[string]bool{"claude-haiku-4-5": true}, capUSD: 0.50}
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

// rcLocalUse makes b the active backend for one test.
func rcLocalUse(t *testing.T, b *rcLocalBackend) {
	t.Helper()
	old := rcLocalActive
	rcLocalActive = b
	t.Cleanup(func() { rcLocalActive = old })
}

func rcLocalBackendNamed(t *testing.T, name string) *rcLocalBackend {
	t.Helper()
	b, _, err := rcLocalPick(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// With no --local-provider the key that is set decides, Anthropic first, and
// the keys passed over are reported. A name wins over what is set.
func TestLocalPick(t *testing.T) {
	for _, b := range rcLocalBackends {
		t.Setenv(b.keyEnv, "")
	}
	if b, also, err := rcLocalPick(""); err != nil || b.name != "anthropic" || len(also) != 0 {
		t.Errorf("no key set: %v %v %v", b, also, err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-proj-x")
	if b, also, _ := rcLocalPick(""); b.name != "openai" || len(also) != 0 {
		t.Errorf("only an OpenAI key: picked %s, also %v", b.name, also)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-x")
	if b, also, _ := rcLocalPick(""); b.name != "openrouter" || len(also) != 1 || also[0] != "OPENAI_API_KEY" {
		t.Errorf("OpenRouter and OpenAI keys: picked %s, also %v", b.name, also)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-x")
	if b, also, _ := rcLocalPick(""); b.name != "anthropic" || len(also) != 2 {
		t.Errorf("all three keys: picked %s, also %v", b.name, also)
	}
	if b, also, _ := rcLocalPick(" OpenAI "); b.name != "openai" || len(also) != 0 {
		t.Errorf("--local-provider openai: picked %s, also %v", b.name, also)
	}
	if _, _, err := rcLocalPick("gemini"); err == nil || !strings.Contains(err.Error(), "anthropic, openrouter, openai") {
		t.Errorf("an unknown provider: %v", err)
	}
}

// Every backend's two models resolve and are priced by that backend, its
// profile names nothing else, and its estimate fits the default caps.
func TestLocalBackendsAreSelfConsistent(t *testing.T) {
	for _, b := range rcLocalBackends {
		rcLocalUse(t, b)
		for _, m := range []string{b.fast, b.deep} {
			id, err := b.model(m)
			if err != nil {
				t.Errorf("%s: model %s: %v", b.name, m, err)
				continue
			}
			if c, err := b.cost(id, 1000, 1000); err != nil || c <= 0 {
				t.Errorf("%s: %s has no price: %v %v", b.name, id, c, err)
			}
		}
		got := rcLocalModels(rcLocalProfile())
		want := []string{b.deep, b.fast}
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: profile models %v, want %v", b.name, got, want)
		}
		fast, deep := rcLocalEstimate(false, 30_000, 5), rcLocalEstimate(true, 30_000, 5)
		if fast <= 0 || fast > rcLocalFastCapUSD || deep <= fast || deep > rcLocalDeepCapUSD {
			t.Errorf("%s: estimates fast=$%.3f deep=$%.3f against caps $%.2f / $%.2f", b.name, fast, deep, rcLocalFastCapUSD, rcLocalDeepCapUSD)
		}
	}
}

// Each backend reads its own variable, refuses a sign-in token and a key
// from another vendor, and its meter refuses the other backends' models.
func TestLocalBackendKeys(t *testing.T) {
	for _, b := range rcLocalBackends {
		t.Setenv(b.keyEnv, "")
	}
	// A ChatGPT sign-in is a JWT.
	openai := rcLocalBackendNamed(t, "openai")
	rcLocalUse(t, openai)
	if _, err := rcLocalProvider([]string{openai.fast}, 1); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("no key: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "eyJhbGciOiJSUzI1NiJ9.x.y")
	if _, err := rcLocalProvider([]string{openai.fast}, 1); err == nil || !strings.Contains(err.Error(), "sign-in token") {
		t.Errorf("a sign-in token as the OpenAI key: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-ant-api03-x")
	if _, err := rcLocalProvider([]string{openai.fast}, 1); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("an Anthropic key as the OpenAI key: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-proj-ok")
	m, err := rcLocalProvider([]string{openai.fast}, 1)
	if err != nil {
		t.Fatalf("an OpenAI key: %v", err)
	}
	if m.SupportsCache() || m.SupportsCacheForModel(openai.fast) {
		t.Error("the GPT models were reported as taking cache breakpoints")
	}
	for _, other := range []string{"claude-haiku-4-5", "gpt-5.4", "gpt-6.2-sol"} {
		if _, err := m.admit(rcLocalReq(other)); err == nil {
			t.Errorf("a run started on %s admitted %s", openai.fast, other)
		}
	}
	if _, err := rcLocalProvider([]string{"claude-haiku-4-5"}, 1); err == nil {
		t.Error("a Claude model was accepted on an OpenAI key")
	}

	or := rcLocalBackendNamed(t, "openrouter")
	rcLocalUse(t, or)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-ok")
	m, err = rcLocalProvider([]string{or.fast}, 1)
	if err != nil {
		t.Fatalf("an OpenRouter key: %v", err)
	}
	if !m.SupportsCacheForModel(or.fast) {
		t.Error("Claude through OpenRouter was reported as cacheless")
	}
	// The bare id and the OpenRouter id are the same model.
	if _, err := m.admit(rcLocalReq("claude-haiku-4-5")); err != nil {
		t.Errorf("the bare spelling of the run's model was refused: %v", err)
	}
	if _, err := m.admit(rcLocalReq("openai/gpt-5.4")); err == nil {
		t.Error("a GPT model was admitted on the OpenRouter backend")
	}
}

// A fast review on an OpenAI key, end to end on a stand-in: it calls GPT-5.4
// mini only and the bundle names the OpenAI provider.
func TestLocalReviewEndToEndOpenAI(t *testing.T) {
	dir := rcLocalRepo(t)
	t.Chdir(dir)

	fake := &rcLocalFakeProvider{costUSD: 0.02}
	oldNew := rcLocalNewProvider
	rcLocalNewProvider = func(string) (provider.Provider, error) { return fake, nil }
	// An Anthropic key is set too: the flag decides.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-test")
	t.Setenv("OPENAI_API_KEY", "sk-proj-test")
	t.Setenv("OPENROUTER_API_KEY", "")

	oldKaiDir, oldFormat, oldBase, oldProfile, oldActive := kaiDir, reviewCommitFormat, reviewCommitBase, rcActiveProfile, rcLocalActive
	kaiDir, reviewCommitFormat, reviewCommitLocal, reviewCommitLocalProvider = t.TempDir(), "json", true, "openai"
	var stdout bytes.Buffer
	reviewCommitCmd.SetOut(&stdout)
	t.Cleanup(func() {
		rcLocalNewProvider = oldNew
		kaiDir, reviewCommitFormat, reviewCommitBase, rcActiveProfile, rcLocalActive = oldKaiDir, oldFormat, oldBase, oldProfile, oldActive
		reviewCommitLocal, reviewCommitLocalProvider, rcLocalRun = false, "", nil
		reviewCommitCmd.SetOut(nil)
	})

	if err := runReviewCommit(reviewCommitCmd, nil); err != nil {
		t.Fatalf("local review: %v", err)
	}
	if len(fake.asked) == 0 {
		t.Fatal("the review made no model call")
	}
	for _, m := range fake.asked {
		if m != "gpt-5.4-mini" {
			t.Errorf("the review called %s", m)
		}
	}
	var bundle struct {
		Local *rcLocalReport `json:"local"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &bundle); err != nil {
		t.Fatalf("bundle: %v\n%s", err, stdout.String())
	}
	if l := bundle.Local; l == nil || l.Provider != string(provider.KindOpenAILocalReview) || len(l.Models) != 1 || l.Models[0] != "gpt-5.4-mini" {
		t.Errorf("local = %+v", bundle.Local)
	}
}

// An effort left in the shell does not reach a local review: every kept stage
// names its own. Only the OpenAI backend's grounded stages ask for one.
func TestLocalProfilePinsEffort(t *testing.T) {
	t.Setenv("KAI_REVIEW_REASONING_EFFORT", "high")
	oldProfile := rcActiveProfile
	t.Cleanup(func() { rcActiveProfile = oldProfile })
	for _, b := range rcLocalBackends {
		rcLocalUse(t, b)
		rcActiveProfile = rcLocalProfile()
		for _, stage := range []string{rcStageQuickDraft, rcStageQuickFactcheck} {
			if e := rcStageEffort(stage); e != "" {
				t.Errorf("%s: fast stage %s runs at effort %q", b.name, stage, e)
			}
		}
		for _, stage := range []string{rcStageIntent, rcStageMain, rcStageSweep, rcStageFactcheck, rcStageConclusion} {
			if e := rcStageEffort(stage); e != b.deepEffort {
				t.Errorf("%s: stage %s runs at effort %q, want %q", b.name, stage, e, b.deepEffort)
			}
		}
	}
}
