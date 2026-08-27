package main

// kai ship — the SHIP phase's exit ramp: turn the session's verified
// working-tree changes into a git branch, a commit with session
// trailers, and a GitHub pull request. This is the local (phase-1)
// path: it generalizes autofix's proven branch → stage-exact-paths →
// commit → push → PR pipeline for session work, and runs only in a
// repo with a real git remote. Spawned workspaces (orphan git repos)
// ship via the kailab publisher instead.
//
// The branch name is the session identity — kai/<workspace> — so
// concurrent sessions can never contend for a ref, and re-shipping
// from the same session lands on the same branch (GitHub then emits
// `synchronize` and the server-side review supersede machinery takes
// it from there).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/kaicontext/kai-engine/gitio"
	"github.com/kaicontext/kai-engine/kaipath"
	spawnpkg "github.com/kaicontext/kai-engine/spawn"
	"kai/internal/autofix"
)

var (
	shipSession string
	shipSlug    string
	shipBranch  string
	shipBase    string
	shipRemote  string
	shipRepo    string
	shipToken   string
	shipTitle   string
	shipReady   bool
	shipPush    bool
	shipPR      bool
	shipDryRun  bool
)

var shipCmd = &cobra.Command{
	Use:   "ship",
	Short: "Ship the session's changes: branch, commit with session trailers, push, open a PR",
	Long: `Turn the working tree's changes into a reviewable unit: a branch named
after the session (kai/<workspace>), a commit carrying Kai-Session /
Kai-Snapshot trailers, a push, and a GitHub pull request (draft by
default).

Only the changed paths are staged — never ` + "`git add -A`" + ` — so kai's own
artifacts stay out of the commit. Re-running on the ship branch commits
and pushes again; the existing PR updates via GitHub's synchronize.

Identity resolution: --branch wins; else --session names the branch
kai/s-<first 8>; else the current kai workspace name is used.

Credentials: --token or $GITHUB_TOKEN; --repo or $GITHUB_REPOSITORY
(else derived from the git remote).`,
	RunE: runShip,
}

func init() {
	shipCmd.Flags().StringVar(&shipSession, "session", "", "session UUID; names the branch kai/s-<first 8> and is recorded as a commit trailer")
	shipCmd.Flags().StringVar(&shipSlug, "slug", "", "optional goal slug appended to the branch name (stable across re-ships of one session)")
	shipCmd.Flags().StringVar(&shipBranch, "branch", "", "explicit branch name (overrides --session / workspace identity)")
	shipCmd.Flags().StringVar(&shipBase, "base", "", "base branch for the PR (default: the branch you ship from)")
	shipCmd.Flags().StringVar(&shipRemote, "remote", "origin", "git remote to push to")
	shipCmd.Flags().StringVar(&shipRepo, "repo", "", "owner/name (default $GITHUB_REPOSITORY or git remote)")
	shipCmd.Flags().StringVar(&shipToken, "token", "", "GitHub token (default $GITHUB_TOKEN)")
	shipCmd.Flags().StringVar(&shipTitle, "title", "", "commit subject and PR title (default: ship: <branch>)")
	shipCmd.Flags().BoolVar(&shipReady, "ready", false, "open the PR ready-for-review (default: draft)")
	shipCmd.Flags().BoolVar(&shipPush, "push", true, "push the branch (set false to stop after the local commit)")
	shipCmd.Flags().BoolVar(&shipPR, "pr", true, "open a pull request after pushing")
	shipCmd.Flags().BoolVar(&shipDryRun, "dry-run", false, "print the plan without changing anything")
	rootCmd.AddCommand(shipCmd)
}

func runShip(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	branch, err := resolveShipBranch(cwd)
	if err != nil {
		return err
	}
	sessionID := resolveShipSession(cwd)

	current, err := gitio.CurrentBranch(cwd)
	if err != nil {
		return fmt.Errorf("not a git repo (kai ship --local needs one; spawned workspaces ship via the server): %w", err)
	}
	reShip := current == branch
	if !reShip && gitio.BranchExists(cwd, branch) {
		return fmt.Errorf("branch %s already exists but %s is checked out — check out the ship branch to re-ship, or pass --branch for a fresh one", branch, current)
	}
	base := shipBase
	if base == "" && !reShip {
		base = current
	}
	if base == branch {
		return fmt.Errorf("base and ship branch are both %s — pass --base", base)
	}

	// Stage exactly what changed; kai's own artifacts never ride along.
	dirty, err := gitio.DirtyPaths(cwd)
	if err != nil {
		return err
	}
	changed := autofix.FilterArtifacts(dirty)
	if len(changed) == 0 {
		return fmt.Errorf("nothing to ship — the working tree has no changes")
	}

	// Fail fast on missing credentials before any tree mutation.
	var gh *autofix.Client
	if shipPush && shipPR {
		gh, err = resolveShipClient(cwd)
		if err != nil {
			return fmt.Errorf("%w (or pass --pr=false to ship without opening a PR)", err)
		}
	}

	snapHex := shipSnapshotHex(cwd)

	if shipDryRun {
		fmt.Printf("would ship %d file(s) on %s (base %s, re-ship: %v)\n", len(changed), branch, orDash(base), reShip)
		for _, p := range changed {
			fmt.Printf("  %s\n", p)
		}
		if sessionID != "" {
			fmt.Printf("Kai-Session: %s\n", sessionID)
		}
		if snapHex != "" {
			fmt.Printf("Kai-Snapshot: %s\n", snapHex)
		}
		return nil
	}

	unlock, err := acquireShipLock(cwd)
	if err != nil {
		return err
	}
	defer unlock()

	if !reShip {
		if err := gitio.CreateBranch(cwd, branch); err != nil {
			return fmt.Errorf("creating %s: %w", branch, err)
		}
	}
	// On any failure after the branch exists, put the tree back where
	// the user was; a stranded half-shipped branch blocks the retry.
	shipped := false
	defer func() {
		if shipped || reShip {
			return
		}
		_ = gitio.CheckoutBranch(cwd, current)
		_ = gitio.DeleteBranch(cwd, branch)
	}()

	diffBase := base
	if diffBase == "" {
		diffBase = "HEAD"
	}
	if _, err := gitio.StageAndDiffPaths(cwd, diffBase, changed); err != nil {
		return fmt.Errorf("staging changes: %w", err)
	}
	if err := gitio.CommitStaged(cwd, shipCommitMessage(branch, sessionID, snapHex)); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	fmt.Printf("committed %d file(s) on %s\n", len(changed), branch)

	if !shipPush {
		shipped = true
		fmt.Printf("shipped locally — push with: git push %s %s\n", shipRemote, branch)
		return nil
	}
	if err := gitio.Push(cwd, shipRemote, branch); err != nil {
		// The commit is real and correct; don't unwind it over a
		// network failure. Report the exact state instead.
		shipped = true
		return fmt.Errorf("committed on %s but push to %s failed: %w", branch, shipRemote, err)
	}
	fmt.Printf("pushed %s to %s\n", branch, shipRemote)

	if !shipPR {
		shipped = true
		return nil
	}
	if pr, err := gh.FindOpenPRForHead(branch); err == nil && pr != nil {
		shipped = true
		fmt.Printf("PR already open, updated by the push: %s\n", pr.HTML)
		return nil
	}
	prBase := base
	if prBase == "" {
		prBase = "main"
	}
	title := shipTitle
	if title == "" {
		title = "ship: " + branch
	}
	pr, err := gh.CreatePR(autofix.CreatePRInput{
		Title: title,
		Head:  branch,
		Base:  prBase,
		Body:  shipPRBody(branch, sessionID, snapHex, changed),
		Draft: !shipReady,
	})
	if err != nil {
		shipped = true
		return fmt.Errorf("pushed %s but opening the PR failed: %w", branch, err)
	}
	shipped = true
	state := "draft"
	if shipReady {
		state = "ready"
	}
	fmt.Printf("opened %s PR: %s\n", state, pr.HTML)
	return nil
}

// resolveShipBranch derives the branch name from, in order: --branch,
// --session (kai/s-<sid8>), the current kai workspace name.
func resolveShipBranch(cwd string) (string, error) {
	if shipBranch != "" {
		return shipBranch, nil
	}
	identity := ""
	if shipSession != "" {
		base, err := spawnpkg.WorkspaceBase(shipSession, "")
		if err != nil {
			return "", err
		}
		identity = base
	} else if ws, err := getCurrentWorkspace(); err == nil && ws != "" {
		identity = ws
	}
	if identity == "" {
		return "", fmt.Errorf("no session identity: pass --session, --branch, or check out a kai workspace")
	}
	if shipSlug != "" {
		slug, err := spawnpkg.SanitizeName(shipSlug)
		if err != nil {
			return "", fmt.Errorf("--slug: %w", err)
		}
		identity += "-" + slug
	}
	return "kai/" + identity, nil
}

// resolveShipSession returns the full session UUID for the commit
// trailer: --session, else the spawn registry entry for this dir.
func resolveShipSession(cwd string) string {
	if shipSession != "" {
		return shipSession
	}
	reg, err := spawnpkg.Load()
	if err != nil {
		return ""
	}
	resolved, _ := filepath.EvalSymlinks(cwd)
	for _, e := range reg.Spawned {
		p, _ := filepath.EvalSymlinks(e.Path)
		if e.Path == cwd || (resolved != "" && p == resolved) {
			return e.SessionID
		}
	}
	return ""
}

// shipSnapshotHex best-effort resolves the latest kai snapshot for the
// Kai-Snapshot trailer. "" when the repo isn't captured.
func shipSnapshotHex(cwd string) string {
	hex, err := resolveSourceSnapshot(cwd, "@snap:last")
	if err != nil {
		return ""
	}
	return hex
}

func shipCommitMessage(branch, sessionID, snapHex string) string {
	subject := shipTitle
	if subject == "" {
		subject = "ship: " + branch
	}
	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n\nShipped with `kai ship`.\n")
	if sessionID != "" || snapHex != "" {
		b.WriteString("\n")
		if sessionID != "" {
			fmt.Fprintf(&b, "Kai-Session: %s\n", sessionID)
		}
		if snapHex != "" {
			fmt.Fprintf(&b, "Kai-Snapshot: %s\n", snapHex)
		}
	}
	return b.String()
}

func shipPRBody(branch, sessionID, snapHex string, files []string) string {
	var b strings.Builder
	b.WriteString("Shipped from a kai session.\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n| Branch | `%s` |\n", branch)
	if sessionID != "" {
		fmt.Fprintf(&b, "| Session | `%s` |\n", sessionID)
	}
	if snapHex != "" {
		fmt.Fprintf(&b, "| Snapshot | `%s` |\n", snapHex)
	}
	fmt.Fprintf(&b, "| Files | %d |\n", len(files))
	b.WriteString("\n<details><summary>Changed files</summary>\n\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s`\n", f)
	}
	b.WriteString("\n</details>\n\n<!-- kai-ship -->\n")
	return b.String()
}

// acquireShipLock serializes ships per repo — the local path mutates
// the one working tree, so two concurrent ships would fight over HEAD.
// No kai dir (plain git repo) means no lock file home; proceed unlocked.
func acquireShipLock(cwd string) (func(), error) {
	kd := kaipath.Resolve(cwd)
	if _, err := os.Stat(kd); err != nil {
		return func() {}, nil
	}
	f, err := os.OpenFile(filepath.Join(kd, "ship.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, nil
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another kai ship is in progress in this repo")
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// resolveShipClient mirrors autofix's client resolution with ship's
// own flags: --repo / $GITHUB_REPOSITORY / the git remote's slug.
func resolveShipClient(cwd string) (*autofix.Client, error) {
	repo := shipRepo
	if repo == "" && os.Getenv("GITHUB_REPOSITORY") == "" {
		if url, err := gitio.RemoteURL(cwd, shipRemote); err == nil {
			repo = autofix.RepoSlugFromRemote(url)
		}
	}
	return autofix.NewClient(shipToken, repo)
}
