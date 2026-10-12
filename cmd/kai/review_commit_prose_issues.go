package main

// review_commit_prose_issues.go — findings the reviewer wrote in the prose.
//
// The reviewer is asked to list its findings under ISSUES: in the
// ===REVIEW-DATA=== coda. Sometimes it writes them as bullets in the prose and
// ends the coda after MERGE_READY with no ISSUES section at all. Nothing then
// reads them: they never reach the challenge, never become claims, and the
// review publishes as "nothing jumped out" with the prose below listing the
// points. It happened on kai-server#442 and on 2 of ~15 drafts in the
// 2026-10-12 repeat-run study.
//
// When — and only when — the coda has no ISSUES (or FINDINGS) header, a prose
// bullet that leads with a location ("- path:line — …", "- nit: path:line — …")
// is taken as the finding it is written as, and listed under a new ISSUES
// header so the challenge checks it like any other. A coda that says
// "ISSUES: (none)" said so on purpose and is left alone.

import "strings"

// rcRecoverProseIssues adds an ISSUES section built from the prose's located
// bullets to a draft whose coda has none.
func rcRecoverProseIssues(raw string) (string, int) {
	i := strings.Index(raw, rcReviewDataMarker)
	if i < 0 {
		return raw, 0
	}
	prose, coda := raw[:i], raw[i+len(rcReviewDataMarker):]
	for _, ln := range strings.Split(coda, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(rcUnwrapMachineBullet(t), "-") {
			continue
		}
		if key, _, labelled := rcMachineLine(t); labelled && (key == "issues" || key == "findings") {
			return raw, 0
		}
	}
	var found []string
	seen := map[string]bool{}
	for _, ln := range strings.Split(prose, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "- ") && !strings.HasPrefix(t, "* ") {
			continue
		}
		item := strings.TrimSpace(t[2:])
		if _, _, ok := rcIssueLocation(item); !ok || seen[item] {
			continue
		}
		seen[item] = true
		found = append(found, item)
	}
	if len(found) == 0 {
		return raw, 0
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(raw, "\n"))
	b.WriteString("\nISSUES:\n")
	for _, f := range found {
		b.WriteString("- " + f + "\n")
	}
	return b.String(), len(found)
}
