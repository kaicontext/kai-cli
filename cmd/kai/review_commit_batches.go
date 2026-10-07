package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/provider"
)

// The publication gate, in batches.
//
// The gate used to check every allegation of a draft in one call with one
// three-minute deadline. A draft with many allegations — which is what a
// review that reads every changed line produces — made that call long, and
// when it ran out of time or came back malformed the WHOLE review was
// withheld: 26 of the 119 misses on the 2026-09-27 benchmark rerun were
// reviews lost this way or to the job clock ("challenge call: context
// deadline exceeded", "resubmission still rejected"), with the defects
// already drafted. Checking a few allegations per call keeps each call short,
// runs them side by side, and confines a failure to its own batch: those
// allegations are listed as not verified, and every other batch publishes.

var (
	rcChallengeBatchSize     = 5
	rcChallengeBatchParallel = 4
)

// rcChallengeBatches checks the draft's allegations in batches of at most
// rcChallengeBatchSize and assembles one review from the results. A draft that
// fits in one batch takes exactly the old path.
func rcChallengeBatches(ctx context.Context, prov provider.Provider, model, draft string, sources, extra []rcSource, sandbox *rcShellSandbox) (*rcChallengeResult, error) {
	prose, issues, decisions, _, _, _ := rcParseReviewOutput(draft)
	if len(issues) <= rcChallengeBatchSize {
		res, err := rcChallengeDraft(ctx, prov, model, draft, rcBatchSources(sources, extra, issues), sandbox)
		if err != nil && rcCapacityFailure(err) && ctx.Err() == nil {
			rr, ee := []*rcChallengeResult{res}, []error{err}
			rcRecoverCapacityBatches(ctx, rr, ee, func(int) (*rcChallengeResult, error) {
				return rcChallengeDraft(ctx, prov, model, draft, rcBatchSources(sources, extra, issues), sandbox)
			}, rcCapacityWait)
			return rr[0], ee[0]
		}
		if err == nil || len(issues) < 2 || ctx.Err() != nil {
			return res, err
		}
		fmt.Fprintf(os.Stderr, "  challenge: the check failed (%v); re-checking its %d allegations one at a time\n", err, len(issues))
		batches := rcSingles(issues)
		results, errs := rcRunBatches(ctx, prov, model, prose, decisions, 0, batches, sources, extra, sandbox)
		return rcMergeBatches(batches, decisions, results, errs)
	}
	var batches [][]string
	n := (len(issues) + rcChallengeBatchSize - 1) / rcChallengeBatchSize
	for i := 0; i < n; i++ {
		lo, hi := i*len(issues)/n, (i+1)*len(issues)/n
		batches = append(batches, issues[lo:hi])
	}
	fmt.Fprintf(os.Stderr, "  challenge: %d allegations in %d batches\n", len(issues), len(batches))
	results, errs := rcRunBatches(ctx, prov, model, prose, decisions, 0, batches, sources, extra, sandbox)
	rcRecoverCapacityBatches(ctx, results, errs, func(i int) (*rcChallengeResult, error) {
		var ds []string
		if i == 0 {
			ds = decisions
		}
		return rcChallengeDraft(ctx, prov, model, rcSubDraft(prose, batches[i], ds), rcBatchSources(sources, extra, batches[i]), sandbox)
	}, rcCapacityWait)
	batches, results, errs = rcRetryFailedBatches(ctx, prov, model, prose, decisions, batches, results, errs, sources, extra, sandbox)
	return rcMergeBatches(batches, decisions, results, errs)
}

// rcRunBatches checks each batch side by side. The batch at decisionsAt (-1
// for none) also carries the draft's decisions.
func rcRunBatches(ctx context.Context, prov provider.Provider, model, prose string, decisions []string, decisionsAt int, batches [][]string, sources, extra []rcSource, sandbox *rcShellSandbox) ([]*rcChallengeResult, []error) {
	results := make([]*rcChallengeResult, len(batches))
	errs := make([]error, len(batches))
	sem := make(chan struct{}, rcChallengeBatchParallel)
	var wg sync.WaitGroup
	for i, batch := range batches {
		wg.Add(1)
		go func(i int, batch []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var ds []string
			if i == decisionsAt {
				ds = decisions
			}
			results[i], errs[i] = rcChallengeDraft(ctx, prov, model, rcSubDraft(prose, batch, ds), rcBatchSources(sources, extra, batch), sandbox)
		}(i, batch)
	}
	wg.Wait()
	return results, errs
}

// rcRetryFailedBatches re-checks every failed batch one allegation at a time.
//
// A batch fails as a unit — its call ran out of time, or its answer broke the
// protocol twice — and before this every allegation in it was lost with it:
// listed as not verified, and a sweep proposal among them withheld. Five of
// the run 5 misses (2026-09-28) were drafted defects lost exactly this way.
// One allegation per call is the shortest check there is, so the retry is
// the likeliest to finish; the results are spliced back in place, so the
// first entry still carries the decisions.
func rcRetryFailedBatches(ctx context.Context, prov provider.Provider, model, prose string, decisions []string, batches [][]string, results []*rcChallengeResult, errs []error, sources, extra []rcSource, sandbox *rcShellSandbox) ([][]string, []*rcChallengeResult, []error) {
	if ctx.Err() != nil {
		return batches, results, errs
	}
	var singles [][]string
	decisionsAt := -1
	for i, batch := range batches {
		if errs[i] == nil || len(batch) < 2 || rcCapacityFailure(errs[i]) {
			continue
		}
		fmt.Fprintf(os.Stderr, "  challenge: batch %d of %d failed (%v); re-checking its %d allegations one at a time\n", i+1, len(batches), errs[i], len(batch))
		if i == 0 {
			decisionsAt = len(singles)
		}
		singles = append(singles, rcSingles(batch)...)
	}
	if len(singles) == 0 {
		return batches, results, errs
	}
	sr, se := rcRunBatches(ctx, prov, model, prose, decisions, decisionsAt, singles, sources, extra, sandbox)
	var nb [][]string
	var nr []*rcChallengeResult
	var ne []error
	next := 0
	for i, batch := range batches {
		if errs[i] == nil || len(batch) < 2 || rcCapacityFailure(errs[i]) {
			nb, nr, ne = append(nb, batch), append(nr, results[i]), append(ne, errs[i])
			continue
		}
		for range batch {
			nb, nr, ne = append(nb, singles[next]), append(nr, sr[next]), append(ne, se[next])
			next++
		}
	}
	return nb, nr, ne
}

// rcSingles splits issues into one-allegation batches.
func rcSingles(issues []string) [][]string {
	out := make([][]string, len(issues))
	for i, is := range issues {
		out[i] = []string{is}
	}
	return out
}

// rcSubDraft is a draft whose coda carries only these issues and decisions.
// The prose stays whole: it is the reviewer's reasoning, and a check reads it
// for context and contradictions.
func rcSubDraft(prose string, issues, decisions []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(prose))
	b.WriteString("\n\n")
	b.WriteString(rcReviewDataMarker)
	b.WriteString("\nISSUES:\n")
	for _, is := range issues {
		fmt.Fprintf(&b, "- %s\n", is)
	}
	if len(decisions) > 0 {
		b.WriteString("DECISIONS:\n")
		for _, d := range decisions {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	return b.String()
}

// rcBatchSources is the base sources plus the extra ones that cover a file
// these issues name, stopping short of the evidence limit rather than
// tripping it.
func rcBatchSources(sources, extra []rcSource, issues []string) []rcSource {
	if len(extra) == 0 {
		return sources
	}
	paths := map[string]bool{}
	for _, is := range issues {
		if p, _, ok := rcIssueLocation(is); ok {
			paths[p] = true
		}
	}
	out := append([]rcSource(nil), sources...)
	size := 0
	for _, s := range sources {
		size += len(s.Text)
	}
	for _, s := range extra {
		relevant := false
		for p := range paths {
			if strings.Contains(s.Text, "+++ b/"+p) || strings.Contains(s.Text, "--- a/"+p) {
				relevant = true
				break
			}
		}
		if !relevant || size+len(s.Text) > rcEvidenceLimit*3/4 {
			continue
		}
		out = append(out, s)
		size += len(s.Text)
	}
	return out
}

// rcMergeBatches assembles one result from the batches. A failed batch's
// allegations are unresolved with the failure as the reason; decisions travel
// with the first batch. Only when every batch failed is the draft withheld,
// exactly as a single failed check withholds it.
func rcMergeBatches(batches [][]string, decisions []string, results []*rcChallengeResult, errs []error) (*rcChallengeResult, error) {
	var ok []*rcChallengeResult
	var firstErr error
	for i, r := range results {
		if errs[i] == nil && r != nil {
			ok = append(ok, r)
		} else if firstErr == nil {
			firstErr = errs[i]
		}
	}
	if len(ok) == 0 {
		return nil, firstErr
	}
	merged := &rcChallengeResult{match: ok[0].match, proposed: ok[0].proposed}
	seenScope, seenLimit := map[string]bool{}, map[string]bool{}
	for i, batch := range batches {
		r := results[i]
		if errs[i] != nil || r == nil {
			fmt.Fprintf(os.Stderr, "  challenge: batch %d of %d failed (%v) — its %d allegation(s) are listed as not verified\n", i+1, len(batches), errs[i], len(batch))
			for _, is := range batch {
				merged.Allegations = append(merged.Allegations, rcAllegationResult{Issue: is, Status: rcStatusUnresolved, Reason: fmt.Sprintf("the check for this allegation's batch did not complete (%v)", errs[i]), Unchecked: true})
			}
			continue
		}
		merged.Allegations = append(merged.Allegations, r.Allegations...)
		if r.proposed.Valid() && r.proposed < merged.proposed {
			merged.proposed = r.proposed
		}
		for _, s := range r.scope {
			if !seenScope[s] {
				seenScope[s] = true
				merged.scope = append(merged.scope, s)
			}
		}
		for _, s := range r.limitations {
			if !seenLimit[s] {
				seenLimit[s] = true
				merged.limitations = append(merged.limitations, s)
			}
		}
	}
	for i := range merged.Allegations {
		merged.Allegations[i].ID = i + 1
	}
	if errs[0] == nil && results[0] != nil {
		merged.Decisions = results[0].Decisions
		merged.match = results[0].match
	} else {
		for i, d := range decisions {
			merged.Decisions = append(merged.Decisions, rcDecisionResult{ID: i + 1, Decision: d, Status: rcStatusUnresolved, Reason: "the check for this decision's batch did not complete"})
		}
		// The first batch carries the decisions, so its intent verdict is the
		// one that saw the whole change. Without it, no surviving batch's
		// verdict can stand in for it optimistically: take the least
		// favourable one.
		merged.match = rcWorstMatch(ok)
	}
	rcFinalize(merged)
	return merged, nil
}

// rcFinalize recomputes a result's readiness, summary and published review
// from its allegations and decisions, with the caps rcValidateChallenge
// applies. Used after batches are merged and after allegations change status.
func rcFinalize(merged *rcChallengeResult) {
	merged.FailedChecks = 0
	for _, a := range merged.Allegations {
		if a.Unchecked {
			merged.FailedChecks++
		}
	}
	merged.VerificationIncomplete = merged.FailedChecks > 0
	supported, refuted, unresolved := 0, 0, 0
	for _, a := range merged.Allegations {
		switch a.Status {
		case rcStatusSupported:
			supported++
		case rcStatusRefuted:
			refuted++
		default:
			unresolved++
		}
	}
	kept, openDecisions := 0, 0
	for _, d := range merged.Decisions {
		switch d.Status {
		case rcStatusSupported:
			kept++
		case rcStatusUnresolved:
			openDecisions++
		}
	}
	// The same caps rcValidateChallenge applies, over the merged counts.
	readiness := merged.proposed
	if supported > 0 && readiness > finding.ReadinessSmallFixes {
		readiness = finding.ReadinessSmallFixes
	}
	if (kept > 0 || unresolved+openDecisions > 0) && readiness > finding.ReadinessDecideThenMerge {
		readiness = finding.ReadinessDecideThenMerge
	}
	summary := rcDeriveSummary(supported, refuted, unresolved, openDecisions, merged.match, readiness)
	merged.Review = rcAssembleReview(merged.scope, merged.limitations, merged.Allegations, merged.Decisions, merged.match, readiness, summary)
}

// rcWorstMatch is the least favourable intent verdict among results:
// diverges over partial over verified.
func rcWorstMatch(results []*rcChallengeResult) finding.Match {
	rank := map[finding.Match]int{finding.MatchVerified: 1, finding.MatchPartial: 2, finding.MatchDiverges: 3}
	worst := results[0].match
	for _, r := range results[1:] {
		if rank[r.match] > rank[worst] {
			worst = r.match
		}
	}
	return worst
}

// rcChallengeModel is the model the publication gate runs on: the review
// profile's factcheck stage, else KAI_CHALLENGE_MODEL when set, else the
// review model.
//
// The gate is where GLM's format failures cost whole reviews — a malformed
// submission, rejected twice, withholds the draft — and where false positives
// get through, while it is a small share of a review's tokens. So it is the
// stage worth moving to a model chosen for reliable structured output, apart
// from the model that investigates. The review job sets it; unset, nothing
// changes.
func rcChallengeModel(reviewModel string) string {
	model := reviewModel
	if m := strings.TrimSpace(os.Getenv("KAI_CHALLENGE_MODEL")); m != "" {
		model = m
	}
	return rcStageModel(rcStageFactcheck, model)
}

// rcWithholdUnsettledSweep withholds the sweep's proposals that the gate could
// not settle. The sweep is a candidate generator: a proposal it made and the
// check neither confirmed nor refuted is a guess, and listing it under "Could
// not verify" would publish that guess as a doubt — an unmatched finding on
// the benchmark and noise on a real PR. The reviewer's own unsettled points
// are still listed, as before. Reports how many were withheld.
//
// A proposal whose check never ran — its batch failed, even one allegation at
// a time — is NOT withheld: the gate reached no view of it, and an
// infrastructure failure is not a refutation. It stays under "Could not
// verify".
func rcWithholdUnsettledSweep(res *rcChallengeResult, sweep []string) int {
	if res == nil || len(sweep) == 0 {
		return 0
	}
	fromSweep := map[string]bool{}
	for _, is := range sweep {
		fromSweep[rcIssueKey(is)] = true
	}
	n := 0
	for i, a := range res.Allegations {
		if a.Status == rcStatusUnresolved && !a.Unchecked && fromSweep[rcIssueKey(a.Issue)] {
			res.Allegations[i].Status = rcStatusRefuted
			res.Allegations[i].Reason = "withheld: a sweep proposal the check could not settle (" + a.Reason + ")"
			n++
		}
	}
	if n > 0 && res.match != "" {
		rcFinalize(res)
	}
	return n
}
