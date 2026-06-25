package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kai/internal/autofix"
	"kai/internal/contract"
	"kai/internal/gitio"
	"kai/internal/orchestrator"
	"kai/internal/planner"
	"kai/internal/verify"
)

var (
	autofixIssue  int
	autofixRepo   string
	autofixToken  string
	autofixBase   string
	autofixRemote string
	autofixReady  bool
	autofixPush   bool
	autofixLabel  string
)

var autofixCmd = &cobra.Command{
	Use:   "autofix",
	Short: "Headlessly fix a GitHub issue and open a PR with proof",
	Long: "Reads a GitHub issue, branches, fixes it with the agent loop, verifies the " +
		"change (tests + semantic judge + code review), commits, pushes, and opens a " +
		"pull request whose body carries the evidence. The PR opens as a draft unless " +
		"all three verification signals agree AND --ready is set — the publish gate that " +
		"makes an unattended PR trustworthy.\n\n" +
		"Credentials: --token or $GITHUB_TOKEN; --repo or $GITHUB_REPOSITORY (else derived " +
		"from the git remote). Run from inside the target repo.",
	RunE: runAutofixCmd,
}

var autofixPollCmd = &cobra.Command{
	Use:   "poll",
	Short: "Fix every open issue carrying a label (default: kai-autofix)",
	Long: "Lists open issues with the given label and runs the autofix loop on each, " +
		"skipping any that already have a kai/issue-N branch or open PR. Intended to be " +
		"driven on a schedule (cron / CI).",
	RunE: runAutofixPollCmd,
}

func init() {
	autofixCmd.Flags().IntVar(&autofixIssue, "issue", 0, "issue number to fix (required)")
	autofixCmd.Flags().StringVar(&autofixRepo, "repo", "", "owner/name (default $GITHUB_REPOSITORY or git remote)")
	autofixCmd.Flags().StringVar(&autofixToken, "token", "", "GitHub token (default $GITHUB_TOKEN)")
	autofixCmd.Flags().StringVar(&autofixBase, "base", "", "base branch for the PR (default: current branch)")
	autofixCmd.Flags().StringVar(&autofixRemote, "remote", "origin", "git remote to push to")
	autofixCmd.Flags().BoolVar(&autofixReady, "ready", false, "open as ready-for-review when the gate is fully green (default: always draft)")
	autofixCmd.Flags().BoolVar(&autofixPush, "push", true, "push the branch and open the PR (set false for a local dry run)")

	autofixPollCmd.Flags().StringVar(&autofixLabel, "label", "kai-autofix", "issue label to act on")
	autofixPollCmd.Flags().StringVar(&autofixRepo, "repo", "", "owner/name (default $GITHUB_REPOSITORY or git remote)")
	autofixPollCmd.Flags().StringVar(&autofixToken, "token", "", "GitHub token (default $GITHUB_TOKEN)")
	autofixPollCmd.Flags().StringVar(&autofixBase, "base", "", "base branch for PRs (default: current branch)")
	autofixPollCmd.Flags().StringVar(&autofixRemote, "remote", "origin", "git remote to push to")
	autofixPollCmd.Flags().BoolVar(&autofixReady, "ready", false, "open as ready-for-review when the gate is fully green")
	autofixPollCmd.Flags().BoolVar(&autofixPush, "push", true, "push branches and open PRs")

	autofixCmd.AddCommand(autofixPollCmd)
	rootCmd.AddCommand(autofixCmd)
}

func runAutofixCmd(cmd *cobra.Command, args []string) error {
	if autofixIssue <= 0 {
		return fmt.Errorf("--issue is required (e.g. `kai autofix --issue 42`)")
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	gh, err := resolveGitHubClient()
	if err != nil {
		return err
	}
	return runAutofixOne(ctx, gh, autofixIssue)
}

func runAutofixPollCmd(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	gh, err := resolveGitHubClient()
	if err != nil {
		return err
	}
	issues, err := gh.ListOpenIssues(autofixLabel)
	if err != nil {
		return fmt.Errorf("listing issues: %w", err)
	}
	if len(issues) == 0 {
		fmt.Printf("no open issues labeled %q\n", autofixLabel)
		return nil
	}
	fmt.Printf("found %d issue(s) labeled %q\n", len(issues), autofixLabel)
	var failures int
	for _, iss := range issues {
		fmt.Printf("\n=== #%d: %s ===\n", iss.Number, iss.Title)
		if err := runAutofixOne(ctx, gh, iss.Number); err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "  #%d failed: %v\n", iss.Number, err)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d issue(s) failed", failures, len(issues))
	}
	return nil
}

// resolveGitHubClient builds the client, deriving the repo slug from the
// git remote when neither --repo nor $GITHUB_REPOSITORY is set.
func resolveGitHubClient() (*autofix.Client, error) {
	repo := autofixRepo
	if repo == "" && os.Getenv("GITHUB_REPOSITORY") == "" {
		cwd, _ := os.Getwd()
		if url, err := gitio.RemoteURL(cwd, autofixRemote); err == nil {
			repo = autofix.RepoSlugFromRemote(url)
		}
	}
	return autofix.NewClient(autofixToken, repo)
}

// runAutofixOne executes the full loop for a single issue.
func runAutofixOne(ctx context.Context, gh *autofix.Client, issueNum int) error {
	cwd, _ := os.Getwd()
	branch := autofix.BranchName(issueNum)

	// 1. Fetch the issue.
	iss, err := gh.FetchIssue(issueNum)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}
	if iss.State != "open" {
		return fmt.Errorf("#%d is %s, not open", issueNum, iss.State)
	}

	// 2. Idempotency: skip if we already have a branch or open PR.
	if gitio.BranchExists(cwd, branch) {
		return fmt.Errorf("branch %s already exists — already handled (delete it to retry)", branch)
	}
	if autofixPush {
		if pr, err := gh.FindOpenPRForHead(branch); err == nil && pr != nil {
			return fmt.Errorf("open PR #%d already exists for %s", pr.Number, branch)
		}
	}

	// 3. Determine base and cut the branch.
	base := autofixBase
	if base == "" {
		if cur, err := gitio.CurrentBranch(cwd); err == nil {
			base = cur
		} else {
			base = "main"
		}
	}
	if dirty, _ := gitio.WorkingTreeDirty(cwd); dirty {
		return fmt.Errorf("working tree is dirty; commit or stash before autofix")
	}
	if err := gitio.CreateBranch(cwd, branch); err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	fmt.Printf("→ branched %s off %s\n", branch, base)

	// 4. Build agent services and run the fix.
	svc, err := buildAgentServices(ctx, cwd, false)
	if err != nil {
		return err
	}
	defer svc.Close()

	fmt.Println("→ fixing…")
	res, pres, err := svc.runAgentTask(ctx, autofix.BuildFixPrompt(iss))
	if err != nil {
		return fmt.Errorf("agent run: %w", err)
	}
	if res == nil {
		return fmt.Errorf("planner produced no plan for the issue (it may need clarification)")
	}

	changed := aggregateChangedPaths(res)
	if len(changed) == 0 {
		return fmt.Errorf("agent made no changes — nothing to submit")
	}
	fmt.Printf("→ %d file(s) changed\n", len(changed))

	// 5. Verification — three independent signals.
	diff, err := gitio.DiffAgainst(cwd, base)
	if err != nil {
		return fmt.Errorf("computing diff: %w", err)
	}

	timeout := time.Duration(maxInt(svc.cfg.Agent.TimeoutSeconds, 300)) * time.Second
	det := verify.Continuous(ctx, cwd, timeout)
	detVerdict := verify.Verdict(det, true)
	fmt.Printf("→ deterministic: %s\n", detVerdict)

	intent := iss.Title + "\n\n" + iss.Body
	semText, _, _, serr := svc.judge(ctx, verify.SemanticSystem,
		verify.BuildSemanticPrompt(intent, contract.Plan{}, diff))
	if serr != nil {
		fmt.Fprintf(os.Stderr, "  warning: semantic judge errored: %v\n", serr)
	}
	sem, residue := verify.ParseSemantic(semText)
	fmt.Printf("→ semantic judge: %s\n", semVerdictWord(sem))

	revText, rin, rout, rerr := svc.judge(ctx, autofix.ReviewSystem,
		autofix.BuildReviewPrompt(iss.Title, iss.Body, diff))
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "  warning: reviewer errored: %v\n", rerr)
	}
	review := autofix.ParseReview(revText)
	fmt.Printf("→ code review: approved=%v blocking=%d\n", review.Approved, len(review.Blocking))

	decision := autofix.Decide(detVerdict, sem, review)
	ready := decision.Ready && autofixReady

	// 6. Commit.
	commitMsg := fmt.Sprintf("Fix #%d: %s\n\nCloses #%d.", issueNum, iss.Title, issueNum)
	if err := gitio.CommitAll(cwd, commitMsg); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	tokIn, tokOut := agentTokens(pres, rin, rout)
	evidence := autofix.Evidence{
		IssueNumber:  issueNum,
		IssueTitle:   iss.Title,
		IssueURL:     iss.HTML,
		Branch:       branch,
		Model:        svc.agentModel,
		FilesChanged: changed,
		TestSummary:  detSummary(det),
		DetVerdict:   detVerdict,
		Semantic:     sem,
		Residue:      residue,
		Review:       review,
		AgentSummary: agentSummary(res),
		Decision:     decision,
		TokensIn:     tokIn,
		TokensOut:    tokOut,
	}
	body := autofix.RenderPRBody(evidence)

	if !autofixPush {
		fmt.Println("\n--- dry run (--push=false): not pushing or opening PR ---")
		fmt.Printf("decision: ready=%v\n", ready)
		fmt.Println(body)
		return nil
	}

	// 7. Push and open the PR.
	if err := gitio.Push(cwd, autofixRemote, branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	title := fmt.Sprintf("Fix #%d: %s", issueNum, iss.Title)
	pr, err := gh.CreatePR(autofix.CreatePRInput{
		Title: title,
		Head:  branch,
		Base:  base,
		Body:  body,
		Draft: !ready,
	})
	if err != nil {
		return fmt.Errorf("open PR: %w", err)
	}
	state := "draft"
	if ready {
		state = "ready"
	}
	fmt.Printf("\n✓ opened %s PR #%d: %s\n", state, pr.Number, pr.HTML)
	if !ready {
		fmt.Println("  held as draft — see the PR body for why.")
	}
	return nil
}

// detSummary renders the deterministic CheckResult as one human line.
func detSummary(cr contract.CheckResult) string {
	if len(cr.Failures) > 0 {
		return strings.Join(cr.Failures, "; ")
	}
	if cr.TestsPass == nil {
		return "no test convention detected — deterministic layer inconclusive"
	}
	if *cr.TestsPass {
		return "build + tests passed"
	}
	return "tests failed"
}

// agentTokens sums the token usage we can see: planner + the judge/review
// turns. Orchestrator-internal agent tokens aren't surfaced on Result, so
// this is a lower bound (noted as such in the PR footer wording).
func agentTokens(pres *planner.PlannerResult, extraIn, extraOut int) (int, int) {
	in, out := extraIn, extraOut
	if pres != nil {
		in += pres.TokensIn
		out += pres.TokensOut
	}
	return in, out
}

// aggregateChangedPaths unions ChangedPaths across all agent runs, sorted.
func aggregateChangedPaths(res *orchestrator.Result) []string {
	seen := map[string]bool{}
	for _, r := range res.Runs {
		for _, p := range r.ChangedPaths {
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// agentSummary lists the plan's task names as the "what changed" headline.
// (The orchestrator doesn't surface each agent's final prose on Result, so
// task names are the best structured summary available without re-reading
// the run log.)
func agentSummary(res *orchestrator.Result) string {
	var parts []string
	for _, r := range res.Runs {
		if s := strings.TrimSpace(r.Task.Name); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "; ")
}

func semVerdictWord(s contract.SemanticResult) string {
	switch {
	case s.Matches == nil:
		return "unsure"
	case *s.Matches:
		return "match"
	default:
		return "no-match"
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
