// Package tui is the Bubble Tea front-end for kai. When the kai binary
// is invoked with no subcommand, cmd/kai/tui.go calls Run, which boots
// a three-pane interface: gate (held integrations), sync (live agent
// activity), and REPL (input/output).
//
// All rendering happens here; engine work is delegated in-process to
// kai-cli/internal/* packages. Nothing in this package owns business
// logic beyond layout and event routing.
package tui

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/sys/unix"

	"kai/api/graph"
	"kai/api/provider"
	"kai/api/memstat"
	"kai/api/projects"
	"kai/internal/tui/views"
	"kai/api/watcher"
)

// Options configures a TUI session. The TUI reads from a live graph
// DB and a kai data directory; the caller (cmd/kai) opens both before
// handing them in so the TUI doesn't duplicate path resolution logic.
type Options struct {
	// Projects, when set, advertises the multi-root workspace the
	// user opened. The TUI's internals (file watcher, REPL working
	// dir) use Projects.Primary() for path-bound operations — full
	// multi-root file watching is a known follow-up. Header rendering
	// uses the full project list.
	Projects *projects.Set

	DB      *graph.DB
	KaiDir  string
	WorkDir string
	// Binary is the path to the kai executable used by the REPL when
	// shelling out subcommands. Defaults to os.Args[0] if empty.
	Binary string
	// Planner enables natural-language input in the REPL. Nil → REPL
	// shells out everything (no planner path).
	Planner *views.PlannerServices

	// ResumeSessionID, when non-empty, hands the REPL a prior
	// session id to resume. The agent runner's session.Resume picks
	// up the persisted transcript so the model has its prior
	// context. Scrollback and other in-memory TUI state are NOT
	// restored — only the agent conversation continues.
	ResumeSessionID string
}

// terminalResetSequences disables every xterm mouse-tracking mode,
// re-shows the cursor, and exits the alt-screen. We emit these
// directly to /dev/tty (not stderr — the user may have redirected
// stderr) on any abnormal exit path, so the shell isn't left
// printing "64;55;19M" gibberish from the terminal's mouse-event
// reports after a SIGKILL.
//
// Ordering matters slightly: turn off mouse modes BEFORE leaving
// the alt-screen so the modes-disable bytes don't end up echoed
// into the user's scrollback. Cursor-show last so it lands in the
// restored main buffer.
const terminalResetSequences = "" +
	"\x1b[?1000l" + // disable X10 mouse tracking
	"\x1b[?1002l" + // disable cell-motion tracking
	"\x1b[?1003l" + // disable any-event tracking
	"\x1b[?1006l" + // disable SGR mouse encoding
	"\x1b[?1015l" + // disable urxvt mouse encoding
	"\x1b[?1049l" + // exit alt-screen, restore main buffer
	"\x1b[?25h" //   show cursor

// resetTerminalRaw writes the cleanup sequence directly to the
// controlling terminal. Best-effort: if /dev/tty can't be opened
// (no tty, container), we fall back to stderr. Errors swallowed —
// failing to clean up the terminal shouldn't itself produce more
// noise.
func resetTerminalRaw() {
	if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		_, _ = f.WriteString(terminalResetSequences)
		_ = f.Close()
		return
	}
	_, _ = os.Stderr.WriteString(terminalResetSequences)
}

// Run starts the TUI event loop. Blocks until the user quits.
func Run(ctx context.Context, opts Options) error {
	if opts.DB == nil {
		return fmt.Errorf("tui.Run: DB is required")
	}
	if opts.Binary == "" {
		opts.Binary = os.Args[0]
	}

	// Memory telemetry. Boots a sample at startup, then a
	// background sampler at 60s intervals so we capture growth
	// during long idle stretches — the window where macOS reaches
	// for kai during memory pressure. Output: ~/.kai/memory-stats.log.
	// Always-on, local-only, lossy by design (a failed write does
	// not bubble up). The deferred close on done stops the sampler
	// at TUI exit.
	memstat.Log("tui-boot")
	memstatDone := make(chan struct{})
	defer close(memstatDone)
	memstat.StartIdleSampler(60*time.Second, memstatDone)

	// Best-effort: lower nice value so the kernel sees kai as slightly
	// higher-priority than ambient background work. This is mostly
	// cosmetic for the macOS jetsam decision — true jetsam priority
	// requires entitlements only granted to system-signed apps — but
	// it does signal "this process is doing user-facing work" via the
	// POSIX scheduler. Silent failure on permission denied (the
	// common case for non-root users; the kernel just keeps us at the
	// default priority).
	_ = unix.Setpriority(unix.PRIO_PROCESS, 0, -5)

	// Defensive terminal cleanup. Bubble Tea normally tears down
	// mouse tracking + alt-screen on its own, but a panic, an
	// uncaught SIGTERM from outside the bubbletea loop (parent
	// shell killing the process), or an OOM kill all bypass that
	// teardown — and the user is left with a wedged shell echoing
	// "64;55;19M" mouse-event escapes for every wheel tick. The
	// defer runs even when the bubbletea program panics; the
	// signal handler covers SIGTERM/SIGHUP which bubbletea
	// doesn't intercept (it only catches SIGINT for KeyCtrlC).
	defer resetTerminalRaw()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		s, ok := <-sigCh
		if !ok {
			return
		}
		resetTerminalRaw()
		// Re-raise the signal so the process exits with the
		// conventional code (128 + signum) instead of clean 0.
		// Without this, scripts that check $? after `kai code`
		// can't tell normal exit from "killed by signal."
		signal.Reset(s)
		_ = syscall.Kill(syscall.Getpid(), s.(syscall.Signal))
	}()
	defer signal.Stop(sigCh)

	// Buffered so the watcher's callback (which fires from its event
	// loop goroutine) never blocks waiting for the pump to drain.
	syncCh := make(chan views.SyncEvent, 256)

	// Chat-activity channel: the chat-fallback agent's tool/file
	// hooks push tool dispatches and file mutations through here so
	// REPL renders them inline ("→ write package.json"). Sized large
	// enough that a chatty turn doesn't drop events; non-blocking
	// sends in the hook handle overflow gracefully.
	chatCh := make(chan views.ChatActivityEvent, 64)
	if opts.Planner != nil {
		opts.Planner.ChatActivityCh = chatCh
	}

	// Managed-process event channel (host_proc.go). Same fan-out
	// pattern as ChatActivityCh — buffered, non-blocking sends in
	// the scanner. 16 events is plenty: the scanner ticks every 2s
	// and dedupes, so even a thrashing dev-server emits well under
	// the cap.
	hostProcCh := make(chan views.HostProcEvent, 16)
	if opts.Planner != nil {
		opts.Planner.HostProcEventCh = hostProcCh
	}

	w, watcherErr := startWatcher(opts, syncCh)
	if w != nil {
		defer w.Stop()
	}

	// Wire the orchestrator's per-spawn agent activity into the same
	// sync channel the main-repo watcher uses. Tagged with the spawn
	// name so the user can see which agent did what; non-blocking
	// send drops on backpressure (better to lose a render than stall
	// the agent's session).
	if opts.Planner != nil && opts.Planner.OrchestratorCfg.OnActivity == nil {
		opts.Planner.OrchestratorCfg.OnActivity = func(spawnName, relPath, op string) {
			// Three flavors flow through OnActivity:
			//   - "(assistant)": per-turn assistant prose from a
			//     spawned agent. Route to chatCh so it lands inline
			//     in the conversation scroll — otherwise users staring
			//     at a multi-minute agent run see no progress at all.
			//   - "(tool)": tool dispatch ("→ spawn: bash"). Same
			//     reasoning — surfaces what the agent is *doing*.
			//   - file paths: filesystem activity. Stays on syncCh
			//     so the sync pane / status bar render it.
			//
			// chatCh is unbounded enough to absorb chatty agents but
			// the send is non-blocking — better to lose a single line
			// than stall the agent's session if the REPL is paused.
			switch relPath {
			case "(assistant)":
				if chatCh != nil {
					summary := firstNonEmptyLine(op)
					if summary == "" {
						return
					}
					select {
					case chatCh <- views.ChatActivityEvent{
						Kind:    "agent_text",
						Summary: summary,
						When:    time.Now(),
					}:
					default:
					}
				}
			case "(tool)":
				if chatCh != nil {
					select {
					case chatCh <- views.ChatActivityEvent{
						Kind:    "tool",
						Summary: op,
						When:    time.Now(),
					}:
					default:
					}
				}
			default:
				select {
				case syncCh <- views.SyncEvent{
					Path: spawnName + ": " + relPath,
					Op:   op,
					When: time.Now(),
				}:
				default:
				}
			}
		}
	}

	// OrchestratorCfg.OnFileDiff: orchestrator-spawned agents fire
	// per-edit diffs the same way the chat agent does. Forward
	// them into the chat-activity channel as `kind: "diff"` so the
	// REPL renders them as inline `Update(file.go) +12 -3` blocks.
	// Without this hook the orchestrator path silently produces
	// edits that the user can't see — feels like a regression
	// after using the chat agent.
	// OrchestratorCfg.OnAgentLifecycle: forward spawn-agent
	// start/end events into the chat-activity channel so the
	// status bar's Agents counter increments for orchestrator
	// runs (not just chat-fallback runs). Without this the
	// counter sits at 0 even while spawn agents are clearly
	// active in the Sync pane.
	if opts.Planner != nil && opts.Planner.OrchestratorCfg.OnAgentLifecycle == nil && chatCh != nil {
		opts.Planner.OrchestratorCfg.OnAgentLifecycle = func(spawnName, event string) {
			var kind string
			switch event {
			case "start":
				kind = "agent_start"
			case "end":
				kind = "agent_end"
			case "verify_start":
				kind = "verify_start"
			case "verify_end":
				kind = "verify_end"
			case "test_start":
				kind = "test_start"
			case "test_end":
				kind = "test_end"
			case "test_skipped":
				kind = "test_skipped"
			default:
				kind = "agent_start"
			}
			select {
			case chatCh <- views.ChatActivityEvent{
				Kind:    kind,
				Summary: spawnName,
				When:    time.Now(),
			}:
			default:
			}
		}
	}

	// OrchestratorCfg.OnAgentProviderState: forward each spawned
	// agent's HTTP/SSE lifecycle through chatCh as a "provider_state"
	// event so the spinner can show the actual call state of whichever
	// agent is currently working. Tagged with spawn name when we have
	// multiple agents in flight so it's clear which call the state
	// refers to.
	if opts.Planner != nil && opts.Planner.OrchestratorCfg.OnAgentProviderState == nil && chatCh != nil {
		opts.Planner.OrchestratorCfg.OnAgentProviderState = func(spawnName string, state provider.RequestState) {
			select {
			case chatCh <- views.ChatActivityEvent{
				Kind:    "provider_state",
				Summary: spawnName + ": " + views.ProviderStateSummary(state),
				When:    state.When,
			}:
			default:
			}
		}
	}

	// Planner's own provider state is wired inside
	// buildPlannerAgent (which constructs a fresh PlannerAgent per
	// turn from PlannerServices.ChatActivityCh) — no app-level
	// hook needed.

	// OrchestratorCfg.OnAgentBashOutput: forward each spawned
	// agent's bash stdout/stderr through chatCh so the user sees
	// what the command actually printed. Without this the orchestrator
	// path runs `make` / `./hello_world` and the user only sees the
	// dispatch indicator — no idea what was built or what was output.
	// Already throttled to 20 lines per bash call in the orchestrator
	// hook so a chatty command can't flood the pane.
	if opts.Planner != nil && opts.Planner.OrchestratorCfg.OnAgentBashOutput == nil && chatCh != nil {
		opts.Planner.OrchestratorCfg.OnAgentBashOutput = func(spawnName, line string) {
			select {
			case chatCh <- views.ChatActivityEvent{
				Kind:    "bash",
				Summary: line,
				When:    time.Now(),
			}:
			default:
			}
		}
	}

	// OrchestratorCfg.OnAgentFileConfirm: per-write approval gate.
	// Same channel-blocking pattern as the bash confirm: ship a
	// "file_confirm" event with a reply chan, wait for the REPL to
	// answer y/a/n. A closed channel is treated as cancel for the
	// same shutdown-race reason as bash.
	if opts.Planner != nil && opts.Planner.OrchestratorCfg.OnAgentFileConfirm == nil && chatCh != nil {
		opts.Planner.OrchestratorCfg.OnAgentFileConfirm = func(spawnName, op, path string, added, removed int, diff string) bool {
			reply := make(chan bool, 1)
			select {
			case chatCh <- views.ChatActivityEvent{
				Kind:      "file_confirm",
				SpawnName: spawnName,
				Op:        op,
				Path:      path,
				Added:     added,
				Removed:   removed,
				Diff:      diff,
				Reply:     reply,
				When:      time.Now(),
			}:
			default:
				return false
			}
			ok, open := <-reply
			if !open {
				return false
			}
			return ok
		}
	}

	// OrchestratorCfg.OnAgentBashConfirm: per-command bash approval.
	// Allowlist passes auto; everything else routes through here.
	// Implementation sends a "bash_confirm" event with a reply chan,
	// then blocks until the REPL writes the user's decision back.
	// A closed channel is treated as cancel — guards against shutdown
	// races where the REPL goes away mid-prompt.
	if opts.Planner != nil && opts.Planner.OrchestratorCfg.OnAgentBashConfirm == nil && chatCh != nil {
		opts.Planner.OrchestratorCfg.OnAgentBashConfirm = func(spawnName, cmd, warning string) bool {
			reply := make(chan bool, 1)
			select {
			case chatCh <- views.ChatActivityEvent{
				Kind:      "bash_confirm",
				Summary:   cmd,
				SpawnName: spawnName,
				Warning:   warning,
				Reply:     reply,
				When:      time.Now(),
			}:
			default:
				// Channel full — fail closed (cancel). Better than
				// the agent silently running unconfirmed bash.
				return false
			}
			ok, open := <-reply
			if !open {
				return false
			}
			return ok
		}
	}

	if opts.Planner != nil && opts.Planner.OrchestratorCfg.OnFileDiff == nil && chatCh != nil {
		opts.Planner.OrchestratorCfg.OnFileDiff = func(spawnName, relPath, op, patch string, added, removed int) {
			select {
			case chatCh <- views.ChatActivityEvent{
				Kind:    "diff",
				Path:    relPath,
				Op:      op,
				Diff:    patch,
				Added:   added,
				Removed: removed,
				When:    time.Now(),
			}:
			default:
			}
		}
	}

	m := initialModel(opts, syncCh, chatCh, hostProcCh, watcherErr)
	// Alt-screen + internal viewport: the terminal's native
	// scrollback is unreliable for inline (non-alt-screen)
	// rendering — resizing the window mid-session leaves
	// "ghost" copies of the live region behind because the
	// renderer's cursor-up math can't account for lines that
	// wrap differently at the new width. Alt-screen gives us
	// a fixed canvas that repaints atomically. Committed
	// turns now live in an in-program viewport (REPL.history)
	// instead of native scrollback, scrollable via PgUp/PgDn
	// or the mouse wheel. Tradeoff: native click-and-drag
	// copy stops working under mouse capture; users can hold
	// Option/Alt while dragging to bypass the capture in most
	// terminals.
	// Redirect the standard logger off stderr for the lifetime of the
	// alt-screen TUI. Bubble Tea owns the terminal; any stray
	// log.Printf (e.g. the orchestrator's [ci-plan-preview] diagnostic,
	// which dumps the affected-test slice) would otherwise scribble
	// onto the live canvas and land as garbage in the scrollback — the
	// exact corruption reported 2026-05-30. Route it to a file so the
	// diagnostics are still captured, just not on screen; restore on
	// exit so post-TUI CLI logging behaves normally.
	if opts.KaiDir != "" {
		if lf, lerr := os.OpenFile(filepath.Join(opts.KaiDir, "tui-debug.log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); lerr == nil {
			prevLogOut := log.Writer()
			log.SetOutput(lf)
			defer func() { log.SetOutput(prevLogOut); _ = lf.Close() }()
		}
	}

	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	// Terminal-restore safety net. Bubble Tea normally restores the
	// terminal on clean exit (alt-screen, raw mode, mouse tracking
	// all reverted), but a goroutine panic, runtime crash, or the
	// program returning before Run()'s deferred cleanup runs all
	// leave the user staring at a totally wedged terminal — they
	// have to type `reset` blind. 2026-05-14 dogfood: a
	// SQLITE_BUSY-killed agent run exited the TUI with the alt-
	// screen and mouse tracking still active. This deferred reset
	// emits the standard ANSI sequences directly so even a violent
	// crash leaves a usable terminal. Cheap when nothing went
	// wrong — the sequences are just no-ops if the modes are
	// already off.
	defer restoreTerminalForSafety()
	final, err := p.Run()
	// Print the resume command at exit when a session id was active
	// in the run. Lands AFTER the alt-screen restore so it's visible
	// in the user's normal terminal — printing inside the TUI loop
	// would get wiped by the alt-screen tear-down.
	if final != nil {
		if app, ok := final.(model); ok {
			if id := strings.TrimSpace(app.repl.SessionID()); id != "" {
				fmt.Fprintln(os.Stderr)
				fmt.Fprintln(os.Stderr, "Session saved. Resume with:")
				fmt.Fprintf(os.Stderr, "  kai code --session %s\n", id)
			}
		}
	}
	return err
}

// startWatcher boots a watcher rooted at opts.WorkDir and wires its
// OnUpdate callback to the sync channel. Returns the watcher (so the
// caller can Stop it) plus any startup error. A failed startup is
// non-fatal — the sync pane shows "watcher unavailable" and the rest
// of the TUI works normally.
//
// The OnUpdate callback fires for every file mutation in the watched
// tree. Two things happen on each fire:
//
//  1. SyncEvent to the UI channel — drives the sync pane.
//  2. FTS5 incremental refresh — re-index the changed file so
//     kai_search reflects the working tree without waiting for a
//     full backfill. Without this, the index goes stale the moment
//     the user (or the agent) saves a file. Best-effort: any read
//     or DB error is swallowed; the search index just lags one edit
//     until the next mutation.
func startWatcher(opts Options, ch chan<- views.SyncEvent) (*watcher.Watcher, error) {
	w, err := watcher.New(opts.WorkDir, opts.DB)
	if err != nil {
		return nil, err
	}
	w.OnUpdate = func(path, op string) {
		select {
		case ch <- views.SyncEvent{Path: path, Op: op, When: time.Now()}:
		default:
		}
		refreshSearchIndex(opts, path, op)
	}
	if err := w.Start(); err != nil {
		return nil, err
	}
	return w, nil
}

// refreshSearchIndex keeps the kai_search FTS5 table in sync with
// the working tree as files change. The watcher gives us workspace-
// relative paths; we resolve to (project, body) and call IndexFile
// or RemoveFile. Scoped to text extensions only so a build dropping
// a binary into the tree doesn't waste cycles.
//
// Best-effort: every failure path silently bails. The user-facing
// path (kai_search) tolerates a stale index — it just means one
// edit's worth of staleness, not a wedge.
func refreshSearchIndex(opts Options, relPath, op string) {
	if opts.DB == nil {
		return
	}
	// Match the same extension allowlist the lazy backfill uses so
	// the live index and the cold-start index agree on what's
	// searchable. Without this, a watcher refresh could insert a
	// row a backfill would have skipped, and the "indexed N files"
	// count would drift.
	ext := strings.ToLower(filepath.Ext(relPath))
	if !textExtensions[ext] {
		return
	}
	// Project resolution: prefer the multi-root set so a watcher
	// firing on a file in a sibling root indexes under THAT
	// project's basename. Falls back to the workspace's basename
	// for single-root sessions.
	project := filepath.Base(opts.WorkDir)
	absPath := filepath.Join(opts.WorkDir, relPath)
	storePath := filepath.ToSlash(relPath)
	if opts.Projects != nil {
		if proj := opts.Projects.ProjectFor(absPath); proj != nil {
			project = filepath.Base(proj.Path)
			if rel, err := filepath.Rel(proj.Path, absPath); err == nil {
				storePath = filepath.ToSlash(rel)
			}
		}
	}
	if op == "delete" {
		_ = opts.DB.RemoveFile(project, storePath)
		return
	}
	body, err := os.ReadFile(absPath)
	if err != nil {
		return
	}
	if len(body) > 1<<20 { // 1 MiB cap — mirrors the backfill
		return
	}
	_ = opts.DB.IndexFile(project, storePath, string(body))
}

// textExtensions duplicates the inclusion list used by the kai_search
// lazy backfill (fsTextExtensions in internal/agent/tools/kai_search.go).
// We don't share the symbol to avoid a tui→tools import that would
// reverse the existing layering — the list rarely changes and a
// divergence between the two would be a visible UX bug, not a silent
// correctness one.
var textExtensions = map[string]bool{
	".go": true, ".py": true, ".rs": true, ".ts": true, ".tsx": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".java": true, ".kt": true, ".swift": true, ".c": true, ".cc": true,
	".cpp": true, ".cxx": true, ".h": true, ".hpp": true,
	".rb": true, ".php": true, ".cs": true, ".scala": true, ".ex": true,
	".exs": true, ".erl": true, ".clj": true, ".hs": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true,
	".yaml": true, ".yml": true, ".toml": true, ".json": true,
	".md": true, ".mdx": true, ".rst": true, ".txt": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".sass": true,
	".sql": true, ".graphql": true, ".proto": true,
	".dockerfile": true, ".tf": true, ".tfvars": true,
}

// model is the Bubble Tea root. Owns REPL, gate, and sync sub-views,
// plus a channel for live watcher events. The polished three-pane
// layout lands in task 13.
type model struct {
	opts   Options
	width  int
	height int

	repl    views.REPL
	gate    views.Gate
	sync    views.Sync
	status  views.StatusBar
	syncCh     <-chan views.SyncEvent
	chatCh     <-chan views.ChatActivityEvent
	hostProcCh <-chan views.HostProcEvent
	focused    focus

	// Gate banner / notification bookkeeping. gateLastHeld is the
	// held count from the most recent refresh; we compare against
	// it to detect new mid-session holds (count increases) and
	// fire an inline REPL notification. gateBannerShown gates the
	// one-time launch banner so a multi-refresh startup doesn't
	// print it twice. -1 marks "no refresh yet" so the very first
	// refresh that lands a non-zero count shows the banner without
	// also being treated as a delta-driven mid-session event.
	gateLastHeld    int
	gateBannerShown bool
}

type focus int

const (
	focusREPL focus = iota
	focusGate
	focusSync
)

func initialModel(opts Options, syncCh <-chan views.SyncEvent, chatCh <-chan views.ChatActivityEvent, hostProcCh <-chan views.HostProcEvent, watcherErr error) model {
	s := views.NewSync(200)
	var status views.StatusBar
	if watcherErr != nil {
		s, _ = s.Update(views.SyncErrorMsg{Err: watcherErr})
		status = status.Update(views.SyncErrorMsg{Err: watcherErr})
	}
	gate := views.NewGate(opts.DB)
	// Wire the multi-root project set so the gate refresh
	// aggregates held counts across every project, not just
	// the primary. Without this the status bar's
	// "Gate: N held" undercount silently misses items in
	// non-primary roots in a multi-root workspace.
	gate.SetProjects(opts.Projects)
	return model{
		opts:    opts,
		repl:    views.NewREPLWithSession(opts.Binary, opts.WorkDir, opts.Planner, opts.ResumeSessionID),
		gate:    gate,
		sync:    s,
		status:  status,
		syncCh:       syncCh,
		chatCh:       chatCh,
		hostProcCh:   hostProcCh,
		focused:      focusREPL,
		gateLastHeld: -1,
	}
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.repl.Focus(),
		m.gate.Refresh(),
		// Banner lands once in scrollback, before the live region.
		// Completed turns will accumulate above it as the session
		// grows.
		m.repl.Banner(),
	}
	if m.syncCh != nil {
		cmds = append(cmds, views.PumpEvents(m.syncCh))
	}
	if m.chatCh != nil {
		cmds = append(cmds, views.PumpChatActivity(m.chatCh))
	}
	if m.hostProcCh != nil {
		cmds = append(cmds, views.PumpHostProcEvents(m.hostProcCh))
	}
	// Fixxy event drainer: only when the secret --fixxy-upper
	// flag was passed (Fixxy is nil otherwise). Pump self-
	// re-arms after each delivery so events keep flowing for
	// the life of the session.
	if m.repl.FixxyEvents() != nil {
		cmds = append(cmds, views.PumpFixxy(m.repl.FixxyEvents()))
	}
	return tea.Batch(cmds...)
}

// Update is the Bubble Tea entry point. We wrap the entire dispatch
// in a recover so a panic anywhere downstream (parser bugs, nil
// derefs in event handlers, third-party code) doesn't tear the TUI
// down. The model state pre-panic is returned unchanged, plus a
// short transient error line in the REPL so the user sees that
// SOMETHING went wrong without seeing the stack.
//
// Stack traces are written to ~/.kai/tui-panic.log so a developer
// can post-mortem without disturbing the user's session.
func (m model) Update(msg tea.Msg) (resultModel tea.Model, resultCmd tea.Cmd) {
	defer func() {
		if r := recover(); r != nil {
			logTUIPanic(m, msg, r)
			// Surface a brief, non-alarming line in the REPL.
			// The model returned is the one captured pre-panic so
			// state stays consistent; only the error display is
			// added.
			m.repl = m.repl.AppendSystemError(fmt.Sprintf(
				"internal error suppressed (see ~/.kai/tui-panic.log) — continuing"))
			resultModel = m
			resultCmd = nil
		}
	}()
	return m.updateImpl(msg)
}

// updateImpl is the original Update body; renamed so the public
// Update can wrap it with recover.
func (m model) updateImpl(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		// Do NOT emit tea.ClearScreen here. In inline
		// (no-alt-screen) mode, ClearScreen on every resize
		// event causes Bubble Tea's renderer to lose track
		// of its prior frame size, and the next repaint
		// stacks a fresh copy of the live region into
		// scrollback. Resizing the window then leaves a
		// trail of duplicate input/status bars (see image
		// from 2026-05-05). Bubble Tea's own renderer
		// handles inline reflow on resize without help.
		return m, nil

	case tea.FocusMsg:
		// Focus regain (e.g. waking from sleep, alt-tabbing
		// back) is the case where the terminal may have
		// pushed stale live-region copies into scrollback.
		// Clearing here gives one clean canvas exactly when
		// it's wanted, without firing on every resize tick.
		return m, tea.ClearScreen

	case tea.MouseMsg:
		// Mouse events (wheel up/down) drive the history
		// viewport. Routed before any view-specific
		// dispatch so wheel-scrolling works regardless of
		// which pane has keyboard focus.
		var c tea.Cmd
		m.repl, c = m.repl.HandleScroll(msg)
		return m, c

	case tea.KeyMsg:
		// History scroll keys (PgUp/PgDn) bypass the input
		// textarea — without this they'd insert nothing
		// (textarea ignores them) and the user would have
		// no way to scroll the in-program history. Routed
		// to the viewport via REPL.HandleScroll.
		switch msg.String() {
		case "pgup", "pgdown", "shift+up", "shift+down", "shift+pgup", "shift+pgdown":
			var c tea.Cmd
			m.repl, c = m.repl.HandleScroll(msg)
			return m, c
		}
		if msg.Type == tea.KeyCtrlC {
			// Run/queue cancellation takes priority over the
			// exit gesture: while a run is in flight (or items
			// are queued behind one), Ctrl+C cancels the active
			// run, and a quick second press also drops the
			// queue. Falls through to the existing draft-clear
			// / quit path only when nothing is busy AND nothing
			// is queued.
			if m.repl.HandleCtrlC() {
				return m, nil
			}
			// Two-step exit, like Claude Code / readline: first
			// Ctrl+C clears the input draft so a half-typed prompt
			// doesn't disappear forever to a misfire; the second
			// (with input already empty) actually quits.
			if strings.TrimSpace(m.repl.InputValue()) != "" {
				m.repl.ClearInput()
				return m, nil
			}
			// Kill any managed dev-server process before quitting
			// so we don't orphan it. Same teardown as /stop and
			// /exit.
			if m.opts.Planner != nil {
				views.StopManagedProcess(m.opts.Planner)
			}
			return m, tea.Quit
		}
		switch msg.String() {
		case "ctrl+g":
			m.setFocus(focusGate)
			return m, nil
		case "ctrl+s":
			m.setFocus(focusSync)
			return m, nil
		case "ctrl+r":
			m.setFocus(focusREPL)
			return m, m.repl.Focus()
		case "esc":
			// Esc anywhere returns to REPL — keeps the keyboard
			// shortcut consistent regardless of which pane is active.
			if m.focused != focusREPL {
				m.setFocus(focusREPL)
				return m, m.repl.Focus()
			}
		}

	case views.SyncEventMsg:
		// Re-arm the pump immediately so the next event flows in.
		var cmds []tea.Cmd
		if m.syncCh != nil {
			cmds = append(cmds, views.PumpEvents(m.syncCh))
		}
		var c tea.Cmd
		m.sync, c = m.sync.Update(msg)
		cmds = append(cmds, c)
		// Status bar mirrors the most recent sync activity.
		m.status = m.status.Update(msg)
		// REPL also gets it so the spinner's stuck-detector can
		// reset its idle clock — during execute mode the only
		// "model is alive" signals are file-write activity from
		// spawned agents (assistant text, tool calls, file changes
		// all flow through OnActivity → syncCh, not chatCh).
		// Without this forwarding the REPL false-positives "no
		// response from model" while agents are clearly working.
		m.repl, c = m.repl.Update(msg)
		cmds = append(cmds, c)
		return m, tea.Batch(cmds...)

	case views.ChatActivityMsg:
		// Status bar snapshots agent_start/agent_end here so the
		// "Agents: N" counter updates the moment a run kicks off,
		// not after the bar's next refresh.
		m.status = m.status.Update(msg)
		// Re-arm the chat-activity pump and let the REPL append the
		// inline event line.
		var cmds []tea.Cmd
		if m.chatCh != nil {
			cmds = append(cmds, views.PumpChatActivity(m.chatCh))
		}
		var c tea.Cmd
		m.repl, c = m.repl.Update(msg)
		cmds = append(cmds, c)
		return m, tea.Batch(cmds...)

	case views.HostProcEventMsg:
		// Managed-process lifecycle event from host_proc.go's
		// scanner. Re-arm the pump first so we never miss a
		// subsequent event (process exit happens after the
		// scanner has already cancelled, but a final exit event
		// is fired from the waiter goroutine).
		var cmds []tea.Cmd
		if m.hostProcCh != nil {
			cmds = append(cmds, views.PumpHostProcEvents(m.hostProcCh))
		}
		var c tea.Cmd
		m.repl, c = m.repl.Update(msg)
		cmds = append(cmds, c)
		return m, tea.Batch(cmds...)
	}

	// Async messages (CmdResultMsg, gateActionMsg, gateRefreshedMsg,
	// SyncErrorMsg) route to every view; key input gets filtered
	// inside each view by the focused flag.
	var cmds []tea.Cmd
	var c tea.Cmd
	m.repl, c = m.repl.Update(msg)
	cmds = append(cmds, c)
	m.gate, c = m.gate.Update(msg)
	cmds = append(cmds, c)
	m.sync, c = m.sync.Update(msg)
	cmds = append(cmds, c)
	// Status bar snapshots gate + sync state from the same broadcast.
	m.status = m.status.Update(msg)
	// Re-fire gate refresh whenever an orchestrator run
	// finishes — the integration may have produced a held
	// snapshot, and without this the status bar's "Gate: N
	// held" would stay stale until the next TUI launch.
	// Same logic for CmdResultMsg from gate-mutating shellouts
	// (kai gate approve / reject / capture-with-integrate).
	switch msg.(type) {
	case views.ExecuteDoneMsg, views.CmdResultMsg,
		views.GateReviewActionMsg, views.GateReviewFixMsg:
		cmds = append(cmds, m.gate.Refresh())
	}
	// Gate banner / mid-session notification. Evaluate ONLY on a
	// GateRefreshedMsg — that is the one moment m.gate.HeldCount() is
	// guaranteed fresh (m.gate.Update just set g.items from this very
	// message). Running it on every Update read a stale count: right
	// after a /gate-review approve, the count is still the pre-review
	// value because the refresh that approve triggered is async and
	// hasn't landed yet — which fired a "N held" banner for items the
	// user had just cleared (the banner vs `/gate list` mismatch).
	if _, isRefresh := msg.(views.GateRefreshedMsg); isRefresh {
		now := m.gate.HeldCount()
		if m.gateLastHeld < 0 {
			// First refresh of the session.
			if now > 0 && !m.gateBannerShown {
				m.repl = m.repl.AppendGateBanner(now)
				m.gateBannerShown = true
			}
		} else if now > m.gateLastHeld {
			// Self-review-at-end-of-turn: hand control to the in-TUI
			// review walkthrough automatically rather than asking the
			// user to type /gate review. The trigger no-ops if the REPL
			// is still busy (queued run, ongoing planning/exec, already
			// in review) — fall back to the legacy nudge so the user
			// still knows something is held.
			var autoReviewCmd tea.Cmd
			m.repl, autoReviewCmd = m.repl.TriggerAutoGateReview()
			if autoReviewCmd != nil {
				cmds = append(cmds, autoReviewCmd)
			} else {
				m.repl = m.repl.AppendGateHoldNotice(now - m.gateLastHeld)
			}
		}
		m.gateLastHeld = now
	}
	// Refresh the secret fixxy-upper indicator on every
	// Update tick. Cheap (a single mutex read in the worker)
	// and ensures the elapsed counter ticks visually even
	// when claude is silent. No-op when worker is nil
	// (--fixxy-upper wasn't passed).
	if w := m.repl.Fixxy(); w != nil {
		m.status.SetFixxyStatus(w.Status())
	}
	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	// Three-pane vertical layout under alt-screen:
	//   1. History viewport — committed turns, scrollable.
	//      Takes whatever vertical space the live region and
	//      status bar leave behind.
	//   2. REPL live region — input box, transient line,
	//      streaming preview, plan menu, suggestions popup.
	//      Always pinned just above the status bar so the
	//      user's input doesn't move.
	//   3. Status bar — single-row gate/sync summary at the
	//      very bottom.
	return lipgloss.JoinVertical(lipgloss.Left,
		m.repl.HistoryView(),
		m.repl.View(),
		m.status.View(),
	)
}

// layout recomputes child sizes from the latest window dimensions.
// REPL gets the full width and all height minus one row reserved
// for the status bar at the bottom. Gate + Sync still receive
// SetSize so the /gate and /sync detail commands render correctly
// when the user shells out to them.
func (m *model) layout() {
	statusHeight := 1
	replHeight := m.height - statusHeight
	if replHeight < 4 {
		replHeight = 4
	}
	m.repl.SetSize(m.width, replHeight)
	m.status.SetSize(m.width, statusHeight)
	// Gate/Sync are background sinks now — give them sensible
	// defaults so any list/view their /command paths show is
	// shaped to the window.
	m.gate.SetSize(m.width, replHeight)
	m.sync.SetSize(m.width, replHeight)
}

// setFocus moves input focus between sub-views and updates each
// view's visual state.
func (m *model) setFocus(f focus) {
	m.focused = f
	m.gate.Blur()
	m.sync.Blur()
	m.repl.Blur()
	switch f {
	case focusREPL:
		// REPL focus is reasserted via the returned tea.Cmd by the caller.
	case focusGate:
		m.gate.Focus()
	case focusSync:
		m.sync.Focus()
	}
}


// logTUIPanic appends a stack trace to ~/.kai/tui-panic.log so a
// developer can post-mortem the panic that just got swallowed by
// the recover in Update. Best-effort: failing to open the log
// must not itself panic. Falls back to UserHomeDir when the
// primary kai dir is missing (e.g. running outside a project).
func logTUIPanic(m model, msg tea.Msg, panicVal any) {
	dir := ""
	if m.opts.Projects != nil && m.opts.Projects.Primary() != nil {
		dir = m.opts.Projects.Primary().KaiDir
	}
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".kai")
		}
	}
	if dir == "" {
		return // nowhere to write
	}
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "tui-panic.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n========== PANIC %s ==========\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "msg type: %T\n", msg)
	fmt.Fprintf(f, "panic:    %v\n\n", panicVal)
	_, _ = f.Write(debug.Stack())
}


// firstNonEmptyLine returns the first non-empty trimmed line of s,
// restoreTerminalForSafety emits the ANSI sequences that revert the
// modes Bubble Tea sets (alt-screen, mouse tracking, bracketed paste,
// cursor visibility). Called via defer at the TUI's outermost level
// so even a panic or runtime crash that bypasses Bubble Tea's own
// cleanup still leaves the terminal usable instead of wedged.
//
// Idempotent — running it on a healthy exit just re-emits the
// already-applied reverts. The cost is a handful of escape bytes
// to stderr.
//
// Sequences:
//   1049l  exit alternate screen buffer (return to the user's
//          normal scrollback)
//   25h    show cursor (Bubble Tea hides it during the run)
//   1000l  disable basic mouse tracking
//   1002l  disable cell-motion mouse tracking (what
//          WithMouseCellMotion turned on)
//   1003l  disable any-event mouse tracking (defensive)
//   2004l  disable bracketed-paste mode
//   ?7h    re-enable line wrap (the default; some TUIs disable it)
func restoreTerminalForSafety() {
	const reset = "\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?2004l\x1b[?7h"
	fmt.Fprint(os.Stderr, reset)
}

// truncated to ~120 chars so a long paragraph collapses to a one-line
// status summary. Used to summarize per-turn agent prose for the
// inline conversation feed — full text would overwhelm the scroll.
func firstNonEmptyLine(s string) string {
	// 120 was too tight — the 2026-05-26 dogfood showed executor
	// agent summaries getting clipped mid-sentence ("…The project
	// has…") with the user unable to read what was actually
	// reported. Critic-style summaries and post-execution
	// narratives commonly run 200-350 chars; 400 fits those
	// without truncating, with the cap kept as a safety net for
	// pathologically long lines (raw stack traces emitted as
	// assistant text, etc.).
	const maxLen = 400
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			if len(t) > maxLen {
				t = t[:maxLen-1] + "…"
			}
			return t
		}
	}
	return ""
}
