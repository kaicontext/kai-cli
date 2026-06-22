package tui

// harness_example_test.go exercises the TUI through the tuiHarness
// (see harness_test.go). These double as worked examples: copy one as
// the starting point for a new TUI flow test.

import (
	"testing"
	"time"

	"kai/internal/tui/views"
)

// TestTUI_BootsAndRendersStatusBar is the smoke test: the model boots
// in a simulated 80x24 terminal and paints the status bar. The bar's
// gate/sync segments render fixed text on a fresh session, so they're
// a stable anchor for "did the TUI come up at all".
func TestTUI_BootsAndRendersStatusBar(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Gate: 0 held")
	h.WaitForText("Sync: idle")
}

// TestTUI_TypingEchoesIntoREPL confirms printable keystrokes land in
// the focused REPL input and show up on screen — the basic path every
// interactive test depends on.
func TestTUI_TypingEchoesIntoREPL(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Sync: idle") // wait for first paint
	h.Type("describe a change")
	h.WaitForText("describe a change")
}

// TestTUI_FocusSwitchesToGate drives the ctrl+g shortcut and inspects
// the terminal model: focus should have moved off the REPL onto the
// gate pane. Demonstrates asserting on unexported state via FinalModel.
func TestTUI_FocusSwitchesToGate(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Sync: idle")
	h.Press("ctrl+g")

	m := h.FinalModel()
	if m.focused != focusGate {
		t.Fatalf("ctrl+g should focus the gate pane, got focus=%v", m.focused)
	}
	if !m.gate.Focused() {
		t.Fatal("gate sub-view should report itself focused")
	}
}

// TestTUI_EscReturnsToREPL checks the documented "esc anywhere goes
// back to the REPL" behavior: focus the sync pane, then press esc.
func TestTUI_EscReturnsToREPL(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Sync: idle")
	h.Press("ctrl+s") // focus sync
	h.Press("esc")    // ...and back

	m := h.FinalModel()
	if m.focused != focusREPL {
		t.Fatalf("esc should return focus to the REPL, got focus=%v", m.focused)
	}
}

// TestTUI_ResizeDoesNotCrash boots small, then drives a range of
// resize events — the edge case Bubble Tea apps are most fragile to.
// A panic in layout() would be swallowed by Update's recover wrapper
// rather than crash the test, so we assert on the terminal model: it
// must have absorbed the final resize (proving the events were
// processed) and survived to a clean exit.
func TestTUI_ResizeDoesNotCrash(t *testing.T) {
	h := newTUI(t, WithSize(40, 10))
	// "Sync: idle" is truncated to "Sync: …" at 40 columns; the gate
	// segment sits earlier in the bar and stays fully visible.
	h.WaitForText("Gate: 0 held")
	for _, s := range []struct{ w, h int }{
		{120, 40}, {200, 60}, {30, 8}, {80, 24},
	} {
		h.Resize(s.w, s.h)
	}
	m := h.FinalModel()
	if m.width != 80 || m.height != 24 {
		t.Fatalf("model should reflect the final 80x24 resize, got %dx%d", m.width, m.height)
	}
}

// TestTUI_CtrlCIsTwoStep verifies the readline-style exit gesture: the
// first ctrl+c with a non-empty draft clears the input rather than
// quitting, so a misfire can't lose a half-typed prompt.
func TestTUI_CtrlCIsTwoStep(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Sync: idle")
	h.Type("half-typed prompt")
	h.WaitForText("half-typed prompt")

	h.Press("ctrl+c") // first press: clears the draft, does NOT quit
	m := h.FinalModel()
	if got := m.repl.InputValue(); got != "" {
		t.Fatalf("first ctrl+c should clear the draft, input still %q", got)
	}
}

// TestTUI_SyncEventReachesPane injects a watcher-style SyncEvent on the
// wired channel and confirms it surfaces in the rendered UI. Shows how
// to drive the async, goroutine-fed message paths from a test.
func TestTUI_SyncEventReachesPane(t *testing.T) {
	syncCh := make(chan views.SyncEvent, 8)
	h := newTUI(t, WithChannels(syncCh, nil))
	h.WaitForText("Sync: idle")

	syncCh <- views.SyncEvent{Path: "internal/foo.go", Op: "modified", When: time.Now()}
	h.WaitForText("internal/foo.go")
}
