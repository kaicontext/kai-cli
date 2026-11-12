package main

// review_commit_retry.go — what the review does when its model returns nothing.
//
// kai-engine's send path recovers from an empty reply by switching models,
// along chains that ignore the review's model set: one empty GLM reply could
// move the review onto claude-opus-4-8 for the rest of the run, and a Free
// review's Gemini model fell back to GLM. A review runs on every push and has
// its own account of a run that did not finish, so when the server sends the
// set's backups (KAI_REVIEW_FALLBACK_MODELS, review_commit_backups.go):
//   - the engine fails over only to them (agent.Options.FallbackModels);
//   - a run that still ends with no answer is resumed on the same model once,
//     then on each backup in order (rcRunWithRetries).
//
// With KAI_REVIEW_PARTIAL on, a conclusion written from the transcript is
// adopted only when it carries a verdict block, stray tool-call text is
// removed from it, and a review with no verdict block publishes the sweep's
// checked findings instead of throwing them away (rcDraftForCheck). With
// neither setting sent, none of this runs and the review is today's.
//
// The resume sits around agent.Run rather than around the provider because
// the engine uploads a run's cost only when Options.Provider is a
// *provider.Kailab; wrapping the provider would silently drop every review's
// cost record.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// rcResumePrompt continues a review whose last model call returned nothing. The
// empty turn itself never reaches the next request: the OpenAI-shape serializer
// drops a message with no content.
const rcResumePrompt = "The previous model call returned nothing. Continue this review from where it stopped " +
	"and finish it: end with the complete review and its machine coda."

// rcAgentRun is agent.Run, replaceable in tests.
type rcAgentRun func(context.Context, agent.Options) (*agent.Result, error)

// rcRanDry reports whether a run ended because the model stopped answering,
// rather than on a clock or a turn cap, where the conclusion step is the right
// next move and asking the model for more is not.
func rcRanDry(res *agent.Result) bool {
	if res == nil || strings.TrimSpace(res.FinalText) != "" {
		return false
	}
	switch res.FinishReason {
	case message.FinishReasonTimeBudget, message.FinishReasonTurnCap, message.FinishReasonCanceled:
		return false
	}
	return true
}

// rcRunWithRetries runs the review agent, resuming it when the model stops
// answering: on the same model when it ran dry, then on each of backups in
// order until one finishes. A run that failed on transient errors skips the
// same-model step, because the engine and the provider have already retried
// it. Any other failure is returned as it is: more attempts cannot fix an auth
// error or a spent budget. Each move to a backup is reported in run.failovers.
//
// It returns the result to use, the model that produced it, and the run's
// error. A resume that fails leaves the earlier result standing, because the
// earlier run's transcript is still good evidence for the conclusion step.
func rcRunWithRetries(ctx context.Context, opts agent.Options, started time.Time, backups []string, run rcAgentRun) (*agent.Result, string, error) {
	res, err := run(ctx, opts)
	active := opts.Model
	// failed are the models this run has already seen fail. A resume on a
	// backup must not have the engine fail over back to one of them: the
	// set's list usually names the main model too.
	failed := map[string]bool{strings.ToLower(opts.Model): true}

	try := func(model string) bool {
		next, ok := rcResumeOptions(opts, res, model, started)
		if !ok {
			fmt.Fprintf(os.Stderr, "  not enough clock left to resume the review on %s\n", model)
			return false
		}
		if opts.FallbackModels != nil {
			next.FallbackModels = rcWithoutModels(opts.FallbackModels, failed, model)
		}
		if model != active {
			rcRun.swapped(rcStageMain, active, model, rcSwapResume)
		}
		r, e := run(ctx, next)
		if e != nil || rcRanDry(r) {
			failed[strings.ToLower(model)] = true
		}
		if e != nil {
			fmt.Fprintf(os.Stderr, "  review resume on %s failed: %v\n", model, e)
			return false
		}
		if !rcRanDry(r) {
			// The stage answered after all: an earlier failed attempt no
			// longer makes it a failed stage in the run report.
			rcRun.recoveredStage(rcStageMain)
		}
		// A dry resume is still adopted, but leaves the stage failed. Its
		// session holds the whole transcript so far, which the conclusion
		// step writes the review from; keeping the earlier error instead
		// would end the review with no conclusion at all.
		res, err, active = r, nil, model
		return true
	}

	if err == nil && rcRanDry(res) {
		fmt.Fprintf(os.Stderr, "  the model returned nothing — resuming the review on %s\n", opts.Model)
		try(opts.Model)
	}
	for _, b := range backups {
		if !((err == nil && rcRanDry(res)) || rcFailoverWorthy(ctx, err)) {
			break
		}
		if strings.EqualFold(b, opts.Model) {
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s is not answering — resuming the review on the set's backup %s\n", active, b)
		if !try(b) && rcReviewHardDeadline-time.Since(started) < rcGateMinHeadroom {
			break
		}
	}
	return res, active, err
}

// rcWithoutModels is list without the models in drop, except keep, in order.
// Non-nil even when empty: an empty list means no failover left.
func rcWithoutModels(list []string, drop map[string]bool, keep string) []string {
	out := []string{}
	for _, m := range list {
		if !drop[strings.ToLower(m)] || strings.EqualFold(m, keep) {
			out = append(out, m)
		}
	}
	return out
}

// rcFailoverWorthy reports whether a failed run might succeed on another model:
// the provider failing every retry, with the review's own clock still running.
func rcFailoverWorthy(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return provider.IsTransient(err) && !provider.IsCapExceeded(err)
}

// rcResumeOptions is opts set up to continue res on model, with what remains of
// the review's clock and turns. False when too little clock remains to be worth
// a model call. A run with no session to resume (a provider failing before the
// first turn) starts over with the original prompt.
func rcResumeOptions(opts agent.Options, res *agent.Result, model string, started time.Time) (agent.Options, bool) {
	elapsed := time.Since(started)
	if rcReviewHardDeadline-elapsed < rcGateMinHeadroom {
		return opts, false
	}
	next := opts
	next.Model = model
	if opts.SoftTimeBudget > 0 {
		next.SoftTimeBudget = opts.SoftTimeBudget - elapsed
		if next.SoftTimeBudget < rcGateMinHeadroom {
			next.SoftTimeBudget = rcGateMinHeadroom
		}
	}
	if res != nil && res.SessionID != "" {
		next.SessionID = res.SessionID
		next.Prompt = rcResumePrompt
		next.InjectedContext = "" // already in the session's history
		if left := opts.MaxTurns - rcTurns(res.Transcript); opts.MaxTurns > 0 {
			next.MaxTurns = max(left, 4)
		}
	}
	return next, true
}

// rcStrayToolCall matches tool-call syntax written as text. Asked to conclude
// with no tools on offer, GLM can carry on as if it still had them, in a form
// nothing downstream recognises:
// <tool_call>kai_code_read add.go</arg_value></tool_call>
//
// Only text that starts its line with one of the tags is stray. A review that
// quotes a tag inside a sentence or an ISSUES bullet (a review of code that
// handles tool calls) keeps it: the bullet is a finding.
var (
	rcStrayToolCallBlock = regexp.MustCompile(`(?ms)^[ \t]*<tool_call>.*?</tool_call>[ \t]*$`)
	rcStrayToolCallLine  = regexp.MustCompile(`(?m)^[ \t]*</?(?:tool_call|arg_key|arg_value)>.*$`)
)

// rcWithoutStrayToolCalls removes tool-call syntax written as text, so it can
// neither pass for a review nor sit in the prose above a real coda.
func rcWithoutStrayToolCalls(text string) string {
	text = rcStrayToolCallBlock.ReplaceAllString(text, "")
	text = rcStrayToolCallLine.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

// rcConclusionCategoryNoCoda is rcConcludeFromTranscript's label for a reply
// that had text but no review in it.
const rcConclusionCategoryNoCoda = "no_coda"

// rcConcludeRetryable reports whether a failed conclusion is worth another
// attempt, and whether that attempt should stay on the same model. An empty or
// coda-less reply can be a blip. A provider failure has already been retried
// inside the provider, so only another model can help, and the rest (evidence
// too large, a 4xx, a spent budget) no model can fix.
func rcConcludeRetryable(category string) (retry, sameModel bool) {
	switch category {
	case "empty", rcConclusionCategoryNoCoda:
		return true, true
	case "upstream_5xx", "rate_limited", "upstream_network", "timeout":
		return true, false
	}
	return false, false
}

// rcDraftForCheck is the draft the publication gate should check once every
// finder has reported. A draft with a usable coda is checked as it is. Without
// one, a real review does not exist: the sweep's proposals are the only thing
// left worth a check, in a draft that claims nothing else (rcSweepOnlyDraft).
// With no proposals either, ok is false and the run reports itself incomplete.
//
// Text with no coda is never returned. It reaches no check and carries no
// verdict, and publishing it reads as a clean review.
func rcDraftForCheck(raw string, sweepIssues []string) (draft string, sweepOnly, ok bool) {
	if rcUsableCoda(raw) {
		return raw, false, true
	}
	if len(sweepIssues) == 0 {
		return "", false, false
	}
	return rcSweepOnlyDraft, true, true
}

// rcSweepOnlyDraft is the draft a sweep's proposals are merged into when no
// finder wrote a review. It has no INTENT_MATCH and no MERGE_READY: nothing
// assessed the change as a whole, so it offers no verdict for anything to read.
const rcSweepOnlyDraft = rcReviewDataMarker + "\nSUMMARY: The main review did not finish; these are the diff sweep's proposals.\nISSUES:\n- (none)\n"
