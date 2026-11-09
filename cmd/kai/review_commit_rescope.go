package main

// review_commit_rescope.go — a re-review pays for what changed, not for the
// whole pull request again.
//
// A re-review already starts from the previous review (review_commit_prior.go):
// it re-checks each earlier point and then reviews what changed since. But its
// budgets were still sized to the whole pull request. The turn budget grew
// with every file the PR touches, the coverage gate asked for every file the
// reviewer had not opened, and both sweeps read the whole diff again. So a
// one-file fix pushed to a twenty-file PR cost about as much as the first
// review.
//
// On kaicontext/kai-server's 2026-11-09 sample of one customer's pushes
// (18 re-reviews in three days), 12 found nothing new and 6 found one point
// each, and the new points were in the code the fix pushes changed.
//
// So on a re-review the FOCUS is the files that need reading again: those
// changed since the previous review (or since the commit an older open point
// was raised at), plus the files an earlier point names. The turn budget, the
// coverage gate and both sweeps are sized to the focus. Nothing else is
// narrowed: the reviewer still gets the whole diff in its prompt and can open
// any file, so a fix that breaks an unchanged caller can still be followed.
//
// Whenever the focus cannot be worked out reliably the review covers the whole
// change, as before: no previous review, a previous commit missing from the
// clone (a force-push), nothing changed since, or a focus that is the whole
// change anyway. KAI_REVIEW_RESCOPE=off turns it off.

import (
	"os"
	"sort"
	"strings"
)

// rcReReviewFocus returns the changed files a re-review must read again, or
// nil when it should cover the whole change. changed is the pull request's
// changed files; since lists the files that differ between a commit and HEAD
// (rcChangedSince), with false when it cannot tell.
func rcReReviewFocus(prior *rcPrior, changed []string, since func(string) ([]string, bool)) []string {
	if prior == nil || len(changed) == 0 || rcReScopeOff() {
		return nil
	}
	inPR := make(map[string]bool, len(changed))
	for _, f := range changed {
		inPR[f] = true
	}
	focus := map[string]bool{}
	add := func(f string) {
		if inPR[f] {
			focus[f] = true
		}
	}

	// The commits to diff from: the previous review's, and each older open
	// point's own, since its fix is checked from there.
	commits := []string{prior.Head}
	seen := map[string]bool{prior.Head: true}
	for _, pt := range prior.Points {
		if s := rcPointSince(prior, pt); !seen[s] {
			seen[s] = true
			commits = append(commits, s)
		}
	}
	for _, c := range commits {
		files, ok := since(c)
		if !ok {
			return nil // a commit this clone does not have: review it all
		}
		for _, f := range files {
			add(f)
		}
	}
	// Nothing changed since: the review is a re-check of the same code. Leave
	// it whole rather than size it to nothing.
	if len(focus) == 0 {
		return nil
	}
	// The files earlier points name, so a point is re-checked in its own file
	// even when that file did not change.
	for _, pt := range prior.Points {
		for _, loc := range rcIssueLocations(pt.Text) {
			add(loc.path)
		}
	}
	if len(focus) >= len(changed) {
		return nil
	}
	out := make([]string, 0, len(focus))
	for f := range focus {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// rcReScopeOff reports whether KAI_REVIEW_RESCOPE turns scoping off.
func rcReScopeOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAI_REVIEW_RESCOPE"))) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// rcReviewScope is what the review's budgets are sized to: the focus on a
// scoped re-review, else every changed file.
func rcReviewScope(changed, focus []string) []string {
	if focus != nil {
		return focus
	}
	return changed
}

// rcOnlyPatches keeps the sweep's per-file patches for the focus files, in
// their diff order. A nil focus keeps everything.
func rcOnlyPatches(order []string, patches map[string]string, focus []string) ([]string, map[string]string) {
	if focus == nil {
		return order, patches
	}
	keep := make(map[string]bool, len(focus))
	for _, f := range focus {
		keep[f] = true
	}
	var o []string
	p := map[string]string{}
	for _, f := range order {
		if keep[f] {
			o = append(o, f)
			p[f] = patches[f]
		}
	}
	return o, p
}
