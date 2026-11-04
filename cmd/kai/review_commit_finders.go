package main

// Which finders raised each finding.
//
// A review's ISSUES come from up to four finders: the main reviewer, the
// second finder, and two diff sweeps. When two of them state one defect, the
// merges keep one bullet and drop the other (rcDraftWithSweep for the second
// finder and the sweeps, rcMergeSweeps between the sweeps), so the agreement
// is lost before the publish gate counts votes from the check's duplicates
// and the selection's repeats (review_commit_publish.go). On benchmark run 47
// (2026-10-26) 88 bullets were dropped that way, and the gate's count is of
// linked bullets, not of distinct finders.
//
// The ledger records, at each merge, which finders proposed each bullet, a
// dropped repeat crediting the bullet it repeats. Each allegation then
// carries the distinct finders behind it, over its vote group. This is a
// record for measurement: the gate still decides on Votes.

import (
	"fmt"
	"sort"
	"strings"
)

// The finders, in the order a finding lists them.
const (
	rcFinderMain    = "main"
	rcFinderSecond  = "finder2"
	rcFinderSweep   = "sweep"
	rcFinderSweep2  = "sweep2"
	rcFinderUnknown = ""
)

var rcFinderOrder = map[string]int{rcFinderMain: 0, rcFinderSecond: 1, rcFinderSweep: 2, rcFinderSweep2: 3}

// rcFinderLedger maps each proposed bullet (by rcFinderKey) to the finders
// that proposed it. A nil ledger records nothing.
type rcFinderLedger struct {
	by map[string]map[string]bool
	// at lists the keys proposed at each "path:line".
	at map[string][]string
}

func newRcFinderLedger() *rcFinderLedger {
	return &rcFinderLedger{by: map[string]map[string]bool{}, at: map[string][]string{}}
}

// rcFinderKey is the key a bullet is recorded by: its location and its
// sentence, normalised, without tags or an "(also: …)" list, so a bullet
// matches after the tags come off and the check takes it as an allegation.
// loc is its "path:line", or "" when it has none.
func rcFinderKey(item string) (key, loc string) {
	item = strings.TrimSpace(item)
	item = strings.TrimSpace(strings.TrimPrefix(item, "- "))
	item = rcEarlierTagRe.ReplaceAllString(item, "")
	sentence := rcIssueKey(rcIssueSentence(rcAlsoListRe.ReplaceAllString(item, "")))
	where, ok := rcEarlierKey(item)
	if !ok {
		return "|" + sentence, ""
	}
	return where + "|" + sentence, where
}

// add records that finders proposed item.
func (l *rcFinderLedger) add(item string, finders ...string) {
	if l == nil || strings.TrimSpace(item) == "" {
		return
	}
	key, loc := rcFinderKey(item)
	set := l.by[key]
	if set == nil {
		set = map[string]bool{}
		l.by[key] = set
		if loc != "" {
			l.at[loc] = append(l.at[loc], key)
		}
	}
	for _, f := range finders {
		if f != rcFinderUnknown {
			set[f] = true
		}
	}
}

// addAll records that finder proposed each of items.
func (l *rcFinderLedger) addAll(items []string, finder string) {
	for _, is := range items {
		l.add(is, finder)
	}
}

// merged records the result of rcDraftWithSweepMatched: each extra issue's
// finders credit the bullet it repeated, or the issue itself when it was
// added. by gives an issue's finders (nil: fallback for all).
func (l *rcFinderLedger) merged(extra, matched []string, by map[string][]string, fallback string) {
	if l == nil {
		return
	}
	for i, is := range extra {
		finders := []string{fallback}
		if by != nil {
			if f, ok := by[rcFinderKeyOnly(is)]; ok && len(f) > 0 {
				finders = f
			}
		}
		target := is
		if i < len(matched) && matched[i] != "" {
			target = matched[i]
		}
		l.add(target, finders...)
	}
}

// of is the finders behind issue: its own bullet's, plus, for each location
// it names (its lead and its "(also: …)" list), those of the one bullet
// proposed there when only one was. The fold that writes "(also: …)" drops
// the bullet it folds, so its finders are found by the location it leaves.
func (l *rcFinderLedger) of(issue string) []string {
	if l == nil {
		return nil
	}
	set := map[string]bool{}
	key, _ := rcFinderKey(issue)
	for f := range l.by[key] {
		set[f] = true
	}
	for _, loc := range rcIssueLocations(issue) {
		keys := l.at[fmt.Sprintf("%s:%d", loc.path, loc.line)]
		if len(keys) == 1 {
			for f := range l.by[keys[0]] {
				set[f] = true
			}
		}
	}
	return rcSortedFinders(set)
}

// rcFinderKeyOnly is rcFinderKey's key alone.
func rcFinderKeyOnly(item string) string {
	key, _ := rcFinderKey(item)
	return key
}

// rcFindersOf is a sweep's By: every issue proposed by finder.
func rcFindersOf(issues []string, finder string) map[string][]string {
	if len(issues) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, is := range issues {
		out[rcFinderKeyOnly(is)] = []string{finder}
	}
	return out
}

// rcUnionFinders joins two sweeps' By maps.
func rcUnionFinders(a, b map[string][]string) map[string][]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	sets := map[string]map[string]bool{}
	for _, m := range []map[string][]string{a, b} {
		for k, fs := range m {
			if sets[k] == nil {
				sets[k] = map[string]bool{}
			}
			for _, f := range fs {
				sets[k][f] = true
			}
		}
	}
	out := map[string][]string{}
	for k, s := range sets {
		out[k] = rcSortedFinders(s)
	}
	return out
}

// rcSortedFinders lists a set of finders in rcFinderOrder.
func rcSortedFinders(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, iok := rcFinderOrder[out[i]]
		oj, jok := rcFinderOrder[out[j]]
		if iok != jok {
			return iok
		}
		if oi != oj {
			return oi < oj
		}
		return out[i] < out[j]
	})
	return out
}

// rcRecordProposers sets each allegation's Proposers from the ledger: the
// finders of its own bullet. rcCountVotes widens them to the vote group.
// Returns how many allegations the ledger knew.
func rcRecordProposers(res *rcChallengeResult, l *rcFinderLedger) int {
	if res == nil || l == nil {
		return 0
	}
	known := 0
	for i := range res.Allegations {
		a := &res.Allegations[i]
		a.Proposers = l.of(a.Issue)
		if len(a.Proposers) > 0 {
			known++
		}
	}
	return known
}
