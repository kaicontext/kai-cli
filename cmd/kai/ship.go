package main

// kai ship — the SHIP phase's exit ramp: turn the session's verified
// working-tree changes into a git branch, a commit with session
// trailers, and a GitHub pull request. This is the local (phase-1)
// path: it generalizes autofix's proven branch → stage-exact-paths →
// commit → push → PR pipeline for session work, and runs only in a
// repo with a real git remote. Spawned workspaces (orphan git repos)
// ship via the kailab publisher instead — and they do so BY DEFAULT:
// a registered spawn takes the --server path unless --local is passed
// (see shipUseServer). Before that default, an agent that committed
// its work in a spawn and then ran a bare `kai ship` was told "nothing
// to ship" — the local path only sees dirty files — and gave up
// (2026-09-10, session de960690).
//
// The branch is named for what the change IS — kai/<slug>-<id>, the slug
// from the PR title (or the session's first commit subject) and the id
// the first six characters of the session identity — so a reviewer
// scanning GitHub's branch list reads "kai/fix-login-redirect-98d608"
// instead of "kai/s-98d60850", while two sessions with the same title
// still never contend for a ref. A session with nothing to name it by
// keeps the bare identity, kai/<workspace>. Re-shipping lands on the same
// branch: the server redirects a session with an open PR to that PR's
// branch, and the local path re-ships onto a checked-out branch of the
// same session (GitHub then emits `synchronize` and the server-side
// review supersede machinery takes it from there).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kaicontext/kai-engine/gitio"
	"github.com/kaicontext/kai-engine/graph"
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
	shipServer  bool
	shipLocal   bool
	shipClean   bool
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

Branch name: --branch wins. Otherwise the branch is named for the change,
kai/<slug>-<id>: the slug from --slug, else --title, else the session's
first commit subject; the id is the first six characters of the session
identity (--session, else the current kai workspace), so two sessions with
the same title never share a branch. With nothing to name it by, the branch
is the bare identity: kai/s-<first 8> or kai/<workspace>.

Credentials: --token or $GITHUB_TOKEN; --repo or $GITHUB_REPOSITORY
(else derived from the git remote).

Inside a spawned workspace (a session tree registered in ~/.kai/spawned.json)
the ship goes through the kailab server by default — the spawn has no git
remote, and its delta is measured against the session baseline, so work the
agent already committed still ships. Pass --local to force the local git
path anyway.`,
	RunE: runShip,
}

func init() {
	shipCmd.Flags().StringVar(&shipSession, "session", "", "session UUID; its first characters make the branch unique, and it is recorded as a commit trailer")
	shipCmd.Flags().StringVar(&shipSlug, "slug", "", "the readable part of the branch name (default: derived from --title or the session's first commit)")
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
	shipCmd.Flags().BoolVar(&shipClean, "clean", false, "after a successful --server ship, stash the shipped changes (labeled; `git stash pop` restores) so the working tree returns to pristine main")
	shipCmd.Flags().BoolVar(&shipServer, "server", false, "publish via the kailab server (GitHub App) instead of local git — the default inside a spawned workspace; --repo means kai org/repo in this mode")
	shipCmd.Flags().BoolVar(&shipLocal, "local", false, "force the local git path (branch, commit, push with your own token) even inside a spawned workspace")
	rootCmd.AddCommand(shipCmd)
}

func runShip(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	useServer, err := shipUseServer(cwd, shipServer, shipLocal)
	if err != nil {
		return err
	}
	branch, err := resolveShipBranch(cwd)
	if err != nil {
		return err
	}
	sessionID := resolveShipSession(cwd)

	if useServer {
		if !shipServer {
			fmt.Fprintln(os.Stderr, "spawned workspace: shipping via the kailab server (pass --local to force local git)")
			shipServer = true
		}
		return runShipServer(cwd, branch, sessionID)
	}

	current, err := gitio.CurrentBranch(cwd)
	if err != nil {
		return fmt.Errorf("not a git repo (kai ship --local needs one; spawned workspaces ship via the server): %w", err)
	}
	// Already on a ship branch of this session: re-ship onto it, even when
	// this ship's title would name a different one — a second title must
	// not fork the session's work onto a second branch and PR.
	if shipBranch == "" && current != branch {
		if identity, _ := shipIdentityFor(cwd); shipBranchIsSessions(cwd, current, identity, sessionID) {
			branch = current
		}
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
		if shipSpawnEntry(cwd) != nil {
			return fmt.Errorf("nothing dirty to ship locally — this is a spawned workspace, whose delta (committed work included) ships via the server; drop --local")
		}
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

// resolveShipBranch derives the branch name: --branch as given, else
// kai/<slug>-<id> named for the change (see shipBranchName), else the bare
// identity kai/<workspace> when there is nothing to name it by.
func resolveShipBranch(cwd string) (string, error) {
	if shipBranch != "" {
		return shipBranch, nil
	}
	identity, err := shipIdentityFor(cwd)
	if err != nil {
		return "", err
	}
	if identity == "" {
		return "", fmt.Errorf("no session identity: pass --session, --branch, or check out a kai workspace")
	}
	slug := ""
	if shipSlug != "" {
		if slug = shipSlugify(shipSlug); slug == "" {
			return "", fmt.Errorf("--slug: no usable characters in %q", shipSlug)
		}
	} else if slug = shipSlugify(shipTitle); slug == "" {
		slug = shipSlugify(shipFirstCommitSubject(cwd))
	}
	return shipBranchName(identity, slug), nil
}

// shipIdentityFor is the session identity a branch is made unique by:
// --session's workspace base (s-<first 8>), else the current kai
// workspace name. "" when there is neither; an error when --session was
// given but cannot name one, so a malformed id is not reported as a
// missing one.
func shipIdentityFor(cwd string) (string, error) {
	if shipSession != "" {
		base, err := spawnpkg.WorkspaceBase(shipSession, "")
		if err != nil {
			return "", fmt.Errorf("--session: %w", err)
		}
		return base, nil
	}
	if ws, err := getCurrentWorkspace(); err == nil && ws != "" {
		return ws, nil
	}
	return "", nil
}

// shipBranchSlugMax bounds the readable part of a branch name. Long enough
// for a real title ("fix-voice-chat-parent-title-resolution"), short
// enough that GitHub's branch picker shows the whole thing.
const shipBranchSlugMax = 48

// shipBranchName joins the two halves: the slug says what the change is,
// the id keeps two sessions with the same title off the same ref. With no
// slug the identity alone names the branch, as it always has.
func shipBranchName(identity, slug string) string {
	if slug == "" {
		return "kai/" + identity
	}
	return "kai/" + slug + "-" + shipShortID(identity)
}

// shipShortID is the identity's distinguishing part. A session identity
// ("s-98d60850") contributes the first six hex characters of its UUID —
// random, so six beside a slug that already differs between most sessions
// keep refs apart. A workspace name is not random ("my-workspace" and
// "my-workflow" share any prefix you cut), so it is kept whole.
func shipShortID(identity string) string {
	if hex := strings.TrimPrefix(identity, "s-"); hex != identity && shipIsHex(hex) {
		if len(hex) > 6 {
			hex = hex[:6]
		}
		return hex
	}
	if id := shipSlugify(identity); id != "" {
		return id
	}
	return "kai"
}

func shipIsHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// shipBranchIsSessions reports whether branch — the one checked out — is a
// ship branch of THIS session, so a re-ship lands on it instead of forking
// a second branch because this ship's title names a different one. The
// bare kai/<identity> always is. A named kai/<slug>-<id> must end in this
// identity's id and, when the session is known, its tip must carry this
// session's Kai-Session trailer: six hex characters alone could, however
// rarely, be another session's, and re-shipping onto its PR would be far
// worse than opening a second one.
func shipBranchIsSessions(cwd, branch, identity, sessionID string) bool {
	if identity == "" || !strings.HasPrefix(branch, "kai/") {
		return false
	}
	if branch == "kai/"+identity {
		return true
	}
	if !strings.HasSuffix(branch, "-"+shipShortID(identity)) {
		return false
	}
	if sessionID == "" {
		return true
	}
	msg, err := gitOut(cwd, "log", "-1", "--format=%B", branch)
	return err == nil && strings.Contains(msg, "Kai-Session: "+sessionID)
}

// shipSlugify turns a title into the readable half of a branch name:
// lowercase words joined by dashes, cut on a word boundary at
// shipBranchSlugMax. A title that is itself a transport placeholder
// ("ship: kai/s-98d60850") names nothing and yields "".
func shipSlugify(title string) string {
	title = strings.TrimSpace(title)
	if strings.HasPrefix(strings.ToLower(title), "ship: kai/") {
		return ""
	}
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			cur.WriteRune(r)
		case r == '\'':
			// "don't" → "dont", not "don-t"
		default:
			flush()
		}
	}
	flush()
	out := ""
	for _, w := range words {
		next := w
		if out != "" {
			next = out + "-" + w
		}
		if len(next) > shipBranchSlugMax {
			if out == "" {
				out = w[:shipBranchSlugMax]
			}
			break
		}
		out = next
	}
	return out
}

// shipFirstCommitSubject is the subject of the session's first own commit
// in a spawned workspace — the part's one-line intent, and fixed from the
// moment it exists, so a branch named from it does not drift between
// re-ships. kai's own bookkeeping commits (the spawn baseline, warm syncs,
// merges, earlier ship fallbacks) are not the session's intent and are
// skipped. "" outside a spawn, or before the session has committed.
func shipFirstCommitSubject(cwd string) string {
	if shipSpawnEntry(cwd) == nil {
		return ""
	}
	base := shipBaselineCommit(cwd)
	if base == "" || base == "HEAD" {
		return ""
	}
	out, err := gitOut(cwd, "log", "--reverse", "--format=%s", base+"..HEAD")
	if err != nil {
		return ""
	}
	for _, subject := range strings.Split(out, "\n") {
		subject = strings.TrimSpace(subject)
		low := strings.ToLower(subject)
		if subject == "" || strings.HasPrefix(low, "kai spawn from") || strings.HasPrefix(low, "kai warm sync") ||
			strings.HasPrefix(low, "merge ") || strings.HasPrefix(low, "ship: kai/") {
			continue
		}
		return subject
	}
	return ""
}

// resolveShipSession returns the full session UUID for the commit
// trailer: --session, else the spawn registry entry for this dir.
func resolveShipSession(cwd string) string {
	if shipSession != "" {
		return shipSession
	}
	if e := shipSpawnEntry(cwd); e != nil {
		return e.SessionID
	}
	return ""
}

// shipSpawnEntry returns the spawn registry entry whose path is cwd, or
// nil when cwd is not a registered spawn. It is THE matcher for every
// ship decision that hinges on "is this tree a spawn" — the session
// trailer, the server base, the mode default — so they cannot disagree
// about which trees are spawns. Symlinks are resolved on both sides:
// /tmp is a symlink on macOS, and a spawn registered under /private/tmp
// must still match a cwd spelled /tmp.
func shipSpawnEntry(cwd string) *spawnpkg.Entry {
	reg, err := spawnpkg.Load()
	if err != nil {
		return nil
	}
	resolved, _ := filepath.EvalSymlinks(cwd)
	for i := range reg.Spawned {
		e := &reg.Spawned[i]
		p, _ := filepath.EvalSymlinks(e.Path)
		if e.Path == cwd || (resolved != "" && p == resolved) {
			return e
		}
	}
	return nil
}

// shipUseServer decides which publish path a ship takes. --server and
// --local are explicit and win; with neither, a registered spawn ships
// via the server and a plain checkout ships via local git.
//
// The default matters more than a flag usually does: a spawn is an
// orphan repo with no remote, and the local path stages only dirty
// files, so a spawn whose agent committed its work reads as "nothing to
// ship" locally while the server path — baseline-vs-tree — sees all of
// it. Making the agent remember --server was the failure mode; deciding
// it from the tree removes the thing to remember.
func shipUseServer(cwd string, server, local bool) (bool, error) {
	if server && local {
		return false, fmt.Errorf("--server and --local are mutually exclusive")
	}
	if server {
		return true, nil
	}
	if local {
		return false, nil
	}
	return shipSpawnEntry(cwd) != nil, nil
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
	b.WriteString("\n</details>\n")
	if known := ledgerKnownIssues(); known != "" {
		b.WriteString(known)
	}
	b.WriteString("\n<!-- kai-ship -->\n")
	return b.String()
}

// ledgerKnownIssues renders the session's unresolved review-ledger
// claims (open + accepted) for the PR body — the same claims the
// turn-0 injector shows the agent, now shown to the human reviewer so
// the PR review starts where the gate audit left off instead of
// re-diagnosing. Best-effort and capped: no ledger, no section.
func ledgerKnownIssues() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	kd := kaipath.Resolve(cwd)
	if _, err := os.Stat(filepath.Join(kd, dbFile)); err != nil {
		return ""
	}
	db, err := graph.Open(filepath.Join(kd, dbFile), filepath.Join(kd, objectsDir))
	if err != nil {
		return ""
	}
	defer db.Close()
	findings, err := db.ListFindingsByState(graph.FindingOpen, graph.FindingAccepted)
	if err != nil || len(findings) == 0 {
		return ""
	}
	const knownCap = 10
	var b strings.Builder
	b.WriteString("\n### Known issues (kai review ledger)\n\n")
	b.WriteString("Unresolved claims from kai's gate audit, riding along so review starts from the diagnosis instead of re-deriving it.\n\n")
	for i, f := range findings {
		if i == knownCap {
			fmt.Fprintf(&b, "- …%d more — `kai findings` lists them all\n", len(findings)-knownCap)
			break
		}
		what := f.Symptom
		if what == "" {
			what = f.Description
		}
		fmt.Fprintf(&b, "- **[%s]** %s", f.State, what)
		if len(f.Files) > 0 {
			fmt.Fprintf(&b, " (`%s`)", strings.Join(f.Files, "`, `"))
		}
		if f.Prescription != "" {
			fmt.Fprintf(&b, " — fix: %s", f.Prescription)
		}
		b.WriteString("\n")
	}
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
	if held, err := tryLockFile(f); err != nil || !held {
		f.Close()
		return nil, fmt.Errorf("another kai ship is in progress in this repo")
	}
	return func() {
		_ = unlockFile(f)
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

// ---------------------------------------------------------------------------
// kai ship rebase — the conflicted-PR recovery flow

var (
	shipRebaseRemote string
	shipRebaseBase   string
)

var shipRebaseCmd = &cobra.Command{
	Use:   "rebase",
	Short: "Rebase the ship branch onto the moved base and update the PR",
	Long: `When GitHub reports the ship PR conflicts with a moved base, this
rebases the checked-out kai/ branch onto the remote base and pushes
with --force-with-lease (safe against a concurrent push from another
machine); the PR updates via synchronize.

On conflict the rebase is aborted so the tree stays clean, and the
conflicted files are named — resolve them with a normal
` + "`git rebase`" + ` and re-run ` + "`kai ship`" + `.`,
	RunE: runShipRebase,
}

func init() {
	shipRebaseCmd.Flags().StringVar(&shipRebaseRemote, "remote", "origin", "git remote holding the base")
	shipRebaseCmd.Flags().StringVar(&shipRebaseBase, "base", "main", "base branch the PR targets")
	shipCmd.AddCommand(shipRebaseCmd)
}

func runShipRebase(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	current, err := gitio.CurrentBranch(cwd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(current, "kai/") {
		return fmt.Errorf("%s is not a ship branch — check out the kai/ branch to rebase it", current)
	}
	if dirty, err := gitio.WorkingTreeDirty(cwd); err != nil {
		return err
	} else if dirty {
		return fmt.Errorf("the working tree has uncommitted changes — ship or stash them before rebasing")
	}
	if err := gitio.Fetch(cwd, shipRebaseRemote, shipRebaseBase); err != nil {
		return fmt.Errorf("fetching %s/%s: %w", shipRebaseRemote, shipRebaseBase, err)
	}
	upstream := shipRebaseRemote + "/" + shipRebaseBase
	conflicted, err := gitio.RebaseOnto(cwd, upstream)
	if err != nil {
		if len(conflicted) > 0 {
			fmt.Printf("rebase onto %s stopped on conflicts (tree restored, nothing lost):\n", upstream)
			for _, f := range conflicted {
				fmt.Printf("  %s\n", f)
			}
			fmt.Printf("resolve manually: git rebase %s, fix the files, git rebase --continue, then kai ship\n", upstream)
		}
		return err
	}
	if err := gitio.PushForceWithLease(cwd, shipRebaseRemote, current); err != nil {
		return fmt.Errorf("rebased locally, but the push was refused (%w) — another machine may have pushed this branch; pull it first", err)
	}
	fmt.Printf("rebased %s onto %s and pushed — the PR updates via synchronize\n", current, upstream)
	return nil
}
