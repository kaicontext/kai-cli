package views

import (
	tea "github.com/charmbracelet/bubbletea"

	"kai/internal/tui/fixxy"
)

// FixxyEventMsg wraps a fixxy.Event for delivery to REPL.Update.
// REPL handles it by writing a dim "fixxy: <text>" line to the
// scrollback so the user can watch the self-heal progress
// without it dominating their session.
type FixxyEventMsg struct {
	Event fixxy.Event
}

// PumpFixxy reads one event from the fixxy worker's channel and
// emits it as a FixxyEventMsg. Mirrors PumpChatActivity — the
// parent re-arms after each delivery so we never block shutdown
// and so the channel keeps draining for the life of the TUI.
//
// Returns nil when the channel closes (worker stopped). Bubble
// Tea drops nil messages, naturally ending the pump loop.
func PumpFixxy(ch <-chan fixxy.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return FixxyEventMsg{Event: ev}
	}
}
