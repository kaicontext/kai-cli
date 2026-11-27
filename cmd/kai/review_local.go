package main

// review_local.go — `kai review-commit --local`: the review pipeline on the
// developer's own Anthropic API key, before they push.
//
// The review of record is still the one Kai runs on the pull request, through
// kailab. This is the run before it: the same prompts, hints, repository rules
// and fact-check, on the diff that has not left the machine, paid for by
// whoever holds the key. The diff and the review are not sent to Kai's server,
// and the key is read from the environment and handed to api.anthropic.com
// only (kai-engine provider.AnthropicLocalReview).
//
// Three things are different from the review CI runs, and all three are
// because the money is somebody else's:
//
//   - Every stage runs on a Claude model. An Anthropic key cannot run the GLM,
//     GPT and Gemini stages of the server's review sets, so the local profile
//     names a Claude model for the stages it keeps and turns the ensemble off.
//     A local review is therefore not the same reviewers as the pull request's.
//   - The models are fixed before the first call and cannot change during the
//     run. A call for any other model is refused (rcLocalMeter), so no failover
//     can move the review to a dearer one.
//   - The run has a spending cap. The estimate is printed before the first
//     call, a deep review asks before it starts, and a review that reaches the
//     cap stops there.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/kaicontext/kai-engine/provider"
)

var (
	reviewCommitLocal   bool
	reviewCommitMaxCost float64
	reviewCommitYes     bool
	// reviewCommitEstimate prints what a local review would cost and stops.
	reviewCommitEstimate bool
)

const (
	// The fast pass's draft and fact-check. Claude Haiku 4.5 is what the
	// fast pass drafts on everywhere else (rcFastDefaultModel), so a local
	// fast review is the pass those prompts were tuned on.
	rcLocalFastModel = "claude-haiku-4-5"
	// The grounded review's agent, sweep and fact-check.
	rcLocalDeepModel = "claude-sonnet-5-5"

	rcLocalFastCapUSD = 0.50
	rcLocalDeepCapUSD = 5.00

	rcLocalKeyEnv = "ANTHROPIC_API_KEY"
)

// rcLocalProfile is the review profile a local review runs under: a Claude
// model for every stage it keeps, and the ensemble stages off. It sets no
// reasoning effort (a bare claude- id carries none, rcCheckProfileEfforts).
func rcLocalProfile() *rcReviewProfile {
	p := &rcReviewProfile{Source: "local review (your Anthropic API key)", Stages: map[string]rcStageSetting{}}
	for _, s := range []string{rcStageQuickDraft, rcStageQuickFactcheck} {
		p.Stages[s] = rcStageSetting{Model: rcLocalFastModel}
	}
	for _, s := range []string{rcStageIntent, rcStageMain, rcStageSweep, rcStageFactcheck, rcStageConclusion} {
		p.Stages[s] = rcStageSetting{Model: rcLocalDeepModel}
	}
	for _, s := range []string{rcStageFinder2, rcStageSweep2, rcStageRank, rcStageHumanize, rcStageSpecLogic, rcStageSpecSecurity, rcStageSpecContracts} {
		p.Stages[s] = rcStageSetting{Model: rcEffortOff}
	}
	return p
}

// rcLocalModels are the models the active local profile names: the only ones
// the run may call.
func rcLocalModels(p *rcReviewProfile) []string {
	seen := map[string]bool{}
	for _, s := range p.Stages {
		if s.Model != "" && s.Model != rcEffortOff && s.Model != "none" {
			seen[s.Model] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// rcLocalNewProvider builds the direct provider from a key. A variable so a
// test can stand a local server in for api.anthropic.com.
var rcLocalNewProvider = func(key string) (provider.Provider, error) {
	return provider.NewAnthropicLocalReview(key)
}

// rcLocalProvider is the provider a local review runs on: the direct one,
// metered. The key comes from ANTHROPIC_API_KEY and nowhere else. In
// particular the two variables that hold a Claude subscription sign-in
// (ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN) are never read.
func rcLocalProvider(models []string, capUSD float64) (*rcLocalMeter, error) {
	key := strings.TrimSpace(os.Getenv(rcLocalKeyEnv))
	if key == "" {
		return nil, fmt.Errorf("a local review runs on your own Anthropic API key: set %s to a key from the Claude Console.\n"+
			"A Claude subscription sign-in is not an API key and cannot be used here", rcLocalKeyEnv)
	}
	inner, err := rcLocalNewProvider(key)
	if err != nil {
		return nil, fmt.Errorf("local review: %w", err)
	}
	allowed := map[string]bool{}
	for _, m := range models {
		bare, err := provider.AnthropicLocalReviewModel(m)
		if err != nil {
			return nil, fmt.Errorf("local review: %w", err)
		}
		allowed[bare] = true
	}
	return &rcLocalMeter{inner: inner, allowed: allowed, capUSD: capUSD}, nil
}

// rcLocalMeter is the provider every call of a local review goes through. It
// refuses a model the run did not start with, refuses a call once the cap is
// reached, and adds up what the answers cost.
type rcLocalMeter struct {
	inner   provider.Provider
	allowed map[string]bool
	capUSD  float64

	mu       sync.Mutex
	spentUSD float64
	// reservedUSD is the ceiling of every call in flight (admit, settle).
	reservedUSD float64
	calls       int
}

// admit lets a call through, or says why not, and reserves what the call may
// cost. The reservation is what makes the cap hold when calls overlap: a deep
// review's sweep sends chunks side by side with the agent, and a check against
// money already spent would let every one of them through while none had been
// billed yet. Calls in flight count against the cap at their reserved price
// until settle replaces it with what the answer cost.
func (m *rcLocalMeter) admit(req provider.Request) (reserved float64, err error) {
	bare, err := provider.AnthropicLocalReviewModel(req.Model)
	if err != nil {
		return 0, fmt.Errorf("local review: %w", err)
	}
	if !m.allowed[bare] {
		return 0, fmt.Errorf("local review: refusing a call to %s — this run was started on %s and does not switch models",
			bare, strings.Join(m.models(), ", "))
	}
	reserved = rcLocalCallCeiling(bare, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.capUSD > 0 && m.spentUSD+m.reservedUSD >= m.capUSD {
		return 0, fmt.Errorf("local review stopped: it has spent or committed about $%.2f, the cap for this run (raise it with --max-cost)",
			m.spentUSD+m.reservedUSD)
	}
	m.reservedUSD += reserved
	return reserved, nil
}

// rcLocalCallCeiling is the most one call can cost at list prices: its whole
// prompt read uncached, and every output token it is allowed.
func rcLocalCallCeiling(model string, req provider.Request) float64 {
	in := len(req.System)
	if raw, err := json.Marshal(req.Messages); err == nil {
		in += len(raw)
	}
	if raw, err := json.Marshal(req.Tools); err == nil {
		in += len(raw)
	}
	c, _ := provider.AnthropicLocalReviewCostUSD(model, in/3+1, req.MaxTokens)
	return c
}

func (m *rcLocalMeter) models() []string {
	out := make([]string, 0, len(m.allowed))
	for k := range m.allowed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// settle ends a call's reservation and, when it was answered, records what
// the answer cost.
func (m *rcLocalMeter) settle(reserved float64, r *provider.Response) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reservedUSD -= reserved
	if m.reservedUSD < 0 {
		m.reservedUSD = 0
	}
	if r != nil {
		m.spentUSD += r.EstimatedCostUSD
		m.calls++
	}
}

// Spent is what the run's answers cost at list prices, and how many calls
// that was.
func (m *rcLocalMeter) Spent() (usd float64, calls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spentUSD, m.calls
}

func (m *rcLocalMeter) Send(ctx context.Context, req provider.Request) (provider.Response, error) {
	reserved, err := m.admit(req)
	if err != nil {
		return provider.Response{}, err
	}
	resp, err := m.inner.Send(ctx, req)
	if err != nil {
		m.settle(reserved, nil)
		return resp, err
	}
	m.settle(reserved, &resp)
	return resp, nil
}

func (m *rcLocalMeter) SendStream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	s, ok := m.inner.(provider.Streamer)
	if !ok {
		return nil, fmt.Errorf("local review: provider does not stream")
	}
	reserved, err := m.admit(req)
	if err != nil {
		return nil, err
	}
	in, err := s.SendStream(ctx, req)
	if err != nil {
		m.settle(reserved, nil)
		return nil, err
	}
	out := make(chan provider.StreamEvent, 32)
	go func() {
		defer close(out)
		var final *provider.Response
		for ev := range in {
			if ev.Kind == "done" && ev.Final != nil {
				final = ev.Final
			}
			out <- ev
		}
		m.settle(reserved, final)
	}()
	return out, nil
}

func (m *rcLocalMeter) SupportsCache() bool { return true }

func (m *rcLocalMeter) SupportsCacheForModel(string) bool { return true }

// rcLocalEstimate is a rough cost for the review about to run, from the size
// of the diff and list prices. It is deliberately on the high side of what a
// typical run spends: it is shown to the person paying before the first call.
//
// Fast: the draft and the fact-check each read the diff and the context built
// around it once. Deep: an agent reads the diff, then files, over turns; later
// turns re-read earlier ones from the prompt cache at a fraction of the price,
// so the turns are counted at a third of a full read each, plus the sweep and
// the fact-check.
func rcLocalEstimate(deep bool, diffBytes, changedFiles int) float64 {
	diffTokens := diffBytes/3 + 1
	const overhead = 6000
	fastIn, fastOut := 2*(diffTokens+overhead), 6000
	est, _ := provider.AnthropicLocalReviewCostUSD(rcLocalFastModel, fastIn, fastOut)
	if !deep {
		return est
	}
	turns := rcReviewMaxTurns(changedFiles)
	perTurn := diffTokens + overhead + 4000
	deepIn := perTurn*turns/3 + 3*(diffTokens+overhead)
	deepOut := turns*800 + 8000
	est, _ = provider.AnthropicLocalReviewCostUSD(rcLocalDeepModel, deepIn, deepOut)
	return est
}

// rcLocalCap is the run's spending cap: --max-cost, else the tier's default.
func rcLocalCap(deep bool) float64 {
	if reviewCommitMaxCost > 0 {
		return reviewCommitMaxCost
	}
	if deep {
		return rcLocalDeepCapUSD
	}
	return rcLocalFastCapUSD
}

// rcLocalConfirm prints the estimate and, for a deep review, asks. It returns
// an error when the review should not start. A deep review that cannot ask
// (no terminal on stdin) needs --yes: a tool that calls this on someone's
// behalf has to have shown them the figure first.
func rcLocalConfirm(deep bool, est, capUSD float64, models []string) error {
	tier := "fast"
	if deep {
		tier = "deep"
	}
	fmt.Fprintf(os.Stderr, "  local review (%s) on your Anthropic API key · %s\n", tier, strings.Join(models, ", "))
	fmt.Fprintf(os.Stderr, "  estimated cost about $%.2f at list prices; this run stops at $%.2f\n", est, capUSD)
	if est > capUSD {
		return fmt.Errorf("the estimate ($%.2f) is over this run's cap ($%.2f); pass --max-cost to raise it, or review a smaller range", est, capUSD)
	}
	if !deep || reviewCommitYes {
		return nil
	}
	if !rcStdinIsTerminal() {
		return fmt.Errorf("a deep local review can cost a few dollars: pass --yes to run it without a prompt (estimate $%.2f, cap $%.2f)", est, capUSD)
	}
	fmt.Fprint(os.Stderr, "  run the deep review? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return fmt.Errorf("deep local review not started")
	}
	return nil
}

var rcStdinIsTerminal = func() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// rcLocalReport is the bundle's "local" object: present only on a local
// review, so a reader of the JSON can tell it from a review Kai ran.
type rcLocalReport struct {
	// Provider is always "anthropic-local-review".
	Provider string `json:"provider"`
	// Models are the models the run was allowed to call.
	Models []string `json:"models"`
	// SpentUSD is the answers' cost at list prices; CapUSD is where the run
	// would have stopped. Anthropic's invoice is the record, not this.
	SpentUSD float64 `json:"spentUsd"`
	CapUSD   float64 `json:"capUsd"`
	Calls    int     `json:"calls"`
	// Base is what the change was compared against, and WorkingTree says the
	// reviewed commit was made from uncommitted work.
	Base        string `json:"base,omitempty"`
	WorkingTree bool   `json:"workingTree,omitempty"`
}

// rcLocalRun is the state of the local review in progress, nil for any other
// review.
var rcLocalRun *rcLocalState

type rcLocalState struct {
	meter       *rcLocalMeter
	models      []string
	base        string
	workingTree bool
}

func (s *rcLocalState) report() *rcLocalReport {
	if s == nil {
		return nil
	}
	spent, calls := s.meter.Spent()
	return &rcLocalReport{
		Provider: string(provider.KindAnthropicLocalReview), Models: s.models,
		SpentUSD: spent, CapUSD: s.meter.capUSD, Calls: calls,
		Base: s.base, WorkingTree: s.workingTree,
	}
}

func (s *rcLocalState) printSpend() {
	if s == nil {
		return
	}
	spent, calls := s.meter.Spent()
	fmt.Fprintf(os.Stderr, "  local review spent about $%.3f over %d call(s), at list prices (your Anthropic invoice is the record)\n", spent, calls)
}

// rcLocalTarget resolves what a local review reviews. With a commit named it
// is that commit (against --base when given). With none it is everything on
// this branch that has not been pushed plus the uncommitted work on top: a
// commit made from the working tree, compared against where the branch left
// the default branch.
//
// The working-tree commit is written with a temporary index, so the real
// index, the branch and the stash are untouched; it is referenced by nothing
// and git collects it in its own time.
func rcLocalTarget(args []string, base string) (ref, resolvedBase string, workingTree bool, err error) {
	if len(args) == 1 {
		return args[0], base, false, nil
	}
	if strings.TrimSpace(base) == "" {
		base, err = rcLocalDefaultBase()
		if err != nil {
			return "", "", false, err
		}
	}
	commit, made, err := rcLocalWorktreeCommit(base)
	if err != nil {
		return "", "", false, err
	}
	return commit, base, made, nil
}

// rcLocalDefaultBase is the remote default branch this branch grew from:
// origin/HEAD when the clone knows it, else origin/main or origin/master.
func rcLocalDefaultBase() (string, error) {
	candidates := []string{}
	if out, err := exec.Command("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD").Output(); err == nil {
		if r := strings.TrimSpace(string(out)); r != "" {
			candidates = append(candidates, r)
		}
	}
	candidates = append(candidates, "origin/main", "origin/master")
	for _, c := range candidates {
		if exec.Command("git", "rev-parse", "--verify", "--quiet", c+"^{commit}").Run() == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not tell what to compare against (no origin/HEAD, origin/main or origin/master): pass --base <branch>")
}

// rcLocalWorktreeCommit returns a commit holding the working tree as it is
// now (tracked changes and untracked files, ignored files left out), with
// HEAD as its parent. When the tree has nothing uncommitted it returns HEAD
// and made=false.
func rcLocalWorktreeCommit(base string) (commit string, made bool, err error) {
	git := func(env []string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			msg := ""
			if ee, ok := err.(*exec.ExitError); ok {
				msg = strings.TrimSpace(string(ee.Stderr))
			}
			return "", fmt.Errorf("git %s: %w %s", strings.Join(args, " "), err, msg)
		}
		return strings.TrimSpace(string(out)), nil
	}
	head, err := git(nil, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	tmp, err := os.CreateTemp("", "kai-local-review-index-*")
	if err != nil {
		return "", false, err
	}
	tmp.Close()
	// git wants to create the index file itself.
	os.Remove(tmp.Name())
	defer os.Remove(tmp.Name())
	idx := []string{"GIT_INDEX_FILE=" + tmp.Name()}
	if _, err := git(idx, "read-tree", "HEAD"); err != nil {
		return "", false, err
	}
	if _, err := git(idx, "add", "-A"); err != nil {
		return "", false, err
	}
	tree, err := git(idx, "write-tree")
	if err != nil {
		return "", false, err
	}
	headTree, err := git(nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", false, err
	}
	if tree == headTree {
		return head, false, nil
	}
	branch, _ := git(nil, "rev-parse", "--abbrev-ref", "HEAD")
	var msg strings.Builder
	fmt.Fprintf(&msg, "Local changes on %s, not pushed\n\n", branch)
	if subjects, _ := git(nil, "log", "--format=- %s", base+"..HEAD"); subjects != "" {
		msg.WriteString("Commits on this branch:\n")
		msg.WriteString(subjects)
		msg.WriteString("\n\n")
	}
	msg.WriteString("Plus uncommitted changes in the working tree.\n")
	// commit-tree needs an identity; a machine with none configured still
	// gets a review.
	ident := []string{}
	if exec.Command("git", "config", "user.name").Run() != nil {
		ident = append(ident, "GIT_AUTHOR_NAME=kai local review", "GIT_COMMITTER_NAME=kai local review")
	}
	if exec.Command("git", "config", "user.email").Run() != nil {
		ident = append(ident, "GIT_AUTHOR_EMAIL=local-review@invalid", "GIT_COMMITTER_EMAIL=local-review@invalid")
	}
	commit, err = git(ident, "commit-tree", tree, "-p", head, "-m", msg.String())
	if err != nil {
		return "", false, err
	}
	return commit, true, nil
}

// rcLocalEstimateReport is what --estimate prints with --format json.
type rcLocalEstimateReport struct {
	Depth        string   `json:"depth"`
	EstimateUSD  float64  `json:"estimateUsd"`
	CapUSD       float64  `json:"capUsd"`
	Models       []string `json:"models"`
	Base         string   `json:"base,omitempty"`
	ChangedFiles int      `json:"changedFiles"`
}

// rcLocalPrintEstimate is `review-commit --local --estimate`: the figure a
// person, or a tool acting for one, reads before deciding to run the review.
func rcLocalPrintEstimate(w io.Writer, deep bool, est, capUSD float64, models []string, base string, files int) error {
	depth := "fast"
	if deep {
		depth = "deep"
	}
	if reviewCommitFormat == "json" {
		out, err := json.MarshalIndent(rcLocalEstimateReport{depth, est, capUSD, models, base, files}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(out))
		return nil
	}
	fmt.Fprintf(w, "A %s local review of %d changed file(s) is estimated at about $%.2f at list prices, on %s.\n"+
		"It would stop at $%.2f. Nothing was run.\n", depth, files, est, strings.Join(models, ", "), capUSD)
	return nil
}
