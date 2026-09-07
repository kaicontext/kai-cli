package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// review-commit --fast is the SHALLOW first pass: one model call over the diff,
// no agent loop, no graph, no `kai capture`. It exists to land a review inside
// the ~2 minutes a PR author will actually wait, which the grounded reviewer
// cannot do and should not try to — its 9m soft budget buys the callers-checked
// claims that are the whole point.
//
// What makes this more than a diff dump is that the two cheapest grounding
// inputs the slow path builds are GIT-based, not graph-based, so the fast path
// keeps both: rcChangedSymbols (names this change introduces) and
// rcIdentifierLookups (a ripgrep resolving where the diff's identifiers live).
// The fast reviewer therefore starts oriented, and its ISSUES still ground to
// real path:line through rcGroundIssue, which reads git trees and never the DB.
//
// What it gives up, and must say out loud: kai_callers / kai_dependents /
// kai_context, kai_web_search, and reading any file the diff did not touch.
//
// The budget. The CI step is clone -> review -> ingest; with capture skipped
// and a shallow clone, everything around this call is ~15-25s, so the call gets
// 100 seconds and fails loudly rather than half-writing past the deadline. A
// fast pass that misses its deadline has no reason to exist.
var rcFastHardDeadline = rcFastBudget()

// rcFastBudget lets a deploy tune the ceiling (KAI_FAST_BUDGET=90s) without a
// rebuild — the number is a claim about a provider's latency, which is exactly
// the kind of fact that goes stale between releases.
func rcFastBudget() time.Duration {
	if v := strings.TrimSpace(os.Getenv("KAI_FAST_BUDGET")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		fmt.Fprintf(os.Stderr, "  ignoring unparseable KAI_FAST_BUDGET=%q\n", v)
	}
	return 100 * time.Second
}

// rcFastMaxTokens caps the prose. Output tokens are what the deadline is
// actually spent on, and a first pass is meant to be read in thirty seconds.
const rcFastMaxTokens = 2000

// rcFastReviewSystem is a DIFFERENT epistemic contract from rcReviewSystem, not
// a shortened one. The slow prompt is built on "ground every claim before you
// make it"; handed to a model with no tools, that instruction has exactly two
// outcomes, and this codebase has shipped both: it drops everything it cannot
// confirm and posts a hollow all-clear, or it asserts what it never checked.
// So this prompt tells the model what it CAN conclude from a diff alone, makes
// the shallow scope part of the deliverable, and forbids the green check.
const rcFastReviewSystem = `You are doing a FAST FIRST PASS on a merged commit or PR range. You get the author's description, the commit message, the diff, the symbols this change declares, and a set of already-resolved identifier lookups. You have NO tools: you cannot read a file the diff did not touch, cannot look up callers or dependents, and cannot search the web. A slower, graph-grounded review of this same change is running behind you and will supersede this one.

That shapes what you are for. Report what is VISIBLE IN THE DIFF ITSELF — the changed hunks and their context lines, read carefully. These are the defects a careful reader finds without leaving the patch:
- A changed signature, field, or return whose other uses are visible in this same diff and were not updated.
- Off-by-one, inverted conditions, nil dereference on a value the diff itself shows can be nil, a loop variable captured by a closure, a slice reused after append.
- An error created and dropped, returned to a caller that ignores it, or wrapped into the wrong branch; a defer that never runs because it sits after the return.
- A lock taken and not released on every path, state mutated outside the lock the surrounding code uses, a goroutine/ticker/file/connection opened in the diff with no visible stop or close.
- A secret, token, password, session id, HMAC or signature compared with ==/!= instead of a constant-time compare.
- Missing validation on an input the diff newly trusts; a new branch beside an existing one that skips a guard the older branch right there in the diff still has.
- A test that would pass on the unfixed code: it calls the thing and discards the answer, asserts that a mechanism was configured rather than that the behaviour happened, or skips for an environmental reason.

SAY WHAT YOU DID NOT READ. Your first line names your scope in the author's words, not as a disclaimer: this is a fast pass over the diff only, callers and dependents were not checked, and a grounded review is following.

AT MOST THREE ISSUES, MOST CONFIDENT FIRST, AND NONE OF THEM HEDGED. A fast pass earns its place by being short and right, not by being thorough — six maybes are worse than one certainty, because every ISSUES bullet becomes a risk-tagged claim and flips the PR badge to "review before merging". If you would write "appears", "seems", "worth confirming", "could", "may", "assuming", "depends on", or "if X then" into a bullet, it is NOT an issue: it goes in the prose as a sentence for the grounded pass to settle, and nowhere else. An ISSUES bullet is something you would bet on from the diff alone — a nil that will dereference, a lock that will not release, a caller in this same diff that was not updated. Anything softer, leave to the pass that can actually check it.

NEVER GREEN-CHECK. You did not do enough work to clear a change. If you found nothing, the honest sentence is "nothing visible in the diff itself" — never "this is correct", "this is safe", or "no issues". An all-clear from a pass that read no callers is worse than no pass at all, because it reads as coverage to the author who is about to merge.

NO UNIVERSALS. You searched nothing, so you cannot say "only", "never", "always", or "the sole caller". If the change's correctness rests on something outside the diff, name that thing and move on.

EVERY ISSUE NAMES ITS FILE. An ISSUES bullet MUST start with a path from the diff, verbatim, then a colon, then ONE line number, like frontend/dist/panel-changes.js:2152 — Not a bare line number, not a range, not a file you did not see in the diff. This is not formatting: the pipeline resolves each bullet's path:line against the reviewed tree, and one it cannot resolve is HELD — the concern still shows, but it stops counting as a risk, so a bullet written "2152 - the cache goes stale" hands the author a green badge over a real defect. Copy the path out of the diff header.

Write it the way a colleague skims a PR: the scope line, one short paragraph on what the change does, then each concern in plain sentences with its path:line, what goes wrong, and what you would do instead. Under 300 words. No severity labels, no category tags, no style nits.

Finish with this machine coda, exactly once, after everything else.

INTENT_MATCH judges the change against the author's ACTUAL goal, not a stricter one: verified = does what they intended; partial = mostly, with gaps; diverges = materially different or broken. On a fast pass, prefer partial over verified unless the diff is small enough that you genuinely saw all of it.

MERGE_READY answers what should happen to this branch NEXT, and on a fast pass it CANNOT be 5. A 5 means "no defects, and nothing here needs anyone's decision", which is a claim about code you did not read. Your ceiling is 4.
  4 — nothing visible in the diff blocks this; the grounded pass has not reported yet.
  3 — real defects, but local and quick; the change itself is sound.
  2 — defects in the core of what the change does.
  1 — do not merge. It does not do what it claims, or it breaks something that works today.

DECISIONS is for a change that is correct and still needs a human's yes — follow the changed values outward to anything that CHARGES a customer, LIMITS one, SENDS or PUBLISHES on their behalf, DELETES, or changes who can access what, and say who it affects and what the consequence is. When money moves, name who is debited and who is credited. A DECISION is not a defect: it carries no path:line and never lowers INTENT_MATCH or MERGE_READY. Omit either list entirely when it is empty.
===REVIEW-DATA===
INTENT_MATCH: verified|partial|diverges
MERGE_READY: 1|2|3|4
SUMMARY: <one honest sentence — your bottom line, and that this was a fast pass>
ISSUES:
- <path from the diff>:<line> — <one-sentence version of each concern from your review>
DECISIONS:
- <what the author is deciding, who it affects, and the consequence — no path:line>`

// rcRunFastReview makes ONE completion over the diff and its git-derived
// context. No agent loop, no session store, no graph — so it also runs in a
// repo that was never captured, which is what lets the CI workflow skip the
// capture step entirely.
//
// The separate intent-reconstruction call the slow path makes is deliberately
// NOT made here: it is a serial round-trip whose only consumer is the review
// prompt, and the fast reviewer can read the commit message itself. The
// finding's Intent.Stated still comes from the commit subject, exactly as
// before, so the bundle shape is unchanged.
func rcRunFastReview(ctx context.Context, prov provider.Provider, model, root, authorContext, subject, body, diff string, changedPaths []string) (string, error) {
	var user strings.Builder
	if sc := strings.TrimSpace(authorContext); sc != "" {
		if len(sc) > rcMaxAuthorContextBytes {
			sc = sc[:rcMaxAuthorContextBytes] + "\n... (context truncated)"
		}
		user.WriteString("AUTHOR CONTEXT (the change author's own description):\n")
		user.WriteString(sc)
		user.WriteString("\n\n")
	}
	user.WriteString("COMMIT MESSAGE:\n")
	user.WriteString(strings.TrimSpace(subject))
	if b := strings.TrimSpace(body); b != "" {
		user.WriteString("\n\n")
		user.WriteString(b)
	}
	user.WriteString("\n\n")

	// Same two seeds the agent path builds, for the same reason: a reviewer
	// must never spend its budget rediscovering what its own input states.
	// Both are git/ripgrep, so neither needs a captured graph.
	declared := map[string]bool{}
	if symbols := rcChangedSymbols(diff); symbols != "" {
		user.WriteString("CHANGED SYMBOLS (extracted from this diff — these are new or modified IN THIS CHANGE):\n")
		user.WriteString(symbols)
		user.WriteString("\n\n")
		for _, ln := range strings.Split(symbols, "\n") {
			if i := strings.LastIndex(ln, ": "); i >= 0 {
				declared[strings.TrimSpace(ln[i+2:])] = true
			}
		}
	}
	if root != "" {
		if lookups := rcIdentifierLookups(diff, root, declared); lookups != "" {
			user.WriteString("WHERE THESE LIVE (resolved from the repository before this review started — ")
			user.WriteString("these are the only facts you have about code outside the diff):\n")
			user.WriteString(lookups)
			user.WriteString("\n")
		}
	}
	// The paths, spelled out. The model has them in the diff headers and still
	// wrote bare line numbers on the first live run (kai-desktop 94dbbe8:
	// both of its real defects came back "2152 — ..." and "2649-2652 — ...",
	// both held, RiskCount 0, green badge over two genuine bugs). Listing them
	// where the instruction can point at them costs nothing.
	if len(changedPaths) > 0 {
		user.WriteString("FILES IN THIS DIFF (every ISSUES bullet must begin with one of these paths, verbatim, then :line):\n")
		for _, p := range changedPaths {
			user.WriteString("  ")
			user.WriteString(p)
			user.WriteString("\n")
		}
		user.WriteString("\n")
	}
	user.WriteString("DIFF:\n")
	user.WriteString(diff)
	user.WriteString("\n")

	// A fresh deadline, not a slice of the caller's: the fast pass's whole
	// contract is that it answers inside the budget or fails saying so.
	cctx, cancel := context.WithTimeout(ctx, rcFastHardDeadline)
	defer cancel()

	resp, err := prov.Send(cctx, provider.Request{
		Model:     model,
		System:    rcFastReviewSystem,
		MaxTokens: rcFastMaxTokens,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: user.String()}}}},
	})
	if err != nil {
		return "", fmt.Errorf("fast review call: %w", err)
	}
	var out strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			out.WriteString(t.Text)
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// rcCapFastReadiness enforces the ceiling the prompt states. The prompt is the
// contract and the cap is the enforcement — a model that ignores the ceiling
// would otherwise post "merge it" on a change whose callers nobody read, which
// is precisely the failure the two-pass design exists to avoid.
//
// ReadinessUnknown is left alone: "the reviewer did not say" is not a score,
// and turning it into a 4 would invent one.
func rcCapFastReadiness(r finding.Readiness) finding.Readiness {
	if r == finding.ReadinessMerge {
		fmt.Fprintln(os.Stderr, "  fast pass scored 5 (merge it) — capped to 4: a diff-only pass cannot clear a change")
		return finding.ReadinessDecideThenMerge
	}
	return r
}

// rcFastDefaultModel is what a fast pass falls back to when the configured
// review model is a reasoning model. It is deliberately NOT the agent-loop
// model: the two roles optimize for opposite things.
//
// Measured 2026-09-07 on kai-desktop 94dbbe8 (3 files, +182/-10), one call,
// same prompt: z-ai/glm-5.2 — the production review model — took 3m58s and
// blew a 100s budget. That is not a fluke and not a slow provider hour; it is
// what the model is. provider.isReasoningModel already carries the note: "z.ai's
// GLM family emits a large hidden chain-of-thought — 16.7KB of reasoning
// content + ~3 min on a single turn". The agent path can absorb that because it
// has nine minutes and the thinking buys grounded claims. A pass whose entire
// value proposition is landing before the author navigates away cannot.
const rcFastDefaultModel = "anthropic/claude-haiku-4-5"

// rcFastModel picks the model for the fast pass. KAI_FAST_MODEL wins outright.
// Otherwise a reasoning review model is swapped out — loudly, because silently
// reviewing on a different model than `kai config show` reports is exactly the
// kind of divergence that makes a benchmark unreproducible — and a
// non-reasoning one is kept as-is.
//
// The substitution is only legal where rcFastDefaultModel actually resolves.
// It is an OpenRouter-style namespaced id, which kailab (proxying OpenRouter)
// and OpenRouter itself both serve. KindOpenAI is every OpenAI-COMPATIBLE
// endpoint — Together, Groq, Ollama, vLLM and LM Studio all normalize to it —
// and those serve their own namespaces, so handing one an "anthropic/..." id
// fails the request. Since the fast pass is now the DEFAULT review, that
// failure would not be a slow review, it would be no review at all. On such a
// provider the configured model is kept and the cost is announced instead;
// this repo has been bitten by the same prefix-decides-the-route mechanism
// before (kai-cli #63: a bare id routed DIRECT to api.anthropic.com).
func rcFastModel(reviewModel string, kind provider.Kind) string {
	if m := strings.TrimSpace(os.Getenv("KAI_FAST_MODEL")); m != "" {
		return m
	}
	if !provider.IsReasoningModel(reviewModel) {
		return reviewModel
	}
	if kind != provider.KindKailab && kind != provider.KindOpenRouter {
		fmt.Fprintf(os.Stderr, "  %s is a reasoning model (hidden chain-of-thought) and this fast pass may be slow; "+
			"the %s provider serves its own model namespace, so %s cannot be substituted — set KAI_FAST_MODEL to a fast model it serves\n",
			reviewModel, kind, rcFastDefaultModel)
		return reviewModel
	}
	fmt.Fprintf(os.Stderr, "  %s is a reasoning model (hidden chain-of-thought); fast pass uses %s — override with KAI_FAST_MODEL\n",
		reviewModel, rcFastDefaultModel)
	return rcFastDefaultModel
}

// rcFastMaxIssues is the ceiling the prompt states. Three is not a style
// preference: every ISSUES bullet becomes a risk-tagged Claim, the inbox
// denormalizes RiskCount from those, and mergeLine flips the PR comment to
// "Review before merging" on RiskCount > 0. A fast pass that files six
// maybes has not reviewed the change, it has degraded the badge.
const rcFastMaxIssues = 3

// rcFastHedges are the phrases that mark a bullet as something the model did
// not actually establish. Kept deliberately narrow — each one is a phrase
// whose presence makes the sentence non-committal on its own, not a word that
// merely CAN appear in a hedge. "could" and "may" are excluded for exactly
// that reason: "removeChild(null) will throw" and "this could throw" both
// contain a defect, and only the second reads as a guess.
//
// Measured 2026-09-07: the first fast run on kai-desktop 94dbbe8 filed six
// ISSUES, four of which closed with "worth confirming", "this appears
// correct but", or "should confirm" — the model narrating its own uncertainty
// into the defect list. The prompt now forbids it; this is the enforcement,
// on the same principle as rcCapFastReadiness. The prompt is the contract and
// the code is what makes it true.
var rcFastHedges = []string{
	"worth confirming", "worth checking", "worth a look", "worth verifying",
	"should confirm", "should be confirmed", "needs confirming", "would confirm",
	"appears correct", "appears to be", "appears sound", "appears intentional", "appears safe",
	"seems correct", "seems fine", "seems sound", "seems intentional", "seems safe",
	"could not verify", "cannot verify", "unable to verify", "did not verify",
	"hard to tell", "not sure", "unclear whether", "may or may not",
	"assuming ", "depends on whether", "if this is intended",
}

// rcFilterFastIssues applies both ceilings to a fast pass's ISSUES list, and
// announces every drop. Silence here would be the worst of both worlds: a
// filter nobody can see is indistinguishable from a model that got it right.
//
// Order matters — hedges are dropped BEFORE the count is capped, so the cap
// keeps the three most confident bullets rather than the first three the model
// happened to emit.
func rcFilterFastIssues(issues []string) []string {
	kept := make([]string, 0, len(issues))
	for _, it := range issues {
		if h, ok := rcHedgePhrase(it); ok {
			fmt.Fprintf(os.Stderr, "  fast pass: dropping hedged issue (%q) — %s\n", h, rcOneLine(it, 80))
			continue
		}
		kept = append(kept, it)
	}
	if len(kept) > rcFastMaxIssues {
		for _, it := range kept[rcFastMaxIssues:] {
			fmt.Fprintf(os.Stderr, "  fast pass: dropping issue past the %d-issue cap — %s\n", rcFastMaxIssues, rcOneLine(it, 80))
		}
		kept = kept[:rcFastMaxIssues]
	}
	return kept
}

// rcHedgePhrase reports the first hedge phrase in an issue bullet, if any.
func rcHedgePhrase(item string) (string, bool) {
	lower := strings.ToLower(item)
	for _, h := range rcFastHedges {
		if strings.Contains(lower, h) {
			return strings.TrimSpace(h), true
		}
	}
	return "", false
}
