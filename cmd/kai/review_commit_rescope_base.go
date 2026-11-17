package main

// review_commit_rescope_base.go — what changed since the previous review,
// counting the pull request's own changes only.
//
// A re-review's focus is the files changed since the previous review's commit
// (review_commit_rescope.go). Compared commit to commit, that also counts
// everything the base branch changed when the author merged it in or rebased
// onto it: every file of the pull request that main touched in the meantime
// lands in the focus, and after a rebase onto a busy main that is often the
// whole pull request, so the re-review falls back to a full one.
//
// So when the base moved between the two commits, each commit is compared
// with its own merge base instead, and a file counts as changed only if the
// pull request's own change to it differs: its added and removed lines,
// ignoring where they sit. A file that changed only because the base moved
// under it is left out of the focus and named to the reviewer separately, so
// it can still look when a base change meets the pull request's code.
//
// When the base did not move, or the base is unknown (a single-commit review),
// or a merge base cannot be found, the commit-to-commit comparison stands.

import (
	"context"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rcSinceResult is one previous commit's comparison with HEAD, worked out
// once per review: the focus, the re-check test and the prompt all ask, and
// with a moved base each answer costs two merge bases and two full diffs.
type rcSinceResult struct {
	files    []string
	ok       bool
	baseOnly []string // changed only because the base moved
	prAt     []string // the pull request's files at that commit; nil if unknown
	prNow    []string // the pull request's files at HEAD; nil if unknown
}

var (
	rcSinceMu   sync.Mutex
	rcSinceMemo = map[string]rcSinceResult{}
)

// rcGitPRChangedSince is rcChangedSince when the review knows its base: the
// files whose pull-request change differs between head and HEAD.
func rcGitPRChangedSince(head string) ([]string, bool) {
	r := rcSince(head)
	return r.files, r.ok
}

func rcSince(head string) rcSinceResult {
	key := head + "|" + rcRevParse("HEAD") + "|" + reviewCommitBase
	rcSinceMu.Lock()
	r, done := rcSinceMemo[key]
	rcSinceMu.Unlock()
	if done {
		return r
	}
	r = rcComputeSince(head)
	// A commit the clone does not have is not remembered: it may be fetched
	// (rcFetchCommit), and asking again is cheap.
	if r.ok {
		rcSinceMu.Lock()
		rcSinceMemo[key] = r
		rcSinceMu.Unlock()
	}
	return r
}

func rcComputeSince(head string) rcSinceResult {
	names, ok := rcGitChangedSince(head)
	r := rcSinceResult{files: names, ok: ok}
	if !ok || strings.TrimSpace(reviewCommitBase) == "" {
		return r
	}
	oldBase := rcMergeBase(head, reviewCommitBase)
	newBase := rcMergeBase("HEAD", reviewCommitBase)
	if oldBase == "" || newBase == "" {
		return r
	}
	// The pull request's files then and now, spelled and listed the same way
	// (unquoted, a rename as both its paths), so they compare with the
	// changed-since list and with each other.
	r.prNow, _ = rcGitNames(newBase, "HEAD")
	if oldBase == newBase {
		r.prAt, _ = rcGitNames(oldBase, head)
		return r
	}
	oldP, ok1 := rcGitPatchByFile(oldBase, head)
	newP, ok2 := rcGitPatchByFile(newBase, "HEAD")
	if !ok1 || !ok2 {
		return r
	}
	r.files, r.baseOnly = rcSplitPRChanges(names, oldP, newP)
	r.prAt = make([]string, 0, len(oldP))
	for f := range oldP {
		r.prAt = append(r.prAt, f)
	}
	sort.Strings(r.prAt)
	return r
}

// rcBaseOnlySince is what rcGitPRChangedSince left out for head.
func rcBaseOnlySince(head string) []string {
	return rcSince(head).baseOnly
}

// rcDroppedSince lists the files the pull request changed at head and no
// longer changes: a change taken back out. Nil when the base is unknown.
// changed is the pull request's file list as the review has it; the list
// worked out here (rcSinceResult.prNow) is preferred, since it spells and
// lists files the way prAt does.
func rcDroppedSince(head string, changed []string) []string {
	r := rcSince(head)
	if len(r.prAt) == 0 {
		return nil
	}
	now := map[string]bool{}
	if r.prNow != nil {
		for _, f := range r.prNow {
			now[f] = true
		}
	} else {
		for _, f := range changed {
			now[rcGitUnquote(f)] = true
		}
	}
	var out []string
	for _, f := range r.prAt {
		if !now[f] {
			out = append(out, f)
		}
	}
	return out
}

// rcPRNowFn is the pull request's files at HEAD as rcSince worked them out;
// tests replace it.
var rcPRNowFn = func(head string) []string { return rcSince(head).prNow }

// rcPRFiles maps each of the pull request's files, unquoted, to the
// spelling the review uses for it (changed's, from --numstat, which quotes
// non-ASCII names and gives a rename its old path only). Files the review's
// list misses that way (a rename's new path) come from rcPRNowFn, spelled
// as git checks them out.
func rcPRFiles(head string, changed []string) map[string]string {
	spell := make(map[string]string, len(changed))
	for _, f := range changed {
		spell[rcGitUnquote(f)] = f
	}
	for _, f := range rcPRNowFn(head) {
		if _, ok := spell[f]; !ok {
			spell[f] = f
		}
	}
	return spell
}

// rcDroppedSinceFn is rcDroppedSince; tests replace it.
var rcDroppedSinceFn = rcDroppedSince

func rcRevParse(rev string) string {
	out, err := exec.Command("git", "rev-parse", "--verify", "--quiet", rev).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// rcGitNames is the files changed from..to, unquoted.
func rcGitNames(from, to string) ([]string, bool) {
	out, err := exec.Command("git", "-c", "core.quotePath=false", "--no-pager", "diff", "--name-only", "--no-renames", from, to).Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, true
}

// rcSplitPRChanges splits the files that differ between two commits into
// those whose pull-request change differs (own) and those that differ only
// because the base moved (baseOnly). oldP and newP are each commit's
// pull-request patch per file, normalised (rcPatchByFile).
func rcSplitPRChanges(names []string, oldP, newP map[string]string) (own, baseOnly []string) {
	own = []string{}
	seen := map[string]bool{}
	for _, f := range names {
		seen[f] = true
		if oldP[f] != newP[f] {
			own = append(own, f)
		} else if _, inPR := newP[f]; inPR {
			baseOnly = append(baseOnly, f)
		}
		// A file in neither patch changed only in the base and is not part
		// of the pull request: rcReReviewFocus drops it either way.
	}
	// A pull-request change that the commit-to-commit list missed (the base
	// moved the same lines back): still the pull request's own.
	for f := range newP {
		if !seen[f] && oldP[f] != newP[f] {
			own = append(own, f)
		}
	}
	for f := range oldP {
		if _, ok := newP[f]; !ok && !seen[f] {
			own = append(own, f) // the pull request no longer changes it
		}
	}
	sort.Strings(own)
	sort.Strings(baseOnly)
	return own, baseOnly
}

func rcMergeBase(a, b string) string {
	out, err := exec.Command("git", "merge-base", a, b).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// rcGitPatchByFile is the zero-context diff from..to, normalised per file.
func rcGitPatchByFile(from, to string) (map[string]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.quotePath=false", "--no-pager", "diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", from, to)
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := cmd.Start(); err != nil {
		return nil, false
	}
	limit := rcPatchLimit
	out, _ := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(out)) > limit {
		// Too big to compare. Stop git now: it is blocked writing the rest,
		// and waiting would hold the review until the deadline.
		cancel()
		_ = cmd.Wait()
		return nil, false // a truncated patch would compare wrong
	}
	if err := cmd.Wait(); err != nil {
		return nil, false
	}
	return rcPatchByFile(string(out)), true
}

// rcPatchLimit is the largest pull-request patch compared; tests lower it.
var rcPatchLimit int64 = 32 << 20

// rcPatchByFile splits a diff per file and keeps what the change IS: its
// added and removed lines and whether the file is new or deleted. Hunk
// positions and blob ids are dropped, so a change the base moved down the
// file compares equal.
func rcPatchByFile(diff string) map[string]string {
	out := map[string]string{}
	var b strings.Builder
	file, oldFile := "", ""
	// inBody: past the file's first "@@". From there every line is part of
	// the change, including "--- x" (a removed "-- x") and "+++ x" (an added
	// "++ x"), which before it would be the file's own header lines. A line
	// at column 0 starting "diff --git " is always a new file: body lines
	// start with "+", "-", " " or "\".
	inBody := false
	flush := func() {
		if file != "" {
			out[file] = b.String()
		}
		b.Reset()
		file, oldFile, inBody = "", "", false
	}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			// A fallback only: the "---"/"+++" headers below give the real
			// path. This line does not carry it reliably — a quoted path has
			// no " b/", and a path whose directory ends " b" a misleading one.
			if i := strings.LastIndex(line, " b/"); i >= 0 {
				file = line[i+3:]
			}
			continue
		}
		if !inBody {
			switch {
			case strings.HasPrefix(line, "--- "):
				oldFile = rcDiffHeaderPath(line, "--- ", "a/")
			case strings.HasPrefix(line, "+++ "):
				if p := rcDiffHeaderPath(line, "+++ ", "b/"); p == "/dev/null" {
					file = oldFile // a deleted file: key by its old path
				} else {
					file = p
				}
			case strings.HasPrefix(line, "@@"):
				inBody = true
			// The whole change of a chmod, and the mode a file is added or
			// deleted with: no hunk carries them. They hold no position.
			case strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"),
				strings.HasPrefix(line, "old mode "), strings.HasPrefix(line, "new mode "):
				b.WriteString(line)
				b.WriteString("\n")
			case strings.HasPrefix(line, "Binary files "):
				b.WriteString(line)
				b.WriteString("\n")
			}
			continue // index and header lines hold no change
		}
		if len(line) > 0 && (line[0] == '+' || line[0] == '-') {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	return out
}

// rcDiffHeaderPath is the file path from a "--- "/"+++ " diff header, with the
// prefix and the "a/"/"b/" stripped and git's quoting (a tab or quote in the
// name) and trailing tab (a spaced name) undone. "/dev/null" is left as is.
func rcDiffHeaderPath(line, hdr, side string) string {
	p := rcGitUnquote(strings.TrimSuffix(strings.TrimPrefix(line, hdr), "\t"))
	return strings.TrimPrefix(p, side)
}

// rcGitUnquote undoes git's quoting of a path ("caf\303\251.go"), which
// --numstat and diff headers apply to non-ASCII names unless core.quotePath
// is off. A path git did not quote is returned as it is.
func rcGitUnquote(p string) string {
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		if u, err := strconv.Unquote(p); err == nil {
			return u
		}
	}
	return p
}

// rcPRFileCount is how many files the pull request has, as rcReReviewFocus
// counts them (rcPRFiles).
func rcPRFileCount(prior *rcPrior, changed []string) int {
	if prior == nil {
		return len(changed)
	}
	return len(rcPRFiles(prior.Head, changed))
}
