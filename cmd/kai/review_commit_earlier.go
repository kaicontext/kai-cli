package main

// review_commit_earlier.go — saying which earlier point a finding is.
//
// kai-server cannot recognise a re-raised point by its wording: the reviewer
// rewords it every time. On kai-server#437 "`Excerpt` panics on a negative
// `limit`" at line 9 came back after a push as "When `limit <= 0` …
// `r[:limit]` panics" at line 13 and was posted as a new 🐞 comment. The
// reviewer knows better — it is re-checking its previous review — so each
// earlier defect arrives with a ref (KAI_PRIOR_REVIEW, rcPriorBlock), and the
// reviewer ends an ISSUES bullet that is one of them with "(earlier: <ref>)".
// Decisions too (kai-cli#171 re-announced its two decisions on every push).
//
// The reviewer's "(severity: high|medium|low)" on a defect is the same kind of
// tag and rides the same way (kai-server reads both; review_earlier.go).
//
// The tag is bookkeeping, not text: the sweep merge, the duplicate fold and
// the challenge all match bullets by their wording, and the challenge's
// "## Findings" headings are prose. So it comes off right after the reviewer
// writes, remembered by the bullet's path:line, and goes back on the final
// ISSUES bullets at that location — the statement kai-server reads it from
// (api/review_earlier.go), which strips it before anything is shown.

import (
	"fmt"
	"regexp"
	"strings"
)

// rcEarlierTagRe is a bookkeeping tag kai-server reads off a statement: the
// earlier-ref (its 12-hex fingerprint) or the reviewer's severity. Both ride
// the same way — off while bullets are matched by wording, back on at the end.
var rcEarlierTagRe = regexp.MustCompile(`(?i)\s*\((?:earlier:\s*[0-9a-f]{12}|severity:\s*(?:high|medium|low))\)`)

// rcTakeEarlierTags strips every tag from a draft and returns the tags of the
// bullets that carried them ("(severity: low) (earlier: …)"), keyed by
// rcEarlierBulletKey: an ISSUES bullet by its location and wording, with its
// location alone as a fallback; a DECISIONS bullet (which has no location) by
// its wording.
func rcTakeEarlierTags(raw string) (string, map[string]string) {
	if !rcEarlierTagRe.MatchString(raw) {
		return raw, nil
	}
	refs := map[string]string{}
	lines := strings.Split(raw, "\n")
	section := ""
	for i, l := range lines {
		section = rcEarlierSection(section, l)
		stripped, tags := rcSplitTags(l)
		if tags == "" {
			continue
		}
		lines[i] = stripped
		rcRememberTags(refs, section, stripped, tags)
	}
	return strings.Join(lines, "\n"), refs
}

// rcSplitTags strips a line's tags and returns them, normalised and in order.
func rcSplitTags(line string) (string, string) {
	tags := rcEarlierTagRe.FindAllString(line, -1)
	if len(tags) == 0 {
		return line, ""
	}
	for i := range tags {
		tags[i] = strings.ToLower(strings.TrimSpace(tags[i]))
	}
	return rcEarlierTagRe.ReplaceAllString(line, ""), strings.Join(tags, " ")
}

// rcAmbiguous marks a location two differently tagged bullets share: there,
// only an exact wording match may take a tag.
const rcAmbiguous = "\x00"

// rcRememberTags files one bullet's tags under its keys.
func rcRememberTags(refs map[string]string, section, line, tags string) {
	key, loc, ok := rcEarlierBulletKey(section, line)
	if !ok {
		return
	}
	if _, seen := refs[key]; !seen {
		refs[key] = tags // the first teller wins: the reviewer before the sweep
	}
	if loc != "" {
		if prev, seen := refs[loc]; seen && prev != tags {
			refs[loc] = rcAmbiguous
		} else {
			refs[loc] = tags
		}
	}
}

// rcPutEarlierTags appends each remembered tag to the ISSUES or DECISIONS
// bullet it belongs to in the final review — matched by location and
// wording, or by location alone when only one tagged bullet was there — and
// only the tags the bullet does not already carry.
func rcPutEarlierTags(raw string, refs map[string]string) string {
	if len(refs) == 0 {
		return raw
	}
	lines := strings.Split(raw, "\n")
	section := ""
	for i, l := range lines {
		section = rcEarlierSection(section, l)
		if section == "" {
			continue
		}
		key, loc, ok := rcEarlierBulletKey(section, l)
		if !ok {
			continue
		}
		tags := refs[key]
		if tags == "" && loc != "" && refs[loc] != rcAmbiguous {
			tags = refs[loc]
		}
		if tags == "" {
			continue
		}
		have := strings.ToLower(l)
		var add []string
		for _, t := range rcEarlierTagRe.FindAllString(tags, -1) {
			if t = strings.TrimSpace(t); !strings.Contains(have, strings.ToLower(t)) {
				add = append(add, t)
			}
		}
		if len(add) > 0 {
			lines[i] = strings.TrimRight(l, " ") + " " + strings.Join(add, " ")
		}
	}
	return strings.Join(lines, "\n")
}

// rcEarlierSection tracks which coda list a line is in: "issues",
// "decisions", or "" outside both.
func rcEarlierSection(section, line string) string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "- ") {
		return section
	}
	if key, _, labelled := rcMachineLine(t); labelled {
		switch key {
		case "issues", "findings":
			return "issues"
		case "decisions":
			return "decisions"
		}
	}
	return ""
}

// rcAlsoListRe is a bullet's "(also: …)" list, which the duplicate fold
// adds after the reviewer wrote it.
var rcAlsoListRe = regexp.MustCompile(`\s*\(also:[^)]*\)`)

// rcEarlierBulletKey is the key a tagged bullet is remembered by, and for an
// ISSUES bullet also its location key ("loc:path:line"): two different
// defects on one line must not share a severity.
func rcEarlierBulletKey(section, line string) (key, loc string, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "- ") {
		return "", "", false
	}
	item := strings.TrimPrefix(t, "- ")
	switch section {
	case "issues":
		where, ok := rcEarlierKey(item)
		if !ok {
			return "", "", false
		}
		sentence := rcIssueKey(rcIssueSentence(rcAlsoListRe.ReplaceAllString(item, "")))
		return where + "|" + sentence, "loc:" + where, true
	case "decisions":
		if k := rcIssueKey(item); k != "" {
			return "decision:" + k, "", true
		}
	}
	return "", "", false
}

// rcEarlierKey is a bullet's path:line.
func rcEarlierKey(item string) (string, bool) {
	path, line, ok := rcIssueLocation(item)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s:%d", path, line), true
}
