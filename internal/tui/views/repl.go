// Package views holds the individual Bubble Tea sub-models that the
// root TUI app stitches into a single layout.
package views

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"kai/api/agent"
	"kai/api/message"
	"kai/api/provider"
	"kai/api/session"
	"kai/api/clipboard"
	"kai/api/kaipath"
	"kai/api/memstat"
	"kai/api/planner"
	"kai/api/telemetry"
	"github.com/kaicontext/kai-engine/tasksmd"
	errpkg "kai/internal/tui/errors"
	"kai/internal/tui/fixxy"
	"kai/internal/vision"
)

// REPL is the input/output pane. It accepts free-form text, treats
// the first word as a kai subcommand, and shells out to the running
// binary to execute it. Output streams into the viewport.
//
// Shell-out (vs. invoking the cobra command tree in-process) is the
// simplest honest path: it keeps the REPL's notion of "running a
// command" identical to what a user would get at their normal shell,
// and avoids tangling Bubble Tea's stdout capture with cobra's
// internal output writers. Switching to in-process dispatch later is
// straightforward if shell-out becomes a bottleneck.
type REPL struct {
	input textarea.Model
	// pendingPrints holds every line queued for the terminal's
	// native scrollback. The slice grows over the session — slices
	// from index `flushedTo` to the end are what flushPrints
	// emits next via tea.Println; older entries stay around so
	// tests (and any future inspection) can see the full session
	// log without having to capture from tea.Cmd return values.
	// /clear resets both the slice and the cursor.
	pendingPrints []string
	flushedTo     int

	// historyVP is the in-program scrollback pane. With
	// alt-screen mode we can no longer use the terminal's
	// native scrollback (tea.Println), so finished turns
	// accumulate inside a viewport.Model the user can scroll
	// with PgUp/PgDn or the mouse wheel. Sticky to the bottom
	// unless the user has scrolled up.
	historyVP viewport.Model
	// historyContent is the source-of-truth string the
	// viewport renders. Kept on the struct so flushPrints
	// can append in O(append) instead of re-reading the view.
	historyContent string
	// historyReady gates viewport sizing — until SetSize fires
	// we don't know the terminal width and SetContent at zero
	// width would wrap to one-char-per-row.
	historyReady bool

	// transient is the live status line — spinner ("planning…"),
	// streamed token counter, etc. Rendered above the input each
	// View() call. Cleared rather than scrolled away because it's
	// a per-turn indicator, not part of the conversation log.
	//
	// The composed value is rebuilt from spinnerView + tokenView each
	// time either updates so the spinner phrase and the live token
	// counter can co-exist on consecutive lines instead of flashing
	// over each other (each owns its own bucket; composeTransient
	// joins them). Modal writers (cost-cap prompt, plan menu, error
	// detail) still set transient directly — they're mutually
	// exclusive with the spinner/token state, so overwriting the
	// composed value is the right behavior.
	transient    string
	spinnerView  string
	tokenView    string
	// bannerText holds the rendered startup banner. Emitted to
	// scrollback once via Banner() (returned from the parent's
	// Init). Stored on the struct so a /clear can re-emit it.
	bannerText string

	history []string
	histIdx int // -1 = not browsing history
	width   int
	height  int
	binary  string
	workDir string

	// transcript mirrors every block written to scrollback (write /
	// writeRaw / writeMarkdown). Used by /copy to ship recent output
	// to the clipboard. Capped at transcriptCap entries to bound
	// memory in long-running sessions; older entries fall off the
	// front. Keeps raw (un-wrapped, un-styled) text where possible —
	// what gets pasted is the same content the user saw.
	transcript []string

	// pendingBashConfirm is non-nil while an agent is blocked on a
	// bash-approval prompt. Set when a "bash_confirm" ChatActivityMsg
	// arrives; cleared when the user presses [y]/[n]/esc and the
	// REPL writes the decision back to the event's Reply channel.
	// While set, key handling intercepts y/n/esc instead of dispatching
	// them to the textarea.
	pendingBashConfirm *ChatActivityEvent

	// bashAllowlist records first-tokens the user blanket-approved
	// for this REPL session via the [a] key in the bash prompt.
	// Future bash calls whose first-token matches a key here skip
	// the prompt entirely. Per-process — exits with the TUI; not
	// persisted to .kai/config.yaml. Agents can't add to it; only
	// user keystrokes can.
	bashAllowlist map[string]bool

	// pendingFileConfirm mirrors pendingBashConfirm but for write/edit
	// approval. Set when a "file_confirm" event arrives; cleared on
	// the user's keystroke response. y/a/n keys route to it the same
	// way they route to bash approval.
	pendingFileConfirm *ChatActivityEvent

	// pendingHostCommand is non-nil while the user is being asked
	// whether to run a host-shell command proposed by the host-task
	// fast path (triage.TrackHost). UNLIKE pendingBashConfirm, the
	// command runs DIRECTLY in the kai process's shell context (not
	// inside a CoW spawn workspace) — kai already has the user's
	// permissions, so `make install` writes to ~/go/bin exactly the
	// way it would if the user typed it themselves. y/Enter = run,
	// n/esc = dismiss.
	pendingHostCommand string

	// fileWritesAllowed is the session-wide blanket approval for
	// file mutations. Once flipped true (by the user pressing [a]
	// in any file_confirm prompt, or at construction in hands-off
	// mode), all subsequent write/edit calls auto-approve until the
	// TUI exits. Per-process; not persisted.
	fileWritesAllowed bool

	// allowAllBash is the bash equivalent of fileWritesAllowed: when
	// true, every bash_confirm auto-approves. Set at construction in
	// hands-off mode. Distinct from bashAllowlist (per-first-token
	// approvals the user granted with [a]); this is the blanket form.
	allowAllBash bool

	// Planner state. nil services → REPL operates in shell-out-only
	// mode and unrecognized commands fail through to the kai binary
	// (which prints its usual "unknown command" message).
	services    *PlannerServices
	pendingPlan *planner.WorkPlan
	originalReq string // the request that produced pendingPlan; carries through Replan

	// pendingImages is the queue of image file paths attached via
	// `/image <path>`. Consumed on the next user submit: each image
	// is sent to the vision model, descriptions are appended to
	// the prompt under "Images used in prompt:", then the queue
	// clears. Empty queue → submit dispatches as normal text.
	pendingImages []string

	// pendingAction holds the proposed action from the last chat
	// reply that ended with the "Reply 'yes' and I'll apply it."
	// trailer. On the next turn, if the user types a short
	// affirmative, dispatch wraps the request with this text as a
	// structured "you offered X, user confirmed, execute X"
	// preamble — the model no longer has to scan history and
	// infer which offer "yes" refers to. P0 fix from the
	// 2026-05-26 confirmation-loop spec: without this, "yes"
	// after a clear offer routed through the inherited session
	// but still got bounced as "what would you like me to do?"
	// because the binding was implicit in history, not explicit
	// in the prompt.
	pendingAction *pendingAction

	// lastExecutedPlan keeps the plan that was just dispatched to
	// runExecute so end-of-turn gate review has plan-preferred
	// context (Summary/Approach/Diagnosis) on top of the chat ring.
	// Cleared on /clear; survives multiple held snapshots within
	// the same plan execution so each gets the same plan context.
	lastExecutedPlan *planner.WorkPlan

	// forceScrollNextFlush overrides flushPrints' wasAtBottom check
	// for the next flush. Set when the user submits a new turn so
	// the prompt + response land in the visible area even if the
	// user had scrolled up mid-stream reviewing earlier output.
	// Cleared as soon as it's honored.
	forceScrollNextFlush bool

	// planChoice is the highlighted option in the plan-confirmation
	// menu rendered while pendingPlan is set:
	//   0 → go        (default)
	//   1 → cancel
	//   2 → feedback  (focus textarea, type to replan)
	// -1 means "no menu showing." Reset to 0 each time a fresh plan
	// lands so the user starts on the most-likely "go" answer.
	planChoice         int
	planDetailsExpanded bool   // true while full plan details are shown (toggled by ?)
	planning           bool   // true while a planner LLM call is in flight
	executing     bool // true while orchestrator is running
	gateReviewing bool // true while a gate-review audit LLM call is in flight


	// lastActivity is the wall-clock time of the most recent
	// model-side signal in this turn — used by the soft stuck-hint
	// when a real provider state event hasn't arrived yet.
	lastActivity time.Time

	// providerState is the most recent RequestState reported by the
	// active provider call (planner / spawned agent / gate review).
	// renderSpinner shows it directly — real call state, not a guess.
	providerState   ChatActivityEvent
	providerStateAt time.Time

	// sessionID stickies the SHARED conversation across turns
	// within this TUI run. Both paths — chat fallback and planner
	// agent — resume this same session, so a follow-up like "fix
	// it" after a chat-mode answer inherits the chat's transcript
	// even if the follow-up routes to the planner. Without
	// unification, chat and planner kept separate transcripts and
	// the cross-talk was invisible to the model.
	//
	// The system prompt sent on each turn still varies by mode
	// (ModePlanning vs ModeConversation vs ModeCoding), so the
	// model knows what it's doing on THIS turn even though prior
	// turns may have been a different mode. Anthropic respects the
	// latest system prompt as authoritative.
	//
	// Empty until the first turn lands. Forgotten on TUI exit —
	// not persisted across `kai code` invocations.
	sessionID string

	// stepParser scans assistant deltas for STEPS:/STEP_DONE: markers
	// and emits a TaskProgress checklist that renders inline above
	// the streaming text. Reset on every user submit so a fresh
	// turn doesn't inherit the previous turn's pending block.
	stepParser   stepParser
	taskProgress *TaskProgress

	// forcedMode holds a developer-issued slash override (/code,
	// /debug, /review, /plan, /chat) until the next agent turn
	// consumes it. Once consumed, the resolved mode is persisted to
	// the session's prev_mode column and forcedMode resets to
	// ModeUnknown — subsequent turns flow through normal sticky/soft
	// resolution against the persisted prev mode. Slash overrides
	// outrank detection per the spec, so when forcedMode is set the
	// dispatch layer uses it directly instead of calling DetectMode.
	forcedMode agent.Mode

	// spinner powers the animated "planning…" / "running plan…"
	// status line so the user can tell something is in flight even
	// when the model is between tool calls and the screen is
	// otherwise quiet. Re-rendered into the transient on every
	// spinner.TickMsg; idle when neither planning nor executing.
	spinner     spinner.Model
	spinnerText string // current label ("planning…", "thinking…", etc.)
	// thinkingLine is the latest sentence of model narration
	// captured during a planner run via OnThinking. Rendered
	// below the spinner so the user sees what the model is
	// working on. Reset between turns by the dispatch handler
	// when a new user submit lands.
	thinkingLine string

	// streamBuf accumulates assistant text as it arrives via
	// OnAssistantDelta events. Rendered live above the input while
	// streaming so the reply visibly grows as it arrives; on
	// finalize either dropped (the markdown-rendered final replaces
	// it) or flushed to scrollback. streamActive distinguishes "no
	// stream" from "empty in-flight stream".
	//
	// Plain string (not strings.Builder) because Bubble Tea
	// copies models by value on every Update — copying a non-zero
	// strings.Builder panics at runtime. Don't switch back to a
	// Builder without using a pointer indirection.
	streamBuf    string
	streamActive bool
	// streamClosed flips true once finalizeStream has run for the
	// current turn, and resets to false when the user submits a new
	// prompt. While true, late delta events (channel/queue stragglers
	// that arrive after PlanReadyMsg has already been processed —
	// PumpChatActivity and runPlan land messages from different
	// goroutines so ordering isn't guaranteed) are dropped instead
	// of re-priming streamBuf. Without this, those late deltas
	// would re-render the streamed text in View *after* the
	// rendered final has already landed in scrollback, producing
	// two copies of the response on screen.
	streamClosed bool

	// Token counter animation. tokenTarget is the "true" cumulative
	// count from the most recent OnTurnComplete; tokenShown is the
	// currently-rendered count (interpolated toward target via
	// tickTokenAnim). When shown == target, the tweener idles and
	// no further ticks are scheduled. Cleared on each new chat
	// turn so each reply animates from 0.
	tokenTargetIn     int
	tokenTargetOut    int
	tokenTargetCached int
	tokenShownIn      int
	tokenShownOut     int
	tokenShownCached  int
	tokenAnimating    bool

	// sessionCostUSD accumulates EstimatedCostUSD across every chat
	// and planner turn in this TUI invocation. Surfaced in the
	// trailer as "Session: ~$X.XX (N turns)" so the user can see
	// the running total climb without doing arithmetic across
	// per-turn costs. Reset only on TUI exit — not on /clear,
	// since the cost was real even if the conversation was wiped.
	//
	// Used by the cost guardrail: when KAI_MAX_SESSION_COST_USD
	// is set and sessionCostUSD exceeds it, the next agent run is
	// gated on a y/N confirmation rendered as a modal banner.
	sessionCostUSD float64
	sessionTurns   int

	// runStart / runOutputTokens drive the Claude-Code-comparable
	// summary line "(14m 15s · ↓ 46.0k tokens)" that lands above
	// the verbose trailer. Reset at the start of every user
	// submit; accumulated output tokens come from chat + planner
	// turns within the run. Wall-clock is real elapsed (not CPU
	// time) so users comparing kai vs Claude Code see the same
	// dimension Claude Code's status line shows.
	runStart        time.Time
	runOutputTokens int

	// verboseTools, when false (default), suppresses the per-tool-
	// call scrollback lines ("→ kai_grep …", "→ view …") that
	// otherwise pile up during an exploration phase and flood the
	// screen. Spinner still shows live activity. When true, the
	// previous behavior (every tool dispatch writes a scrollback
	// line) resumes. Toggle with /verbose. The 2026-05-24 feedback
	// pinned this as the biggest TUI ergonomics complaint: agents
	// often emit 15-30 tool calls during an exploration phase, all
	// of which are "context" the user usually doesn't need.
	verboseTools bool

	// suppressedToolEvents counts how many tool events the current
	// run hid from scrollback because verboseTools was false. The
	// run-trailer surfaces the count as a single dim line so the
	// user can choose to flip /verbose for the next turn.
	suppressedToolEvents int

	// criticRetryCount tracks how many auto-retries the critic
	// has triggered in the current chain. Reset on PASS, on a
	// user-typed prompt, or when the cap fires. Exists to bound
	// an infinite retry loop: the critic may keep marking FAIL
	// on a request the agent legitimately can't fulfill, and
	// we'd rather surface the final critique than burn LLM
	// calls forever.
	criticRetryCount int

	// retractedAnswer holds the prose answer currently under critic
	// review, so it can be RESTORED if the critic FAILs it and the
	// auto-retry itself then fails (e.g. the model returns no text).
	// Without this, a failed retry leaves the user with a hard error
	// and no answer — strictly worse than the (merely critiqued) answer
	// they already had. Captured when the critic runs; the restore is
	// gated on criticRetryCount>0 so it only fires during an active
	// retry. Cleared on restore and on a passing critic verdict.
	retractedAnswer string

	// inFlightPendingAction holds the pendingAction text just
	// consumed by the current dispatch, scoped to a single turn.
	// On critic FAIL, the retry path copies this into the new
	// pendingCriticRetry so the retried turn restores the
	// pending-action binding instead of replaying a bare "yes"
	// that would routed by the planner as standalone-vague.
	// P1-3 from the 2026-05-26 confirmation-loop spec: a retry
	// that reproduces the exact failure is wasted; this carries
	// the structural binding forward so the retry has different
	// inputs than the failed attempt.
	inFlightPendingAction string

	// autoEscalatedTurn marks whether the current chat turn has
	// already triggered a kai_consult auto-escalation. One-shot
	// per turn so an agent that gets stuck AGAIN after consult
	// doesn't loop in escalation. Reset on a new user-typed
	// prompt.
	autoEscalatedTurn bool

	// autoEscalateRequest holds the original user request from
	// the turn that's currently stalling. When the escalation
	// fires, we use this as the prompt seed for the new turn
	// rather than the agent's most recent (stuck) output.
	autoEscalateRequest string

	// suppressedToolLines is a bounded ring of the formatted
	// tool-event lines hidden from scrollback this turn. On a
	// successful PlanReadyMsg we discard it (just print the count
	// trailer). On an errored / deadline-exceeded PlanReadyMsg we
	// dump the buffer to scrollback automatically — the tool-call
	// trace is the most important debug artifact when a run fails,
	// and burying it behind a /verbose toggle the user could only
	// flip BEFORE the failure wasted the turn. Capped at
	// suppressedToolLinesCap to bound memory on runaway loops.
	suppressedToolLines []string

	// lastToolSummary holds the most recent tool dispatch's
	// formatted "→ name args" line — surfaced in the spinner thinking
	// row when verboseTools is off so the user can see what's
	// happening live without the scrollback flood.
	lastToolSummary string

	// turnFirstResponseAt is the moment the first provider
	// streaming-phase event arrived this turn — i.e. when the LLM
	// stopped "thinking" and started emitting bytes. The delta
	// runStart → turnFirstResponseAt is the time-to-first-byte the
	// user actually felt; the trailer surfaces it next to the total
	// elapsed and token counts so slow turns can be diagnosed
	// without a profiler. Zero before the first streaming event
	// arrives; reset by startRun.
	turnFirstResponseAt time.Time

	// pendingCostCap, when non-nil, is the request the user
	// submitted that triggered the session-cost cap. The REPL
	// freezes input mode to "y/N" until they answer. y → reset
	// the cap-strike state and dispatch the request; n/N/esc →
	// drop the request, print "canceled" line, return to prompt.
	pendingCostCap *pendingCostPrompt

	// pendingModelPicker, when non-nil, is the arrow-controlled model
	// picker modal. Opens on bare `/model` or `/model <provider>` (no
	// id) and routes ↑/↓/Enter/Esc until the user picks or cancels.
	// Same modal-priority pattern as pendingCostCap / pendingPlan.
	pendingModelPicker *ModelPickerState

	// trailerRendered flips true once the run's final combined
	// trailer has landed in scrollback (via PlanReadyMsg). Late
	// `tokens` activity events from the agent's final
	// OnTurnComplete callback can arrive AFTER PlanReadyMsg —
	// the agent goroutine and the dispatch goroutine race on
	// channel delivery. Without this guard, those late events
	// would set fresh tween targets and re-animate the live
	// transient line, leaving an orphan "X fresh / Y out / Z%
	// reused" trailer below "─ end ─" until the user's next
	// submit. Reset to false on startRun().
	trailerRendered bool

	// recentTurns rolls a small ring of the last few user
	// requests + assistant replies. Mode 2 (fixxy "no sir i
	// don't like it") needs this to bundle context with the
	// complaint when forwarding to claude. Capped at 5 turns
	// to keep the prompt tight; older turns drop silently.
	recentTurns []turnRecord
	responseStarts []int // responseStarts records transcript indices where each agent run began. Used by /copy response to ship whole exchanges.

	// Slash-command autocomplete. Active whenever the input starts
	// with "/" and contains no space (i.e. user is still typing the
	// command name, not its args). suggestItems is the filtered list
	// of matches; suggestIdx is the highlighted entry. Tab accepts,
	// ↑/↓ cycles, Esc dismisses. Items are recomputed after every
	// keystroke that mutates the input.
	suggestItems []string
	suggestIdx   int
	// suggestKind tracks what the current popup is offering so
	// acceptSuggestion knows how to insert the choice — top-level
	// commands replace the whole input, subcommands preserve the
	// `/<cmd> ` prefix.
	suggestKind suggestKind

	// gateReview, when non-nil, holds the in-TUI walkthrough state
	// for `/gate` review mode. Keystrokes [a]/[r]/[s]/[d]/[f]/[q]
	// route to handleGateReviewKey before the textarea sees them.
	// Cleared back to nil on completion / [q] / [esc].
	gateReview *gateReviewState

	// fileIndex is a sorted, deduped list of workspace files used
	// for "@" autocomplete. Populated at REPL construction by
	// walking each project root via walkIndexableFiles, refreshed
	// every fileIndexRefresh interval so newly-created files
	// appear without restarting the TUI.
	//
	// Multi-root: each entry is the workspace-relative path; in a
	// multi-project workspace, paths are prefixed with the
	// project name (e.g. "kai-cli/cmd/kai/main.go") to match the
	// kai_files tool's reporting.
	fileIndex   []string
	fileIndexAt time.Time

	// tasksAddedThisRun counts user messages typed during the
	// active run that were captured as Pending tasks in TASKS.md
	// (mid-flight input is no longer queued — it becomes a task the
	// running agent picks up on its next turn). Reset when a run
	// starts; read on cancel to drive the "press Enter to continue"
	// prompt.
	tasksAddedThisRun int
	// continueArmed is set by a cancel that left newly-added tasks
	// in TASKS.md: the next bare Enter launches a fresh turn that
	// incorporates them. Cleared on that continue or any real submit.
	continueArmed bool
	// cancelRequestedAt records when a cancel last tripped
	// CancelCurrent. Retained for idle bookkeeping; one-press cancel
	// clears the busy flags immediately so it rarely matters now.
	cancelRequestedAt time.Time
}

type suggestKind int

const (
	suggestKindNone suggestKind = iota
	suggestKindCommand
	suggestKindSubcommand
	// suggestKindArg fires for the third token of certain
	// `/<cmd> <sub> <arg>` commands that take a single concrete
	// identifier — currently the held-integration ID for
	// `/gate approve|reject|show`. Provider funcs in
	// slashArgProviders return the candidate strings.
	suggestKindArg
	// suggestKindFile fires when the cursor is inside an "@<prefix>"
	// token. acceptSuggestion replaces the @<prefix> span with the
	// chosen path (the @ marker is dropped on insertion).
	suggestKindFile
)

// fileIndexRefresh bounds the staleness of the @-autocomplete index.
// 30s strikes a balance: short enough that a user creating a new
// file mid-session sees it in autocomplete after a brief delay;
// long enough that the periodic walk doesn't noticeably affect
// CPU on huge repos.
const fileIndexRefresh = 30 * time.Second

// fileIndexCap caps how many entries the autocomplete keeps in
// memory. 50k handles even very large monorepos; beyond that the
// popup loses utility anyway and the walk cost stops being free.
const fileIndexCap = 50000

// slashArgProviders supplies dynamic completion candidates for the
// third token of `/<cmd> <sub> <prefix>`. Keyed by `cmd sub` (a
// space-joined string). Providers receive the live PlannerServices
// so they can query the graph DB for IDs / paths / etc.
//
// Returns must be cheap — these run on every keystroke. Cache or
// pre-sort inside the provider if the source list could be large.
var slashArgProviders = map[string]func(s *PlannerServices) []string{
	"gate approve": gateHeldIDs,
	"gate reject":  gateHeldIDs,
	"gate show":    gateHeldIDs,
	"gate diff":    gateHeldIDs,
	"gate review":  gateHeldIDs,
}

// slashCommands is the curated set of names offered as completions
// when the user types "/". /clear is TUI-internal; the rest map 1:1
// to top-level kai cobra subcommands. Keep alphabetized — the order
// here is the order shown in the popup.
var slashCommands = []string{
	"blame", "capture", "changeset", "chat", "checkout", "checkpoint", "ci",
	"clear", "clone", "code", "copy", "debug", "diff", "exit", "fetch", "gate", "import",
	"init", "integrate", "intent", "list", "live", "log", "merge",
	"model", "modules", "org", "plan", "pull", "push", "query", "quiet", "refresh", "remote", "resolve",
	"review", "share", "snap", "snapshot", "spawn", "stats", "status", "test",
	"ui", "update", "verbose", "version", "ws",
}

// slashSubcommands maps a top-level slash command to the sub-tokens
// the autocomplete should offer when the user types `/<cmd> <prefix>`.
// Static lists — dynamic args (snap IDs for `gate show`, file paths
// for `diff`) would need DB / fs lookups and are deferred.
//
// Keep alphabetized within each command for predictable popup order.
var slashSubcommands = map[string][]string{
	"gate":    {"approve", "configure", "diff", "list", "reject", "review", "show"},
	"ws":      {"checkout", "create", "current", "integrate", "list"},
	"modules": {"add", "init", "list", "preview", "remove"},
	"review":  {"comment", "list", "open", "show"},
	"live":    {"off", "on", "status"},
	"remote":  {"add", "list", "remove", "set-default"},
	"snap":    {"list", "show"},
	"ci":      {"plan", "run"},
}

// modeOverrideSlash maps the leading token of a mode-override slash
// command to its target mode. Handled inside dispatch as TUI-internal
// state changes — they never shell out to the kai binary.
var modeOverrideSlash = map[string]agent.Mode{
	"/code":   agent.ModeCoding,
	"/edit":   agent.ModeCoding, // alias — /edit is the natural verb users type for "I want to make changes now"
	"/debug":  agent.ModeDebug,
	"/review": agent.ModeReview,
	"/plan":   agent.ModePlanning,
	"/chat":   agent.ModeConversation,
}

// immediateSlashes lists the TUI-internal slash commands that bypass
// the busy-queue and run immediately even when a turn is in flight.
// Each one mutates next-turn state only (model, mode) or is read-only
// (/status), so applying it now is the user's clear intent — they'd
// be confused to find a /model swap waiting in the queue until the
// current turn finishes.
var immediateSlashes = map[string]bool{
	"/model":   true,
	"/status":  true,
	"/code":    true,
	"/debug":   true,
	"/review":  true,
	"/plan":    true,
	"/chat":    true,
	"/stop":    true,
	"/verbose": true,
	"/quiet":   true,
	"/share":   true,
}

// isImmediateSlash reports whether line is a slash command that should
// fire immediately rather than queue behind an active run. Matches on
// the first whitespace-separated token so `/model kailab gpt-5.5`
// still qualifies.
func isImmediateSlash(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return false
	}
	head := line
	if i := strings.IndexAny(line, " \t"); i > 0 {
		head = line[:i]
	}
	return immediateSlashes[strings.ToLower(head)]
}

// NewREPL builds a fresh REPL view. binary is the path to the kai
// executable to dispatch input to; workDir is the cwd for child
// commands. services is optional — when non-nil it enables natural-
// language requests via the planner; nil means shell-out-only.
// NewREPLWithSession is the resume-aware constructor. When
// resumeSessionID is non-empty, the REPL stamps that id onto its
// sessionID field at construction; the first agent turn then runs
// with Options.SessionID set, which session.Resume picks up from
// the persisted store. Empty means "fresh session" — a new id gets
// minted on the first turn.
//
// NewREPL stays as the simpler entry point (no session arg) so test
// helpers and any non-TUI callers don't need to plumb a session id.
func NewREPLWithSession(binary, workDir string, services *PlannerServices, resumeSessionID string) REPL {
	r := NewREPL(binary, workDir, services)
	r.sessionID = strings.TrimSpace(resumeSessionID)
	if r.sessionID != "" {
		r.write(styleDim.Render(fmt.Sprintf("resumed session %s — agent has prior context", r.sessionID)))
	}
	return r
}

func NewREPL(binary, workDir string, services *PlannerServices) REPL {
	in := textarea.New()
	in.Placeholder = "describe a change, or /command (e.g. /gate list, /push)"
	// SetPromptFunc draws "› " on the first visible line and matching
	// whitespace on continuation lines. textarea's plain `Prompt`
	// field repeats on every line, which produced the "›\n›\n›"
	// look users hit when adding newlines via alt+enter.
	in.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return "› "
		}
		return "  "
	})
	in.Focus()
	in.CharLimit = 8192
	// One row by default, growing up to 8 as the user types more.
	// Layout() recomputes height each Update so the bordered box
	// expands and the status bar stays glued to the bottom.
	in.SetHeight(1)
	in.ShowLineNumbers = false
	// Enter SUBMITS the input (handled in REPL.Update); newline is
	// alt+enter / ctrl+j. Without this rebind, every Enter would
	// just insert a newline and the user could never send.
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter", "ctrl+j"))
	// Option+Delete on macOS terminals: most send ESC+0x7f which
	// Bubble Tea reports as alt+backspace, but a few send other
	// variants (ESC+0x08 → alt+ctrl+h on some configs, raw 0x08
	// → ctrl+h on legacy setups). Bind the full family so Option+
	// Delete works regardless of which sequence the user's
	// terminal emits. Same idea for forward word-delete.
	in.KeyMap.DeleteWordBackward = key.NewBinding(key.WithKeys(
		"alt+backspace",
		"alt+delete",
		"alt+ctrl+h",
		"ctrl+w",
		"ctrl+backspace",
	))
	in.KeyMap.DeleteWordForward = key.NewBinding(key.WithKeys("alt+d", "alt+ctrl+delete"))
	// Static cursor — blinking causes the layout to re-render every
	// half-second, which made the status bar / top of the viewport
	// appear to flash on slower terminals.
	in.Cursor.SetMode(cursor.CursorStatic)

	// MiniDot is a single rotating braille character —
	// recognizable, calm (vs. Points' 4-frame dot-position
	// rotation that read as "multiple icons switching"), and
	// what Claude Code uses. Bright cyan + bold makes it pop
	// against scrollback without being busy.
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)

	// History viewport. Width/height get set in the first
	// SetSize call.
	vp := viewport.New(0, 0)

	r := REPL{
		input:      in,
		histIdx:    -1,
		binary:     binary,
		workDir:       workDir,
		services:      services,
		spinner:       sp,
		bannerText:    renderBanner(services),
		historyVP:     vp,
		bashAllowlist: map[string]bool{},
		// -1 means no plan menu showing. Set to 0 (focus "go")
		// when a plan lands.
		planChoice: -1,
	}
	// Seed the viewport with the banner so first frame shows
	// it, like the old tea.Println-banner did.
	if r.bannerText != "" {
		r.historyContent = r.bannerText
		r.historyVP.SetContent(r.historyContent)
	}

	// Restore prompt history from disk so up-arrow recalls prior
	// sessions' prompts. Per-project (under .kai/repl_history) so
	// prompts from one repo don't leak into another. Best-effort —
	// missing/unreadable file just yields an empty in-memory list.
	r.history = loadReplHistory(workDir)

	// Hands-off mode: pre-arm the blanket approvals so file-write and
	// bash confirmations never block the run. The plan menu is
	// auto-confirmed in the PlanReadyMsg handler. The final gate
	// approve still requires the user.
	if services != nil && services.HandsOff {
		r.fileWritesAllowed = true
		r.allowAllBash = true
	}
	return r
}

// Banner returns a tea.Cmd that wipes the terminal (visible screen
// AND scrollback) and prints the startup banner at the top, with
// the live input region pinned to wherever the cursor lands below
// it. Used once from the parent's Init so launch feels like a
// fresh canvas, and again from /clear so the wiped session has
// the identity line back at the top.
//
// ESC[3J clears scrollback; ESC[2J clears the visible screen;
// ESC[H homes the cursor. tea.Sequence guarantees the clear
// happens before the println — tea.Batch would let them race and
// occasionally print the banner first, then erase it.
// AppendSystemError surfaces a brief, non-alarming line in the
// REPL when the TUI's panic recover swallowed something. Returns
// the (value-receiver) REPL so the caller can chain the assignment
// back into the model. Doesn't itself touch terminal state — the
// next View() pass picks up the appended line normally.
func (r REPL) AppendSystemError(text string) REPL {
	r.write(styleError.Render("⚠ " + text))
	return r
}

// AppendGateBanner writes the launch-time "N changes held" banner into
// the REPL scrollback. Called once per session by the parent model
// when the first gate refresh comes back non-empty. The text is a
// nudge, not a gate — typing /gate review runs the AI review flow.
func (r REPL) AppendGateBanner(n int) REPL {
	noun := "change"
	if n != 1 {
		noun = "changes"
	}
	r.write(styleWarn.Render(fmt.Sprintf(
		"⚠ %d %s held for review. Type /gate review to inspect, or continue working.",
		n, noun)))
	return r
}

// AppendGateHoldNotice fires when the held count grows during a
// session — an integration just produced a new held snapshot. Kept
// short so it doesn't interrupt the user's current task.
func (r REPL) AppendGateHoldNotice(delta int) REPL {
	noun := "integration"
	if delta != 1 {
		noun = "integrations"
	}
	r.write(styleWarn.Render(fmt.Sprintf(
		"⚠ Gate held %d new %s. Type /gate review when you're ready.",
		delta, noun)))
	return r
}

// Banner is a no-op now: the banner is seeded into the
// history viewport at construction (NewREPL) and re-seeded by
// ClearHistory on /clear. Method retained so app.Init's call
// shape doesn't change.
func (r REPL) Banner() tea.Cmd { return nil }

// ClearHistory wipes the in-program scrollback and re-seeds
// the banner. Used by /clear.
func (r *REPL) ClearHistory() {
	r.pendingPrints = nil
	r.flushedTo = 0
	r.historyContent = r.bannerText
	r.historyVP.SetContent(r.historyContent)
	r.historyVP.GotoBottom()
}

// HistoryView renders the in-program scrollback pane. The
// parent model places this above the live region.
func (r REPL) HistoryView() string { return r.historyVP.View() }

// IsBusy reports whether the REPL has a run in flight that Ctrl+C
// should cancel rather than treating as "exit kai." Read-only —
// safe to call from the parent app.go's key handler.
func (r REPL) IsBusy() bool { return r.planning || r.executing || r.gateReviewing }

// fullCancel stops the active run in a SINGLE gesture: trip the
// context cancel, then immediately drop our local belief that
// anything is running (clear the busy flags, stop the spinner) so the
// TUI is usable at once. An in-flight model call may still finish in a
// background goroutine — that's accepted; the TUI no longer waits on
// it, and the process still exits cleanly on quit. If the user added
// tasks during the run, arm the "press Enter to continue" follow-up.
func (r *REPL) fullCancel() {
	if r.services != nil {
		r.services.CancelCurrent()
	}
	r.planning = false
	r.executing = false
	r.gateReviewing = false
	r.clearTransient()
	r.cancelRequestedAt = time.Time{}
	if r.tasksAddedThisRun > 0 {
		r.write(styleDim.Render(fmt.Sprintf(
			"cancelled — +%d task(s) you added during the run are in TASKS.md; press Enter to continue with them, or keep typing to redirect",
			r.tasksAddedThisRun)))
		r.continueArmed = true
	} else {
		r.write(styleDim.Render("cancelled (an in-flight model call may finish in the background)"))
	}
}

// HandleCtrlC handles a Ctrl+C. When busy it fully cancels the active
// run in one press (same as esc) and returns true. When idle it
// returns false so the caller (app.go) falls through to its
// draft-clear / quit gesture.
func (r *REPL) HandleCtrlC() bool {
	if !r.IsBusy() {
		r.cancelRequestedAt = time.Time{}
		return false
	}
	memstat.Log("ctrl-c-pressed")
	memstat.LogBurst("ctrl-c", 2*time.Second, 3*time.Second, 5*time.Second, 10*time.Second)
	r.fullCancel()
	return true
}

// SessionID returns the persisted session id stamped on the REPL,
// either from a --session resume at launch or generated on the
// first agent turn. Empty when no agent has run yet (a TUI launch
// where the user typed nothing returns "" — there's nothing to
// resume).
func (r REPL) SessionID() string { return r.sessionID }

// HandleScroll routes a scroll-related msg (mouse wheel,
// PgUp/PgDn) to the history viewport.
func (r REPL) HandleScroll(msg tea.Msg) (REPL, tea.Cmd) {
	var cmd tea.Cmd
	r.historyVP, cmd = r.historyVP.Update(msg)
	return r, cmd
}

// SetSize records the latest window dimensions, reflows the
// textarea width, and resizes the history viewport to fill the
// space above the live region.
// SetVerboseTools toggles per-tool-call scrollback rendering. False
// (default) is quiet mode: spinner shows live activity, no
// scrollback lines per dispatch. True restores the pre-v0.31.24
// "every dispatch prints" behavior. Public so test harnesses and
// configuration loaders can opt in without going through the
// /verbose slash command.
func (r *REPL) SetVerboseTools(on bool) {
	r.verboseTools = on
}

// suppressedToolLinesCap bounds the size of the auto-promote buffer.
// A pathological runaway loop could otherwise fill memory; 200 lines
// is enough to debug any real failure (the dogfood transcripts that
// motivated this auto-promote had 10-16 hidden tool calls per run).
const suppressedToolLinesCap = 200

// flushSuppressedToolLines dumps the buffer of hidden tool-event
// lines into scrollback, prefixed by a single header line so the
// user knows what they're seeing. Called from the failed-run path
// of PlanReadyMsg. No-op when the buffer is empty (verbose mode on
// or no tool events fired this turn).
func (r *REPL) flushSuppressedToolLines(header string) {
	if len(r.suppressedToolLines) == 0 {
		return
	}
	r.clearTransient()
	r.write(styleDim.Render(header))
	for _, line := range r.suppressedToolLines {
		r.writeRaw(line)
	}
	r.suppressedToolLines = nil
	// Zero the count too: the trailing "N tool calls hidden"
	// summary would be misleading once we've already promoted
	// them above.
	r.suppressedToolEvents = 0
}

func (r *REPL) SetSize(width, height int) {
	r.width, r.height = width, height
	r.input.SetWidth(width - 2)
	r.inputBoxHeight()

	live := r.liveRegionHeight()
	histH := height - live
	if histH < 1 {
		histH = 1
	}
	r.historyVP.Width = width
	r.historyVP.Height = histH
	if !r.historyReady {
		r.historyReady = true
		r.historyVP.SetContent(r.historyContent)
		r.historyVP.GotoBottom()
	}
}

// liveRegionHeight estimates rows occupied by the bottom live
// region (input + transient + popup + streaming preview). The
// transient may span multiple rows now that the spinner and token
// counter render as separate lines (composeTransient joins them with
// '\n'); under-counting here would let the second line spill into
// the input box's top border on tall transient states.
func (r *REPL) liveRegionHeight() int {
	h := r.inputBoxHeight()
	if r.transient != "" {
		h += 1 + strings.Count(r.transient, "\n")
	}
	if r.streamActive && r.streamBuf != "" {
		h += 6
	}
	if len(r.suggestItems) > 0 {
		h += 4
	}
	return h
}

// maxInputRows caps how tall the input box grows. Beyond this the
// textarea scrolls internally — protects the viewport from being
// shrunk to nothing when a user pastes a 40-line prompt.
const maxInputRows = 8

// inputBoxHeight returns the total rendered height of the input
// area: the textarea's current line count + 2 for the border. Used
// to decide how much vertical room the viewport gets so the layout
// re-flows as the user types multi-line prompts.
func (r *REPL) inputBoxHeight() int {
	rows := r.input.LineCount()
	if rows < 1 {
		rows = 1
	}
	if rows > maxInputRows {
		rows = maxInputRows
	}
	if r.input.Height() != rows {
		r.input.SetHeight(rows)
	}
	return rows + 2 // +2 for top + bottom border lines
}

// InputValue returns the current draft text. Used by the parent
// model's Ctrl+C handler to decide between "clear draft" and "quit"
// without poking at the underlying textarea directly.
func (r REPL) InputValue() string { return r.input.Value() }

// ClearInput drops whatever the user has typed and resets history
// browsing. The two-step Ctrl+C calls this on the first press so a
// misfire doesn't kill the TUI with a half-written prompt.
func (r *REPL) ClearInput() {
	r.input.Reset()
	r.histIdx = -1
	r.suggestItems = nil
	r.suggestIdx = 0
	if r.width > 0 && r.height > 0 {
		r.SetSize(r.width, r.height)
	}
}

// Focus gives focus to the input. Returns the underlying tea.Cmd so
// the parent can chain it into Init/Update.
func (r *REPL) Focus() tea.Cmd { return r.input.Focus() }

// Blur removes focus from the input.
func (r *REPL) Blur() { r.input.Blur() }

// Update handles key input and the async CmdResultMsg that arrives
// when a child command finishes.
func (r REPL) Update(msg tea.Msg) (next REPL, cmd tea.Cmd) {
	// Flush any queued tea.Println content on every return path,
	// not just the bottom one. Handlers like PlanReadyMsg and
	// ChatActivityMsg call r.write/r.writeMarkdown and then return
	// early — without this defer, their writes would sit in
	// pendingPrints until the *next* Update (e.g. a keystroke)
	// happened to fall through to the bottom-of-function flush.
	// User-visible symptom: the assistant's reply only appeared
	// after the user typed something. The defer wraps every
	// return through a single point that always batches the
	// flush.
	defer func() {
		if flush := next.flushPrints(); flush != nil {
			cmd = tea.Batch(cmd, flush)
		}
	}()
	// Async messages from the in-TUI gate review walkthrough land
	// before any sub-view dispatch so the REPL state advances even
	// while the textarea has focus. handleGateReviewKey (below) takes
	// priority over textarea input on the keystroke side, so review
	// mode runs end-to-end without the user needing to dance around
	// other modals.
	switch msg.(type) {
	case gateReviewResultMsg, GateReviewActionMsg, gateReviewDiffMsg, GateReviewFixMsg:
		c := r.applyGateReviewMsg(msg)
		return r, c
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Gate-review keystroke intercept. Active only while a review
		// walkthrough is in progress; the handler ignores keys when
		// the user is mid-typing (input non-empty), so a literal "a"
		// in a feedback message during review still works as text.
		if ok, c := r.handleGateReviewKey(msg.String()); ok {
			return r, c
		}
		// Bash-approval intercept. Same priority as the cost cap:
		// while the agent is blocked waiting for run/cancel, the
		// next key must answer that question. Anything else is
		// ignored so an accidental Enter or quote can't unblock
		// the agent unintentionally.
		if r.pendingBashConfirm != nil {
			ev := r.pendingBashConfirm
			switch strings.ToLower(msg.String()) {
			case "y", "enter":
				r.pendingBashConfirm = nil
				if ev.Reply != nil {
					select {
					case ev.Reply <- true:
					default:
					}
				}
				r.write(styleDim.Render("running…"))
				return r, nil
			case "a":
				// Approve AND remember the first-token for the
				// rest of the session — future bash calls with
				// the same first-token skip the prompt.
				r.pendingBashConfirm = nil
				first := firstShellToken(ev.Summary)
				if first != "" {
					if r.bashAllowlist == nil {
						r.bashAllowlist = map[string]bool{}
					}
					r.bashAllowlist[first] = true
				}
				if ev.Reply != nil {
					select {
					case ev.Reply <- true:
					default:
					}
				}
				if first != "" {
					r.write(styleDim.Render(fmt.Sprintf("running… and auto-allowing %q for the rest of this session", first+" *")))
				} else {
					r.write(styleDim.Render("running…"))
				}
				return r, nil
			case "n", "esc", "ctrl+c":
				r.pendingBashConfirm = nil
				if ev.Reply != nil {
					select {
					case ev.Reply <- false:
					default:
					}
				}
				r.write(styleDim.Render("cancelled — agent will continue without running it"))
				return r, nil
			}
			return r, nil
		}

		// File-approval intercept. Same priority as bash — agent is
		// blocked waiting for the answer; nothing else should
		// process keys. y=write, a=write+allow-all-future,
		// n/esc=cancel.
		if r.pendingFileConfirm != nil {
			ev := r.pendingFileConfirm
			switch strings.ToLower(msg.String()) {
			case "y", "enter":
				r.pendingFileConfirm = nil
				if ev.Reply != nil {
					select {
					case ev.Reply <- true:
					default:
					}
				}
				return r, nil
			case "a":
				r.pendingFileConfirm = nil
				r.fileWritesAllowed = true
				if ev.Reply != nil {
					select {
					case ev.Reply <- true:
					default:
					}
				}
				r.write(styleDim.Render("    auto-allowing all file writes for the rest of this session"))
				return r, nil
			case "n", "esc", "ctrl+c":
				r.pendingFileConfirm = nil
				if ev.Reply != nil {
					select {
					case ev.Reply <- false:
					default:
					}
				}
				r.write(styleDim.Render("cancelled — agent will continue without writing"))
				return r, nil
			}
			return r, nil
		}

		// Host-command approval intercept. The host-task fast path
		// proposed a command (e.g. `cd kai-cli && make install`);
		// the user's next keystroke decides whether kai runs it.
		// y/Enter = run, n/esc = dismiss. No "[a]llow all" affordance
		// — host-task commands aren't agent-emitted (they come from
		// triage's fixed recipe table), so blanket-approval doesn't
		// apply. Each one is a deliberate, one-off ask.
		if r.pendingHostCommand != "" {
			cmd := r.pendingHostCommand
			switch strings.ToLower(msg.String()) {
			case "y", "enter":
				r.pendingHostCommand = ""
				r.write(styleDim.Render("running on host: " + cmd))
				// Dev-server-shape commands take the managed-process
				// path (host_proc.go): kai owns the process for its
				// lifetime, a background scanner watches output for
				// new errors, /stop kills it. Non-dev commands use
				// the existing capture-with-tail path (fast exit
				// expected; 12s window is fine).
				if IsDevServerCommand(cmd) && r.services != nil {
					if _, err := StartManagedProcess(r.services, cmd); err != nil {
						r.write(styleDim.Render("✗ failed to launch: " + err.Error()))
						return r, nil
					}
					r.write(styleDim.Render("↗ launched as managed process — kai is watching for errors. /stop to kill it."))
					return r, nil
				}
				// Transient progress indicator so the capture window
				// doesn't read as kai-is-hung. The HostCommandDoneMsg
				// handler clears it before rendering the captured
				// output. Window length is command-shape-dependent —
				// fast commands (~12s).
				window := hostCommandWindowFor(cmd)
				r.transient = styleDim.Render(fmt.Sprintf(
					"↺ watching output for errors (~%ds)…",
					int(window.Seconds()),
				))
				return r, runHostCommand(cmd)
			case "n", "esc", "ctrl+c":
				r.pendingHostCommand = ""
				r.write(styleDim.Render("dismissed — run it yourself if you want: " + cmd))
				return r, nil
			}
			return r, nil
		}

		// Cost-cap modal intercept. Highest-priority key handler:
		// while pendingCostCap is set the user must answer y/N
		// before any other input goes through. Event-driven (not
		// stdin) so it composes with the rest of the Bubble Tea
		// event loop the same way the plan-confirmation menu does.
		if r.pendingCostCap != nil {
			switch strings.ToLower(msg.String()) {
			case "y":
				p := r.pendingCostCap
				r.pendingCostCap = nil
				// Reset the running total so subsequent runs
				// aren't all blocked by the same threshold —
				// honoring "continue" means the next cap strike
				// fires after another full cap's worth of usage.
				r.sessionCostUSD = 0
				r.startRun()
				r.write(styleDim.Render(fmt.Sprintf("continuing past ~$%.2f cap", p.cap)))
				return r, tea.Batch(runPlan(r.services, p.request, r.sessionID, p.forced, ""), r.spinner.Tick)
			case "n", "esc", "ctrl+c":
				r.pendingCostCap = nil
				r.write(styleDim.Render("canceled — request not sent"))
				return r, nil
			}
			// Any other key: ignore (forces an explicit answer).
			return r, nil
		}

		// Model-picker modal intercept. While open, arrows move the
		// cursor, Enter applies the swap, Esc cancels. Everything
		// else is swallowed so a stray keystroke can't bleed into
		// the textarea behind the modal.
		if r.pendingModelPicker != nil {
			switch msg.String() {
			case "up", "k":
				r.pendingModelPicker.MoveUp()
				r.transient = r.pendingModelPicker.Render()
				return r, nil
			case "down", "j":
				r.pendingModelPicker.MoveDown()
				r.transient = r.pendingModelPicker.Render()
				return r, nil
			case "enter":
				picked := r.pendingModelPicker.Selected()
				kind := r.pendingModelPicker.Kind
				r.pendingModelPicker = nil
				r.clearTransient()
				if picked != "" {
					r.write(handleModelCommand(r.services, []string{string(kind), picked}))
				}
				return r, nil
			case "esc", "ctrl+c":
				r.pendingModelPicker = nil
				r.clearTransient()
				r.write(styleDim.Render("model picker canceled"))
				return r, nil
			}
			return r, nil
		}

		// Plan-confirmation menu intercept. When a plan is pending
		// and the textarea is empty, navigation keys cycle the menu
		// selection and Enter confirms. Esc cancels. Once the user
		// types ANY printable char into the textarea, the menu
		// stays visible but Enter switches to "submit feedback as
		// replan" mode (existing dispatch handles this).
		if r.pendingPlan != nil && r.input.Value() == "" {
			switch msg.String() {
			case "left", "shift+tab":
				r.planChoice = (r.planChoice - 1 + 3) % 3
				r.renderPlanMenu()
				return r, nil
			case "right", "tab":
				r.planChoice = (r.planChoice + 1) % 3
				r.renderPlanMenu()
				return r, nil
			case "enter":
				return r.dispatchPlanChoice()
			case "esc":
				r.pendingPlan = nil
				r.originalReq = ""
				r.planChoice = -1
				r.clearTransient()
				r.write(styleDim.Render("plan canceled"))
				return r, nil
			case "?":
				// Toggle expanded plan details inline. First
				// press shows full agent prompts + exploration
				// notes; second press hides them. The action
				// menu stays visible so the user can still pick
				// go / cancel / feedback after reading.
				r.planDetailsExpanded = !r.planDetailsExpanded
				r.renderPlanMenu()
				return r, nil
			}
			// Other keys fall through — typing routes to the
			// textarea and switches to feedback mode implicitly.
		}

		// Autocomplete keys take priority over textarea/history when
		// suggestions are visible. Recompute happens at the end of
		// this case so any path that mutates the input refreshes the
		// list (or hides it).
		if len(r.suggestItems) > 0 {
			switch msg.String() {
			case "tab":
				r.acceptSuggestion()
				return r, nil
			case "up":
				r.suggestIdx = (r.suggestIdx - 1 + len(r.suggestItems)) % len(r.suggestItems)
				return r, nil
			case "down":
				r.suggestIdx = (r.suggestIdx + 1) % len(r.suggestItems)
				return r, nil
			case "esc":
				r.suggestItems = nil
				r.suggestIdx = 0
				return r, nil
			}
		}
		switch msg.String() {
		case "esc":
			// Esc fully cancels the in-flight run in ONE press. Reaches
			// here only when no higher-priority modal claimed the
			// keystroke (gate review, bash/file approval, plan menu,
			// autocomplete) — those switches above already returned.
			// When nothing is running, esc falls through to the textarea
			// below (no-op for the run).
			if r.IsBusy() {
				r.fullCancel()
				return r, nil
			}
		case "enter":
			line := strings.TrimSpace(r.input.Value())
			if line == "" {
				// Bare Enter after a cancel that left newly-added tasks:
				// continue with them. The fresh turn loads TASKS.md and
				// injects it (runner reloads it every turn), so the agent
				// sees the tasks the user added during the cancelled run.
				if r.continueArmed {
					r.continueArmed = false
					r.tasksAddedThisRun = 0
					r.input.Reset()
					return r.submitLine("Continue with the pending tasks in TASKS.md.", true)
				}
				return r, nil
			}
			// A real submission means the user is steering — they're not
			// taking the "press Enter to continue" offer, so disarm it.
			r.continueArmed = false
			// Any user-typed prompt resets the critic auto-retry
			// chain. The user is steering a new direction; we
			// don't want a stale critic-retry counter from a
			// prior turn deciding when to stop on the next one.
			r.criticRetryCount = 0
			// Same for the idle-escalation one-shot guard — new
			// user input is a fresh attempt.
			r.autoEscalatedTurn = false
			r.autoEscalateRequest = line
			// Mid-flight case: input typed while a run is active is NOT
			// queued. It's captured as a Pending task in TASKS.md so the
			// running agent picks it up on its next turn (TASKS.md is
			// reloaded + injected every turn) and it survives an
			// interrupt. Immediate TUI-internal slashes (/model, mode
			// overrides, /status) still apply now via the fall-through.
			if r.IsBusy() && !isImmediateSlash(line) {
				r.history = append(r.history, line)
				appendReplHistory(r.workDir, line)
				r.histIdx = -1
				r.input.Reset()
				n, err := tasksmd.AddPending(r.workDir, line)
				if err != nil {
					r.write(styleDim.Render("could not add task to TASKS.md: " + err.Error()))
				} else {
					r.tasksAddedThisRun++
					r.write(styleDim.Render(fmt.Sprintf(
						"📝 added to TASKS.md as task #%d — the agent will pick it up on its next step (or after you continue)", n)))
				}
				return r, nil
			}
			return r.submitLine(line, true)

		case "up":
			// History only fires when the cursor is on the first
			// row of the textarea — otherwise let textarea handle
			// up-arrow as line navigation within a multi-line draft.
			if r.input.Line() == 0 && len(r.history) > 0 {
				if r.histIdx < 0 {
					r.histIdx = len(r.history) - 1
				} else if r.histIdx > 0 {
					r.histIdx--
				}
				r.input.SetValue(r.history[r.histIdx])
				r.input.CursorEnd()
				return r, nil
			}
		case "down":
			// Same: only navigate history from the bottom row.
			if r.input.Line() == r.input.LineCount()-1 && r.histIdx >= 0 {
				r.histIdx++
				if r.histIdx >= len(r.history) {
					r.histIdx = -1
					r.input.SetValue("")
				} else {
					r.input.SetValue(r.history[r.histIdx])
					r.input.CursorEnd()
				}
				return r, nil
			}
		}

	case CmdResultMsg:
		r.write(formatCmdResult(msg))
		return r, nil

	case HostCommandDoneMsg:
		// Clear the "↺ watching output…" transient that was set when
		// the host command launched. Done first so the output below
		// doesn't render with the indicator still visible above it.
		r.clearTransient()
		// Render the result of a host-shell command kai just ran on
		// the user's behalf (host-task fast path approval). Combined
		// output goes to scrollback; a one-line status banner
		// summarises exit. We render output BEFORE the status banner
		// so the user reads top-down: what happened, then the verdict.
		out := strings.TrimRight(msg.Output, "\n")
		if out != "" {
			for _, line := range strings.Split(out, "\n") {
				r.write("    " + line)
			}
		}
		switch {
		case msg.Detached && msg.DetectedError != "":
			if strings.Contains(msg.DetectedError, "\n") {
				r.write(styleDim.Render("⚠ kai detected an error during the capture window:"))
				for _, line := range strings.Split(msg.DetectedError, "\n") {
					r.write(styleDim.Render("    " + line))
				}
			} else {
				r.write(styleDim.Render("⚠ kai detected an error during the capture window: " + msg.DetectedError))
			}
			r.write(styleDim.Render("  process keeps running in your shell — kai is no longer watching"))
		case msg.Detached:
			r.write(styleDim.Render("↗ launched — process keeps running in your shell, kai is no longer watching"))
			r.write(styleDim.Render("  paste any errors back here if you want kai to debug them"))
		case msg.Err != nil:
			r.write(styleDim.Render("✗ exited with error: " + msg.Err.Error()))
		default:
			r.write(styleDim.Render("✓ done — " + msg.Command))
		}
		// Auto-dispatch a follow-up turn when kai detected an error
		// in the host-command output AND the user hasn't already
		// moved on. Same guard the v0.31.42 critic-retry uses: don't
		// armCancel-murder a user-typed prompt that's in flight, and
		// don't fire when the user has new text waiting. The agent
		// gets the detected error pasted in as a fresh chat turn so
		// it can react without the user copying/pasting.
		if msg.DetectedError != "" && !r.IsBusy() && strings.TrimSpace(r.input.Value()) == "" {
			r.write(styleDim.Render("↻ asking kai to look at the error"))
			// Force coding mode for the auto-followup. The user's
			// implicit intent on "kai detected an error" is "fix it,"
			// which requires write/edit tools. Without this override,
			// the dispatch would inherit sticky-chat mode if the user
			// had a prior /chat session, leaving the model in
			// read-only — at which point the model correctly reports
			// "no edit tool registered here" and the user gets
			// confused about why /code didn't stick. 2026-05-25
			// dogfood pinned this: user typed /code twice, ran it,
			// got an auto-followup, and the agent reported "I am
			// in chat mode" because the prior session's prev_mode
			// was still "Conversation."
			r.forcedMode = agent.ModeCoding
			followup := fmt.Sprintf(
				"The host command `%s` produced this error:\n\n%s\n\nFull output above. Diagnose and fix.",
				msg.Command, msg.DetectedError,
			)
			return r.dispatch(followup)
		}
		return r, nil

	case AutoRepairDoneMsg:
		// A background recovery (kicked off by a classifier-
		// tagged error like preflight.missing_blobs) just
		// finished. Clear the "Reindexing…" transient and
		// write a one-line outcome to scrollback so the user
		// sees the loop close. On success we also drop a hint
		// so they know they can re-issue the request that
		// failed; we don't auto-retry because the original
		// error already cleared the in-flight plan/execute
		// state and the user may have moved on.
		r.clearTransient()
		if msg.Err != nil {
			r.write(styleError.Render(fmt.Sprintf(
				"⚠ auto-repair (%s) failed after %s: %s",
				msg.Kind, msg.Elapsed.Round(100*time.Millisecond), msg.Err,
			)))
			// Show the LAST chunk of captured output (not the
			// first), since the actual error message in a
			// long-running command lives at the END after all
			// the progress chatter ("Analyzing 1/272…
			// 2/272…"). Showing the first 800 chars
			// truncated the relevant failure line. Cap is
			// generous (3000) — diagnostics over conciseness
			// when the auto-repair has already failed and the
			// user is stuck.
			if msg.Output != "" {
				out := msg.Output
				const cap = 3000
				if len(out) > cap {
					out = "…\n" + out[len(out)-cap:]
				}
				r.write(styleDim.Render(out))
			}
			// Mirror the full output to the local error log so
			// it's reachable via `tail -f .kai/errors.log` even
			// when the on-screen render is still capped.
			errpkg.LogLocal(workspaceFor(r.services), errpkg.UserError{
				Kind:       "auto_repair.failed",
				Headline:   fmt.Sprintf("auto-repair %s failed", msg.Kind),
				LogContext: msg.Output,
			}, false)
			return r, nil
		}
		r.write(styleDim.Render(fmt.Sprintf(
			"✓ workspace reindexed (%s) — try your request again",
			msg.Elapsed.Round(100*time.Millisecond),
		)))
		return r, nil

	case FixxyEventMsg:
		// Status updates from the secret fixxy-upper worker.
		// Render dim with a prefix so it's visually distinct
		// from agent activity and easy to scan past.
		// Re-arm the pump immediately so subsequent events
		// keep flowing for the life of the session.
		r.write(styleDim.Render(formatFixxyEvent(msg.Event)))
		if r.services != nil && r.services.Fixxy != nil {
			return r, PumpFixxy(r.services.Fixxy.Events())
		}
		return r, nil

	case SyncEventMsg:
		// During execute mode the only "model is alive" signals are
		// file-write events from spawned agents (assistant text and
		// tool calls flow through OnActivity → syncCh). Bump the
		// idle clock so the soft stuck-hint doesn't false-positive
		// while agents work. App.go also routes this msg here in
		// addition to the sync pane / status bar.
		r.lastActivity = time.Now()
		return r, nil

	case HostProcEventMsg:
		// Managed-process lifecycle event from host_proc.go's
		// scanner. Four event kinds:
		//   - "started"        Process just spawned; nothing to
		//                      render (the y-approval banner
		//                      already covered it).
		//   - "output"         Throttled batch of new stdout/stderr
		//                      lines. Render dim to scrollback so
		//                      the user sees the dev server
		//                      starting / re-compiling. Capped to
		//                      managedOutputBatchMax per batch.
		//   - "error_detected" Scanner found a new error class in
		//                      the output. Render the line in dim
		//                      to scrollback, then (if the user
		//                      hasn't moved on) auto-dispatch a
		//                      follow-up chat turn so the agent can
		//                      investigate without copy-paste.
		//   - "exited"         Process is no longer alive. Render a
		//                      status banner with exit code.
		switch msg.Event.Kind {
		case "output":
			// Managed-process output stream. Two display modes:
			//   - QUIET (default):   drop on the floor. The error
			//                        scanner still fires for real
			//                        problems via "error_detected".
			//                        kai_logs lets the agent read
			//                        recent output on demand.
			//   - VERBOSE (/verbose): write each line dimmed to
			//                         scrollback for live tailing.
			//
			// v0.32.1 streamed-by-default, but the 2026-05-25
			// dogfood pinned the accumulation cost — vite +
			// electron + concurrently produces a lot of noise per
			// second and the scrollback filled up fast. The same
			// /verbose flag that gates tool-call output applies
			// here, so users have one toggle for "show me the
			// noisy stuff."
			if r.verboseTools {
				for _, line := range msg.Event.OutputLines {
					r.write(styleDim.Render("  " + line))
				}
			}
		case "error_detected":
			// ErrorLine may be a multi-line vite/plugin context
			// block (see detectHostCommandError) — render the
			// header on its own line and indent the block below
			// so the scrollback stays scannable.
			if strings.Contains(msg.Event.ErrorLine, "\n") {
				r.write(styleDim.Render("⚠ kai detected an error in `" + msg.Event.Command + "`:"))
				for _, line := range strings.Split(msg.Event.ErrorLine, "\n") {
					r.write(styleDim.Render("    " + line))
				}
			} else {
				r.write(styleDim.Render("⚠ kai detected an error in `" + msg.Event.Command + "`: " + msg.Event.ErrorLine))
			}
			// Auto-dispatch a chat follow-up if the user hasn't
			// moved on (same guard as the v0.31.42 critic-retry
			// and the v0.31.45 host-command auto-followup —
			// user input always wins).
			if r.services != nil && !r.IsBusy() && strings.TrimSpace(r.input.Value()) == "" {
				r.write(styleDim.Render("↻ asking kai to look at the error"))
				// Force coding mode (write+edit). See the matching
				// override in the HostCommandDoneMsg branch above —
				// "fix this error" is the implicit intent of an
				// auto-followup, regardless of any prior /chat that
				// left the session sticky in conversation mode.
				r.forcedMode = agent.ModeCoding
				followup := fmt.Sprintf(
					"The managed process `%s` produced this error:\n\n%s\n\nDiagnose and fix.",
					msg.Event.Command, msg.Event.ErrorLine,
				)
				return r.dispatch(followup)
			}
		case "exited":
			if msg.Event.ExitCode == 0 {
				r.write(styleDim.Render("● `" + msg.Event.Command + "` exited cleanly"))
			} else if msg.Event.ExitCode < 0 {
				r.write(styleDim.Render("● `" + msg.Event.Command + "` was killed"))
			} else {
				r.write(styleDim.Render(fmt.Sprintf("● `%s` exited with code %d", msg.Event.Command, msg.Event.ExitCode)))
			}
		}
		return r, nil

	case ChatActivityMsg:
		// Any chat-activity event counts as model-side progress for
		// the stuck-detector. Stamp here once rather than at every
		// inner case so we never miss a Kind we forgot to enumerate.
		r.lastActivity = time.Now()
		// Inline activity from a chat-fallback agent run.
		switch msg.Event.Kind {
		case "tokens":
			// Drop late events that arrive after the run's
			// final trailer already landed. See trailerRendered's
			// field comment for the race this guards against.
			if r.trailerRendered {
				return r, nil
			}
			// Update animation target. If we're not already running
			// the tweener, kick off a tick. Each tick steps the
			// shown value toward target and re-renders the live
			// line in-place by swapping the current transient.
			r.tokenTargetIn = msg.Event.TokensIn
			r.tokenTargetOut = msg.Event.TokensOut
			r.tokenTargetCached = msg.Event.TokensCached
			if !r.tokenAnimating {
				r.tokenAnimating = true
				return r, scheduleTokenTick()
			}
			return r, nil
		case "diff":
			// Per-edit diff. Render header + colorized hunk lines,
			// like Claude Code's inline "Update(path) — Added N
			// lines" block. Routed through writeRaw because the
			// lipgloss styles embed ANSI escapes and the word
			// wrapper would split mid-escape and trash the colors.
			r.clearTransient()
			r.writeRaw(formatDiffEvent(msg.Event, r.wrapWidth()))
		case "tool":
			// Tool dispatch from the planner/agent runner — the
			// "→ bash: ls" headline a tool call emits before its
			// streamed output. In verbose mode (off by default), it
			// pins cleanly to scrollback like other events. In the
			// default (quiet) mode, it instead updates the spinner's
			// thinking line so the user sees the latest activity
			// without 15-30 lines piling into scrollback during an
			// exploration phase.
			toolLine := formatToolEvent(msg.Event, r.wrapWidth())
			if r.verboseTools {
				r.clearTransient()
				r.writeRaw(toolLine)
			} else {
				r.suppressedToolEvents++
				if len(r.suppressedToolLines) < suppressedToolLinesCap {
					r.suppressedToolLines = append(r.suppressedToolLines, toolLine)
				}
				r.lastToolSummary = strings.TrimSpace(msg.Event.Summary)
				if r.lastToolSummary != "" {
					r.thinkingLine = r.lastToolSummary
				}
				r.renderSpinner()
			}
		case "bash":
			// Live bash stdout/stderr line. Two-space indent + dim
			// styling so streamed output reads as subordinate to
			// the "→ bash: cmd" line above it without needing its
			// own header per line.
			r.clearTransient()
			r.writeRaw(styleDim.Render("  " + msg.Event.Summary))
		case "gate":
			r.clearTransient()
			r.writeRaw(formatGateVerdict(msg.Event))
		case "thinking":
			// Planner's per-turn narration. Each event is one
			// complete assistant message (not a delta); fires once
			// per planner turn. Two destinations:
			//
			//   1. Flush the full text into scrollback dimmed so
			//      the user sees the reasoning flow up near their
			//      question, not buried below the spinner. Long
			//      planner runs (3m+) were rendering with NOTHING
			//      visible in the message area — all the model's
			//      thinking lived in the one-line slot under the
			//      timer, which scrolls off as it updates.
			//   2. Keep the latest sentence below the spinner as a
			//      live "what's happening right now" indicator.
			//
			// Skip the scrollback flush when the message looks like
			// the final plan JSON emission — the rendered "📋 Plan"
			// block lands separately and already covers the same
			// content. Dumping the raw fenced JSON would duplicate
			// it as dim noise.
			r.clearTransient()
			text := strings.TrimSpace(msg.Event.Summary)
			if text != "" && !looksLikePlanJSON(text) {
				r.write(styleDim.Render(text))
			}
			r.thinkingLine = lastSentence(msg.Event.Summary)
			r.renderSpinner()
			return r, nil
		case "provider_state":
			// Real HTTP/SSE call state from the provider layer.
			// Stash the latest; renderSpinner formats it below
			// the spinner glyph.
			r.providerState = msg.Event
			r.providerStateAt = msg.Event.When
			// First-byte timestamp: the first streaming-phase event
			// of the turn is the moment tokens started flowing. Match
			// on Summary contents ("streaming") but exclude "stream
			// idle" which is a different phase that means "no bytes
			// recently." The Summary may be tagged with the spawn
			// name (e.g. "agent-foo: streaming"), so use Contains
			// rather than HasPrefix.
			if r.turnFirstResponseAt.IsZero() &&
				strings.Contains(msg.Event.Summary, "streaming") &&
				!strings.Contains(msg.Event.Summary, "idle") {
				when := msg.Event.When
				if when.IsZero() {
					when = time.Now()
				}
				r.turnFirstResponseAt = when
			}
			r.renderSpinner()
			return r, nil
		case "mode_route":
			// One-line notice that auto-routing picked a non-coding
			// mode. Dim styling so it sits as metadata, not as
			// content — the actual run output follows immediately
			// below it.
			r.write(styleDim.Render(msg.Event.Summary))
			return r, nil
		case "verify_start":
			// Auto-verify pass kicked off by the orchestrator after
			// a debug-mode agent applied edits. Render a clearly
			// labeled separator so the user can tell verify activity
			// from the original agent's output.
			r.clearTransient()
			r.write(stylePlannerBanner.Render("─ verify ─"))
			if msg.Event.Summary != "" {
				r.write(styleDim.Render("  re-running the original failing command to confirm the fix held"))
			}
			r.historyVP.GotoBottom()
			return r, nil
		case "test_skipped":
			// Auto-test pass considered the run but found no test
			// convention (no Makefile `test:` target, no
			// scripts.test, etc.). Surface the skip — silence here
			// looks like a bug ("did test pass run? did it pass?").
			r.write(styleDim.Render("⊘ test pass skipped — no test convention detected (Makefile test:, package.json scripts.test, go.mod, Cargo.toml)"))
			return r, nil
		case "verify_end":
			// Closing marker for the verify block. The summary
			// (✓ verified / ⚠ remaining issues / etc.) is emitted
			// separately by the orchestrator via assistant_text or
			// the run's VerifySummary field surfaced at planner-end
			// time — this just closes the visual block.
			r.write(stylePlannerBanner.Render("─ end verify ─"))
			return r, nil
		case "file_confirm":
			ev := msg.Event
			// Session-wide allow flag. If the user already said
			// "yes to all writes" once, auto-approve and skip the
			// prompt entirely.
			if r.fileWritesAllowed {
				if ev.Reply != nil {
					select {
					case ev.Reply <- true:
					default:
					}
				}
				return r, nil
			}
			// Auto-cancel any prior pending file ask (rare —
			// writes serialize, but defensive).
			if r.pendingFileConfirm != nil && r.pendingFileConfirm.Reply != nil {
				select {
				case r.pendingFileConfirm.Reply <- false:
				default:
				}
			}
			r.pendingFileConfirm = &ev
			who := ev.SpawnName
			if who == "" {
				who = "agent"
			}
			verb := ev.Op
			if verb == "" {
				verb = "write"
			}
			r.write(stylePlannerBanner.Render("✏ " + who + " wants to " + verb + ":"))
			delta := ""
			switch {
			case ev.Op == "create":
				delta = fmt.Sprintf(" (%d lines)", ev.Added)
			case ev.Added > 0 && ev.Removed > 0:
				delta = fmt.Sprintf(" (+%d -%d)", ev.Added, ev.Removed)
			case ev.Added > 0:
				delta = fmt.Sprintf(" (+%d)", ev.Added)
			case ev.Removed > 0:
				delta = fmt.Sprintf(" (-%d)", ev.Removed)
			}
			r.writeRaw(styleDim.Render("    ") + ev.Path + styleDim.Render(delta))
			r.historyVP.GotoBottom()
			return r, nil
		case "bash_confirm":
			// Agent wants to run a bash command. The header + command
			// go to scrollback (audit trail — visible after the run
			// completes); the [y]/[n] hint lives in the transient
			// live region (rendered by View() while pendingBashConfirm
			// is non-nil), so once the user answers and we clear
			// pendingBashConfirm, the hint disappears with no leftover
			// "[y] run / [n] cancel" line cluttering scrollback.
			//
			// Multiple back-to-back asks would trample pendingBashConfirm —
			// auto-cancel any prior outstanding ask first.
			if r.pendingBashConfirm != nil && r.pendingBashConfirm.Reply != nil {
				select {
				case r.pendingBashConfirm.Reply <- false:
				default:
				}
			}
			ev := msg.Event
			// Hands-off mode: every bash command auto-approves —
			// destructive ones included, since an unattended run
			// can't answer a prompt and would otherwise hang. The
			// command still goes to scrollback as an audit trail,
			// and the resulting integration is still gated for the
			// user's final approve.
			if r.allowAllBash {
				if ev.Reply != nil {
					select {
					case ev.Reply <- true:
					default:
					}
				}
				r.write(styleDim.Render("    auto-allowed (hands-off mode): " + clipForQueue(ev.Summary)))
				return r, nil
			}
			// Session allowlist short-circuit. If the first token
			// matches a pattern the user previously approved with
			// [a], auto-reply true and don't render the prompt.
			// Same path as the static allowlist in BashTool, just
			// scoped to this REPL session's runtime additions.
			//
			// Carve-out: a Warning on the event signals a destructive
			// command (rm/git rm reached the approval prompt because
			// the BashTool's mustReachApproval guard fired). Skip the
			// auto-allow even if the first token is on the session
			// allowlist — the user must explicitly re-confirm each
			// destructive command.
			if ev.Warning == "" {
				if first := firstShellToken(ev.Summary); first != "" && r.bashAllowlist[first] {
					if ev.Reply != nil {
						select {
						case ev.Reply <- true:
						default:
						}
					}
					r.write(styleDim.Render(fmt.Sprintf("    auto-allowed (matched %q from session allowlist)", first+" *")))
					return r, nil
				}
			}
			r.pendingBashConfirm = &ev
			who := ev.SpawnName
			if who == "" {
				who = "agent"
			}
			r.write(stylePlannerBanner.Render("🔒 " + who + " wants to run:"))
			if ev.Warning != "" {
				r.write(styleError.Render("    ⚠ destructive: " + ev.Warning))
			}
			// Multi-line commands (heredocs, &&-chains broken across
			// lines) need every line indented, not just the first.
			// Otherwise continuation lines hug the left margin and
			// read as prose.
			r.writeRaw(formatCommandBlock(ev.Summary))
			// Force the scrollback to the bottom even if the user
			// had scrolled up reading earlier output. Interactive
			// prompts MUST be visible — the agent is blocked
			// waiting for a y/n, so a hidden prompt deadlocks the
			// run.
			r.historyVP.GotoBottom()
			return r, nil
		case "delta":
			// Streaming assistant text. Each delta passes through
			// the step parser, which strips STEPS:/STEP_DONE:
			// markers and emits parser events for the checklist.
			// Whatever the parser hands back as `Display` lands in
			// streamBuf — that's what the developer reads.
			//
			// Drop late deltas that arrive after the run was
			// finalized — see streamClosed's field comment.
			if r.streamClosed {
				return r, nil
			}
			r.clearTransient()
			ev := r.stepParser.Feed(msg.Event.Delta)
			if len(ev.Block) > 0 {
				tp := NewTaskProgress("", ev.Block)
				r.taskProgress = &tp
				// Auto-start step 0 — declaring the block implies
				// the model is about to begin.
				r.taskProgress.StartStep(0, r.tokenTargetIn)
			}
			if ev.DoneIdx >= 0 && r.taskProgress != nil {
				r.taskProgress.FinishStep(ev.DoneIdx, r.tokenTargetIn)
				next := ev.DoneIdx + 1
				if next < len(r.taskProgress.Steps) {
					r.taskProgress.StartStep(next, r.tokenTargetIn)
				}
			}
			if ev.Display != "" {
				// Append the chunk verbatim. Bullet decoration is
				// applied at render time by bulletParagraphs in the
				// view layer — prepending here was a regression that
				// produced "● Hello● world" when the model streamed
				// in multiple chunks (TestStreamingDelta_NoCopyPanic
				// pinned this; pin restored May 2026).
				r.streamBuf += ev.Display
			}
			r.streamActive = true
			return r, nil
		case "executor_heartbeat":
			// Silent ticker from runExecute. The unconditional
			// r.lastActivity = time.Now() above already did its
			// job (keeps stuck-hint quiet during long-but-
			// legitimate executor work). DO NOT fall through to
			// the default branch — that would write "executing…"
			// to scrollback every 5s, which v0.32.27 accidentally
			// did until the 2026-05-26 dogfood screenshot caught
			// it. The heartbeat is intentionally invisible; the
			// existing "↑ connected · HTTP 200 · Ns" provider-
			// state line already shows the user that work is
			// happening.
			return r, nil
		default:
			// Render dim so it sits in the background of the
			// conversation. Cleared transient first so any pending
			// status line ("planning…") doesn't pin above.
			r.clearTransient()
			// Flush any in-flight streamed text into scrollback as
			// its own paragraph BEFORE writing the activity line.
			// Without this, multi-turn agent runs concatenate every
			// "Now let me look at..." into one wall of text — each
			// turn's prose gets appended to streamBuf with no
			// separator. Flushing on every tool/event boundary
			// keeps each model "thought" visually distinct.
			r.flushStreamSegment()
			// Bash dispatch indicator gets a two-tone render: the
			// "→ bash:" prefix dim (it's metadata), the command
			// itself in white+bold so the user can actually read
			// what's about to run. The plain styleDim render below
			// covers everything else (file ops, kai_* tools, etc.)
			// where the summary is short enough that dim is fine.
			summary := msg.Event.Summary
			if strings.HasPrefix(summary, "→ bash: ") {
				r.writeRaw(styleDim.Render("→ bash: ") +
					styleBashCommand.Render(strings.TrimPrefix(summary, "→ bash: ")))
			} else {
				r.write(styleDim.Render(summary))
			}
			// Re-render the token line below the activity entry so
			// the live counter stays as the bottom-most line.
			if r.tokenAnimating || r.tokenShownIn > 0 || r.tokenShownOut > 0 {
				r.renderTokenLine()
			}
			return r, nil
		}

	case spinner.TickMsg:
		// Advance the spinner frame, repaint the transient line, and
		// schedule the next tick — but only while there's still work
		// in flight. When planning/executing flip false (PlanReadyMsg
		// or ExecuteDoneMsg cleared them), we don't re-arm and the
		// animation idles.
		var sCmd tea.Cmd
		r.spinner, sCmd = r.spinner.Update(msg)
		if r.planning || r.executing || r.gateReviewing {
			r.renderSpinner()
			// Idle-escalation watchdog: if the agent has emitted no
			// activity for autoEscalateAfter AND we haven't already
			// escalated this turn, force a kai_consult call to
			// break the loop. The 2026-05-25 dogfood pinned the
			// need: DeepSeek-V4-Pro spent 11+ minutes reasoning
			// about an over-escaped Svelte template, reaching for
			// encoding/preprocessor explanations instead of
			// recognizing the simple over-escape. A stronger
			// model (or a different family) often breaks that
			// loop in one shot. Threshold of 4 min is past one
			// full reasoning-cycle (DeepSeek's longest observed
			// turn was 4m52s) but well before the user's
			// patience expires.
			if escCmd := r.maybeAutoEscalate(); escCmd != nil {
				return r, tea.Batch(sCmd, escCmd)
			}
			return r, sCmd
		}
		return r, nil

	case tokenTickMsg:
		// Drop stale ticks: the PlanReadyMsg handler resets target to
		// 0 once the reply lands, so any in-flight tick scheduled
		// before the reply arrived would otherwise render a phantom
		// "0 in / 0 out" line below the real one.
		if r.tokenTargetIn == 0 && r.tokenTargetOut == 0 && r.tokenTargetCached == 0 {
			r.tokenAnimating = false
			return r, nil
		}
		r.tokenShownIn = stepToward(r.tokenShownIn, r.tokenTargetIn)
		r.tokenShownOut = stepToward(r.tokenShownOut, r.tokenTargetOut)
		r.tokenShownCached = stepToward(r.tokenShownCached, r.tokenTargetCached)
		r.renderTokenLine()
		// Schedule another frame if there's still distance to cover.
		// When shown matches target, idle — the next OnTurnComplete
		// re-arms us by setting tokenAnimating in the tokens case.
		if r.tokenShownIn != r.tokenTargetIn ||
			r.tokenShownOut != r.tokenTargetOut ||
			r.tokenShownCached != r.tokenTargetCached {
			return r, scheduleTokenTick()
		}
		r.tokenAnimating = false
		return r, nil

	case PlanReadyMsg:
		r.planning = false
		r.cancelRequestedAt = time.Time{}
		r.spinnerText = ""
		r.clearTransient()
		// Close the stream regardless of which branch we take below.
		// The Err and Plan branches don't write rendered markdown,
		// but they still need late deltas (channel stragglers) to
		// stop landing in View.
		r.finalizeStream()
		// Leading bookend on every result path. Without this the
		// planner's exploration text trails off into a token
		// trailer and looks like the run "stopped by mistake."
		// One short banner makes it unambiguous: the run finished,
		// what's below is the final answer.
		// (removed "─ planner finished ─" banner — it framed every
		// turn as a milestone but added more visual chrome than
		// signal. The diagnosis/approach/plan blocks below carry
		// their own headers; the run end is marked by the token
		// trailer line.)
		switch {
		case msg.Err != nil:
			// Critic-retry safety net. If this error is the outcome of
			// an auto-retry WE dispatched (criticRetryCount>0), don't
			// strand the user with an error and no answer — we retracted
			// a usable answer to make room for the retry. Log/telemeter
			// the failure (it's a real provider error) but RESTORE the
			// prior answer instead of surfacing the error. The 2026-05-31
			// repro: a reasoning chat model returned no text on the
			// heavy critic-retry turn, and the run errored after the
			// good first answer had already been retracted.
			if r.criticRetryCount > 0 && strings.TrimSpace(r.retractedAnswer) != "" {
				restored := r.retractedAnswer
				r.retractedAnswer = ""
				r.criticRetryCount = 0
				r.inFlightPendingAction = ""
				ue := errpkg.Classify(msg.Err)
				errpkg.Report(workspaceFor(r.services), ue, false)
				r.write(styleWarn.Render("  ↻ retry failed (" + ue.Headline + ") — keeping the prior answer:"))
				r.writeMarkdown(restored)
				return r, nil
			}
			// Auto-promote hidden tool calls on failure. When a run
			// fails — context deadline, classifier-flagged error,
			// upstream timeout — the buffered tool-event lines are
			// the most important debug artifact (they show exactly
			// what the agent tried before giving up). Burying them
			// behind a /verbose toggle the user can only flip BEFORE
			// the failed run wasted their time is hostile. Promote
			// them now, before the friendly error text, so the user
			// reads "the agent did A, B, C, then errored" instead
			// of just "kai errored."
			r.flushSuppressedToolLines("× run errored — restoring hidden tool calls:")
			// Every error goes through the central classifier
			// (internal/tui/errors). The classifier translates
			// raw upstream/internal error text into a friendly
			// UserError with a clean headline, optional detail,
			// and concrete next-step action. The raw form is
			// logged to .kai/errors.log AND reported to PostHog
			// telemetry (when enabled) so the kai team sees what
			// breaks without users having to file issues.
			//
			// Hardcoding "planner: <err.Error()>" here was the
			// pattern that leaked the May-5 "object store is
			// missing blobs the snapshot references" message to
			// the user. The classifier prevents recurrence: even
			// unknown errors render as "Something unexpected —
			// run kai diagnose" instead of dumping internals.
			ue := errpkg.Classify(msg.Err)
			errpkg.Report(workspaceFor(r.services), ue, false)
			r.write(errpkg.Render(ue))
			// Same planner-session memory wiring as the
			// execute-side path — see ExecuteDoneMsg handler.
			// Planner failures DURING planning (e.g. the LLM
			// returned malformed output) also benefit: the
			// next turn picks up the prior failure and won't
			// loop on the same dead end. The recorder itself
			// filters out auto-repairable infrastructure kinds
			// (missing_blobs, no_snapshots) so callers don't
			// have to remember to gate.
			if r.services != nil {
				recordExecuteFailureForPlanner(r.services.OrchestratorCfg.AgentSessionStore, msg.SessionID, ue)
			}
			// Auto-repair: if the classifier tagged this as a
			// recoverable failure with a known fix path, kick
			// off the recovery in the background. The
			// "Reindexing…" line in classify.go promised this
			// happens; this is the wiring that makes it true.
			var repairCmd tea.Cmd
			if cmd := runAutoRepair(r.services, ue.Kind); cmd != nil {
				r.transient = styleDim.Render("· " + ue.Detail)
				repairCmd = cmd
			}
			// Mode 1+ fixxy: any Block-severity error fires a
			// claude self-heal in the background. Lower
			// severities don't trigger (would spam claude on
			// info-level recovered hiccups). The worker is
			// nil when --fixxy-upper wasn't passed; Trigger
			// is nil-safe via the worker's own guard.
			if r.services != nil && r.services.Fixxy != nil &&
				r.services.FixxyMode >= fixxy.M1 && ue.Severity == errpkg.Block {
				tail := fixxy.ReadErrorLogTail(r.services.KaiDir, 8192)
				prompt := fixxy.BuildErrorPrompt(ue.Kind, ue.Headline, ue.LogContext, tail)
				r.services.Fixxy.Trigger("error: "+ue.Kind, prompt, r.services.FixxyMode)
			}
			r.pendingPlan = nil
			if repairCmd != nil {
				return r, repairCmd
			}
			// Persist planner session id even on error so the user
			// can re-ask in the same conversation thread.
			if msg.SessionID != "" {
				r.sessionID = msg.SessionID
			}
		case msg.ChatReply != "":
			// Conversational fallback: the request wasn't a planable
			// change, so render the model's chat reply inline. The
			// reply is markdown (lists, bold, code spans) — feed it
			// through glamour so it renders in the terminal style.
			if msg.SessionID != "" {
				r.sessionID = msg.SessionID
			}
			if msg.SessionID != "" {
				r.sessionID = msg.SessionID
			}
			// Drop any in-flight streaming text first; we're about
			// to replace it with the glamour-rendered final.
			r.finalizeStream()
			// Drop the live counter line before printing the reply so
			// it doesn't sit awkwardly above the assistant's text.
			r.clearTransient()
			r.writeMarkdown(msg.ChatReply)
			// Permission-handshake pre-arm. When the chat agent ends
			// its reply with the exact offer trailer (instructed by
			// the conversation-mode prompt for change-proposing
			// turns), set forcedMode=Coding for the next turn. A
			// follow-up "yes" then hits the short-affirmative branch
			// at planner_dispatch.go:1482 which routes to the chat
			// agent in coding mode WITH the prior session loaded,
			// so the model actually applies the change it just
			// offered. Without this, "yes" routes to conversation
			// mode (read-only tools) and the chat agent can't act.
			// Trailer string is exact-match deterministic (not
			// regex-based intent detection) — the prompt instructs
			// the model to emit this literal line when it proposes
			// an actionable change.
			if pa := extractPendingAction(msg.ChatReply); pa != nil {
				r.pendingAction = pa
				r.forcedMode = agent.ModeCoding
			}
			// Host-task fast path with a known command. Stash it
			// and render an approval prompt; the key handler at the
			// top of Update picks up y/n on the next keystroke.
			if msg.HostCommand != "" {
				r.pendingHostCommand = msg.HostCommand
				r.write("")
				r.write(styleDim.Render("    " + msg.HostCommand))
				r.write("")
				r.write(styleDim.Render("    [y]es run on host  /  [n]o dismiss"))
			}
			// ONE trailer per user turn. A single user submit can
			// invoke both the planner agent (for vagueness check)
			// AND the chat agent (for the actual reply) — each
			// produces its own token stats. Rendering both
			// produced the May-4 "two trailers, one says 99%
			// cache, one says 0% cache" UX disaster: confusing
			// and contradictory to the user even though both
			// numbers are individually correct.
			//
			// Resolution: sum across paths. Total fresh / total
			// out / total create+read. Combined hit rate gives an
			// honest "what did this user-turn cost overall."
			totalIn := msg.ChatTokensIn + msg.PlannerTokensIn
			totalOut := msg.ChatTokensOut + msg.PlannerTokensOut
			// Chat path doesn't expose the create/read split — it
			// only reports lump `cached` (TokensCached). Treat
			// that lump as cache_read for cost-band purposes:
			// the chat agent typically inherits a primed cache
			// from the planner that just ran, so READ dominates.
			// This understates create slightly when chat actually
			// wrote new cache, but the impact is small (chat's
			// cache contribution is ~10% of planner's typically)
			// and the alternative of double-counting was worse.
			totalCreate := msg.PlannerTokensCacheCreate
			totalRead := msg.PlannerTokensCacheRead + msg.ChatTokensCached
			if totalIn > 0 || totalOut > 0 {
				cost := estimateCost(totalIn, totalOut, totalCreate, totalRead)
				r.sessionCostUSD += cost
				r.sessionTurns++
				r.runOutputTokens += totalOut
				// Apples-to-apples Claude-Code-style line: same
				// "(elapsed · ↓ tokens)" shape so users moving
				// between tools can compare directly.
				r.write(styleDim.Render(formatRunSummary(r.runStart, r.turnFirstResponseAt, r.runOutputTokens)))
				r.write(styleDim.Render(formatTokensSplit(totalIn, totalOut, totalCreate, totalRead) +
					formatSessionTotal(r.sessionCostUSD, r.sessionTurns)))
				r.maybeRenderDailyCapWarning()
			}
			// Suppressed-tool-events footer: when quiet mode hid
			// per-call detail from scrollback this turn, surface the
			// count so the user knows it happened and can opt into
			// detail next time via /verbose.
			if !r.verboseTools && r.suppressedToolEvents > 0 {
				s := ""
				if r.suppressedToolEvents != 1 {
					s = "s"
				}
				r.write(styleDim.Render(fmt.Sprintf(
					"  · %d tool call%s hidden — /verbose to show next turn",
					r.suppressedToolEvents, s,
				)))
			}
			// Record this turn for fixxy modes 2/3. Mode 2
			// uses the ring as feedback context; mode 3
			// reviews each turn directly. Always recording
			// (even when fixxy is off) keeps the ring warm
			// in case the user enables fixxy mid-session
			// via env (future feature).
			r.recordTurn(msg.Request, msg.ChatReply)
			// Mode 3 fixxy: every completed turn ships to
			// claude for review. Claude either says "looks
			// fine" (most turns) or fixes kai's behavior.
			// Skipped if a Block error already triggered
			// fixxy in the same turn — would double-spawn.
			if r.services != nil && r.services.Fixxy != nil &&
				r.services.FixxyMode >= fixxy.M3 && msg.Err == nil {
				prompt := fixxy.BuildReviewPrompt(msg.Request, msg.ChatReply, "")
				r.services.Fixxy.Trigger("post-turn review", prompt, r.services.FixxyMode)
			}
			// Reset counters so the next chat turn animates from 0.
			r.tokenShownIn, r.tokenShownOut, r.tokenShownCached = 0, 0, 0
			r.tokenTargetIn, r.tokenTargetOut, r.tokenTargetCached = 0, 0, 0
			r.tokenAnimating = false
			r.pendingPlan = nil
			// Mark the trailer as already rendered so any late
			// `tokens` event from the agent's last
			// OnTurnComplete (racing with this PlanReadyMsg)
			// gets dropped instead of restarting the tween and
			// rendering an orphan transient line below "─ end ─".
			r.trailerRendered = true
			// Kick off the satisfaction-gate critic. Runs
			// asynchronously after the trailer prints; verdict
			// arrives later as a CriticReadyMsg the REPL handles
			// separately. Skipped on chat replies short enough
			// that they're definitionally a one-shot lookup
			// (vocab / "what is X" / yes-no). Threshold: 80
			// chars — calibrated to catch substantive answers
			// while letting trivial responses pass without
			// burning a critic call.
			// Skip critic on trivial-action / host-command turns. The
			// proposal text ("Trivial action recognized... proposing
			// host command") trips the >=80-char threshold and looks
			// like a substantive reply, but the actual DELIVERABLE
			// is the host command kai is about to execute on user
			// approval. The critic, seeing only the proposal text,
			// reliably classifies it as "agent only proposed, didn't
			// execute" — false. 2026-05-25 dogfood showed this firing
			// repeatedly on every "can you run it" turn. The host-
			// command path has its own end-to-end visibility now via
			// the v0.31.45 auto-error feedback; a separate critic on
			// top is noise.
			if msg.Err == nil && msg.HostCommand == "" && len(strings.TrimSpace(msg.ChatReply)) >= 80 {
				// Fast-path: cheap pre-critic detector for generic-
				// opening replies to workspace questions. When the
				// model has clearly bypassed the workspace context
				// (started with "Most coding agents...", etc.), skip
				// the LLM critic round-trip and synthesize a FAIL
				// CriticReadyMsg directly. Saves 5-15s of latency on
				// the obvious failures the dogfoods kept surfacing.
				// See forbiddenGenericOpenings in critic.go.
				if canned := detectGenericOpening(msg.ChatReply, msg.Request); canned != "" {
					return r, func() tea.Msg {
						return CriticReadyMsg{
							OriginalRequest: msg.Request,
							Pass:            false,
							Critique:        canned,
							RetryHint:       "Here's what I'm doing now, in this turn: re-reading the file:line evidence I already collected and writing the answer FROM those concrete findings instead of from generic patterns.",
						}
					}
				}
				// Stash the answer under review so a failed auto-retry
				// can restore it instead of leaving the user with an
				// error and nothing.
				r.retractedAnswer = msg.ChatReply
				if criticCmd := runCritic(r.services, msg.Request, msg.ChatReply, msg.SessionID); criticCmd != nil {
					// Visible loading state — the critic call has
					// latency (one LLM round-trip with a reasoning
					// model on top). Without this line the user sees
					// the agent's reply land, then dead air, then a
					// critic message popping out of nowhere. The dim
					// line clears when CriticReadyMsg arrives (the
					// next message overwrites it visually via the
					// scroll).
					r.write(styleDim.Render("· reviewing the reply…"))
					return r, criticCmd
				}
			}
		case msg.Plan == nil:
			r.write(styleError.Render("planner: empty result"))
			r.pendingPlan = nil
		default:
			r.pendingPlan = msg.Plan
			r.originalReq = msg.Request
			r.write(formatPlan(msg.Plan))
			// Record this Plan-path turn for the fixxy ring
			// too. Without this, "no sir i don't like it"
			// after a Plan response sees an empty ring and
			// fixxy bails with "no recent turns to review."
			// Use the plan's summary as the "reply" since
			// that's what the user actually saw rendered.
			if msg.Plan != nil {
				r.recordTurn(msg.Request, msg.Plan.Summary)
			}
			if msg.SessionID != "" {
				r.sessionID = msg.SessionID
			}
			if msg.PlannerTokensIn > 0 || msg.PlannerTokensOut > 0 {
				// Mirror the chat-reply trailer: run summary
				// (claude-code-comparable) above, verbose
				// trailer + session total below, daily-cap
				// warning if applicable. Without these the
				// planner-only path renders a stripped-down
				// trailer that's missing the at-a-glance
				// "(time · ↓ tokens)" line and the session
				// total — a UX inconsistency the user noticed
				// immediately when "hi" produced an empty plan.
				cost := estimateCost(msg.PlannerTokensIn, msg.PlannerTokensOut,
					msg.PlannerTokensCacheCreate, msg.PlannerTokensCacheRead)
				r.sessionCostUSD += cost
				r.sessionTurns++
				r.runOutputTokens += msg.PlannerTokensOut
				r.write(styleDim.Render(formatRunSummary(r.runStart, r.turnFirstResponseAt, r.runOutputTokens)))
				r.write(styleDim.Render(formatTokensSplit(
					msg.PlannerTokensIn, msg.PlannerTokensOut,
					msg.PlannerTokensCacheCreate, msg.PlannerTokensCacheRead) +
					formatSessionTotal(r.sessionCostUSD, r.sessionTurns)))
				r.maybeRenderDailyCapWarning()
			}
			// Same race guard as the chat-reply branch — drop
			// late `tokens` events so they don't re-animate
			// the transient below "─ end ─".
			r.trailerRendered = true
			// Open the action menu only when there's actually
			// something to act on. Empty-agents plans (the
			// "Already done — nothing to do" case) get the
			// formatted headline but no go/cancel/feedback —
			// none of those make sense when there's no work.
			if msg.Plan != nil && len(msg.Plan.Agents) > 0 {
				if msg.AutoRun || msg.Plan.Trivial {
					// Triage quick track (AutoRun), or a plan the
					// planner judged trivial — execute immediately,
					// no go/cancel/feedback confirm. Mirrors the
					// "go" branch of dispatchPlanChoice.
					r.executing = true
					r.spinnerText = pickSpinnerPhrase()
					r.spinner.Spinner = pickSpinnerStyle()
					r.clearTransient()
					r.planChoice = -1
					r.renderSpinner()
					return r, tea.Batch(runExecute(r.services, msg.Plan), r.spinner.Tick)
				}
				r.planChoice = 0
				// Hands-off mode: skip the go/cancel/feedback menu
				// and dispatch the "go" choice immediately — the
				// same path the Enter key takes. The user opted
				// out of confirming each plan; they still get the
				// final gate approve.
				if r.services != nil && r.services.HandsOff {
					r.write(styleDim.Render("hands-off: auto-confirming plan"))
					var cmd tea.Cmd
					r, cmd = r.dispatchPlanChoice()
					return r, cmd
				}
				r.renderPlanMenu()
			} else {
				// Clear pendingPlan so the next user input
				// doesn't get treated as feedback on a non-
				// existent plan.
				r.pendingPlan = nil
				r.planChoice = -1
			}
		}
		// (removed trailing "─ end ─" banner together with the
		// "─ planner finished ─" header. The token-trailer line
		// already marks "the run is over" implicitly; double-
		// banner framing was visual chrome.)
		return r, nil

	case CriticReadyMsg:
		// Satisfaction-gate critic landed. Display in light
		// gray on both PASS and FAIL — the critic is a soft
		// signal, not an error. A red "× critic:" line read
		// as a failure-of-kai when it's actually feedback on
		// kai's WORK. Same dim weight as the trailer keeps
		// it in the right register.
		// On Err: silently drop — a critic transport failure
		// shouldn't surface noise; the agent's reply was
		// already rendered.
		if msg.Err != nil {
			return r, nil
		}
		if msg.Pass {
			// Render the critique as a self-acknowledgment. The
			// critic now writes in kai's first-person voice — no
			// "✓ critic:" prefix, just the message itself in dim
			// so it reads as kai confirming its own work.
			r.write(styleDim.Render("· " + strings.TrimSpace(msg.Critique)))
			r.criticRetryCount = 0
			r.inFlightPendingAction = ""
			return r, nil
		}
		// FAIL: render the critique as kai's self-correction. The
		// prompt has the critic write in first person ("my last
		// response..."), so this reads as kai catching itself and
		// pivoting — not an external judge slapping it down. The
		// retry hint, when present, is a continuation of the same
		// self-message ("Here's what I'm doing now, in this turn:
		// ...") and renders as a follow-on paragraph.
		// Auto-retry fires immediately. Retry cap prevents an
		// infinite critic-retry loop on a critic that keeps
		// failing the agent's output (the agent may genuinely
		// be unable to satisfy the request). After the cap,
		// surface the final self-message and stop.
		r.write(styleDim.Render("· " + msg.Critique))
		if strings.TrimSpace(msg.RetryHint) != "" {
			r.write(styleDim.Render("  " + msg.RetryHint))
		}
		// User-priority guard: if the user has already moved on —
		// either typed a new prompt that's now in flight (IsBusy)
		// or has text waiting in the input buffer — DO NOT
		// auto-dispatch the retry. The retry's runPlan would call
		// armCancel, which trips the previous run's cancel func
		// (the user's typed prompt), killing it mid-flight with a
		// `· Cancelled` line that the user didn't trigger.
		// 2026-05-25 transcript pinned this: user typed "what is
		// the latest svelte?" right after a long agent response,
		// the critic-fail landed milliseconds later, and the
		// auto-retry murdered the user's question.
		userMovedOn := r.IsBusy() || strings.TrimSpace(r.input.Value()) != ""
		if userMovedOn {
			r.write(styleDim.Render("  skipping auto-retry — you've already moved on. The critique above stands as-is."))
			r.criticRetryCount = 0
			return r, nil
		}
		if r.criticRetryCount >= criticMaxRetries {
			r.write(styleDim.Render(fmt.Sprintf("  retry cap (%d) reached — stopping. Refine the request and try again.", criticMaxRetries)))
			r.criticRetryCount = 0
			r.inFlightPendingAction = ""
			return r, nil
		}
		r.criticRetryCount++
		// Retract the failed turn before dispatching the retry so the
		// model isn't doubling down on its own bad answer:
		//   1. Drop the failed assistant turn (+ tool messages) from
		//      session history. Retry sees the original user request
		//      followed by the new "Prior attempt critique" prompt,
		//      not the answer the critic just rejected.
		//   2. Trim the in-program transcript so /copy response and
		//      similar surfaces don't ship the retracted text.
		//   3. (opt-in) Try an ANSI overwrite of the scrollback lines
		//      we just printed. Best-effort — once the bad answer has
		//      scrolled past the viewport, no terminal will let us
		//      reach back for it. Gated on KAI_CRITIC_RETRY_OVERWRITE
		//      until the cosmetic side has been dogfooded enough.
		r.retractFailedTurn()
		r.write(styleDim.Render("  ↻ retracted the prior answer from the retry's context"))
		retry := &pendingCriticRetry{
			originalRequest:   msg.OriginalRequest,
			critique:          msg.Critique,
			retryHint:         msg.RetryHint,
			pendingActionText: r.inFlightPendingAction,
		}
		retryPrompt := retry.retryPrompt()
		r.write(styleDim.Render(fmt.Sprintf("↻ auto-retrying (%d/%d) with critique appended", r.criticRetryCount, criticMaxRetries)))
		next, cmd := r.dispatch(retryPrompt)
		return next, cmd

	case ExecuteDoneMsg:
		r.executing = false
		r.cancelRequestedAt = time.Time{}
		r.spinnerText = ""
		r.clearTransient()
		// Errors here have to go through the classifier
		// before reaching scrollback. The May-5 orchestrator
		// preflight leak ("object store is missing blobs the
		// snapshot references — run kai capture to rebuild")
		// hit this path: formatExecuteResult was rendering
		// msg.Err.Error() verbatim, bypassing classify and
		// telemetry. Now any execute-side error gets the same
		// friendly headline + auto-repair routing as the
		// planner side. Successful results still go through
		// formatExecuteResult.
		var repairCmd tea.Cmd
		if msg.Err != nil {
			ue := errpkg.Classify(msg.Err)
			errpkg.Report(workspaceFor(r.services), ue, false)
			r.write(errpkg.Render(ue))
			// Feed the failure back into the planner session
			// so the next plan request doesn't blindly re-
			// propose the same agents. Without this, the user
			// sees "go → fail → re-prompt → same plan" loops
			// (reported May-2026: "third time it happened").
			if r.services != nil {
				recordExecuteFailureForPlanner(r.services.OrchestratorCfg.AgentSessionStore, r.sessionID, ue)
			}
			// Auto-repair (see planner-side comment for
			// rationale). Same wiring on this path so a
			// missing-blobs preflight failure during execute
			// also triggers the background reindex.
			if cmd := runAutoRepair(r.services, ue.Kind); cmd != nil {
				r.transient = styleDim.Render("· " + ue.Detail)
				repairCmd = cmd
			}
			// Mode 1+ fixxy: same Block-severity gate as
			// the planner side.
			if r.services != nil && r.services.Fixxy != nil &&
				r.services.FixxyMode >= fixxy.M1 && ue.Severity == errpkg.Block {
				tail := fixxy.ReadErrorLogTail(r.services.KaiDir, 8192)
				prompt := fixxy.BuildErrorPrompt(ue.Kind, ue.Headline, ue.LogContext, tail)
				r.services.Fixxy.Trigger("error: "+ue.Kind, prompt, r.services.FixxyMode)
			}
		} else {
			r.write(formatExecuteResult(msg.Result, nil))
		}
		r.pendingPlan = nil
		r.originalReq = ""
		return r, repairCmd
	}

	var inCmd tea.Cmd
	priorLines := r.input.LineCount()
	priorValue := r.input.Value()
	r.input, inCmd = r.input.Update(msg)
	if r.input.LineCount() != priorLines && r.width > 0 && r.height > 0 {
		r.SetSize(r.width, r.height)
	}
	// Recompute slash-command suggestions whenever the input text
	// changed. Cheap (linear scan over a small static list) and
	// keeps the popup in sync without a separate change-detection
	// hook inside the textarea.
	if r.input.Value() != priorValue {
		r.refreshSuggestions()
	}
	return r, inCmd
}

// flushPrints emits any pendingPrints entries past flushedTo via
// tea.Println, then advances the cursor. The slice itself isn't
// drained — see the field comment for why. Returns nil when
// there's nothing new so callers can blend it through tea.Batch
// without spamming empty commands.
//
// Called from the defer at the top of Update so every return path
// flushes, not just the bottom one. (Earlier bug: handlers that
// returned early — PlanReadyMsg with a chat reply, ChatActivityMsg
// — left their writes sitting in the queue until a stray
// keystroke triggered the bottom flush, so replies appeared only
// after the next keypress.)
func (r *REPL) flushPrints() tea.Cmd {
	if r.flushedTo >= len(r.pendingPrints) {
		return nil
	}
	out := strings.Join(r.pendingPrints[r.flushedTo:], "\n")
	r.flushedTo = len(r.pendingPrints)

	// Append to the in-program history viewport. Only auto-scroll
	// to the bottom if the user wasn't already scrolled up reviewing
	// earlier content — unless forceScrollNextFlush is set (the user
	// just submitted a new turn and wants to see the prompt + response
	// land in the visible area, even if they'd scrolled up mid-stream).
	wasAtBottom := r.historyVP.AtBottom()
	if r.historyContent != "" {
		r.historyContent += "\n"
	}
	r.historyContent += out
	r.historyVP.SetContent(r.historyContent)
	if wasAtBottom || r.forceScrollNextFlush {
		r.historyVP.GotoBottom()
		r.forceScrollNextFlush = false
	}
	return nil
}

// refreshSuggestions recomputes the autocomplete list from the
// current input. Three modes:
//
//  1. Top-level: input starts with "/" and contains no space —
//     suggest top-level command names matching the prefix.
//  2. Subcommand: input is `/<cmd> [<prefix>]` (one space, no
//     further whitespace), and <cmd> is in slashSubcommands —
//     suggest matching subcommands.
//  3. None: anything else — clear the suggestion list.
//
// suggestKind tracks which mode is active so acceptSuggestion knows
// whether to replace the whole input or just append the subtoken.
func (r *REPL) refreshSuggestions() {
	v := r.input.Value()
	// File-mention completion: an "@" anywhere in the current line
	// followed by a non-whitespace prefix triggers file autocomplete.
	// Slash-command completion only fires when "/" is at the START
	// of the input (a `/` in the middle of "what's the /tmp/dir
	// like?" shouldn't open a command popup), so the "@" branch
	// goes first — once we've matched it we return without falling
	// through to the slash logic.
	if mention, ok := currentFileMention(v); ok {
		r.refreshFileSuggestions(mention)
		return
	}
	if !strings.HasPrefix(v, "/") {
		r.suggestItems = nil
		r.suggestIdx = 0
		r.suggestKind = suggestKindNone
		return
	}
	// Newlines disable suggestions entirely — multi-line drafts are
	// almost certainly long-form input, not slash commands.
	if strings.ContainsAny(v, "\n") {
		r.suggestItems = nil
		r.suggestIdx = 0
		r.suggestKind = suggestKindNone
		return
	}
	rest := strings.TrimPrefix(v, "/")
	spaceIdx := strings.IndexAny(rest, " \t")
	if spaceIdx < 0 {
		// No space yet — top-level command completion.
		matches := make([]string, 0, len(slashCommands))
		for _, c := range slashCommands {
			if strings.HasPrefix(c, rest) {
				matches = append(matches, c)
			}
		}
		r.suggestItems = matches
		r.suggestKind = suggestKindCommand
		if r.suggestIdx >= len(matches) {
			r.suggestIdx = 0
		}
		return
	}
	// `/<cmd> <stuff>` form. The "stuff" portion may itself
	// contain a space — `/gate approve <id>` is three tokens.
	// Split off the second token to find <sub>; whatever remains
	// after the next space is the <arg> prefix (if any).
	cmd := rest[:spaceIdx]
	stuff := strings.TrimLeft(rest[spaceIdx+1:], " \t")
	subs, ok := slashSubcommands[cmd]
	if !ok {
		r.suggestItems = nil
		r.suggestIdx = 0
		r.suggestKind = suggestKindNone
		return
	}
	// Three-token form: `/<cmd> <sub> <argPrefix>` — try
	// dynamic arg completion if a provider exists for that
	// (cmd, sub) pair.
	if subSpace := strings.IndexAny(stuff, " \t"); subSpace >= 0 {
		sub := stuff[:subSpace]
		argPart := strings.TrimLeft(stuff[subSpace+1:], " \t")
		// Only one arg supported today — kill the popup if the
		// user has typed past the first.
		if strings.ContainsAny(argPart, " \t") {
			r.suggestItems = nil
			r.suggestIdx = 0
			r.suggestKind = suggestKindNone
			return
		}
		provider, hasProvider := slashArgProviders[cmd+" "+sub]
		if !hasProvider {
			r.suggestItems = nil
			r.suggestIdx = 0
			r.suggestKind = suggestKindNone
			return
		}
		argMatches := make([]string, 0)
		for _, a := range provider(r.services) {
			if strings.HasPrefix(a, argPart) {
				argMatches = append(argMatches, a)
			}
		}
		r.suggestItems = argMatches
		r.suggestKind = suggestKindArg
		if r.suggestIdx >= len(argMatches) {
			r.suggestIdx = 0
		}
		return
	}
	subPart := stuff
	matches := make([]string, 0, len(subs))
	for _, s := range subs {
		if strings.HasPrefix(s, subPart) {
			matches = append(matches, s)
		}
	}
	r.suggestItems = matches
	r.suggestKind = suggestKindSubcommand
	if r.suggestIdx >= len(matches) {
		r.suggestIdx = 0
	}
}

// submitLine handles the "user pressed Enter on a non-empty line"
// path: bookkeeping (history append, input reset), per-turn state
// reset (stream gate, step parser), the visual separator + echoed
// prompt line, and the dispatch into runPlan / runChat / shell-out.
//
// fromUser is true for direct Enter presses. Queue advancement
// passes false so a queued item doesn't re-append to history (the
// item already landed in history when it was queued, so a second
// append would duplicate it).
func (r *REPL) submitLine(line string, fromUser bool) (REPL, tea.Cmd) {
	if fromUser {
		r.history = append(r.history, line)
		appendReplHistory(r.workDir, line)
		r.histIdx = -1
	}
	// New run starts: reset the mid-flight task-capture counter and the
	// continue-armed flag (any tasks added previously are already in
	// TASKS.md and will be injected this turn).
	r.tasksAddedThisRun = 0
	r.continueArmed = false
	// New turn — pin the viewport to the bottom on the next flush so
	// the prompt echo + spinner are visible even if the user had
	// scrolled up reviewing earlier output.
	r.forceScrollNextFlush = true
	r.input.Reset()
	r.suggestItems = nil
	r.suggestIdx = 0
	// Reopen the stream gate for the next turn so its deltas land
	// in View again. Reset the step parser + task progress: each
	// user submit starts a fresh checklist.
	r.streamClosed = false
	r.stepParser = stepParser{}
	r.taskProgress = nil
	r.thinkingLine = ""
	// Visual separator between turns. dispatch echoes the prompt
	// line itself.
	r.appendSeparator()
	r.write(replPrompt() + line)
	return r.dispatch(line)
}

// clipForQueue trims a queued prompt for the queue indicator and
// the resume marker. 60 chars matches the spec; anything longer
// gets a single-character ellipsis so the line stays one row.
func clipForQueue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

// visionModel picks the model id used for /image describe calls.
// Override via KAI_VISION_MODEL env, otherwise falls back to the
// vision package's DefaultModel (currently Qwen/Qwen3.5-397B-A17B,
// which is in kailab's KailabTogetherModels allowlist and supports
// image-in / text-out per Together's catalog).
func visionModel(s *PlannerServices) string {
	if v := strings.TrimSpace(os.Getenv("KAI_VISION_MODEL")); v != "" {
		return v
	}
	return vision.DefaultModel
}

// currentFileMention returns the "@<prefix>" token under the user's
// cursor, where the prefix is everything between the last unquoted
// "@" and the cursor position with no intervening whitespace.
// Returns the prefix (without the @) and ok=true when a match
// exists, otherwise ok=false.
//
// We use the END of the input rather than the textarea cursor
// position because the existing slash-completion logic is
// end-cursor-anchored too — keeping them consistent avoids surprise
// when the user moves the cursor mid-word.
func currentFileMention(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	// Walk backward from the end. Stop at whitespace (no mention),
	// at "@" (mention starts here), or at start-of-string.
	end := len(v)
	for i := end - 1; i >= 0; i-- {
		c := v[i]
		if c == '@' {
			// Must be at start of input or preceded by whitespace —
			// otherwise we'd match emails ("user@host"), which
			// would be obnoxious.
			if i == 0 || v[i-1] == ' ' || v[i-1] == '\t' || v[i-1] == '\n' {
				return v[i+1 : end], true
			}
			return "", false
		}
		if c == ' ' || c == '\t' || c == '\n' {
			return "", false
		}
	}
	return "", false
}

// refreshFileSuggestions populates the popup with workspace files
// matching the given prefix. Three-tier match: prefix-on-basename
// scores best, prefix-on-full-path next, substring fallback last.
// Sorted within each tier by path length (shorter = closer to root,
// usually more relevant). Capped at 10 to keep the popup compact.
func (r *REPL) refreshFileSuggestions(prefix string) {
	r.maybeRefreshFileIndex()
	low := strings.ToLower(prefix)
	type scored struct {
		path  string
		score int
	}
	hits := make([]scored, 0, 32)
	for _, p := range r.fileIndex {
		lowP := strings.ToLower(p)
		base := strings.ToLower(filepath.Base(p))
		switch {
		case strings.HasPrefix(base, low):
			hits = append(hits, scored{p, 0})
		case strings.HasPrefix(lowP, low):
			hits = append(hits, scored{p, 1})
		case low != "" && strings.Contains(lowP, low):
			hits = append(hits, scored{p, 2})
		}
	}
	// Stable sort: by score, then by length, then alphabetical.
	// Insertion sort — bounded list, sub-millisecond on 50k files.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0; j-- {
			a, b := hits[j-1], hits[j]
			if a.score < b.score {
				break
			}
			if a.score == b.score && len(a.path) < len(b.path) {
				break
			}
			if a.score == b.score && len(a.path) == len(b.path) && a.path <= b.path {
				break
			}
			hits[j-1], hits[j] = b, a
		}
	}
	const popupCap = 10
	if len(hits) > popupCap {
		hits = hits[:popupCap]
	}
	items := make([]string, 0, len(hits))
	for _, h := range hits {
		items = append(items, h.path)
	}
	r.suggestItems = items
	r.suggestKind = suggestKindFile
	if r.suggestIdx >= len(items) {
		r.suggestIdx = 0
	}
}

// walkProjectsForAutocomplete returns workspace-relative paths under
// each project root, suitable for "@" autocomplete. Skips the usual
// noise dirs (.git, node_modules, .kai, target, dist, build, vendor)
// so the popup doesn't drown in dependency files. In a multi-root
// workspace, paths are prefixed with the project's name to keep
// them disambiguated across roots — same convention the kai_files
// tool uses.
//
// Best-effort: walk errors on individual files are swallowed; a
// blown stat on one entry shouldn't kill the whole index.
func walkProjectsForAutocomplete(s *PlannerServices, fallbackWorkDir string) []string {
	var roots []struct{ name, path string }
	if s != nil && s.Projects != nil {
		for _, p := range s.Projects.Projects() {
			roots = append(roots, struct{ name, path string }{p.Name, p.Path})
		}
	}
	if len(roots) == 0 && fallbackWorkDir != "" {
		roots = append(roots, struct{ name, path string }{"", fallbackWorkDir})
	}
	prefixWithName := len(roots) > 1
	out := make([]string, 0, 1024)
	for _, r := range roots {
		if r.path == "" {
			continue
		}
		_ = filepath.WalkDir(r.path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if path != r.path && autocompleteIgnoreDirs[name] {
					return fs.SkipDir
				}
				return nil
			}
			// Skip dotfiles at any depth — autocomplete is for the
			// project's source, not the .DS_Store / .gitkeep noise.
			if strings.HasPrefix(name, ".") {
				return nil
			}
			rel, err := filepath.Rel(r.path, path)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if prefixWithName && r.name != "" {
				rel = r.name + "/" + rel
			}
			out = append(out, rel)
			if len(out) >= fileIndexCap {
				return fs.SkipAll
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// autocompleteIgnoreDirs is the noise filter for the @-autocomplete
// walker. Mirrors the same set kai_files uses (.git, node_modules,
// vendor, etc.) so what the popup shows lines up with what the
// agent's tools see — no surprise gaps.
var autocompleteIgnoreDirs = map[string]bool{
	".git":         true,
	".kai":         true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"__pycache__":  true,
	"target":       true,
	"dist":         true,
	"build":        true,
	".next":        true,
	".turbo":       true,
	".cache":       true,
}

// maybeRefreshFileIndex re-walks the workspace if the cached index
// is empty or older than fileIndexRefresh. Cheap when fresh
// (timestamp check); a real walk only happens at first use and
// every 30s thereafter. Best-effort: walk errors leave the index
// at whatever it was — autocomplete keeps working with stale data
// rather than blanking out on a transient fs hiccup.
func (r *REPL) maybeRefreshFileIndex() {
	if !r.fileIndexAt.IsZero() && time.Since(r.fileIndexAt) < fileIndexRefresh {
		return
	}
	r.fileIndex = walkProjectsForAutocomplete(r.services, r.workDir)
	r.fileIndexAt = time.Now()
}

// acceptSuggestion replaces the input with the highlighted
// completion. Behavior depends on suggestKind: top-level commands
// replace the whole input with `/<cmd> `, subcommands replace the
// trailing partial token while preserving the `/<cmd> ` prefix.
// No-op when no suggestion is active.
func (r *REPL) acceptSuggestion() {
	if r.suggestIdx < 0 || r.suggestIdx >= len(r.suggestItems) {
		return
	}
	pick := r.suggestItems[r.suggestIdx]
	v := r.input.Value()
	switch r.suggestKind {
	case suggestKindSubcommand:
		// Preserve `/<cmd> `, replace whatever the user typed
		// after that with the completion + trailing space (so
		// the user can immediately type / autocomplete an arg).
		spaceIdx := strings.IndexAny(strings.TrimPrefix(v, "/"), " \t")
		head := v[:spaceIdx+2] // includes the space
		r.input.SetValue(head + pick + " ")
	case suggestKindArg:
		// Preserve `/<cmd> <sub> `, replace the trailing
		// arg-prefix with the chosen value. No trailing space
		// since this is the last token.
		rest := strings.TrimPrefix(v, "/")
		spaceIdx := strings.IndexAny(rest, " \t")
		afterCmd := strings.TrimLeft(rest[spaceIdx+1:], " \t")
		subSpace := strings.IndexAny(afterCmd, " \t")
		// Length of `/<cmd> <sub> ` in v.
		prefixLen := 1 + spaceIdx + 1 + subSpace + 1
		head := v[:prefixLen]
		r.input.SetValue(head + pick)
	case suggestKindFile:
		// Replace the trailing "@<prefix>" token with the bare
		// path (no @). The planner's buildContext does substring
		// matching against mentioned paths — quoting them as
		// "@apps/x.go" breaks that, so we drop the marker.
		// Trailing space so the user can keep typing.
		atIdx := strings.LastIndex(v, "@")
		if atIdx < 0 {
			// Defensive — refreshSuggestions matched, but the @
			// vanished by the time we got here. No-op rather
			// than corrupt the input.
			break
		}
		r.input.SetValue(v[:atIdx] + pick + " ")
	default:
		r.input.SetValue("/" + pick + " ")
	}
	r.input.CursorEnd()
	r.suggestItems = nil
	r.suggestIdx = 0
	r.suggestKind = suggestKindNone
}

// View renders only the live region: any in-flight streaming text,
// the current transient (spinner / token counter), the slash-command
// popup, and the input box. Completed turns live in the terminal's
// own scrollback above this region — they got there via tea.Println
// from flushPrints.
//
// The streaming buffer is capped to streamWindow when it'd otherwise
// push the live region past terminal height. Bubble Tea's inline
// (non-alt-screen) renderer can't reach lines that scroll off the
// top of the screen — they become permanent scrollback. Without
// the cap, a long streaming reply would scroll its raw markdown
// into scrollback, then the finalized rendered version would land
// above the live region via tea.Println, leaving the user with two
// copies of the response stacked: rendered, then raw. The cap
// keeps the live region bounded; on finalize the raw stream
// disappears cleanly and the rendered final is the only copy in
// scrollback.
func (r REPL) View() string {
	var parts []string
	// Inline checklist sits above the streaming response so the
	// developer can see step progress without losing the prose
	// stream. Renders only while the run is active; once finalized
	// the checklist is flushed to scrollback and not redrawn here.
	if r.taskProgress != nil && r.streamActive {
		if v := r.taskProgress.View(); v != "" {
			parts = append(parts, v)
		}
	}
	if r.streamActive && r.streamBuf != "" {
		parts = append(parts, r.streamView())
	}
	// Live region. While a run is active, this slot is always
	// reserved at a stable height so incoming events can't shift
	// the surrounding layout. r.transient is the cached string from
	// the last renderSpinner call; if it's empty (because a tool/diff
	// handler ran clearTransient and never repainted), fall back to
	// rebuilding the block here from current state. View() takes a
	// value receiver so we can't mutate r.transient — we just
	// compute the same string locally.
	//
	// Suppress the in-flight spinner when a confirm prompt is open:
	// the agent isn't waiting on the model, it's waiting on the
	// user. Leaving "interrogating the rubber duck... (3m38s)" up
	// while the user reads a [y]/[n] prompt is misleading — it
	// reads as "agent still working" when the agent is blocked.
	awaitingUser := r.pendingBashConfirm != nil || r.pendingFileConfirm != nil
	switch {
	case awaitingUser:
		// no spinner — render the confirm block below instead.
	case r.transient != "":
		parts = append(parts, r.transient)
	case r.planning || r.executing || r.gateReviewing:
		if live := r.liveBlock(); live != "" {
			parts = append(parts, live)
		}
	}
	// Bash-approval block. Re-renders the command summary above the
	// hint so the question and the answer choices are visually
	// adjacent. The 🔒 header still went to scrollback above; this
	// is a duplicate intentionally — by the time a long-thinking
	// model returns from a 3-minute turn, the scrollback header
	// has scrolled out of the viewport, leaving the user staring
	// at "[y] run" with no idea what was being asked. The summary
	// here is a one-line recap (truncated at 200 chars); full
	// command stays in scrollback for the audit trail.
	if r.pendingBashConfirm != nil {
		ev := r.pendingBashConfirm
		cmd := strings.TrimSpace(ev.Summary)
		if len(cmd) > 200 {
			cmd = cmd[:197] + "…"
		}
		who := "agent"
		if ev.SpawnName != "" {
			who = ev.SpawnName
		}
		parts = append(parts, stylePlannerBanner.Render("🔒 "+who+" wants to run:"))
		parts = append(parts, "    "+cmd)
		hint := "    [y] run · [a] allow all "
		if first := firstShellToken(ev.Summary); first != "" {
			hint += "`" + first + " *` · "
		} else {
			hint += "of this kind · "
		}
		hint += "[n] cancel · esc cancels"
		parts = append(parts, styleDim.Render(hint))
	}
	if r.pendingFileConfirm != nil {
		ev := r.pendingFileConfirm
		// Re-render the "wants to write:" header above the diff so
		// the question and answer choices stay visually adjacent,
		// matching the bash-confirm shape. The original header in
		// scrollback may have scrolled out of the viewport by the
		// time a slow model returns and the prompt opens.
		who := "agent"
		if ev.SpawnName != "" {
			who = ev.SpawnName
		}
		verb := "write"
		if ev.Op == "edit" {
			verb = "edit"
		}
		header := fmt.Sprintf("🔒 %s wants to %s: %s", who, verb, ev.Path)
		if ev.Added > 0 || ev.Removed > 0 {
			header += fmt.Sprintf(" (+%d -%d)", ev.Added, ev.Removed)
		}
		parts = append(parts, stylePlannerBanner.Render(header))
		if d := strings.TrimRight(ev.Diff, "\n"); d != "" {
			var db strings.Builder
			for i, ln := range strings.Split(d, "\n") {
				if i > 0 {
					db.WriteByte('\n')
				}
				db.WriteString("    ")
				switch {
				case strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++"):
					db.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render(ln))
				case strings.HasPrefix(ln, "-") && !strings.HasPrefix(ln, "---"):
					db.WriteString(styleError.Render(ln))
				default:
					db.WriteString(styleDim.Render(ln))
				}
			}
			parts = append(parts, db.String())
		}
		parts = append(parts, styleDim.Render("    [y] write · [a] allow all writes for this session · [n] cancel · esc cancels"))
	}
	if popup := r.suggestionsView(); popup != "" {
		parts = append(parts, popup)
	}
	parts = append(parts, r.inputBoxStyle().Render(r.input.View()))
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// streamView returns the streamBuf wrapped to pane width, clamped
// to the last streamWindow lines so the rendered live region can't
// grow past terminal height. See View() for the rationale.
func (r REPL) streamView() string {
	wrapped := wrapToWidth(r.streamBuf, r.wrapWidth())
	lines := strings.Split(wrapped, "\n")

	// Reserve rows for the rest of the live region: input box (3
	// rows including borders, can grow), transient (1), popup (up
	// to ~7 with hint), and a one-line buffer. Floor at 4 so the
	// user sees something even on a tiny terminal.
	reserved := 12
	cap := r.height - reserved
	if cap < 4 {
		cap = 4
	}
	if len(lines) <= cap {
		return strings.Join(lines, "\n")
	}
	tail := lines[len(lines)-cap+1:]
	hint := styleDim.Render(fmt.Sprintf("… +%d earlier lines (full reply lands below on completion)", len(lines)-len(tail)))
	return hint + "\n" + strings.Join(tail, "\n")
}

// suggestionsView renders the autocomplete popup that floats above
// the input. Empty string when no suggestions are active. Caps the
// list at maxSuggestions so a bare "/" doesn't push the viewport
// off-screen on small terminals.
func (r REPL) suggestionsView() string {
	if len(r.suggestItems) == 0 {
		return ""
	}
	const maxSuggestions = 6
	items := r.suggestItems
	idx := r.suggestIdx
	if len(items) > maxSuggestions {
		// Window slides to keep the highlighted entry visible when
		// the user cycles past the bottom of the cap.
		start := 0
		if idx >= maxSuggestions {
			start = idx - maxSuggestions + 1
		}
		items = items[start : start+maxSuggestions]
		idx -= start
	}
	sel := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	hint := styleDim.Render(" tab: complete · ↑/↓: cycle · esc: dismiss")
	lines := make([]string, 0, len(items)+1)
	// Top-level commands display with a leading "/" since that's
	// what the user is completing. Subcommands display bare —
	// "/gate approve" not "/gate /approve" (the leading slash is
	// already typed and only the subtoken needs to land).
	for i, it := range items {
		marker := "  "
		text := it
		if r.suggestKind != suggestKindSubcommand {
			text = "/" + it
		}
		if i == idx {
			marker = "› "
			text = sel.Render(text)
		} else {
			text = styleDim.Render(text)
		}
		lines = append(lines, marker+text)
	}
	lines = append(lines, hint)
	return strings.Join(lines, "\n")
}

// inputBoxStyle returns the lipgloss style applied to the input —
// horizontal rules above and below, dim color so it sits in the
// background and doesn't compete with text. Constructed per-call so
// it picks up the latest pane width on resize.
func (r REPL) inputBoxStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, true, false).
		BorderForeground(lipgloss.Color("8")).
		Width(r.width)
}

// dispatch routes user input. The discipline is simple and explicit:
//
//   - Lines that start with "/" are kai subcommands (the leading "/"
//     is stripped, then shelled out). Examples:
//     /gate list           → kai gate list
//     /integrate --ws feat → kai integrate --ws feat
//   - Everything else is a natural-language request for the planner
//     (when configured) — a request like "Update index.js to have
//     multiple greetings" goes straight to the LLM, no ambiguity.
//   - With no planner services available, non-slash lines fall through
//     to shellout so the bare kai binary surfaces its own "unknown
//     command" error rather than swallowing the input silently.
//   - The pending-plan state machine takes priority over routing —
//     while a plan is awaiting confirmation, any input is "go",
//     "cancel", or feedback to replan.
func (r REPL) dispatch(line string) (REPL, tea.Cmd) {
	// Frustration-phrase telemetry. Logs one event per submission
	// when a canonical negativity phrase (wtf, piece of shit, so
	// frustrating, …) appears. Only the matched canonical phrases
	// are shipped — never the user's raw line. Routing continues
	// normally; this is a signal, not a gate.
	if matches := matchNegativity(line); len(matches) > 0 {
		if e := telemetry.NewEvent("user_negativity"); e != nil {
			for _, p := range matches {
				e.SetStat(negativityKey(p), 1)
			}
			e.SetStat("match_total", int64(len(matches)))
			e.Finish()
		}
	}

	// Mode 2 fixxy: the magic phrase "no sir i don't like it"
	// (case-insensitive, fuzzy on whitespace + punctuation)
	// signals that the recent answer was bad. Bundle the
	// recent turns + the complaint and ship to claude. Done
	// before any other dispatch so even pending-plan or slash
	// states route through fixxy when the phrase fires.
	if r.services != nil && r.services.Fixxy != nil &&
		r.services.FixxyMode >= fixxy.M2 && isFixxyFeedbackPhrase(line) {
		// No turn context → no signal for claude to act on. Spawning
		// fixxy here just burns a claude run on a vague complaint
		// (and the prompt explicitly tells it the answer was bad,
		// which biases it toward inventing something to "fix").
		// Skip and tell the user instead.
		turns := r.fmtRecentTurns()
		if strings.TrimSpace(turns) == "" {
			r.write(styleDim.Render("→ fixxy: no recent turns to review — complain after kai actually answers something"))
			return r, nil
		}
		prompt := fixxy.BuildFeedbackPrompt(line, turns)
		r.services.Fixxy.Trigger("feedback", prompt, r.services.FixxyMode)
		// Acknowledge to the user so they know it landed.
		// The actual fixxy progress streams in via the
		// drainer below.
		r.write(styleDim.Render("→ fixxy: noted; routing to claude with recent context"))
		return r, nil
	}

	// Pending-plan state machine: once a plan is up for confirmation,
	// any input is interpreted in that context.
	if r.pendingPlan != nil {
		lower := strings.ToLower(strings.TrimSpace(line))
		switch {
		case isPlanAffirmative(lower):
			plan := r.pendingPlan
			r.lastExecutedPlan = plan
			r.executing = true
			r.spinnerText = pickSpinnerPhrase()
			r.spinner.Spinner = pickSpinnerStyle()
			r.renderSpinner()
			return r, tea.Batch(runExecute(r.services, plan), r.spinner.Tick)
		case isPlanCancel(lower):
			r.pendingPlan = nil
			r.originalReq = ""
			r.write(styleDim.Render("plan canceled"))
			return r, nil
		default:
			original := r.originalReq
			r.planning = true
			r.spinnerText = pickSpinnerPhrase()
			r.spinner.Spinner = pickSpinnerStyle()
			r.renderSpinner()
			return r, tea.Batch(runReplan(r.services, original, line), r.spinner.Tick)
		}
	}

	if strings.HasPrefix(line, "/") {
		// /clear is a TUI-internal action: wipe the scrollback and
		// drop the chat session so the next turn starts fresh.
		// Handled here rather than via the kai binary because the
		// shell-out has no way to mutate this REPL's state.
		trimmed := strings.TrimSpace(line)
		head := trimmed
		if i := strings.IndexAny(head, " \t"); i > 0 {
			head = head[:i]
		}
		if strings.TrimSpace(strings.TrimPrefix(line, "/")) == "clear" {
			r.streamBuf = ""
			r.streamActive = false
			r.sessionID = ""
			r.pendingPlan = nil
			r.lastExecutedPlan = nil
			r.pendingAction = nil
			r.originalReq = ""
			r.planChoice = -1
			r.clearTransient()
			r.forcedMode = agent.ModeUnknown
			r.ClearHistory()
			return r, nil
		}
		if strings.ToLower(head) == "/exit" {
			// Kill any managed process before quitting so we don't
			// orphan a dev-server when the user types /exit. The
			// process gets SIGTERM+grace; if it doesn't die in 2s
			// it gets SIGKILL. Same teardown as /stop.
			StopManagedProcess(r.services)
			return r, tea.Quit
		}
		if strings.ToLower(head) == "/history" {
			n := 20
			arg := strings.TrimSpace(strings.TrimPrefix(trimmed, head))
			if arg != "" {
				if v, err := strconv.Atoi(arg); err == nil && v > 0 {
					n = v
				}
			}
			entries := loadReplHistory(r.workDir)
			if n > len(entries) {
				n = len(entries)
			}
			entries = entries[len(entries)-n:]
			var b strings.Builder
			for i, e := range entries {
				idx := fmt.Sprintf("%d", i+1)
				fmt.Fprintf(&b, "  %s\n", lipgloss.JoinHorizontal(lipgloss.Top, historyIdxStyle.Render(idx), historyEntryStyle.Render(e)))
			}
			r.write(b.String())
			return r, nil
		}
		if strings.ToLower(head) == "/version" {
			v := "(unknown)"
			if r.services != nil && r.services.Version != "" {
				v = r.services.Version
			}
			r.write(styleDim.Render("kai v" + v))
			return r, nil
		}
		// /image <path>: attach an image for the next user submit.
		// Multiple /image calls queue multiple attachments; submit
		// describes each via the vision model and rebuilds the
		// prompt with an "Images used in prompt:" block.
		// Text-only main models (Deepseek/GLM) can't see images
		// directly — this lets the user paste a screenshot of an
		// error or UI and have the planner reason about it as if
		// it were text. Mirrors Claude Code's image-input UX with
		// the describe-then-text shim added for our model stack.
		if strings.ToLower(head) == "/image" {
			path := strings.TrimSpace(strings.TrimPrefix(trimmed, "/image"))
			if path == "" {
				r.write(styleError.Render("/image: path required (e.g. /image /tmp/screenshot.png)"))
				return r, nil
			}
			// Resolve ~ and relative paths against cwd so users can
			// drag-paste a path without thinking about the working dir.
			abs := path
			if strings.HasPrefix(abs, "~/") || abs == "~" {
				if home, err := os.UserHomeDir(); err == nil {
					if abs == "~" {
						abs = home
					} else {
						abs = filepath.Join(home, abs[2:])
					}
				}
			}
			if !filepath.IsAbs(abs) {
				if cwd, err := os.Getwd(); err == nil {
					abs = filepath.Join(cwd, abs)
				}
			}
			st, err := os.Stat(abs)
			if err != nil {
				r.write(styleError.Render(fmt.Sprintf("/image: %v", err)))
				return r, nil
			}
			if st.IsDir() {
				r.write(styleError.Render("/image: path is a directory, expected an image file"))
				return r, nil
			}
			if vision.DetectMIME(abs) == "" {
				r.write(styleError.Render("/image: unsupported file type (expected .png / .jpg / .jpeg / .webp / .gif)"))
				return r, nil
			}
			r.pendingImages = append(r.pendingImages, abs)
			r.write(styleDim.Render(fmt.Sprintf("📎 image %d queued: %s", len(r.pendingImages), abs)))
			r.write(styleDim.Render("  (type your prompt and press Enter — image(s) will be described and included)"))
			return r, nil
		}
		// Mode-override slash commands: TUI-internal, never shell out.
		// They set forcedMode for the next agent turn AND persist to
		// the session's prev_mode if a session already exists, so
		// switching modes is durable across REPL restarts when the
		// user resumes a session. The empty-session case is handled
		// via forcedMode alone.
		if mode, ok := modeOverrideSlash[strings.ToLower(head)]; ok {
			r.forcedMode = mode
			if r.services != nil && r.sessionID != "" {
				if store := r.services.OrchestratorCfg.AgentSessionStore; store != nil {
					_ = session.SaveMode(store, r.sessionID, mode.String())
				}
			}
			r.write(styleDim.Render("mode → " + mode.String()))
			return r, nil
		}
		if strings.ToLower(head) == "/model" {
			// /model is TUI-internal (mutates live PlannerServices,
			// nothing to shell out to). Args after /model are passed
			// through; see handleModelCommand for behavior.
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "/model"))
			var args []string
			if rest != "" {
				args = strings.Fields(rest)
			}
			// 0 args → arrow picker for the active api.
			// 1 arg  → arrow picker for the named api.
			// 2+     → direct swap, falls through to text handler.
			if len(args) <= 1 && r.services != nil {
				kind := currentProviderKind(r.services)
				if len(args) == 1 {
					kind = normalizeProviderArg(args[0])
				}
				picker := NewModelPickerWithPlanner(kind, r.services.OrchestratorCfg.AgentModel, r.services.PlannerCfg.Model)
				if picker != nil {
					r.pendingModelPicker = picker
					r.transient = picker.Render()
					r.input.Reset()
					return r, nil
				}
			}
			r.write(handleModelCommand(r.services, args))
			return r, nil
		}
		if strings.ToLower(head) == "/status" {
			r.write(r.statusReport())
			return r, nil
		}
		if strings.ToLower(head) == "/copy" {
			// /copy [n|all|response] ships transcript blocks to the
		// clipboard. n defaults to 1 (the most recent block).
		// "all" copies the full retained buffer. "response" (or
		// "r") copies all blocks from the start of the most
		// recent agent response to the end — no need to count
		// blocks. OSC 52 inside clipboard.Copy makes this work
		// over SSH on terminals that support it; falls back to
		// pbcopy/xclip locally.
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "/copy"))
			n := 1
			switch {
			case rest == "":
				// default n=1
			case strings.EqualFold(rest, "all"):
				n = len(r.transcript)
			default:
				if v, err := strconv.Atoi(rest); err == nil && v > 0 {
					n = v
				} else {
					r.write(styleDim.Render("usage: /copy [n|all]"))
					return r, nil
				}
			}
			if len(r.transcript) == 0 {
				r.write(styleDim.Render("/copy: nothing to copy yet"))
				return r, nil
			}
			if n > len(r.transcript) {
				n = len(r.transcript)
			}
			payload := strings.Join(r.transcript[len(r.transcript)-n:], "\n\n")
			// Strip ANSI escapes before clipboard. The transcript
			// holds lipgloss-rendered strings (color + bold + dim),
			// which look like literal `[38;5;242m...` garbage when
			// pasted into anywhere that isn't a terminal — Slack,
			// GitHub issues, email, an IDE. The user wants the
			// content, not the styling.
			payload = stripANSI(payload)
			if err := clipboard.Copy(payload); err != nil {
				r.write(styleDim.Render("/copy: " + err.Error()))
				return r, nil
			}
			r.write(styleDim.Render(fmt.Sprintf("copied last %d block(s) to clipboard (%d chars, plain text)", n, len(payload))))
			return r, nil
		}
		if strings.ToLower(head) == "/verbose" || strings.ToLower(head) == "/quiet" {
			// Toggle per-tool-call scrollback writes for the rest of
			// the session. /verbose forces ON (every dispatch lands a
			// "→ name args" line); /quiet forces OFF (the default —
			// spinner shows live activity, scrollback stays clean).
			// Bare /verbose with no arg toggles. The default is quiet
			// (verboseTools=false) so users typing /verbose into a
			// fresh session opt INTO the noisy mode.
			arg := strings.TrimSpace(strings.TrimPrefix(line, head))
			switch strings.ToLower(arg) {
			case "on":
				r.verboseTools = true
			case "off":
				r.verboseTools = false
			default:
				// Bare /verbose or /quiet without arg: /verbose
				// means "I want verbose mode on"; /quiet means off.
				if strings.ToLower(head) == "/verbose" {
					r.verboseTools = true
				} else {
					r.verboseTools = false
				}
			}
			state := "off"
			if r.verboseTools {
				state = "on"
			}
			r.write(styleDim.Render(fmt.Sprintf("verbose tool output: %s", state)))
			return r, nil
		}
		if strings.ToLower(head) == "/share" {
			// Add an external path (outside the workspace) to the
			// session-scoped read allowlist. Read-only tools (view,
			// kai_grep when they consult sharedPaths, etc.) will then
			// accept paths under the shared root. Write/edit refuse
			// regardless — the boundary is read-only.
			//
			// Idempotent: re-sharing the same path is a no-op with a
			// confirmation message.
			arg := strings.TrimSpace(strings.TrimPrefix(line, head))
			if arg == "" {
				if r.services != nil && len(r.services.SharedPaths) > 0 {
					r.write(styleDim.Render("shared paths (session):"))
					for _, p := range r.services.SharedPaths {
						r.write(styleDim.Render("  " + p))
					}
				} else {
					r.write(styleDim.Render("usage: /share <path>    (no paths shared this session)"))
				}
				return r, nil
			}
			// Expand ~/ and resolve to absolute.
			expanded := arg
			if strings.HasPrefix(expanded, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					expanded = filepath.Join(home, expanded[2:])
				}
			}
			abs, err := filepath.Abs(expanded)
			if err != nil {
				r.write(styleDim.Render("cannot resolve path: " + err.Error()))
				return r, nil
			}
			abs = filepath.Clean(abs)
			// Stat to confirm it exists. Don't reject if it doesn't —
			// just warn — because the user might pre-share a path that
			// they're about to create.
			if _, serr := os.Stat(abs); serr != nil {
				r.write(styleDim.Render("warning: " + abs + " does not exist yet (sharing anyway)"))
			}
			if r.services == nil {
				r.write(styleDim.Render("share failed: planner services not configured"))
				return r, nil
			}
			for _, existing := range r.services.SharedPaths {
				if existing == abs {
					r.write(styleDim.Render("already shared: " + abs))
					return r, nil
				}
			}
			r.services.SharedPaths = append(r.services.SharedPaths, abs)
			r.write(styleDim.Render("shared (read-only, this session): " + abs))
			return r, nil
		}
		if strings.ToLower(head) == "/stop" {
			// Kill the managed dev-server process kai is watching.
			// No-op when nothing is running. The host_proc.go
			// StopManagedProcess handles SIGTERM with 2s grace then
			// SIGKILL on the process group so concurrently's
			// children die alongside the parent.
			if r.services == nil {
				r.write(styleDim.Render("/stop: no services configured"))
				return r, nil
			}
			mp := r.services.ManagedProc()
			if mp == nil {
				r.write(styleDim.Render("/stop: no managed process running"))
				return r, nil
			}
			r.write(styleDim.Render("stopping `" + mp.Command + "`…"))
			StopManagedProcess(r.services)
			return r, nil
		}
		if strings.ToLower(head) == "/refresh" {
			// Manual escape hatch for the rare ghost-input
			// pile-up: WindowSizeMsg already triggers a
			// ClearScreen on resize/wake, but some terminal
			// events (focus regain on certain emulators,
			// alt-buffer toggling by other tools) push the
			// live region into scrollback without firing a
			// resize. /refresh clears the screen on demand
			// so the user can recover without restarting.
			return r, tea.ClearScreen
		}
		// Bare `/gate` (no subcommand) is the spec's headline gesture:
		// enter the in-TUI review walkthrough rather than shelling out
		// to `kai gate` (which would only print the usage block). The
		// in-process path renders styled summaries and accepts the
		// [a]/[r]/[s]/[d] keystrokes the spec calls for. Falls back to
		// shellout when services are absent (test mode, no DB).
		if strings.TrimSpace(strings.TrimPrefix(line, "/")) == "gate" {
			if r.services != nil && r.services.DB != nil {
				return r, r.enterGateReview()
			}
			return r, runShellCommand(r.binary, "gate review", r.workDir)
		}
		// Strip the leading "/" before handing to cobra. We trim only
		// the single leading slash; arguments preserve their own
		// punctuation.
		return r, runShellCommand(r.binary, strings.TrimPrefix(line, "/"), r.workDir)
	}

	// No slash → planner if configured, else fall back to shellout
	// so the bare kai binary can render its own usage message. The
	// pending slash override (forcedMode) is consumed here: it's
	// passed into runPlan/runChatAgent for this single turn, then
	// reset so subsequent turns rely on persisted prev_mode for
	// sticky/soft resolution.
	if r.services != nil {
		// Session-cost guardrail: when KAI_MAX_SESSION_COST_USD is
		// set and we've already accumulated past it, hold the
		// request and ask y/N before dispatching. The check happens
		// here (after slash routing, before any LLM-touching path)
		// so it gates planner runs, chat replies, and replans
		// uniformly. Per-turn cost from the previous reply is
		// already added to sessionCostUSD by the trailer renderer.
		if cap := sessionCostCapUSD(); cap > 0 && r.sessionCostUSD >= cap {
			r.pendingCostCap = &pendingCostPrompt{
				request:  line,
				forced:   r.forcedMode,
				cap:      cap,
				incurred: r.sessionCostUSD,
			}
			r.write(styleError.Render(fmt.Sprintf(
				"Session cost ~$%.2f has reached limit ~$%.2f. Continue? [y/N]",
				r.sessionCostUSD, cap)))
			return r, nil
		}
		// Image attachments (Phase 1 of the image-input feature).
		// If the user queued one or more images via /image <path>,
		// describe each via the vision model and rebuild the prompt
		// before dispatching. Text-only main models (Deepseek/GLM)
		// can't see images directly; the description is the bridge.
		// Sequential describe (not parallel) for v1 — simpler error
		// paths, and typical attach count is 1-2 images.
		if len(r.pendingImages) > 0 {
			r.write(styleDim.Render(fmt.Sprintf("· describing %d image(s) via %s…", len(r.pendingImages), visionModel(r.services))))
			descriptions := make([]string, 0, len(r.pendingImages))
			for i, path := range r.pendingImages {
				data, err := os.ReadFile(path)
				if err != nil {
					r.write(styleError.Render(fmt.Sprintf("image %d (%s): read failed: %v", i+1, path, err)))
					descriptions = append(descriptions, fmt.Sprintf("(read failed: %v)", err))
					continue
				}
				mime := vision.DetectMIME(path)
				desc, derr := vision.Describe(
					context.Background(),
					r.services.OrchestratorCfg.KailabBaseURL,
					r.services.OrchestratorCfg.KailabToken,
					visionModel(r.services),
					data, mime,
				)
				if derr != nil {
					r.write(styleError.Render(fmt.Sprintf("image %d describe failed: %v", i+1, derr)))
					descriptions = append(descriptions, fmt.Sprintf("(description failed: %v)", derr))
					continue
				}
				descriptions = append(descriptions, desc)
				r.write(styleDim.Render(fmt.Sprintf("  · image %d described (%d chars)", i+1, len(desc))))
			}
			// Rebuild the prompt per the user's spec.
			var b strings.Builder
			b.WriteString(strings.TrimRight(line, "\n"))
			b.WriteString("\n\nImages used in prompt:\n")
			for i, d := range descriptions {
				fmt.Fprintf(&b, "image %d: %s\n", i+1, d)
			}
			line = b.String()
			r.pendingImages = nil
		}

		r.startRun()
		forced := r.forcedMode
		r.forcedMode = agent.ModeUnknown
		// Implementation-action override. When the user types an
		// action verb that means "actually do the thing" (implement,
		// ship, build, write the code) AFTER a chat reply that
		// offered to do work, the chat agent would just produce
		// another description because conversation mode has no edit
		// tools. Override forced=ModeCoding so the dispatch routes
		// to the coding agent path which CAN edit. 2026-05-28 dogfood:
		// user typed "yes please implement it" after chat asked
		// "Want me to implement it?", chat replied with another
		// markdown wall of code blocks but never opened a file.
		// The /code or /edit slash is the manual workaround; this
		// auto-routes when the user said "go" but the trailer wasn't
		// the canonical pre-arm phrase.
		if forced == agent.ModeUnknown && isImplementationActionRequest(line) {
			forced = agent.ModeCoding
			r.write(styleDim.Render("· routing to /code (you asked to implement — chat mode is read-only)"))
		}
		// Pending-action lifecycle (P0 confirmation-loop fix).
		// Three branches for this turn's user input:
		//   - short affirmative + pending action exists → consume:
		//     pass the action text through to runPlan so dispatch
		//     wraps the prompt with an explicit "you offered X,
		//     user confirmed X, execute X" preamble.
		//   - short negative + pending action exists → cancel:
		//     drop the slot, write a dim ack, and treat the
		//     negative as a no-op turn (don't dispatch — the user
		//     declined, the next prompt is their real next ask).
		//   - anything else + pending action exists → user moved
		//     on to a new request: clear the slot silently so the
		//     stale offer can't get re-applied on a future "yes".
		pendingActionText := ""
		if r.pendingAction != nil {
			switch {
			case isShortAffirmative(line):
				pendingActionText = r.pendingAction.text
				r.pendingAction = nil
				// Snapshot for the critic-retry path (P1-3).
				// Cleared on critic PASS or retry-cap reached.
				r.inFlightPendingAction = pendingActionText
			case isShortNegative(line):
				r.pendingAction = nil
				r.write(styleDim.Render("· pending action cancelled — what would you like instead?"))
				r.planning = false
				return r, nil
			default:
				r.pendingAction = nil
			}
		}
		return r, tea.Batch(runPlan(r.services, line, r.sessionID, forced, pendingActionText), r.spinner.Tick)
	}
	return r, runShellCommand(r.binary, line, r.workDir)
}

// startRun stamps the per-run timing/output baseline and flips
// the spinner on. Called at every fresh user submit and at the
// "y" confirmation that resumes a cap-paused run. Centralized so
// the Claude-Code-comparable summary line can rely on
// runStart/runOutputTokens being set consistently.
func (r *REPL) startRun() {
	r.planning = true
	r.responseStarts = append(r.responseStarts, len(r.transcript))
	// First-byte tracker also resets — last turn's value would
	// give a false negative reading on the new turn.
	r.turnFirstResponseAt = time.Time{}
	// Quiet-mode tool counter resets too — the per-run count is
	// only meaningful for the current run.
	r.suppressedToolEvents = 0
	r.suppressedToolLines = nil
	r.lastToolSummary = ""
	r.spinnerText = pickSpinnerPhrase()
	r.spinner.Spinner = pickSpinnerStyle()
	r.renderSpinner()
	r.runStart = time.Now()
	r.lastActivity = r.runStart
	r.providerState = ChatActivityEvent{}
	r.providerStateAt = time.Time{}
	r.runOutputTokens = 0
	// Re-arm the late-tokens guard. The previous run set
	// trailerRendered=true to drop late events; a fresh run
	// expects to receive `tokens` events again to animate the
	// live line.
	r.trailerRendered = false
	// Drop any leftover transient (e.g. a token line that
	// didn't get cleared because a late event slipped through
	// before the previous run's trailer landed). Without this,
	// the next run starts with stale text floating below the
	// input until the spinner re-renders. clearTransient also
	// empties the spinner/token buckets so a stale value can't
	// re-surface on the next composeTransient call.
	r.clearTransient()
}

// statusReport renders the /status output: current mode (forced if a
// slash override is pending; otherwise the persisted prev_mode for
// the active chat session), session id (truncated), and the
// cumulative token usage shown in the live counter. Held gate items
// and per-task agent counts are intentionally not surfaced here in
// v1 — they need plumbing into PlannerServices that doesn't exist
// yet, and the spec said "doesn't need to be fancy."
func (r REPL) statusReport() string {
	mode := "coding (default)"
	source := "default"
	if r.forcedMode != agent.ModeUnknown {
		mode = r.forcedMode.String()
		source = "forced via slash override"
	} else if r.services != nil && r.sessionID != "" {
		store := r.services.OrchestratorCfg.AgentSessionStore
		if store != nil {
			if raw, err := session.LookupMode(store, r.sessionID); err == nil && raw != "" {
				mode = raw
				source = "persisted from previous turn"
			}
		}
	}
	sess := r.sessionID
	if sess == "" {
		sess = "(no active session)"
	} else if len(sess) > 8 {
		sess = sess[:8] + "…"
	}
	lines := []string{
		styleDim.Render("status:"),
		"  mode:    " + mode + "  " + styleDim.Render("("+source+")"),
		"  session: " + sess,
		fmt.Sprintf("  tokens:  %s in / %s out%s",
			humanCount(r.tokenTargetIn),
			humanCount(r.tokenTargetOut),
			cachedSuffix(r.tokenTargetCached),
		),
	}
	// Per-role models: kai routes classify / plan / chat / code work
	// to different models. Show what each role resolved to so the
	// user can confirm routing without reading config.yaml.
	if r.services != nil {
		lines = append(lines,
			styleDim.Render("  models:"),
			"    classify: "+r.services.ClassifierModel,
			"    plan:     "+r.services.PlannerModel,
			"    chat:     "+r.services.ChatModel,
			"    code:     "+r.services.OrchestratorCfg.AgentModel,
			"    review:   "+r.services.ReviewModel,
		)
	}
	return strings.Join(lines, "\n")
}

func cachedSuffix(cached int) string {
	if cached <= 0 {
		return ""
	}
	return fmt.Sprintf(" (+ %s cached)", humanCount(cached))
}

// write queues a word-wrapped line for the next tea.Println flush.
// Suitable for chat replies / tool output / status messages that
// don't already manage their own line breaks.
// transcriptCap bounds the in-memory copy buffer. ~1000 entries is
// plenty for "copy the last few replies" without growing unbounded
// over a long session.
const transcriptCap = 1000

func (r *REPL) appendTranscript(s string) {
	r.transcript = append(r.transcript, s)
	if over := len(r.transcript) - transcriptCap; over > 0 {
		r.transcript = r.transcript[over:]
		// Adjust responseStarts to account for the trimmed prefix.
		for i := range r.responseStarts {
			r.responseStarts[i] -= over
		}
		// Drop entries that predate the retained window.
		j := 0
		for _, v := range r.responseStarts {
			if v >= 0 {
				r.responseStarts[j] = v
				j++
			}
		}
		r.responseStarts = r.responseStarts[:j]
	}
}

// retractFailedTurn purges the most recent agent turn from session
// history and from the in-program transcript so a critic auto-retry
// doesn't see (or republish) the answer the critic just rejected.
//
// Returns the number of transcript lines that belonged to the
// retracted turn — caller may use this to drive an ANSI overwrite
// of the recently-flushed scrollback lines (best-effort cosmetic).
//
// Silent on errors: a session-store glitch or an empty
// responseStarts must not break the retry path. Worst case is the
// retry sees one extra bad turn — same as today's behavior.
func (r *REPL) retractFailedTurn() int {
	retracted := 0
	// 1. In-program transcript. responseStarts[len-1] marks the index
	//    where the failed run began appending. Slice both back so
	//    /copy response and the transcript-cap math line up.
	if n := len(r.responseStarts); n > 0 {
		start := r.responseStarts[n-1]
		if start <= len(r.transcript) {
			retracted = len(r.transcript) - start
			r.transcript = r.transcript[:start]
		}
		r.responseStarts = r.responseStarts[:n-1]
	}
	// 2. Persisted session. Drop every message after the last user
	//    turn — that's exactly the failed assistant reply and any
	//    tool calls/results it produced. Wrapped in Resume so we
	//    don't depend on the runner having handed us a *Session
	//    handle.
	if store := r.services.OrchestratorCfg.AgentSessionStore; store != nil && r.sessionID != "" {
		if s, err := session.Resume(store, r.sessionID); err == nil {
			if n, _ := s.TruncateAfterLastUser(); n > 0 {
				// After truncation the session ends on a user message.
				// The retry will then append the new user message
				// (originalRequest + critique), leaving two user
				// messages in a row with no assistant turn between
				// them. Providers that strictly enforce role
				// alternation either reject the request or — observed
				// on deepseek/openai-compat — produce an off-topic
				// reply like "what would you like me to do?" because
				// the model can't bind the second user message back to
				// the first. Insert a placeholder assistant stub so
				// the conversation alternates cleanly.
				stub := message.Message{
					Role: message.RoleAssistant,
					Parts: []message.ContentPart{
						message.TextContent{Text: "[prior attempt retracted by quality gate — see retry prompt below]"},
					},
					Finished: message.FinishReasonEndTurn,
				}
				_ = s.AppendMessage(stub, 0, 0)
			}
		}
	}
	// 3. Opt-in ANSI overwrite. tea.Println has already flushed the
	//    bad answer to terminal scrollback; ESC[<n>F (cursor up,
	//    column 1) + ESC[J (clear to end of screen) reaches back into
	//    the most recent scrollback rows on terminals that honor it.
	//    Fails silently on terminals that don't, on bbtea bridges
	//    that buffer differently, or when the user has scrolled past
	//    the affected rows. Gated until it has been dogfooded enough
	//    to enable by default.
	if retracted > 0 && os.Getenv("KAI_CRITIC_RETRY_OVERWRITE") != "" {
		r.pendingPrints = append(r.pendingPrints,
			fmt.Sprintf("\x1b[%dF\x1b[J", retracted))
	}
	return retracted
}

func (r *REPL) write(line string) {
	r.pendingPrints = append(r.pendingPrints, wrapToWidth(line, r.wrapWidth()))
	r.appendTranscript(line)
}

// writeRaw queues pre-formatted text without word-wrapping. Use
// this for content that already carries ANSI escape codes
// (diff hunks, gate verdicts, banner): the wrapper's
// strings.Fields split tokenizes inside escape sequences and silently
// destroys colors.
func (r *REPL) writeRaw(text string) {
	r.pendingPrints = append(r.pendingPrints, text)
	r.appendTranscript(text)
}

// flushStreamSegment pins the current in-flight streamBuf into
// scrollback as a discrete paragraph and resets the buffer so the
// next batch of deltas starts a new visual block. Unlike
// finalizeStream this does NOT mark the stream closed — the agent
// is still running, the model is just transitioning from "writing
// prose" to "calling a tool." Called from the activity-event
// handler on each tool/diff/bash/gate event so multi-turn runs
// don't render as one wall of "Now let me look at..." text.
//
// No-op when there's nothing to flush. Skips the markdown render
// pass — partial in-flight text isn't necessarily well-formed
// markdown, and we want the visual block to land immediately, not
// wait for a glamour pass that might mangle a half-emitted code
// fence.
func (r *REPL) flushStreamSegment() {
	if !r.streamActive || strings.TrimSpace(r.streamBuf) == "" {
		r.streamBuf = ""
		r.streamActive = false
		return
	}
	r.write(strings.TrimRight(r.streamBuf, " \n"))
	r.streamBuf = ""
	r.streamActive = false
}

// finalizeStream ends the in-flight streaming block. Returns the
// streamed text so callers (PlanReadyMsg) can decide whether to
// re-render via markdown or skip (when the final reply differs
// from what was streamed, e.g. an error replaced the in-flight
// text). The View() drops the streaming line as soon as
// streamActive flips false.
func (r *REPL) finalizeStream() string {
	// Mark the stream closed unconditionally, even if we never
	// streamed (no-op finalize) — guards against late delta
	// stragglers between PlanReadyMsg and the next user submit.
	r.streamClosed = true
	// If a STEPS: block was open mid-stream when the model stopped,
	// close it now so a partial checklist still surfaces.
	if leftover := r.stepParser.FinalizeBlock(); len(leftover) > 0 && r.taskProgress == nil {
		tp := NewTaskProgress("", leftover)
		r.taskProgress = &tp
	}
	// Pin the checklist into scrollback before clearing it from the
	// live region. Without this the developer sees the live block
	// vanish the moment the run ends — which is wrong, the
	// completed checklist is the most useful artifact of the turn.
	if r.taskProgress != nil {
		// Mark any still-in-progress step done with whatever
		// duration accumulated (model might have skipped the
		// final STEP_DONE). Don't fabricate steps the model
		// didn't declare; just terminate the active one.
		for i := range r.taskProgress.Steps {
			if r.taskProgress.Steps[i].Status == StepInProgress {
				r.taskProgress.FinishStep(i, r.tokenTargetIn)
			}
		}
		if v := r.taskProgress.View(); v != "" {
			r.writeRaw(v)
		}
	}
	if !r.streamActive {
		return ""
	}
	streamed := r.streamBuf
	r.streamBuf = ""
	r.streamActive = false
	return streamed
}

// writeMarkdown renders the input as markdown via glamour and
// queues the result for the next flush. Falls back to plain wrap
// if glamour fails for any reason.
func (r *REPL) writeMarkdown(md string) {
	md = bulletParagraphs(md)
	width := r.wrapWidth()
	rendered, err := renderMarkdown(md, width)
	if err != nil || strings.TrimSpace(rendered) == "" {
		r.write(md)
		return
	}
	// Glamour wraps internally; don't double-wrap. Trim trailing
	// blank lines glamour likes to add so successive prints don't
	// stack visual whitespace.
	r.pendingPrints = append(r.pendingPrints, strings.TrimRight(rendered, "\n"))
	// Stash the source markdown, not the styled render — clipboard
	// consumers (issue trackers, chat) want the markdown back, not
	// ANSI escapes.
	r.appendTranscript(md)
}

// bulletParagraphs prepends "• " to the start of every paragraph-level
// line in a markdown source. A paragraph line is one that:
//   - is non-empty after trimming,
//   - is NOT inside a fenced code block,
//   - does NOT already start with a markdown structural prefix
//     (heading "#", list marker "- ", "* ", "+ ", numbered "1. ",
//     blockquote "> ", inline-code "`", table "|", or 4-space indent).
//
// Glamour renders "• " as a bare Unicode bullet — distinct from its
// own list styling, which is what we want for prose paragraphs that
// aren't semantically a list. Code fences are tracked so we don't
// stamp bullets inside ``` blocks.
func bulletParagraphs(md string) string {
	if md == "" {
		return md
	}
	lines := strings.Split(md, "\n")
	inFence := false
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		// Toggle code-fence state on lines starting with ``` or ~~~.
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if trim == "" {
			continue
		}
		// Skip lines that already carry markdown structure — bullets
		// would either double up (lists) or break rendering (headings,
		// code, tables, blockquotes).
		if startsWithMarkdownStructure(trim) {
			continue
		}
		// Preserve any leading whitespace (rare in prose, but if the
		// model emitted indented prose, we keep the indent and insert
		// the bullet at the first non-space column).
		leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = leading + "• " + line[len(leading):]
	}
	return strings.Join(lines, "\n")
}

// startsWithMarkdownStructure reports whether a trimmed line begins
// with a markdown structural marker that the bullet-paragraph
// preprocessor must leave alone.
func startsWithMarkdownStructure(trim string) bool {
	if trim == "" {
		return false
	}
	// Heading, blockquote, inline-code-only line, table row.
	switch trim[0] {
	case '#', '>', '`', '|':
		return true
	}
	// List markers: "- ", "* ", "+ " (require a space to avoid
	// catching prose that incidentally starts with these chars).
	if len(trim) >= 2 && (trim[0] == '-' || trim[0] == '*' || trim[0] == '+') && trim[1] == ' ' {
		return true
	}
	// Numbered list: leading digits then ". " or ") ".
	end := 0
	for end < len(trim) && trim[end] >= '0' && trim[end] <= '9' {
		end++
	}
	if end > 0 && end < len(trim)-1 && (trim[end] == '.' || trim[end] == ')') && trim[end+1] == ' ' {
		return true
	}
	return false
}

// tokenTickMsg is the self-scheduled tween tick for the live token
// counter. Each tick interpolates tokenShown toward tokenTarget and,
// if there's still distance to cover, schedules the next tick.
type tokenTickMsg struct{}

// tokenAnimFrame is the inter-frame delay for the counter tween.
// 33ms (~30 fps) feels live without saturating the event loop.
const tokenAnimFrame = 33 * time.Millisecond

func scheduleTokenTick() tea.Cmd {
	return tea.Tick(tokenAnimFrame, func(time.Time) tea.Msg { return tokenTickMsg{} })
}

// stepToward returns the new value of `shown` advanced toward
// `target` for one animation frame. Eases out — moves a fraction of
// the remaining distance each frame, with a minimum step so small
// gaps still close in finite time.
func stepToward(shown, target int) int {
	if shown == target {
		return shown
	}
	delta := target - shown
	step := delta / 5 // ~5 frames to close the gap
	if step == 0 {
		if delta > 0 {
			return shown + 1
		}
		return shown - 1
	}
	return shown + step
}

// renderTokenLine refreshes the token-counter bucket and recomposes
// the transient. The bucket is independent of spinnerView so the
// counter doesn't get clobbered every spinner tick (and vice versa) —
// previously both renderers wrote to r.transient and the result
// flashed back and forth at spinner-tick frequency.
func (r *REPL) renderTokenLine() {
	// During a run, the spinner block owns the token slot (see
	// renderSpinner) so the bottom region renders as one fixed
	// 3-line block. Skipping the bucket here avoids double-render
	// and keeps the layout stable as inline events arrive.
	if r.planning || r.executing || r.gateReviewing {
		r.tokenView = ""
		r.composeTransient()
		return
	}
	r.tokenView = styleDim.Render(formatTokens(r.tokenShownIn, r.tokenShownOut, r.tokenShownCached))
	r.composeTransient()
}

// renderSpinner refreshes the spinner bucket. During an active run
// (planning/executing/gateReviewing) it produces a fixed 3-line
// block: spinner glyph + label, provider-state line, token-counter
// line. The block height is constant so the layout doesn't jitter
// as inline events arrive (clearTransient + repaint can't desync
// the lines anymore — they're all in one render call).
func (r *REPL) renderSpinner() {
	if r.spinnerText == "" {
		r.spinnerView = ""
		r.composeTransient()
		return
	}
	label := r.spinnerText
	if !r.runStart.IsZero() {
		if elapsed := time.Since(r.runStart); elapsed >= 2*time.Second {
			label += " (" + elapsed.Round(time.Second).String() + ")"
		}
	}
	first := r.spinner.View() + " " + styleSpinnerLabel.Render(label)

	// Optional thinking line under the spinner (latest model
	// narration sentence, dim, truncated to one terminal width).
	if r.thinkingLine != "" {
		w := r.wrapWidth() - 2
		if w < 20 {
			w = 20
		}
		think := r.thinkingLine
		if runeCount(think) > w {
			think = truncateRunes(think, w-1)
		}
		first += "\n  " + styleDim.Render(think)
	}

	if r.planning || r.executing || r.gateReviewing {
		// Provider state line. Always reserved during a run so
		// the layout doesn't shift as state events come and go.
		// Empty between transitions renders as a single bullet.
		stateLine := ""
		if r.providerState.Kind == "provider_state" {
			stateLine = r.providerState.Summary
			if !r.providerStateAt.IsZero() {
				elapsed := time.Since(r.providerStateAt)
				if elapsed >= 2*time.Second {
					stateLine += " · " + elapsed.Round(time.Second).String()
				}
			}
		}
		first += "\n  " + styleDim.Render("· "+stateLine)

		// Token counter line. Slot is reserved so the 3-line
		// block stays a constant height. While the model is
		// streaming we usually have no usage numbers — most
		// providers only ship the usage block in the final SSE
		// chunk. Showing "0 in / 0 out" reads as broken; show a
		// placeholder until at least one of in/out has flowed.
		var tokenLine string
		if r.tokenShownIn > 0 || r.tokenShownOut > 0 {
			tokenLine = formatTokens(r.tokenShownIn, r.tokenShownOut, r.tokenShownCached)
		} else {
			tokenLine = "· tokens reported at end of turn"
		}
		first += "\n  " + styleDim.Render(tokenLine)

		// Stuck hint. When no model-side activity (text delta, tool
		// call, file change, provider-state transition) has landed
		// in stuckHintAfter, surface that fact so the user can decide
		// whether to wait or hit Esc. Without this the spinner just
		// keeps ticking and the user has no way to tell "still doing
		// real work" from "wedged on a slow LLM call or a hung
		// subprocess." The line stays out of the layout otherwise
		// (we only render when the threshold is crossed) so happy
		// runs don't pay a visual cost.
		if hint := r.stuckHint(); hint != "" {
			first += "\n  " + styleDim.Render(hint)
		}
	}
	r.spinnerView = first
	r.composeTransient()
}

// autoEscalateAfter is the idle threshold past which the REPL
// auto-cancels the in-flight chat turn and dispatches a new turn
// instructing the agent to start with kai_consult. Past one full
// DeepSeek-V4-Pro reasoning cycle (~5min observed max) but well
// short of the 15-minute chatWallClockBudgetReasoning hard cap,
// so the escalation has room to run.
const autoEscalateAfter = 4 * time.Minute

// maybeAutoEscalate checks the idle-time guard and, if past the
// threshold AND this turn hasn't already escalated, cancels the
// in-flight run and dispatches a new chat turn whose prompt tells
// the agent to call kai_consult immediately rather than continue
// the stalled investigation. Returns the tea.Cmd to dispatch the
// new turn, or nil when no escalation is needed yet.
//
// One-shot per turn (autoEscalatedTurn flag). User input always
// wins — if the user types something, the new turn's user-typed
// prompt replaces the auto-escalation.
func (r *REPL) maybeAutoEscalate() tea.Cmd {
	if r.autoEscalatedTurn {
		return nil
	}
	if r.services == nil {
		return nil
	}
	if r.lastActivity.IsZero() {
		return nil
	}
	if time.Since(r.lastActivity) < autoEscalateAfter {
		return nil
	}
	// Don't fire while the orchestrator is executing. Executor
	// runs go through orchestrator.Execute which calls back via
	// OnAgentLifecycle / OnAgentBashOutput / OnAgentProviderState
	// / OnFileDiff — but those events can stall for minutes
	// during legitimate work: a long LLM turn with no streamed
	// deltas, a between-phase pause (despawn → integrate →
	// re-spawn), a chatCh overflow (cap 64) dropping events
	// silently. The 2026-05-26 dogfood pinned this: the executor
	// was mid-`view + write + edit` when "agent idle for 4m0s"
	// fired, CancelCurrent() murdered the in-flight work, and
	// the planner failed-vague on the escalation dispatch — the
	// user lost the entire fix that was about to land. Until the
	// executor-side activity wiring is reworked to emit a
	// heartbeat (separate change), trust r.executing as the
	// "work is happening, don't kill it" signal.
	if r.executing {
		return nil
	}
	// Need an original-request anchor to escalate on. If we don't
	// have one cached, fall back to the last user-typed history
	// entry — same content as the prompt that started this turn.
	req := strings.TrimSpace(r.autoEscalateRequest)
	if req == "" && len(r.history) > 0 {
		req = strings.TrimSpace(r.history[len(r.history)-1])
	}
	if req == "" {
		return nil
	}
	r.autoEscalatedTurn = true
	// Cancel the in-flight chat turn so we don't race against it.
	r.services.CancelCurrent()
	idleStr := time.Since(r.lastActivity).Round(time.Second).String()
	// Tool availability gates the escalation strategy. kai_consult
	// only registers when ConsultModel is configured (production
	// default sets it, but KAI_CONSULT_MODEL=off disables, and
	// some users land here with consult unconfigured). When
	// unavailable, instructing the agent to call kai_consult is
	// worse than nothing — the 2026-05-25 dogfood pinned this:
	// agent said "I don't have a kai_consult tool available, so
	// let me explore the project to understand what 'run it'
	// means." and started a fresh investigation, losing all
	// prior context. New behavior: forced-finalize prompt that
	// tells the agent to STOP investigating and commit to a
	// concrete answer/fix from what it already has.
	consultAvailable := strings.TrimSpace(r.services.OrchestratorCfg.ConsultModel) != ""
	var prompt string
	if consultAvailable {
		r.write(styleDim.Render(fmt.Sprintf(
			"⚠ agent idle for %s — auto-escalating via kai_consult", idleStr,
		)))
		prompt = fmt.Sprintf(
			"%s\n\n[Prior attempt stalled in reasoning for %s with no output. Call kai_consult IMMEDIATELY as your first tool — do not investigate further on your own. Pass the original request as goal, list any tool calls you'd already made as tried, and describe what's stuck in blocked_by. Act on the consult's diagnosis.]",
			req, idleStr,
		)
	} else {
		r.write(styleDim.Render(fmt.Sprintf(
			"⚠ agent idle for %s — forcing finalize (kai_consult not configured)", idleStr,
		)))
		// Forced-finalize prompt: stop investigating, commit
		// to a concrete answer from what you've already seen.
		// 2026-05-25 dogfood evidence: the stuck DeepSeek turn
		// had the full file content + error message in its
		// context, but kept reaching for new exploration paths
		// (encoding, preprocessor config, package.json) instead
		// of acting on what was already visible. This prompt
		// shuts that loop down.
		prompt = fmt.Sprintf(
			"%s\n\n[Prior attempt stalled in reasoning for %s with no visible output. STOP investigating. Based ONLY on tool results and content you've already seen in this conversation, commit to a single concrete next step: either (a) an exact edit (file + line + replacement) you propose, or (b) one clear sentence stating what specific piece of information you genuinely need from the user that you cannot infer. Do NOT call more exploratory tools. Do NOT reason about possibilities you haven't already confirmed. Pick (a) or (b) and answer briefly — verbosity is the failure mode here.]",
			req, idleStr,
		)
	}
	// dispatch returns (REPL, tea.Cmd); pull just the cmd. The
	// REPL we built here is already what we want — the dispatch
	// will be entered next via the cmd.
	_, cmd := r.dispatch(prompt)
	return cmd
}

// stuckHintAfter is the threshold past which the spinner annotates
// itself with a "no activity for N" warning. 20s is the empirical
// sweet spot from the 2026-05-12 dogfood: shorter triggered on
// normal LLM round-trips (Anthropic prefill on a 30K context can
// burn 10–15s before any text streams), longer left the user
// staring at "compiling justifications…" for 7 minutes without a
// signal that something might be wrong.
const stuckHintAfter = 20 * time.Second

// stuckHint returns a one-line annotation to append below the
// spinner when the run has gone idle. Empty string means "say
// nothing" — we suppress the line entirely rather than reserve a
// blank slot, because a noisy "all good" line would defeat the
// purpose (the user trains themselves to ignore it).
//
// "Idle" means no activity-bearing message has updated
// r.lastActivity within stuckHintAfter. The bottom-line ergonomics:
// the hint either tells you "still working, model just slow" (you
// keep waiting) or "wedged for minutes, probably stuck" (you
// cancel and inspect). Either is better than guessing from a
// blinking spinner.
func (r REPL) stuckHint() string {
	if r.lastActivity.IsZero() {
		return ""
	}
	idle := time.Since(r.lastActivity)
	if idle < stuckHintAfter {
		return ""
	}
	// Suppress when the latest provider state shows an active
	// in-flight HTTP/SSE call. The connection itself is the
	// activity signal here — the model is mid-think before
	// streaming, or streaming-without-deltas-this-instant. The
	// 2026-05-26 dogfood screenshot showed "↑ connected · HTTP
	// 200 · 50s" right above "⚠ no activity for 50s — agent
	// reasoning…" — the line below contradicted the line above
	// because lastActivity stamps on each provider-state EVENT
	// (not on the connection holding open continuously). Until
	// providers emit a periodic in-flight heartbeat we trust
	// the latest provider state's phase as the more accurate
	// signal: connected / streaming / first byte = active,
	// don't nag.
	if r.providerState.Kind == "provider_state" {
		sum := strings.ToLower(r.providerState.Summary)
		if (strings.Contains(sum, "streaming") && !strings.Contains(sum, "idle")) ||
			strings.Contains(sum, "connected") ||
			strings.Contains(sum, "first byte") {
			return ""
		}
	}
	rounded := idle.Round(time.Second)
	// Past 2 minutes the user should probably consider canceling.
	// Past 5, the hint gets more direct about it. The wording stays
	// non-prescriptive — sometimes a 5-minute reasoning step is
	// genuinely the work the model is doing.
	switch {
	case idle >= 5*time.Minute:
		return fmt.Sprintf("⏱  no activity for %s — likely wedged. Esc to cancel and inspect.", rounded)
	case idle >= 2*time.Minute:
		return fmt.Sprintf("⏱  no activity for %s — agent may be on a slow model turn or stuck. Esc to cancel.", rounded)
	default:
		return fmt.Sprintf("⏱  no activity for %s — agent reasoning or waiting on the model.", rounded)
	}
}

// liveBlock builds the 3-line live region directly from the REPL's
// current state. Pure (value receiver, no mutation) so View() can
// call it as a fallback when r.transient was cleared by an inline
// event handler and never repainted before the next render.
//
// Mirrors the structure renderSpinner produces. Returning the same
// bytes either way means no jitter when the source switches between
// "cached r.transient" and "rebuilt-on-the-fly liveBlock" — they're
// the same content.
func (r REPL) liveBlock() string {
	if r.spinnerText == "" {
		return ""
	}
	label := r.spinnerText
	if !r.runStart.IsZero() {
		if elapsed := time.Since(r.runStart); elapsed >= 2*time.Second {
			label += " (" + elapsed.Round(time.Second).String() + ")"
		}
	}
	out := r.spinner.View() + " " + styleSpinnerLabel.Render(label)

	if r.thinkingLine != "" {
		w := r.wrapWidth() - 2
		if w < 20 {
			w = 20
		}
		think := r.thinkingLine
		if runeCount(think) > w {
			think = truncateRunes(think, w-1)
		}
		out += "\n  " + styleDim.Render(think)
	}

	if r.planning || r.executing || r.gateReviewing {
		stateLine := ""
		if r.providerState.Kind == "provider_state" {
			stateLine = r.providerState.Summary
			if !r.providerStateAt.IsZero() {
				elapsed := time.Since(r.providerStateAt)
				if elapsed >= 2*time.Second {
					stateLine += " · " + elapsed.Round(time.Second).String()
				}
			}
		}
		out += "\n  " + styleDim.Render("· "+stateLine)

		var tokenLine string
		if r.tokenShownIn > 0 || r.tokenShownOut > 0 {
			tokenLine = formatTokens(r.tokenShownIn, r.tokenShownOut, r.tokenShownCached)
		} else {
			tokenLine = "· tokens reported at end of turn"
		}
		out += "\n  " + styleDim.Render(tokenLine)
	}
	return out
}

// composeTransient joins the spinnerView and tokenView buckets into
// the rendered r.transient string. Either bucket may be empty; the
// joined view shows whichever is populated, with a newline separator
// when both are. Called from the bucket renderers above whenever
// either updates.
func (r *REPL) composeTransient() {
	switch {
	case r.spinnerView == "" && r.tokenView == "":
		r.transient = ""
	case r.spinnerView == "":
		r.transient = r.tokenView
	case r.tokenView == "":
		r.transient = r.spinnerView
	default:
		r.transient = r.spinnerView + "\n" + r.tokenView
	}
}

// clearTransient drops the live status line. Cheap; safe to call
// defensively from message handlers that may or may not have set a
// transient earlier. Also empties the spinner/token buckets so the
// next renderSpinner / renderTokenLine doesn't repaint a stale value
// from the previous turn.
func (r *REPL) clearTransient() {
	r.transient = ""
	r.spinnerView = ""
	r.tokenView = ""
}

// lastSentence returns the trailing prose sentence of the model's
// latest text — the most recent thing the model "said," with JSON
// fragments stripped. Used by the thinking-line transient so the
// user sees what the planner is currently reasoning about, not a
// raw `"files": ["index.js"]` slice.
//
// Pipeline:
//
//  1. Truncate at the first JSON-shaped marker (```json fence,
//     leading {, "summary":, etc.). Anything past these markers is
//     the model's final plan output, which renders separately.
//  2. Strip lines that look like JSON object fields (start with a
//     quote, end with comma).
//  3. Pick the trailing sentence from what remains, splitting on
//     ". " followed by capital/backtick.
//
// Returns "" when nothing prose-y survives — caller should leave
// the thinking line empty rather than render JSON garbage.
// firstShellToken returns the first whitespace-separated token of a
// shell command, after stripping leading env-var assignments and
// "cd /path && " prefixes. Used by the bash-approval allowlist so
// "cd /tmp && bun test" matches "bun" — without the prefix-strip,
// every cd-prefixed command would land under "cd" and the allowlist
// would either be useless or dangerously permissive.
//
// Conservative: only handles patterns the agent commonly emits
// (cd-then-cmd, env=val cmd). A user crafting clever commands to
// exploit the allowlist isn't a threat model worth designing
// against — they're approving their own bash.
func firstShellToken(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for {
		// Strip leading "cd <path> && " or "cd <path> ; " — the
		// agent's "anchor my command in /tmp/repo" idiom.
		if strings.HasPrefix(cmd, "cd ") {
			rest := cmd[3:]
			for _, sep := range []string{"&&", ";"} {
				if i := strings.Index(rest, sep); i > 0 {
					cmd = strings.TrimSpace(rest[i+len(sep):])
					goto next
				}
			}
		}
		break
	next:
	}
	for _, tok := range strings.Fields(cmd) {
		if strings.Contains(tok, "=") {
			continue // env-var assignment, skip
		}
		return strings.TrimPrefix(tok, "./")
	}
	return ""
}

// formatCommandBlock renders a (possibly multi-line) shell command
// for the bash-approval prompt. The first line gets a dim "$ "
// prompt; subsequent lines get aligned spaces of the same width so
// the whole command reads as a single visual block instead of
// breaking out of the indent on the second line. Output already
// includes the leading 4-space indent that anchors the block under
// the "🔒 ... wants to run:" header above it.
//
// Why this matters: a command like
//
//	cd /tmp/repo
//	pkill -x hello_world
//	gcc -o hello_world hello_world.c
//
// without per-line indent renders the second/third line flush with
// the surrounding prose. The user can't tell what's command vs
// commentary at a glance — see the May 2026 layout report.
func formatCommandBlock(cmd string) string {
	const promptIndent = "    $ "
	const contIndent = "      " // same width as "    $ "
	lines := strings.Split(cmd, "\n")
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(styleDim.Render(promptIndent) + styleBashCommand.Render(lines[0]))
	for _, line := range lines[1:] {
		b.WriteString("\n")
		b.WriteString(styleDim.Render(contIndent) + styleBashCommand.Render(line))
	}
	return b.String()
}

// looksLikePlanJSON reports whether the planner's assistant text
// is its final JSON plan emission rather than narration. The planner
// is instructed to output a fenced ```json {…} ``` block as its
// FINAL message; in practice the model sometimes ships it unfenced
// or with a brief prose preamble. We detect either by the presence
// of a JSON-shaped fence OR a fragment with the plan's signature
// keys ("agents" + "summary"). False positives are fine — they just
// mean we skip one dim flush; the rendered plan block still appears.
func looksLikePlanJSON(text string) bool {
	if strings.Contains(text, "```json") {
		return true
	}
	if strings.Contains(text, `"agents"`) && strings.Contains(text, `"summary"`) {
		return true
	}
	return false
}

func lastSentence(s string) string {
	s = stripJSONFragments(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	last := s
	for i := 0; i < len(s)-2; i++ {
		if s[i] == '.' && s[i+1] == ' ' {
			next := s[i+2]
			if (next >= 'A' && next <= 'Z') || next == '`' {
				last = strings.TrimSpace(s[i+2:])
			}
		}
	}
	return last
}

// stripJSONFragments removes JSON-shaped portions from a model's
// text turn. Two passes:
//
//   - Truncate at the first JSON-block marker: ```json, ``` json,
//     opening { followed by ", or "summary": / "agents": / etc.
//     anything after these is the structured plan.
//   - Drop lines that look like JSON fields (start with leading
//     whitespace + quote, end with comma or close-brace/bracket).
//
// Returns the surviving prose, possibly empty.
func stripJSONFragments(s string) string {
	// Truncate at code-fence start.
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[:i]
	}
	// Truncate at structured-plan markers.
	for _, marker := range []string{`"summary":`, `"agents":`, `"risk_notes":`, `"files":`, `"dont_touch":`, `"prompt":`} {
		if i := strings.Index(s, marker); i >= 0 {
			// Walk backwards to start of the line so we don't
			// keep "{ ... " open-brace context.
			lineStart := i
			for lineStart > 0 && s[lineStart-1] != '\n' {
				lineStart--
			}
			s = s[:lineStart]
			break
		}
	}
	// Drop trailing lines that look like JSON fields.
	lines := strings.Split(s, "\n")
	keep := lines[:0]
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			keep = append(keep, line)
			continue
		}
		// JSON-field heuristic: starts with `"` or `{` or `[`,
		// or ends with `,` `{` `}` `[` `]`. Skip these.
		if strings.HasPrefix(t, `"`) || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			continue
		}
		if last := t[len(t)-1]; last == ',' || last == '{' || last == '}' || last == '[' || last == ']' {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// renderPlanMenu paints the plan-confirmation menu into r.transient
// so it sits live above the input. Three options laid out left-to-
// right; the highlighted one wears a `❯` prefix and bold styling.
// Trailing "← →/Tab" hint reminds the user they can navigate.
//
// No-op when no plan is pending or planChoice is out of range —
// keeps callers from having to nil-check the state.
func (r *REPL) renderPlanMenu() {
	if r.pendingPlan == nil || r.planChoice < 0 {
		return
	}
	labels := []string{"go", "cancel", "feedback…"}
	parts := make([]string, len(labels))
	for i, l := range labels {
		if i == r.planChoice {
			parts[i] = stylePlanChoice.Render("❯ " + l)
		} else {
			parts[i] = styleDim.Render("  " + l)
		}
	}
	hint := styleDim.Render("  (←/→ or Tab to choose, Enter to confirm, ? for details, Esc to cancel; type to leave feedback)")
	menu := strings.Join(parts, "  ") + hint
	if r.planDetailsExpanded {
		r.transient = formatPlanDetails(r.pendingPlan) + "\n" + menu
	} else {
		r.transient = menu
	}
}

// isPlanAffirmative reports whether the user's typed feedback on a
// pending plan should be treated as "execute the plan" rather than
// re-planning. Caught real failure 2026-05-12: user picked
// "feedback" from the menu and typed "go ahead", expecting that to
// be equivalent to picking "go" — but the dispatcher only matched
// the literals "go"/"yes"/"y" and routed "go ahead" through replan,
// re-running the planner with "go ahead" as feedback.
//
// Broad coverage of natural-language affirmations. Trims trailing
// punctuation so "go!", "go.", "yes please." all match.
func isPlanAffirmative(s string) bool {
	s = strings.TrimRight(strings.TrimSpace(s), "!.?")
	switch s {
	case "go", "yes", "y", "ok", "okay", "sure", "do it", "go for it",
		"go ahead", "yes please", "yep", "yeah", "yup", "ship it",
		"make it so", "confirmed", "lgtm", "looks good", "proceed",
		"run it", "let's go", "lets go":
		return true
	}
	return false
}

// isPlanCancel mirrors isPlanAffirmative for the cancel path so
// "no, cancel", "abort it", "nope" all dismiss the plan. Same
// punctuation trimming — "cancel.", "no!", "nope!!" all match.
func isPlanCancel(s string) bool {
	s = strings.TrimRight(strings.TrimSpace(s), "!.?")
	switch s {
	case "cancel", "no", "n", "abort", "nope", "nah", "stop",
		"don't", "do not", "scrap it", "drop it", "never mind", "nevermind":
		return true
	}
	return false
}

// dispatchPlanChoice acts on the selected menu option. Mirrors the
// text-based path in dispatch() so the existing execute / cancel
// flows stay single-source. Feedback choice focuses the textarea
// and clears the menu — the user types, hits Enter, and the
// existing dispatch routes the input as a replan.
func (r REPL) dispatchPlanChoice() (REPL, tea.Cmd) {
	if r.pendingPlan == nil {
		return r, nil
	}
	switch r.planChoice {
	case 0: // go
		plan := r.pendingPlan
		r.executing = true
		r.spinnerText = pickSpinnerPhrase()
		r.spinner.Spinner = pickSpinnerStyle()
		r.clearTransient()
		r.planChoice = -1
		r.renderSpinner()
		return r, tea.Batch(runExecute(r.services, plan), r.spinner.Tick)
	case 1: // cancel
		r.pendingPlan = nil
		r.originalReq = ""
		r.planChoice = -1
		r.clearTransient()
		r.write(styleDim.Render("plan canceled"))
		return r, nil
	case 2: // feedback — drop the menu and let the user type
		r.planChoice = -1
		r.transient = styleDim.Render("type your feedback below, then press Enter to replan (or just hit Enter on an empty line to keep the plan)")
		return r, nil
	}
	return r, nil
}

// appendSeparator queues a blank line so successive turns have
// breathing room in the scrollback. No-op when nothing has been
// printed yet, or when the previous queued entry is already blank.
func (r *REPL) appendSeparator() {
	if len(r.pendingPrints) == 0 {
		return
	}
	if last := r.pendingPrints[len(r.pendingPrints)-1]; last == "" {
		return
	}
	r.pendingPrints = append(r.pendingPrints, "")
}

// renderMarkdown turns markdown into ANSI-styled terminal text using
// glamour. The renderer is reconstructed each call because the pane
// width can change between calls (resize); glamour caches per-width.
//
// We pin the "dark" style rather than using glamour.WithAutoStyle().
// AutoStyle queries the terminal for its background color via an
// OSC 11 sequence; in alt-screen mode (which Bubble Tea uses) the
// terminal's reply leaks back into the visible buffer as garbled
// text like `> 11;rgb:158e/193a/1e75\`. Pinned style avoids the
// query entirely. Light-terminal users can override via a future
// `repl.markdown_style` config — not worth the complexity right now.
func renderMarkdown(md string, width int) (string, error) {
	if width <= 0 {
		width = 80
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return "", err
	}
	return r.Render(md)
}

// wrapWidth returns the column count to wrap at, leaving a tiny
// margin on the right so terminal-edge artifacts don't bite.
func (r *REPL) wrapWidth() int {
	w := r.width
	if w <= 0 {
		w = 80
	}
	if w > 4 {
		w -= 2
	}
	return w
}

// wrapToWidth word-wraps `s` at `width` visible columns. Naïve about
// ANSI escapes (treats them as normal runes) which is acceptable for
// our content — chat replies are plain text and styled prompts are
// short enough that they never overflow. Preserves explicit newlines
// in the input. Falls through unchanged when width <= 0.
func wrapToWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(wrapLine(line, width))
	}
	return out.String()
}

func wrapLine(line string, width int) string {
	if utf8RuneLen(line) <= width {
		return line
	}
	var out strings.Builder
	col := 0
	first := true
	for _, word := range strings.Fields(line) {
		wlen := utf8RuneLen(word)
		switch {
		case first:
			out.WriteString(word)
			col = wlen
			first = false
		case col+1+wlen <= width:
			out.WriteByte(' ')
			out.WriteString(word)
			col += 1 + wlen
		default:
			out.WriteByte('\n')
			out.WriteString(word)
			col = wlen
		}
	}
	return out.String()
}

// utf8RuneLen counts visible runes, not bytes, so a CJK character
// counts as one column. ANSI escapes still pollute the count — but
// the content we wrap (chat replies, tool output) is mostly plain.
func utf8RuneLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// CmdResultMsg is the Bubble Tea message produced when a shell-out
// command completes.
type CmdResultMsg struct {
	Cmd    string
	Stdout string
	Stderr string
	Err    error
}

// runShellCommand invokes `kai <args>` in workDir and returns the
// result as a Bubble Tea message. Wrapped in tea.Cmd so the program
// loop drives it asynchronously and the UI stays responsive.
func runShellCommand(binary, line, workDir string) tea.Cmd {
	return func() tea.Msg {
		args := strings.Fields(line)
		// Bare `kai` no longer launches the TUI (that's `kai code`),
		// so no recursion guard is needed — a child `kai` invocation
		// just prints help unless the user explicitly typed `code`.
		c := exec.Command(binary, args...)
		c.Dir = workDir
		var stdout, stderr bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = &stderr
		err := c.Run()
		return CmdResultMsg{
			Cmd:    line,
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}
}

func formatCmdResult(m CmdResultMsg) string {
	var b strings.Builder
	if s := strings.TrimRight(m.Stdout, "\n"); s != "" {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	if s := strings.TrimRight(m.Stderr, "\n"); s != "" {
		b.WriteString(replError(s))
		b.WriteByte('\n')
	}
	if m.Err != nil && m.Stderr == "" {
		b.WriteString(replError(m.Err.Error()))
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		b.WriteString(replDim("(no output)"))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

var (
	stylePrompt       = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	styleError        = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleDim          = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	historyIdxStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true).Width(5)
	historyEntryStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("254"))
	// styleBashCommand renders the actual command text in the bash
	// approval prompt at full white (color 15) so it stands out
	// against the dim "$" prompt and surrounding metadata. Without
	// an explicit color, some terminal themes render unstyled text
	// as the same gray lipgloss uses elsewhere — making the
	// command itself unreadable, which defeats the whole point of
	// the approval gate.
	styleBashCommand = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	// styleBody renders text in the terminal's default foreground
	// (white/normal). Use it where dim grey would make content
	// harder to read — e.g. supporting evidence bullets.
	styleBody = lipgloss.NewStyle()
	// stylePlannerBanner brackets the planner's final result with
	// a clear visual delimiter. Without this the model's
	// exploration text trails into the trailer and looks like
	// the run died mid-thought. Bold dim cyan reads as a
	// section header without competing with content text.
	stylePlannerBanner = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	// stylePlanChoice highlights the selected option in the plan
	// confirmation menu. Bold + bright cyan reads as "this is the
	// active choice" without being as loud as a full background
	// inversion.
	stylePlanChoice = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	// styleSpinnerLabel matches the spinner's bright cyan so the
	// running-status line reads as a single visual unit. Bold for
	// extra punch — the previous dim grey blended into log output.
	styleSpinnerLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
)

func replGreeting() string {
	return styleDim.Render("kai TUI — /command for kai subcommands, ↑/↓ for history, Esc to switch panes")
}

func replPrompt() string        { return stylePrompt.Render("› ") }
func replError(s string) string { return styleError.Render(s) }
func replDim(s string) string   { return styleDim.Render(s) }

// maybeRenderDailyCapWarning surfaces the kailab daily cost cap
// as a warning trailer line when usage crosses the 80% mark. The
// trailer stays quiet below 80% — under normal usage, the
// per-turn + session-total numbers are enough context. Above
// 80% the user is approaching the hard block and benefits from
// "I have time to plan" rather than "I got cut off mid-task."
//
// No-op when:
//   - the configured provider isn't kailab (BYOM has no cap)
//   - no kailab response has been observed in this process yet
//     (the snapshot would lie about the cap)
//   - the cap is zero or the cost is below 80% of it
//
// Implementation note: we read the snapshot from the live
// provider rather than maintaining a separate cache in the REPL.
// The kailab provider stamps the snapshot on every response, so
// reading it here is always at-most-one-turn-stale.
func (r *REPL) maybeRenderDailyCapWarning() {
	if r.services == nil || r.services.OrchestratorCfg.AgentProvider == nil {
		return
	}
	cost, cap, ok := provider.DailyUsage(r.services.OrchestratorCfg.AgentProvider)
	if !ok || cap <= 0 {
		return
	}
	// 90% threshold. Earlier rev fired at 80% which surfaced at
	// $60/$75 — too noisy when the user still has $14+ of headroom
	// and ample reaction time. 90% gives ~$7.50 of runway, still
	// plenty to react before hard-block but quiet at 80%.
	if cost*100 < cap*90 {
		return
	}
	r.write(styleError.Render(fmt.Sprintf(
		"⚠ Today: $%.2f / $%.2f (kailab daily cap). Set ANTHROPIC_API_KEY + KAI_PROVIDER=anthropic to continue past the cap.",
		float64(cost)/100, float64(cap)/100)))
}

// pendingCostPrompt holds a request that was deferred because the
// session-cost cap was reached. The REPL renders a y/N banner and
// only acts on this struct when the user answers — at which point
// it either dispatches with the original request and forced mode
// (y) or drops it (n / esc).
type pendingCostPrompt struct {
	request  string
	forced   agent.Mode
	cap      float64
	incurred float64
}

// sessionCostCapUSD reads the user-configurable cap from
// KAI_MAX_SESSION_COST_USD. Returns 0 when unset or unparseable
// (no cap). Read at every guard check rather than at startup so
// the user can lower or raise it mid-session via shell tricks
// without restarting the TUI. Cheap.
func sessionCostCapUSD() float64 {
	s := strings.TrimSpace(os.Getenv("KAI_MAX_SESSION_COST_USD"))
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// turnRecord is one entry in REPL.recentTurns. Used by mode 2
// fixxy: when the user complains, we forward the last few
// turns to claude so the fix has context. Kept structural
// rather than serialized so future fixxy modes can reuse the
// data without re-parsing.
type turnRecord struct {
	UserRequest string
	KaiReply    string
	When        time.Time
}

// recordTurn appends a turn to the recent-turns ring (cap 5).
// Called from the chat-reply render path. Old entries drop
// off the front. Cheap (slice with explicit cap).
func (r *REPL) recordTurn(req, reply string) {
	const cap = 5
	if len(r.recentTurns) >= cap {
		r.recentTurns = r.recentTurns[1:]
	}
	r.recentTurns = append(r.recentTurns, turnRecord{
		UserRequest: req,
		KaiReply:    reply,
		When:        time.Now(),
	})
}

// fmtRecentTurns renders the ring as a chronological block
// for the fixxy feedback / review prompts. Each entry shows
// "USER: <req>\nKAI: <reply>\n" with truncation. Empty when
// the ring is empty.
//
// When lastExecutedPlan is set, a <plan> block leads the output
// (plan-preferred context for gate review): the planner's stated
// Summary/Approach/Diagnosis is a higher-fidelity statement of what
// the turn was supposed to do than scraping the chat ring alone.
// The chat ring still follows so the reviewer sees both the intended
// plan and any clarifications the user shouted mid-run.
func (r *REPL) fmtRecentTurns() string {
	var b strings.Builder
	if p := r.lastExecutedPlan; p != nil {
		b.WriteString("<plan>\n")
		if p.Summary != "" {
			fmt.Fprintf(&b, "  summary: %s\n", truncateForLog(p.Summary, 400))
		}
		if p.Approach != "" {
			fmt.Fprintf(&b, "  approach: %s\n", truncateForLog(p.Approach, 400))
		}
		if p.Diagnosis != "" {
			fmt.Fprintf(&b, "  diagnosis: %s\n", truncateForLog(p.Diagnosis, 600))
		}
		if len(p.Agents) > 0 {
			b.WriteString("  agents:\n")
			for _, a := range p.Agents {
				fmt.Fprintf(&b, "    - %s: %s\n", a.Name, truncateForLog(a.Prompt, 240))
			}
		}
		b.WriteString("</plan>\n\n")
	}
	if len(r.recentTurns) == 0 {
		return strings.TrimRight(b.String(), "\n")
	}
	for _, t := range r.recentTurns {
		b.WriteString("USER: ")
		b.WriteString(truncateForLog(t.UserRequest, 400))
		b.WriteByte('\n')
		b.WriteString("KAI:  ")
		b.WriteString(truncateForLog(t.KaiReply, 800))
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncateForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isFixxyFeedbackPhrase reports whether `line` matches the
// mode-2 trigger phrase. Fuzzy on whitespace, punctuation,
// and casing because the user is venting and we shouldn't
// require exact spelling.
//
// Matches "no sir i don't like it", "NO SIR I DONT LIKE IT",
// "no, sir — i don't like it.", etc. Doesn't match unrelated
// "no" / "i don't like X" phrases by accident — it requires
// "sir" + "like" + "no" all present in close-enough order.
func isFixxyFeedbackPhrase(line string) bool {
	low := strings.ToLower(line)
	// Quick reject: must contain all three keywords.
	if !strings.Contains(low, "no") ||
		!strings.Contains(low, "sir") ||
		!strings.Contains(low, "like") {
		return false
	}
	// Order check: "no" before "sir" before "like".
	noPos := strings.Index(low, "no")
	sirPos := strings.Index(low, "sir")
	likePos := strings.Index(low, "like")
	return noPos < sirPos && sirPos < likePos
}

// formatFixxyEvent renders a single fixxy.Event as a dim
// scrollback line. Glyph + prefix make fixxy chatter visually
// distinct from agent activity and trailer info, so the user
// can scan past it when they don't care.
func formatFixxyEvent(ev fixxy.Event) string {
	var glyph string
	switch ev.Kind {
	case "start":
		glyph = "🛠"
	case "rebuild_ok":
		glyph = "✓"
	case "rebuild_fail":
		glyph = "✗"
	case "skipped":
		glyph = "⋯"
	case "done":
		// "done" with empty text is the silent terminator.
		if strings.TrimSpace(ev.Text) == "" {
			return ""
		}
		glyph = "·"
	default:
		glyph = "·"
	}
	return "  " + glyph + " fixxy: " + ev.Text
}

// FixxyEvents returns the secret fixxy worker's event channel
// when --fixxy-upper was passed, or nil otherwise. Wired into
// the TUI bootstrap (internal/tui/app.go Init) so the pump
// starts at session start.
func (r *REPL) FixxyEvents() <-chan fixxy.Event {
	if r.services == nil || r.services.Fixxy == nil {
		return nil
	}
	return r.services.Fixxy.Events()
}

// Fixxy returns the secret fixxy-upper worker pointer (or nil
// when --fixxy-upper wasn't passed). Used by the status bar
// to poll Status() each tick for the persistent "fixxy:
// working (Ns)" indicator.
func (r *REPL) Fixxy() *fixxy.Worker {
	if r.services == nil {
		return nil
	}
	return r.services.Fixxy
}

// --- prompt history persistence -------------------------------------
//
// Per-project file at <kaiDir>/repl_history. One prompt per line.
// Newlines inside multi-line prompts are escaped to `\n` on disk so
// the file stays line-delimited; we unescape on load. Capped at
// replHistoryMax entries — older lines drop off the front when the
// cap is hit. Best-effort throughout: any I/O error silently degrades
// to in-session-only history rather than aborting startup or submit.

const (
	replHistoryFile = "repl_history"
	replHistoryMax  = 1000
)

// loadReplHistory reads the saved prompt history from
// <kaiDir>/repl_history and returns it as a slice. Entries are in
// submission order (oldest first). An empty / missing / unreadable
// file returns nil — the REPL starts with no recall, which matches
// the pre-feature behavior.
func loadReplHistory(workDir string) []string {
	if workDir == "" {
		return nil
	}
	p := filepath.Join(kaipath.Resolve(workDir), replHistoryFile)
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	// Allow long single-line prompts (the agent sometimes gets pasted
	// stack traces of several KB) without bufio's default 64KB cap
	// silently truncating them.
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		if line == "" {
			continue
		}
		out = append(out, unescapeHistoryLine(line))
	}
	if len(out) > replHistoryMax {
		out = out[len(out)-replHistoryMax:]
	}
	return out
}

// appendReplHistory writes a single prompt to the history file. If
// the file would exceed replHistoryMax entries, the oldest are
// dropped via a load+rewrite. Best-effort: any I/O error is dropped.
//
// We rewrite (rather than append-only) when the cap is hit, which is
// O(N) on submit. N ≤ 1000 keeps that cheap; if it ever becomes a
// hot path we can switch to a periodic compactor.
func appendReplHistory(workDir, line string) {
	if workDir == "" || strings.TrimSpace(line) == "" {
		return
	}
	dir := kaipath.Resolve(workDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	p := filepath.Join(dir, replHistoryFile)
	encoded := escapeHistoryLine(line) + "\n"

	// Fast path: append. If after the append the file is over the
	// cap, rewrite with a trimmed slice. Counting via the existing
	// load avoids a second walk.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	if _, err := f.WriteString(encoded); err != nil {
		f.Close()
		return
	}
	f.Close()

	// Cheap line count via the loader — saves writing a separate
	// counting pass and keeps the trim/rewrite logic in one place.
	loaded := loadReplHistory(workDir)
	if len(loaded) <= replHistoryMax {
		return
	}
	trimmed := loaded[len(loaded)-replHistoryMax:]
	tmp := p + ".tmp"
	w, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	for _, l := range trimmed {
		if _, err := w.WriteString(escapeHistoryLine(l) + "\n"); err != nil {
			w.Close()
			os.Remove(tmp)
			return
		}
	}
	if err := w.Close(); err != nil {
		os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, p)
}

// escapeHistoryLine encodes a prompt for line-delimited storage.
// Newlines (alt+enter inside the textarea) become `\n`; literal
// backslashes get doubled so unescape can round-trip cleanly.
func escapeHistoryLine(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// unescapeHistoryLine reverses escapeHistoryLine. Pure inverse;
// any unrecognized `\x` survives as-is rather than getting eaten.
func unescapeHistoryLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ansiCSIPattern matches the ANSI Control Sequence Introducer escapes
// lipgloss + chalk-like libraries emit: ESC [ <params> <terminator>.
// Covers SGR (color + bold + dim, `m` terminator), cursor control,
// erase-in-line, etc. Doesn't try to handle OSC (window title /
// clipboard) or DCS — none of the rendering libraries in use emit
// those in normal output.
var ansiCSIPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripANSI removes ANSI escape sequences from a styled-output blob.
// Used by /copy so pasted output is plain text (Slack, GitHub,
// email, IDEs all render the raw escapes as `[38;5;242m...` garbage
// otherwise). Idempotent: re-stripping a clean string is a no-op.
func stripANSI(s string) string {
	return ansiCSIPattern.ReplaceAllString(s, "")
}
