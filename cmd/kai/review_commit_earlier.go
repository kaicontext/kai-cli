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

// rcEarlierTagRe is the tag; the ref is kai-server's 12-hex fingerprint.
var rcEarlierTagRe = regexp.MustCompile(`\s*\(earlier:\s*([0-9a-f]{12})\)`)

// rcTakeEarlierTags strips every tag from a draft and returns the refs of the
// bullets that carried one: an ISSUES bullet keyed by its path:line, a
// DECISIONS bullet (which has no location) by its wording.
func rcTakeEarlierTags(raw string) (string, map[string]string) {
	if !strings.Contains(raw, "(earlier:") {
		return raw, nil
	}
	refs := map[string]string{}
	lines := strings.Split(raw, "\n")
	section := ""
	for i, l := range lines {
		section = rcEarlierSection(section, l)
		m := rcEarlierTagRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		l = rcEarlierTagRe.ReplaceAllString(l, "")
		lines[i] = l
		if key, ok := rcEarlierBulletKey(section, l); ok {
			refs[key] = m[1]
		}
	}
	return strings.Join(lines, "\n"), refs
}

// rcPutEarlierTags appends each remembered ref to the ISSUES or DECISIONS
// bullet it belongs to in the final review.
func rcPutEarlierTags(raw string, refs map[string]string) string {
	if len(refs) == 0 {
		return raw
	}
	lines := strings.Split(raw, "\n")
	section := ""
	for i, l := range lines {
		section = rcEarlierSection(section, l)
		if section == "" || strings.Contains(l, "(earlier:") {
			continue
		}
		if key, ok := rcEarlierBulletKey(section, l); ok {
			if ref := refs[key]; ref != "" {
				lines[i] = strings.TrimRight(l, " ") + " (earlier: " + ref + ")"
			}
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

// rcEarlierBulletKey is the key a tagged bullet is remembered by.
func rcEarlierBulletKey(section, line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "- ") {
		return "", false
	}
	item := strings.TrimPrefix(t, "- ")
	switch section {
	case "issues":
		return rcEarlierKey(item)
	case "decisions":
		if k := rcIssueKey(item); k != "" {
			return "decision:" + k, true
		}
	}
	return "", false
}

// rcEarlierKey is a bullet's path:line.
func rcEarlierKey(item string) (string, bool) {
	path, line, ok := rcIssueLocation(item)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s:%d", path, line), true
}
