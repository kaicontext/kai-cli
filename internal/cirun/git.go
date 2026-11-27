package cirun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// childWaitDelay bounds the wait for a killed child's pipes: a grandchild can
// hold them open after the child itself is gone.
const childWaitDelay = 5 * time.Second

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	c.WaitDelay = childWaitDelay
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// GitInfo is what the synthetic event and the report need from the repo.
type GitInfo struct {
	Root          string
	Branch        string // "" when detached
	HeadSHA       string
	BaseRef       string // e.g. "origin/main"; "" when none was found
	BaseSHA       string // merge-base of BaseRef and HEAD
	DefaultBranch string // the repo's default branch
	BaseBranch    string // the branch a pull request would target
	Repo          string // "owner/name", or the directory name when there is no remote
	UserName      string
	UserEmail     string
	Dirty         bool
}

// ReadGit collects GitInfo for the repo containing dir. base overrides the
// default base ref (origin/HEAD, then main, then master).
func ReadGit(ctx context.Context, dir, base string) (*GitInfo, error) {
	root, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	gi := &GitInfo{Root: root}
	if gi.HeadSHA, err = git(ctx, root, "rev-parse", "HEAD"); err != nil {
		return nil, fmt.Errorf("the repository has no commits yet: %w", err)
	}
	gi.Branch, _ = git(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")

	// The repo's default branch: origin/HEAD when there is one, else the
	// first of the usual names that exists. It does not depend on --base.
	exists := func(ref string) bool {
		_, err := git(ctx, root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		return err == nil
	}
	defaultRef := ""
	if ref, err := git(ctx, root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" && exists(ref) {
		defaultRef = ref
	} else {
		for _, ref := range []string{"origin/main", "main", "origin/master", "master"} {
			if exists(ref) {
				defaultRef = ref
				break
			}
		}
	}
	gi.DefaultBranch = strings.TrimPrefix(defaultRef, "origin/")
	if gi.DefaultBranch == "" {
		gi.DefaultBranch = "main"
	}

	// The diff base: --base, or the default branch.
	baseRef := base
	if baseRef == "" {
		baseRef = defaultRef
	}
	if baseRef != "" && exists(baseRef) {
		if mb, err := git(ctx, root, "merge-base", baseRef, "HEAD"); err == nil {
			gi.BaseRef, gi.BaseSHA = baseRef, mb
		}
	}
	if base != "" && gi.BaseRef == "" {
		return nil, fmt.Errorf("--base %s: not a commit, or it shares no history with HEAD", base)
	}
	// A pull request targets the default branch unless --base names another
	// branch. A --base that is a commit or an expression (HEAD~3) moves the
	// diff base only.
	gi.BaseBranch = gi.DefaultBranch
	if base != "" {
		// git knows every spelling of a branch: release, origin/release,
		// refs/heads/release, heads/release. It prints nothing for a commit.
		if full, err := git(ctx, root, "rev-parse", "--symbolic-full-name", base); err == nil {
			for _, prefix := range []string{"refs/heads/", "refs/remotes/origin/"} {
				if name, ok := strings.CutPrefix(full, prefix); ok && name != "" && name != "HEAD" {
					gi.BaseBranch = name
					break
				}
			}
		}
	}

	gi.Repo = filepath.Base(root)
	if url, err := git(ctx, root, "remote", "get-url", "origin"); err == nil {
		if r := repoFromURL(url); r != "" {
			gi.Repo = r
		}
	}
	gi.UserName, _ = git(ctx, root, "config", "user.name")
	gi.UserEmail, _ = git(ctx, root, "config", "user.email")
	if st, err := git(ctx, root, "status", "--porcelain"); err == nil && st != "" {
		gi.Dirty = true
	}
	return gi, nil
}

// repoFromURL pulls "owner/name" out of an https or ssh remote URL.
func repoFromURL(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), ".git")
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
		if j := strings.IndexByte(url, '/'); j >= 0 {
			url = url[j+1:]
		}
	} else if i := strings.IndexByte(url, ':'); i >= 0 { // git@host:owner/name
		url = url[i+1:]
	}
	parts := strings.Split(strings.Trim(url, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// ChangedFiles lists the files that differ from the merge-base. With working
// set it adds uncommitted and untracked files. ok is false when there is no
// base to diff against.
func ChangedFiles(ctx context.Context, gi *GitInfo, working bool) (files []string, ok bool) {
	if gi.BaseSHA == "" {
		return nil, false
	}
	seen := map[string]bool{}
	add := func(out string) {
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				seen[l] = true
			}
		}
	}
	out, err := git(ctx, gi.Root, "diff", "--name-only", gi.BaseSHA, "HEAD")
	if err != nil {
		return nil, false
	}
	add(out)
	if working {
		// A truncated list would let a paths: filter skip a workflow the
		// uncommitted files do touch, so a failure here means "unknown".
		for _, args := range [][]string{
			{"diff", "--name-only", "HEAD"},
			{"ls-files", "--others", "--exclude-standard"},
		} {
			out, err := git(ctx, gi.Root, args...)
			if err != nil {
				return nil, false
			}
			add(out)
		}
	}
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, true
}

// BuildEvent writes the synthetic webhook payload act is given with -e.
func BuildEvent(event string, gi *GitInfo) ([]byte, error) {
	owner, name, _ := strings.Cut(gi.Repo, "/")
	if name == "" {
		owner, name = gi.UserName, gi.Repo
	}
	repo := map[string]any{
		"name":           name,
		"full_name":      gi.Repo,
		"default_branch": gi.DefaultBranch,
		"owner":          map[string]any{"login": owner, "name": owner},
	}
	user := map[string]any{"login": gi.UserName, "name": gi.UserName, "email": gi.UserEmail}
	branch := gi.Branch
	if branch == "" {
		branch = gi.HeadSHA
	}
	before := gi.BaseSHA
	if before == "" {
		before = strings.Repeat("0", 40)
	}
	var payload map[string]any
	switch event {
	case "push":
		payload = map[string]any{
			"ref":        "refs/heads/" + branch,
			"before":     before,
			"after":      gi.HeadSHA,
			"repository": repo,
			"pusher":     map[string]any{"name": gi.UserName, "email": gi.UserEmail},
			"sender":     user,
			"head_commit": map[string]any{
				"id": gi.HeadSHA,
			},
		}
	case "pull_request":
		payload = map[string]any{
			"action": "synchronize",
			"number": 0,
			"pull_request": map[string]any{
				"number": 0,
				"user":   user,
				"base":   map[string]any{"ref": gi.BaseBranch, "sha": before, "repo": repo},
				"head":   map[string]any{"ref": branch, "sha": gi.HeadSHA, "repo": repo},
			},
			"repository": repo,
			"sender":     user,
		}
	default:
		return nil, fmt.Errorf("unsupported event %q (use push or pull_request)", event)
	}
	return json.MarshalIndent(payload, "", "  ")
}

// headCheckout puts a clean copy of HEAD in a fresh temp directory and
// returns it with a cleanup function. It is a clone that borrows the repo's
// objects, not a linked worktree: act can't read git metadata through a
// worktree's .git file.
func headCheckout(ctx context.Context, gi *GitInfo) (string, func(), error) {
	parent, err := os.MkdirTemp("", "kai-ci-run-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(parent) }
	dir := filepath.Join(parent, filepath.Base(gi.Root))
	if _, err := git(ctx, gi.Root, "clone", "--quiet", "--shared", "--no-checkout", gi.Root, dir); err != nil {
		cleanup()
		return "", nil, err
	}
	branch := gi.Branch
	if branch == "" {
		branch = "kai-ci-run"
	}
	if _, err := git(ctx, dir, "checkout", "--quiet", "-B", branch, gi.HeadSHA); err != nil {
		cleanup()
		return "", nil, err
	}
	// The clone's origin is the local path; actions read the repo's name
	// from the real one.
	if url, err := git(ctx, gi.Root, "remote", "get-url", "origin"); err == nil && url != "" {
		_, _ = git(ctx, dir, "remote", "set-url", "origin", url)
	}
	return dir, cleanup, nil
}
