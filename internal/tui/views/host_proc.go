// host_proc: long-running managed process model for kai-spawned
// commands. Replaces the 35s capture-then-detach approach (v0.31.47)
// for dev-server-shape commands (npm run dev, vite, webpack, etc.)
// with a real ownership model: kai spawns it, owns it, watches it
// for the process's lifetime — or until /stop, or until kai exits.
//
// Why: vite/webpack errors can fire 10s, 5min, or 30min after launch
// (file save → re-compile → new error). A fixed capture window
// guesses badly: either too short (miss late errors) or too long
// (hold the prompt). The right model is sustained watching.
//
// Design constraints for v1:
//   - Single slot. PlannerServices holds one *ManagedProcess. A new
//     dev-server command kills + replaces the prior. Avoids a
//     multi-process state machine.
//   - Output → bounded ring buffer (managedRingBytes, default 64KB)
//     + temp logfile for full history.
//   - Background scanner goroutine polls the ring every 2s, runs
//     detectHostCommandError on new content, emits events via
//     HostProcEventCh when a new error class appears.
//   - Cleanup on TUI exit. The REPL's quit handler calls
//     StopManagedProcess so the user doesn't leak processes.
//   - Dev-server-shape detection reuses hostCommandWindowFor's
//     token list — same shape that triggered the extended window
//     now triggers managed-process mode instead.
package views

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// managedRingBytes caps the in-memory output buffer. 64KB holds
// roughly the last 1500 lines of typical compiler output — plenty
// for the error scanner; full history goes to disk.
const managedRingBytes = 64 * 1024

// managedScanInterval is how often the background scanner polls
// the ring buffer for new error content. 2s balances responsiveness
// (most users notice an error within 5s) against scanner overhead
// (small string scan ~100µs).
const managedScanInterval = 2 * time.Second

// HostProcEvent is the channel payload the background watcher sends
// back to the TUI when something noteworthy happens to a managed
// process. Mirrors the ChatActivityEvent pattern.
type HostProcEvent struct {
	// Kind: "started" | "error_detected" | "exited" | "output"
	Kind string
	// Command is the user-facing command string.
	Command string
	// ErrorLine, when Kind == "error_detected", carries the
	// first new error-shaped line the scanner found. Capped at
	// 240 chars (same as the v0.31.45 detector).
	ErrorLine string
	// OutputLines, when Kind == "output", carries 1+ new stdout/
	// stderr lines from the managed process. Lines are batched so
	// the TUI doesn't get N separate events for a single burst.
	// Throttled to ~1 batch per scan tick (every 2s).
	OutputLines []string
	// ExitCode, when Kind == "exited", carries the process exit
	// status. -1 means we killed it.
	ExitCode int
	// When carries the event's clock time so the TUI can render
	// "5s ago" / "2m ago" alongside the line.
	When time.Time
}

// ManagedProcess is a kai-owned long-running command. The TUI reads
// its state through accessor methods (mutex-guarded); the background
// watcher writes events through HostProcEventCh on the parent
// PlannerServices.
type ManagedProcess struct {
	Command   string
	Pid       int
	StartedAt time.Time
	LogPath   string

	mu          sync.Mutex
	cmd         *exec.Cmd
	output      *ringBuf
	errorRing   []string // recent unique error-sig "seen" set; dedupes repeated detections
	stopped     bool
	exited      bool
	exitCode    int
	cancelWatch context.CancelFunc
}

// ringBuf is a bounded-byte ring buffer. Writes that overflow drop
// the oldest bytes. Snapshot() returns the current content as a
// string in chronological order. Thread-safe via mu.
type ringBuf struct {
	mu   sync.Mutex
	data []byte
	cap  int
}

func newRingBuf(cap int) *ringBuf {
	return &ringBuf{cap: cap}
}

func (r *ringBuf) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if over := len(r.data) - r.cap; over > 0 {
		r.data = r.data[over:]
	}
	return len(p), nil
}

func (r *ringBuf) Snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.data)
}

func (r *ringBuf) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.data)
}

// StartManagedProcess spawns cmd as a long-running managed process.
// Kills + replaces any prior managed process on PlannerServices.
// Returns immediately after spawn; the background watcher emits
// events via s.HostProcEventCh for the process's lifetime.
//
// On Unix systems we set Setpgid so concurrently-spawned children
// (e.g. `concurrently "vite" "electron ."`) live under the same
// process group, and StopManagedProcess can kill the whole group
// instead of orphaning the children.
func StartManagedProcess(s *PlannerServices, command string) (*ManagedProcess, error) {
	if s == nil {
		return nil, fmt.Errorf("StartManagedProcess: nil services")
	}
	// Replace any prior managed process. Best-effort stop; if the
	// prior is already dead this is a no-op.
	StopManagedProcess(s)

	logFile, err := os.CreateTemp("", "kai-host-proc-*.log")
	if err != nil {
		return nil, fmt.Errorf("create logfile: %w", err)
	}

	c := exec.Command("bash", "-c", command)
	// Run in the active project, NOT kit's process cwd. Without this the
	// managed process inherited wherever kit was launched from — so "run it"
	// in ~/projects/loom could execute against a different project entirely
	// (e.g. kit's own kai-desktop dir). Mirror the bash tool, which pins
	// c.Dir to the workspace; workspaceFor() resolves InvokedFrom→MainRepo→cwd.
	if dir := workspaceFor(s); dir != "" && dir != "(unknown)" {
		c.Dir = dir
	}
	// Put the child in its own process group so SIGTERM to the
	// group reaches every child (concurrently's vite + electron
	// case). Unix only — on Windows this is a no-op via
	// SysProcAttr's zero value.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	ring := newRingBuf(managedRingBytes)
	c.Stdout = io.MultiWriter(ring, logFile)
	c.Stderr = io.MultiWriter(ring, logFile)

	if err := c.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mp := &ManagedProcess{
		Command:     command,
		Pid:         c.Process.Pid,
		StartedAt:   time.Now(),
		LogPath:     logFile.Name(),
		cmd:         c,
		output:      ring,
		cancelWatch: cancel,
	}

	// Set on services before launching watcher goroutines so any
	// race with /stop or TUI exit sees the new process consistently.
	s.SetManagedProc(mp)

	// Emit "started" event so the TUI can render a banner.
	emitHostProcEvent(s, HostProcEvent{
		Kind:    "started",
		Command: command,
		When:    time.Now(),
	})

	// Waiter goroutine: cleans up file handle when process exits,
	// emits "exited" event with status.
	go func() {
		err := c.Wait()
		_ = logFile.Close()
		exitCode := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
			}
		}
		mp.mu.Lock()
		mp.exited = true
		mp.exitCode = exitCode
		mp.mu.Unlock()
		cancel() // stop the scanner
		emitHostProcEvent(s, HostProcEvent{
			Kind:     "exited",
			Command:  command,
			ExitCode: exitCode,
			When:     time.Now(),
		})
	}()

	// Scanner goroutine: polls the ring buffer for new content,
	// runs detectHostCommandError on the delta, emits an event
	// when a new error class appears. Bounded by the errorRing
	// dedupe set so a sustained error doesn't fire 30 events.
	go runManagedScanner(ctx, s, mp)

	return mp, nil
}

// runManagedScanner polls mp.output every managedScanInterval and
// (a) emits "output" events for any new lines so the TUI can stream
// them dimly into scrollback, (b) runs detectHostCommandError on
// the latest content and emits "error_detected" when a new error
// class appears.
//
// New error-classes (signatures not in the dedupe ring) fire
// "error_detected" events; repeats are dropped.
//
// The scanner exits when ctx is cancelled (process exited OR /stop
// killed it OR TUI is shutting down).
func runManagedScanner(ctx context.Context, s *PlannerServices, mp *ManagedProcess) {
	tick := time.NewTicker(managedScanInterval)
	defer tick.Stop()
	// lastScanned tracks the byte index in the most recent
	// Snapshot we've already emitted. Note: the ring buffer may
	// shift content (oldest bytes evicted) between ticks, so this
	// is a best-effort delta — when the snapshot length shrinks
	// relative to lastScanned we reset to "everything from here
	// looks new" and emit the full snapshot tail (capped to
	// managedOutputBatchMax lines).
	lastSnapshot := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			content := mp.output.Snapshot()
			if content == "" {
				continue
			}
			// Compute the new portion since last tick. If the
			// previous snapshot is a prefix of the current, the
			// new portion is the suffix. Otherwise the ring
			// shifted; treat the whole tail as new (capped below).
			newPortion := content
			if lastSnapshot != "" && strings.HasPrefix(content, lastSnapshot) {
				newPortion = content[len(lastSnapshot):]
			} else if lastSnapshot != "" {
				// Ring shifted. Best-effort: find the LAST
				// occurrence of any line from lastSnapshot in
				// content, and treat everything after as new.
				// Simpler: take the last managedOutputBatchMax
				// lines of content as the "new" portion.
				newPortion = "" // signal "use tail below"
			}
			lastSnapshot = content

			// Emit "output" event if there's new content to show.
			emitOutputLines(s, mp.Command, newPortion, content)

			// Error detection over the FULL content (not just the
			// delta) so we catch errors that span lines or whose
			// shape we've seen before-but-in-different-form.
			line := detectHostCommandError(content, nil, true)
			if line == "" {
				continue
			}
			sig := normalizeHostProcSig(line)
			mp.mu.Lock()
			if managedRingContains(mp.errorRing, sig) {
				mp.mu.Unlock()
				continue
			}
			mp.errorRing = managedRingPush(mp.errorRing, sig, managedErrorRingSize)
			mp.mu.Unlock()
			emitHostProcEvent(s, HostProcEvent{
				Kind:      "error_detected",
				Command:   mp.Command,
				ErrorLine: line,
				When:      time.Now(),
			})
		}
	}
}

// managedOutputBatchMax bounds the number of lines per "output"
// event so a chatty webpack run can't flood the TUI scrollback.
// 8 lines per 2-second tick = ~4 lines/sec sustained, which
// matches a human's read speed. Bursts past this get truncated
// with a "[... N lines elided ...]" marker.
const managedOutputBatchMax = 8

// emitOutputLines splits newPortion (or the tail of content when
// newPortion is empty) into lines and emits an "output" event
// capped at managedOutputBatchMax. Empty lines are dropped.
func emitOutputLines(s *PlannerServices, command, newPortion, content string) {
	source := newPortion
	if source == "" {
		// Ring shifted — fall back to the tail of the full
		// snapshot, capped.
		source = content
	}
	lines := strings.Split(strings.TrimRight(source, "\n"), "\n")
	// Filter empties + cap to the most recent N.
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) == 0 {
		return
	}
	elided := 0
	if len(kept) > managedOutputBatchMax {
		elided = len(kept) - managedOutputBatchMax
		kept = kept[len(kept)-managedOutputBatchMax:]
	}
	if elided > 0 {
		kept = append([]string{fmt.Sprintf("[… %d earlier lines elided …]", elided)}, kept...)
	}
	emitHostProcEvent(s, HostProcEvent{
		Kind:        "output",
		Command:     command,
		OutputLines: kept,
		When:        time.Now(),
	})
}

// StopManagedProcess kills the current managed process and clears
// the slot. No-op when the slot is empty. Best-effort: SIGTERM with
// 2-second grace, then SIGKILL. Group-kill via the negative pid
// so concurrently's children die alongside the parent.
func StopManagedProcess(s *PlannerServices) {
	if s == nil {
		return
	}
	mp := s.SwapManagedProc(nil)
	if mp == nil {
		return
	}
	mp.mu.Lock()
	if mp.exited || mp.stopped {
		mp.mu.Unlock()
		return
	}
	mp.stopped = true
	cmd := mp.cmd
	mp.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative pid = process group. Kills children too. Best-effort
	// on systems where the syscall fails (e.g. macOS missing
	// privileges); fall back to direct kill of the parent pid.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	// 2-second grace for clean shutdown.
	graceful := make(chan struct{})
	go func() {
		// The waiter goroutine in StartManagedProcess calls
		// cmd.Wait — we can't race for it here. Poll mp.exited
		// instead.
		for i := 0; i < 20; i++ {
			mp.mu.Lock()
			exited := mp.exited
			mp.mu.Unlock()
			if exited {
				close(graceful)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		close(graceful)
	}()
	select {
	case <-graceful:
	case <-time.After(2 * time.Second):
	}
	// If still alive, SIGKILL the group.
	mp.mu.Lock()
	stillAlive := !mp.exited
	mp.mu.Unlock()
	if stillAlive {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			_ = cmd.Process.Kill()
		}
	}
	mp.cancelWatch()
}

// IsDevServerCommand reports whether cmd matches the dev-server
// shape that should be managed instead of capture-and-detach.
// Mirrors hostCommandWindowFor's token list — single source of
// truth would be cleaner but the runtime cost of the duplicate
// check is zero and the failure-mode differs (window-vs-managed),
// so keeping them parallel for now.
func IsDevServerCommand(cmd string) bool {
	return hostCommandWindowFor(cmd) == hostCommandDevServerWindow
}

// emitHostProcEvent sends ev to the HostProcEventCh, dropping on
// floor when the channel is nil or full. Same non-blocking pattern
// as the ChatActivityCh emitter: a slow TUI mustn't block the
// scanner.
func emitHostProcEvent(s *PlannerServices, ev HostProcEvent) {
	if s == nil || s.HostProcEventCh == nil {
		return
	}
	select {
	case s.HostProcEventCh <- ev:
	default:
	}
}

// managedErrorRingSize caps the dedupe set so a sustained error
// (vite reporting the same problem every save) doesn't fire 30
// events. 8 is plenty — most dogfood runs see 1-3 distinct error
// classes; anything beyond is the model thrashing, not new info.
const managedErrorRingSize = 8

func managedRingContains(ring []string, s string) bool {
	for _, x := range ring {
		if x == s {
			return true
		}
	}
	return false
}

func managedRingPush(ring []string, s string, cap int) []string {
	if managedRingContains(ring, s) {
		return ring
	}
	if len(ring) >= cap {
		copy(ring, ring[1:])
		ring = ring[:len(ring)-1]
	}
	return append(ring, s)
}

// normalizeHostProcSig collapses the detected error line to a stable
// signature for dedupe. Same idea as normalizeBashErrSig from the
// agent package — keep file:line:col, strip absolute paths to
// basenames, cap length. We don't import that helper to avoid a
// cross-package dep just for one function.
func normalizeHostProcSig(line string) string {
	// Strip absolute paths down to basename so the dogfood
	// path /Users/jacobschatz/... doesn't differ from a CI path.
	out := line
	for _, prefix := range []string{"/Users/", "/private/tmp/", "/tmp/", "/var/", "/home/"} {
		for i := strings.Index(out, prefix); i >= 0; i = strings.Index(out, prefix) {
			end := i + len(prefix)
			for end < len(out) && !isSigBreak(out[end]) {
				end++
			}
			base := filepath.Base(out[i:end])
			out = out[:i] + base + out[end:]
		}
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func isSigBreak(b byte) bool {
	switch b {
	case ' ', '\t', '\n', ',', ';', ')', ']':
		return true
	}
	return false
}

// managedProcLogger adapts PlannerServices to the tools.ManagedProcLogger
// interface so the kai_logs tool can read the current managed process's
// output buffer. Returns (command, output, running) — running=false
// when no managed process is active, in which case the tool surfaces
// that to the model.
type managedProcLogger struct {
	services *PlannerServices
}

// NewManagedProcLogger builds a ManagedProcLogger backed by the given
// PlannerServices. Stored as a concrete type so the import surface
// here stays clean; callers pass it to agent.Options through the
// tools.ManagedProcLogger interface.
func NewManagedProcLogger(s *PlannerServices) *managedProcLogger {
	return &managedProcLogger{services: s}
}

// RecentLogs implements tools.ManagedProcLogger by snapshotting the
// current ManagedProcess's ring buffer. Returns running=false when
// no process is in the slot.
func (l *managedProcLogger) RecentLogs() (command string, output string, running bool) {
	if l == nil || l.services == nil {
		return "", "", false
	}
	mp := l.services.ManagedProc()
	if mp == nil {
		return "", "", false
	}
	mp.mu.Lock()
	if mp.exited || mp.stopped {
		mp.mu.Unlock()
		// Still surface the last output — the model may be
		// asking about the just-exited process. Running=false
		// signals "process is gone" so the tool can phrase the
		// response correctly.
		out := ""
		if mp.output != nil {
			out = mp.output.Snapshot()
		}
		return mp.Command, out, false
	}
	mp.mu.Unlock()
	return mp.Command, mp.output.Snapshot(), true
}
