package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/kaicontext/kai-engine/finding"
)

// This file is the deterministic half of review-commit: the parts of a
// finding that git and grep can establish without a model. It exists because
// of one review (kai-server, 2026-09-02, rc-01b3cf323d89eed7): the reviewed
// commit was "Merge origin/main into kai/s-94c6bf29", so "what you stated"
// was a merge line and the intent was judged against nothing; the branch
// read "—"; every claim said "no lookup" although each carried a path:line
// the model had actually read; a new config default pointed at a domain
// nothing served and nobody noticed; and a DECISION described a credit as a
// debit. Each of those has a mechanical check, and they live here.

// ---------------------------------------------------------------------------
// Intent for merge commits

// rcIsMergeCommit reports whether hash has more than one parent.
func rcIsMergeCommit(hash string) bool {
	out, err := exec.Command("git", "rev-list", "--parents", "-n", "1", hash).Output()
	if err != nil {
		return false
	}
	return len(strings.Fields(string(out))) > 2
}

// rcRangeCommits lists the non-merge commits in base..ref, oldest first, as
// (subject, body) pairs. Empty when base is unset or git fails.
func rcRangeCommits(base, ref string) (subjects, bodies []string) {
	if strings.TrimSpace(base) == "" {
		return nil, nil
	}
	const sep = "\x1e"
	out, err := exec.Command("git", "log", "--no-merges", "--reverse", "--format=%s%x1f%b"+sep, base+".."+ref).Output()
	if err != nil {
		return nil, nil
	}
	for _, rec := range strings.Split(string(out), sep) {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		s, b, _ := strings.Cut(rec, "\x1f")
		subjects = append(subjects, strings.TrimSpace(s))
		bodies = append(bodies, strings.TrimSpace(b))
	}
	return subjects, bodies
}

// rcStatedIntent is what the finding shows as "what you stated". A squash or
// ordinary commit states itself. A merge commit states nothing ("Merge
// origin/main into X" is not a goal), so for one the stated intent is the
// list of commits it brings in — the author's own words, in order.
func rcStatedIntent(subject string, isMerge bool, rangeSubjects []string) string {
	if !isMerge || len(rangeSubjects) == 0 {
		return subject
	}
	var b strings.Builder
	for _, s := range rangeSubjects {
		b.WriteString("- ")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// rcTitle is the inbox title. For a merge commit it is the newest commit the
// merge brings in, plus a count, so the inbox row names the work rather than
// the merge.
func rcTitle(subject string, isMerge bool, rangeSubjects []string) string {
	if !isMerge || len(rangeSubjects) == 0 {
		return subject
	}
	newest := rangeSubjects[len(rangeSubjects)-1]
	if n := len(rangeSubjects) - 1; n > 0 {
		return fmt.Sprintf("%s (+%d commits)", newest, n)
	}
	return newest
}

// rcAuthorContext is the AUTHOR CONTEXT block: the reviewed commit's own
// message, or for a merge every message in the range, so the reviewer reads
// what the author wrote and not "Merge origin/main".
func rcAuthorContext(subject, body string, isMerge bool, rangeSubjects, rangeBodies []string) string {
	if !isMerge || len(rangeSubjects) == 0 {
		if strings.TrimSpace(body) != "" {
			return subject + "\n\n" + body
		}
		return subject
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — a merge bringing in %d commit(s), listed oldest first:\n", subject, len(rangeSubjects))
	for i, s := range rangeSubjects {
		b.WriteString("\n## ")
		b.WriteString(s)
		b.WriteString("\n")
		if i < len(rangeBodies) && strings.TrimSpace(rangeBodies[i]) != "" {
			b.WriteString(rangeBodies[i])
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------------------
// Branch

// rcBranchName resolves the branch the finding belongs to: the --branch flag,
// then the CI environment (GITHUB_HEAD_REF is the PR's source branch,
// GITHUB_REF_NAME the pushed ref), then the checked-out branch. Empty in a
// detached checkout with no hints, which the UI renders as "—".
func rcBranchName(explicit string) string {
	if b := strings.TrimSpace(explicit); b != "" {
		return b
	}
	for _, env := range []string{"GITHUB_HEAD_REF", "GITHUB_REF_NAME", "CI_COMMIT_REF_NAME", "BRANCH_NAME"} {
		if b := strings.TrimSpace(os.Getenv(env)); b != "" {
			return b
		}
	}
	out, err := exec.Command("git", "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// Grounding ISSUES against the tree

// rcIssueLocation extracts the leading path:line from an ISSUES bullet. The
// coda contract is "path:line — sentence"; the model also writes ranges
// ("billing.go:960-987"), lists ("billing.go:978,983") and pairs ("a.go:12 &
// b.go:40"), so the first path and its first line number are taken.
var rcIssueLocationRe = regexp.MustCompile(`^\s*` + "`?" + `([A-Za-z0-9_./\-]+\.[A-Za-z0-9]+):(\d+)`)

func rcIssueLocation(item string) (path string, line int, ok bool) {
	m := rcIssueLocationRe.FindStringSubmatch(item)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return m[1], n, true
}

// rcFileLines reads path at hash from git (the tree the reviewer was given,
// not the working copy). ok is false when the path does not exist there.
func rcFileLines(hash, path string) (lines []string, ok bool) {
	out, err := exec.Command("git", "show", hash+":"+path).Output()
	if err != nil {
		return nil, false
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), true
}

// rcGroundIssue turns one ISSUES bullet into a Claim whose Lookup is the
// actual source line at the reviewed revision. A bullet whose path:line
// resolves is a grounded risk; one whose path or line does not exist at that
// revision, or that carries no location at all, is HELD — recorded, shown,
// but not counted as a grounded risk, because the one thing the reviewer was
// asked to pin down could not be found where it said.
func rcGroundIssue(hash, item string, readFile func(hash, path string) ([]string, bool)) finding.Claim {
	c := finding.Claim{Statement: item, Tag: finding.TagRisk}
	path, line, ok := rcIssueLocation(item)
	if !ok {
		c.Lookup = "no path:line given — held until the reviewer names where"
		return c
	}
	lines, exists := readFile(hash, path)
	if !exists {
		c.Lookup = fmt.Sprintf("%s does not exist at %s — held", path, rcShort(hash))
		return c
	}
	if line > len(lines) {
		c.Lookup = fmt.Sprintf("%s has %d lines at %s; line %d does not exist — held", path, len(lines), rcShort(hash), line)
		return c
	}
	src := strings.TrimSpace(lines[line-1])
	if len(src) > 120 {
		src = src[:120] + "…"
	}
	c.Lookup = fmt.Sprintf("%s:%d @ %s → %s", path, line, rcShort(hash), src)
	c.Resolved = true
	c.Verified = true
	return c
}

// rcDecisionClaim wraps a DECISIONS bullet. It is grounded by construction —
// the reviewer traced a value to a charge, a cap, a send, or a delete — and
// deliberately carries no path:line, so its lookup says what it is instead
// of "no lookup".
func rcDecisionClaim(decision string) finding.Claim {
	return finding.Claim{
		Statement: "Decision: " + decision,
		Lookup:    "policy, not a defect — needs the author's yes; who pays and who receives should be stated in words",
		Tag:       finding.TagRisk,
		Resolved:  true,
		Verified:  true,
	}
}

// ---------------------------------------------------------------------------
// New URL hosts that nothing else references

// rcKnownHostSuffixes are hosts a diff may legitimately introduce without the
// repository referencing them anywhere else: the platforms and standards a
// codebase talks to. Anything else that appears for the first time in this
// change is flagged so a default like "https://kai.dev/r/" (a domain the
// project did not own, 2026-09-02) cannot ship silently.
var rcKnownHostSuffixes = []string{
	"localhost", "127.0.0.1", "0.0.0.0", "example.com", "example.org", "example.net",
	"github.com", "githubusercontent.com", "github.io",
	"w3.org", "schema.org", "json-schema.org", "ietf.org", "rfc-editor.org",
	"golang.org", "go.dev", "pkg.go.dev", "npmjs.com", "npmjs.org", "nodejs.org", "pypi.org",
	"stripe.com", "googleapis.com", "google.com", "gstatic.com", "anthropic.com",
	"openrouter.ai", "openai.com", "slack.com", "cloudflare.com", "amazonaws.com",
	"microsoft.com", "apple.com", "mozilla.org", "wikipedia.org",
}

var rcURLHostRe = regexp.MustCompile(`https?://([A-Za-z0-9][A-Za-z0-9.\-]*\.[A-Za-z]{2,})`)

func rcHostIsKnown(host string) bool {
	host = strings.ToLower(host)
	for _, s := range rcKnownHostSuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// rcAddedURLHost is one URL host introduced by an added line of the diff.
type rcAddedURLHost struct {
	Host string
	Path string
	Line int // line number in the new file
}

// rcAddedURLHosts scans a unified diff for URL hosts on added lines, skipping
// comment lines and Markdown (a link to docs is not a default). One entry per
// host, at its first occurrence.
func rcAddedURLHosts(diff string) []rcAddedURLHost {
	var out []rcAddedURLHost
	seen := map[string]bool{}
	var path string
	newLine := 0
	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			f := strings.Fields(raw)
			path = ""
			if len(f) >= 4 {
				path = strings.TrimPrefix(f[len(f)-1], "b/")
			}
			continue
		case strings.HasPrefix(raw, "+++ "):
			continue
		case strings.HasPrefix(raw, "@@ "):
			// @@ -a,b +c,d @@ — c is the first new-side line of the hunk.
			if i := strings.Index(raw, "+"); i >= 0 {
				num := raw[i+1:]
				if j := strings.IndexAny(num, ", "); j >= 0 {
					num = num[:j]
				}
				newLine, _ = strconv.Atoi(num)
			}
			continue
		case strings.HasPrefix(raw, "-"):
			continue
		case strings.HasPrefix(raw, "+"):
			line := raw[1:]
			cur := newLine
			newLine++
			if path == "" || strings.HasSuffix(strings.ToLower(path), ".md") {
				continue
			}
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") ||
				strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "--") || strings.HasPrefix(t, "<!--") {
				continue
			}
			for _, m := range rcURLHostRe.FindAllStringSubmatch(line, -1) {
				host := strings.ToLower(m[1])
				if seen[host] || rcHostIsKnown(host) {
					continue
				}
				seen[host] = true
				out = append(out, rcAddedURLHost{Host: host, Path: path, Line: cur})
			}
		default:
			// context line
			newLine++
		}
	}
	return out
}

// rcFilesMentioningHost lists the files at hash that contain host, via git
// grep, so the check reads the reviewed tree and not the working copy.
func rcFilesMentioningHost(hash, host string) []string {
	out, err := exec.Command("git", "grep", "-l", "-i", "-F", "--", host, hash).Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// git grep prefixes each path with "<hash>:".
		if _, p, ok := strings.Cut(l, ":"); ok && p != "" {
			files = append(files, p)
		}
	}
	return files
}

// rcNewHostClaims flags every URL host the change introduces that no file
// outside the change mentions at the reviewed revision. It is a grounded risk
// with the grep as its lookup: the host may be fine, but nobody has said so.
func rcNewHostClaims(hash, diff string, changedPaths []string, filesMentioning func(hash, host string) []string) []finding.Claim {
	changed := map[string]bool{}
	for _, p := range changedPaths {
		changed[p] = true
	}
	var claims []finding.Claim
	for _, h := range rcAddedURLHosts(diff) {
		var elsewhere []string
		for _, f := range filesMentioning(hash, h.Host) {
			if !changed[f] {
				elsewhere = append(elsewhere, f)
			}
		}
		if len(elsewhere) > 0 {
			continue
		}
		claims = append(claims, finding.Claim{
			Statement: fmt.Sprintf("%s:%d — this change introduces the host %s, which no file outside the change mentions; confirm something actually serves it before a default that points there ships to users", h.Path, h.Line, h.Host),
			Lookup:    fmt.Sprintf("git grep -i -F %q %s → only the changed files", h.Host, rcShort(hash)),
			Tag:       finding.TagRisk,
			Resolved:  true,
			Verified:  true,
		})
	}
	return claims
}
