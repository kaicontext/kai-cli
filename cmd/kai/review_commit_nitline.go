package main

// review_commit_nitline.go — a point about a comment is a nit.
//
// The prompts ask the reviewer and the sweep to mark "nit: " on a defect that
// cannot change what the program does, and the model does not reliably do
// it. On kai-server#434, #437 and #440 — three runs, same fixture — "the doc
// comment names a `max` argument but the parameter is `limit`" came out as a
// 🐞 "fix before merge" every time, and held the score at 3/5.
//
// Where a point is can settle what the model would not: a bullet whose
// path:line (and every "also:" location) is a comment line is about the
// comment, and a comment does not run. So it is marked a nit before the
// challenge counts it. Directives that do change the build or the tools —
// //go:build, // +build, //go:generate, //nolint, // eslint-…, # type:,
// # noqa, #! — are not comments for this purpose.

import (
	"regexp"
	"strconv"
	"strings"
)

// rcCommentLineRe matches a line that is only a comment.
var rcCommentLineRe = regexp.MustCompile(`^\s*(//|/\*|\*|#|--|<!--|""")`)

// rcDirectiveRe matches a comment that tools read.
var rcDirectiveRe = regexp.MustCompile(`^\s*(//\s*(go:|\+build|nolint|eslint|@ts-|prettier-|istanbul )|#\s*(type:|noqa|pragma|pylint:|!|include|define|if|endif|ifdef|ifndef|else|elif|undef)|/\*\s*(eslint|istanbul|webpackChunkName))`)

// rcAlsoLocRe reads the locations in a bullet's "(also: …)" list.
var rcAlsoLocRe = regexp.MustCompile(`([A-Za-z0-9_./-]+):(\d+)`)

// rcIsCommentLine reports whether a source line is a plain comment.
func rcIsCommentLine(src string) bool {
	return rcCommentLineRe.MatchString(src) && !rcDirectiveRe.MatchString(src)
}

// rcMarkCommentNits prefixes "nit: " to each ISSUES bullet whose every cited
// line is a comment line in the reviewed tree.
func rcMarkCommentNits(raw string, readFile func(path string) ([]string, bool)) string {
	i := strings.Index(raw, rcReviewDataMarker)
	if i < 0 {
		return raw
	}
	head, coda := raw[:i+len(rcReviewDataMarker)], raw[i+len(rcReviewDataMarker):]
	lines := strings.Split(coda, "\n")
	inIssues := false
	for j, ln := range lines {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "- ") {
			if key, _, labelled := rcMachineLine(t); labelled {
				inIssues = key == "issues" || key == "findings"
			}
			continue
		}
		if !inIssues {
			continue
		}
		item := strings.TrimPrefix(t, "- ")
		if _, nit := rcSplitNit(item); nit {
			continue
		}
		if rcAllCommentLines(item, readFile) {
			lines[j] = strings.Replace(ln, "- "+item, "- nit: "+item, 1)
		}
	}
	return head + strings.Join(lines, "\n")
}

// rcAllCommentLines reports whether the bullet's location and each "also:"
// location are comment lines.
func rcAllCommentLines(item string, readFile func(path string) ([]string, bool)) bool {
	path, line, ok := rcIssueLocation(item)
	if !ok {
		return false
	}
	locs := [][2]string{{path, strconv.Itoa(line)}}
	if k := strings.Index(item, "(also:"); k >= 0 {
		for _, m := range rcAlsoLocRe.FindAllStringSubmatch(item[k:], -1) {
			locs = append(locs, [2]string{m[1], m[2]})
		}
	}
	for _, l := range locs {
		src, ok := readFile(l[0])
		n, _ := strconv.Atoi(l[1])
		if !ok || n < 1 || n > len(src) || !rcIsCommentLine(src[n-1]) {
			return false
		}
	}
	return true
}
