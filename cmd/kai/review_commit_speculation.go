package main

import (
	"fmt"
	"os"
	"strings"
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
// The phrases are the future-change wording itself, not hedges in general: a
// hedge ("appears", "may") is the fast pass's filter; here the reviewer is
// sure, and sure about a trigger that does not exist yet. Each was checked
// against ordinary defect sentences in TestSpeculativePhraseLeavesRealDefectsAlone.
var rcSpeculativePhrases = []string{
	"dormant today", "dormant for now", "dormant until",
	"latent footgun", "footgun for future", "footgun if",
	"is ever added", "are ever added", "is ever introduced", "are ever introduced",
	"is ever reordered", "are ever reordered", "is ever changed to", "is ever refactored",
	"if someone later", "if someone ever", "should anyone later", "if a future",
	"a future change", "a future refactor", "a future caller", "future strict",
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
