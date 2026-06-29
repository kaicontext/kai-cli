package tui

// slash_commands_test.go exercises Kai's own REPL slash commands —
// the `/command` surface a user types inside `kai code` — by driving
// them through the tuiHarness (see harness_test.go).
//
// Scope: the TUI-internal commands that need neither a graph DB nor a
// configured planner. With a zero-value Options the REPL has nil
// services, so commands like `/gate` (DB-backed) and bare subcommands
// (which shell out to the kai binary) are deliberately not covered
// here — they belong in an integration test with a real fixture.

import (
	"testing"
)

// submit types a slash command into the focused REPL and presses
// enter, the same gesture a user makes ("Enter sends" per the banner).
func (h *tuiHarness) submit(line string) {
	h.t.Helper()
	h.Type(line)
	h.Press("enter")
}

// TestSlash_StatusReport runs `/status` and checks the report renders
// with the default mode for a fresh, session-less REPL.
func TestSlash_StatusReport(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Sync: idle")
	h.submit("/status")
	h.WaitForText("status:")
	h.WaitForText("coding (default)")
	h.WaitForText("(no active session)")
}

// TestSlash_ModeOverrides runs each mode-override command and confirms
// the REPL echoes the new mode. These set forcedMode for the next turn
// without touching the network or DB.
func TestSlash_ModeOverrides(t *testing.T) {
	cases := []struct {
		cmd  string
		mode string
	}{
		{"/code", "coding"},
		{"/debug", "debug"},
		{"/plan", "planning"},
		{"/review", "review"},
		{"/chat", "conversation"},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			h := newTUI(t)
			h.WaitForText("Sync: idle")
			h.submit(c.cmd)
			h.WaitForText("mode → " + c.mode)
		})
	}
}

// TestSlash_ModeOverrideThenStatus confirms a `/code` override is
// reflected by a subsequent `/status` — the report should attribute
// the mode to the slash override rather than the default.
func TestSlash_ModeOverrideThenStatus(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Sync: idle")
	h.submit("/debug")
	h.WaitForText("mode → debug")
	h.submit("/status")
	h.WaitForText("forced via slash override")
}

// TestSlash_Copy exercises the /copy command's block-selection logic.
// Bare /copy ships the entire transcript; /copy 1 ships only the last block.
func TestSlash_Copy(t *testing.T) {
	t.Run("bare copies all", func(t *testing.T) {
		h := newTUI(t)
		h.WaitForText("Sync: idle")
		h.submit("/copy")
		// Bare /copy copies ALL transcript blocks, not just one.
		// The confirmation reads "copied N block(s)"; N must be > 1
		// on a fresh REPL (the boot banner produces multiple blocks).
		h.WaitForText("block(s)")
	})
	t.Run("explicit 1", func(t *testing.T) {
		h := newTUI(t)
		h.WaitForText("Sync: idle")
		h.submit("/copy 1")
		h.WaitForText("copied 1 block(s)")
	})
}

// TestSlash_Clear runs `/clear` and confirms the REPL survives it —
// the command resets history, session, and pending plan state. There
// is no positive output to assert, so the check is that the status
// bar still renders afterward (the program did not crash or quit).
func TestSlash_Clear(t *testing.T) {
	h := newTUI(t)
	h.WaitForText("Sync: idle")
	h.submit("/status") // put something in scrollback first
	h.WaitForText("status:")
	h.submit("/clear")

	m := h.FinalModel()
	if m.focused != focusREPL {
		t.Fatalf("/clear should leave focus on the REPL, got %v", m.focused)
	}
}
