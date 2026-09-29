package main

import (
	"fmt"
	"regexp"
)

// Refutations the gate's own words, or the diff, contradict.
//
// Run 5 of the 2026-09-28 benchmark lost real findings to refutations that
// were wrong on their face: one reason read "This IS a defect. The allegation
// is correct." under a refuted verdict, and others dismissed a defect as
// "pre-existing" / "unchanged context" on a line inside the very function the
// change rewrote. Both are checked in code. The gate gets one correction; a
// refutation that still contradicts itself is not allowed to clear the
// allegation — it becomes unresolved.

// rcAffirmingReason matches a reason that concludes the allegation holds.
// The phrases are affirmative on their own ("is a defect", not "would be a
// defect if"); a negation between the words breaks the match.
var rcAffirmingReason = regexp.MustCompile(`(?i)\b(?:this|it|that)\s+(?:is|IS)\s+(?:indeed\s+|clearly\s+)?an?\s+(?:real\s+|genuine\s+|actual\s+|valid\s+)?(?:defect|bug)\b|\ballegation\s+is\s+(?:correct|accurate|valid|right)\b|\b(?:the\s+)?(?:defect|bug)\s+is\s+real\b|\bthe\s+(?:allegation|claim)\s+holds\b`)

// rcNegatedAffirmation catches the common negated forms the affirmation
// pattern would otherwise accept inside a longer sentence.
var rcNegatedAffirmation = regexp.MustCompile(`(?i)\b(?:not|isn't|is\s+not|no\s+longer)\s+(?:a\s+|an\s+)?(?:real\s+|genuine\s+)?(?:defect|bug)\b|\ballegation\s+is\s+(?:not|in)correct\b`)

// rcPreexistingReason matches a refutation that rests on the code predating
// the change.
var rcPreexistingReason = regexp.MustCompile(`(?i)pre-?existing|predates\s+this\s+change|unchanged\s+(?:context|code|line|lines)|not\s+(?:introduced|changed|touched|modified)\s+(?:by|in)\s+this\s+(?:change|pr|diff|commit)|not\s+this\s+change'?s\s+(?:defect|code|bug)|already\s+(?:present|existed)\s+before`)

// rcVerdictProblem is one refutation to send back.
type rcVerdictProblem struct {
	Index int // into res.Allegations
	Text  string
}

// rcVerdictProblems lists refuted allegations whose reason affirms the defect,
// or dismisses it as pre-existing although it sits within rcNearChangeRadius
// lines of a line the change touched.
func rcVerdictProblems(res *rcChallengeResult, repo *rcRepo) []rcVerdictProblem {
	if res == nil {
		return nil
	}
	var out []rcVerdictProblem
	for i, a := range res.Allegations {
		if a.Status != rcStatusRefuted {
			continue
		}
		if rcAffirmingReason.MatchString(a.Reason) && !rcNegatedAffirmation.MatchString(a.Reason) {
			out = append(out, rcVerdictProblem{i, fmt.Sprintf("check %d is refuted, but its reason says the allegation is correct (%q). A reason that concludes the defect is real belongs to a supported verdict; decide which the evidence shows and make verdict and reason agree.", i+1, rcClip(a.Reason, 160))})
			continue
		}
		if !rcPreexistingReason.MatchString(a.Reason) {
			continue
		}
		file, line, ok := rcIssueLocation(a.Issue)
		if !ok {
			continue
		}
		if rg, near := repo.changedNear(file, line, rcNearChangeRadius); near {
			out = append(out, rcVerdictProblem{i, fmt.Sprintf("check %d is refuted as pre-existing, but %s:%d is within %d lines of lines this change modifies (%d-%d). Code in a function the change modifies, or that its new code reaches or relies on, is in scope: judge the allegation on its merits.", i+1, file, line, rcNearChangeRadius, rg[0], rg[1])})
		}
	}
	return out
}

// rcDemoteVerdicts turns the listed refutations into unresolved allegations:
// a refutation that contradicts itself or the diff cannot clear a defect.
func rcDemoteVerdicts(res *rcChallengeResult, problems []rcVerdictProblem) {
	if res == nil || len(problems) == 0 {
		return
	}
	for _, p := range problems {
		a := &res.Allegations[p.Index]
		a.Status = rcStatusUnresolved
		a.Reason = "the refutation contradicted itself or the diff and was not corrected: " + a.Reason
	}
	if res.match != "" {
		rcFinalize(res)
	}
}

func rcClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
