package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"kai/internal/autofix"
	"kai/internal/config"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/gitio"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/planner"
	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/reviewanalyze"
	"github.com/kaicontext/kai-engine/session"
)

// review-commit is the agentic, graph-grounded PR reviewer the CI review
// workflow runs (kai-review.yml). It rides the SAME harness the coding paths
// use — agent.ModeReview on the shared runner, with the harness's review
// personality, tool whitelist, graph context injection, session/run logging,
// and effort tiers — instead of the bespoke prompt-in-prompt agent it started
// as. The reviewer writes an actual review (prose a colleague would leave on
// the PR) plus a small machine coda that feeds finding.Finding — the shape the
// findings inbox stores.
const maxReviewCommitDiffBytes = 120 * 1024

// rcInferIntentSystem reconstructs a commit's goal from its message + diff (one
// focused call), mirroring kit verify-commit's intent reconstruction.
const rcInferIntentSystem = "You reconstruct the INTENT of a merged pull request from its commit message and its diff. " +
	"Write the intent as a short specification of the desired end-state — what the change is supposed to ACHIEVE, not a " +
	"summary of which lines changed. 2 to 6 sentences. Be FAITHFUL to the author, not stricter than they were. Treat the " +
	"commit message as the source of truth for the goal; the diff is only evidence. Output only the intent prose: no " +
	"preamble, no markdown headers, no fences."

// rcReviewSystem is composed from reviewskill/ (review_commit_skill.go).

const rcMaxAuthorContextBytes = 8 * 1024

// Review time contract. The reviewer rides the shared agent runner, which has
// grown outer-agent (Jeff) machinery with no bound that applies to a ReadOnly
// run — its coding-run guards are all skipped, leaving MaxTurns and the CI
// step's 30-minute timeout as the only limits. A review is worth ~5 minutes of
// agent time; a run still going past that is thrashing, not reviewing.
// rcReviewHardDeadline backstops a hung provider call that never reaches a
// turn boundary (the soft budget is only enforced between turns).
//
// Raised 2026-08-31. The grounding rules added in #60 make the reviewer do
// strictly more work per review — it now web-searches an external rate before
// endorsing it and reads across repos to bound its own claims — and the old
// 5m/9m budget was set before any of that. Measured on the same commit
// (kai-server#126): a run that completed took 6m24s, i.e. already past the old
// soft budget and living on extensions, and a second run truncated mid-sentence
// with "my web check did not complete before time ran out", producing
// intent=unknown and zero flags. A hollow finding is worse than a slow one: it
// reads as "nothing to report" rather than "I ran out of time".
//
// The CI job's timeout is 60 minutes (review_default_workflow.go), so this
// stays well inside it.
//
// Raised again 2026-09-29, with the turn budget below. The extension was never
// real for a review: the engine extends only a run that recently edited a file
// (runner.go lastSubstantiveEditAt), and a review never edits, so 9 minutes
// was the whole budget. And run 5 of the 2026-09-28 benchmark found the
// reviewer stopping on its TURN cap at a median of ~4 minutes, having opened
// the right code for 19 of the 45 defects it missed without raising them. The
// soft budget is now the budget the review actually gets, sized for the larger
// turn cap at a thinking model's slower pace (KAI_REVIEW_REASONING_EFFORT).
const (
	rcReviewSoftBudget    = 15 * time.Minute
	rcReviewSoftExtension = 3 * time.Minute // only granted after an edit, which a review never makes
	rcReviewHardDeadline  = 25 * time.Minute
)

// The review's TURN budget, which is the one that actually binds.
//
// It was a flat 20 from the day the agentic reviewer shipped (dc19c2a,
// 2026-07-07, in a commit about diff patches) and was never revisited, while
// the time budget above was raised twice on measurement. Measuring the turn
// budget shows the mistake: across 85 live reviews carrying the coverage
// manifest the median run took 90 seconds against a 540-second soft budget,
// and only 6% came near it. Nothing ever hit the clock, because 20 turns at
// the observed 9.3s/turn costs about 187 seconds — a third of the budget it
// was given.
//
// The runner spends the last of that on winding down, too: it injects
// "wrap it up" hints three turns before the cap and strips every tool on the
// final turn, so a flat 20 is 17 turns of real work no matter how large the
// change.
//
// What that cost: on PRs of nine files or more the reviewer opened 6.5 files
// and left 8.4 unopened, 45% of those runs reached the wind-down with three
// fifths of their clock unspent, and findings per changed file fell from 1.40
// on a one-file PR to 0.14 on a PR of twenty-one or more.
//
// So the budget is sized to the change instead. A reviewer needs at least one
// turn per changed file to read it and roughly one more to follow what it
// found; the base covers the sweep the prompt asks for on top. The ceiling is
// set so the WALL CLOCK becomes the binding limit again, which is what the
// soft budget was designed to be: 45 turns at the observed pace is about 420
// seconds, inside the 540-second soft budget, and a run slower than that hits
// the time budget and degrades the way the time budget already handles.
//
// Raised 2026-09-29 (base 20→30, 2→3 per file, ceiling 45→72): run 5 showed
// the reviewer finishing on this cap in ~4 minutes with most of its clock
// unspent, on PRs where it had read the defect's code and moved on. 72 turns
// at the measured pace is ~670 seconds, inside the 15-minute soft budget.
const (
	rcReviewBaseTurns    = 30
	rcReviewTurnsPerFile = 3
	rcReviewTurnCeiling  = 72
	// rcObservedSecondsPerTurn is the median turn cost measured across 85 live
	// reviews on 2026-09-09 (z-ai/glm-5.2, the model the CI workflow exports).
	// It is a constant rather than a number in a comment because the ceiling
	// above is only correct while it holds: TestReviewMaxTurnsScalesWithTheChange
	// multiplies the two and fails if the product leaves the soft budget. Change
	// the model tier or the graph and this is the figure to re-measure — the
	// test will say so.
	//
	// The test guards one direction, and that is on purpose. Too LARGE and the
	// ceiling would leave the soft budget, which nothing else would catch —
	// hence the assertion. Too SMALL and turns cost more than this says, so
	// the run reaches rcReviewSoftBudget before its turn cap: the soft budget
	// fires, the review concludes, and the incomplete path reports it. That
	// direction already has a backstop, and it is the one the whole design
	// wants — the wall clock binding rather than the turn count.
	rcObservedSecondsPerTurn = 9.3
)

// rcReviewMaxTurns sizes the exploration budget to the diff. Never below the
// old flat value, so no review gets less room than it has today.
func rcReviewMaxTurns(changedFiles int) int {
	n := rcReviewBaseTurns + rcReviewTurnsPerFile*changedFiles
	if n > rcReviewTurnCeiling {
		return rcReviewTurnCeiling
	}
	if n < rcReviewBaseTurns {
		return rcReviewBaseTurns
	}
	return n
}

var (
	reviewCommitFormat string
	reviewCommitBase   string
	reviewCommitBranch string
	reviewCommitFast   bool
	reviewCommitDeep   bool
)

var reviewCommitCmd = &cobra.Command{
	Use:   "review-commit <commit>",
	Short: "Harness-grade, graph-grounded code review of a commit — a human-readable review plus a finding (headless)",
	Long: "Reviews a commit (typically a squash-merged PR) with the same agent harness the coding paths use, in its\n" +
		"review mode: read-only tools, graph context injection, and the harness's review personality. It reconstructs\n" +
		"the author's intent, hunts for concrete defects (concurrency, security, resource leaks, correctness, error\n" +
		"handling) using kai_callers / kai_dependents / kai_context to confirm each is real and reachable, and writes\n" +
		"the review the way a colleague would — prose you can read, not a findings block. --format json emits a\n" +
		"finding.Finding (verdict + intent + risks + diff), the same JSON the findings inbox stores, with the prose\n" +
		"review on stderr.\n\n" +
		"This is what the CI review workflow runs.\n\n" +
		"DEFAULT: the fast pass — one model call over the diff, no agent loop, no graph, no `kai capture`\n" +
		"required, an answer in seconds rather than minutes. It reads no callers, so it clears nothing:\n" +
		"MERGE_READY is capped at 4 and it never reports an all-clear.\n\n" +
		"--deep is the grounded review: the agent harness with the graph, kai_callers / kai_dependents /\n" +
		"kai_context and web search, on a 9-minute soft budget. It is what confirms a concern is real and\n" +
		"reachable, and it requires a captured graph (`kai capture`). In CI both run — the fast pass posts\n" +
		"first and --deep supersedes it in place.",
	Args: cobra.ExactArgs(1),
	RunE: runReviewCommit,
}

func init() {
	reviewCommitCmd.Flags().StringVar(&reviewCommitFormat, "format", "text", "output format: text|json")
	reviewCommitCmd.Flags().StringVar(&reviewCommitBase, "base", "", "review the aggregate diff of <base>...<commit> (PR range) instead of a single commit")
	reviewCommitCmd.Flags().StringVar(&reviewCommitBranch, "branch", "", "branch name to record on the finding (default: GITHUB_HEAD_REF / GITHUB_REF_NAME / the checked-out branch)")
	// --fast is the default, so the flag is only ever redundant. It stays
	// because CI pods, scripts, and the built-in review workflow all spell it
	// out, and a flag that silently becomes an error breaks them on the next
	// image bump for no gain.
	reviewCommitCmd.Flags().BoolVar(&reviewCommitFast, "fast", false, "shallow first pass over the diff (the default; the flag is explicit-only)")
	reviewCommitCmd.Flags().BoolVar(&reviewCommitDeep, "deep", false, "the grounded review: agent harness + graph, minutes not seconds — requires `kai capture`")
	rootCmd.AddCommand(reviewCommitCmd)
}

func runReviewCommit(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ref := args[0]
	cwd, _ := os.Getwd()

	// The fast pass is the default review. --deep opts into the grounded one;
	// --fast is the explicit spelling of the default. Asking for both is a
	// contradiction, and guessing which one the caller meant is how a CI job
	// silently reviews at the wrong depth for a month.
	if reviewCommitFast && reviewCommitDeep {
		return fmt.Errorf("--fast and --deep are opposites; pass one or neither (the default is --fast)")
	}
	fast := !reviewCommitDeep
	// The quick pass is retired from CI: its comment was deleted as soon as
	// the grounded review posted, so it cost a model call and a minute of
	// every review for nothing a reader kept. The CI workflow still asks for
	// it by name (--fast) and tolerates a failure, so refusing here skips it
	// in a second. A local run without flags still gets the fast default.
	if reviewCommitFast && os.Getenv("KAI_REVIEW_QUICK_PASS") != "1" {
		return fmt.Errorf("the quick pass is retired; the grounded review (--deep) is the review (KAI_REVIEW_QUICK_PASS=1 runs it anyway)")
	}

	// The graph is REQUIRED for the grounded review and OPTIONAL for --fast.
	// That is the whole latency win: a fast pass that needed a captured graph
	// would still make its CI job pay for `kai capture` before the first
	// token, which is most of the two-minute budget it is trying to fit in.
	// A fast run in a captured repo still uses the project's kai dir (config,
	// credentials) and its root (identifier lookups); an uncaptured one falls
	// back to main.go's cwd resolve and the cwd.
	set, outcome := projects.Discover(cwd)
	switch {
	case outcome == projects.OutcomeRootsFound:
		if err := set.Open(); err != nil {
			return fmt.Errorf("opening projects: %w", err)
		}
		defer set.Close()
		// Point config loads and the run log at the discovered project's kai
		// dir (main.go's default is a cwd resolve, which diverges in
		// sub-directories).
		kaiDir = set.Primary().KaiDir
	case fast:
		set = nil
	default:
		return fmt.Errorf("--deep needs a captured graph — run `kai capture` first, or drop --deep for the fast pass, which needs none")
	}
	// The root the fast pass greps for identifier lookups. A captured project
	// knows its own root; an uncaptured one must ask git, NOT assume the cwd.
	// The diff comes from git and its paths are worktree-relative, so a run
	// from a subdirectory would grep a subtree while reviewing the whole
	// change — the lookups would quietly cover less than the diff, which is
	// the one thing they exist to prevent.
	repoRoot := rcWorktreeRoot(cwd)
	if set != nil {
		repoRoot = set.Primary().Path
	}

	hash, subject, body, err := rcCommitMeta(ref)
	if err != nil {
		return err
	}
	// A merge commit says nothing about the work it brings in ("Merge
	// origin/main into X" is not a goal), so its stated intent, inbox title,
	// and author context come from the commits in the reviewed range instead.
	isMerge := rcIsMergeCommit(hash)
	rangeSubjects, rangeBodies := rcRangeCommits(reviewCommitBase, ref)
	stated := rcStatedIntent(subject, isMerge, rangeSubjects)
	title := rcTitle(subject, isMerge, rangeSubjects)
	authorContext := rcWithPRDescription(rcAuthorContext(subject, body, isMerge, rangeSubjects, rangeBodies))
	intentBody := body
	if isMerge && len(rangeSubjects) > 0 {
		intentBody = authorContext
		fmt.Fprintf(os.Stderr, "  merge commit: intent taken from %d commit(s) in %s..%s\n",
			len(rangeSubjects), reviewCommitBase, rcShort(hash))
	}
	diff := rcCommitDiff(reviewCommitBase, ref, maxReviewCommitDiffBytes)
	if strings.TrimSpace(diff) == "" {
		return fmt.Errorf("commit %s has an empty diff (merge commit? try a child, or pass --base)", rcShort(hash))
	}

	// The run's review profile, if the benchmark put one on the base branch
	// (review_commit_profile.go). Loaded before any model is resolved, so
	// every stage below sees it.
	if err := rcLoadProfileFor(reviewCommitBase); err != nil {
		return err
	}

	prov, model, provKind := rcReviewProvider()
	if prov == nil {
		return fmt.Errorf("no LLM provider available (run `kai login`)")
	}
	model = rcStageModel(rcStageMain, model)
	stageModels := map[string]string{
		rcStageQuickDraft:     rcFastModel(model, provKind),
		rcStageQuickFactcheck: rcFastChallengeModel(model),
		rcStageIntent:         rcStageModel(rcStageIntent, model),
		rcStageMain:           model,
		rcStageSweep:          rcSweepModel(model),
		rcStageFactcheck:      rcChallengeModel(model),
		rcStageConclusion:     rcStageModel(rcStageConclusion, model),
		rcStageFinder2:        rcEnsembleModel(rcStageFinder2),
		rcStageSweep2:         rcEnsembleModel(rcStageSweep2),
		rcStageRank:           rcEnsembleModel(rcStageRank),
	}
	if err := rcCheckProfileEfforts(stageModels); err != nil {
		return err
	}
	if line := rcDescribeProfile(stageModels); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}

	// Diff stat up front: it is pure git, and --fast hands the changed paths to
	// the model so its ISSUES bullets name a file the pipeline can resolve. The
	// first live fast run wrote both of its real defects as bare line numbers
	// ("2152 — ..."), which rcGroundIssue holds — visible in the inbox, but not
	// counted, so two genuine bugs would have shipped under a green badge.
	added, removed, files := rcCommitDiffStat(reviewCommitBase, ref)
	changedPaths := make([]string, 0, len(files))
	for _, df := range files {
		changedPaths = append(changedPaths, df.Path)
	}

	mode := "review-commit --deep"
	if fast {
		mode = "review-commit (fast)"
	}
	fmt.Fprintf(os.Stderr, "kai %s %s · %s\n", mode, rcShort(hash), subject)

	var raw string
	// Non-nil only on the grounded path, and only describes HOW the run ended
	// — see rcIncomplete. Used solely when the review produced nothing to parse.
	var inc *rcIncomplete
	// challenge is the publication gate's structured record from whichever path
	// ran: each allegation's and decision's final status.
	var challenge *rcChallengeResult
	if fast {
		// The fast pass may substitute a non-reasoning model for the DRAFT. The
		// CHALLENGE — the publication gate — uses the configured challenge
		// model (KAI_CHALLENGE_MODEL, else the review model); the draft's
		// substitution must never silently reach it.
		fastModel := rcFastModel(model, provKind)
		fmt.Fprintf(os.Stderr, "  fast pass: one call over the diff, no graph (draft model %s, challenge model %s, budget %s)…\n",
			fastModel, rcFastChallengeModel(model), rcFastHardDeadline)
		phase := time.Now()
		raw, challenge, err = rcRunFastReview(ctx, prov, fastModel, rcFastChallengeModel(model), repoRoot, authorContext, stated, intentBody, diff, changedPaths)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  timing: fast-review=%s\n", time.Since(phase).Round(time.Second))
	} else {
		intentModel := rcStageModel(rcStageIntent, model)
		fmt.Fprintf(os.Stderr, "  reconstructing intent (model %s)…\n", intentModel)
		phase := time.Now()
		intent, ierr := rcInferIntent(ctx, prov, intentModel, stated, intentBody, diff)
		if ierr != nil {
			return fmt.Errorf("infer intent: %w", ierr)
		}
		fmt.Fprintf(os.Stderr, "  timing: intent=%s\n", time.Since(phase).Round(time.Second))

		fmt.Fprintf(os.Stderr, "  reviewing against the graph…\n\n")
		phase = time.Now()
		hosts := rcNewHostsBlock(rcNewHosts(hash, diff, rcPathsOf(files), rcFilesMentioningHost))
		sweep := rcStartSweep(ctx, prov, model, intent, reviewCommitBase, ref)
		// The publication gate reads the reviewed commit to settle what the
		// reviewer's own tool results do not show (review_commit_repo.go).
		gateCtx := rcWithRepo(ctx, rcNewRepo(reviewCommitBase, ref, hash))
		raw, inc, err = rcRunReviewAgent(gateCtx, set, prov, model, authorContext, intent, hosts, diff, rcPathsOf(files), sweep)
		if err != nil {
			return err
		}
		if inc != nil {
			challenge = inc.Challenge
		}
		fmt.Fprintf(os.Stderr, "  timing: review=%s\n", time.Since(phase).Round(time.Second))
	}

	prose, risks, decisions, match, readiness, note := rcParseReviewOutput(raw)
	if fast {
		readiness = rcCapFastReadiness(readiness)
		risks = rcFilterFastIssues(risks)
	} else if strings.TrimSpace(prose) != "" {
		prose = rcDefectsOnlyProse(prose)
	}

	// An empty review is a FAILURE, not a finding. Shipping a bundle with no
	// prose, no risks, and an unknown intent verdict green-checks a shell —
	// the CI job succeeds, the inbox shows nothing, and nobody learns the
	// review never happened (PRs #89/#90, 2026-08-26). That invariant holds:
	// this run still exits non-zero.
	//
	// What changed is that failing no longer means vanishing. Returning here
	// emitted NOTHING, and the review step runs this CLI unguarded under
	// set -eu, so the job died before the ingest and the PR was told the
	// review could not be finished — with no hint that twelve minutes of
	// reviewing had happened (kai-server#184, 2026-09-08). When the run left
	// us facts about its own ending, write those down as the review, keep
	// match/readiness Unknown (the value that means "no opinion", never a
	// point on the merge scale), emit the bundle so it can be delivered, and
	// THEN fail. A reader gets "did not finish, here is how far it got"
	// instead of silence.
	incomplete := false
	if strings.TrimSpace(prose) == "" && len(risks) == 0 && len(decisions) == 0 && match == finding.MatchUnknown {
		salvaged := rcIncompleteProse(inc)
		if salvaged == "" {
			return fmt.Errorf("review produced no content (no prose, no risks, intent unknown) — failing instead of posting an empty finding")
		}
		prose = salvaged
		incomplete = true
		fmt.Fprintf(os.Stderr, "  review produced no conclusion — emitting an incomplete-review finding, then failing\n")
	}
	// Items the challenge could not settle are withheld and listed under
	// "Could not verify" in the review; they no longer make it incomplete
	// (rcValidateChallenge).
	if open := challenge.unresolved(); len(open) > 0 {
		fmt.Fprintf(os.Stderr, "  review published with %d item(s) it could not verify: %s\n", len(open), strings.Join(open, "; "))
	}
	// An empty record (the draft had nothing to challenge) is omitted from the
	// bundle rather than published as a hollow block.
	if challenge.rcEmpty() {
		challenge = nil
	}

	// Blast radius: walk the captured graph outward from the changed files so the
	// finding shows what the change reaches (callers/importers), not just the diff.
	// headHex="" walks the freshly-captured (single-snapshot) graph unscoped.
	// Non-fatal — blast is a panel, not the review itself.
	//
	// A --fast run in an uncaptured repo has no graph to walk; the panel is
	// simply absent, which is honest for a pass that read no callers.
	var blast finding.Blast
	if set != nil {
		b, berr := reviewanalyze.BlastFor(ctx, set.Primary().DB, changedPaths, "")
		if berr != nil {
			fmt.Fprintf(os.Stderr, "  blast radius unavailable: %v\n", berr)
		}
		blast = b
	}

	from := rcShort(rcParentHash(hash))
	if reviewCommitBase != "" {
		if b, e := exec.Command("git", "rev-parse", reviewCommitBase).Output(); e == nil {
			from = rcShort(strings.TrimSpace(string(b)))
		}
	}

	// A DECISION is a correct change that still needs a human's yes — a changed
	// number that reaches a charge, a cap, a send, or a delete. It is not a
	// defect, so it carries no path:line and never lowers INTENT_MATCH; but it
	// MUST stop the green badge, because "intent verified, nothing flagged" is
	// exactly the wrong thing to tell an author who is about to move customer
	// money without having said so. Folding decisions into the risk-tagged
	// claims does that with no server change: mergeLine (github_pr_comment.go)
	// flips to "Review before merging" on RiskCount > 0. The prefix keeps them
	// readable as what they are in the inbox and the PR comment.
	// kaicontext/kai-server#126 (2026-08-31) is the case this exists for: the
	// reviewer traced a 5% surcharge into the credit-drawdown path, reported it
	// as evidence of correctness, and green-checked a billing change.
	flags := make([]string, 0, len(risks)+len(decisions))
	flags = append(flags, risks...)
	for _, d := range decisions {
		flags = append(flags, "Decision: "+d)
	}

	// Each ISSUE becomes a Claim grounded against the reviewed tree: its
	// path:line resolves to the source line itself as the lookup, or the
	// claim is held when that location does not exist at this revision (the
	// one thing the reviewer was asked to pin down could not be found where
	// it said). A DECISION is grounded by construction. (URL hosts the change
	// introduces are no longer added here: they reach the reviewer as context
	// — rcNewHostsBlock — and become findings only through its ISSUES.) The
	// inbox denormalizes RiskCount from
	// grounded risk-tagged claims, so held claims are visible but do not
	// count.
	claims := make([]finding.Claim, 0, len(flags))
	tree := rcTreeFiles(hash)
	for _, r := range risks {
		r = rcLocateNamedIssue(hash, r, tree, rcFileLines)
		claims = append(claims, rcGroundIssue(hash, r, tree, rcFileLines))
	}
	for _, d := range decisions {
		claims = append(claims, rcDecisionClaim(d))
	}

	f := finding.Finding{
		ID:      rcFindingID(hash),
		Title:   title,
		Branch:  rcBranchName(reviewCommitBranch),
		Author:  rcCommitAuthor(hash),
		From:    from,
		To:      rcShort(hash),
		Added:   added,
		Removed: removed,
		Files:   len(files),
		Verdict: finding.VerdictAwaiting,
		// What should happen to this branch next, 1-5. Unknown when the
		// reviewer gave no score, which renders as nothing rather than
		// as the harsh end of the scale.
		Readiness: readiness,
		Intent: finding.Intent{
			Stated: stated,
			Match:  match,
			Note:   note,
			Risks:  flags,
		},
		Claims: claims,
		Diff:   finding.Diff{Files: files},
		Blast:  blast,
	}

	if reviewCommitFormat == "json" {
		// stdout stays pure JSON for ingestion; the human review still goes
		// to stderr so a CI log shows the actual review, not just a blob.
		if prose != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n\n", prose)
		}
		// Carry the prose review inside the bundle as a "review" field. The
		// server stores the bundle verbatim (json.RawMessage), so the field
		// round-trips to `kai findings get` and the inbox without any server
		// or finding-package change; finding.Finding can adopt it later.
		// Depth rides alongside Review, for the same reason and by the same
		// mechanism: the server stores the bundle verbatim, so a new field
		// round-trips to `kai findings get` and the inbox with no server or
		// finding-package change. It is what lets the two-pass flow tell a
		// shallow finding from the grounded one that supersedes it — without
		// it, a 60-second skim renders in the inbox identically to a
		// nine-minute callers-checked review, which is worse than being slow.
		var reason *rcIncompleteReason
		if incomplete {
			reason = rcIncompleteReasonOf(inc)
		}
		depth := "grounded"
		if fast {
			depth = "fast"
		}
		// incomplete rides along because the counts cannot carry it: a run
		// that stopped before its conclusion emits no risks, no decisions and
		// an unknown intent, which is arithmetically identical to a review
		// that read everything and liked it. Without this flag the renderer
		// has only the prose to go on, and it opened two timed-out reviews
		// with "Nothing jumped out" directly above their own "This review did
		// not finish" (kai-desktop#304, kai-server#186, 2026-09-08).
		out, err := json.MarshalIndent(struct {
			finding.Finding
			Review     string      `json:"review,omitempty"`
			Depth      string      `json:"depth,omitempty"`
			Incomplete bool        `json:"incomplete,omitempty"`
			Coverage   *rcCoverage `json:"coverage,omitempty"`
			// Challenge is the publication gate's structured record: each
			// allegation's and decision's final status, reason and citations.
			// Additive and optional; readers that do not know it ignore it.
			Challenge *rcChallengeResult `json:"challenge,omitempty"`
			// IncompleteReason says, in fixed labels, why an incomplete
			// review stopped, so the server need not infer it from Review.
			IncompleteReason *rcIncompleteReason `json:"incompleteReason,omitempty"`
		}{f, prose, depth, incomplete, rcCoverageOf(inc), challenge, reason}, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling finding: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out))
		// Emitted first, failed second, and the order is the entire point: the
		// bundle is on stdout (so the workflow's `> finding.json` has content to
		// ingest) before the non-zero exit tells CI the review did not finish.
		if incomplete {
			return rcErrIncompleteReview
		}
		return nil
	}

	// Text mode: the review itself is the output. Fall back to the parsed
	// fields only when the model skipped the prose (legacy strict-block runs).
	if prose != "" {
		fmt.Println(prose)
		if note != "" {
			fmt.Printf("\nBottom line: %s\n", note)
		}
		if incomplete {
			return rcErrIncompleteReview
		}
		return nil
	}
	if note != "" {
		fmt.Println(note)
	}
	if len(risks) == 0 {
		fmt.Println("No concerns — looks good.")
	} else {
		fmt.Printf("\n%d concern(s):\n", len(risks))
		for _, r := range risks {
			fmt.Printf("  • %s\n", r)
		}
	}
	fmt.Printf("\nIntent match: %s\n", f.Intent.Match)
	if incomplete {
		return rcErrIncompleteReview
	}
	return nil
}

// rcWorktreeRoot resolves the git worktree root for dir, falling back to dir
// itself when git cannot answer (not a repo, or git missing). Only the
// uncaptured fast path needs this — a captured project already carries its
// root — and it is what keeps the identifier lookups covering the same tree
// the diff was taken from.
func rcWorktreeRoot(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return dir
	}
	if root := strings.TrimSpace(string(out)); root != "" {
		return root
	}
	return dir
}

// rcReviewProvider builds the LLM provider (kailab creds → OpenRouter, or an
// ANTHROPIC_API_KEY fallback), reusing the gate/planner plumbing.
func rcReviewProvider() (provider.Provider, string, provider.Kind) {
	cfg, err := config.Load(kaiDir)
	if err != nil {
		return nil, "", ""
	}
	prov, reviewModel, _, err := buildGateProvider(cfg)
	if err != nil {
		return nil, "", ""
	}
	// The kind, not just the model: it decides whether the fast pass may
	// substitute a model at all. An OpenAI-compatible endpoint (Together,
	// Groq, Ollama, vLLM, LM Studio all normalize to KindOpenAI) serves its
	// own namespace, and handing it an OpenRouter-style id would fail the
	// DEFAULT review outright. See rcFastModel.
	base, token := kailabCreds()
	return prov, reviewModel, provider.FromEnv(base, token, cfg.Planner.Model).Kind
}

func rcInferIntent(ctx context.Context, prov provider.Provider, model, subject, body, diff string) (string, error) {
	var in strings.Builder
	in.WriteString("COMMIT MESSAGE:\n")
	in.WriteString(strings.TrimSpace(subject))
	if strings.TrimSpace(body) != "" {
		in.WriteString("\n\n")
		in.WriteString(strings.TrimSpace(body))
	}
	in.WriteString("\n\nDIFF:\n")
	in.WriteString(diff)

	resp, err := prov.Send(ctx, provider.Request{
		Model:           model,
		System:          rcInferIntentSystem,
		MaxTokens:       rcTokensFor(600, rcStageEffort(rcStageIntent)),
		ReasoningEffort: rcStageEffort(rcStageIntent),
		Messages:        []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: in.String()}}}},
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			out.WriteString(t.Text)
		}
	}
	return out.String(), nil
}

// rcRepoIdentity resolves the repository the review is about, as an
// "owner/name" GitHub slug.
//
// The reviewer used to have no way to know this, and it showed: reviews on
// kai-desktop#288 and kai-desktop#300 (2026-09-08) stated they were reading
// "the kai-engine repo" and "the kai-server working tree". Neither was true,
// and nothing in the run could have told them otherwise — the CI job clones
// into `mktemp -d`, so the workspace is a random path like /tmp/tmp.aBc123,
// and the prompt named the repository nowhere. Meanwhile rcReviewSystem
// REQUIRES a repository in the output ("name the boundary you actually
// searched … 'within this repo, the only caller is X' is honest"). The
// instructions demanded an answer the input withheld, so the model supplied a
// plausible sibling from the same ecosystem.
//
// The resolution order mirrors resolveGitHubClient (autofix_cmd.go), so this
// repo has one answer to "which GitHub repo am I in" rather than two:
// GITHUB_REPOSITORY_FULLNAME first — the CI workflow prefers it for exactly
// the case where the kai org name and the GitHub org name differ — then
// GITHUB_REPOSITORY, then the checkout's own origin remote, which is what
// makes this work for a human running review-commit locally.
//
// Returns "" when nothing resolves. The caller then says nothing rather than
// guessing, which is the whole point.
func rcRepoIdentity(dir string) string {
	for _, env := range []string{"GITHUB_REPOSITORY_FULLNAME", "GITHUB_REPOSITORY"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	if url, err := gitio.RemoteURL(dir, "origin"); err == nil {
		return autofix.RepoSlugFromRemote(url)
	}
	return ""
}

// rcRepoHeader is the prompt's opening line: which repository this is.
//
// Empty in, empty out. An unnamed boundary is recoverable — the reviewer says
// "this repo" and a reader knows which PR they are looking at — while a
// confidently wrong one is not, and inventing a name here would rebuild the
// exact defect this exists to close.
func rcRepoHeader(repo string) string {
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("REPOSITORY: %s\n(The repository under review. Every path below is relative to its root. "+
		"This is the boundary to name when you write one — \"within %s, the only caller is X\". "+
		"Do not name a different repository as the one you are reading; sibling repos you cannot see "+
		"here are exactly the limit worth stating.)\n\n", repo, repo)
}

// rcRunReviewAgent runs the review through the shared harness runner, set up
// the way the orchestrator sets up its executors: agent.ModeReview supplies
// the harness's review personality + read-only tool whitelist, the graph
// injection seeds turn 0 with real context, the session store and run log make
// the run inspectable (`kai run summary`), and ApplyEffort honors KAI_SPEED.
// rcReviewSystem rides in Options.System underneath the mode prompt.
func rcRunReviewAgent(ctx context.Context, set *projects.Set, prov provider.Provider, model, sourceContext, intent, hosts, diff string, changed []string, sweep <-chan rcSweepResult) (string, *rcIncomplete, error) {
	// The packs for this change's languages and risk areas (review_commit_skill.go).
	packs := rcPacksFor(diff)
	if len(packs) > 0 {
		fmt.Fprintf(os.Stderr, "  review skill packs: %s\n", strings.Join(packs, ", "))
	}
	publicationCtx := ctx
	primary := set.Primary()
	gdb := asGraphDB(primary.DB)

	var user strings.Builder
	// First line of the prompt, because everything after it is relative to
	// this. rcReviewSystem asks the reviewer to name the boundary it searched;
	// this is the name. Omitted entirely when it cannot be resolved — an
	// unnamed boundary is recoverable, a confidently wrong one is not.
	user.WriteString(rcRepoHeader(rcRepoIdentity(primary.Path)))
	if sc := strings.TrimSpace(sourceContext); sc != "" {
		if len(sc) > rcMaxAuthorContextBytes {
			sc = sc[:rcMaxAuthorContextBytes] + "\n... (context truncated)"
		}
		user.WriteString("AUTHOR CONTEXT (the change author's own description):\n")
		user.WriteString(sc)
		user.WriteString("\n\n")
	}
	// Hand the reviewer the diff's own added/changed symbols up front. The
	// PR#89 dogfood run burned its entire time budget grepping for
	// SendUsageWarning — a name sitting in the diff it had been given —
	// because brand-new symbols resolve poorly through graph search. The
	// reviewer must never spend turns discovering what its input states.
	declared := map[string]bool{}
	if symbols := rcChangedSymbols(diff); symbols != "" {
		user.WriteString("CHANGED SYMBOLS (extracted from this diff — these are new or modified IN THIS CHANGE. ")
		user.WriteString("Do not search the graph for these names; open the listed files directly):\n")
		user.WriteString(symbols)
		user.WriteString("\n\n")
		for _, ln := range strings.Split(symbols, "\n") {
			if i := strings.LastIndex(ln, ": "); i >= 0 {
				declared[strings.TrimSpace(ln[i+2:])] = true
			}
		}
	}
	// The lookups the reviewer would otherwise spend a turn each on. Only the
	// identifiers this change reads or assigns, only where they live, and only
	// when the answer is short enough to be a shortcut. See
	// review_commit_lookups.go for why the graph cannot cover these.
	if lookups := rcIdentifierLookups(diff, primary.Path, declared); lookups != "" {
		user.WriteString("WHERE THESE LIVE (resolved from the repository before this review started — ")
		user.WriteString("treat as already-run searches; do not re-run them):\n")
		user.WriteString(lookups)
		user.WriteString("\n")
	}
	// The contracts this diff rests on that live in OTHER modules. Fetched at
	// the pinned commit where that is possible, named as a limitation where it
	// is not — both rendered from one result, so a block saying "you cannot
	// read these" can never sit beside one containing the source.
	if deps := rcChangedDeps(diff); len(deps) > 0 {
		phase := time.Now()
		src, unresolved := rcFetchDepSources(ctx, deps)
		user.WriteString(rcDepSourceBlock(src))
		user.WriteString(rcDepLimitsBlock(unresolved))
		fmt.Fprintf(os.Stderr, "  dependencies: %d file(s) fetched, %d module(s) unread (%s)\n",
			len(src), len(unresolved), time.Since(phase).Round(time.Millisecond))
	}
	user.WriteString(hosts)
	user.WriteString(rcReviewHints(diff))
	user.WriteString("INTENT:\n")
	user.WriteString(strings.TrimSpace(intent))
	user.WriteString("\n\nDIFF:\n")
	if strings.TrimSpace(diff) == "" {
		user.WriteString("(no changes)\n")
	} else {
		user.WriteString(diff)
		user.WriteString("\n")
	}

	// Graph-powered turn-0 injection, same as the orchestrator's executors:
	// resolve the diff's entry points against the call graph + command index
	// so the reviewer starts oriented instead of spending turns rediscovering
	// structure. Best-effort — an empty body just skips injection.
	var injected string
	if gdb != nil {
		injected = planner.BuildInjectedContext(user.String(), gdb, planner.LoadCommandIndex(primary.Path))
	}

	// Session + run-log plumbing so the review shows up in `kai run summary`
	// like every other harness run. Non-fatal: we review without it.
	if err := session.EnsureSchema(gdb); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent session schema: %v\n", err)
	}

	kaiBin := "kai"
	if exe, err := os.Executable(); err == nil {
		kaiBin = exe
	}

	ctx, cancel := context.WithTimeout(ctx, rcReviewHardDeadline)
	defer cancel()

	opts := agent.Options{
		Projects:  set,
		Workspace: primary.Path,
		Provider:  prov,
		Model:     model,
		Graph:     gdb,
		// The harness's review lane: prepends the review-mode system prompt,
		// scopes graph context to changed functions, and whitelists the
		// read-only tool set (+ kai_impact / kai_diff). ReadOnly is belt and
		// braces on top of the mode's whitelist.
		Mode:       agent.ModeReview,
		System:     rcReviewSystemFor(packs),
		ReadOnly:   true,
		EnableBash: false,
		MaxTurns:   rcReviewMaxTurns(len(changed)),
		Prompt:     user.String(),

		InjectedContext: injected,
		SessionStore:    gdb,
		TaskName:        "review-commit",
		RunLogDir:       kaiDir,
		KaiBinary:       kaiBin,
		// Keep tool results verbatim so prompt caching works across turns —
		// the same lesson every other harness path already carries.
		KeepToolResults: true,

		// The review's time contract: without this a ReadOnly run has no
		// wall-clock bound at all (the runner's coding-run guards are skipped
		// for ReadOnly) and slow harness turns stack up to the CI step timeout.
		SoftTimeBudget:          rcReviewSoftBudget,
		SoftTimeBudgetExtension: rcReviewSoftExtension,
		// Thinking, when the job asks for it (KAI_REVIEW_REASONING_EFFORT, or
		// the review profile's main stage). Unset, the provider keeps GLM's
		// reasoning off, as before.
		ReasoningEffort: rcStageEffort(rcStageMain),
		Hooks: agent.Hooks{
			OnToolCall: func(name, inputJSON string) {
				fmt.Fprintf(os.Stderr, "  → %s %s\n", name, rcOneLine(inputJSON, 90))
			},
		},
	}
	// Effort tier LAST, after every deliberate field above — ApplyEffort only
	// tightens. Zero-value Speed resolves KAI_SPEED → thorough (a no-op).
	agent.ApplyEffort(&opts, 0)

	// A second finder on another model family reviews beside the first
	// (review_commit_ensemble.go); its issues join the draft before the check.
	second := rcStartSecondFinder(ctx, publicationCtx, opts, prov)

	started := time.Now()
	res, err := agent.Run(ctx, opts)
	if err != nil {
		return "", nil, fmt.Errorf("review run: %w", err)
	}
	fmt.Fprintln(os.Stderr)

	// Facts about how the run ENDED, gathered whether or not it wrote itself
	// down. When the write-down is missing these are the whole report the PR
	// gets, so they are collected unconditionally and cost nothing.
	inc := &rcIncomplete{
		Model:        model,
		FinishReason: string(res.FinishReason),
		Elapsed:      time.Since(started),
		Turns:        rcTurns(res.Transcript),
		FilesRead:    rcFilesRead(res.Transcript, primary.Path),
	}
	raw := rcRestoreCodaMarker(strings.TrimSpace(res.FinalText))

	// COVERAGE GATE. A review that never opened a changed file is not a
	// verdict on it, and until now the only consequence was a line in the
	// manifest telling the reader so. Ask for the reading instead.
	//
	// This is where the misses actually are. Of 89 real defects found by
	// another reviewer and not by Kai on the same pull requests, 56 were
	// visible in the changed lines themselves — the miss rate is the same
	// whether the evidence sits in the diff (64%), in another file of the
	// same repository (67%), or behind a caller (67%). Distance is not the
	// explanation; not looking is.
	//
	// The pass RESUMES the run's own session rather than starting over, so
	// the model keeps everything it has already read and pays only for the
	// files it skipped. It is skipped entirely when the first run died on
	// the clock — there is no point asking for more reading from a run that
	// ran out of time — and its output is adopted only if it produced a
	// coda, so a failed second pass leaves the first review exactly as it
	// was.
	// transcript is what the conclusion fallback below reads. It is the FIRST
	// run's history until the gate runs, and the gate's afterwards — a resumed
	// session's transcript is the whole conversation, so it is a superset. The
	// gate exists to read files the first pass skipped, and dropping its
	// transcript here would throw away exactly the reading it was run for.
	transcript := res.Transcript

	unopened := rcUnopenedChanged(changed, inc.FilesRead)
	if len(unopened) > 0 && res.FinishReason != message.FinishReasonTimeBudget && res.SessionID != "" {
		if left := rcGateHeadroom(started); left <= 0 {
			fmt.Fprintf(os.Stderr, "\n  coverage gate: %d file(s) unopened, but not enough clock left to ask\n", len(unopened))
		} else {
			fmt.Fprintf(os.Stderr, "\n  coverage gate: %d of %d changed file(s) never opened — asking for them\n",
				len(unopened), len(changed))
			gate := opts
			gate.SessionID = res.SessionID
			gate.MaxTurns = rcCoverageGateTurns(len(unopened))
			gate.Prompt = rcCoverageGatePrompt(unopened)
			gate.InjectedContext = "" // already in the session's history
			// Its own clock. `ctx` carries rcReviewHardDeadline counted from
			// the START of the first pass, so a first run that spent its turns
			// but finished cleanly could hand the gate seconds — and a gate
			// that then died on that inherited deadline would write
			// FinishReasonTimeBudget over a first pass that had finished
			// perfectly well. Same reasoning as rcConcludeFromTranscript's
			// fresh deadline, and the soft budget is scaled down to match.
			gate.SoftTimeBudget = left
			gate.SoftTimeBudgetExtension = 0
			res2, err2 := func() (*agent.Result, error) {
				// Its own scope so the cancel is DEFERRED. It fires on any
				// exit from agent.Run, not only the ordinary one, which is
				// what makes "does the runner leave a session lock or a
				// goroutine behind on an abort?" a question this code does
				// not have to answer.
				gctx, gcancel := context.WithTimeout(context.Background(), left)
				defer gcancel()
				return agent.Run(gctx, gate)
			}()
			switch {
			case err2 != nil:
				fmt.Fprintf(os.Stderr, "  coverage gate failed (%v) — keeping the first review\n", err2)
			default:
				// A resumed session's Transcript is the WHOLE conversation, so
				// these are assignments, not additions. Adding would have
				// counted the first pass's turns twice and republished the
				// very 2x overcount this change removes from the manifest.
				//
				// Verified in kai-engine rather than assumed, because the
				// property lives outside this repo and nothing here can test
				// it: runner.go's resolveSession loads s.History() as the seed
				// `hist` on the opts.SessionID path, and res.Transcript is
				// assigned that same history. res.SessionID is likewise set
				// whenever a session exists (`if sess != nil { res.SessionID =
				// sess.ID }`), and the review always passes SessionStore, so
				// the gate's `res.SessionID != ""` guard is satisfied on every
				// grounded run rather than silently never firing.
				inc.Elapsed = time.Since(started)
				inc.Turns = rcTurns(res2.Transcript)
				inc.FilesRead = rcMergeFilesRead(inc.FilesRead, rcFilesRead(res2.Transcript, primary.Path))
				transcript = res2.Transcript
				// Adopt the second answer only when it is a WHOLE review. A
				// second pass that ran out of turns mid-write can carry the
				// marker and none of the fields behind it, and swapping that
				// in would turn a good first review into an incomplete
				// finding. When it is not usable the first review stands and
				// the fallback below still gets the gate's transcript, so the
				// files it opened are not lost either.
				raw, inc.FinishReason, _ = rcMergeGate(raw, res2.FinalText, string(res2.FinishReason))
				if still := rcUnopenedChanged(changed, inc.FilesRead); len(still) > 0 {
					fmt.Fprintf(os.Stderr, "  coverage gate: %d file(s) still unopened\n", len(still))
				}
			}
		}
	}
	// A run that ran out of road — the soft time budget fired, or the loop
	// ended without ever emitting the structured coda — has read the code
	// but never wrote the review down. Don't ship that as an empty finding:
	// make ONE direct conclusion call over the run's own transcript, forcing
	// the write-down from what was already seen. (PR#89 dogfood: 5m38s of
	// healthy exploration, budget expiry at a turn boundary, hollow finding
	// posted as success.)
	//
	// The condition is the ANSWER, not the finish reason. It used to be
	// "timed out OR no marker", from before rcUsableCoda existed — and once
	// the gate could set inc.FinishReason, a gate that died on the clock
	// would fire this branch and overwrite a perfectly good first review with
	// a transcript conclusion. A run that timed out has no usable coda by
	// construction, so !rcUsableCoda already covers the case the first clause
	// was there for, without covering the one it should not.
	if rcNeedsConclusion(raw) {
		fmt.Fprintf(os.Stderr, "  review ended without a conclusion (finish=%s) — requesting one from the transcript…\n", inc.FinishReason)
		concluded, category := rcConcludeFromTranscript(publicationCtx, prov, model, transcript)
		if concluded != "" {
			raw = rcRestoreCodaMarker(concluded)
		} else {
			inc.ConclusionCategory, inc.ConclusionModel = category, rcStageModel(rcStageConclusion, model)
		}
	}
	raw = rcWithSecondFinder(raw, rcAwaitSecondFinder(second))
	if rcUsableCoda(raw) {
		sw := rcAwaitSweep(sweep)
		if len(sw.Issues) > 0 {
			before := len(rcIssuesOf(raw))
			raw = rcDraftWithSweep(raw, sw.Issues)
			fmt.Fprintf(os.Stderr, "  sweep: %d defect(s) proposed from %d chunk(s) — %d added to the reviewer's %d\n",
				len(sw.Issues), sw.Chunks, len(rcIssuesOf(raw))-before, before)
		}
		fmt.Fprintln(os.Stderr, "  challenging proposed defects before publication…")
		gateModel := rcChallengeModel(model)
		if gateModel != model {
			fmt.Fprintf(os.Stderr, "  challenge model: %s (review model %s)\n", gateModel, model)
		}
		res, err := rcChallengeReviewWith(publicationCtx, prov, gateModel, raw, rcChallengeSources(transcript), sw.Sources, rcConfiguredSandbox())
		if err == nil {
			if n := rcWithholdUnsettledSweep(res, sw.Issues); n > 0 {
				fmt.Fprintf(os.Stderr, "  sweep: %d proposal(s) the check could not settle were withheld\n", n)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  review challenge incomplete: %v\n", err)
			inc.ChallengeFailure = err.Error()
			inc.ChallengeCategory, inc.ChallengeModel = rcFailureCategory(err), gateModel
			return "", inc, nil
		}
		// The check settled what is true; the selection step chooses what is
		// worth publishing (review_commit_ensemble.go).
		rcRankSupported(publicationCtx, prov, intent, diff, len(changed), res)
		raw = res.Review
		inc.Challenge = res
	}
	return raw, inc, nil
}

// rcCommitContextReserve is how much of rcMaxAuthorContextBytes is held back
// for the commit messages, and rcMaxPRDescriptionBytes is what is left for the
// description after it and the block's own header.
//
// Derived rather than chosen, so the guarantee the comment makes is the
// guarantee the arithmetic makes. A flat 6KB cap inside an 8KB context left
// the claim "the description cannot crowd the commits out" true only by
// accident and only approximately: a near-cap description plus the header
// left the commits a sliver, and the truncation that took the rest happened
// somewhere else entirely (rcRunReviewAgent's rcMaxAuthorContextBytes cut).
const (
	rcCommitContextReserve  = 2 * 1024
	rcPRDescriptionHeader   = 256 // the block's own prose, generously rounded
	rcMaxPRDescriptionBytes = rcMaxAuthorContextBytes - rcCommitContextReserve - rcPRDescriptionHeader
)

// rcWithPRDescription puts the pull request's own description at the front of
// the author context.
//
// AUTHOR CONTEXT is what the reviewer tests the code against — the prompt tells
// it to treat this as the author's account of the change. It has always been
// built from commit messages, and on a pull request that is the wrong document:
// it is what the author wrote about a commit, not about the change. Every
// review a customer has had was judged against the wrong statement of intent.
//
// The description leads and the commits follow, so if anything is truncated it
// is the supporting detail rather than the claim being tested.
//
// Read from the environment rather than a flag because the review pod inherits
// it: kai-server injects KAI_PR_TITLE / KAI_PR_BODY into the runner, and the
// built-in workflow needs no change to pass them on. Absent — a local
// review-commit, a push that is not a pull request, an older control plane —
// and the author context is exactly what it is today.
func rcWithPRDescription(commits string) string {
	title := strings.TrimSpace(os.Getenv("KAI_PR_TITLE"))
	body := strings.TrimSpace(os.Getenv("KAI_PR_BODY"))
	if title == "" && body == "" {
		return commits
	}
	var b strings.Builder
	b.WriteString("THE PULL REQUEST, in the author's words. This is the change's stated goal; " +
		"the commit messages below are how it was built.\n\n")
	if title != "" {
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
	}
	if body != "" {
		if len(body) > rcMaxPRDescriptionBytes {
			body = body[:rcMaxPRDescriptionBytes] + "\n\n... (description truncated)"
		}
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(commits) != "" {
		b.WriteString("--- the commits that make it up ---\n\n")
		b.WriteString(commits)
	}
	return strings.TrimSpace(b.String())
}

// rcPathsOf lists the files a change touches, from the diff STAT rather than
// from the prompt's diff text.
//
// The distinction is the whole point. The prompt's diff is truncated at
// maxReviewCommitDiffBytes, deletions end their hunk at `+++ /dev/null`, and a
// binary or mode-only change has no `+++` header at all — so reading paths out
// of it silently omits files on exactly the large changes the turn budget and
// the coverage gate exist for. rcCommitDiffStat asks git for the file list and
// has none of those holes.
//
// It is also the list the server compares the manifest against
// (changedFilesNotListed reads the bundle's diff.files, which is this), so the
// gate now asks for precisely the files the review will otherwise be shown to
// have skipped.
func rcPathsOf(files []finding.DiffFile) []string {
	out := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// rcUnopenedChanged returns the changed files that do not appear in what the
// run recorded opening. It is deliberately the same comparison the server
// renders under the coverage manifest, so the gate and the disclosure can
// never disagree about which files were skipped.
//
// The capture walks tool-call arguments for path fields, so a file can be
// absent from the manifest and still have been read — asking for it again
// costs a turn and is the cheap side of that error.
func rcUnopenedChanged(changed, filesRead []string) []string {
	if len(changed) == 0 {
		return nil
	}
	read := make(map[string]bool, len(filesRead))
	for _, r := range filesRead {
		read[r] = true
	}
	var out []string
	for _, c := range changed {
		if read[c] {
			continue
		}
		// A manifest entry may be an ABSOLUTE path — the run works in a
		// mktemp checkout — so a read path that ends in the changed path
		// counts as having opened it.
		//
		// Only that direction. The mirror test, "the changed path ends in the
		// read path", is unsound: with `db/secrets.go` and `pkg/db/secrets.go`
		// both changed and only the first opened, it declares the second
		// opened too and drops the one file the gate exists to name. Two
		// changed files sharing a tail is not exotic in a repo with parallel
		// package trees, and the failure is silent.
		hit := false
		for _, r := range filesRead {
			if strings.HasSuffix(r, "/"+c) {
				hit = true
				break
			}
		}
		if !hit {
			out = append(out, c)
		}
	}
	return out
}

// rcMergeFilesRead unions two manifests, keeping order and dropping repeats.
func rcMergeFilesRead(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// rcGateMinHeadroom is the least clock worth starting a second pass with, and
// rcGateMaxBudget the most it may take. Below the floor the gate would produce
// a truncated answer and a worse manifest than saying nothing.
const (
	rcGateMinHeadroom = 45 * time.Second
	rcGateMaxBudget   = 4 * time.Minute
)

// rcGateHeadroom is how long the coverage pass may run: what remains of the
// review's hard deadline, capped, or zero when there is not enough left to be
// worth asking. The gate is an addition to a review that already has an
// answer, so it never gets to be the thing that makes the run late.
func rcGateHeadroom(started time.Time) time.Duration {
	left := rcReviewHardDeadline - time.Since(started)
	if left < rcGateMinHeadroom {
		return 0
	}
	if left > rcGateMaxBudget {
		return rcGateMaxBudget
	}
	return left
}

// rcMergeGate decides what a finished coverage pass changes about the review:
// the answer to keep, how the run ended, and whether the second answer was the
// one adopted.
//
// It is a function rather than four lines inside rcRunReviewAgent because the
// two rules in it are the ones that broke, and neither is observable from a
// test that drives the helpers around it:
//
//   - the finish reason belongs to the pass that finished LAST, adopted or
//     not, or the manifest describes a pass that is no longer the last thing
//     to have happened;
//   - a second answer replaces the first only when it is a WHOLE review, or a
//     pass that ran out of turns mid-write turns a good review into an
//     incomplete finding.
//
// Reverting either rule now fails a test instead of nothing.
func rcMergeGate(firstRaw, secondRaw, secondFinish string) (raw, finish string, adopted bool) {
	raw, finish = firstRaw, secondFinish
	if s := rcRestoreCodaMarker(strings.TrimSpace(secondRaw)); rcUsableCoda(s) {
		return s, secondFinish, true
	}
	return raw, finish, false
}

// rcNeedsConclusion reports whether the review still has to be written down.
//
// The ANSWER decides, not the finish reason. It used to be "timed out OR no
// marker", from before rcUsableCoda existed — and once the gate could set the
// finish reason, a gate that died on the clock fired this branch and
// overwrote a perfectly good first review with a transcript conclusion. A run
// that timed out has no usable coda by construction, so this covers the case
// the timeout clause was there for and not the one it should not.
func rcNeedsConclusion(raw string) bool { return !rcUsableCoda(raw) }

// rcRestoreCodaMarker puts back the marker line when the answer carries the
// coda's fields without it. The reviewer model drops that line far more often
// than it keeps it — 78 of 84 finished answers across seven regression runs
// ended in a complete INTENT_MATCH / MERGE_READY / SUMMARY / ISSUES block with
// no marker above it — and every one of them was then treated as a review
// that never concluded: thrown away, the coverage gate's rewrite thrown away
// with it, and replaced by a tool-less conclusion written from the transcript.
//
// The coda starts at the LAST line that opens with INTENT_MATCH: and is
// followed by MERGE_READY: or SUMMARY:, so prose that merely mentions the
// field, or quotes an example block earlier on, is left alone. A horizontal
// rule the model put between prose and coda goes with the marker it stood in
// for.
func rcRestoreCodaMarker(raw string) string {
	if strings.Contains(raw, rcReviewDataMarker) {
		return raw
	}
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "INTENT_MATCH:") {
			continue
		}
		tail := strings.Join(lines[i+1:], "\n")
		if !strings.Contains(tail, "MERGE_READY:") && !strings.Contains(tail, "SUMMARY:") {
			return raw
		}
		prose := strings.TrimSpace(strings.Join(lines[:i], "\n"))
		prose = strings.TrimSpace(strings.TrimSuffix(prose, "---"))
		coda := strings.Join(lines[i:], "\n")
		if prose == "" {
			return rcReviewDataMarker + "\n" + coda
		}
		return prose + "\n\n" + rcReviewDataMarker + "\n" + coda
	}
	return raw
}

// rcUsableCoda reports whether a raw answer carries a machine coda the
// pipeline can actually read, rather than just the line that introduces one.
//
// The distinction matters wherever one answer replaces another. A pass that
// ran out of turns mid-write emits the marker and stops, and treating that as
// a review would swap a complete one for an incomplete finding.
func rcUsableCoda(raw string) bool {
	i := strings.Index(raw, rcReviewDataMarker)
	if i < 0 {
		return false
	}
	tail := raw[i+len(rcReviewDataMarker):]
	return strings.Contains(tail, "INTENT_MATCH:") || strings.Contains(tail, "SUMMARY:")
}

// rcCoverageGateTurns bounds the second pass: a turn to open each skipped file
// and a few to fold what they contain back into the review. Small on purpose —
// this pass exists to close a hole, not to start the review over.
func rcCoverageGateTurns(unopened int) int {
	n := unopened + 4
	if n > 16 {
		return 16
	}
	return n
}

// rcCoverageGatePrompt is the second pass's whole instruction. It names the
// files rather than asking the model to work out what it skipped, and it asks
// for the WHOLE review again rather than a supplement, because the coda is
// what the pipeline parses and a partial second answer would have to be
// merged with the first by hand.
func rcCoverageGatePrompt(unopened []string) string {
	var b strings.Builder
	b.WriteString("Before your review can stand, these files are part of this change and you did not open them:\n\n")
	const cap = 12
	shown := unopened
	if len(shown) > cap {
		shown = shown[:cap]
	}
	for _, p := range shown {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	if len(unopened) > cap {
		fmt.Fprintf(&b, "- …and %d more\n", len(unopened)-cap)
	}
	b.WriteString("\nOpen each one and apply the same sweep you applied to the rest of the change. " +
		"Then output your review AGAIN in full, ending with the machine coda exactly once — " +
		"revised if what you just read changes it, unchanged if it does not. " +
		"Do not describe what you did in this pass; write the review.")
	return b.String()
}

// rcTurns counts the model's turns in a transcript.
//
// Not len(transcript): that is the MESSAGE count — the prompt, each assistant
// turn, and each tool result — which is roughly twice the number of turns.
// The coverage manifest published it as "turns" and told every reader of a
// review that it had run about twice as long as it did. A manifest that exists
// to stop overclaiming cannot overclaim by 2x.
func rcTurns(transcript []message.Message) int {
	n := 0
	for _, m := range transcript {
		if m.Role == message.RoleAssistant {
			n++
		}
	}
	return n
}

// rcIncomplete records how a review run ENDED, so a run that never wrote
// itself down can still say something true about itself.
//
// The 2026-09-08 kai-server#184 shape: 12m7s of real reviewing against a
// 39-file diff, the soft budget expires at a turn boundary
// (FinishReasonTimeBudget), the transcript-conclusion fallback then blows its
// own deadline, and rcParseReviewOutput sees nothing. The old behaviour was to
// return an error here — which, because the review step runs the CLI unguarded
// under set -eu, aborted the job BEFORE the ingest and left the PR with
// "Couldn't finish reviewing this change" and no trace of the twelve minutes.
// Losing the work is not the same as reporting that it did not finish.
type rcIncomplete struct {
	ChallengeFailure string
	FinishReason     string
	// Model is the review agent's model. The categories and the stage models
	// below are fixed labels and model ids, never error text: they ride in the
	// bundle (rcIncompleteReasonOf), which the server stores and logs.
	Model              string
	ChallengeCategory  string
	ChallengeModel     string
	ConclusionCategory string
	ConclusionModel    string
	Elapsed            time.Duration
	Turns              int
	FilesRead          []string
	// Challenge is the gate's structured record when it PUBLISHED a review.
	// Unlike ChallengeFailure, the review body is real and kept; items it
	// could not settle are listed in it under "Could not verify".
	Challenge *rcChallengeResult
}

// rcCoverage is the machine-written record of what a review actually did:
// which files it opened, over how many turns, in how long.
//
// It exists because the reviewer's own "Scope:" paragraph is model-authored
// prose, and prose can be wrong about itself — reviews on kai-desktop#288 and
// #300 named the repository they were reading as kai-engine and kai-server.
// A reader cannot tell a confident clean verdict that read everything from a
// confident clean verdict that read two files, and neither can we. This is the
// half of the answer that cannot hallucinate.
//
// These facts were already gathered on every grounded run and thrown away
// unless the run died (see rcIncomplete). Now they always ship.
type rcCoverage struct {
	FilesRead []string `json:"filesRead,omitempty"`
	Turns     int      `json:"turns,omitempty"`
	Seconds   int      `json:"seconds,omitempty"`
}

// rcCoverageOf converts the run's own account of itself into the bundle's
// coverage record. Nil in, nil out: the fast pass makes one call over the diff
// and opens nothing, so it has no manifest to publish and omitempty drops the
// field entirely rather than claiming it read zero files.
func rcCoverageOf(inc *rcIncomplete) *rcCoverage {
	if inc == nil {
		return nil
	}
	return &rcCoverage{
		FilesRead: inc.FilesRead,
		Turns:     inc.Turns,
		Seconds:   int(inc.Elapsed.Round(time.Second).Seconds()),
	}
}

// rcFilesRead pulls the distinct FILES the run actually opened out of its tool
// calls. Deliberately cheap and schema-loose, like rcChangedSymbols: any tool
// that names a file names it in a "path" or "file_path" field, and a missed one
// only shortens a list that is already a courtesy.
//
// Directories are dropped, and root is how. The same "path" argument that a
// file read uses is also what a directory listing passes, so the manifest
// published "What I opened — 6 files" over a list whose first two entries were
// `frontend` and `frontend/dist` (kai-desktop#314), and "4 files" over one
// containing `cmd/kai` (kai-cli#99). A coverage report that miscounts its own
// coverage is the one thing this feature cannot afford.
//
// Only a CONFIRMED directory is dropped. A path that does not resolve — an
// absolute path from another tree, a bare name, anything stat cannot answer —
// is kept, because the cost is asymmetric: an extra entry slightly overstates
// what was read, while a wrongly dropped one makes changedFilesNotListed
// accuse the review of skipping a file it actually opened.
func rcFilesRead(transcript []message.Message, root string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range transcript {
		for _, pt := range m.Parts {
			tc, ok := pt.(message.ToolCall)
			if !ok {
				continue
			}
			var args struct {
				FilePath string `json:"file_path"`
				Path     string `json:"path"`
			}
			if json.Unmarshal([]byte(tc.Input), &args) != nil {
				continue
			}
			for _, p := range []string{args.FilePath, args.Path} {
				if p == "" || seen[p] {
					continue
				}
				seen[p] = true
				if rcIsDir(root, p) {
					continue
				}
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// rcIsDir reports whether p names a directory in the reviewed checkout.
//
// Answers only what the filesystem confirms: a stat that fails, for any reason,
// is not a directory as far as this is concerned. See rcFilesRead for why the
// uncertain case keeps the entry rather than dropping it.
func rcIsDir(root, p string) bool {
	full := p
	if !filepath.IsAbs(full) {
		if root == "" {
			return false
		}
		full = filepath.Join(root, p)
	}
	fi, err := os.Stat(full)
	return err == nil && fi.IsDir()
}

// rcIncompleteProse is the review of last resort: not a review at all, but an
// honest account of a run that read code and ran out of road. It exists so the
// finding still carries something a human can act on — how long it ran, why it
// stopped, and what it had opened when it did — instead of the PR being told
// the review could not be started.
//
// It never guesses a verdict. The caller leaves match and readiness Unknown,
// which is the one value that means "I have no opinion" rather than any point
// on the merge/do-not-merge scale.
func rcIncompleteProse(inc *rcIncomplete) string {
	if inc == nil {
		return ""
	}
	if inc.ChallengeFailure != "" {
		return "**This review did not finish.** The draft's defect claims could not be checked before publication. " +
			"The unchecked draft has been withheld; this is not an approval or a verdict on the change. Re-run the review."
	}
	reason := "the run ended without writing its review down"
	if inc.FinishReason == string(message.FinishReasonTimeBudget) {
		reason = "the review ran out of time before it could write its conclusion"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**This review did not finish.** After %s and %d turns, %s, and the follow-up request for a conclusion from the transcript also failed.\n\n",
		inc.Elapsed.Round(time.Second), inc.Turns, reason)
	if n := len(inc.FilesRead); n > 0 {
		const cap = 20
		shown := inc.FilesRead
		more := ""
		if n > cap {
			shown = shown[:cap]
			more = fmt.Sprintf("\n- …and %d more", n-cap)
		}
		fmt.Fprintf(&b, "It had opened %d file(s) before it stopped:\n\n- %s%s\n\n", n, strings.Join(shown, "\n- "), more)
	} else {
		b.WriteString("It had not opened any files before it stopped.\n\n")
	}
	b.WriteString("Nothing here is a verdict on the change: no claim was checked to completion, " +
		"so treat this as \"not reviewed\" rather than \"reviewed and clean\". Re-run the review, " +
		"or split the change into smaller pieces if it keeps exhausting the budget.")
	return b.String()
}

// rcConcludeFromTranscript makes one non-tool completion over the review
// run's message history, demanding the final write-up. Best-effort: any
// failure returns "" and the caller keeps whatever the run produced. The
// second result is rcFailureCategory's label for why it returned "".
func rcConcludeFromTranscript(ctx context.Context, prov provider.Provider, model string, transcript []message.Message) (string, string) {
	if len(transcript) == 0 {
		return "", "no_transcript"
	}
	model = rcStageModel(rcStageConclusion, model)
	// Preserve the actual evidence. Prefix trimming can remove the changed
	// function while retaining the model's allegation about it. Desktop #418's
	// incorrect shell review went through this fallback.
	// If full evidence cannot fit, leave this review incomplete.
	encoded, err := json.Marshal(transcript)
	if err != nil || len(encoded) > rcEvidenceLimit {
		fmt.Fprintln(os.Stderr, "  conclusion evidence is too large; refusing to discard source material")
		return "", "evidence_too_large"
	}
	msgs := append(append([]message.Message(nil), transcript...), message.Message{
		Role: message.RoleUser,
		Parts: []message.ContentPart{message.TextContent{Text: "Finish the review using only the evidence already present. " +
			"Do not invent runtime behavior or promote a suspicion into a defect to finish the task. " +
			"State unresolved questions as limitations. Output the human review and a complete " +
			rcReviewDataMarker + " coda with INTENT_MATCH, MERGE_READY, SUMMARY, ISSUES and DECISIONS."}},
	})
	// The conclusion is a deliberate grace period BEYOND the run, so it gets
	// a FRESH deadline — hanging it off the run's context handed it whatever
	// scraps remained of the 12-minute hard deadline, which after a 9-minute
	// review was not enough for one completion (the PR#90 failure).
	send := func(m []message.Message) (provider.Response, error) {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		return prov.Send(cctx, provider.Request{
			Model:           model,
			System:          rcReviewSystem,
			MaxTokens:       rcTokensFor(2500, rcStageEffort(rcStageConclusion)),
			Messages:        m,
			ReasoningEffort: rcStageEffort(rcStageConclusion),
		})
	}
	resp, err := send(msgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  conclusion call failed: %v\n", err)
		return "", rcFailureCategory(err)
	}
	var out strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			out.WriteString(t.Text)
		}
	}
	if text := strings.TrimSpace(out.String()); text != "" {
		return text, ""
	}
	return "", "empty"
}

// rcChangedSymbols extracts top-level declarations the diff ADDS or touches,
// grouped by file, so the reviewer starts knowing the names this change
// introduces. Line-regex over the unified diff — deliberately cheap and
// language-loose (Go keywords + class/function for the frontend); a missed
// symbol costs nothing, the reviewer just discovers it the old way.
func rcChangedSymbols(diff string) string {
	var b strings.Builder
	file := ""
	count := 0
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			file = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if file == "" || len(line) < 2 || line[0] != '+' || strings.HasPrefix(line, "+++") {
			continue
		}
		t := strings.TrimSpace(line[1:])
		var name string
		for _, kw := range []string{"func ", "type ", "class ", "function "} {
			if strings.HasPrefix(t, kw) {
				rest := t[len(kw):]
				// Method receivers: skip "(r *T) " to the method name.
				if strings.HasPrefix(rest, "(") {
					if i := strings.Index(rest, ")"); i >= 0 {
						rest = strings.TrimSpace(rest[i+1:])
					}
				}
				name = rest
				for i, c := range name {
					if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
						name = name[:i]
						break
					}
				}
				break
			}
		}
		if name == "" || seen[file+"|"+name] {
			continue
		}
		seen[file+"|"+name] = true
		fmt.Fprintf(&b, "- %s: %s\n", file, name)
		count++
		if count >= 40 {
			b.WriteString("- … (more omitted)\n")
			break
		}
	}
	return b.String()
}

// rcErrIncompleteReview is returned AFTER an incomplete-review finding has
// already been written to stdout. It exists so the exit code still says "this
// review did not finish" while the bundle that says so is deliverable — the
// two halves of a graceful failure.
var rcErrIncompleteReview = errors.New("review did not finish — an incomplete-review finding was emitted; re-run the review")

// rcReviewDataMarker separates the human review (everything before it) from
// the machine coda (INTENT_MATCH / MERGE_READY / SUMMARY / ISSUES) the
// pipeline parses.
const rcReviewDataMarker = "===REVIEW-DATA==="

var rcIntentVerdicts = map[string]finding.Match{
	"verified": finding.MatchVerified,
	"matches":  finding.MatchVerified,
	"partial":  finding.MatchPartial,
	"diverges": finding.MatchDiverges,
}

func rcMachineLine(line string) (key, value string, ok bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(line[:i]), "#*`")))
	if key == "" {
		return "", "", false
	}
	value = strings.TrimSpace(line[i+1:])
	if opener := rcMarkdownOpener(line); opener != "" {
		value = strings.TrimSpace(strings.TrimPrefix(value, opener))
		value = strings.TrimSpace(strings.TrimSuffix(value, opener))
	}
	return key, value, true
}

func rcMarkdownOpener(line string) string {
	t := strings.TrimSpace(line)
	i := 0
	for i < len(t) && (t[i] == '*' || t[i] == '`') {
		i++
	}
	return t[:i]
}

func rcUnwrapMachineBullet(line string) string {
	t := strings.TrimSpace(line)
	if opener := rcMarkdownOpener(t); opener != "" {
		t = strings.TrimSpace(strings.TrimPrefix(t, opener))
		t = strings.TrimSpace(strings.TrimSuffix(t, opener))
	}
	return t
}

// rcIntentVerdict reads only the first word after INTENT_MATCH. This accepts a
// trailing period or explanation without turning "not verified" into verified.
func rcIntentVerdict(value string) (finding.Match, bool) {
	words := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	if len(words) == 0 {
		return finding.MatchUnknown, false
	}
	m, ok := rcIntentVerdicts[words[0]]
	return m, ok
}

// rcParseReviewOutput splits the reviewer's output into the human review prose
// and the structured fields the finding carries. Tolerant of the legacy shape
// (no marker; FINDINGS:/NOTE: lines inline) so an old model answer still parses.
func rcParseReviewOutput(raw string) (prose string, risks, decisions []string, match finding.Match, readiness finding.Readiness, note string) {
	match = finding.MatchUnknown
	readiness = finding.ReadinessUnknown
	var statedMatch finding.Match
	matchConflict := false
	coda := raw
	if i := strings.Index(raw, rcReviewDataMarker); i >= 0 {
		prose = strings.TrimSpace(raw[:i])
		coda = raw[i+len(rcReviewDataMarker):]
	}
	// section tracks which bulleted list the parser is inside: "issues" (a
	// defect) or "decisions" (a correct change that still needs a human's
	// yes). Empty means neither, so a stray bullet outside a list header is
	// ignored rather than silently filed as a defect.
	section := ""
	var proseEnd int // legacy: prose runs until the first machine line
	sawMachineLine := false
	seenIssuesHeader := false
	supersededBlock := false
	for _, line := range strings.Split(coda, "\n") {
		t := strings.TrimSpace(line)
		key, value, labelled := rcMachineLine(t)
		switch {
		case labelled && (key == "issues" || key == "findings"):
			// The reviewer sometimes quotes an example block before its real
			// closing block. A repeated header supersedes that earlier block as a
			// unit; otherwise quoted risks, verdicts, and notes leak into the
			// finding. Do not reset on the first header, because legacy output may
			// put its verdict before FINDINGS.
			if seenIssuesHeader {
				supersededBlock = true
				risks = nil
				decisions = nil
				statedMatch = ""
				matchConflict = false
				readiness = finding.ReadinessUnknown
				note = ""
			}
			seenIssuesHeader = true
			section = "issues"
			sawMachineLine = true
		case labelled && key == "decisions":
			section = "decisions"
			sawMachineLine = true
		case labelled && key == "intent_match":
			section = ""
			sawMachineLine = true
			if m, ok := rcIntentVerdict(value); ok {
				if statedMatch != "" && statedMatch != m {
					matchConflict = true
				}
				statedMatch = m
			}
		case labelled && key == "merge_ready":
			section = ""
			sawMachineLine = true
			if r, ok := rcParseReadinessLine(t); ok {
				readiness = r
			}
		case labelled && key == "summary":
			section = ""
			sawMachineLine = true
			note = value
		case labelled && key == "note": // legacy spelling
			section = ""
			sawMachineLine = true
			note = value
		case section != "" && strings.HasPrefix(rcUnwrapMachineBullet(t), "-"):
			// "- (none)" under a header the model was told to omit when
			// empty is an empty list, not a finding (kai-server
			// rc-d93f2dc3595c38e0 shipped one as a verified claim).
			bullet := rcUnwrapMachineBullet(t)
			if item := strings.TrimSpace(strings.TrimPrefix(bullet, "-")); item != "" && !rcIsEmptyListItem(item) {
				if section == "decisions" {
					decisions = append(decisions, item)
				} else {
					risks = append(risks, item)
				}
			}
		default:
			if !sawMachineLine {
				proseEnd += len(line) + 1
			}
		}
	}
	if !matchConflict && statedMatch != "" {
		match = statedMatch
	}
	// Legacy shape: no marker — whatever preceded the first machine line is
	// the review prose (may be empty for the old strict-block output). The
	// clamp covers the final line's missing trailing newline.
	if prose == "" && proseEnd > 0 {
		if proseEnd > len(coda) {
			proseEnd = len(coda)
		}
		prose = strings.TrimSpace(coda[:proseEnd])
	}
	// The score, wherever the model actually put it.
	//
	// The coda is where it is ASKED for, and when the model obeys, the
	// loop above already has it. On 2026-09-06 it did not: the review of
	// kai-desktop#244 ended its prose with "**Merge readiness:** small
	// fixes first" and emitted no MERGE_READY line at all. The score was
	// right, the parser saw nothing, the bundle carried no readiness, and
	// the surface built to show it rendered nothing — the score was back
	// to being prose nobody downstream can read, which is the entire
	// failure the field exists to end.
	//
	// A prompt is a request. This is the part that does not depend on the
	// model choosing to comply.
	// Once a repeated findings header supersedes an earlier block, prose before
	// that block is not a safe fallback: it may be the quoted example's prose
	// readiness, which would restore the score we deliberately cleared above.
	if readiness == finding.ReadinessUnknown && !supersededBlock {
		for _, line := range strings.Split(prose, "\n") {
			if r, ok := rcParseReadinessLine(strings.TrimSpace(line)); ok {
				readiness = r
				break
			}
		}
	}
	return prose, risks, decisions, match, readiness, note
}

// rcReadinessLabels maps the canonical phrase for each score back to the
// score, for a model that answered in words instead of the digit. The
// phrases are finding.Readiness.Label()'s, which is what the prompt shows
// the model, so this recognizes the vocabulary the reviewer was taught.
//
// Longest first: "ready to merge" is a suffix of nothing here, but "needs
// work" and "do not merge" both appear inside longer sentences, and an
// anchored longest-match keeps "your call, then merge" from reading as a
// merge.
var rcReadinessLabels = []struct {
	phrase string
	score  finding.Readiness
}{
	{"your call, then merge", finding.ReadinessDecideThenMerge},
	{"your call then merge", finding.ReadinessDecideThenMerge},
	{"small fixes first", finding.ReadinessSmallFixes},
	{"ready to merge", finding.ReadinessMerge},
	{"do not merge", finding.ReadinessBlocked},
	{"needs work", finding.ReadinessNeedsWork},
}

// rcReadinessKeys are the ways a line can announce the score, in the
// canonical form rcReadinessKey produces (letters only). Only a LABELLED
// line counts: the reviewer says "ready to merge" in ordinary prose all
// the time, and reading that as a 5 would invent a verdict nobody gave —
// the same mistake as dropping one, pointed the other way.
var rcReadinessKeys = map[string]bool{
	"mergeready":     true, // the machine coda: MERGE_READY
	"mergereadiness": true, // what the model writes in prose
	"readiness":      true,
}

// rcReadinessKey reduces a line's key to letters, so MERGE_READY,
// "**Merge readiness**", "### Merge-readiness" and "merge ready" all
// land on the same string. Underscores, spaces, hyphens and markdown are
// decoration around the word; only the letters carry the meaning.
func rcReadinessKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rcParseReadinessLine reads a score off one line, in either the machine
// form ("MERGE_READY: 3") or the prose form the model actually writes
// ("**Merge readiness:** small fixes first — ..."). Reports whether it
// found one.
//
// An unparseable or out-of-range value yields nothing rather than a guess
// at an end of the scale: "the reviewer did not say" and "do not merge"
// are different claims, and a missing score must never render as the
// harsher one.
func rcParseReadinessLine(line string) (finding.Readiness, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return finding.ReadinessUnknown, false
	}
	if !rcReadinessKeys[rcReadinessKey(line[:i])] {
		return finding.ReadinessUnknown, false
	}
	// Markdown is decoration around the value, never part of it.
	value := strings.TrimSpace(strings.NewReplacer("*", "", "`", "", "#", "").Replace(line[i+1:]))
	if value == "" {
		return finding.ReadinessUnknown, false
	}
	// The digit, when the model gave one. First field only: anything
	// after it ("4 — pending your call") is commentary.
	if f := strings.Fields(value); len(f) > 0 {
		if n, err := strconv.Atoi(strings.TrimRight(f[0], ".:,")); err == nil {
			if r := finding.Readiness(n); r.Valid() {
				return r, true
			}
			return finding.ReadinessUnknown, false
		}
	}
	// Otherwise the words, anchored at the start so a label named later
	// in a sentence about something else cannot win.
	lower := strings.ToLower(value)
	for _, l := range rcReadinessLabels {
		if strings.HasPrefix(lower, l.phrase) {
			return l.score, true
		}
	}
	return finding.ReadinessUnknown, false
}

func rcCommitMeta(ref string) (hash, subject, body string, err error) {
	h, e := exec.Command("git", "rev-parse", ref).Output()
	if e != nil {
		return "", "", "", fmt.Errorf("resolve commit %q: %w", ref, e)
	}
	hash = strings.TrimSpace(string(h))
	s, _ := exec.Command("git", "log", "-1", "--format=%s", hash).Output()
	b, _ := exec.Command("git", "log", "-1", "--format=%b", hash).Output()
	return hash, strings.TrimSpace(string(s)), strings.TrimSpace(string(b)), nil
}

func rcCommitDiff(base, ref string, maxBytes int) string {
	var out []byte
	var err error
	if strings.TrimSpace(base) != "" {
		out, err = exec.Command("git", "--no-pager", "diff", "--no-color", base+"..."+ref).Output()
	} else {
		out, err = exec.Command("git", "--no-pager", "show", "--no-color", "--format=", ref).Output()
	}
	if err != nil || len(out) == 0 {
		return ""
	}
	if len(out) > maxBytes {
		return string(out[:maxBytes]) + "\n... (diff truncated)\n"
	}
	return string(out)
}

func rcCommitDiffStat(base, ref string) (added, removed int, files []finding.DiffFile) {
	var out []byte
	var err error
	if strings.TrimSpace(base) != "" {
		out, err = exec.Command("git", "--no-pager", "diff", "--numstat", base+"..."+ref).Output()
	} else {
		out, err = exec.Command("git", "--no-pager", "show", "--numstat", "--format=", ref).Output()
	}
	if err != nil {
		return 0, 0, nil
	}
	// Also capture the actual unified diff hunks per file so the findings inbox
	// Code tab renders real added/removed lines, not just counts. numstat alone
	// gave us Added/Removed but left DiffFile.Patch empty (blank Code tab).
	patches := rcCommitPatches(base, ref)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		a, _ := strconv.Atoi(fields[0])
		r, _ := strconv.Atoi(fields[1])
		added += a
		removed += r
		path := fields[2]
		files = append(files, finding.DiffFile{Path: path, Action: "modified", Added: a, Removed: r, Patch: patches[path]})
	}
	return added, removed, files
}

// rcCommitPatches returns the unified-diff hunks for each changed file over the
// same base...ref range rcCommitDiffStat counts, keyed by (b-side) path. File
// headers (diff --git, index, ---/+++, mode/rename lines) are stripped; hunk
// headers (@@) and the +/-/context body are kept so the inbox can colorize them.
func rcCommitPatches(base, ref string) map[string]string {
	var out []byte
	var err error
	if strings.TrimSpace(base) != "" {
		out, err = exec.Command("git", "--no-pager", "diff", base+"..."+ref).Output()
	} else {
		out, err = exec.Command("git", "--no-pager", "show", "--format=", ref).Output()
	}
	if err != nil {
		return nil
	}
	patches := map[string]string{}
	var curPath string
	var buf strings.Builder
	flush := func() {
		if curPath != "" {
			patches[curPath] = strings.TrimRight(buf.String(), "\n")
		}
		buf.Reset()
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			f := strings.Fields(line)
			curPath = ""
			if len(f) >= 4 {
				curPath = strings.TrimPrefix(f[len(f)-1], "b/")
			}
			continue
		}
		if curPath == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "--- "),
			strings.HasPrefix(line, "+++ "),
			strings.HasPrefix(line, "new file mode "),
			strings.HasPrefix(line, "deleted file mode "),
			strings.HasPrefix(line, "old mode "),
			strings.HasPrefix(line, "new mode "),
			strings.HasPrefix(line, "similarity index "),
			strings.HasPrefix(line, "rename "),
			strings.HasPrefix(line, "Binary files "):
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()
	return patches
}

func rcCommitAuthor(hash string) string {
	out, _ := exec.Command("git", "log", "-1", "--format=%an", hash).Output()
	return strings.TrimSpace(string(out))
}

func rcParentHash(hash string) string {
	out, err := exec.Command("git", "rev-parse", hash+"^").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func rcFindingID(hash string) string {
	if len(hash) >= 16 {
		return "rc-" + hash[:16]
	}
	return "rc-" + hash
}

func rcShort(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func rcOneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
