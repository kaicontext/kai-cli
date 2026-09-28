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
		return rcChallengeDraft(ctx, prov, model, draft, rcBatchSources(sources, extra, issues), sandbox)
	}
	var batches [][]string
	n := (len(issues) + rcChallengeBatchSize - 1) / rcChallengeBatchSize
	for i := 0; i < n; i++ {
		lo, hi := i*len(issues)/n, (i+1)*len(issues)/n
		batches = append(batches, issues[lo:hi])
	}
	fmt.Fprintf(os.Stderr, "  challenge: %d allegations in %d batches\n", len(issues), len(batches))
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
			if i == 0 {
				ds = decisions
			}
			results[i], errs[i] = rcChallengeDraft(ctx, prov, model, rcSubDraft(prose, batch, ds), rcBatchSources(sources, extra, batch), sandbox)
		}(i, batch)
	}
	wg.Wait()
	return rcMergeBatches(batches, decisions, results, errs)
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
				merged.Allegations = append(merged.Allegations, rcAllegationResult{Issue: is, Status: rcStatusUnresolved, Reason: fmt.Sprintf("the check for this allegation's batch did not complete (%v)", errs[i])})
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
	}
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
	return merged, nil
}
