package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"kai/internal/authorship"
	"kai/internal/livesync"
	"kai/internal/ref"
	"kai/internal/remote"
	"kai/internal/watcher"
)

// liveRunCmd runs a foreground bidirectional peer-sync session for the
// current workspace: it pushes local edits up and applies peers' edits to
// disk, until interrupted. This is the non-MCP path — two plain `kai`
// clients that `kai ws checkout <same-ws>` and run `kai live run` sync with
// each other live.
var liveRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a foreground live peer-sync session for the current workspace",
	Long: `Start a live peer-sync session and block until interrupted (Ctrl-C).

Local edits are pushed to peers on the same workspace, and peers' edits are
applied to your working tree (3-way merged when you both touched a file).
Scoped to the checked-out workspace — peers on other workspaces of the same
repo are not synced.

Examples:
  kai ws checkout feat   # on each client
  kai live run           # in each client's terminal`,
	RunE: runLiveRun,
}

// liveRunCheckpoint, when set, runs the session in checkpoint mode: local
// edits are held and pushed only on `kai live checkpoint`. Auto-sync passes
// this when the repo's persisted sync-mode is "checkpoint".
var liveRunCheckpoint bool

var liveCheckpointCmd = &cobra.Command{
	Use:   "checkpoint",
	Short: "Push your accumulated edits now (checkpoint-mode flush)",
	Long: `Flush local edits to peers immediately.

In checkpoint mode this is how your changes reach peers — it signals the
running live-sync session to push everything you've edited since the last
checkpoint. In live mode it's a no-op (your edits already stream out).`,
	RunE: runLiveCheckpoint,
}

var liveModeCmd = &cobra.Command{
	Use:   "mode [live|checkpoint]",
	Short: "Show or set the sync mode for this repo",
	Long: `Without an argument, prints the current sync mode.

  live        push every change as you edit (default)
  checkpoint  hold local edits and push only on 'kai live checkpoint'

Receiving peers' edits is unaffected by the mode. Changing the mode restarts
any running auto-sync daemon for the current workspace.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLiveMode,
}

func runLiveCheckpoint(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat(kaiDir); err != nil {
		return fmt.Errorf("not in a kai repo: run `kai init` first")
	}
	data, err := os.ReadFile(liveRunPidPath(kaiDir))
	if err != nil {
		return fmt.Errorf("no live sync running (start with `kai ws checkout` or `kai live run`)")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !processAlive(pid) {
		return fmt.Errorf("no live sync running")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding live sync process: %w", err)
	}
	if err := proc.Signal(syscall.SIGUSR1); err != nil {
		return fmt.Errorf("signaling live sync: %w", err)
	}
	fmt.Println("Checkpoint flushed to live sync.")
	return nil
}

func runLiveMode(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat(kaiDir); err != nil {
		return fmt.Errorf("not in a kai repo: run `kai init` first")
	}
	if len(args) == 0 {
		fmt.Printf("Sync mode: %s\n", readSyncMode(kaiDir))
		return nil
	}
	mode := args[0]
	switch mode {
	case "live":
		os.Remove(syncModePath(kaiDir))
	case "checkpoint":
		if err := os.WriteFile(syncModePath(kaiDir), []byte("checkpoint\n"), 0644); err != nil {
			return fmt.Errorf("writing sync-mode: %w", err)
		}
	default:
		return fmt.Errorf("mode must be 'live' or 'checkpoint'")
	}
	fmt.Printf("Sync mode set to %s.\n", mode)
	// Apply to a running auto-sync daemon by restarting it for the current ws.
	if autoSyncRunningPid(kaiDir) > 0 {
		if ws, _ := getCurrentWorkspace(); ws != "" {
			stopAutoSync(kaiDir)
			startAutoSync(kaiDir, ws)
		}
	}
	return nil
}

func runLiveRun(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat(kaiDir); err != nil {
		return fmt.Errorf("not in a kai repo: run `kai init` first")
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}

	// Single-instance guard: only one live-sync daemon per repo. Prevents
	// orphaned daemons (from repeated checkout/off cycles) from accumulating
	// and fighting over the same files. Held for this process's lifetime.
	lockFile, ok := acquireLiveRunLock(kaiDir)
	if !ok {
		fmt.Println("Live sync is already running for this repo — not starting another. (Stop it with `kai live off`.)")
		return nil
	}
	if lockFile != nil {
		defer lockFile.Close() // releases the flock on exit
	}
	// Publish OUR pid as the canonical autosync pid now that we hold the lock,
	// so `kai live off` / workspace-switch always SIGTERM the real running
	// daemon — even a manually-started one — not a stale pidfile entry.
	_ = os.WriteFile(autoSyncPidPath(kaiDir), []byte(strconv.Itoa(os.Getpid())), 0644)

	client, err := remote.NewClientForRemote("origin")
	if err != nil {
		return fmt.Errorf("live sync: no `origin` remote configured (`kai remote set origin <url>`): %w", err)
	}
	if client.AuthToken == "" {
		return fmt.Errorf("live sync: not logged in (`kai auth login`)")
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	ws, _ := getCurrentWorkspace()
	sessionID := fmt.Sprintf("kai-cli_%d", os.Getpid())
	// CRDT op-transport is the default for peer sync. Emergency rollback to the
	// whole-file + server-merge path: KAI_DISABLE_CRDT_SYNC=1.
	crdtMode := os.Getenv("KAI_DISABLE_CRDT_SYNC") != "1"
	eng := livesync.New(livesync.Options{
		WorkDir:          workDir,
		KaiDir:           kaiDir,
		DB:               db,
		Resolver:         ref.NewResolver(db),
		Client:           client,
		Agent:            "kai-cli",
		SessionID:        sessionID,
		Workspace:        ws,
		CheckpointWriter: authorship.NewCheckpointWriter(kaiDir, sessionID),
		CRDTMode:         crdtMode,
	})

	// Sync mode. In checkpoint mode local edits accumulate and are pushed only
	// on an explicit checkpoint (`kai live checkpoint` → SIGUSR1); live mode
	// (default) pushes every change. Receiving peers' edits is unaffected by
	// the mode. The flag wins; otherwise the persisted per-repo mode applies.
	checkpointMode := liveRunCheckpoint || readSyncMode(kaiDir) == "checkpoint"
	var dirtyMu sync.Mutex
	dirty := make(map[string]bool)

	// File watcher: feed changed paths to the engine's push side.
	w, err := watcher.New(workDir, db)
	if err != nil {
		return fmt.Errorf("starting file watcher: %w", err)
	}
	w.OnError = func(err error) { fmt.Fprintf(os.Stderr, "[kai-watcher] %v\n", err) }
	w.OnActivity = func(entries []watcher.ActivityEntry) {
		paths := make([]string, 0, len(entries))
		for _, e := range entries {
			paths = append(paths, e.Path)
		}
		if checkpointMode {
			dirtyMu.Lock()
			for _, p := range paths {
				dirty[p] = true
			}
			dirtyMu.Unlock()
			return
		}
		eng.PushChanges(paths)
	}

	// flushCheckpoint pushes everything edited since the last checkpoint.
	flushCheckpoint := func() {
		dirtyMu.Lock()
		paths := make([]string, 0, len(dirty))
		for p := range dirty {
			paths = append(paths, p)
		}
		dirty = make(map[string]bool)
		dirtyMu.Unlock()
		if len(paths) > 0 {
			eng.PushChanges(paths)
			fmt.Printf("Live sync: checkpoint pushed %d file(s)\n", len(paths))
		}
	}

	// Start the watcher first so local graph updates work even before we
	// reach the network. Pushes no-op until the engine is subscribed.
	if err := w.Start(); err != nil {
		return fmt.Errorf("starting watcher: %w", err)
	}

	// Publish our pid so `kai live checkpoint` can signal us (SIGUSR1).
	_ = os.WriteFile(liveRunPidPath(kaiDir), []byte(strconv.Itoa(os.Getpid())), 0644)
	defer os.Remove(liveRunPidPath(kaiDir))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	chk := make(chan os.Signal, 1)
	signal.Notify(chk, syscall.SIGUSR1)

	wsLabel := ws
	if wsLabel == "" {
		wsLabel = "(repo-wide)"
	}

	// Connect, tolerating offline: probe the server, subscribe when reachable,
	// otherwise wait and retry with backoff (Ctrl-C / SIGTERM exits cleanly).
	// Once subscribed, the engine's SSE loop self-reconnects on later drops.
	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second
	wasOffline := false
	connected := false
	for !connected {
		select {
		case <-sig:
			fmt.Println("\nStopping live sync...")
			w.Stop()
			return nil
		default:
		}

		if probeOnline(client.BaseURL) {
			synced, err := eng.Start(nil)
			if err != nil {
				fmt.Printf("Live sync: connect failed (%v) — retrying...\n", err)
			} else {
				eng.SaveState(nil)
				connected = true
				fmt.Printf("Live sync running for workspace %s on %s\n", wsLabel, client.BaseURL)
				if synced > 0 {
					fmt.Printf("  applied %d file(s) from server on connect\n", synced)
				}
				if checkpointMode {
					fmt.Println("  checkpoint mode: applying peer edits live; run 'kai live checkpoint' to push yours — Ctrl-C to stop")
				} else {
					fmt.Println("  pushing local edits + applying peer edits — Ctrl-C to stop")
				}
				break
			}
		} else if !wasOffline {
			wasOffline = true
			fmt.Printf("Live sync: offline — waiting for network to reach %s...\n", client.BaseURL)
		}

		select {
		case <-sig:
			fmt.Println("\nStopping live sync...")
			w.Stop()
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	for {
		select {
		case <-sig:
			fmt.Println("\nStopping live sync...")
			if checkpointMode {
				flushCheckpoint() // don't lose the last batch
			}
			w.Stop()
			eng.Stop()
			eng.ClearState()
			// Give any in-flight push/apply a moment to settle.
			time.Sleep(200 * time.Millisecond)
			return nil
		case <-chk:
			flushCheckpoint()
		}
	}
}

// liveSyncWiring is what setupLiveSync produces: a broadcast hook the
// orchestrator passes to the in-process agent, plus a stop function
// to release the kailab subscription on TUI exit.
type liveSyncWiring struct {
	// Broadcast forwards one file change to the kailab live-sync
	// channel. Best-effort — failures don't propagate; the TUI's
	// sync pane still shows the activity from the agent's local hook.
	Broadcast func(relPath, digest, contentBase64 string)
	// Stop releases the subscription. Safe to call multiple times.
	Stop func()
}

// setupLiveSync configures live-sync broadcasting for an in-process
// agent run. Behavior:
//
//   - If `<kaiDir>/sync-state.json` doesn't exist or has Enabled=false,
//     returns (nil, nil) — live sync is just disabled, not an error.
//     User runs `kai live on` to enable it.
//   - If a remote isn't configured or auth is missing, returns
//     (nil, err) so the caller surfaces a clear message about why
//     live sync isn't going to work.
//   - On success, returns a wiring with Broadcast + Stop.
//
// Subscription is one-shot per kai-code session: we register a channel
// when the TUI starts and tear it down on exit. Channel agent name is
// `kai-code:<pid>` so multiple kai-code sessions don't collide.
func setupLiveSync(kaiDir string) (*liveSyncWiring, error) {
	state, ok := readLiveSyncState(kaiDir)
	if !ok || !state.Enabled {
		return nil, nil
	}

	client, err := remote.NewClientForRemote("origin")
	if err != nil {
		return nil, fmt.Errorf("live sync: no `origin` remote configured (`kai remote set origin <url>`): %w", err)
	}
	if client.AuthToken == "" {
		return nil, fmt.Errorf("live sync: not logged in (`kai auth login`)")
	}

	agent := fmt.Sprintf("kai-code:%d", os.Getpid())
	// Scope to the current workspace (empty = repo-wide, backward compatible).
	cw, _ := getCurrentWorkspace()
	resp, err := client.SubscribeSync(agent, client.Actor, cw, state.Files)
	if err != nil {
		return nil, fmt.Errorf("live sync: subscribe failed: %w", err)
	}
	channelID := resp.ChannelID

	stopped := false
	return &liveSyncWiring{
		Broadcast: func(rel, digest, b64 string) {
			// Push errors are intentionally swallowed: a one-off
			// network blip shouldn't surface as a tool failure to
			// the agent. The local OnFileChange hook already gave
			// the user immediate visibility.
			// No tracked merge base on this legacy TUI broadcast path; send
			// empty base so the server treats it as a last-write relay.
			_ = client.SyncPushFile(agent, channelID, rel, digest, b64, "")
		},
		Stop: func() {
			if stopped {
				return
			}
			stopped = true
			_ = client.UnsubscribeSync(channelID)
		},
	}, nil
}

// orchLiveSync converts a liveSyncWiring (or nil) into the
// `func(...)` shape orchestrator.Config.LiveSync expects. Returns
// nil when wiring is nil so the orchestrator's nil-check at the
// hook site routes the agent's file writes only to the local
// OnFileChange callback (no broadcast attempted).
func orchLiveSync(w *liveSyncWiring) func(string, string, string) {
	if w == nil {
		return nil
	}
	return w.Broadcast
}

// readLiveSyncState reads `<kaiDir>/sync-state.json`. Mirrors
// `liveSyncState` defined in main.go (which I can't reach from here
// without circular import gymnastics in cmd/kai). Returns ok=false on
// any read or parse error so callers can simply skip live sync
// without worrying about whether the file is missing vs malformed.
func readLiveSyncState(kaiDir string) (*liveSyncState, bool) {
	data, err := os.ReadFile(filepath.Join(kaiDir, "sync-state.json"))
	if err != nil {
		return nil, false
	}
	var st liveSyncState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false
	}
	return &st, true
}
