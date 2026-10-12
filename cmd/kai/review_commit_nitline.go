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
// comment, and a comment does not run — unless the bullet is plainly about
// the code beside it (rcAboutTheCode). So it is marked a nit before the
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
		if rcAllCommentLines(item, readFile) && !rcAboutTheCode(item, readFile) {
			lines[j] = strings.Replace(ln, "- "+item, "- nit: "+item, 1)
		}
	}
	return head + strings.Join(lines, "\n")
}

// rcCodeExprRe finds `code spans` that hold an expression rather than a bare
// name: something with an operator, a call, an index or a slice.
var rcCodeExprRe = regexp.MustCompile("`([^`]*[\\[\\]().<>=!+*/%&|-][^`]*)`")

// rcBehaviourRe names a runtime effect. A point that claims one is about what
// the code does, whichever line it cites.
var rcBehaviourRe = regexp.MustCompile(`(?i)\b(panic|panics|crash|crashes|out of range|out of bounds|nil pointer|null pointer|invalid utf-?8|mid-rune|data race|deadlock|leak|leaks|overflow|off-by-one|race condition)\b`)

// rcAboutTheCode reports whether a bullet that cites a comment is really
// about the code beside it: it quotes an expression that appears on a
// non-comment line of the cited file, or it names a runtime effect. "The
// docstring says characters, but the implementation slices by bytes
// (`text[:limit]`)" cites the doc line and is a defect; "the docstring
// names `max` but the parameter is `limit`" names identifiers only and is a
// nit. (The 2026-10-12 repeat-run study caught the first downgraded.)
func rcAboutTheCode(item string, readFile func(path string) ([]string, bool)) bool {
	if rcBehaviourRe.MatchString(item) {
		return true
	}
	path, _, ok := rcIssueLocation(item)
	if !ok {
		return false
	}
	src, ok := readFile(path)
	if !ok {
		return false
	}
	for _, m := range rcCodeExprRe.FindAllStringSubmatch(item, -1) {
		expr := strings.TrimSpace(m[1])
		if len(expr) < 3 {
			continue
		}
		for _, l := range src {
			if !rcIsCommentLine(l) && strings.Contains(l, expr) {
				return true
			}
		}
	}
	return false
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
		also := item[k:]
		if e := strings.Index(also, ")"); e >= 0 {
			also = also[:e] // the list, not the prose after it
		}
		for _, m := range rcAlsoLocRe.FindAllStringSubmatch(also, -1) {
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
