package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/kaipath"
	"kai/internal/ref"
	"kai/internal/remote"
	"kai/internal/spawnclone"
	"github.com/kaicontext/kai-engine/util"
	"kai/internal/workspace"
	spawnpkg "kai/pkg/spawn"
)

// ---------------------------------------------------------------------------
// Flags

var (
	spawnCount        int
	spawnPrefix       string
	spawnFrom         string
	spawnNoGit        bool
	spawnSync         string
	spawnAgent        string
	spawnCopyStrategy string
	spawnDryRun       bool
	spawnExplain      bool
	spawnJSON         bool

	despawnAll              bool
	despawnForce            bool
	despawnKeepCheckpoints  bool
	despawnPrune            bool
	despawnDryRun           bool
	despawnExplain          bool
	despawnNoKeepCheckpoint bool

	spawnListJSON    bool
	spawnListVerbose bool
)

// ---------------------------------------------------------------------------
// Commands

var spawnCmd = &cobra.Command{
	Use:   "spawn [targets...]",
	Short: "Spawn one or more disposable, sync-connected workspaces from a snapshot",
	Long: `Create one or more workspace directories from a snapshot, ready for an
agent to start coding immediately. Each spawned dir is its own
independently-` + "`kai init`" + `'d repo; sync flows between them via the
shared remote.

The first spawn is materialized via 'kai checkout' from the object
store. Workspaces 2..N are CoW-cloned from workspace 1 (APFS clone on
macOS, reflink on btrfs/xfs) for near-zero disk overhead.

Examples:
  kai spawn /tmp/my-agent                         # one workspace
  kai spawn --count 4 --agent claude              # four, agents pre-registered
  kai spawn --count 3 --from @snap:login-fix
  kai spawn /tmp/experiment --sync none --no-git
`,
	RunE: runSpawn,
}

var despawnCmd = &cobra.Command{
	Use:   "despawn [targets...]",
	Short: "Tear down spawned workspaces",
	Long: `Remove spawned workspaces. Pushes any unpushed checkpoints first
(if a remote is configured), then deletes the workspace metadata and
the directory. Refuses to despawn workspaces with unpushed checkpoints
unless --force is passed.

Examples:
  kai despawn /tmp/my-agent
  kai despawn --all --prune
`,
	RunE: runDespawn,
}

var spawnListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active spawned workspaces",
	RunE:  runSpawnList,
}

func init() {
	spawnCmd.Flags().IntVar(&spawnCount, "count", 1, "Number of workspaces to create")
	spawnCmd.Flags().StringVar(&spawnPrefix, "prefix", "/tmp/kai-", "Path prefix for auto-generated directories")
	spawnCmd.Flags().StringVar(&spawnFrom, "from", "@snap:last", "Snapshot to spawn from")
	spawnCmd.Flags().BoolVar(&spawnNoGit, "no-git", false, "Skip git init in spawned workspaces")
	spawnCmd.Flags().StringVar(&spawnSync, "sync", "full", "Sync mode: full or none")
	spawnCmd.Flags().StringVar(&spawnAgent, "agent", "", "Agent name (numbered if --count > 1)")
	spawnCmd.Flags().StringVar(&spawnCopyStrategy, "copy-strategy", "auto", "Copy strategy: auto, cow, or full")
	spawnCmd.Flags().BoolVar(&spawnDryRun, "dry-run", false, "Print plan without executing")
	spawnCmd.Flags().BoolVar(&spawnExplain, "explain", false, "Print detailed walkthrough")
	spawnCmd.Flags().BoolVar(&spawnJSON, "json", false, "Output as JSON")

	despawnCmd.Flags().BoolVar(&despawnAll, "all", false, "Despawn all registered workspaces")
	despawnCmd.Flags().BoolVar(&despawnForce, "force", false, "Despawn even with unpushed checkpoints")
	despawnCmd.Flags().BoolVar(&despawnKeepCheckpoints, "keep-checkpoints", true, "Push checkpoints before teardown")
	despawnCmd.Flags().BoolVar(&despawnPrune, "prune", false, "Run kai prune after despawning")
	despawnCmd.Flags().BoolVar(&despawnDryRun, "dry-run", false, "Print what would be removed")
	despawnCmd.Flags().BoolVar(&despawnExplain, "explain", false, "Print detailed walkthrough")

	spawnListCmd.Flags().BoolVar(&spawnListJSON, "json", false, "Output as JSON")
	spawnListCmd.Flags().BoolVar(&spawnListVerbose, "verbose", false, "Show extra details")

	spawnCmd.AddCommand(spawnListCmd)
}

// ---------------------------------------------------------------------------
// runSpawn

func runSpawn(cmd *cobra.Command, args []string) error {
	// Source repo = cwd. Must be kai-init'd.
	srcRepo, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(srcRepo, kaiDir)); err != nil {
		return fmt.Errorf("not in a kai repo: run `kai init` first")
	}

	// Resolve target dirs.
	targets, err := resolveSpawnTargets(args, spawnCount, spawnPrefix)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no targets")
	}

	// Validate --sync.
	switch spawnSync {
	case "full", "none":
	default:
		return fmt.Errorf("--sync must be 'full' or 'none' (got %q)", spawnSync)
	}

	// Resolve source remote (only required if sync=full).
	var srcRemote *remote.RemoteEntry
	var remoteName string
	if spawnSync == "full" {
		remoteName = "origin"
		srcRemote, err = remote.GetRemote(remoteName)
		if err != nil || srcRemote == nil || srcRemote.Tenant == "default" {
			return fmt.Errorf("--sync full requires a remote; run `kai remote set origin <url>` first or pass --sync none")
		}
	}

	// Resolve --from snapshot to a hex ID using the source DB.
	srcSnapHex, err := resolveSourceSnapshot(srcRepo, spawnFrom)
	if err != nil {
		return fmt.Errorf("resolving --from %q: %w", spawnFrom, err)
	}

	// Detect copy strategy against the *target parent* dir (not source).
	parent := filepath.Dir(targets[0])
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("preparing target parent dir: %w", err)
	}
	resolved, err := spawnpkg.Detect(parent, spawnpkg.Strategy(spawnCopyStrategy))
	if err != nil {
		return err
	}

	if spawnDryRun {
		printDryRun(targets, srcSnapHex, resolved, srcRemote)
		return nil
	}

	// ----- Materialize workspace 1 -----
	first := targets[0]
	if _, err := os.Stat(first); err == nil {
		return fmt.Errorf("target %s already exists", first)
	}
	wsName1 := workspaceNameFor(first, 1)
	agent1 := agentNameFor(spawnAgent, 1, len(targets))

	if err := materializeFirst(srcRepo, first, srcSnapHex, wsName1, agent1, srcRemote); err != nil {
		return fmt.Errorf("materializing first workspace: %w", err)
	}
	if !spawnNoGit {
		if err := gitInitAndCommit(first, srcSnapHex); err != nil {
			fmt.Fprintf(os.Stderr, "warning: git init in %s failed: %v\n", first, err)
		}
	}

	// ----- CoW clone workspaces 2..N -----
	clones := make([]string, 0, len(targets)-1)
	for i := 1; i < len(targets); i++ {
		dst := targets[i]
		if _, err := os.Stat(dst); err == nil {
			return fmt.Errorf("target %s already exists", dst)
		}
		if err := spawnpkg.Copy(first, dst, resolved); err != nil {
			return fmt.Errorf("cloning %s → %s: %w", first, dst, err)
		}
		nameN := workspaceNameFor(dst, i+1)
		agentN := agentNameFor(spawnAgent, i+1, len(targets))
		// Resolve kai dir per-clone — the clone may not have the
		// same `.git/kai` vs `.kai` layout as the parent invoker.
		dstKai := kaipath.Resolve(dst)
		if err := spawnclone.RewriteClonedWorkspace(dstKai, nameN, agentN); err != nil {
			return fmt.Errorf("rewriting clone %s: %w", dst, err)
		}
		// Update the .kai/workspace pointer file inside the clone.
		if err := os.WriteFile(filepath.Join(dstKai, workspaceFile), []byte(nameN+"\n"), 0644); err != nil {
			return fmt.Errorf("setting current workspace in %s: %w", dst, err)
		}
		clones = append(clones, dst)
	}

	// ----- Register all entries -----
	entries := make([]spawnpkg.Entry, 0, len(targets))
	for i, dir := range targets {
		ent := spawnpkg.Entry{
			Path:           dir,
			WorkspaceName:  workspaceNameFor(dir, i+1),
			Agent:          agentNameFor(spawnAgent, i+1, len(targets)),
			SourceSnapshot: srcSnapHex,
			SourceRepo:     srcRepo,
			SyncMode:       spawnSync,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		if srcRemote != nil {
			ent.RemoteName = remoteName
			ent.RepoChannel = srcRemote.Tenant + "/" + srcRemote.Repo
		}
		if i > 0 {
			ent.CopySource = first
		}
		// Workspace ID lookup (for display) — best-effort.
		if id, err := lookupWorkspaceID(filepath.Join(dir, kaiDir), ent.WorkspaceName); err == nil {
			ent.WorkspaceID = id
		}
		entries = append(entries, ent)
	}
	if err := spawnpkg.Add(entries...); err != nil {
		return fmt.Errorf("writing registry: %w", err)
	}

	if spawnJSON {
		return emitSpawnJSON(entries, srcSnapHex, resolved)
	}
	printSpawnSummary(entries, srcSnapHex, resolved)
	return nil
}

// ---------------------------------------------------------------------------
// runDespawn

func runDespawn(cmd *cobra.Command, args []string) error {
	all, err := spawnpkg.List()
	if err != nil {
		return err
	}
	var targets []spawnpkg.Entry
	switch {
	case despawnAll:
		targets = all
	case len(args) == 0:
		return fmt.Errorf("pass a path or --all")
	default:
		// Resolve each arg against entry.Path or entry.WorkspaceName.
		for _, a := range args {
			abs, _ := filepath.Abs(a)
			matched := false
			for _, e := range all {
				if e.Path == a || e.Path == abs || e.WorkspaceName == a {
					targets = append(targets, e)
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("not a registered spawn: %s", a)
			}
		}
	}

	if despawnDryRun {
		fmt.Printf("would despawn %d workspaces:\n", len(targets))
		for _, t := range targets {
			fmt.Printf("  %s  ws:%s\n", t.Path, t.WorkspaceName)
		}
		return nil
	}

	for _, t := range targets {
		if err := despawnOne(t); err != nil {
			return fmt.Errorf("despawning %s: %w", t.Path, err)
		}
	}

	if despawnPrune {
		fmt.Println("running prune in source repo...")
		c := exec.Command(kaiExe(), "prune")
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: prune failed: %v\n", err)
		}
	}

	return nil
}

func despawnOne(e spawnpkg.Entry) error {
	kdPath := filepath.Join(e.Path, kaiDir)
	if _, err := os.Stat(kdPath); err == nil {
		rep, _ := spawnpkg.HasUnpushedCheckpoints(kdPath)
		if rep.Any() && !despawnForce {
			return fmt.Errorf("unpushed checkpoints (%d pending). Push first or pass --force",
				rep.PendingCheckpoints)
		}
		// Push if a remote is configured and we want to keep checkpoints.
		if e.RemoteName != "" && despawnKeepCheckpoints {
			c := exec.Command(kaiExe(), "push", e.RemoteName)
			c.Dir = e.Path
			out, err := c.CombinedOutput()
			if err != nil {
				return fmt.Errorf("push failed: %w: %s", err, string(out))
			}
			fmt.Printf("  %s  pushed to %s\n", e.Path, e.RemoteName)
		} else if e.RemoteName == "" && despawnKeepCheckpoints {
			fmt.Printf("  %s  no remote — checkpoints kept locally at %s\n", e.Path, kdPath)
		}
		// Delete the workspace from the dir's own DB (best-effort).
		if e.WorkspaceName != "" {
			c := exec.Command(kaiExe(), "ws", "delete", e.WorkspaceName, "--yes")
			c.Dir = e.Path
			_ = c.Run() // best-effort; rm -rf below makes this academic
		}
	}
	if err := os.RemoveAll(e.Path); err != nil {
		return fmt.Errorf("rm -rf: %w", err)
	}
	if err := spawnpkg.RemoveByPath(e.Path); err != nil {
		return fmt.Errorf("registry remove: %w", err)
	}
	fmt.Printf("  despawned %s\n", e.Path)
	return nil
}

// ---------------------------------------------------------------------------
// runSpawnList

func runSpawnList(cmd *cobra.Command, args []string) error {
	entries, err := spawnpkg.List()
	if err != nil {
		return err
	}
	// Drop entries whose dir vanished.
	live := entries[:0]
	for _, e := range entries {
		if _, err := os.Stat(e.Path); err == nil {
			live = append(live, e)
		}
	}
	if spawnListJSON {
		return json.NewEncoder(os.Stdout).Encode(live)
	}
	if len(live) == 0 {
		fmt.Println("no active spawned workspaces")
		return nil
	}
	fmt.Printf("%d active workspaces\n\n", len(live))
	for _, e := range live {
		uptime := ""
		if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
			uptime = humanDuration(time.Since(t))
		}
		fmt.Printf("  %s  ws:%s  agent:%s  sync:%s  uptime:%s\n",
			e.Path, e.WorkspaceName, e.Agent, e.SyncMode, uptime)
	}
	if len(live) > 0 && live[0].RepoChannel != "" {
		fmt.Printf("\nrepo channel: %s\n", live[0].RepoChannel)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers

func resolveSpawnTargets(args []string, count int, prefix string) ([]string, error) {
	if len(args) > 0 {
		out := make([]string, len(args))
		for i, a := range args {
			abs, err := filepath.Abs(a)
			if err != nil {
				return nil, err
			}
			out[i] = abs
		}
		return out, nil
	}
	if count < 1 {
		return nil, fmt.Errorf("--count must be >= 1")
	}
	out := make([]string, count)
	for i := 0; i < count; i++ {
		out[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return out, nil
}

func resolveSourceSnapshot(repo, sel string) (string, error) {
	db, err := graph.Open(filepath.Join(repo, kaiDir, dbFile),
		filepath.Join(repo, kaiDir, objectsDir))
	if err != nil {
		return "", err
	}
	defer db.Close()
	wantSnap := ref.KindSnapshot
	res, err := ref.NewResolver(db).Resolve(sel, &wantSnap)
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", fmt.Errorf("could not resolve %s", sel)
	}
	return util.BytesToHex(res.ID), nil
}

func materializeFirst(srcRepo, dst, snapHex, wsName, agentName string, rem *remote.RemoteEntry) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	// 1. kai checkout <snap> --dir <dst> (run in source repo so it sees the snapshot).
	c := exec.Command(kaiExe(), "checkout", snapHex, "--dir", dst)
	c.Dir = srcRepo
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("kai checkout: %w: %s", err, string(out))
	}
	// 1b. Symlink dependency directories from source into the
	// spawn dir. kai checkout copies source files but skips
	// node_modules / vendor / .venv (they're huge, not part of
	// the snapshot, and re-installing on every spawn would be
	// untenable). Without this step the spawned agent's tests
	// fail with "module not found" the moment they import a
	// dependency, and the agent can spin in a fix-edit-retest
	// loop trying to "fix" code that's actually fine.
	//
	// Symlink (not copy) for speed and disk efficiency. The
	// dep dirs are read-mostly; agent edits to them would be
	// unusual and would correctly affect the source repo too —
	// which the user can revert if needed.
	linkDeps(srcRepo, dst)

	// 2. kai init in dst (fresh kai dir).
	//
	// --force is required here because the source's kai checkout in
	// step 1 may have materialized a kai.projects.yaml into dst (it's
	// a normal file in the source repo's working tree). The
	// container/project guard in runInit refuses to init when that
	// yaml is present at cwd — correctly, for the human-typing-kai-
	// init case, but the spawn flow IS the exception: dst is a
	// brand-new throwaway directory that we genuinely need to set up
	// as a kai workspace regardless of what files the snapshot
	// happens to contain. Without --force, every spawn from a repo
	// whose source includes kai.projects.yaml fails preflight with a
	// confusing "refusing to init" error (May 2026 dogfood case).
	if err := runIn(dst, "init", "--force"); err != nil {
		return fmt.Errorf("kai init: %w", err)
	}
	// 3. Copy remote config from source if available.
	//
	// Two reasons the source file may not exist even when `rem`
	// loaded successfully:
	//   - The remote came from global config (~/.kai/remotes.json)
	//     or environment, not the per-project file.
	//   - The user blew away /tmp or the source kai dir was
	//     re-initialized after a wipe.
	// Either way: skip the copy silently and let the spawned
	// workspace inherit the global config naturally. Same for
	// when the dst's kai dir resolves to a different path than
	// src (kaipath chooses .git/kai vs .kai based on git
	// presence) — we recompute from kaipath instead of reusing
	// the variable that was bound at process start.
	if rem != nil {
		srcRemotes := filepath.Join(srcRepo, kaiDir, "remotes.json")
		if _, err := os.Stat(srcRemotes); err == nil {
			dstKaiDir := kaipath.Resolve(dst)
			if err := os.MkdirAll(dstKaiDir, 0o755); err != nil {
				return fmt.Errorf("preparing dst kai dir: %w", err)
			}
			dstRemotes := filepath.Join(dstKaiDir, "remotes.json")
			if err := copyFile(srcRemotes, dstRemotes); err != nil {
				return fmt.Errorf("copying remote config: %w", err)
			}
		}
	}
	// 4. kai capture (snapshot the materialized files as the new baseline).
	//
	// KAI_CAPTURE_SKIP_SUMMARY=1 by default — the spawn baseline has
	// no human reader for the "X files changed" summary, and the
	// summary phase parses every file with tree-sitter and AST-diffs
	// old vs new. On a workspace with a 20k-line main.go that phase
	// can spike memory enough for the OS to SIGKILL the process
	// (2026-05-14 dogfood: 'kai capture: signal: killed' on a fresh
	// spawn). Skipping the summary keeps snap.latest, the object
	// store, and analyze edges all correct — only the unused
	// summary line is empty.
	//
	// On any capture failure (OOM-kill, lock contention, etc.) we
	// retry once with the parsing layer reduced further via
	// KAI_CAPTURE_NO_ANALYZE. If that also fails, the error
	// surfaces to the caller with both attempt diagnostics.
	if err := runInWithEnvSpawn(dst, []string{"KAI_CAPTURE_SKIP_SUMMARY=1"}, "capture", "-m", "kai spawn baseline"); err != nil {
		// Already minimal-mode; retrying the same command would
		// hit the same wall. Pass the error up.
		return fmt.Errorf("kai capture: %w", err)
	}
	// 5. kai ws create <wsName>.
	if err := runIn(dst, "ws", "create", wsName); err != nil {
		return fmt.Errorf("kai ws create: %w", err)
	}
	// 6. kai ws checkout <wsName> (sets .kai/workspace pointer).
	if err := runIn(dst, "ws", "checkout", wsName); err != nil {
		return fmt.Errorf("kai ws checkout: %w", err)
	}
	// 7. SetAgentName via in-process workspace mgr.
	//
	// kaipath.Resolve(dst) — NOT the global kaiDir — because the
	// spawn dir's kai data location is independent of the source
	// project's. Source might have .git/kai (git-tracked kai), the
	// spawn dir might end up with .kai (no .git after `kai init`).
	// Reusing the parent's kaiDir against dst gives the wrong
	// path; SQLite returns SQLITE_CANTOPEN ("unable to open
	// database file: out of memory") which looks alarming but is
	// just a missing-path error.
	if agentName != "" {
		dstKaiDir := kaipath.Resolve(dst)
		db, err := graph.Open(filepath.Join(dstKaiDir, dbFile),
			filepath.Join(dstKaiDir, objectsDir))
		if err != nil {
			return fmt.Errorf("opening dst db for agent name: %w", err)
		}
		mgr := workspace.NewManager(db)
		err = mgr.SetAgentName(wsName, agentName)
		db.Close()
		if err != nil {
			return fmt.Errorf("setting agent name: %w", err)
		}
	}
	return nil
}

func gitInitAndCommit(dir, snapHex string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return err
	}
	cmds := [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "add", "-A"},
		{"git", "commit", "-q", "-m", "kai spawn from " + snapHex[:12]},
	}
	for _, args := range cmds {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, string(out))
		}
	}
	return nil
}

func runIn(dir string, args ...string) error {
	c := exec.Command(kaiExe(), args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// runInWithEnvSpawn runs the kai binary in dir with env appended to
// the parent's environment. Same signature shape as runIn but with
// an env slice; the spawn-baseline capture uses this to inject
// KAI_CAPTURE_SKIP_SUMMARY=1 so the heavy tree-sitter parsing phase
// is skipped (the OOM-kill culprit). Separate from
// orchestrator/runInWithEnv because spawn.go runs in a different
// process context with no inherited context.Context.
func runInWithEnvSpawn(dir string, env []string, args ...string) error {
	c := exec.Command(kaiExe(), args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func kaiExe() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "kai"
}

// linkDeps symlinks dependency directories from src into dst when
// they exist in src and don't already exist in dst. Targets a
// common set across language ecosystems:
//
//   - node_modules  (Node)
//   - vendor        (Go vendor mode, also PHP composer)
//   - .venv / venv  (Python virtualenvs)
//
// Best-effort: failures are silently ignored — the worst case is
// the agent's tests fail with "module not found" and the user
// notices, which is no worse than today's behavior. The dst is
// guaranteed to exist (caller MkdirAll'd it), and the src dirs
// are read-mostly (agents almost never edit them) so a symlink
// is fine vs. a deep copy.
//
// We use os.Symlink, not bind-mounts: cross-platform, no
// privileges needed, transparent to most tooling.
func linkDeps(src, dst string) {
	for _, name := range []string{"node_modules", "vendor", ".venv", "venv"} {
		srcPath := filepath.Join(src, name)
		info, err := os.Lstat(srcPath)
		if err != nil || !info.IsDir() {
			continue
		}
		dstPath := filepath.Join(dst, name)
		if _, err := os.Lstat(dstPath); err == nil {
			continue // something already there; don't clobber
		}
		_ = os.Symlink(srcPath, dstPath)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func workspaceNameFor(_ string, n int) string {
	return fmt.Sprintf("spawn-%d", n)
}

func agentNameFor(base string, n, total int) string {
	if base == "" {
		return ""
	}
	if total == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n)
}

func lookupWorkspaceID(kaiDirPath, name string) (string, error) {
	db, err := graph.Open(filepath.Join(kaiDirPath, dbFile),
		filepath.Join(kaiDirPath, objectsDir))
	if err != nil {
		return "", err
	}
	defer db.Close()
	ws, err := workspace.NewManager(db).Get(name)
	if err != nil || ws == nil {
		return "", fmt.Errorf("not found")
	}
	return hex.EncodeToString(ws.ID), nil
}

func printDryRun(targets []string, snapHex string, r spawnpkg.Resolved, rem *remote.RemoteEntry) {
	fmt.Printf("would spawn %d workspaces from snap %s (copy: %s)\n",
		len(targets), snapHex[:12], r)
	for i, t := range targets {
		fmt.Printf("  %s  ws:%s  agent:%s\n", t,
			workspaceNameFor(t, i+1),
			agentNameFor(spawnAgent, i+1, len(targets)))
	}
	if rem != nil {
		fmt.Printf("repo channel: %s/%s\n", rem.Tenant, rem.Repo)
	}
}

func printSpawnSummary(entries []spawnpkg.Entry, snapHex string, r spawnpkg.Resolved) {
	fmt.Printf("spawned %d workspaces from snap %s\n\n", len(entries), snapHex[:12])
	for _, e := range entries {
		fmt.Printf("  %s  ws:%s  agent:%s  sync:%s\n",
			e.Path, e.WorkspaceName, e.Agent, e.SyncMode)
	}
	fmt.Printf("\ncopy strategy: %s", r)
	if len(entries) > 1 {
		fmt.Printf(" (workspaces 2-%d cloned from workspace 1)", len(entries))
	}
	fmt.Println()
	if entries[0].RepoChannel != "" {
		fmt.Printf("repo channel: %s\n", entries[0].RepoChannel)
	}
}

func emitSpawnJSON(entries []spawnpkg.Entry, snapHex string, r spawnpkg.Resolved) error {
	out := map[string]interface{}{
		"source_snapshot": snapHex,
		"copy_strategy":   string(r),
		"workspaces":      entries,
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
