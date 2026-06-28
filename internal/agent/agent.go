// Package agent is kai's in-process LLM agent runner. It replaces the
// orchestrator's external-subprocess path (exec.Cmd("claude", -p)) so
// kai owns the full agent loop: the LLM call, the tool dispatch, the
// graph context injection, the file-edit hooks.
//
// As of Slice 6 this is the only path the orchestrator drives — the
// external-subprocess fallback (Claude Code, Cursor, etc. via
// exec.Cmd) is gone. The Run signature stays stable so future
// extensions (streaming responses, multi-turn replan) can land here
// without changing the orchestrator's invocation site.
//
// See ../../docs/phase-3-plan.md and the spec at
// ~/.claude/plans/spec-kai-code-frolicking-origami.md for the full
// migration sequence.
package agent

import (
	"context"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/session"
	"github.com/kaicontext/kai-engine/tools"
	"github.com/kaicontext/kai-engine/authorship"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/safetygate"
)

// Hooks lets the orchestrator observe agent activity without coupling
// the agent package to the TUI. Each callback fires from the runner's
// goroutine; receivers must not block (use non-blocking channel sends
// or buffered queues).
type Hooks struct {
	// OnFileChange fires after the agent's view/edit/write tools
	// modify a file in the workspace. The path is relative to the
	// workspace root; op is "created" / "modified" / "deleted" so it
	// matches `internal/orchestrator/observer.go`'s vocabulary.
	OnFileChange func(relPath, op string)

	// OnFileBroadcast fires after a successful write or edit with
	// the file's content (base64-encoded) and content digest. The
	// orchestrator wires this to `remote.SyncPushFile` so other
	// agents subscribed to the live-sync channel see the change in
	// real time. Distinct from OnFileChange so callers that only
	// need notification (e.g. TUI sync pane) don't have to deal with
	// content payload memory churn.
	//
	// digest may be empty — the kailab side computes its own hash
	// when blank, but supplying one lets the receiver dedupe quickly.
	OnFileBroadcast func(relPath, digest, contentBase64 string)

	// OnAssistantText fires when the model emits user-visible text.
	// The TUI surfaces it inline as the agent narrates its work.
	OnAssistantText func(text string)

	// OnProviderState fires for every HTTP/SSE lifecycle transition
	// of the underlying provider call (sent → connected → streaming
	// → done|error, plus stream_idle/stream_resumed during long
	// generations). Lets the TUI render real call state instead of
	// inferring "is this stuck?" from derived events. Fires
	// synchronously from the provider goroutine; do not block.
	OnProviderState func(state provider.RequestState)

	// OnAssistantDelta fires once per text-delta chunk while the
	// model is still streaming a response. Lets the TUI render
	// assistant prose live as it arrives instead of waiting for
	// the whole turn to complete. No-op when the provider doesn't
	// support streaming.
	OnAssistantDelta func(delta string)

	// OnToolCall fires when the model dispatches a tool. Useful for
	// the sync pane to render a "called kai_callers(file=router.go)"
	// breadcrumb.
	OnToolCall func(name, inputJSON string)

	// OnRoutingTrace fires for every routing decision a tool makes —
	// which project a file path resolved to, which projects a
	// kai_grep walked, which DB a graph tool queried. The runner
	// installs the tracer at session start via
	// tools.SetRoutingTracer and clears it on return; tools emit via
	// tools.TraceRouting. Nil disables — same convention as
	// OnToolCall. Used to diagnose multi-root routing problems.
	OnRoutingTrace func(msg string)

	// OnTurnComplete fires after each provider response with the
	// run's cumulative token counts. The TUI uses this to animate a
	// live token counter as turns complete (the agent's billed
	// usage climbs from 0 → final over the run). Fires once per
	// model call regardless of whether the model also issued tool
	// calls, so the counter ticks as work progresses.
	//
	// tokensCached sums the run's cache_read + cache_creation
	// usage across turns (when the provider supports prompt
	// caching). Surfacing it lets the TUI show "X in (+ Y cached)"
	// so users can see prompt caching working — and demand a
	// refund when it isn't.
	OnTurnComplete func(tokensIn, tokensOut, tokensCached int)

	// OnFileDiff fires after each successful write/edit with a
	// unified diff (`--- a/path` / `+++ b/path` + `+/-/space`
	// lines) plus pre-counted additions/removals. The TUI renders
	// this as an inline "Update(path) — Added N lines" block,
	// mirroring Claude Code's per-edit display. Distinct from
	// OnFileChange so consumers that just need notification don't
	// pay for diff computation.
	OnFileDiff func(relPath, op, unifiedDiff string, added, removed int)

	// OnBashOutput fires once per line of bash stdout/stderr while a
	// command is running, so the TUI can stream long-running
	// commands (brew install, npm test, go test ./...) inline
	// instead of leaving the user staring at a frozen pane.
	OnBashOutput func(line string)

	// OnGateVerdict fires after every workspace mutation with the
	// safety gate's classification (auto / review / block) plus
	// blast radius and human-readable reasons. Lets the TUI render
	// kai's per-edit verdict inline ("auto ✓ — 0 downstream", "held
	// ⚠ — 3 callers affected"). Fires once per mutation event:
	// once per write/edit tool call, and once per batch of files
	// touched by a single bash invocation.
	OnGateVerdict func(paths []string, verdict string, blastRadius int, reasons []string)

	// OnRetryWait fires when the runner encounters a transient
	// upstream error (rate limit, overload, network reset) and is
	// about to back off before retrying. attempt is 1-indexed;
	// delay is the pause before the next attempt; err is the
	// underlying failure. The TUI surfaces this as "Rate limited,
	// retrying in 4s..." so the user knows the loop is alive
	// rather than wondering if the agent has hung.
	OnRetryWait func(attempt int, delay time.Duration, err error)

	// OnRequest fires immediately before each provider.Send/SendStream
	// call with the FULL request the runner is about to ship. Used
	// for debug logging — observability into "what did the model
	// actually receive on this turn", including the per-turn
	// injected project overview, system prompt, tools list, and
	// full message history. Receivers must not block (called from
	// the runner's goroutine in the hot path) and must not mutate
	// the Request (the runner uses it after this returns).
	//
	// Fires once per turn, regardless of whether the turn ends in
	// tool_use or end_turn. Does NOT fire on the recovery-retry
	// after a context-overflow compaction — that re-send carries
	// a different (smaller) Messages slice and would double-count.
	OnRequest func(turn int, req provider.Request)

	// OnBashConfirm fires for each bash command that doesn't match the
	// auto-approve allowlist. The implementation prompts the user
	// (TUI: yellow approval line; CLI mode: stdin) and returns true to
	// run, false to cancel. Cancellation is reported back to the model
	// as a non-error tool response so it can re-plan rather than retry.
	// nil disables the prompt — bash falls back to allowlist-only
	// gating (current default for headless CLI usage).
	//
	// warning is a non-empty label when the command matched a
	// destructive pattern (e.g. "may recursively force-remove files"
	// for `rm -rf`). The prompt should display it prominently above
	// the command. Empty warning means no label.
	OnBashConfirm func(cmd, warning string) bool

	// OnFileConfirm fires for each write/edit before it lands on
	// disk. Same shape as OnBashConfirm. op is "create" or "edit";
	// path is the workspace-relative target; added/removed are
	// line-count previews. Returning false aborts the write with a
	// non-error response so the agent can adjust. nil disables the
	// prompt — writes happen immediately (current headless default).
	OnFileConfirm func(op, path string, added, removed int, diff string) bool
}

// Options configures one agent run.
type Options struct {
	// Projects is the multi-root workspace the agent operates in.
	// When non-nil, file tools (view/write/edit) route per-path to
	// the owning project and the prompt advertises every active
	// root. Backwards-compat: if Projects is nil but Workspace is
	// set, the runner constructs a synthetic single-project Set
	// from Workspace, so existing callers and tests keep working
	// unchanged.
	//
	// V1 limitation: graph tools (kai_callers/dependents/context/
	// impact/symbols) and the safety gate currently use the
	// primary project's DB and config regardless of which project
	// a queried path lives in. Per-project graph and gate routing
	// is a tracked follow-up.
	Projects *projects.Set

	// Workspace is the absolute path to the spawn dir (CoW workspace)
	// the agent should treat as its working directory. Tools resolve
	// paths relative to this — not against process cwd.
	//
	// In multi-root flows callers should populate Projects instead;
	// Workspace is then derived from Projects.Primary().Path. Tests
	// and single-root callers can keep setting Workspace directly.
	Workspace string

	// Prompt is the system+user prompt the planner produced. The
	// runner splits a leading "System: ..." block off as the system
	// role; everything else is the user turn. Future revisions can
	// pass an explicit []Message instead.
	Prompt string

	// Model is the Anthropic model id (e.g. "claude-sonnet-4-6"). If
	// empty the runner picks a sensible default.
	Model string

	// MaxTokens caps a single LLM call's response. Defaults to a
	// reasonable per-turn budget if zero.
	MaxTokens int

	// MaxTotalTokens caps cumulative token use across all turns in
	// this run. 0 disables the cap. Wired to the orchestrator's
	// MaxAgentTokens field.
	MaxTotalTokens int

	// MaxTurns overrides the runner's default turn cap (50). Set
	// lower for workloads that should converge fast — the planner
	// agent uses ~12 because pure-exploration loops past 10 turns
	// almost always devolve into "let me check one more thing"
	// without ever producing the requested output. The runner
	// injects a convergence reminder a few turns before the cap so
	// the model knows it has limited room left.
	MaxTurns int

	// KeepToolResults, when true, disables the runner's "trim
	// older tool results to a one-line stub" optimization. That
	// optimization was added pre-caching to keep re-sent tool
	// results from re-billing on every turn. With prompt caching
	// on the conversation history (kailab provider since 2026-05),
	// the entire prefix is served from cache at ~10% of normal
	// input cost, so the trim is no longer worth its downside:
	// when an exploration agent loses sight of files it already
	// viewed, it re-views them, blowing both the turn cap and
	// the tool-call budget. The planner agent sets this to true.
	KeepToolResults bool

	// Provider is the LLM transport. Required. Typically a
	// `provider.Kailab` wrapping the user's bearer token.
	Provider provider.Provider

	// ConsultProvider is the LLM transport used by kai_consult to
	// escalate stuck explorations to a stronger model. When nil (or
	// when ConsultModel below is empty), kai_consult is silently
	// omitted from the tool registry — the agent then has no
	// escalation path. Production wiring (cmd/kai/tui.go) typically
	// reuses the main Provider here and only differs on ConsultModel.
	ConsultProvider provider.Provider
	// ConsultModel is the model id kai_consult invokes via
	// ConsultProvider. Empty disables the tool. Default in
	// production wiring is "claude-sonnet-4-6".
	ConsultModel string

	// KailabBaseURL + KailabToken authorize kai_web_search against the
	// kai-server Brave proxy at ${KailabBaseURL}/api/v1/search. Both
	// must be set for the tool to register; either missing silently
	// omits it. Threaded through from cmd/kai/{tui,headless}.go where
	// the auth-login credentials are resolved.
	KailabBaseURL string
	KailabToken   string

	// ManagedProcLogger, when non-nil, enables the kai_logs tool so
	// the chat agent can read recent output from the TUI's managed
	// dev-server process. Nil silently omits the tool (chat without
	// the TUI, orchestrator-spawned agents). The TUI wires this in
	// runChatAgent.
	ManagedProcLogger tools.ManagedProcLogger

	// ExtraTools is the optional list of pre-built tools to register
	// alongside the default file tools. Used for one-off tools the
	// caller wants to bolt on; the standard kai_* graph tools come
	// from Graph below.
	ExtraTools []tools.BaseTool

	// Graph is the main repo's graph DB. When set, the runner
	// registers kai_callers, kai_dependents, kai_context as tools
	// the model can call to reason about call structure mid-edit.
	// nil disables those tools (e.g. tests that don't need them).
	Graph *graph.DB

	// EnableBash registers the `bash` tool. Default off so tests
	// that don't need shell access never accidentally execute
	// commands. Production wiring (cmd/kai/tui.go) sets this true.
	EnableBash bool

	// ReadOnly registers only the view tool from the file set
	// (write and edit are skipped). Used by the chat-fallback path
	// where the agent should be able to inspect the workspace
	// (`ls`, view a file) without risking modifications.
	ReadOnly bool

	// SharedPaths is the session-scoped allowlist of paths OUTSIDE
	// the workspace that the user has explicitly shared via the
	// TUI's /share command. Read-only tools (view, kai_grep,
	// kai_files, kai_tree) check this list when their workspace-
	// scoped path resolution fails — letting the agent read design
	// docs in ~/Downloads or reference code in another repo without
	// the user having to copy files in. Write/edit tools refuse
	// these paths regardless; the boundary is read-only.
	SharedPaths []string

	// BashAllow is the optional first-token allowlist enforced by
	// the bash tool. Empty (with EnableBash=true) means "no
	// restriction". Only consulted when EnableBash is true.
	BashAllow []string

	// GateConfig drives in-loop safety classification. When non-zero
	// (BlockThreshold > 0), every file mutation the agent makes —
	// via write/edit tools and via bash — is run through
	// safetygate.Classify against opts.Graph; the verdict (auto /
	// review / block) fires Hooks.OnGateVerdict so the TUI can
	// render it inline. Block verdicts also revert the offending
	// edit before returning the tool result so the model sees the
	// rollback. Leave Graph nil or BlockThreshold=0 to disable.
	GateConfig safetygate.Config

	// SessionStore, when set, persists every turn (assistant +
	// tool-result) to the kai DB so the conversation survives
	// process restarts. nil disables persistence; the run lives
	// only in memory. The orchestrator passes its main DB here
	// (graph.DB satisfies the session.Store interface).
	SessionStore session.Store

	// SessionID, when set, resumes an existing conversation
	// instead of starting fresh. The runner loads History() to
	// seed the model with prior turns. Empty + non-nil
	// SessionStore creates a new session row.
	SessionID string

	// UserVisibleHistoryOnly, when true, has resolveSession load
	// session.UserVisibleHistory() instead of the raw History().
	// The view drops system messages and synthesizes a one-line
	// summary per tool-call cluster — appropriate for chat agents
	// resuming a session that may have been written to by a
	// planner / executor (whose JSON dispatches + plan emits
	// degrade chat-agent recall). Other tasks should leave this
	// false to keep the unfiltered transcript. 2026-05-26 spec
	// item #3.
	UserVisibleHistoryOnly bool

	// TaskName is recorded on the session row for "what was this
	// agent supposed to do" lookups later. Optional; defaults to
	// "" if unset. The orchestrator threads run.Task.Name here.
	TaskName string

	// Hooks plugs in the orchestrator's observers.
	Hooks Hooks

	// NoVerify suppresses the orchestrator's auto-verify pass after
	// this run completes. The orchestrator sets this true on the
	// verify pass itself so a verify-of-a-verify can't cascade —
	// otherwise a debug-mode run that applies more fixes would loop.
	// Default false: a debug-mode run that touches files triggers
	// one follow-up verify pass.
	NoVerify bool

	// InjectedContext, when non-empty, is delivered to the model as
	// the body of a synthetic context_lookup tool result inserted
	// immediately after the user prompt. Used by the graph-powered
	// context injection (see internal/planner/context_inject.go) to
	// hand the agent pre-resolved entry points and call chains
	// before it starts searching. The model sees a tool_use + tool_
	// result pair as if it had performed the lookup itself; the
	// context_lookup tool is registered as a no-op so the model
	// can't actually re-invoke it. Empty means no injection.
	InjectedContext string

	// NoAbsenceGuard disables the in-loop guard that intercepts an
	// agent's final answer when it reads as a negative claim ("X
	// doesn't exist") that's backed by fewer than 3 relevant
	// searches. Default false: every agent run is guarded, fires at
	// most once per run. Set true for tests that intentionally
	// produce thin negative answers, or for paths where the cost of
	// one extra turn outweighs the false-negative risk.
	NoAbsenceGuard bool

	// NoHallucinationGuard disables the in-loop guard that
	// intercepts an agent's final answer when it names files not
	// present in the conversation context. Default false: every
	// agent run is guarded, fires at most once per run. Set true
	// for tests that intentionally feed file-mentions without
	// tool results, or to bypass when the response is known not
	// to be filename-shaped (e.g. pure planning passes).
	NoHallucinationGuard bool

	// MaxReadsPerTurn caps the number of read-only tool calls (view,
	// kai_grep, kai_*) the model is allowed to dispatch within a
	// single turn. Calls beyond the cap get intercepted with a
	// "too many reads in this turn" block message, same splice
	// mechanic as the streak hard-block. Zero means uncapped — the
	// chat agent and planner stay uncapped; the orchestrator sets
	// this to 5 (or scope-aware) on spawned worker agents. The
	// streak counter (turn-grain) and this cap (call-grain) are
	// complementary: the streak prevents many read-only turns; the
	// cap prevents a single turn from issuing dozens of reads.
	MaxReadsPerTurn int

	// ReadStreakSoft / ReadStreakHard override the default per-run
	// read-streak thresholds (5 / 10). Zero means "use the default".
	// The orchestrator sets these per-agent based on the agent's
	// declared scope: a one-file task gets tight limits (2 / 5)
	// because there's no legitimate reason to explore wider; a
	// multi-file refactor keeps the default room. Soft fires every
	// turn the streak is at or above the soft value AND below the
	// hard value; hard intercepts read-only calls.
	ReadStreakSoft int
	ReadStreakHard int

	// NoBuildAfterEdit disables the in-loop build check that runs
	// after every successful write/edit. Default false: per-edit
	// builds run when KAI_SKIP_BUILD_AFTER_EDIT is unset, the
	// workspace exposes a recognized manifest (go.mod / tsconfig.json
	// / Cargo.toml), and the edit touched a file in that ecosystem's
	// extension set. Set true for unit tests that don't want exec
	// calls landing during runner exercises, and for the planner
	// agent (which doesn't emit code edits).
	NoBuildAfterEdit bool

	// NoDangleGuard disables the in-loop guard that intercepts a
	// coding-mode agent's final answer when it describes the change
	// it should have made instead of calling write/edit. Default
	// false: every coding-mode run is guarded, fires at most once
	// per run. Set true for tests that intentionally produce
	// description-shaped output, or for non-coding runs where the
	// deliverable is prose (the guard is gated on Mode==Coding
	// internally, so leaving it false is also safe in those cases).
	NoDangleGuard bool

	// GroundAnswers turns on the search guard: a terminal, prose-only
	// answer with no search AND no edits behind it gets sent back to
	// search the codebase first (fires once per run; pleasantries
	// exempt). Set true by the interactive in-process answer path
	// (runChatAgent) so chat-style questions can't be answered from
	// priors. Left false for background/spawned workers, whose
	// deliverable is edits — the dangle guard covers those.
	GroundAnswers bool

	// OutputJSONSchema, when non-nil, constrains the model's FINAL
	// text response to match this JSON Schema (Anthropic's
	// `output_config` structured-outputs path). Tool calls during
	// exploration turns are unaffected. The planner sets this so its
	// WorkPlan JSON is guaranteed-valid instead of fished out of a
	// markdown fence. Providers that don't support structured outputs
	// (the OpenAI-shaped route, Together) silently ignore it.
	OutputJSONSchema map[string]interface{}

	// RequireToolUseFirstTurn, when true, instructs the provider to
	// enforce tool use via the API's tool_choice parameter on the
	// FIRST turn of this run. The model is structurally required to
	// emit a tool call before any plain-text/JSON response — soft
	// prompt rules ("please make a tool call") that opus-4-6 ignores
	// become impossible to ignore at the API level.
	//
	// Currently honored by: kailab provider (sets Anthropic's
	// {"type":"any"}). OpenAI-shaped providers silently ignore; the
	// caller falls back to soft enforcement there. After turn 1
	// (i.e. once a tool call has been observed) the runner drops
	// back to default tool_choice so the model can emit its final
	// response in turn 2+.
	RequireToolUseFirstTurn bool

	// NoTests suppresses the orchestrator's auto-test pass. Set true
	// on (a) the verify pass itself, (b) the test pass itself, and
	// (c) when the user passed --no-tests or has agent.auto_test:
	// false in their config. Default false: a coding-mode run that
	// touches non-test source triggers one follow-up test pass after
	// verify completes.
	NoTests bool

	// KaiBinary is the absolute path to the kai executable used by
	// the kai_diff tool to shell out for semantic diffs. Empty
	// silently omits kai_diff from the registry.
	KaiBinary string

	// CheckpointWriter, when non-nil, enables kai_checkpoint to
	// record per-edit authorship. The runner doesn't construct one;
	// callers (cmd/kai/tui.go, orchestrator) thread it in alongside
	// SessionStore. nil omits the tool.
	CheckpointWriter *authorship.CheckpointWriter

	// LiveSyncClient + SyncChannelID configure kai_live_sync. Both
	// must be set for the tool to register. Single-agent chat
	// sessions leave both nil.
	LiveSyncClient tools.LiveSyncClient
	SyncChannelID  string

	// Mode shapes the system prompt and tool whitelist for this run.
	// ModeUnknown (the zero value) resolves to ModeCoding via
	// ResolveMode — the safe full-tool default. Orchestrator-spawned
	// agents set this from AgentTask.Mode (parsed via ParseMode); the
	// REPL sets it from DetectMode on the developer's input. See
	// docs/prompt-modes.md.
	Mode Mode

	// RunLogDir, when non-empty, enables per-turn structured run
	// logging. The runner writes <RunLogDir>/runs/<sessionID>/<turn>.json
	// for every model call, capturing prompt-section sizes, hashes,
	// usage breakdown, and tool-call outcomes. Drives `kai run last`
	// and `kai run diff` for after-the-fact debugging. Empty disables
	// (matches SessionStore's nil-is-fine convention).
	RunLogDir string
}

// Result captures everything the run produced for the caller (the
// orchestrator's `runOneAgent`) to consume.
type Result struct {
	// Transcript is the full message history. When SessionStore is
	// set the same content has also been persisted to the DB; the
	// in-memory slice is just a convenience for the immediate caller.
	Transcript []message.Message

	// FinishReason matches the last model turn's reason. Most runs
	// end with EndTurn; ToolUse here would indicate the runner gave
	// up mid-loop, which is a bug.
	FinishReason message.FinishReason

	// FinalText is the assistant's final user-visible text from THIS
	// run only — the answer this run produced. On a resumed session
	// Transcript is the whole conversation, so a caller that wants
	// the current turn's answer MUST read this field; walking
	// Transcript backward for the last non-empty assistant message
	// returns the PRIOR turn's answer whenever the current turn
	// produced an empty completion — a silent replay of stale text.
	// Empty when the run errored or genuinely produced no text.
	FinalText string

	// TokensCached accumulates cache_read + cache_creation across
	// all model calls in the run. Reported separately from
	// TokensIn so the TUI / orchestrator can show how much of the
	// prompt was served from Anthropic's prompt cache.
	//
	// TokensCacheCreate / TokensCacheRead split the same total for
	// honest cost reporting: on Sonnet 4.6, creation bills at
	// ~1.25× normal input cost while reads bill at ~10%, so the
	// effective cost is dominated by the create:read ratio. A
	// trailer that lumped them would consistently under- or over-
	// state cost depending on what dominated.
	TokensCached       int
	TokensCacheCreate  int
	TokensCacheRead    int

	// TokensIn / TokensOut accumulate across all model calls in the
	// run. Plumbed for budget accounting (orchestrator.Config.MaxAgentTokens).
	TokensIn  int
	TokensOut int

	// ProviderCostUSD sums provider-reported real cost (OpenRouter
	// usage.cost) across model calls; RequestCount is the number of calls.
	ProviderCostUSD float64
	RequestCount    int

	// SessionID is the id of the persisted session row (empty when
	// no SessionStore was provided). Callers can pass this back as
	// Options.SessionID on a future Run to resume the conversation.
	SessionID string

	// AbsenceGuardFired is true when the in-loop absence guard
	// (Layer 2) intercepted a negative claim during this run and
	// nudged the agent to do more searches. Reported up to the
	// orchestrator's measurement hook so the graph-context-
	// injection over-claiming signal can be tracked.
	AbsenceGuardFired bool

	// HallucinationGuardFired is true when the in-loop hallucination
	// guard intercepted the model naming files not present in the
	// conversation context. Surfaced for the same telemetry reasons
	// as AbsenceGuardFired — both signals quantify how often the
	// model is being kept honest by the safety nets vs. answering
	// well on its own.
	HallucinationGuardFired bool

	// BuildSuccessGuardFired is true when the in-loop build-success
	// hallucination guard intercepted the model narrating "build
	// succeeded" / "tests pass" while its most recent bash command
	// exited non-zero. Round-21 dogfood produced this exact pattern;
	// surfaced for the same telemetry reasons as the other guards.
	BuildSuccessGuardFired bool

	// DangleGuardFired is true when the in-loop dangle guard
	// intercepted a coding-mode "described instead of edited"
	// terminal turn. Tracked alongside the other guard signals so
	// we can measure how often the safety net is closing the gap
	// vs. the model getting it right on its own.
	DangleGuardFired bool

	// ConversationSearchGuardFired is true when the in-loop
	// conversation search guard intercepted a chat-mode answer that
	// tried to finalize without grounding it in any codebase tool
	// call. Tracked alongside the other guard signals so we can
	// measure how often chat answers needed to be sent back to search.
	ConversationSearchGuardFired bool

	// TruncatedAnswerGuardFired is true when the in-loop truncated-
	// answer guard intercepted a reasoning model's reply that trailed
	// off mid-thought (ended on an ellipsis) and sent it back to finish.
	TruncatedAnswerGuardFired bool

	// InjectedContextChars is the length, in bytes, of the
	// Options.InjectedContext that was spliced into the transcript
	// (zero when injection didn't fire). Used by the measurement
	// hook to correlate injection size with downstream signals
	// (locality / correctness / over-claiming).
	InjectedContextChars int
}

// Run executes a single agent task in-process. Returns when the model
// emits an EndTurn turn (or hits an error / cancellation / token
// budget cap / max-turns guard).
//
// Slice 1: full agent loop wired in. For the orchestrator's invocation
// pattern (one-shot per AgentTask), call Run once per task and inspect
// Result.FinishReason.
func Run(ctx context.Context, opts Options) (*Result, error) {
	return runLoop(ctx, opts)
}
