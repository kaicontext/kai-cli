package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"kai/internal/tui/views"
)

// TestLayoutDoesNotPanic asserts that initialModel + WindowSizeMsg +
// View renders something across a range of window sizes without
// panicking. Bubble Tea apps are notoriously fragile to negative
// dimensions on resize edge cases; this catches obvious regressions.
//
// We pass nil DB to keep the test free of fixtures — the construction
// path doesn't dereference DB until a gate refresh fires, and we
// don't trigger one here.
func TestLayoutDoesNotPanic(t *testing.T) {
	sizes := []struct{ w, h int }{
		{40, 12},   // tiny
		{80, 24},   // standard terminal
		{200, 60},  // large
		{40, 6},    // very short — REPL gets squeezed
		{8, 8},     // pathological
	}
	for _, s := range sizes {
		m := initialModel(Options{}, nil, nil, nil, nil)
		m2, _ := m.Update(tea.WindowSizeMsg{Width: s.w, Height: s.h})
		out := m2.View()
		// Just confirm we got non-empty output for non-zero sizes.
		if s.w > 0 && s.h > 0 && len(out) == 0 {
			t.Errorf("size %dx%d: empty view", s.w, s.h)
		}
	}
}

// TestFocusToggle asserts the focus accessor moves between panes on
// the documented keystrokes. Doesn't drive the full event loop —
// just verifies the model's setFocus path.
func TestFocusToggle(t *testing.T) {
	m := initialModel(Options{}, nil, nil, nil, nil)
	if m.focused != focusREPL {
		t.Fatalf("default focus should be REPL, got %v", m.focused)
	}
	m.setFocus(focusGate)
	if m.focused != focusGate || !m.gate.Focused() {
		t.Fatal("Ctrl+G should focus gate")
	}
	m.setFocus(focusSync)
	if m.focused != focusSync || !m.sync.Focused() {
		t.Fatal("Ctrl+S should focus sync")
	}
	if m.gate.Focused() {
		t.Fatal("gate should lose focus when sync gains it")
	}
	m.setFocus(focusREPL)
	if m.gate.Focused() || m.sync.Focused() {
		t.Fatal("Ctrl+R should clear focus from gate and sync")
	}
}

// TestSyncErrorRendersInPane confirms a watcher startup failure shows
// up as text inside the sync pane rather than crashing the TUI.
func TestSyncErrorRendersInPane(t *testing.T) {
	m := initialModel(Options{}, nil, nil, nil, errStub("watcher couldn't start"))
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	out := m2.View()
	if !strings.Contains(out, "watcher couldn't start") {
		t.Errorf("expected watcher error in view, got: %s", out)
	}
}

// errStub is a tiny error type so the test doesn't need to import
// errors.New just for one literal. Keeps the test file dependency-free.
type errStub string

func (e errStub) Error() string { return string(e) }

// _ assertion: the views package's exported types we depend on are
// still in place. If any of these renames the test fails to compile,
// which is the right early signal.
var (
	_ = views.NewSync
	_ = views.NewGate
	_ = views.NewREPL
	_ = views.SyncEventMsg{}
)
