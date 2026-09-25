package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/kaicontext/kai-engine/finding"
)

// A defect needs a trigger that exists today. The benchmark run of 2026-09-24
// published, as confirmed defects, findings whose own wording said the
// trigger was a change nobody had made: "dormant today (only SMS writes
// retryCount) but will silently delete email/whatsapp reminders if
// retry-incrementing is ever added to those paths" (Cal.com #14943) and
// "latent footgun for future strict comparisons" (Cal.com #11059). The three
// prompts say this now; rcWithoutSpeculativeIssues is the enforcement, on the
// principle of rcFilterFastIssues — the prompt is the contract, the code makes
// it true.
//
// The phrases name a future CODE change. A future runtime event ("if a future
// request arrives before the lock is taken") is a trigger that exists today,
// so bare "if a future" is not on the list.
//
// The phrases are the future-change wording itself, not hedges in general: a
// hedge ("appears", "may") is the fast pass's filter; here the reviewer is
// sure, and sure about a trigger that does not exist yet. Each was checked
// against ordinary defect sentences in TestSpeculativePhraseLeavesRealDefectsAlone.
var rcSpeculativePhrases = []string{
	"dormant today", "dormant for now", "dormant until",
	"latent footgun", "footgun for future", "footgun if",
	"is ever added", "are ever added", "is ever introduced", "are ever introduced",
	"is ever reordered", "are ever reordered", "is ever changed to", "is ever refactored",
	"if someone later", "if someone ever", "should anyone later",
	"a future change", "a future refactor", "a future caller", "future strict",
	"a future step", "a future edit", "a future version", "a future maintainer",
	"a future contributor", "future non-terminal", "future code path",
	"future guard reorder", "if the guards are reordered", "if the checks are reordered",
	"would break if someone", "will break if someone",
}

// rcSpeculativePhrase reports the first future-change phrase in an ISSUES
// bullet, if any.
func rcSpeculativePhrase(item string) (string, bool) {
	lower := strings.ToLower(item)
	for _, p := range rcSpeculativePhrases {
		if strings.Contains(lower, p) {
			return p, true
		}
	}
	return "", false
}

// rcWithoutSpeculativeIssues removes, from a draft's ISSUES list, every bullet
// whose trigger is a future change, and returns the draft without them plus
// one refuted result per removal. It runs BEFORE the publication gate: the
// gate assembles the published review from its per-allegation verdicts, so a
// bullet removed here can never reach the findings, the ISSUES coda or a claim
// — and the gate is not asked to spend its budget on it. Only ISSUES are
// touched; prose and DECISIONS pass through.
func rcWithoutSpeculativeIssues(draft string) (string, []rcAllegationResult) {
	start := 0
	if i := strings.Index(draft, rcReviewDataMarker); i >= 0 {
		start = i + len(rcReviewDataMarker)
	}
	lines := strings.Split(draft[start:], "\n")
	kept := lines[:0:0]
	var dropped []rcAllegationResult
	section := ""
	for _, line := range lines {
		t := strings.TrimSpace(line)
		// The headers rcParseReviewOutput switches on, and only those: a
		// bullet like "- a.go:12 — …" also has a colon, and is not a header.
		if key, _, labelled := rcMachineLine(t); labelled {
			switch key {
			case "issues", "findings":
				section = "issues"
				kept = append(kept, line)
				continue
			case "decisions":
				section = "decisions"
				kept = append(kept, line)
				continue
			case "intent_match", "merge_ready", "summary", "note":
				section = ""
				kept = append(kept, line)
				continue
			}
		}
		if section == "issues" {
			bullet := rcUnwrapMachineBullet(t)
			if strings.HasPrefix(bullet, "-") {
				item := strings.TrimSpace(strings.TrimPrefix(bullet, "-"))
				if phrase, ok := rcSpeculativePhrase(item); ok {
					fmt.Fprintf(os.Stderr, "  dropping speculative issue (%q) — %s\n", phrase, rcOneLine(item, 80))
					dropped = append(dropped, rcAllegationResult{
						Issue:  item,
						Status: rcStatusRefuted,
						Reason: fmt.Sprintf("hypothetical trigger: the failure needs a change nobody has made (%q), so it is not a defect in this change", phrase),
					})
					continue
				}
			}
		}
		kept = append(kept, line)
	}
	if len(dropped) == 0 {
		return draft, nil
	}
	return draft[:start] + strings.Join(kept, "\n"), dropped
}

// rcReviewWithoutSpeculation is the published review of a draft whose every
// ISSUE was speculative and which has no DECISIONS: nothing is left for the
// gate to check, but the draft itself cannot be published either — its prose,
// SUMMARY and MERGE_READY were written about the concerns that were just
// refuted, so it would say "two defects, small fixes first" over an empty
// list. It is assembled the way the gate assembles every review, from the
// verdicts: no finding, the refutations counted, a summary that says so, and
// MERGE_READY 4 — concerns judged not to be defects do not count against the
// score, and not 5, because nothing re-examined the change. ok is false when
// the draft still has an issue or a decision, which the gate then handles.
func rcReviewWithoutSpeculation(draft string, speculative []rcAllegationResult) (string, bool) {
	_, issues, decisions, match, readiness, _ := rcParseReviewOutput(draft)
	if len(speculative) == 0 || len(issues) > 0 || len(decisions) > 0 {
		return "", false
	}
	// Always 4: the draft's own score was given with the refuted concerns in
	// it, so neither a lower one nor a "merge it" 5 describes this review.
	_ = readiness
	readiness = finding.Readiness(4)
	summary := fmt.Sprintf("No defect survived review: %d concern(s) raised needed a change nobody has made to trigger, so none is reported.", len(speculative))
	return rcAssembleReview(nil, nil, speculative, nil, match, readiness, summary), true
}
