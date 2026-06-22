package views

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SyncEvent is a single file-system change observed by the watcher.
// The TUI's parent owns the *watcher.Watcher and forwards events to
// the sync pane via a channel; this pane is intentionally decoupled
// from the watcher type so it can be tested with synthetic events.
type SyncEvent struct {
	Path string
	Op   string // "modified" | "created" | "deleted" | "updated"
	When time.Time
}

// SyncEventMsg wraps a SyncEvent for delivery via tea.Cmd. The pane's
// Update handler appends it to the rolling buffer and re-arms the
// channel pump so the next event also arrives.
type SyncEventMsg struct {
	Event SyncEvent
}

// SyncErrorMsg surfaces watcher errors (start failure, fsnotify
// errors, etc.) so the pane can show "watcher unavailable" without
// crashing the whole TUI.
type SyncErrorMsg struct {
	Err error
}

// Sync is the live-activity pane. It renders the most recent N file
// events as a scrollable log. The pane is purely a consumer — it
// holds no fsnotify state and never blocks; the parent app pipes
// events into it via PumpEvents.
type Sync struct {
	width   int
	height  int
	focused bool

	events  []SyncEvent // ring buffer, newest at the end
	maxKeep int

	lastErr error
}

// NewSync builds a fresh sync pane. maxKeep caps the buffer; older
// events fall off the front.
func NewSync(maxKeep int) Sync {
	if maxKeep <= 0 {
		maxKeep = 200
	}
	return Sync{maxKeep: maxKeep}
}

func (s *Sync) SetSize(width, height int) { s.width, s.height = width, height }
func (s *Sync) Focus()                    { s.focused = true }
func (s *Sync) Blur()                     { s.focused = false }
func (s *Sync) Focused() bool             { return s.focused }

// PumpEvents returns a tea.Cmd that, when run, reads one event from
// the channel and emits it as a SyncEventMsg. The caller arranges
// for the pump to re-arm itself on every Update — see app.go.
//
// We don't loop forever inside one Cmd because tea.Program serializes
// Cmds through goroutines; a continuously-blocked Cmd would prevent
// graceful shutdown. One event per Cmd, re-armed after, lets the
// program stop cleanly.
func PumpEvents(ch <-chan SyncEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil // channel closed; pump stops naturally
		}
		return SyncEventMsg{Event: ev}
	}
}

// Update appends incoming events. Returns nothing on its own — the
// parent re-arms the pump after delivering each event.
func (s Sync) Update(msg tea.Msg) (Sync, tea.Cmd) {
	switch msg := msg.(type) {
	case SyncEventMsg:
		s.events = append(s.events, msg.Event)
		if len(s.events) > s.maxKeep {
			drop := len(s.events) - s.maxKeep
			s.events = s.events[drop:]
		}
		return s, nil
	case SyncErrorMsg:
		s.lastErr = msg.Err
		return s, nil
	}
	return s, nil
}

// View renders the most recent events that fit in the pane's height.
// Newest at the bottom so the cursor of attention naturally tracks
// the latest activity.
func (s Sync) View() string {
	header := styleHeader.Render("Sync — live activity")
	if !s.focused {
		header = styleHeaderDim.Render("Sync — live activity")
	}

	var body string
	switch {
	case s.lastErr != nil:
		body = styleError.Render("  watcher: " + s.lastErr.Error())
	case len(s.events) == 0:
		body = styleDim.Render("  (no activity yet)")
	default:
		body = strings.Join(s.renderRows(), "\n")
	}

	frame := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(maxInt(s.width-2, 0))
	if s.focused {
		frame = frame.BorderForeground(lipgloss.Color("12"))
	} else {
		frame = frame.BorderForeground(lipgloss.Color("8"))
	}

	inner := lipgloss.JoinVertical(lipgloss.Left, header, body)
	return frame.Render(inner)
}

// renderRows returns the rendered event lines, oldest first, capped
// to whatever the pane height allows. We keep ~3 chrome lines (header,
// border top, border bottom) clear of content.
func (s *Sync) renderRows() []string {
	visible := s.height - 3
	if visible < 1 {
		visible = 1
	}
	start := 0
	if len(s.events) > visible {
		start = len(s.events) - visible
	}
	rows := make([]string, 0, len(s.events)-start)
	for _, ev := range s.events[start:] {
		rows = append(rows, formatSyncRow(ev))
	}
	return rows
}

func formatSyncRow(ev SyncEvent) string {
	ts := ev.When.Format("15:04:05")
	op := strings.ToLower(ev.Op)
	if op == "" {
		op = "updated"
	}
	style := styleDim
	switch op {
	case "deleted":
		style = styleError
	case "created":
		style = styleWarn
	}
	return fmt.Sprintf("  %s  %s  %s",
		styleDim.Render(ts),
		style.Render(fmt.Sprintf("%-8s", op)),
		ev.Path,
	)
}
