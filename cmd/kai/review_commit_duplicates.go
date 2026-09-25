package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// One root cause, one finding. On the 2026-09-24 benchmark Kai posted the same
// refreshOAuthTokens return-shape mismatch three times on Cal.com #11059 — once
// for HubSpot, then "same Axios/Response mismatch" for Zoho Bigin and again for
// Zoho CRM — so one defect cost three findings of precision and read as three
// problems. The prompt now asks for one ISSUE per cause with its other places
// listed as "(also: path:line, …)"; rcMergeDuplicateIssues enforces it for the
// two shapes a duplicate actually takes in a draft:
//
//   - a bullet that says it is a repeat ("same …", "likewise", "as above"),
//     which is folded into the bullet before it;
//   - a bullet whose sentence is word for word another's at a different
//     location, which is folded into the first.
//
// Anything subtler (two different sentences about one cause) is left to the
// reviewer and the gate: merging on similarity would fold distinct defects
// that happen to share vocabulary.

// rcRepeatOpeners are the ways a bullet announces it repeats the one before.
var rcRepeatOpeners = []string{
	"same ", "the same ", "same:", "same;", "same.", "same,", "same —", "same -",
	"likewise", "similarly", "as above", "as in ", "ditto", "identical ",
}

// rcMinFoldWords is the shortest sentence folded for being repeated word for
// word. A terse repeat ("missing error check") is a pattern, not proof of one
// cause; a sentence long enough to describe a mechanism, repeated verbatim at
// another place, is the same failure described twice.
const rcMinFoldWords = 8

var rcIssueLeadRe = regexp.MustCompile(`^\s*` + "`?" + `[A-Za-z0-9_./\-]+\.[A-Za-z0-9]+:\d+[0-9,\- ]*` + "`?" + `\s*(?:—|–|--|-|:)?\s*`)

// rcIssueSentence is a bullet without its leading location.
func rcIssueSentence(item string) string {
	return strings.TrimSpace(rcIssueLeadRe.ReplaceAllString(item, ""))
}

func rcIsRepeat(sentence string) bool {
	s := strings.ToLower(strings.TrimLeft(sentence, "`*_ "))
	for _, o := range rcRepeatOpeners {
		if strings.HasPrefix(s, o) {
			return true
		}
	}
	return false
}

func rcNormalizedSentence(sentence string) string {
	s := strings.ToLower(strings.NewReplacer("`", "", "*", "").Replace(sentence))
	return strings.Join(strings.Fields(s), " ")
}

// rcWithAlso appends locations to a bullet's "(also: …)" list, creating it
// when the bullet has none.
func rcWithAlso(item string, locs []string) string {
	if len(locs) == 0 {
		return item
	}
	extra := strings.Join(locs, ", ")
	if i := strings.LastIndex(item, "(also:"); i >= 0 {
		if j := strings.Index(item[i:], ")"); j >= 0 {
			return item[:i+j] + ", " + extra + item[i+j:]
		}
	}
	return strings.TrimRight(item, " .") + " (also: " + extra + ")"
}

// rcMergeDuplicateIssues folds repeated ISSUES bullets into the one they
// repeat and returns the draft with one bullet per cause, plus a note per
// fold for the log. Like rcWithoutSpeculativeIssues it runs before the
// publication gate and touches only the ISSUES list.
func rcMergeDuplicateIssues(draft string) (string, []string) {
	start := 0
	if i := strings.Index(draft, rcReviewDataMarker); i >= 0 {
		start = i + len(rcReviewDataMarker)
	}
	lines := strings.Split(draft[start:], "\n")

	type bullet struct {
		line int      // index in lines
		item string   // text after "- "
		also []string // locations folded into it
	}
	var bullets []*bullet
	bySentence := map[string]*bullet{}
	drop := map[int]bool{}
	var notes []string
	section := ""
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if key, _, labelled := rcMachineLine(t); labelled {
			switch key {
			case "issues", "findings":
				section = "issues"
				continue
			case "decisions", "intent_match", "merge_ready", "summary", "note":
				section = ""
				continue
			}
		}
		if section != "issues" {
			continue
		}
		b := rcUnwrapMachineBullet(t)
		if !strings.HasPrefix(b, "-") {
			continue
		}
		item := strings.TrimSpace(strings.TrimPrefix(b, "-"))
		if item == "" || rcIsEmptyListItem(item) {
			continue
		}
		path, ln, hasLoc := rcIssueLocation(item)
		sentence := rcIssueSentence(item)
		norm := rcNormalizedSentence(sentence)
		var into *bullet
		switch {
		case !hasLoc:
		case rcIsRepeat(sentence) && len(bullets) > 0:
			into = bullets[len(bullets)-1]
		case bySentence[norm] != nil && len(strings.Fields(norm)) >= rcMinFoldWords:
			into = bySentence[norm]
		}
		if into != nil {
			loc := fmt.Sprintf("%s:%d", path, ln)
			into.also = append(into.also, loc)
			drop[i] = true
			notes = append(notes, fmt.Sprintf("%s folded into %s", loc, rcOneLine(into.item, 60)))
			fmt.Fprintf(os.Stderr, "  merging duplicate issue at %s into — %s\n", loc, rcOneLine(into.item, 70))
			continue
		}
		nb := &bullet{line: i, item: item}
		bullets = append(bullets, nb)
		if norm != "" && bySentence[norm] == nil {
			bySentence[norm] = nb
		}
	}
	if len(drop) == 0 {
		return draft, nil
	}
	for _, b := range bullets {
		if len(b.also) > 0 {
			lines[b.line] = "- " + rcWithAlso(b.item, b.also)
		}
	}
	kept := lines[:0:0]
	for i, line := range lines {
		if !drop[i] {
			kept = append(kept, line)
		}
	}
	return draft[:start] + strings.Join(kept, "\n"), notes
}
