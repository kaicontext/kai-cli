package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The publish gate: a defect the check confirmed and the selection chose is
// published only when it sits in the change and, when enough proposers ran,
// when more than one proposal raised it.
//
// Benchmark run 43 (2026-10-23): a published finding that no other proposal
// repeated matched the answer key 15% of the time, below the ~25% at which
// one more comment pays for itself; raised two or more times, 45–80%. A
// finding whose every cited line sat more than 3 lines from the change hit
// 12%. KAI_REVIEW_PUBLISH_GATE=0 turns the gate off.

// rcPublishRadius is how close to a changed line a published finding must be.
const rcPublishRadius = 3

// rcDuplicateOfRe reads the check's fold of a repeat ("Duplicate of issue N").
var rcDuplicateOfRe = regexp.MustCompile(`^Duplicate of issue \[?(\d+)`)

// rcRepeatOfRe reads the selection step's fold of a repeat, after its prefix
// ("repeat of N", "covered by N").
var rcRepeatOfRe = regexp.MustCompile(`(?i)^(?:repeat of|covered by)\s*\[?(\d+)`)

// rcPublishGate withholds published allegations that cite no line near the
// change and, when corroborate is set, those only one proposal raised. A
// withheld one keeps its record with the selection step's prefix, so it reads
// as "not published", not as refuted. A review that had a published defect
// keeps at least one: the selection's first.
func rcPublishGate(res *rcChallengeResult, repo *rcRepo, corroborate bool) {
	if res == nil || os.Getenv("KAI_REVIEW_PUBLISH_GATE") == "0" {
		return
	}
	rcCountVotes(res)
	ranged := repo != nil && len(repo.changed) > 0
	first, firstReason := -1, ""
	total, kept, single, outside, distinct := 0, 0, 0, 0, 0
	for i := range res.Allegations {
		a := &res.Allegations[i]
		if a.Status != rcStatusSupported {
			continue
		}
		total++
		if len(a.Proposers) >= 2 {
			distinct++
		}
		if first < 0 {
			first, firstReason = i, a.Reason
		}
		why := rcGateWhy(a, repo, ranged, corroborate)
		switch {
		case strings.HasPrefix(why, "outside"):
			outside++
		case why != "":
			single++
		default:
			kept++
			continue
		}
		a.keptReason = a.Reason
		a.Status, a.Reason = rcStatusRefuted, rcSelectionSkipPrefix+why
	}
	if first < 0 {
		return
	}
	fallback := kept == 0
	if fallback {
		res.Allegations[first].Status, res.Allegations[first].Reason = rcStatusSupported, firstReason
		kept = 1
	}
	refilled := 0
	if rcRefillOn() && !fallback {
		refilled = rcRefillFromCap(res, repo, ranged, corroborate, outside+single)
		kept += refilled
	}
	if outside+single > 0 && res.match != "" {
		rcFinalize(res)
	}
	fmt.Fprintf(os.Stderr, "  publish gate: kept %d of %d (single-proposal %d, outside change %d, fallback %v; corroboration %v; distinct finders >=2: %d of %d)\n",
		kept, total, single, outside, fallback, corroborate, distinct, total)
	if refilled > 0 {
		fmt.Fprintf(os.Stderr, "  refill: %d defect(s) the cap had left out took the slots the gate freed\n", refilled)
	}
}

// rcGateWhy is the gate's reason for withholding a published allegation, or
// "" when it passes.
func rcGateWhy(a *rcAllegationResult, repo *rcRepo, ranged, corroborate bool) string {
	switch {
	case ranged && !rcNearChange(a.Issue, repo):
		return fmt.Sprintf("outside the change: no cited line is within %d lines of a changed line", rcPublishRadius)
	case corroborate && a.Votes < 2:
		return "raised by one proposal only"
	}
	return ""
}

// rcRefillOn reports whether the refill is on. It follows the review depth
// the server chose (KAI_REVIEW_TIER): on for standard, deep and auto (auto
// runs Standard for a change rated S or M and Deep for the rest, so it only
// ever lands on a depth that has the refill), off for light and for every
// other value, including unset (free, baseline, or a repo running its own
// workflow). KAI_REVIEW_REFILL=0 turns it off in every depth and
// KAI_REVIEW_REFILL=1 forces it on.
func rcRefillOn() bool {
	switch strings.TrimSpace(os.Getenv("KAI_REVIEW_REFILL")) {
	case "0":
		return false
	case "1":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAI_REVIEW_TIER"))) {
	case "standard", "deep", "auto":
		return true
	}
	return false
}

// rcRefillFromCap gives the slots the gate freed back to the allegations the
// cap left out. The cap runs before the gate, so a defect it cut never came
// back when the gate then withheld one of the published ones; here the cut
// ones, in the selection's order, take the freed slots when they pass the same
// gate. Returns how many were brought back.
func rcRefillFromCap(res *rcChallengeResult, repo *rcRepo, ranged, corroborate bool, slots int) int {
	var cut []int
	for i, a := range res.Allegations {
		if a.Status == rcStatusRefuted && strings.HasPrefix(a.Reason, rcSelectionSkipPrefix+"outside the ") &&
			strings.Contains(a.Reason, "most important") && a.rankPos > 0 {
			cut = append(cut, i)
		}
	}
	sort.SliceStable(cut, func(x, y int) bool { return res.Allegations[cut[x]].rankPos < res.Allegations[cut[y]].rankPos })
	back := 0
	for _, i := range cut {
		if back == slots {
			break
		}
		a := &res.Allegations[i]
		if rcGateWhy(a, repo, ranged, corroborate) != "" {
			continue
		}
		a.Status, a.Reason = rcStatusSupported, a.keptReason
		back++
	}
	return back
}

// rcCountVotes sets each allegation's Votes: how many allegations the check's
// duplicates and the selection's repeats link to it, itself included. It also
// widens each allegation's Proposers to every finder behind its group
// (review_commit_finders.go), and sets OriginStage from the stages behind
// the group; the gate does not decide on either.
func rcCountVotes(res *rcChallengeResult) {
	byID := map[int]int{}
	parent := make([]int, len(res.Allegations))
	for i, a := range res.Allegations {
		byID[a.ID], parent[i] = i, i
	}
	find := func(i int) int {
		for parent[i] != i {
			i = parent[i]
		}
		return i
	}
	link := func(i, id int) {
		if j, ok := byID[id]; ok && id != 0 {
			parent[find(i)] = find(j)
		}
	}
	for i, a := range res.Allegations {
		link(i, a.DuplicateOf)
		if why, ok := strings.CutPrefix(a.Reason, rcSelectionSkipPrefix); ok {
			if m := rcRepeatOfRe.FindStringSubmatch(why); m != nil {
				id, _ := strconv.Atoi(m[1])
				link(i, id)
			}
		}
	}
	size := map[int]int{}
	finders := map[int]map[string]bool{}
	stages := map[int]map[string]bool{}
	for i, a := range res.Allegations {
		r := find(i)
		size[r]++
		for _, f := range a.Proposers {
			if finders[r] == nil {
				finders[r] = map[string]bool{}
			}
			finders[r][f] = true
		}
		for _, s := range a.stages {
			if stages[r] == nil {
				stages[r] = map[string]bool{}
			}
			stages[r][s] = true
		}
	}
	for i := range res.Allegations {
		r := find(i)
		a := &res.Allegations[i]
		a.Votes = size[r]
		a.Proposers = rcSortedFinders(finders[r])
		a.stages = rcOriginSorted(stages[r])
		a.OriginStage = ""
		if len(a.stages) > 0 {
			a.OriginStage = a.stages[0]
		}
	}
}

// rcNearChange reports whether any location an issue cites is within
// rcPublishRadius lines of a changed line. An issue with no location is kept.
func rcNearChange(issue string, repo *rcRepo) bool {
	locs := rcIssueLocations(issue)
	if len(locs) == 0 {
		return true
	}
	for _, l := range locs {
		if _, ok := repo.changedNear(l.path, l.line, rcPublishRadius); ok {
			return true
		}
	}
	return false
}
