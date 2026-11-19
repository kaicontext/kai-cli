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
	"fmt"
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
	// Paths are compared unquoted and returned as changed spells them.
	spell := rcPRFiles(prior.Head, changed)
	inPR := make(map[string]bool, len(spell))
	for f := range spell {
		inPR[f] = true
	}
	focus := map[string]bool{}
	add := func(f string) {
		if s, ok := spell[rcGitUnquote(f)]; ok {
			focus[s] = true
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
	unchanged := false
	for i, c := range commits {
		files, ok := since(c)
		if !ok {
			return nil // a commit this clone does not have: review it all
		}
		// Nothing the pull request changes differs from the previous review's
		// commit: the code under review is the code already reviewed. The
		// re-review re-checks the earlier points in their own files and
		// nothing else (rcReReviewUnchanged); an older point's own commit
		// adds nothing, since the previous review already read from there.
		// A change taken back out of the pull request is a change, though,
		// and what it leaves behind is reviewed whole.
		if i == 0 && !rcAnyIn(files, inPR) {
			if len(rcDroppedSinceFn(c, changed)) > 0 {
				return nil
			}
			unchanged = true
			break
		}
		for _, f := range files {
			add(f)
		}
	}
	// The files earlier points name, so a point is re-checked in its own file
	// even when that file did not change.
	for _, pt := range prior.Points {
		for _, loc := range rcIssueLocations(pt.Text) {
			add(loc.path)
		}
	}
	// A re-check stays a re-check even when the earlier points name every
	// file; otherwise a focus that is the whole change is the whole change.
	// Counted like the focus, through spell: a rename is both its paths in
	// each, where changed (from --numstat) lists it once.
	if !unchanged && len(focus) >= len(spell) {
		return nil
	}
	out := make([]string, 0, len(focus)) // empty, not nil, when only re-checking
	for f := range focus {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func rcAnyIn(files []string, set map[string]bool) bool {
	for _, f := range files {
		if set[rcGitUnquote(f)] {
			return true
		}
	}
	return false
}

// rcReReviewUnchanged reports whether a re-review's code is the code the
// previous review read: nothing the pull request changes differs since its
// commit. Such a review only re-checks the earlier points, on a small budget.
func rcReReviewUnchanged(prior *rcPrior, changed []string, since func(string) ([]string, bool)) bool {
	if prior == nil || len(changed) == 0 || rcReScopeOff() {
		return false
	}
	files, ok := since(prior.Head)
	if !ok {
		return false
	}
	inPR := map[string]bool{}
	for f := range rcPRFiles(prior.Head, changed) {
		inPR[f] = true
	}
	return !rcAnyIn(files, inPR) && len(rcDroppedSinceFn(prior.Head, changed)) == 0
}

// rcReCheckTurns is the budget of a re-review whose code has not changed: a
// few turns, and three per file an earlier point names, never more than a
// full review's base.
const rcReCheckBaseTurns = 8

func rcReCheckTurns(pointFiles int) int {
	n := rcReCheckBaseTurns + rcReviewTurnsPerFile*pointFiles
	if n > rcReviewBaseTurns {
		return rcReviewBaseTurns
	}
	return n
}

// rcReCheckBlock replaces the RE-REVIEW FOCUS block when the code has not
// changed since the previous review. baseChained: RE-REVIEW RELATED FILES
// names base changes the pull request calls (rcReCheckBaseChain).
func rcReCheckBlock(prior *rcPrior, pointFiles []string, turns int, baseChained bool) string {
	where := "Open the files the earlier points name"
	if len(pointFiles) > 0 {
		where = "Open the files the earlier points name (" + strings.Join(pointFiles, ", ") + ")"
	}
	// Only point at base changes when a block below names some
	// (rcBaseMovedBlock, rcReReviewChainBlock).
	var blocks []string
	if baseChained {
		blocks = append(blocks, "RE-REVIEW RELATED FILES")
	}
	if len(rcBaseOnlyOutsideFocus(prior, pointFiles)) > 0 {
		blocks = append(blocks, "BASE MOVED")
	}
	base := "Report no new issue: there is no new code to find one in."
	if len(blocks) > 0 {
		base = "The base under the pull request moved (" + strings.Join(blocks, " and ") + ", below): report a new issue only where one of those base changes now meets the pull request's code."
	}
	return fmt.Sprintf("RE-REVIEW, NOTHING CHANGED: the pull request's own changes are identical to the commit you reviewed last time (%s). "+
		"You have %d turns. Do not review the change again: it was reviewed at that commit. "+
		"%s and re-check each earlier point against the code as it is now, stating whether it still holds. %s\n\n",
		rcShort(prior.Head), turns, where, base)
}

// rcBaseOnlyOutsideFocus is the base-moved files that are not in the focus. An
// earlier point's file is added to the focus by rcReReviewFocus, so a base-only
// file can be in it; such a file is reviewed, not outside the focus.
func rcBaseOnlyOutsideFocus(prior *rcPrior, focus []string) []string {
	if prior == nil {
		return nil
	}
	inFocus := map[string]bool{}
	for _, f := range focus {
		inFocus[rcGitUnquote(f)] = true
	}
	var out []string
	for _, f := range rcBaseOnlySince(prior.Head) {
		if !inFocus[f] {
			out = append(out, f)
		}
	}
	return out
}

// rcBaseMovedBlock names the files of the pull request that changed only
// because the base moved under them since the previous review. "" when none.
// The "not in the focus" sentence is omitted on a whole-change review (focus
// nil), where there is no focus.
func rcBaseMovedBlock(prior *rcPrior, focus []string) string {
	if prior == nil {
		return ""
	}
	files := rcBaseOnlyOutsideFocus(prior, focus)
	if len(files) == 0 {
		return ""
	}
	more := ""
	if len(files) > 20 {
		files, more = files[:20], fmt.Sprintf(" … and %d more", len(files)-20)
	}
	note := ""
	if focus != nil {
		note = "They are not in the focus. "
	}
	return fmt.Sprintf("BASE MOVED: since %s these files of the pull request changed only because the base branch changed them; "+
		"the pull request's own changes to them are the same as before: %s%s. %s"+
		"Open one only where a base change touches code the pull request's change relies on.\n\n",
		rcShort(prior.Head), strings.Join(files, ", "), more, note)
}

// rcReReviewFocusBlock tells a scoped re-review what it is sized to, so the
// prompt states the turns the run really has, and what the files outside the
// focus are: reviewed already, not out of scope. The reviewer is pointed at
// them through the change itself (its callers, dependents, shared keys), which
// is where a fix breaks code that did not change. "" when the review covers
// the whole change.
func rcReReviewFocusBlock(prior *rcPrior, focus []string, changed, turns int) string {
	if prior == nil || focus == nil {
		return ""
	}
	return fmt.Sprintf("RE-REVIEW FOCUS: you have %d turns, sized to the %d of %d changed files that need reading again: %s. "+
		"The other changed files were reviewed at %s and have not changed since: they are reviewed, not out of scope, and the whole diff is below. "+
		"Read the focus files first. Then follow what changed into the rest: where a focus file changes a signature, return value, error, nil case, "+
		"shared state, key, route or setting that another file uses (kai_callers, kai_dependents, kai_grep), open that use and check it, "+
		"changed in this pull request or not. A fix that breaks code you already reviewed is the defect a re-review is for.\n\n",
		turns, len(focus), changed, strings.Join(focus, ", "), rcShort(prior.Head))
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
