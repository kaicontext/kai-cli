// Package fixxy is the secret "fixxy-upper" mode: when an
// error fires (or, in higher modes, when the user signals
// dissatisfaction or any turn completes), we spawn a headless
// `claude` process pointed at the kai source repo and let it
// diagnose, fix, and rebuild the kai binary.
//
// Activation: `kai code --fixxy-upper[=1|2|3]`. The flag is
// hidden from --help — discoverable only by people who know
// it exists. End users never see it; devs (kai team) use it
// to close the "I built kai, I'm dogfooding kai, kai broke,
// I want kai to fix itself" loop without context-switching.
//
// Modes (escalating intervention):
//
//	1: errors → claude reviews + fixes + rebuilds
//	2: 1 + the magic phrase "no sir i don't like it" triggers
//	   a fix on the recent conversation
//	3: 1 + 2 + every turn end gets reviewed (claude either
//	   fixes or says "looks fine")
//
// Bounded autonomy:
//   - cwd is always KAI_REPO (env, default ~/projects/kai/kai)
//   - claude has its own auth + tools; we just spawn -p
//   - rebuild is a single `go build` we run AFTER claude exits
//   - the running TUI keeps using the OLD binary; the message
//     "restart kai to use the fix" is the user-facing signal
//   - one fixxy in flight at a time; new triggers queue (cap 4)
//     or drop (above 4) to prevent runaway parallelism
package fixxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mode is the activation level. Off means the worker noops
// every Trigger call (the expected state for ~all users).
type Mode int

const (
	Off Mode = 0
	M1  Mode = 1 // errors only
	M2  Mode = 2 // M1 + "no sir" feedback
	M3  Mode = 3 // M2 + every-turn review
)

// Event is a status update streamed back to the REPL while a
// fixxy worker is running. The REPL renders these as dim
// "fixxy: <text>" lines so the user sees what's happening
// without the output dominating their session.
type Event struct {
	Kind string // "start" | "claude_line" | "rebuild_start" | "rebuild_ok" | "rebuild_fail" | "done" | "skipped"
	Text string // human-readable detail
}

// Worker is the singleton that owns subprocess lifecycle.
// REPL has one Worker per session; Trigger calls funnel
// through it serially with a small queue so the user can't
// accidentally spawn 50 claude processes by hitting an
// errory codepath in a loop.
type Worker struct {
	repoPath    string          // KAI_REPO; cwd for claude
	binaryPath  string          // where to write the rebuilt kai binary
	claudeCmd   string          // resolved path to `claude` (or "" if missing)
	events      chan Event      // status updates → REPL
	queue       chan triggerJob // queued fixxy invocations
	mu          sync.Mutex
	inFlight    bool
	disabled    bool   // true after a hard preflight failure
	disabledMsg string // why we're disabled (printed once)

	// Status-bar tracking (read by the TUI status bar via
	// Status() to show a "fixxy: working (Ns)" indicator
	// while a job is running). Without this the user has
	// no persistent "is fixxy doing anything?" signal —
	// only the chronological event log, which scrolls away.
	statusMu     sync.Mutex
	statusKind   string    // current job kind, "" when idle
	statusStart  time.Time // when current job started
}

// triggerJob is one queued fixxy invocation. Mode is included
// so the worker can render slightly different status text per
// mode without the caller having to format anything.
type triggerJob struct {
	kind   string // human label e.g. "error: <kind>" or "feedback" or "review"
	prompt string // full prompt body for claude
	mode   Mode
}

// New constructs a Worker. If `claude` isn't on PATH or
// repoPath doesn't exist, the worker is permanently disabled
// — Trigger calls noop and the disabledMsg surfaces once via
// the events channel on first call. We don't fail noisily
// because the secret flag should fail soft (the rest of the
// TUI keeps working).
//
// repoPath: where the kai source lives (cwd for claude).
// binaryPath: where to install the rebuilt binary (usually
// $(which kai) so the next `kai code` picks up the fix).
func New(repoPath, binaryPath string) *Worker {
	w := &Worker{
		repoPath:   repoPath,
		binaryPath: binaryPath,
		events:     make(chan Event, 64),
		queue:      make(chan triggerJob, 4),
	}
	if path, err := exec.LookPath("claude"); err != nil {
		w.disabled = true
		w.disabledMsg = "fixxy disabled: `claude` not on PATH"
	} else {
		w.claudeCmd = path
	}
	if !w.disabled {
		if info, err := os.Stat(repoPath); err != nil || !info.IsDir() {
			w.disabled = true
			w.disabledMsg = "fixxy disabled: KAI_REPO does not exist or is not a directory: " + repoPath
		}
	}
	go w.run()
	return w
}

// Events returns the read-only event channel the REPL drains
// to render fixxy status. Buffered (64) so a paused renderer
// doesn't block the worker; events drop silently when full
// (status lines are best-effort, not protocol).
func (w *Worker) Events() <-chan Event {
	return w.events
}

// Trigger queues a fixxy invocation. Returns immediately —
// the actual claude spawn happens on the worker goroutine
// serialized through the queue. Drops the call if the queue
// is full (intentional: chatty errors shouldn't multiply).
//
// kind: short label for the status line ("error: missing_blobs",
// "feedback", "post-turn review"). Used in the "fixxy
// triggered by: <kind>" status banner.
//
// prompt: the full body sent to claude as the -p argument.
// Builders live in this package (BuildErrorPrompt, etc.) but
// callers can construct ad-hoc prompts too.
func (w *Worker) Trigger(kind, prompt string, mode Mode) {
	if w == nil || w.disabled || mode == Off {
		w.maybeAnnounceDisabled()
		return
	}
	select {
	case w.queue <- triggerJob{kind: kind, prompt: prompt, mode: mode}:
	default:
		// queue full — drop. The user's getting plenty of
		// fixxy attention already; another won't help.
		select {
		case w.events <- Event{Kind: "skipped",
			Text: "queue full, dropped: " + kind}:
		default:
		}
	}
}

func (w *Worker) maybeAnnounceDisabled() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.disabled || w.disabledMsg == "" {
		return
	}
	select {
	case w.events <- Event{Kind: "skipped", Text: w.disabledMsg}:
		w.disabledMsg = "" // announce once per session
	default:
	}
}

// run is the worker goroutine. Pulls jobs serially, spawns
// claude, drains its output to status events, runs go build
// when claude finishes (regardless of "did anything change"
// — go build is fast and idempotent on a clean tree), then
// reports outcome. One job at a time guarantees we don't
// have racing edits to the source tree.
func (w *Worker) run() {
	for job := range w.queue {
		w.runJob(job)
	}
}

func (w *Worker) runJob(job triggerJob) {
	w.mu.Lock()
	w.inFlight = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight = false
		w.mu.Unlock()
	}()

	w.send(Event{Kind: "start", Text: "fixxy-upper triggered by: " + job.kind})
	// Mark in-flight BEFORE spawning so the status bar
	// indicator lights up immediately. The user's pain
	// without this: "triggered by: feedback" then 30s of
	// dead air with no way to tell if anything's running.
	w.markInFlight(job.kind)
	defer w.clearInFlight()

	// Immediate "spawning" event so the user sees that
	// fixxy moved past the queue stage. The very next line
	// could be 5–15s away (cold claude startup) so this
	// fills the pre-output dead zone.
	jobStart := time.Now()
	w.send(Event{Kind: "claude_line", Text: "spawning claude (first response usually in 5–15s)…"})

	// Spawn claude with -p (print mode, headless). Bound at
	// 10 minutes — a fix that takes longer is likely stuck
	// and a hung subprocess would tie up the worker forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// --dangerously-skip-permissions: the user opted into
	// fixxy via the secret flag, which is itself the
	// consent gate for "let claude edit and run things in
	// the kai repo without per-tool approval." Without
	// this, headless claude blocks on every Edit/Write
	// attempt and reports "Blocked by permissions" — fixxy
	// runs out the clock and rebuilds nothing.
	//
	// claudeCwd: scope claude to the actual kai SOURCE
	// root (parent of kai-cli/, the dir that contains
	// kai-core/, kai-cli/, kai-desktop/, etc.). When
	// KAI_REPO is set to a multi-checkout container (like
	// ~/projects/kai/ which holds kai/, kai-server/, ...),
	// the source root is one level deeper. Without this
	// scoping claude has visibility into sibling repos
	// and may wander into the wrong checkout when the
	// user's question mentions paths that exist in both.
	claudeCwd := w.repoPath
	if src, err := findKaiCli(w.repoPath); err == nil {
		claudeCwd = filepath.Dir(src) // kai-cli/.. = kai source root
	}
	cmd := exec.CommandContext(ctx, w.claudeCmd,
		"--dangerously-skip-permissions",
		"-p", job.prompt)
	cmd.Dir = claudeCwd
	cmd.Env = os.Environ()

	// Live trace: stream claude's stdout AND stderr line-by-
	// line as they arrive instead of blocking on
	// CombinedOutput(). The user sees claude's prose +
	// tool-call indicators populate in real time so they
	// know fixxy is alive even on multi-minute runs.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		w.send(Event{Kind: "done", Text: "stdout pipe: " + err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		w.send(Event{Kind: "done", Text: "stderr pipe: " + err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		w.send(Event{Kind: "done", Text: "claude failed to start: " + err.Error()})
		return
	}

	// Heartbeat: emit "still working…" lines so silent
	// stretches (cold model startup, slow tool calls) don't
	// look like the worker died. Schedule is escalating —
	// frequent at first to confirm spawn, sparser later so
	// a long-running fix doesn't drown the scrollback.
	//
	// 3s → "starting up…"
	// 8s → "claude is thinking… (8s)"
	// 15s → "still working… (15s)"
	// then every 15s for the rest of the run.
	//
	// Each tick suppresses if claude produced output recently
	// (lastEventAt updated by streamPipe goroutines) — no
	// reason to print a heartbeat when there's already live
	// trace happening.
	var lastEventMu sync.Mutex
	lastEventAt := time.Now()
	w.markInFlightStart(jobStart)
	hbDone := make(chan struct{})
	go func() {
		schedule := []time.Duration{3 * time.Second, 8 * time.Second, 15 * time.Second}
		idx := 0
		for {
			var wait time.Duration
			if idx < len(schedule) {
				wait = schedule[idx] - time.Since(jobStart)
				if wait <= 0 {
					wait = 100 * time.Millisecond
				}
				idx++
			} else {
				wait = 15 * time.Second
			}
			select {
			case <-hbDone:
				return
			case <-time.After(wait):
				lastEventMu.Lock()
				silent := time.Since(lastEventAt)
				lastEventMu.Unlock()
				if silent < 3*time.Second {
					// Live output happened recently; skip
					// this heartbeat to avoid trampling the
					// trace stream.
					continue
				}
				elapsed := time.Since(jobStart).Round(time.Second)
				w.send(Event{Kind: "claude_line",
					Text: "still working… (" + elapsed.String() + " elapsed)"})
				w.markInFlightStart(jobStart) // refresh status bar
				lastEventMu.Lock()
				lastEventAt = time.Now()
				lastEventMu.Unlock()
			}
		}
	}()

	// One scanner per pipe; both feed into the same event
	// channel so the REPL renders them in arrival order.
	// stderr lines get a "[stderr]" tag so users can tell
	// when claude is reporting tool errors vs prose.
	var wg sync.WaitGroup
	wg.Add(2)
	go w.streamPipe(stdout, "", &wg, &lastEventMu, &lastEventAt)
	go w.streamPipe(stderr, "[stderr] ", &wg, &lastEventMu, &lastEventAt)
	wg.Wait()
	close(hbDone)

	if waitErr := cmd.Wait(); waitErr != nil {
		w.send(Event{Kind: "done",
			Text: "claude exited non-zero: " + waitErr.Error() + " (no rebuild attempted)"})
		return
	}

	// Rebuild. Source lives in kai-cli/ but the user's
	// repo path may be the immediate kai checkout
	// (~/projects/kai/kai/kai-cli/go.mod) OR the parent
	// container holding multiple kai-* repos
	// (~/projects/kai/, where ~/projects/kai/kai-cli/ is
	// the binary location and the actual source is one
	// level deeper). findKaiCli walks the candidates.
	srcDir, srcErr := findKaiCli(w.repoPath)
	if srcErr != nil {
		w.send(Event{Kind: "rebuild_fail",
			Text: "couldn't locate kai-cli/go.mod under " + w.repoPath +
				" — set KAI_REPO to the kai source root"})
		return
	}
	w.send(Event{Kind: "rebuild_start",
		Text: "go build (from " + srcDir + ") → " + w.binaryPath})
	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", w.binaryPath, "./cmd/kai")
	buildCmd.Dir = srcDir
	if buildOut, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		w.send(Event{Kind: "rebuild_fail",
			Text: "go build failed: " + truncate(string(buildOut), 400)})
		return
	}
	w.send(Event{Kind: "rebuild_ok",
		Text: "rebuilt; restart kai to pick up the fix"})
	w.send(Event{Kind: "done", Text: ""})
}

// streamPipe scans one of claude's output streams line-by-line
// and emits a claude_line event per non-empty line. tag prefixes
// stderr lines so the REPL can distinguish them. Updates the
// shared lastEventAt watermark so the heartbeat goroutine
// knows when claude has gone silent.
//
// Best-effort: scanner errors (read failures, broken pipe at
// process exit) silently end the goroutine. The cmd.Wait() in
// the caller is the source of truth for "did the run succeed".
func (w *Worker) streamPipe(r io.ReadCloser, tag string, wg *sync.WaitGroup, mu *sync.Mutex, lastAt *time.Time) {
	defer wg.Done()
	defer r.Close()
	scanner := bufio.NewScanner(r)
	// Bump the buffer cap so a long claude line (a multi-KB
	// tool result preview, say) doesn't truncate or panic.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.send(Event{Kind: "claude_line", Text: tag + truncate(line, 280)})
		mu.Lock()
		*lastAt = time.Now()
		mu.Unlock()
	}
}

// Status returns a short human-friendly summary of the
// current job for the status bar. Empty string when the
// worker is idle. Updates monotonically — the TUI polls
// (or rebinds on FixxyEventMsg) to refresh.
//
// Format examples:
//   "" (idle)
//   "fixxy: error: preflight.missing_blobs (12s)"
//   "fixxy: feedback (3s)"
func (w *Worker) Status() string {
	if w == nil {
		return ""
	}
	w.statusMu.Lock()
	kind := w.statusKind
	start := w.statusStart
	w.statusMu.Unlock()
	if kind == "" {
		return ""
	}
	return "fixxy: " + kind + " (" + time.Since(start).Round(time.Second).String() + ")"
}

// markInFlight sets the current job for the status bar.
// Called once per job before subprocess spawn so the
// indicator lights up immediately (no race with the model's
// first response).
func (w *Worker) markInFlight(kind string) {
	w.statusMu.Lock()
	w.statusKind = kind
	w.statusStart = time.Now()
	w.statusMu.Unlock()
}

// markInFlightStart updates the start time so the status-bar
// elapsed counter refreshes against a known anchor. Called
// from the heartbeat tick so even silent stretches keep the
// status bar honest.
func (w *Worker) markInFlightStart(t time.Time) {
	w.statusMu.Lock()
	if w.statusStart.IsZero() {
		w.statusStart = t
	}
	w.statusMu.Unlock()
}

// clearInFlight blanks the status when the job ends. Run via
// defer in runJob so it fires regardless of success/error.
func (w *Worker) clearInFlight() {
	w.statusMu.Lock()
	w.statusKind = ""
	w.statusStart = time.Time{}
	w.statusMu.Unlock()
}

func (w *Worker) send(ev Event) {
	select {
	case w.events <- ev:
	default:
	}
}

// findKaiCli locates the kai-cli source directory (the one
// with go.mod + ./cmd/kai) under repoPath. Tries:
//
//  1. <repoPath>/kai-cli/go.mod   — most common: KAI_REPO is
//     the kai source root and kai-cli is its child
//  2. <repoPath>/kai/kai-cli/go.mod — KAI_REPO is the parent
//     container holding multiple kai-* checkouts; the
//     "real" kai source is one level deeper
//  3. <repoPath>/go.mod — KAI_REPO is the kai-cli dir itself
//     (rare but happens when a user points env at the build
//     dir directly)
//
// Returns the directory containing go.mod, or error if none
// match. The error carries enough info that the caller's
// rebuild_fail event tells the user how to set KAI_REPO.
func findKaiCli(repoPath string) (string, error) {
	candidates := []string{
		filepath.Join(repoPath, "kai-cli"),
		filepath.Join(repoPath, "kai", "kai-cli"),
		repoPath,
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "go.mod")); err == nil {
			// Also confirm cmd/kai exists — go.mod alone
			// could be a different module (kai-server etc.)
			if _, err := os.Stat(filepath.Join(c, "cmd", "kai")); err == nil {
				return c, nil
			}
		}
	}
	return "", fmt.Errorf("no kai-cli/go.mod with cmd/kai found under %s", repoPath)
}

// truncate caps a single line so a runaway claude response
// (or a multi-KB error log line) doesn't blow out the
// scrollback. Keeps the head + an ellipsis marker.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
