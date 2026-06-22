package tui

// harness_test.go is a programmatic test harness for the Bubble Tea
// TUI. It drives the real model (Init/Update/View) inside teatest's
// simulated terminal, so a test can send keystrokes and assert on
// rendered frames without anyone launching `kai code` by hand and
// without a real PTY.
//
// Why in-process rather than a PTY black-box: the root model is cheap
// to construct with a nil DB and nil planner (Gate.Refresh() no-ops
// when both DB and Set are nil — see views/gate.go), so the whole
// event loop runs deterministically in `go test`. No binary build,
// no fixtures, no flake from terminal timing.
//
// Typical use:
//
//	h := newTUI(t)
//	h.WaitForText("Sync: idle")    // wait for first paint
//	h.Press("ctrl+g")              // focus the gate pane
//	m := h.FinalModel()            // inspect terminal model state
//
// teatest is a test-only dependency; keeping the harness in a _test.go
// file means it never leaks into the shipped binary.
//
// Output handling: teatest exposes the program's render stream as a
// drain-once io.Reader (a bytes.Buffer that reports EOF when empty).
// The harness runs one background goroutine that copies that stream
// into a cumulative, ANSI-aware buffer, so WaitForText and Screen see
// every frame ever painted — not just whatever arrived since the last
// call. Without this an idle TUI (which emits no new frames) would
// make a second WaitForText time out against an empty stream.

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest"

	"kai/internal/tui/views"
)

// defaultTermSize is the simulated terminal the harness boots with.
// 80x24 is the classic default and what most layout code is tuned
// for; override per-test with WithSize.
const (
	defaultTermW = 80
	defaultTermH = 24
)

// tuiHarness wraps a teatest.TestModel with kai-specific conveniences:
// special-key presses, de-ANSI'd cumulative screen snapshots, and
// text waits.
type tuiHarness struct {
	t  *testing.T
	tm *teatest.TestModel

	mu       sync.Mutex
	rendered strings.Builder // cumulative raw render stream
	stop     chan struct{}   // closed to tell the drain goroutine to exit
	done     chan struct{}   // closed when the drain goroutine has exited
}

// harnessConfig is assembled by the WithX options passed to newTUI.
type harnessConfig struct {
	opts         Options
	w, h         int
	syncCh       chan views.SyncEvent
	chatCh       chan views.ChatActivityEvent
	verboseTools bool
}

// harnessOption customizes a harness before the program boots.
type harnessOption func(*harnessConfig)

// WithSize overrides the simulated terminal dimensions.
func WithSize(w, h int) harnessOption {
	return func(c *harnessConfig) { c.w, c.h = w, h }
}

// WithOptions supplies a populated tui.Options (DB, Planner, etc.).
// The zero value is the default and keeps the harness fixture-free.
func WithOptions(o Options) harnessOption {
	return func(c *harnessConfig) { c.opts = o }
}

// WithChannels wires live sync / chat-activity channels so a test can
// inject SyncEvent / ChatActivityEvent values and watch the panes
// react. Omit it and the model runs with no pumps (nil channels).
func WithChannels(syncCh chan views.SyncEvent, chatCh chan views.ChatActivityEvent) harnessOption {
	return func(c *harnessConfig) { c.syncCh, c.chatCh = syncCh, chatCh }
}

// WithVerboseTools opts the harness's REPL into verbose tool-event
// rendering — the pre-v0.31.24 behavior where every Kind="tool"
// ChatActivityEvent writes a "→ name args" line to scrollback.
// Default is quiet (the v0.31.24+ behavior) where tool events
// update the spinner thinking line and increment a counter
// without flooding scrollback. Tests that specifically pin the
// rendered scrollback output of tool events should pass this.
func WithVerboseTools() harnessOption {
	return func(c *harnessConfig) { c.verboseTools = true }
}

// newTUI boots the TUI model inside a simulated terminal and returns a
// harness for driving it. The program runs on its own goroutine; the
// returned harness talks to it via teatest.
func newTUI(t *testing.T, opts ...harnessOption) *tuiHarness {
	t.Helper()

	cfg := harnessConfig{w: defaultTermW, h: defaultTermH}
	for _, o := range opts {
		o(&cfg)
	}

	m := initialModel(cfg.opts, cfg.syncCh, cfg.chatCh, nil, nil)
	if cfg.verboseTools {
		m.repl.SetVerboseTools(true)
	}
	tm := teatest.NewTestModel(t, m,
		teatest.WithInitialTermSize(cfg.w, cfg.h),
	)

	h := &tuiHarness{
		t:    t,
		tm:   tm,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	// Drain goroutine: teatest's output reader is a bytes.Buffer that
	// returns EOF the moment it is empty, so a plain io.Copy would
	// stop at the first gap between frames. We poll instead, appending
	// every chunk to the cumulative buffer and sleeping briefly on EOF
	// until the program exits.
	out := tm.Output()
	go h.drain(out)

	// Belt-and-suspenders initial size: WithInitialTermSize already
	// sends one WindowSizeMsg, but sending a second after Run is
	// underway guarantees a non-zero size lands even if the first
	// raced program startup (a zero size makes View() render "").
	tm.Send(tea.WindowSizeMsg{Width: cfg.w, Height: cfg.h})

	// Always tear the program down at test end so a forgotten Quit()
	// doesn't leak the program goroutine into the next test.
	t.Cleanup(func() {
		_ = tm.Quit() // blocks until the program goroutine returns
		close(h.stop) // tell drain to do its final read and exit
		<-h.done      // wait for drain to actually be gone
	})
	return h
}

// drain copies the program's render stream into the cumulative buffer.
// teatest's output is a bytes.Buffer: it reports io.EOF whenever it is
// momentarily empty and *never* reports a real close, so EOF alone
// can't mean "program done". We instead loop on EOF until the stop
// channel is closed (by Cleanup, after the program has exited), then
// do one final read to capture the last frame and return.
func (h *tuiHarness) drain(r io.Reader) {
	defer close(h.done)
	buf := make([]byte, 4096)
	read := func() {
		for {
			n, err := r.Read(buf)
			if n > 0 {
				h.mu.Lock()
				h.rendered.Write(buf[:n])
				h.mu.Unlock()
			}
			if err != nil {
				return // EOF or real error — nothing more right now
			}
		}
	}
	for {
		read()
		select {
		case <-h.stop:
			read() // final sweep for frames painted during shutdown
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// keyTypes maps human key names to bubbletea key types for Press().
// Plain printable text should go through Type() instead.
var keyTypes = map[string]tea.KeyType{
	"enter":     tea.KeyEnter,
	"esc":       tea.KeyEsc,
	"tab":       tea.KeyTab,
	"space":     tea.KeySpace,
	"backspace": tea.KeyBackspace,
	"delete":    tea.KeyDelete,
	"up":        tea.KeyUp,
	"down":      tea.KeyDown,
	"left":      tea.KeyLeft,
	"right":     tea.KeyRight,
	"pgup":      tea.KeyPgUp,
	"pgdown":    tea.KeyPgDown,
	"home":      tea.KeyHome,
	"end":       tea.KeyEnd,
	"ctrl+c":    tea.KeyCtrlC,
	"ctrl+r":    tea.KeyCtrlR,
	"ctrl+g":    tea.KeyCtrlG,
	"ctrl+s":    tea.KeyCtrlS,
	"ctrl+a":    tea.KeyCtrlA,
	"ctrl+e":    tea.KeyCtrlE,
}

// Press sends one or more named special keys in order (e.g. "ctrl+g",
// "enter", "esc"). It fails the test on an unknown name rather than
// silently dropping the keystroke.
func (h *tuiHarness) Press(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		kt, ok := keyTypes[k]
		if !ok {
			h.t.Fatalf("harness: unknown key %q (use Type() for printable text)", k)
		}
		h.tm.Send(tea.KeyMsg{Type: kt})
	}
}

// Type enters printable text into whatever view holds focus, one rune
// per key event — the same path a real keyboard takes.
func (h *tuiHarness) Type(s string) {
	h.t.Helper()
	h.tm.Type(s)
}

// Send pushes a raw tea.Msg into the program. Use it to inject the
// async messages the TUI normally receives from goroutines —
// views.SyncEventMsg, views.CmdResultMsg, tea.WindowSizeMsg, etc.
func (h *tuiHarness) Send(msg tea.Msg) {
	h.t.Helper()
	h.tm.Send(msg)
}

// Resize simulates a terminal resize.
func (h *tuiHarness) Resize(w, hgt int) {
	h.t.Helper()
	h.tm.Send(tea.WindowSizeMsg{Width: w, Height: hgt})
}

// WaitForText blocks until the cumulative rendered output contains
// want (ANSI escape codes stripped, so styling never hides a match),
// or fails the test after the timeout. Default timeout is 3s; pass one
// explicitly to override.
func (h *tuiHarness) WaitForText(want string, timeout ...time.Duration) {
	h.t.Helper()
	d := 3 * time.Second
	if len(timeout) > 0 {
		d = timeout[0]
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(h.Screen(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("WaitForText: %q not found after %s.\n--- screen ---\n%s", want, d, h.Screen())
}

// Screen returns everything rendered so far, with ANSI escape codes
// stripped — a plain-text snapshot for substring assertions.
func (h *tuiHarness) Screen() string {
	h.mu.Lock()
	raw := h.rendered.String()
	h.mu.Unlock()
	return ansi.Strip(raw)
}

// Quit asks the program to exit and blocks until it does. It mirrors
// the TUI's own two-step ctrl+c gesture: an empty input draft means
// the first ctrl+c quits, so one press is enough here.
func (h *tuiHarness) Quit() {
	h.t.Helper()
	h.tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	h.tm.WaitFinished(h.t, teatest.WithFinalTimeout(3*time.Second))
}

// FinalModel quits the program if it is still running, then returns
// the terminal model so a test can assert on unexported state
// (focus, REPL contents). Safe to call after Quit() — teatest guards
// the wait with a sync.Once, and a Send to a finished program is a
// no-op.
func (h *tuiHarness) FinalModel() model {
	h.t.Helper()
	// Nudge the program to exit. With an empty input draft the first
	// ctrl+c quits; if the test already called Quit() this is a
	// harmless extra keystroke against an exited program.
	h.tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	fm := h.tm.FinalModel(h.t, teatest.WithFinalTimeout(3*time.Second))
	m, ok := fm.(model)
	if !ok {
		h.t.Fatalf("harness: final model has unexpected type %T", fm)
	}
	return m
}
