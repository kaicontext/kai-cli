package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// rcReviewSystem layers review-commit's specifics UNDER the harness's review
// personality — the runner prepends agent.ModeReview's system prompt ahead of
// this text — so this only carries what's specific to reviewing a merged
// commit: the grounding discipline, the defect sweep, the decisions sweep, and
// the output contract.
// The deliverable is a human review (prose the author can actually read),
// closed by a machine coda the findings pipeline parses (rcParseReviewOutput).
const rcReviewSystem = `You are reviewing a merged commit. You get the author's own description (AUTHOR CONTEXT), the reconstructed INTENT, and the DIFF; the codebase itself is reachable through your tools. You cannot edit anything and there is no one to ask questions — the review is your whole output.

Ground every claim before you make it. Confirm a suspicion with the graph (kai_callers / kai_dependents on the changed symbols, kai_context to understand one) or by reading the file — a changed signature whose callers were not updated is a defect; so are data races and state mutated outside its lock, resource leaks (goroutines, tickers, files, connections that are never stopped or closed), off-by-one and nil-dereference bugs, swallowed or misrouted errors, and missing validation on inputs. For anything that touches a secret — API key, token, password, session id, HMAC or signature — a plain ==/!= comparison is a timing side channel and must be a constant-time compare (subtle.ConstantTimeCompare / hmac.Equal); check every comparison you can see, and give authentication, authorization, and admin/override paths a specifically suspicious read. Walk the whole diff for each of these before concluding; don't stop at the first thing you find. A finding you would hedge ("could be wrong", "if X implements Y…", "couldn't verify") is not a finding — confirm it or drop it.

GROUND EXTERNAL FACTS TOO — THE REPO CANNOT CONFIRM THEM. Your grounding tools answer questions about THIS codebase. They cannot confirm a claim about the outside world: a third party's fee, rate, or price; an API's actual contract; a spec's required field; a library's current behavior. When the diff (or its comment, or the PR description) asserts such a number and the code's correctness depends on that number being right, the assertion is UNVERIFIED — treat it exactly as you would an unverified claim about a caller. Run ONE kai_web_search before you endorse it, then either cite what you found or say plainly that you could not confirm it. Repeating the author's premise back in your own voice ("the math is correct: $1.00 of credits costs us $1.05") is not review — it launders their assumption into your verdict. kaicontext/kai-server#126 (2026-08-31) shipped a 5% surcharge on a rate nobody checked; the reviewer had kai_web_search in its tool list and never called it.

AN ALL-CLEAR NEEDS THE SAME EVIDENCE AS A CONCERN. Confirm-it-or-drop-it cuts both ways, and the reassuring direction is the one that ships bugs. "Only ever", "never", "always", "nothing else reaches this" are universal claims, and a search that came back empty inside ONE repo does not establish one. Before writing a universal, name the boundary you actually searched and put that boundary in the sentence: "within this repo, the only caller is X" is honest; "X is the only caller" is not, when another repo, another binary, or a client you cannot see also calls it. If the change's correctness rests on something outside your reach, that IS a finding — say what you could not see and what breaks if it is false. Silence about a limit reads as coverage.

SOME THINGS ARE CORRECT AND STILL NEED A HUMAN. A change can be flawless as code and still be a decision the author may not have realized they were making — usually because the PR describes it in a narrower frame than it acts in. Follow the changed values outward until you reach something that CHARGES a customer, LIMITS one (a quota, cap, or rate limit), SENDS or PUBLISHES on their behalf, DELETES, or changes who can access what. If a changed number reaches any of those, report it as a DECISION even when every line is right and the intent matches. "The 5% flows consistently into the daily counter, monthly overflow, credit drawdown, and per-run record" is not a note about internal consistency — it is the sentence "this debits every customer's prepaid balance 5% more", and it belongs in DECISIONS, not in the paragraph reassuring the author that nothing is wrong. Do not weigh whether the decision is a good one: name it, name who it affects, and hand it back.

MONEY HAS A DIRECTION. When a change moves money or credits, say in words who is debited and who is credited, and name the function that moves it — a grant adds to a balance, a drawdown subtracts from one. Read the function, not its name. A decision handed back with the direction inverted ("this draws against the referrer's balance" when it pays them) sends the author to confirm the wrong thing, and it happened (kai-server, 2026-09-02).

SIBLING PATHS SHARE GUARDS. When the diff adds a branch beside an existing one that does the same kind of work — a second case in a webhook switch, a second checkout path, a second handler for the same event — read the older branch's guards and list each one the new branch lacks: a payment-status check, an idempotency key, an auth check, a size limit. A guard the author already wrote once and did not carry over is a defect, not a style nit.

A TEST THAT PASSES ON THE UNFIXED CODE IS NOT A TEST. When a change claims to fix a bug, the question is never whether a test exists or whether it passes — it is whether it FAILS with the fix removed. If nothing in the diff would fail on the old code, the fix is unverified and that is a finding; name the test you looked at and what it would have to assert to catch the bug. Three shapes that look like coverage and are not. A test that calls the thing and discards the answer — one shipped as the test for a new return value, assigning it to a blank and immediately discarding it, asserting nothing (kai-engine, 2026-09-03). A test that asserts the MECHANISM was configured instead of the BEHAVIOUR it was supposed to buy: v0.6.46 gave a shell-out a 5s context and called it bounded, but cancelling a context kills the child while the output read keeps blocking on a pipe a grandchild inherited — no test ever wedged a process, so the hang shipped and had to be fixed again in v0.6.47. And a test that SKIPS for an environmental reason on the machine that runs it, which is a test that runs nowhere and whose silence nobody notices.

THE ENVIRONMENT IS NOT CLEAN. Correct on the author's machine is not correct. Name what the change assumes about the world outside the process, and say what happens when the assumption is false — the failure mode is the finding, not the assumption. The five that keep recurring:
- CONFIG READ FROM THE WRONG PLACE. Does it consult the environment for something whose real home is a config file? kai init guarded its git-identity fallback on the GIT_AUTHOR_NAME environment variable, but identity lives in gitconfig — the guard fired for nearly every user, and because the GIT_* environment OUTRANKS gitconfig, every baseline commit was authored "Kai <kai@local>" on repos whose owner had a perfectly good identity configured (kai-cli, 2026-09-03).
- SUBPROCESSES THAT NEVER RETURN. Every exec needs a deadline, and in Go a deadline alone is not one: WaitDelay is required too, or the output read blocks past the cancel on pipes a grandchild still holds.
- COST ON A HOT PATH. How often does this run? A per-repo shell-out reached from a 6s poll is nine subprocesses every six seconds on a nine-project workspace.
- A NICETY THAT CAN BE FATAL. An optimisation whose failure aborts the whole operation. A failed baseline commit failed kai init outright, and commit signing configured with no usable key is enough to cause it.
- WRITES NOBODY ASKED FOR. Does it touch git history, a dotfile, or anything outside its own state? Name it, and say whether there is an opt-out.

NEW DEFAULTS POINT SOMEWHERE. A new config default that is a URL, host, e-mail address, or path ships to every user who never sets the variable. Confirm the target exists and that something in this repo, or a repo you can see, serves it; a default pointing at a domain nobody here owns is a defect. (The pipeline also greps for hosts the change introduces that nothing else mentions and files them as risks; you still have to say whether the target is real.)

Then write the review the way a good colleague would leave it on the PR:
- Open with one line naming your scope: the repo and revision you read, plus anything the change obviously touches that you could NOT read (another repo, a client, a deployed config, a provider's behavior). Then a short paragraph: what the change actually does, and your overall take.
- Then each real concern, in plain language: where it is (path:line), what goes wrong, why it matters, and what you'd do instead. No category tags, no severity labels, no template — clear sentences addressed to the author.
- If the change is solid, say so plainly. A sentence on what's done well is welcome; flattery is not. Style nits are not concerns.

Close the prose with one line saying how ready this is to merge, in your own words, so the author reads your answer before the machinery does.

Finish with this machine coda, exactly once, after everything else. INTENT_MATCH judges the change against the author's ACTUAL goal, not a stricter one: verified = does what they intended; partial = mostly, with gaps; diverges = materially different or broken. A DECISION never lowers INTENT_MATCH — a change can be verified and still need a human's yes. Omit either list entirely when it is empty.

MERGE_READY answers what should happen to this branch NEXT. It is not a grade for the author, not a confidence score, and not a measure of how much you found. Score what is true of the code now:
  5 — merge it. No defects, and nothing here needs anyone's decision.
  4 — your call, then merge. No defects; something in it is a human's to decide (a tradeoff, a publish, a policy).
  3 — small fixes first. Real defects, but local and quick; the change itself is sound.
  2 — needs work. Defects in the core of what the change does.
  1 — do not merge. It does not do what it claims, or it breaks something that works today.
A DECISION never scores below 4, exactly as it never lowers INTENT_MATCH: a change nobody has objected to is not held back by needing a yes. A concern you could not verify is not a defect — say so and score what you did establish. Findings you raised and then judged not to be defects do not count against the score; if every concern turned out to be a decision or a non-issue, that is a 4 or a 5 and you should say so plainly.
===REVIEW-DATA===
INTENT_MATCH: verified|partial|diverges
MERGE_READY: 1|2|3|4|5
SUMMARY: <one honest sentence — your bottom line>
ISSUES:
- path:line — <one-sentence version of each concern from your review>
DECISIONS:
- <what the author is deciding, who it affects, and the consequence — no path:line; it is not a defect>`

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
// The CI step's own timeout is 30 minutes, so this stays well inside it.
const (
	rcReviewSoftBudget    = 9 * time.Minute
	rcReviewSoftExtension = 3 * time.Minute // granted at most twice → 15m ceiling
	rcReviewHardDeadline  = 20 * time.Minute
)

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
	authorContext := rcAuthorContext(subject, body, isMerge, rangeSubjects, rangeBodies)
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

	prov, model, provKind := rcReviewProvider()
	if prov == nil {
		return fmt.Errorf("no LLM provider available (run `kai login`)")
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
	if fast {
		fastModel := rcFastModel(model, provKind)
		fmt.Fprintf(os.Stderr, "  fast pass: one call over the diff, no graph (model %s, budget %s)…\n",
			fastModel, rcFastHardDeadline)
		phase := time.Now()
		raw, err = rcRunFastReview(ctx, prov, fastModel, repoRoot, authorContext, stated, intentBody, diff, changedPaths)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  timing: fast-review=%s\n", time.Since(phase).Round(time.Second))
	} else {
		fmt.Fprintf(os.Stderr, "  reconstructing intent (model %s)…\n", model)
		phase := time.Now()
		intent, ierr := rcInferIntent(ctx, prov, model, stated, intentBody, diff)
		if ierr != nil {
			return fmt.Errorf("infer intent: %w", ierr)
		}
		fmt.Fprintf(os.Stderr, "  timing: intent=%s\n", time.Since(phase).Round(time.Second))

		fmt.Fprintf(os.Stderr, "  reviewing against the graph…\n\n")
		phase = time.Now()
		raw, inc, err = rcRunReviewAgent(ctx, set, prov, model, authorContext, intent, diff)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  timing: review=%s\n", time.Since(phase).Round(time.Second))
	}

	prose, risks, decisions, match, readiness, note := rcParseReviewOutput(raw)
	if fast {
		readiness = rcCapFastReadiness(readiness)
		risks = rcFilterFastIssues(risks)
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
	// it said). A DECISION is grounded by construction. URL hosts the change
	// introduces that nothing else in the tree mentions are added as risks
	// of their own — with the grep as their lookup — and join Intent.Risks so
	// the intent panel shows them too. The inbox denormalizes RiskCount from
	// grounded risk-tagged claims, so held claims are visible but do not
	// count.
	claims := make([]finding.Claim, 0, len(flags))
	tree := rcTreeFiles(hash)
	for _, r := range risks {
		claims = append(claims, rcGroundIssue(hash, r, tree, rcFileLines))
	}
	for _, d := range decisions {
		claims = append(claims, rcDecisionClaim(d))
	}
	for _, c := range rcNewHostClaims(hash, diff, changedPaths, rcFilesMentioningHost) {
		claims = append(claims, c)
		flags = append(flags, c.Statement)
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
		}{f, prose, depth, incomplete, rcCoverageOf(inc)}, "", "  ")
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
		Model:     model,
		System:    rcInferIntentSystem,
		MaxTokens: 600,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: in.String()}}}},
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
func rcRunReviewAgent(ctx context.Context, set *projects.Set, prov provider.Provider, model, sourceContext, intent, diff string) (string, *rcIncomplete, error) {
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
		System:     rcReviewSystem,
		ReadOnly:   true,
		EnableBash: false,
		MaxTurns:   20,
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
		Hooks: agent.Hooks{
			OnToolCall: func(name, inputJSON string) {
				fmt.Fprintf(os.Stderr, "  → %s %s\n", name, rcOneLine(inputJSON, 90))
			},
		},
	}
	// Effort tier LAST, after every deliberate field above — ApplyEffort only
	// tightens. Zero-value Speed resolves KAI_SPEED → thorough (a no-op).
	agent.ApplyEffort(&opts, 0)

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
		FinishReason: string(res.FinishReason),
		Elapsed:      time.Since(started),
		Turns:        len(res.Transcript),
		FilesRead:    rcFilesRead(res.Transcript),
	}
	raw := strings.TrimSpace(res.FinalText)
	// A run that ran out of road — the soft time budget fired, or the loop
	// ended without ever emitting the structured coda — has read the code
	// but never wrote the review down. Don't ship that as an empty finding:
	// make ONE direct conclusion call over the run's own transcript, forcing
	// the write-down from what was already seen. (PR#89 dogfood: 5m38s of
	// healthy exploration, budget expiry at a turn boundary, hollow finding
	// posted as success.)
	if res.FinishReason == message.FinishReasonTimeBudget || !strings.Contains(raw, rcReviewDataMarker) {
		fmt.Fprintf(os.Stderr, "  review ended without a conclusion (finish=%s) — requesting one from the transcript…\n", res.FinishReason)
		if concluded := rcConcludeFromTranscript(ctx, prov, model, res.Transcript); concluded != "" {
			raw = concluded
		}
	}
	return raw, inc, nil
}

// rcConclusionTailMessages bounds the retry's prompt when a conclusion over
// the whole transcript blew its deadline. Big enough to hold the run's late
// reasoning (where its actual findings are), small enough that the second call
// is cheap. The first message is always kept: it carries the review task.
const rcConclusionTailMessages = 40

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
	FinishReason string
	Elapsed      time.Duration
	Turns        int
	FilesRead    []string
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

// rcFilesRead pulls the distinct paths the run actually opened out of its tool
// calls. Deliberately cheap and schema-loose, like rcChangedSymbols: any tool
// that names a file names it in a "path" or "file_path" field, and a missed one
// only shortens a list that is already a courtesy.
func rcFilesRead(transcript []message.Message) []string {
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
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
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
// failure returns "" and the caller keeps whatever the run produced.
func rcConcludeFromTranscript(ctx context.Context, prov provider.Provider, model string, transcript []message.Message) string {
	if len(transcript) == 0 {
		return ""
	}
	// Trim tool-result bodies: the conclusion needs the run's reasoning and
	// what it read, not every full file dump — a 12-file review's verbatim
	// transcript pushed the single call past its deadline on a slow provider
	// hour (PR#90 retrigger, 2026-08-26).
	trimmed := make([]message.Message, 0, len(transcript))
	for _, m := range transcript {
		parts := make([]message.ContentPart, 0, len(m.Parts))
		for _, pt := range m.Parts {
			if tr, ok := pt.(message.ToolResult); ok && len(tr.Content) > 2000 {
				tr.Content = tr.Content[:2000] + "\n… (tool result trimmed for the conclusion call)"
				parts = append(parts, tr)
				continue
			}
			parts = append(parts, pt)
		}
		m.Parts = parts
		trimmed = append(trimmed, m)
	}
	msgs := append(trimmed, message.Message{
		Role: message.RoleUser,
		Parts: []message.ContentPart{message.TextContent{Text: "Your review time is up. Write the review NOW from what you have " +
			"already read — no more tool calls, no more exploration. Output the human review prose, then the line " +
			rcReviewDataMarker + " followed by INTENT_MATCH: (verified|partial|diverges), SUMMARY:, and ISSUES: " +
			"with one line per concrete defect (empty ISSUES: section if none). If you saw too little to judge some part, " +
			"say so explicitly in the prose rather than omitting the review."}},
	})
	// The conclusion is a deliberate grace period BEYOND the run, so it gets
	// a FRESH deadline — hanging it off the run's context handed it whatever
	// scraps remained of the 12-minute hard deadline, which after a 9-minute
	// review was not enough for one completion (the PR#90 failure).
	_ = ctx
	send := func(m []message.Message) (provider.Response, error) {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		return prov.Send(cctx, provider.Request{
			Model:     model,
			System:    rcReviewSystem,
			MaxTokens: 2500,
			Messages:  m,
		})
	}
	resp, err := send(msgs)
	if err != nil {
		// One completion over a long review's history can itself exceed the
		// grace period — kai-server#184 (2026-09-08) reviewed a 39-file diff
		// for 12m7s and the conclusion call died with "context deadline
		// exceeded", losing everything. Trimming tool-result BODIES was not
		// enough there: the message COUNT is the cost. Retry over the tail,
		// which is where the run's conclusions live anyway; a short prompt has
		// a real chance inside the same 3 minutes, and the alternative is not
		// a slower answer but no answer.
		fmt.Fprintf(os.Stderr, "  conclusion call failed: %v\n", err)
		if len(msgs) > rcConclusionTailMessages {
			tail := append([]message.Message{msgs[0]}, msgs[len(msgs)-rcConclusionTailMessages:]...)
			fmt.Fprintf(os.Stderr, "  retrying the conclusion over the last %d of %d messages…\n", rcConclusionTailMessages, len(msgs))
			if resp, err = send(tail); err != nil {
				fmt.Fprintf(os.Stderr, "  conclusion retry failed: %v\n", err)
				return ""
			}
		} else {
			return ""
		}
	}
	var out strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			out.WriteString(t.Text)
		}
	}
	return strings.TrimSpace(out.String())
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
