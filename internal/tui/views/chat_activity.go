package views

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ChatActivityEvent is a single agent observation streamed back to the
// REPL while a chat-fallback turn is in flight. Tool dispatches and
// file changes both flow through here so the user sees the agent
// working in real time instead of staring at a silent prompt.
type ChatActivityEvent struct {
	// Kind is one of:
	//   - "tool"        tool dispatch ("→ bash: ls"); Summary set
	//   - "file"        minimal file mutation note ("created package.json")
	//   - "diff"        per-edit unified diff; Diff/Path/Op/Added/Removed set
	//   - "tokens"      cumulative usage after a model turn; TokensIn/Out set
	//   - "delta"       streaming assistant text chunk; Delta set
	//   - "gate"        safety gate verdict; GatePaths/GateVerdict/etc.
	//   - "agent_start" a new chat-fallback agent run is in flight
	//   - "agent_end"   an agent run has finished (or errored)
	//   - "agent_text"  per-turn assistant prose from a spawned agent
	//   - "provider_state" HTTP/SSE call lifecycle (sent → connected
	//     → streaming → done|error); Summary is the formatted line
	//   - "bash_confirm" agent wants to run a bash command; Summary
	//     is the command, Reply is the channel it's blocked on.
	//     The REPL reads keys and writes true/false to Reply; the
	//     agent goroutine unblocks and continues. Channel close also
	//     means cancel — defensive against the REPL closing during
	//     shutdown while an agent is mid-prompt.
	//   - "file_confirm" agent wants to write/edit a file; Path/Op/
	//     Added/Removed describe the change, Diff carries a truncated
	//     unified-diff preview (~40 lines), Reply is the decision
	//     channel. Same blocking semantics as bash_confirm.
	Kind         string
	Summary      string
	TokensIn     int
	TokensOut    int
	TokensCached int

	// Diff-event fields. Populated only when Kind == "diff".
	Path    string
	Op      string // "created" | "modified"
	Diff    string // full unified diff
	Added   int
	Removed int

	// Delta-event field: streaming assistant text. Concatenate
	// across events to build the full reply. Populated only when
	// Kind == "delta".
	Delta string

	// Gate-event fields. Populated only when Kind == "gate".
	GatePaths   []string // workspace-relative paths the verdict covers
	GateVerdict string   // "auto" | "review" | "block" | "error"
	GateRadius  int      // depth-1 callers + dependents count
	GateReasons []string // human-readable reasons (protected paths, threshold breach)

	// Bash-confirm fields. Populated only when Kind == "bash_confirm".
	// SpawnName lets the prompt say which agent is asking. Reply is
	// where the REPL writes the user's decision (true=run, false=cancel)
	// before unblocking the agent goroutine.
	//
	// Warning is a non-empty informational label when the command
	// matched a destructive pattern (e.g. "may recursively force-
	// remove files" for `rm -rf`). Display it prominently above the
	// command in the prompt. Empty means no label.
	SpawnName string
	Warning   string
	Reply     chan bool

	// CostUSD is the computed dollar cost for this turn's token usage,
	// derived from the provider's pricing model.
	CostUSD      float64   `json:"costUSD,omitempty"`
	When         time.Time `json:"when"`
}

// ChatActivityMsg wraps a ChatActivityEvent for delivery via tea.Cmd.
// REPL.Update appends it to the scrollback as a dim line.
type ChatActivityMsg struct {
	Event ChatActivityEvent
}

// PumpChatActivity reads one event from the chat-activity channel and
// emits it as a ChatActivityMsg. Mirrors PumpEvents for SyncEvents —
// the parent re-arms after each delivery so we never block shutdown.
func PumpChatActivity(ch <-chan ChatActivityEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return ChatActivityMsg{Event: ev}
	}
}

// HostProcEventMsg wraps a HostProcEvent for delivery via tea.Cmd.
// Mirrors ChatActivityMsg shape; the REPL handler renders the event
// as a scrollback line and (for error_detected) auto-dispatches a
// follow-up turn so the agent can investigate without the user
// copy-pasting.
type HostProcEventMsg struct {
	Event HostProcEvent
}

// PumpHostProcEvents reads one event from the host-proc channel and
// emits it as a HostProcEventMsg. Same re-arm-after-each-delivery
// pattern as PumpChatActivity.
func PumpHostProcEvents(ch <-chan HostProcEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return HostProcEventMsg{Event: ev}
	}
}
