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

// rcEarlierTagRe is a bookkeeping tag kai-server reads off a statement: the
// earlier-ref (its 12-hex fingerprint) or the reviewer's severity. Both ride
// the same way — off while bullets are matched by wording, back on at the end.
var rcEarlierTagRe = regexp.MustCompile(`(?i)\s*\((?:earlier:\s*[0-9a-f]{12}|severity:\s*(?:high|medium|low))\)`)

// rcTakeEarlierTags strips every tag from a draft and returns the tags of the
// bullets that carried them ("(severity: low) (earlier: …)"): an ISSUES bullet
// keyed by its path:line, a DECISIONS bullet (which has no location) by its
// wording.
func rcTakeEarlierTags(raw string) (string, map[string]string) {
	if !rcEarlierTagRe.MatchString(raw) {
		return raw, nil
	}
	refs := map[string]string{}
	lines := strings.Split(raw, "\n")
	section := ""
	for i, l := range lines {
		section = rcEarlierSection(section, l)
		tags := rcEarlierTagRe.FindAllString(l, -1)
		if len(tags) == 0 {
			continue
		}
		l = rcEarlierTagRe.ReplaceAllString(l, "")
		lines[i] = l
		if key, ok := rcEarlierBulletKey(section, l); ok {
			for i := range tags {
				tags[i] = strings.ToLower(strings.TrimSpace(tags[i]))
			}
			refs[key] = strings.Join(tags, " ")
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
		if section == "" || rcEarlierTagRe.MatchString(l) {
			continue
		}
		if key, ok := rcEarlierBulletKey(section, l); ok {
			if tags := refs[key]; tags != "" {
				lines[i] = strings.TrimRight(l, " ") + " " + tags
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
