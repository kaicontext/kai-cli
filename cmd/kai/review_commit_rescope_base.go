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
	"strings"
	"sync"
	"time"
)

var (
	rcBaseOnlyMu sync.Mutex
	// rcBaseOnly remembers, per previous commit, the files that changed
	// only because the base moved, for the prompt (rcBaseMovedBlock).
	rcBaseOnly = map[string][]string{}
)

// rcGitPRChangedSince is rcChangedSince when the review knows its base: the
// files whose pull-request change differs between head and HEAD.
func rcGitPRChangedSince(head string) ([]string, bool) {
	names, ok := rcGitChangedSince(head)
	if !ok || len(names) == 0 || strings.TrimSpace(reviewCommitBase) == "" {
		return names, ok
	}
	oldBase := rcMergeBase(head, reviewCommitBase)
	newBase := rcMergeBase("HEAD", reviewCommitBase)
	if oldBase == "" || newBase == "" || oldBase == newBase {
		return names, ok
	}
	oldP, ok1 := rcGitPatchByFile(oldBase, head)
	newP, ok2 := rcGitPatchByFile(newBase, "HEAD")
	if !ok1 || !ok2 {
		return names, ok
	}
	own, baseOnly := rcSplitPRChanges(names, oldP, newP)
	rcBaseOnlyMu.Lock()
	rcBaseOnly[head] = baseOnly
	rcBaseOnlyMu.Unlock()
	return own, true
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

// rcBaseOnlySince is what rcGitPRChangedSince left out for head.
func rcBaseOnlySince(head string) []string {
	rcBaseOnlyMu.Lock()
	defer rcBaseOnlyMu.Unlock()
	return rcBaseOnly[head]
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
	cmd := exec.CommandContext(ctx, "git", "--no-pager", "diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", from, to)
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := cmd.Start(); err != nil {
		return nil, false
	}
	const limit = 32 << 20
	out, _ := io.ReadAll(io.LimitReader(stdout, limit+1))
	if err := cmd.Wait(); err != nil || len(out) > limit {
		return nil, false // a truncated patch would compare wrong
	}
	return rcPatchByFile(string(out)), true
}

// rcPatchByFile splits a diff per file and keeps what the change IS: its
// added and removed lines and whether the file is new or deleted. Hunk
// positions and blob ids are dropped, so a change the base moved down the
// file compares equal.
func rcPatchByFile(diff string) map[string]string {
	out := map[string]string{}
	var b strings.Builder
	file := ""
	flush := func() {
		if file != "" {
			out[file] = b.String()
		}
		b.Reset()
		file = ""
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			// "diff --git a/x b/x" (no renames, so both sides name it)
			if i := strings.LastIndex(line, " b/"); i >= 0 {
				file = line[i+3:]
			}
		case file == "":
		case strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"):
			b.WriteString(strings.Fields(line)[0])
			b.WriteString("\n")
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
		case strings.HasPrefix(line, "Binary files "):
			b.WriteString(line)
			b.WriteString("\n")
		case len(line) > 0 && (line[0] == '+' || line[0] == '-'):
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	return out
}
