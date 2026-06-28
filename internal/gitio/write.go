package gitio

// Write operations on a working tree. These shell out to the `git`
// binary rather than go-git: the existing read path already does so for
// speed (see DiffFilesNative), and branch/commit/push want the user's
// real git config, credential helpers, and hooks — exactly the parts a
// pure-Go reimplementation would have to mimic. The headless auto-fix
// flow (issue → branch → fix → PR) is the only caller today.

import (
	"fmt"
	"os/exec"
	"strings"
)

// git runs a git subcommand in dir and returns trimmed stdout. On a
// non-zero exit it returns an error carrying stderr so callers can
// surface the real cause (e.g. "nothing to commit", auth failure).
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// CurrentBranch returns the checked-out branch name in dir, or an error
// in detached-HEAD state (where there is no branch to report).
func CurrentBranch(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "HEAD" {
		return "", fmt.Errorf("detached HEAD: no current branch in %s", dir)
	}
	return out, nil
}

// BranchExists reports whether a local branch of the given name exists.
func BranchExists(dir, branch string) bool {
	_, err := runGit(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// CreateBranch creates and checks out a new branch from the current
// HEAD. It fails if the branch already exists — the caller is expected
// to have run BranchExists first for idempotency.
func CreateBranch(dir, branch string) error {
	_, err := runGit(dir, "checkout", "-b", branch)
	return err
}

// CheckoutBranch switches to an existing branch.
func CheckoutBranch(dir, branch string) error {
	_, err := runGit(dir, "checkout", branch)
	return err
}

// WorkingTreeDirty reports whether the tree at dir has uncommitted
// changes (staged, unstaged, or untracked).
func WorkingTreeDirty(dir string) (bool, error) {
	out, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// DirtyPaths returns the repo-relative paths of every uncommitted change
// (staged, unstaged, or untracked) in dir. Lets a caller decide dirtiness
// per-path — e.g. tolerate files a tool wrote for its own operation while
// still blocking on real user changes. For a rename it reports the new path.
func DirtyPaths(dir string) ([]string, error) {
	out, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 { // "XY path" — 2 status chars, a space, then the path
			continue
		}
		p := strings.TrimSpace(line[3:])
		if i := strings.Index(p, " -> "); i >= 0 { // rename: take the destination
			p = p[i+len(" -> "):]
		}
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// DeleteBranch force-deletes a local branch (git branch -D). Used to
// un-strand the issue branch after a failed run so a retry isn't blocked by
// the "branch already exists" idempotency guard.
func DeleteBranch(dir, branch string) error {
	_, err := runGit(dir, "branch", "-D", branch)
	return err
}

// DiscardChanges hard-resets tracked files in dir to HEAD, dropping any
// uncommitted edits. Untracked files are left alone. Used in failure cleanup
// so the subsequent CheckoutBranch back to base can't be blocked by the
// agent's half-applied, uncommitted work.
func DiscardChanges(dir string) error {
	_, err := runGit(dir, "reset", "--hard")
	return err
}

// CommitAll stages every change (tracked, untracked, and deletions) and
// commits with the given message. Returns ErrNothingToCommit when the
// tree is clean so the caller can distinguish "agent made no changes"
// from a real failure.
func CommitAll(dir, message string) error {
	dirty, err := WorkingTreeDirty(dir)
	if err != nil {
		return err
	}
	if !dirty {
		return ErrNothingToCommit
	}
	if _, err := runGit(dir, "add", "-A"); err != nil {
		return err
	}
	if _, err := runGit(dir, "commit", "-m", message); err != nil {
		return err
	}
	return nil
}

// ErrNothingToCommit is returned by CommitAll when the working tree is
// clean — the agent ran but produced no file changes.
var ErrNothingToCommit = fmt.Errorf("nothing to commit: working tree clean")

// Push pushes branch to remote, setting upstream so subsequent pushes
// (e.g. a re-run that adds commits) need no arguments.
func Push(dir, remote, branch string) error {
	_, err := runGit(dir, "push", "-u", remote, branch)
	return err
}

// DiffAgainst returns the unified diff of the working tree (including
// staged and unstaged changes) against base — a ref such as the base
// branch or merge-base. Used both as the proof artifact in the PR body
// and as the input the semantic judge reads. An empty string means no
// changes.
func DiffAgainst(dir, base string) (string, error) {
	// Stage first so the diff captures new files too; `git diff base`
	// alone omits untracked files. Staging is idempotent and the
	// commit step re-stages anyway.
	if _, err := runGit(dir, "add", "-A"); err != nil {
		return "", err
	}
	out, err := runGit(dir, "diff", "--staged", base)
	if err != nil {
		return "", err
	}
	return out, nil
}

// StageAndDiffPaths stages exactly the given paths (additions, modifications,
// and deletions among them) and returns their unified diff against base.
// Unlike DiffAgainst it never runs `git add -A`, so files outside paths —
// e.g. the .codex/.claude/.kai tooling kai writes for its own operation
// during a run — stay out of both the diff and any subsequent CommitStaged.
// An empty paths slice yields an empty diff and stages nothing.
func StageAndDiffPaths(dir, base string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	if _, err := runGit(dir, append([]string{"add", "--"}, paths...)...); err != nil {
		return "", err
	}
	return runGit(dir, append([]string{"diff", "--staged", base, "--"}, paths...)...)
}

// CommitStaged commits whatever is currently staged with the given message,
// returning ErrNothingToCommit when the index is empty. Pairs with
// StageAndDiffPaths: stage exactly the fix's files, then commit only those —
// no `git add -A` that would absorb unrelated tree contents.
func CommitStaged(dir, message string) error {
	staged, err := runGit(dir, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if strings.TrimSpace(staged) == "" {
		return ErrNothingToCommit
	}
	if _, err := runGit(dir, "commit", "-m", message); err != nil {
		return err
	}
	return nil
}

// RemoteURL returns the fetch URL of the named remote, used to derive
// the owner/repo slug when GITHUB_REPOSITORY isn't set in the env.
func RemoteURL(dir, remote string) (string, error) {
	return runGit(dir, "remote", "get-url", remote)
}
