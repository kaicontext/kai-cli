package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kai/internal/config"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/finding"
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

NEW DEFAULTS POINT SOMEWHERE. A new config default that is a URL, host, e-mail address, or path ships to every user who never sets the variable. Confirm the target exists and that something in this repo, or a repo you can see, serves it; a default pointing at a domain nobody here owns is a defect. (The pipeline also greps for hosts the change introduces that nothing else mentions and files them as risks; you still have to say whether the target is real.)

Then write the review the way a good colleague would leave it on the PR:
- Open with one line naming your scope: the repo and revision you read, plus anything the change obviously touches that you could NOT read (another repo, a client, a deployed config, a provider's behavior). Then a short paragraph: what the change actually does, and your overall take.
- Then each real concern, in plain language: where it is (path:line), what goes wrong, why it matters, and what you'd do instead. No category tags, no severity labels, no template — clear sentences addressed to the author.
- If the change is solid, say so plainly. A sentence on what's done well is welcome; flattery is not. Style nits are not concerns.

Finish with this machine coda, exactly once, after everything else. INTENT_MATCH judges the change against the author's ACTUAL goal, not a stricter one: verified = does what they intended; partial = mostly, with gaps; diverges = materially different or broken. A DECISION never lowers INTENT_MATCH — a change can be verified and still need a human's yes. Omit either list entirely when it is empty.
===REVIEW-DATA===
INTENT_MATCH: verified|partial|diverges
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
		"This is what the CI review workflow runs. Requires a captured graph (`kai capture`).",
	Args: cobra.ExactArgs(1),
	RunE: runReviewCommit,
}

func init() {
	reviewCommitCmd.Flags().StringVar(&reviewCommitFormat, "format", "text", "output format: text|json")
	reviewCommitCmd.Flags().StringVar(&reviewCommitBase, "base", "", "review the aggregate diff of <base>...<commit> (PR range) instead of a single commit")
	reviewCommitCmd.Flags().StringVar(&reviewCommitBranch, "branch", "", "branch name to record on the finding (default: GITHUB_HEAD_REF / GITHUB_REF_NAME / the checked-out branch)")
	rootCmd.AddCommand(reviewCommitCmd)
}

func runReviewCommit(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ref := args[0]
	cwd, _ := os.Getwd()

	set, outcome := projects.Discover(cwd)
	if outcome != projects.OutcomeRootsFound {
		return fmt.Errorf("not a kai project here — run `kai capture` first")
	}
	if err := set.Open(); err != nil {
		return fmt.Errorf("opening projects: %w", err)
	}
	defer set.Close()
	// Point config loads and the run log at the discovered project's kai dir
	// (main.go's default is a cwd resolve, which diverges in sub-directories).
	kaiDir = set.Primary().KaiDir

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

	prov, model := rcReviewProvider()
	if prov == nil {
		return fmt.Errorf("no LLM provider available (run `kai login`)")
	}

	fmt.Fprintf(os.Stderr, "kai review-commit %s · %s\n", rcShort(hash), subject)
	fmt.Fprintf(os.Stderr, "  reconstructing intent (model %s)…\n", model)
	phase := time.Now()
	intent, err := rcInferIntent(ctx, prov, model, stated, intentBody, diff)
	if err != nil {
		return fmt.Errorf("infer intent: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  timing: intent=%s\n", time.Since(phase).Round(time.Second))

	fmt.Fprintf(os.Stderr, "  reviewing against the graph…\n\n")
	phase = time.Now()
	raw, err := rcRunReviewAgent(ctx, set, prov, model, authorContext, intent, diff)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  timing: review=%s\n", time.Since(phase).Round(time.Second))

	prose, risks, decisions, match, note := rcParseReviewOutput(raw)

	// An empty review is a FAILURE, not a finding. Shipping a bundle with no
	// prose, no risks, and an unknown intent verdict green-checks a shell —
	// the CI job succeeds, the inbox shows nothing, and nobody learns the
	// review never happened (PRs #89/#90, 2026-08-26). Exit non-zero so the
	// job fails visibly and the intake's retry machinery gets its chance.
	if strings.TrimSpace(prose) == "" && len(risks) == 0 && len(decisions) == 0 && match == finding.MatchUnknown {
		return fmt.Errorf("review produced no content (no prose, no risks, intent unknown) — failing instead of posting an empty finding")
	}

	added, removed, files := rcCommitDiffStat(reviewCommitBase, ref)

	// Blast radius: walk the captured graph outward from the changed files so the
	// finding shows what the change reaches (callers/importers), not just the diff.
	// headHex="" walks the freshly-captured (single-snapshot) graph unscoped.
	// Non-fatal — blast is a panel, not the review itself.
	changedPaths := make([]string, 0, len(files))
	for _, df := range files {
		changedPaths = append(changedPaths, df.Path)
	}
	blast, berr := reviewanalyze.BlastFor(ctx, set.Primary().DB, changedPaths, "")
	if berr != nil {
		fmt.Fprintf(os.Stderr, "  blast radius unavailable: %v\n", berr)
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
		out, err := json.MarshalIndent(struct {
			finding.Finding
			Review string `json:"review,omitempty"`
		}{f, prose}, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling finding: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out))
		return nil
	}

	// Text mode: the review itself is the output. Fall back to the parsed
	// fields only when the model skipped the prose (legacy strict-block runs).
	if prose != "" {
		fmt.Println(prose)
		if note != "" {
			fmt.Printf("\nBottom line: %s\n", note)
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
	return nil
}

// rcReviewProvider builds the LLM provider (kailab creds → OpenRouter, or an
// ANTHROPIC_API_KEY fallback), reusing the gate/planner plumbing.
func rcReviewProvider() (provider.Provider, string) {
	cfg, err := config.Load(kaiDir)
	if err != nil {
		return nil, ""
	}
	prov, reviewModel, _, err := buildGateProvider(cfg)
	if err != nil {
		return nil, ""
	}
	return prov, reviewModel
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

// rcRunReviewAgent runs the review through the shared harness runner, set up
// the way the orchestrator sets up its executors: agent.ModeReview supplies
// the harness's review personality + read-only tool whitelist, the graph
// injection seeds turn 0 with real context, the session store and run log make
// the run inspectable (`kai run summary`), and ApplyEffort honors KAI_SPEED.
// rcReviewSystem rides in Options.System underneath the mode prompt.
func rcRunReviewAgent(ctx context.Context, set *projects.Set, prov provider.Provider, model, sourceContext, intent, diff string) (string, error) {
	primary := set.Primary()
	gdb := asGraphDB(primary.DB)

	var user strings.Builder
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
	if symbols := rcChangedSymbols(diff); symbols != "" {
		user.WriteString("CHANGED SYMBOLS (extracted from this diff — these are new or modified IN THIS CHANGE. ")
		user.WriteString("Do not search the graph for these names; open the listed files directly):\n")
		user.WriteString(symbols)
		user.WriteString("\n\n")
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

	res, err := agent.Run(ctx, opts)
	if err != nil {
		return "", fmt.Errorf("review run: %w", err)
	}
	fmt.Fprintln(os.Stderr)

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
	return raw, nil
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
	cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	resp, err := prov.Send(cctx, provider.Request{
		Model:     model,
		System:    rcReviewSystem,
		MaxTokens: 2500,
		Messages:  msgs,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  conclusion call failed: %v\n", err)
		return ""
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

// rcReviewDataMarker separates the human review (everything before it) from
// the machine coda (INTENT_MATCH / SUMMARY / ISSUES) the pipeline parses.
const rcReviewDataMarker = "===REVIEW-DATA==="

// rcParseReviewOutput splits the reviewer's output into the human review prose
// and the structured fields the finding carries. Tolerant of the legacy shape
// (no marker; FINDINGS:/NOTE: lines inline) so an old model answer still parses.
func rcParseReviewOutput(raw string) (prose string, risks, decisions []string, match finding.Match, note string) {
	match = finding.MatchUnknown
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
	for _, line := range strings.Split(coda, "\n") {
		t := strings.TrimSpace(line)
		upper := strings.ToUpper(t)
		switch {
		case strings.EqualFold(t, "ISSUES:") || strings.EqualFold(t, "FINDINGS:"):
			section = "issues"
			sawMachineLine = true
		case strings.EqualFold(t, "DECISIONS:"):
			section = "decisions"
			sawMachineLine = true
		case strings.HasPrefix(upper, "INTENT_MATCH:"):
			section = ""
			sawMachineLine = true
			// First field only — the value is one word; anything after
			// ("partial — see above") is commentary, not the verdict.
			v := strings.Fields(strings.ToLower(strings.TrimSpace(t[len("INTENT_MATCH:"):])))
			if len(v) > 0 {
				switch v[0] {
				case "verified":
					match = finding.MatchVerified
				case "partial":
					match = finding.MatchPartial
				case "diverges":
					match = finding.MatchDiverges
				}
			}
		case strings.HasPrefix(upper, "SUMMARY:"):
			section = ""
			sawMachineLine = true
			note = strings.TrimSpace(t[len("SUMMARY:"):])
		case strings.HasPrefix(upper, "NOTE:"): // legacy spelling
			section = ""
			sawMachineLine = true
			note = strings.TrimSpace(t[len("NOTE:"):])
		case section != "" && strings.HasPrefix(t, "-"):
			// "- (none)" under a header the model was told to omit when
			// empty is an empty list, not a finding (kai-server
			// rc-d93f2dc3595c38e0 shipped one as a verified claim).
			if item := strings.TrimSpace(strings.TrimPrefix(t, "-")); item != "" && !rcIsEmptyListItem(item) {
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
	// Legacy shape: no marker — whatever preceded the first machine line is
	// the review prose (may be empty for the old strict-block output). The
	// clamp covers the final line's missing trailing newline.
	if prose == "" && proseEnd > 0 {
		if proseEnd > len(coda) {
			proseEnd = len(coda)
		}
		prose = strings.TrimSpace(coda[:proseEnd])
	}
	return prose, risks, decisions, match, note
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
