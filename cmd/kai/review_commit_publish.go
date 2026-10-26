package main

import (
	"fmt"
	"os"
	"regexp"
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
	total, kept, single, outside := 0, 0, 0, 0
	for i := range res.Allegations {
		a := &res.Allegations[i]
		if a.Status != rcStatusSupported {
			continue
		}
		total++
		if first < 0 {
			first, firstReason = i, a.Reason
		}
		var why string
		switch {
		case ranged && !rcNearChange(a.Issue, repo):
			why = fmt.Sprintf("outside the change: no cited line is within %d lines of a changed line", rcPublishRadius)
			outside++
		case corroborate && a.Votes < 2:
			why = "raised by one proposal only"
			single++
		default:
			kept++
			continue
		}
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
	if outside+single > 0 && res.match != "" {
		rcFinalize(res)
	}
	fmt.Fprintf(os.Stderr, "  publish gate: kept %d of %d (single-proposal %d, outside change %d, fallback %v; corroboration %v)\n",
		kept, total, single, outside, fallback, corroborate)
}

// rcCountVotes sets each allegation's Votes: how many allegations the check's
// duplicates and the selection's repeats link to it, itself included.
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
	for i := range res.Allegations {
		size[find(i)]++
	}
	for i := range res.Allegations {
		res.Allegations[i].Votes = size[find(i)]
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
