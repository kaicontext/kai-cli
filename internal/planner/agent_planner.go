package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"kai/internal/agent"
	"github.com/kaicontext/kai-engine/message"
	"kai/internal/agent/provider"
	"github.com/kaicontext/kai-engine/session"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/projects"
	"github.com/kaicontext/kai-engine/promptenv"
	"kai/internal/safetygate"
	"kai/internal/tasksmd"
)

// plannerMaxTotalTokens caps total tokens (in+out, including cache)
// for one planner agent run on a CACHED provider (kailab,
// anthropic-direct). Calibrated for the cache-hit case: with
// prompt caching, accumulated tool-results re-send at ~10% cost,
// so 300k tokens of context across 12 turns remains affordable.
//
// 2026-05-26 dogfood: a kai-desktop chat planning turn crossed into
// the kai/ monorepo to look up `func runStatus`, read main.go
// (~40k tokens on its own), and tripped the prior 200k cap mid-
// exploration with "agent: token budget exceeded (used 252668, cap
// 200000)". Cross-project planning that touches very large source
// files needs more headroom; 300k is the new ceiling. The per-turn
// read cap and read-streak block (runner.go:524) still bound
// individual turn cost, so 300k doesn't translate to a 50% larger
// $$ exposure — just a wider window before the hard stop.
//
// Hitting the cap terminates the run cleanly via the agent runner's
// existing budget guard, so the user sees an error with the debug
// log path instead of silently burning $$$ in a loop.
const plannerMaxTotalTokens = 300_000

// plannerMaxTotalTokensCacheless is the cap when the provider has
// NO prompt cache (openai, local). Every turn re-bills the full
// prompt at full cost, so the same exploration budget would cost
// ~10× more. Cut both the per-turn budget and the turn count to
// keep total session cost in the same ballpark — the planner
// compacts and re-plans earlier, which is the right tradeoff when
// each accumulated turn is genuinely expensive.
//
// 50k tokens / 8 turns / starting budget. Numbers are starting
// points pending the tuning runs in §2 of the BYOM follow-up spec.
const (
	plannerMaxTotalTokensCacheless = 50_000
	plannerMaxTurnsCacheless       = 8
	// plannerMaxTurnsCached: empirically, cached-provider planning
	// runs fill whatever turn budget they're given. Two consecutive
	// 2026-05-26 dogfoods of the same "hook up the snapshot count"
	// request (v0.32.47 with description-only nudges, v0.32.48 with
	// the runner-injected search-without-bash nudge) both ran to
	// turn 12 — the model commits only when forced. Reducing to 10
	// trims the exploration tail without breaking complex multi-
	// repo refactors (which still get 10 turns; their honest budget
	// was never 12 either). The wind-down hint fires at 3 turns
	// left, so the model still gets two "wrap up cleanly" turns
	// before commit, same as before — just earlier in absolute
	// terms.
	plannerMaxTurnsCached = 10
)

// plannerBudget returns the per-run token budget appropriate for
// the configured provider. Cached providers get the full 200k
// budget; cacheless drop to 50k. We branch off the live provider
// (via the SupportsCache type-assertion helper) instead of
// shipping a config field so the runtime decision can't drift from
// the actual provider behavior.
func plannerBudget(p provider.Provider) int {
	if provider.SupportsCache(p) {
		return plannerMaxTotalTokens
	}
	return plannerMaxTotalTokensCacheless
}

func plannerTurnCap(p provider.Provider) int {
	if provider.SupportsCache(p) {
		return plannerMaxTurnsCached
	}
	return plannerMaxTurnsCacheless
}

// workPlanJSONSchema is the Anthropic structured-outputs schema that
// constrains the planner's terminal text block to a parseable WorkPlan.
// Built against Anthropic's grammar restrictions:
//
//   - every object declares additionalProperties: false (required)
//   - every declared property is listed in `required` (no optional
//     fields — empty strings / empty arrays represent "absent")
//   - no recursion, no numerical / string constraints, no patterns
//
// We keep the field set identical to the Go WorkPlan struct so the
// downstream JSON unmarshal is a no-op. When the planner routes to
// a non-Anthropic model (Together, etc.) this schema is dropped on
// the floor in the provider layer and the fenced-JSON extractor
// remains the parser of record.
func workPlanJSONSchema() map[string]interface{} {
	stringArray := map[string]interface{}{
		"type":  "array",
		"items": map[string]interface{}{"type": "string"},
	}
	evidenceObject := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file":         map[string]interface{}{"type": "string"},
			"line_start":   map[string]interface{}{"type": "integer"},
			"line_end":     map[string]interface{}{"type": "integer"},
			"excerpt":      map[string]interface{}{"type": "string"},
			"annotation":   map[string]interface{}{"type": "string"},
			"content_hash": map[string]interface{}{"type": "string"},
		},
		"required":             []string{"file", "line_start", "line_end", "excerpt", "annotation", "content_hash"},
		"additionalProperties": false,
	}
	verifyCheckObject := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"run":                    map[string]interface{}{"type": "string"},
			"expect_exit":            map[string]interface{}{"type": "integer"},
			"expect_stdout_contains": map[string]interface{}{"type": "string"},
			"why":                    map[string]interface{}{"type": "string"},
		},
		"required":             []string{"run", "expect_exit", "expect_stdout_contains", "why"},
		"additionalProperties": false,
	}
	agentObject := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name":                map[string]interface{}{"type": "string"},
			"prompt":              map[string]interface{}{"type": "string"},
			"files":               stringArray,
			"dont_touch":          stringArray,
			"mode":                map[string]interface{}{"type": "string"},
			"acceptance_criteria": stringArray,
			"evidence": map[string]interface{}{
				"type":  "array",
				"items": evidenceObject,
			},
			"verify_checks": map[string]interface{}{
				"type":  "array",
				"items": verifyCheckObject,
			},
		},
		"required":             []string{"name", "prompt", "files", "dont_touch", "mode", "acceptance_criteria", "evidence", "verify_checks"},
		"additionalProperties": false,
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"summary":   map[string]interface{}{"type": "string"},
			"diagnosis": map[string]interface{}{"type": "string"},
			"approach":  map[string]interface{}{"type": "string"},
			"agents": map[string]interface{}{
				"type":  "array",
				"items": agentObject,
			},
			"risk_notes": stringArray,
			"trivial":    map[string]interface{}{"type": "boolean"},
		},
		"required":             []string{"summary", "diagnosis", "approach", "agents", "risk_notes", "trivial"},
		"additionalProperties": false,
	}
}

// plannerConstrainsOutput reports whether the planner should send the
// WorkPlan JSON schema as an output_config structured-output
// constraint. It is hardwired to false.
//
// The 2026-05-15 dogfood proved the schema is actively harmful to the
// PLANNER specifically. The planner is an explore-THEN-emit loop: its
// prompt demands a read tool call on turn 1, exploration, and only
// then a plan. A whole-run output_config constraint short-circuits
// that — a capable model (claude-opus-4-6) sees it can satisfy the
// run by emitting a schema-conforming JSON object immediately, so on
// turn 1 it emits `{"agents":[],...}` (empty) or a hallucinated plan,
// having explored nothing. Worse, output_config and the
// RequireToolUse reprompt (tool_choice:any) are contradictory
// instructions; when both are sent the schema wins and the
// "you MUST call a tool" reprompt is silently defeated, so the
// planner cannot recover. Qwen never hit this only because it is not
// a claude- model and so was never sent the schema.
//
// extractFencedJSON remains the parser of record, so dropping the
// schema costs parse-convenience, not correctness.
//
// modelSupportsStructuredOutputs below is kept — it correctly reports
// model capability and is the right gate for any future caller that
// constrains a single, non-exploratory completion. The planner is
// just not that caller.
func plannerConstrainsOutput() bool { return false }

// plannerFinalizeConstrainsOutput reports whether a single-turn,
// no-exploration planner turn (the finalization turn, or a one-turn
// reprompt) should constrain its output to the WorkPlan schema.
//
// The multi-turn exploration loop stays unconstrained — see the
// comment block above — so the model can freely call tools. But a
// one-shot turn whose ONLY job is to emit the plan JSON is exactly
// what schema-constrained output is for, and it is required for
// models that don't reliably honor the "emit fenced JSON" instruction
// (observed: Kimi-K2.6 returns a prose Markdown plan and dead-ends
// hands-off mode). Together normalizes response_format:json_schema
// across its whole catalog; Anthropic 4.x uses output_config;
// claude-3-* has no support and falls back to the plain fenced ask.
func plannerFinalizeConstrainsOutput(model string) bool {
	return !strings.HasPrefix(model, "claude-") || modelSupportsStructuredOutputs(model)
}

// modelSupportsStructuredOutputs returns true for Anthropic models on
// which `output_config` is GA per platform.claude.com docs (2026-05).
// Conservative match: any `claude-*` id ships through the Anthropic
// path in kailab, but the structured-outputs API is only documented
// for the 4.x family. Older model ids fall back to the fenced-JSON
// extractor unchanged. Together/OpenAI models route through the
// OpenAI-shaped endpoint and never set this field regardless.
func modelSupportsStructuredOutputs(model string) bool {
	if !strings.HasPrefix(model, "claude-") {
		return false
	}
	// Match 4.x family explicitly. Opus 4.5/4.6/4.7, Sonnet 4.5/4.6,
	// Haiku 4.5 — all current ids contain "-4-" between family and
	// version. claude-3-* (older) is excluded.
	return strings.Contains(model, "-4-")
}

// PlannerAgent is the agent-loop replacement for the one-shot Plan()
// function. Where Plan() does a single LLM completion against a
// pre-built context blob and parses the result as JSON, PlannerAgent
// runs the full agent loop in ModePlanning so the model can call
// kai_grep, kai_files, kai_context, kai_callers, view, etc. to look
// at the actual codebase before producing a plan.
//
// This is the planner version of the same dogfooding the chat agent
// already does: kai's value proposition is that an agent should know
// the code before acting, and the planner is the most visible place
// where "knowing the code" matters. A one-shot Plan() that
// hallucinates file paths because it never read the tree is exactly
// the failure mode this fixes.
//
// Session continuity: when SessionID is non-empty on Run, the agent
// loop resumes the prior planning conversation. Follow-ups like
// "what parts of it are already done?" then resolve "it" against
// the previous turn instead of being treated as a standalone vague
// request.
type PlannerAgent struct {
	// Provider is the LLM transport. Required. Typically the
	// shared *provider.Kailab the chat agent also uses.
	Provider provider.Provider

	// Model is the Anthropic model id. Empty falls back to the
	// runner's default ("claude-sonnet-4-6").
	Model string

	// FinalizeModel, when set, overrides Model for the single-turn
	// finalize pass — the call that converts accumulated exploration
	// into the structured WorkPlan JSON. Lets the user pair a smart-
	// but-slow exploration model (e.g. DeepSeek-V4-Pro at ~30 tok/s)
	// with a fast writer (Kimi-K2.6 at ~80 tok/s, GLM-5.1 at ~60)
	// for the emission phase. The 2026-05-27 dogfood pinned the
	// shape: a 7m07s planner run spent ~3m on the final two turns
	// alone (drafting + emitting the JSON). Swapping just that
	// terminal call to a fast writer trims ~30% off wall-clock with
	// no change to the exploration loop. Empty falls back to Model.
	//
	// Set via KAI_PLANNER_FINALIZE_MODEL env var (config layer
	// reads it; see internal/config). Quality risk is real — fast
	// writers can drift back to wishful field-guessing if they
	// don't faithfully consume the prior turns' verified facts.
	// The OutputJSONSchema constraint on the finalize call mitigates
	// shape drift; semantic drift is on the user to evaluate.
	FinalizeModel string

	// Set is the multi-root workspace the planner explores. Required.
	// File-routing tools dispatch through Set.ProjectFor; the primary
	// project's root is used as the agent's cwd.
	Set *projects.Set

	// GateConfig surfaces protected globs to the planner so it never
	// asks an executor agent to touch a path the gate would block.
	GateConfig safetygate.Config

	// Cfg controls plan shape (MaxAgents cap, model id, max tokens).
	// Mirrors what the original Plan() consumed.
	Cfg Config

	// SessionStore, when set, persists the planning conversation so
	// SessionID on a follow-up Run resumes it. Without a store,
	// every turn is fresh — same as the legacy Plan() path.
	SessionStore session.Store

	// OnThinking, when non-nil, is called every time the model
	// emits an assistant text turn during exploration. The TUI
	// uses it to surface the planner's "let me look at X / I'll
	// check Y" narration as a live dim line below the spinner so
	// the user sees what the planner is working on. Plain text
	// per turn — caller decides whether to keep the full text or
	// truncate to a single sentence.
	//
	// Best-effort: hook is called from the agent loop's goroutine,
	// so the receiver must be safe for concurrent calls and must
	// not block.
	OnThinking func(text string)

	// OnToolCall, when non-nil, fires once for every tool dispatch
	// the planner makes during exploration. Lets the TUI render
	// "→ kai_grep …" / "→ view …" lines in scrollback so the user
	// can see what the planner is doing on turns that emit no text
	// (pure tool-call turns produce no OnThinking signal otherwise).
	// Same non-blocking-receiver contract.
	OnToolCall func(name, input string)

	// OnProviderState, when set, forwards every HTTP/SSE lifecycle
	// transition of the planner's underlying provider call. Lets
	// the TUI render real call state instead of inferring from
	// derived events. Same non-blocking-receiver contract as
	// OnThinking.
	OnProviderState func(state provider.RequestState)

	// RunLogDir, when non-empty, enables per-turn runlog artifacts
	// for the planner agent (same plumbing the chat agent uses).
	// Lets `kai run summary` include planner spend in the cost
	// row — important for benchmarking, since the planner can
	// outweigh executor agents on simple tasks.
	RunLogDir string

	// KailabBaseURL + KailabToken authorize kai_web_search. Without
	// both set, the tool isn't registered for the planner and it has
	// to reason about external facts ("was option X removed in
	// TypeScript Y", "latest version of Z") from training-data
	// priors. The chat agent's path threads these from the same
	// source (OrchestratorCfg.KailabBaseURL/Token); the planner
	// historically didn't, so live web lookup was silently
	// unavailable on the planning track. 2026-05-25.
	KailabBaseURL string
	KailabToken   string
}

// PlannerResult is what Run returns. Mirrors the relevant subset of
// agent.Result so the REPL can show token usage and resume the
// session on the next turn.
type PlannerResult struct {
	Plan *WorkPlan

	// Reply, when non-empty, carries the planner's prose response
	// in the case where the model declined to emit a JSON plan
	// (e.g. "the work is already done — here's what I found").
	// The REPL surfaces it as a chat-style reply rather than
	// failing the request: the model's analysis is useful even
	// when it doesn't fit the WorkPlan schema.
	Reply string

	SessionID         string
	TokensIn          int
	TokensOut         int
	TokensCached      int
	TokensCacheCreate int
	TokensCacheRead   int
}

// Run executes one planner turn as an agent loop. The model is
// expected to:
//
//  1. Optionally explore the codebase via read-only tools
//     (kai_grep, kai_files, kai_context, view, etc.) to find what
//     already exists for the requested change.
//  2. Emit a final assistant message containing a single fenced JSON
//     block matching the WorkPlan schema.
//
// repromptKind names the recovery strategy validatePlan picks when
// it rejects a plan attempt. Each kind corresponds to a specific
// reprompt method on PlannerAgent.
type repromptKind int

const (
	repromptNone repromptKind = iota
	// repromptConcreteAgents fires when len(Agents) == 0 on an
	// imperative request. Asks the model to produce at least one
	// concrete agent task.
	repromptConcreteAgents
	// repromptExploration fires when the plan has agents but the
	// model made zero tool calls — the diagnosis is presumptively
	// hallucinated. Asks the model to verify against the code
	// before re-emitting.
	repromptExploration
	// repromptForcedToolUse is the hard-enforcement variant: uses
	// the Anthropic API's tool_choice parameter (when supported by
	// the provider) to make the model literally unable to emit JSON
	// without first making a tool call. Used as the second-line
	// defense when a softer reprompt already failed once.
	repromptForcedToolUse
	// repromptAlreadyDoneVerify fires when the planner returned an
	// "already implemented" verdict but the audit found it cited
	// symbols in its diagnosis that it never actually viewed —
	// the classic round-18 failure where the planner said "the
	// handler at line X calls renderPlanMenu()" without ever
	// reading renderPlanMenu's body. The reprompt forces the model
	// to view the unverified symbols before re-emitting either
	// "already done" (with evidence) or a dispatch plan that
	// closes the gap it found.
	repromptAlreadyDoneVerify
)

// planAction is the outcome validatePlan asks Plan() to take next.
type planAction int

const (
	// planAcceptCurrent: the plan as-passed is dispatchable. Stop
	// the validation loop and proceed to post-processing.
	planAcceptCurrent planAction = iota
	// planAcceptAsEmpty: the plan should be surfaced to the user as
	// an empty-agents result (chat-fallback "Answered" rendering).
	// Used when the summary is non-trivial but no work is needed,
	// or when reprompts have been exhausted on an imperative.
	planAcceptAsEmpty
	// planRouteVague: route to ErrTooVague so the REPL falls back
	// to a conversational chat reply rather than rendering an empty
	// plan.
	planRouteVague
	// planReprompt: re-run the planner with the indicated reprompt
	// strategy.
	planReprompt
)

// planVerdict is the structured decision validatePlan returns for
// one plan attempt. Plan() consumes this in a loop, applying the
// indicated action and re-validating after each reprompt.
type planVerdict struct {
	Action       planAction
	Retry        repromptKind // only meaningful when Action == planReprompt
	RejectReason string       // for the debug log

	// UnverifiedSymbols is populated when Retry == repromptAlreadyDoneVerify.
	// Names the cited-but-not-viewed symbols the verify reprompt should
	// ask the planner to look at before re-confirming "already done."
	UnverifiedSymbols []string
}

// validatePlan inspects ONE plan attempt (the parsed/filtered
// WorkPlan plus the transcript that produced it) and decides what
// happens next. Centralizes every dispatch-acceptance check so the
// Plan() loop applies the same logic uniformly to the initial
// attempt and to any reprompted result. Adding a new defense means
// adding a case here; the loop, retry tracking, and token
// accumulation in Plan() don't need to change.
//
// Pre-conditions: plan is non-nil and has already been passed
// through filterStubAgents (so len(Agents) reflects the post-
// filter count).
//
// The verdict's Retry field is only consulted when Action ==
// planReprompt; otherwise it's repromptNone.
func validatePlan(plan *WorkPlan, transcript []message.Message, request string) planVerdict {
	imperative := looksLikeImperativeChange(request)
	toolCalls := countAssistantToolCalls(transcript)

	// Empty-agents branch: the plan has nothing to dispatch.
	if len(plan.Agents) == 0 {
		// The model explicitly said "I can't plan this" — route to
		// chat-fallback. Same intent as the legacy ErrTooVague path.
		if isVagueRefusal(plan.Summary) {
			return planVerdict{Action: planRouteVague, RejectReason: "vague refusal in summary"}
		}
		// "Already implemented" is a legitimate empty-agents outcome.
		// The user sees the summary as the answer. But: audit the
		// claim. Round-18 dogfood (2026-05-13) — planner said the
		// `?` toggle was "already implemented" because it saw the
		// handler call `r.renderPlanMenu()`. It never read
		// renderPlanMenu, which doesn't actually look at the toggle
		// flag — so the visible behavior was still broken. The
		// planner even flagged this hypothesis in its risk_notes
		// ("the bug may be in renderPlanMenu") and STILL returned
		// done. If the diagnosis cites a symbol the planner never
		// viewed, OR the risk_notes contain doubt-phrases, force a
		// verification reprompt instead of accepting.
		if isAlreadyDoneSummary(plan.Summary) {
			unverified, doubt := auditAlreadyDone(plan, transcript)
			if len(unverified) > 0 || doubt {
				reason := "already-done verdict cites unverified symbols"
				if doubt {
					reason = "already-done verdict has self-doubting risk notes"
				}
				return planVerdict{
					Action:            planReprompt,
					Retry:             repromptAlreadyDoneVerify,
					RejectReason:      reason,
					UnverifiedSymbols: unverified,
				}
			}
			return planVerdict{Action: planAcceptAsEmpty, RejectReason: "already-done"}
		}
		// Imperative request with empty agents: this is the
		// dangling-plan failure. Reprompt to produce a real agent.
		if imperative {
			return planVerdict{Action: planReprompt, Retry: repromptConcreteAgents, RejectReason: "empty agents for imperative request"}
		}
		// Non-imperative, non-vague, non-done — return the prose as
		// an empty-agents answer if there's any content; else route
		// to vague.
		if strings.TrimSpace(plan.Summary) != "" || len(plan.RiskNotes) > 0 {
			return planVerdict{Action: planAcceptAsEmpty, RejectReason: "non-imperative with summary/notes"}
		}
		return planVerdict{Action: planRouteVague, RejectReason: "empty everything"}
	}

	// Non-empty agents — the plan looks dispatchable on the surface.
	// Check for hallucination signals before accepting.
	if imperative && toolCalls == 0 {
		return planVerdict{Action: planReprompt, Retry: repromptExploration, RejectReason: "imperative plan with zero tool calls (likely hallucinated)"}
	}

	return planVerdict{Action: planAcceptCurrent}
}

// We parse the last assistant text for the JSON block, validate it,
// and return. ErrTooVague comes back when the model emits an empty
// agents list (its way of saying "this isn't actionable as-is");
// the REPL falls back to a chat reply in that case, same as before.
//
// sessionID, when non-empty, resumes a prior planning conversation
// so follow-ups inherit context.
func (p *PlannerAgent) Run(ctx context.Context, request, sessionID string) (*PlannerResult, error) {
	if p == nil {
		return nil, fmt.Errorf("planner: nil PlannerAgent")
	}
	if p.Provider == nil {
		return nil, fmt.Errorf("planner: Provider required")
	}
	if p.Set == nil || p.Set.Primary() == nil {
		return nil, fmt.Errorf("planner: projects.Set required")
	}
	request = strings.TrimSpace(request)
	if request == "" {
		return nil, fmt.Errorf("planner: empty request")
	}

	// TASKS.md ledger: append the active list to the request so the
	// planner has the same "what's in flight, what's queued" view
	// the chat runner injects per turn. Silent on Load error — never
	// fail a planner run on a parse glitch. Workspace = primary
	// project's path; multi-root cross-project tasks are out of scope
	// for v1 (see docs/tasks-md-spec.md "Open questions").
	if tm, err := tasksmd.Load(p.Set.Primary().Path); err == nil {
		if extra := tm.FormatForPrompt(); extra != "" {
			request = request + "\n\n" + extra
		}
	}

	prompt := buildPlannerPrompt(request, p.GateConfig, p.Cfg, p.Set)

	// Open the debug log against the primary project's kai dir.
	// Best-effort: if it can't open, dbg is nil and all the log
	// methods become no-ops. Path is NOT printed to stderr —
	// Bubble Tea owns the screen and stderr writes corrupt the
	// rendering (duplicate spinners, stray characters, etc.).
	// Discoverable instead at <KaiDir>/planner-debug.log; the TUI
	// surfaces it on demand (see `kai plan log` or the help
	// text).
	dbg, _ := OpenDebugLog(p.Set.Primary().KaiDir, request)
	defer dbg.Close()

	// Session-resume probe (2026-05-25). When this run inherits a
	// sessionID from a prior chat-mode exchange, log how many
	// messages were carried over. The dogfood reported "mode-switch
	// context-loss" — chat agent diagnoses something, user types
	// /code then a non-affirmative follow-up, planner asks "what
	// to fix?" as if it never saw the chat. Two distinct causes
	// would produce the same symptom: (1) sessionID not threaded
	// across the mode switch (REPL state bug), (2) sessionID
	// loaded fine but planner's system prompt overrides the chat
	// history (model-attention bug). This probe disambiguates:
	// `loaded N messages` → wiring is fine, model is the problem;
	// `loaded 0 messages` → wiring broke.
	if sessionID != "" && p.SessionStore != nil {
		if s, err := session.Resume(p.SessionStore, sessionID); err == nil {
			if hist, herr := s.History(); herr == nil {
				dbg.Text(fmt.Sprintf("session-resume: id=%s task=%s loaded %d messages", sessionID, s.TaskName, len(hist)))
			} else {
				dbg.Text(fmt.Sprintf("session-resume: id=%s loaded — but History() failed: %v", sessionID, herr))
			}
		} else {
			dbg.Text(fmt.Sprintf("session-resume: id=%s Resume failed: %v", sessionID, err))
		}
	} else {
		dbg.Text("session-resume: no sessionID — fresh planner run")
	}

	var outputSchema map[string]interface{}
	if plannerConstrainsOutput() {
		outputSchema = workPlanJSONSchema()
	}
	res, err := agent.Run(ctx, agent.Options{
		Projects:         p.Set,
		Workspace:        p.Set.Primary().Path,
		Prompt:           prompt,
		Model:            p.Model,
		Provider:         p.Provider,
		ReadOnly:         true,
		Mode:             agent.ModePlanning,
		EnableBash:       true, // planner verifies external contracts (kai stats --json, etc.) before emitting
		GateConfig:       p.GateConfig,
		SessionStore:     p.SessionStore,
		SessionID:        sessionID,
		TaskName:         "planner",
		RunLogDir:        p.RunLogDir,
		KailabBaseURL:    p.KailabBaseURL,
		KailabToken:      p.KailabToken,
		OutputJSONSchema: outputSchema,
		MaxTotalTokens:   plannerBudget(p.Provider),
		// Hard cap on planner exploration. The runner injects a
		// convergence reminder ~3 turns before the cap, then a
		// FINAL TURN demand on the last one. Cached providers get
		// 12 turns (thorough explore-then-plan); cacheless drop to
		// 8 because each turn pays the full re-send price and the
		// total session cost matters more than max thoroughness.
		MaxTurns: plannerTurnCap(p.Provider),
		// Don't trim tool results: with prompt caching, re-sending
		// the full transcript is ~free, and trimming old view
		// results to one-line stubs makes the planner re-view
		// files it already saw (we observed runner.go viewed 15
		// times in one run). Tool-result trimming was a
		// pre-caching optimization; for cached agent loops it
		// causes more cost than it saves.
		KeepToolResults: true,
		Hooks: agent.Hooks{
			OnToolCall: func(name, input string) {
				dbg.Tool(name, input)
				if p.OnToolCall != nil {
					p.OnToolCall(name, input)
				}
			},
			OnRoutingTrace: dbg.Routing,
			OnAssistantText: func(text string) {
				dbg.Text(text)
				if p.OnThinking != nil {
					p.OnThinking(text)
				}
			},
			OnTurnComplete: func(in, out, cached int) {
				dbg.Turn(in, out, cached)
			},
			OnRetryWait: func(attempt int, delay time.Duration, err error) {
				dbg.Retry(attempt, delay, err)
			},
			// REQUEST dump for the planner path mirrors the
			// chat-agent path's instrumentation. Lets us
			// answer "what did the planner actually send to
			// the model on turn N" — the project-overview
			// injection, tool-result history, system prompt
			// drift across turns. Grep .kai/planner-debug.log
			// for "REQUEST" to see one entry per turn.
			OnRequest: func(turn int, req provider.Request) {
				dbg.Request(turn, req)
			},
			OnProviderState: p.OnProviderState,
		},
	})
	if err != nil {
		dbg.Errorf("agent run: %v", err)
		return nil, fmt.Errorf("planner: agent run: %w", err)
	}

	plan, perr := extractPlanFromTranscript(res.Transcript)
	if perr != nil {
		// Forced finalization: the model produced text but no
		// JSON — typically a "let me check..." cliffhanger from
		// hitting the turn cap mid-exploration. We do ONE more
		// agent.Run that resumes the same session, gives the
		// model NO tools, and demands a final answer with the
		// evidence already gathered. This is cheap (single
		// turn, mostly cached prefix) and turns a useless
		// cliffhanger into either a real plan or a definitive
		// "I can't" reply.
		fres, ferr := p.finalize(ctx, res.SessionID, dbg)
		if ferr == nil && fres != nil {
			// Combine token usage from both calls so the
			// trailer reflects the full cost.
			fres.TokensIn += res.TokensIn
			fres.TokensOut += res.TokensOut
			fres.TokensCached += res.TokensCached
			fres.TokensCacheCreate += res.TokensCacheCreate
			fres.TokensCacheRead += res.TokensCacheRead
			return fres, nil
		}

		// Finalization failed — surface the original prose as
		// a Reply rather than nothing. The user at least sees
		// the model's last words.
		var unparseable *ErrUnparseable
		if errors.As(perr, &unparseable) && strings.TrimSpace(unparseable.Raw) != "" {
			return &PlannerResult{
				Reply:             strings.TrimSpace(unparseable.Raw),
				SessionID:         res.SessionID,
				TokensIn:          res.TokensIn,
				TokensOut:         res.TokensOut,
				TokensCached:      res.TokensCached,
				TokensCacheCreate: res.TokensCacheCreate,
				TokensCacheRead:   res.TokensCacheRead,
			}, nil
		}
		return nil, perr
	}
	// Unified validation loop: every plan attempt (initial + any
	// reprompts) flows through validatePlan, which returns one of:
	//   - planAcceptCurrent → exit loop, post-process, return
	//   - planAcceptAsEmpty → return as empty-agents plan (chat-fallback render)
	//   - planRouteVague    → return with ErrTooVague (chat fallback)
	//   - planReprompt      → run indicated reprompt, retry-track, loop
	//
	// Token usage accumulates across all attempts so the trailer
	// reflects the full cost. Retry-tracking prevents firing the
	// same reprompt twice (which would be a loop with a model that
	// keeps producing the same failure).
	plan.Agents = filterStubAgents(plan.Agents)

	const maxValidationAttempts = 4 // initial + at most 3 reprompts
	attempted := map[repromptKind]bool{}
	var verdict planVerdict

	for attempt := 0; attempt < maxValidationAttempts; attempt++ {
		verdict = validatePlan(plan, res.Transcript, request)
		if verdict.RejectReason != "" {
			dbg.Errorf("validatePlan: %s", verdict.RejectReason)
		}

		if verdict.Action != planReprompt {
			break // terminal verdict — exit loop, handle below
		}
		if attempted[verdict.Retry] {
			// Already tried this reprompt; the model didn't recover
			// from it. Demote to a terminal verdict: the request was
			// imperative + we couldn't get a verified plan, so route
			// to chat-fallback rather than dispatching junk.
			dbg.Errorf("validatePlan: reprompt %v already attempted; demoting to empty/vague fallback", verdict.Retry)
			// Tag the plan so the UI can render "planner failed"
			// instead of the misleading "Answered" / empty-plan
			// state. Without this the user sees an empty plan and
			// can't tell whether the task was deemed already-done
			// or the planner just couldn't produce a usable plan.
			plan = annotatePlannerFailure(plan, verdict.RejectReason, verdict.Retry)
			if strings.TrimSpace(plan.Summary) != "" || len(plan.RiskNotes) > 0 {
				verdict = planVerdict{Action: planAcceptAsEmpty}
			} else {
				verdict = planVerdict{Action: planRouteVague}
			}
			break
		}
		attempted[verdict.Retry] = true

		newRes, rerr := p.dispatchReprompt(ctx, verdict, res.SessionID, request, dbg)
		if rerr != nil || newRes == nil {
			dbg.Errorf("validatePlan: reprompt %v failed: %v; falling back", verdict.Retry, rerr)
			if strings.TrimSpace(plan.Summary) != "" || len(plan.RiskNotes) > 0 {
				verdict = planVerdict{Action: planAcceptAsEmpty}
			} else {
				verdict = planVerdict{Action: planRouteVague}
			}
			break
		}

		newPlan, nperr := extractPlanFromTranscript(newRes.Transcript)
		if nperr != nil {
			dbg.Errorf("validatePlan: reprompt parse failed: %v; falling back", nperr)
			verdict = planVerdict{Action: planRouteVague}
			break
		}

		// Accumulate tokens from the reprompt onto the running res.
		// We keep `res` as the "latest" attempt so validatePlan and
		// the post-loop return path always see the freshest transcript
		// and session id. Token deltas accumulate across all attempts.
		res.TokensIn += newRes.TokensIn
		res.TokensOut += newRes.TokensOut
		res.TokensCached += newRes.TokensCached
		res.TokensCacheCreate += newRes.TokensCacheCreate
		res.TokensCacheRead += newRes.TokensCacheRead
		res.Transcript = newRes.Transcript
		res.SessionID = newRes.SessionID

		newPlan.Agents = filterStubAgents(newPlan.Agents)
		plan = newPlan
		// Loop iterates: re-validate against the new plan + transcript.
	}

	// Terminal verdicts: act now.
	switch verdict.Action {
	case planRouteVague:
		return &PlannerResult{
			SessionID:         res.SessionID,
			TokensIn:          res.TokensIn,
			TokensOut:         res.TokensOut,
			TokensCached:      res.TokensCached,
			TokensCacheCreate: res.TokensCacheCreate,
			TokensCacheRead:   res.TokensCacheRead,
		}, ErrTooVague
	case planAcceptAsEmpty:
		return &PlannerResult{
			Plan:              plan,
			SessionID:         res.SessionID,
			TokensIn:          res.TokensIn,
			TokensOut:         res.TokensOut,
			TokensCached:      res.TokensCached,
			TokensCacheCreate: res.TokensCacheCreate,
			TokensCacheRead:   res.TokensCacheRead,
		}, nil
	}
	// planAcceptCurrent — fall through to post-processing below.

	if p.Cfg.MaxAgents > 0 && len(plan.Agents) > p.Cfg.MaxAgents {
		plan.Agents = plan.Agents[:p.Cfg.MaxAgents]
		plan.RiskNotes = append(plan.RiskNotes,
			fmt.Sprintf("plan truncated to MaxAgents=%d", p.Cfg.MaxAgents))
	}
	// Belt-and-suspenders consolidation. The system prompt nudges
	// the model toward a single agent for small work, but it's
	// stochastic. Post-process catches the cases where the model
	// still over-decomposes: 2+ agents touching ≤3 distinct files
	// total. Merge into one agent that handles the tasks
	// sequentially. Telemetry on plan.RiskNotes (and the dbg log)
	// lets us measure how often the post-process fires — if >20%
	// the prompt rule isn't working; if <5% we can drop the
	// post-process and simplify.
	if collapsed, before, totalFiles := maybeCollapseSmallPlan(plan); collapsed {
		dbg.Errorf("plan post-process: collapsed %d agents → 1 (total files: %d)", before, totalFiles)
		plan.RiskNotes = append(plan.RiskNotes,
			fmt.Sprintf("planner emitted %d agents for small work (%d files); collapsed to 1 sequential agent to avoid duplicate prompt-setup cost",
				before, totalFiles))
	}
	// Trivial guard. The Trivial flag skips the user-confirm step
	// entirely (repl.go:2429 auto-runs the plan). The prompt rule
	// (agent_planner.go:1742) says trivial ONLY for single-file,
	// handful-of-lines, low-risk changes — but the 2026-05-26
	// dogfood pinned a 4-file plan with new IPC wiring and async
	// prop-drilling shipped as trivial:true, which would have
	// auto-executed without review. Demote when the plan obviously
	// outgrows the trivial envelope. Conservative thresholds: any
	// of >1 agent, >2 files across all agents, or any agent prompt
	// >500 chars demotes. The user still sees the plan card and
	// chooses whether to run it.
	if plan.Trivial {
		if reason, demote := shouldDemoteTrivial(plan); demote {
			dbg.Errorf("plan post-process: demoted trivial=true → false (%s)", reason)
			plan.Trivial = false
			plan.RiskNotes = append(plan.RiskNotes,
				fmt.Sprintf("planner marked trivial:true but the plan exceeds the trivial envelope (%s); demoted so the user sees the confirm step", reason))
		}
	}
	return &PlannerResult{
		Plan:              plan,
		SessionID:         res.SessionID,
		TokensIn:          res.TokensIn,
		TokensOut:         res.TokensOut,
		TokensCached:      res.TokensCached,
		TokensCacheCreate: res.TokensCacheCreate,
		TokensCacheRead:   res.TokensCacheRead,
	}, nil
}

// annotatePlannerFailure rewrites the plan's user-visible summary
// and prepends a structured RiskNote so the UI can distinguish
// "planner exhausted retries" from "task already done" — both
// previously rendered as an empty-agents plan with no signal to
// the user. The reject reason (when present) names what failed
// validation; the retry kind names which guard fired.
//
// Plan.Agents is left empty: by definition we never produced a
// usable plan, and downstream dispatch should remain a no-op.
func annotatePlannerFailure(plan *WorkPlan, rejectReason string, _ repromptKind) *WorkPlan {
	if plan == nil {
		plan = &WorkPlan{}
	}
	header := plannerFailureHeader
	if rejectReason != "" {
		header = fmt.Sprintf("%s — %s", plannerFailureHeader, rejectReason)
	}
	plan.RiskNotes = append([]string{header}, plan.RiskNotes...)
	// Replace the summary so the chat row says "planner failed"
	// instead of whatever empty/garbled value the model emitted.
	plan.Summary = plannerFailureSummary
	plan.Agents = nil
	return plan
}

// plannerFailureHeader is the first RiskNote on a failed plan.
// Stable string — UIs and tests match on this prefix to recognize
// the failure mode.
const plannerFailureHeader = "planner failed after reprompts: emitted invalid/empty plans"

// plannerFailureSummary replaces the plan's summary on failure.
// The TUI renders this in the same slot it would otherwise show
// the "already implemented" / "Answered" string, so users see
// the real outcome.
const plannerFailureSummary = "planner failed — no plan produced. Re-run, switch planner model (e.g. /model claude-sonnet-4-6), or check ~/.kai/planner-debug.log"

// maybeCollapseSmallPlan merges multiple agents into one when the
// combined work is small enough that the per-agent prompt setup
// cost exceeds what a single sequential agent would pay. Heuristic
// for "small": total distinct files across all agents ≤ 3. File
// count is a proxy for work size — loose for v1; later iterations
// can swap in estimated edit-line count once telemetry shows where
// the real break-even lives.
//
// Returns (collapsed, originalAgentCount, totalFiles) so the
// caller can log + add risk_notes for telemetry.
//
// Edits plan.Agents in place: the merged agent's prompt becomes a
// numbered list of the original agents' prompts; files and
// dont_touch unions; name combines the originals to keep the TUI
// trace readable.
// shouldDemoteTrivial reports whether a plan marked Trivial:true
// exceeds the "single-file, handful-of-lines, low-risk" envelope
// the trivial flag is reserved for (see planner prompt
// agent_planner.go:1742). Triggers if any of: >1 agent, >2 distinct
// files across all agents, any agent prompt > 500 chars (a sign the
// work is complex enough that the agent needs detailed instructions
// — not a one-line fix). Returns the demotion reason for telemetry.
func shouldDemoteTrivial(plan *WorkPlan) (string, bool) {
	if plan == nil {
		return "", false
	}
	if len(plan.Agents) > 1 {
		return fmt.Sprintf("%d agents", len(plan.Agents)), true
	}
	seen := map[string]bool{}
	maxPromptLen := 0
	for _, a := range plan.Agents {
		for _, f := range a.Files {
			seen[f] = true
		}
		if n := len(a.Prompt); n > maxPromptLen {
			maxPromptLen = n
		}
	}
	if len(seen) > 2 {
		return fmt.Sprintf("%d distinct files", len(seen)), true
	}
	if maxPromptLen > 500 {
		return fmt.Sprintf("agent prompt %d chars (>500)", maxPromptLen), true
	}
	return "", false
}

func maybeCollapseSmallPlan(plan *WorkPlan) (bool, int, int) {
	if plan == nil || len(plan.Agents) < 2 {
		return false, len(plan.Agents), 0
	}
	// Count distinct files across all agents.
	fileSet := map[string]bool{}
	for _, a := range plan.Agents {
		for _, f := range a.Files {
			fileSet[f] = true
		}
	}
	totalFiles := len(fileSet)
	const collapseFileThreshold = 3
	if totalFiles > collapseFileThreshold {
		return false, len(plan.Agents), totalFiles
	}
	// Build the merged agent.
	originals := plan.Agents
	mergedFiles := make([]string, 0, totalFiles)
	for f := range fileSet {
		mergedFiles = append(mergedFiles, f)
	}
	sort.Strings(mergedFiles)
	dontTouchSet := map[string]bool{}
	for _, a := range originals {
		for _, f := range a.DontTouch {
			dontTouchSet[f] = true
		}
	}
	mergedDontTouch := make([]string, 0, len(dontTouchSet))
	for f := range dontTouchSet {
		mergedDontTouch = append(mergedDontTouch, f)
	}
	sort.Strings(mergedDontTouch)
	// Numbered-list prompt: easier for the executor to reason
	// about as a sequence than a wall of paragraphs. Each item
	// keeps the original sub-agent's intent verbatim — we don't
	// re-write the prompts because that's where the model put its
	// reasoning about each fix.
	var promptB strings.Builder
	promptB.WriteString("Sequential tasks (do them in order; commit after each):\n")
	for i, a := range originals {
		fmt.Fprintf(&promptB, "\n%d. %s\n", i+1, a.Name)
		fmt.Fprintf(&promptB, "   %s\n", strings.TrimSpace(a.Prompt))
		if len(a.Files) > 0 {
			fmt.Fprintf(&promptB, "   files: %s\n", strings.Join(a.Files, ", "))
		}
	}
	mergedName := joinAgentNames(originals)
	plan.Agents = []AgentTask{{
		Name:      mergedName,
		Prompt:    strings.TrimRight(promptB.String(), "\n"),
		Files:     mergedFiles,
		DontTouch: mergedDontTouch,
		Mode:      pickMergedMode(originals),
	}}
	return true, len(originals), totalFiles
}

// joinAgentNames produces a human-readable combined name from the
// merged agents' names. Stays under 80 chars (tasks-list rendering)
// by truncating once total exceeds; downstream readers still see
// the originals via the numbered-list prompt.
func joinAgentNames(agents []AgentTask) string {
	parts := make([]string, 0, len(agents))
	for _, a := range agents {
		parts = append(parts, a.Name)
	}
	joined := strings.Join(parts, "+")
	if len(joined) > 60 {
		// Truncate with ellipsis — keep first names readable;
		// the prompt body has the full list anyway.
		return joined[:57] + "..."
	}
	return joined
}

// pickMergedMode picks the mode for the merged agent. If all
// originals share a mode, use it. Otherwise default to coding —
// the safest "do edits" mode and the most common.
func pickMergedMode(agents []AgentTask) string {
	if len(agents) == 0 {
		return ""
	}
	first := agents[0].Mode
	for _, a := range agents[1:] {
		if a.Mode != first {
			return "coding"
		}
	}
	return first
}

// isVagueRefusal reports whether the summary text indicates the
// model decided "I can't plan this — input was too vague" rather
// than producing a real answer. When true, the empty-agents plan
// routes to ErrTooVague → chat fallback so the user gets a
// conversational reply (vs. a "Couldn't plan this" headline that
// reads as a failure for what's actually a friendly greeting).
//
// Markers cover the phrasings Claude / gpt-4o / Qwen all use
// when the model is structurally producing a WorkPlan but
// signaling refusal in the summary.
func isVagueRefusal(summary string) bool {
	low := strings.ToLower(strings.TrimSpace(summary))
	if low == "" {
		return false
	}
	for _, marker := range []string{
		"too vague",
		"can't plan",
		"cannot plan",
		"no concrete",
		"unable to plan",
		"insufficient",
		"unclear",
		// Greeting / chitchat phrasings. Without these the model's
		// honest "this is a greeting" summary gets rendered as a
		// "No work to plan — awaiting concrete request" headline,
		// instead of routing to the chat fallback. Reported as a
		// dogfood bug on 2026-05-11: "hi how are you" hit the
		// planner output instead of getting a conversational reply.
		"greeting",
		"chitchat",
		"small talk",
		"no code change",
		"not a code",
		"no work to plan",
		"awaiting",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// imperativeChangeRequestRE matches the request shapes that should
// never legitimately produce agents=[]. A user who typed "make X
// toggleable" or "fix the Y bug in Z" is asking for a code change
// with a named target; if the planner returns empty agents on that,
// it's the dangling-plan failure mode the dangle guard (worker
// layer) and concrete-agents rule (system prompt) both exist to
// prevent. Used by repromptForConcreteAgents to decide whether to
// re-ask the planner with a coaching nudge.
//
// Patterns are deliberately over-inclusive: a false positive costs
// one extra planning turn (the model gets a re-ask and produces the
// same empty plan with the same summary, which we then accept). A
// false negative lets the original failure through. Asymmetric cost
// → biased toward firing.
var imperativeChangeRequestRE = regexp.MustCompile(
	`(?i)\b(?:make|add|fix|change|update|rename|remove|delete|toggle|wire\s+up|implement|support|enable|disable|hide|show|move|extract|inline|refactor|replace|swap|introduce|expose|drop|allow|prevent|block|gate|guard)\b`,
)

// looksLikeImperativeChange reports whether the user's request reads
// as an actionable code-change ask (vs. a greeting, a question, or
// genuinely-vague prose).
func looksLikeImperativeChange(request string) bool {
	if strings.TrimSpace(request) == "" {
		return false
	}
	return imperativeChangeRequestRE.MatchString(request)
}

// dispatchReprompt picks the right reprompt method for the given
// kind and returns the raw agent.Result so Plan()'s loop can
// re-validate. Centralizing dispatch here keeps Plan() free of
// switch noise and lets new reprompt strategies slot in without
// touching the loop body.
func (p *PlannerAgent) dispatchReprompt(ctx context.Context, verdict planVerdict, sessionID, originalRequest string, dbg *DebugLog) (*agent.Result, error) {
	switch verdict.Retry {
	case repromptConcreteAgents:
		return p.repromptForConcreteAgents(ctx, sessionID, originalRequest, dbg)
	case repromptExploration:
		return p.repromptForExploration(ctx, sessionID, originalRequest, dbg)
	case repromptAlreadyDoneVerify:
		return p.repromptForAlreadyDoneVerify(ctx, sessionID, originalRequest, verdict.UnverifiedSymbols, dbg)
	default:
		return nil, fmt.Errorf("planner: unknown reprompt kind %d", verdict.Retry)
	}
}

// repromptForConcreteAgents resumes the planner's session with a
// coaching nudge when the first turn returned agents=[] on an
// imperative request. Returns the raw agent.Result so the caller
// can re-run validatePlan against the new transcript.
//
// Post-refactor (2026-05-13 unified validation gate): no internal
// self-validation. The acceptance check happens once in
// validatePlan, called by Plan()'s loop after every attempt — so
// duplicating the no-exploration check inside the reprompt would
// be dead defense. The reprompt's only job is to run the agent
// call and surface whatever came back; Plan() decides if it's good.
func (p *PlannerAgent) repromptForConcreteAgents(ctx context.Context, sessionID, originalRequest string, dbg *DebugLog) (*agent.Result, error) {
	dbg.Errorf("concrete-agents guard: re-asking planner; original returned agents=[] for imperative request")

	prompt := "System: Your previous response returned agents=[] with a prose summary describing the change. That violates the rule: " +
		"\"CONCRETE CHANGES REQUIRE AT LEAST ONE AGENT.\" The original request — " + strconv.Quote(originalRequest) + " — names a concrete behavior to change. " +
		"Re-emit your plan with either:\n" +
		"  (a) at least one agent task that does the work you described, putting any mechanism caveats in risk_notes, OR\n" +
		"  (b) agents=[] with a summary that explicitly explains why this request is not actionable (e.g. \"already implemented at <file:line>\", \"the file you'd need to edit is read-only / generated\", \"need clarification on <X>\"). \"Here's how you'd do it\" is NOT a valid (b) — that's case (a) in disguise.\n\n" +
		"Output the same JSON shape as before. Do not re-explore; you've already seen enough to decide. One turn."

	// Finalize-only model swap. If FinalizeModel is set, use it for
	// this single-turn emission — typically a faster writer (Kimi /
	// GLM / Qwen) that converts exploration context into JSON
	// quickly. Empty falls back to p.Model.
	finalizeModel := p.Model
	if p.FinalizeModel != "" {
		finalizeModel = p.FinalizeModel
	}
	var outputSchema map[string]interface{}
	if plannerFinalizeConstrainsOutput(finalizeModel) {
		outputSchema = workPlanJSONSchema()
	}
	res, err := agent.Run(ctx, agent.Options{
		Projects:         p.Set,
		Workspace:        p.Set.Primary().Path,
		Prompt:           prompt,
		Model:            finalizeModel,
		Provider:         p.Provider,
		ReadOnly:         true,
		Mode:             agent.ModePlanning,
		EnableBash:       true, // planner verifies external contracts (kai stats --json, etc.) before emitting
		GateConfig:       p.GateConfig,
		SessionStore:     p.SessionStore,
		SessionID:        sessionID,
		TaskName:         "planner",
		RunLogDir:        p.RunLogDir,
		KailabBaseURL:    p.KailabBaseURL,
		KailabToken:      p.KailabToken,
		OutputJSONSchema: outputSchema,
		MaxTotalTokens:   plannerBudget(p.Provider),
		MaxTurns:         1,
		Hooks: agent.Hooks{
			OnAssistantText: func(text string) { dbg.Text("[reprompt] " + text) },
			OnTurnComplete: func(in, out, cached int) {
				dbg.Turn(in, out, cached)
			},
		},
	})
	if err != nil {
		dbg.Errorf("reprompt run: %v", err)
		return nil, err
	}
	return res, nil
}

// countAssistantToolCalls walks the planner's transcript and
// returns the number of tool calls the model emitted before
// producing its final answer. Synthetic injections from
// InjectedContext (context_lookup) are NOT counted — those are
// pre-resolved by the planner and arrive as ToolResults in user
// messages, not as ToolCalls in assistant messages. We want the
// count of times the model itself asked to look at something.
//
// Used by the no-exploration structural guard: if the planner
// emits agents=[...] (passing the stub filter) but did zero tool
// calls AND the request looks like a concrete change, the
// diagnosis is presumptively hallucinated and we re-ask.
func countAssistantToolCalls(msgs []message.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			if _, ok := p.(message.ToolCall); ok {
				n++
			}
		}
	}
	return n
}

// repromptForExploration resumes the planner's session with a
// coaching nudge when it produced a plan without making any tool
// calls — the structural signature of a hallucinated diagnosis.
// Unlike repromptForConcreteAgents (which assumes the model had
// the right idea but emitted an empty bucket), this one assumes
// the model produced a confident plan from the request text
// alone, and the resulting agent prompts will be based on guesses
// about file names, function signatures, and handler behaviors
// that may not exist in the codebase. Sending the worker down
// that path wastes the full agent turn budget.
//
// The nudge demands at least one read tool call before emitting
// the next JSON. Like the other reprompt, resumes the session so
// the prefix is cached.
//
// Post-refactor: returns raw agent.Result. validatePlan owns the
// no-exploration check; internal self-validation here would be
// redundant with the loop.
func (p *PlannerAgent) repromptForExploration(ctx context.Context, sessionID, originalRequest string, dbg *DebugLog) (*agent.Result, error) {
	dbg.Errorf("no-exploration guard: re-asking planner; original emitted a plan with zero tool calls")

	prompt := "System: Your previous response produced a plan WITHOUT making any tool calls — no kai_grep, no view, no kai_symbols, nothing. " +
		"That means every claim in your Diagnosis (about what a handler does, what a file contains, what function calls what) is a guess from the request text alone. " +
		"The original request — " + strconv.Quote(originalRequest) + " — names code-level behavior, which means your diagnosis MUST be backed by code you have actually seen.\n\n" +
		"Re-emit your plan with these steps:\n" +
		"  1. FIRST, make at least one read tool call to verify the diagnosis. Examples: kai_grep for the identifier the request mentions, view the file you think is responsible, kai_symbols to locate the handler.\n" +
		"  2. THEN, based on what the tool result actually showed, write your Diagnosis.\n" +
		"  3. THEN, emit the JSON plan with an agent prompt that references the file and lines you saw.\n\n" +
		"If after exploring you find the request's premise is wrong (e.g., the named handler doesn't exist), say so explicitly in the summary and return agents=[] — that's a legitimate (b) refusal. Do NOT re-emit a plan based on text-only inference."

	var outputSchema map[string]interface{}
	if plannerConstrainsOutput() {
		outputSchema = workPlanJSONSchema()
	}
	res, err := agent.Run(ctx, agent.Options{
		Projects:         p.Set,
		Workspace:        p.Set.Primary().Path,
		Prompt:           prompt,
		Model:            p.Model,
		Provider:         p.Provider,
		ReadOnly:         true,
		Mode:             agent.ModePlanning,
		EnableBash:       true, // planner verifies external contracts (kai stats --json, etc.) before emitting
		GateConfig:       p.GateConfig,
		SessionStore:     p.SessionStore,
		SessionID:        sessionID,
		TaskName:         "planner",
		RunLogDir:        p.RunLogDir,
		KailabBaseURL:    p.KailabBaseURL,
		KailabToken:      p.KailabToken,
		OutputJSONSchema: outputSchema,
		MaxTotalTokens:   plannerBudget(p.Provider),
		// Allow exploration this time — that's the whole point.
		MaxTurns:        plannerTurnCap(p.Provider),
		KeepToolResults: true,
		// HARD enforcement: force the model to emit a tool call on
		// the first turn. Soft prompt instructions opus-4-6 ignores
		// (verified across rounds 6, 10, 11) become impossible to
		// ignore at the API level — Anthropic's tool_choice=any
		// rejects responses without a tool call. After turn 1 the
		// constraint comes off so the model can produce its plan
		// in subsequent turns using what it observed. Providers
		// that don't honor RequireToolUse (OpenAI-shaped routes)
		// silently fall back to soft enforcement.
		RequireToolUseFirstTurn: true,
		Hooks: agent.Hooks{
			OnAssistantText: func(text string) { dbg.Text("[reprompt-exploration] " + text) },
			OnTurnComplete: func(in, out, cached int) {
				dbg.Turn(in, out, cached)
			},
		},
	})
	if err != nil {
		dbg.Errorf("reprompt-exploration run: %v", err)
		return nil, err
	}
	return res, nil
}

// repromptForAlreadyDoneVerify resumes the planner's session with a
// targeted demand: it claimed prior work is "already implemented" but
// the audit found it never actually viewed the symbols its diagnosis
// cited. Force it to look at those symbols' bodies (kai_grep,
// view) and then re-emit. After verification, the model should either:
//   - confirm "already done" with the specific evidence the audit
//     wanted to see (the function actually reads the flag, the path
//     is end-to-end wired); OR
//   - return a dispatch plan that closes the gap it found.
//
// Round-18 motivation (2026-05-13): planner declared `?` toggle
// "already implemented" because the handler called `renderPlanMenu()`,
// without ever reading renderPlanMenu's body — which doesn't look at
// the toggle flag at all. The visible behavior was broken in a new
// way (regression from round 17), and the planner's own risk_notes
// flagged the hypothesis ("the bug may be in renderPlanMenu") but the
// verdict was still "done". The verify reprompt closes this gap.
func (p *PlannerAgent) repromptForAlreadyDoneVerify(ctx context.Context, sessionID, originalRequest string, unverified []string, dbg *DebugLog) (*agent.Result, error) {
	dbg.Errorf("already-done-verify guard: re-asking planner; symbols cited but not viewed: %v", unverified)

	var symbolsClause string
	if len(unverified) > 0 {
		symbolsClause = " You cited these symbols in your diagnosis or risk_notes but never queried them: " +
			strings.Join(unverified, ", ") + "."
	}

	prompt := "System: Your previous response returned \"already implemented\" but the audit downgraded the verdict." +
		symbolsClause + " You ALSO emitted risk_notes containing hypotheses like \"the bug may be in X\" or " +
		"\"would need to investigate\" — those are admissions that the verdict is incomplete. A real \"already done\" " +
		"answer requires evidence that the END-TO-END path works, not just that a handler exists.\n\n" +
		"Re-emit in these steps:\n" +
		"  1. For each cited symbol you have not viewed, run kai_grep or view to read its body.\n" +
		"  2. Trace the actual data flow. If the request was \"toggle X\" and you see a handler that flips a flag, " +
		"VERIFY that the renderer/view reads that flag. If the flag is written but never read, the behavior the user " +
		"asked about is NOT done.\n" +
		"  3. Re-emit JSON. If end-to-end is wired AND visible behavior matches the request, return agents=[] with a " +
		"confident summary citing the actual file:line evidence (no 'may be', no 'would need to investigate'). " +
		"Otherwise, return a dispatch plan with one agent that closes the specific gap you found — name the file and " +
		"function that needs the fix.\n\n" +
		"Original request: " + strconv.Quote(originalRequest)

	var outputSchema map[string]interface{}
	if plannerConstrainsOutput() {
		outputSchema = workPlanJSONSchema()
	}
	res, err := agent.Run(ctx, agent.Options{
		Projects:                p.Set,
		Workspace:               p.Set.Primary().Path,
		Prompt:                  prompt,
		Model:                   p.Model,
		Provider:                p.Provider,
		ReadOnly:                true,
		Mode:                    agent.ModePlanning,
		EnableBash:              true, // planner verifies external contracts (kai stats --json, etc.) before emitting
		GateConfig:              p.GateConfig,
		SessionStore:            p.SessionStore,
		SessionID:               sessionID,
		TaskName:                "planner",
		RunLogDir:               p.RunLogDir,
		KailabBaseURL:           p.KailabBaseURL,
		KailabToken:             p.KailabToken,
		OutputJSONSchema:        outputSchema,
		MaxTotalTokens:          plannerBudget(p.Provider),
		MaxTurns:                plannerTurnCap(p.Provider),
		KeepToolResults:         true,
		RequireToolUseFirstTurn: true,
		Hooks: agent.Hooks{
			OnAssistantText: func(text string) { dbg.Text("[reprompt-verify] " + text) },
			OnTurnComplete: func(in, out, cached int) {
				dbg.Turn(in, out, cached)
			},
		},
	})
	if err != nil {
		dbg.Errorf("reprompt-verify run: %v", err)
		return nil, err
	}
	return res, nil
}

// filterStubAgents removes AgentTask entries whose required fields
// are all empty. An "agent" with no name, no prompt, and no files
// is the model satisfying the JSON schema without producing a real
// task — semantically equivalent to agents=[], but len-based gates
// would treat it as a valid plan and dispatch a nameless worker.
// Caller treats the post-filter empty list as the no-agents case,
// which routes through the same concrete-agents reprompt guard as
// the explicitly-empty response.
//
// Drop shape: empty, placeholder, or too-short prompt. The prompt
// is the only field that actually drives the worker — name/files/
// mode are metadata. An agent with no real instruction has nothing
// to do; the spawned worker dangles.
//
// 2026-05-13 dogfood passes:
//   - opus-4-6 emitted {"name":"debug","prompt":""} — caught by
//     the empty-trim check.
//   - opus-4-6 then emitted {"name":"explore","prompt":"placeholder"}
//     — passed the empty check (length > 0), so this round also
//     rejects prompts whose entire body matches a known non-content
//     placeholder string ("placeholder", "test", "todo", etc.).
//   - Length floor: real change instructions describe what to do.
//     A prompt under 20 characters is structurally too short to
//     specify a code change — even "rename X to Y in file.go" is
//     24 chars. Trips on "fix it" / "do the thing" / single-word
//     prompts that aren't on the placeholder list but still aren't
//     real tasks.
func filterStubAgents(agents []AgentTask) []AgentTask {
	out := agents[:0]
	for _, a := range agents {
		trimmed := strings.TrimSpace(a.Prompt)
		if trimmed == "" {
			continue
		}
		if isPlaceholderPrompt(trimmed) {
			continue
		}
		if len(trimmed) < 20 {
			continue
		}
		out = append(out, a)
	}
	return out
}

// placeholderPrompts is the deny-list of strings that look like
// fillers a model emits when it's gone through the JSON-schema
// motions without producing real content. Conservative: only
// strings that a serious instruction would never consist of in
// their entirety. Match is case-insensitive on the trimmed prompt.
var placeholderPrompts = map[string]bool{
	"placeholder":   true,
	"test":          true,
	"todo":          true,
	"tbd":           true,
	"...":           true,
	"…":             true,
	"exploring":     true,
	"explore":       true,
	"investigate":   true,
	"investigating": true,
	"n/a":           true,
	"none":          true,
	"-":             true,
	"empty":         true,
	"unknown":       true,
	"pending":       true,
}

func isPlaceholderPrompt(trimmed string) bool {
	return placeholderPrompts[strings.ToLower(trimmed)]
}

// auditAlreadyDone tests a candidate "already-done" verdict against
// the transcript that produced it. Returns the symbols named in the
// diagnosis / risk_notes that the planner never actually queried (so
// it asserted behavior about them without observation), and a bool
// flagging whether the risk_notes themselves contain self-doubting
// phrases ("the bug may be in X", "should investigate", "if the user
// confirms…"). Either signal — unverified symbols or doubt phrases —
// is enough to downgrade the verdict.
//
// Symbols are camelCase / snake_case identifiers backticked in the
// diagnosis or risk_notes (same shape as the orchestrator's
// plan-coverage extractor). We require a tool_use whose query/path
// arg literally contains the symbol — that's "viewed enough." A view
// of the file containing the symbol's definition would also satisfy,
// but the simpler test catches the round-18 case (no kai_grep /
// kai_symbols / view targeted at renderPlanMenu).
func auditAlreadyDone(plan *WorkPlan, transcript []message.Message) (unverified []string, doubt bool) {
	cited := extractCitedSymbols(plan)
	if len(cited) == 0 && !auditHasDoubtPhrase(plan.RiskNotes) {
		return nil, false
	}
	queried := queriedSymbolsInTranscript(transcript)
	for _, s := range cited {
		if !queried[s] {
			unverified = append(unverified, s)
		}
	}
	return unverified, auditHasDoubtPhrase(plan.RiskNotes)
}

var reAuditBacktick = regexp.MustCompile("`([^`\n]{1,200})`")

// reAuditIdent matches the same shape the plan-coverage extractor
// uses — camelCase or snake_case, ≥5 chars, internal capital or
// underscore. Filters out plain English words in backticks.
var reAuditIdent = regexp.MustCompile(`[A-Za-z][a-z0-9]+(?:[A-Z][A-Za-z0-9]+|_[A-Za-z0-9_]+)+`)

func extractCitedSymbols(plan *WorkPlan) []string {
	if plan == nil {
		return nil
	}
	text := plan.Diagnosis + "\n" + strings.Join(plan.RiskNotes, "\n")
	seen := map[string]bool{}
	var out []string
	for _, m := range reAuditBacktick.FindAllStringSubmatch(text, -1) {
		for _, id := range reAuditIdent.FindAllString(m[1], -1) {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// queriedSymbolsInTranscript walks the transcript and returns the
// set of identifiers the model passed as a query/path argument to a
// read tool (kai_grep, kai_symbols, kai_callers, kai_dependents,
// view). A symbol counts as queried if it appears as a whole-string
// token in the JSON-encoded args of any tool call.
func queriedSymbolsInTranscript(msgs []message.Message) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			tc, ok := p.(message.ToolCall)
			if !ok {
				continue
			}
			args := tc.Input
			// Extract every camelCase / snake_case identifier from
			// the args. Cheap superset: a symbol counts as "queried"
			// if it appears anywhere in the call arguments. Catches
			// kai_grep "renderPlanMenu", kai_symbols
			// "{file: ...renderPlanMenu.go}", view of a file whose
			// path contains the symbol, etc.
			for _, id := range reAuditIdent.FindAllString(args, -1) {
				out[id] = true
			}
		}
	}
	return out
}

// auditHasDoubtPhrase scans the risk notes for phrases that signal
// the planner is uncertain about its own "already done" verdict.
// Round-18 example: "the bug may be in renderPlanMenu()" sitting in
// risk_notes alongside an "Already implemented" summary. That's the
// planner telling us the verdict is incomplete; we should listen.
func auditHasDoubtPhrase(notes []string) bool {
	for _, n := range notes {
		low := strings.ToLower(n)
		for _, phrase := range []string{
			"may be in",
			"may not be",
			"might be",
			"could be a bug",
			"should investigate",
			"would be needed",
			"would need to be",
			"if the user confirms",
			"follow-up investigation",
			"need to verify",
			"not yet tested",
			"may have been added very recently",
		} {
			if strings.Contains(low, phrase) {
				return true
			}
		}
	}
	return false
}

// isAlreadyDoneSummary classifies an empty-agents plan summary
// as "the work is already implemented" (vs. "the request was
// too vague to plan"). Pattern-matches the phrasings the model
// uses when it decides the requested work exists. Conservative:
// false positives just route the result through the chat
// fallback, which produces a worse UX but doesn't break
// anything.
func isAlreadyDoneSummary(summary string) bool {
	low := strings.ToLower(strings.TrimSpace(summary))
	if low == "" {
		return false
	}
	for _, marker := range []string{
		"already implemented",
		"already done",
		"already complete",
		"already wired",
		"already exists",
		"already in place",
		"is implemented",
		"is fully implemented",
		"is fully wired",
		"nothing to do",
		"no work needed",
		"no changes needed",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// finalize runs a single bonus turn with no tools and a brutal
// "you must answer now" prompt to extract a final answer from a
// planner that hit the turn cap mid-exploration. Resumes the
// existing session so the agent inherits the prior turn's
// transcript (and its cached prefix). Returns nil on failure so
// the caller falls back to surfacing whatever prose the original
// run produced.
func (p *PlannerAgent) finalize(ctx context.Context, sessionID string, dbg *DebugLog) (*PlannerResult, error) {
	dbg.Errorf("finalization: forcing single-turn JSON extraction (no tools, no exploration)")

	prompt := "System: You ran out of exploration time. You MUST now produce your final answer using ONLY what you've already learned in this conversation. " +
		"You have NO tools. You have ONE turn. " +
		"Either:\n" +
		"  (a) Emit the JSON plan in a fenced ```json``` block as your entire response, OR\n" +
		"  (b) Reply with one short paragraph explaining what you were trying to verify and what you'd need to verify it.\n\n" +
		"DO NOT write 'let me check' or 'let me look at' — that's incorrect; there is nothing left to check.\n\n" +
		"Produce your final answer NOW."

	// Finalize-only model swap. If FinalizeModel is set, use it for
	// this single-turn emission — typically a faster writer (Kimi /
	// GLM / Qwen) that converts exploration context into JSON
	// quickly. Empty falls back to p.Model.
	finalizeModel := p.Model
	if p.FinalizeModel != "" {
		finalizeModel = p.FinalizeModel
	}
	var outputSchema map[string]interface{}
	if plannerFinalizeConstrainsOutput(finalizeModel) {
		outputSchema = workPlanJSONSchema()
	}
	res, err := agent.Run(ctx, agent.Options{
		Projects:         p.Set,
		Workspace:        p.Set.Primary().Path,
		Prompt:           prompt,
		Model:            finalizeModel,
		Provider:         p.Provider,
		ReadOnly:         true,
		Mode:             agent.ModePlanning,
		EnableBash:       true, // planner verifies external contracts (kai stats --json, etc.) before emitting
		GateConfig:       p.GateConfig,
		SessionStore:     p.SessionStore,
		SessionID:        sessionID,
		TaskName:         "planner",
		RunLogDir:        p.RunLogDir,
		KailabBaseURL:    p.KailabBaseURL,
		KailabToken:      p.KailabToken,
		OutputJSONSchema: outputSchema,
		MaxTotalTokens:   plannerBudget(p.Provider),
		MaxTurns:         1, // single turn — no exploration
		Hooks: agent.Hooks{
			OnAssistantText: func(text string) { dbg.Text("[finalize] " + text) },
			OnTurnComplete: func(in, out, cached int) {
				dbg.Turn(in, out, cached)
			},
		},
	})
	if err != nil {
		dbg.Errorf("finalization run: %v", err)
		return nil, err
	}

	plan, perr := extractPlanFromTranscript(res.Transcript)
	if perr == nil && plan != nil && len(plan.Agents) >= 0 {
		// Either a real plan or a deliberate empty-agents
		// "already done" / "too vague" answer. Both are
		// valid finalization outcomes.
		out := &PlannerResult{
			Plan:              plan,
			SessionID:         res.SessionID,
			TokensIn:          res.TokensIn,
			TokensOut:         res.TokensOut,
			TokensCached:      res.TokensCached,
			TokensCacheCreate: res.TokensCacheCreate,
			TokensCacheRead:   res.TokensCacheRead,
		}
		if len(plan.Agents) == 0 {
			// Empty-agents result from finalization means
			// "I couldn't plan this with what I had."
			// Surface as Reply so the user sees the
			// summary + risk_notes the model included.
			summary := strings.TrimSpace(plan.Summary)
			if summary == "" {
				summary = "(no summary)"
			}
			notes := strings.Join(plan.RiskNotes, "\n  - ")
			if notes != "" {
				notes = "\n\nRisk notes:\n  - " + notes
			}
			out.Plan = nil
			out.Reply = summary + notes
		}
		return out, nil
	}

	// Even finalization failed to produce JSON — extract whatever
	// prose came back as the Reply.
	var unparseable *ErrUnparseable
	if errors.As(perr, &unparseable) && strings.TrimSpace(unparseable.Raw) != "" {
		return &PlannerResult{
			Reply:             strings.TrimSpace(unparseable.Raw),
			SessionID:         res.SessionID,
			TokensIn:          res.TokensIn,
			TokensOut:         res.TokensOut,
			TokensCached:      res.TokensCached,
			TokensCacheCreate: res.TokensCacheCreate,
			TokensCacheRead:   res.TokensCacheRead,
		}, nil
	}
	return nil, perr
}

// buildPlannerPrompt composes the System: + user request the agent
// runner expects. The system half tells the model how to behave
// (explore first, then emit JSON); the user half is the literal
// request plus a small block of static facts (protected paths,
// project layout) the model can refer to without an extra tool call.
// nonCodeLangs are graph "lang" payload values that don't represent
// source code. A project whose indexed files are ALL non-code is
// reported as "empty of code" to the planner — that's the signal
// that stops it inventing a tech stack from the directory name.
var nonCodeLangs = map[string]bool{
	"":         true,
	"blob":     true,
	"binary":   true,
	"yaml":     true,
	"yml":      true,
	"json":     true,
	"toml":     true,
	"ini":      true,
	"markdown": true,
	"md":       true,
	"text":     true,
	"txt":      true,
	"csv":      true,
	"lock":     true,
}

// projectGraphFacts returns a short factual annotation for a project,
// derived from its indexed semantic graph: how many files it has and
// which languages. The planner prints this beside each project so it
// reasons from ground truth instead of guessing.
//
// The 2026-05-15 dogfood pinned the failure this prevents: the
// planner saw a project named "kai-tui", had no content signal at
// all, and fabricated a Rust codebase ("the same clipboard crate") —
// the directory is in fact empty of code. With a real file count and
// language list in the prompt, "kai-tui — EMPTY" is unmissable.
func projectGraphFacts(p *projects.Project) string {
	if p == nil || p.DB == nil {
		return "NOT INDEXED (no graph data) — do not assume a language or stack; run kai_tree before planning"
	}
	nodes, err := p.DB.GetNodesByKind(graph.KindFile)
	if err != nil {
		return "index unavailable — verify layout with kai_tree before planning"
	}
	return summarizeFileNodes(nodes)
}

// summarizeFileNodes is the pure core of projectGraphFacts: given the
// File nodes of a project's graph, it renders the count + language
// annotation. Split out from the DB fetch so it can be unit-tested
// without standing up a real SQLite graph.
func summarizeFileNodes(nodes []*graph.Node) string {
	if len(nodes) == 0 {
		return "EMPTY (0 files indexed) — do not assume a language or stack; verify with kai_tree before planning"
	}
	langCount := map[string]int{}
	codeFiles := 0
	for _, n := range nodes {
		lang, _ := n.Payload["lang"].(string)
		langCount[lang]++
		if !nonCodeLangs[strings.ToLower(lang)] {
			codeFiles++
		}
	}
	if codeFiles == 0 {
		return fmt.Sprintf("%d file(s) indexed, NONE are source code (%s) — appears EMPTY of code; do not invent a stack from the name, verify with kai_tree before planning",
			len(nodes), topLangs(langCount, 3))
	}
	return fmt.Sprintf("%d files indexed (%s)", len(nodes), topLangs(langCount, 3))
}

// topLangs renders the n most common languages in a histogram,
// most-frequent first, comma-joined. Empty lang keys render as
// "unknown" so the planner sees an honest label rather than a blank.
func topLangs(hist map[string]int, n int) string {
	type lc struct {
		lang  string
		count int
	}
	ls := make([]lc, 0, len(hist))
	for l, c := range hist {
		if l == "" {
			l = "unknown"
		}
		ls = append(ls, lc{l, c})
	}
	sort.Slice(ls, func(i, j int) bool {
		if ls[i].count != ls[j].count {
			return ls[i].count > ls[j].count
		}
		return ls[i].lang < ls[j].lang
	})
	if len(ls) > n {
		ls = ls[:n]
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.lang
	}
	return strings.Join(out, ", ")
}

func buildPlannerPrompt(request string, gateCfg safetygate.Config, cfg Config, set *projects.Set) string {
	maxAgents := cfg.MaxAgents
	if maxAgents <= 0 {
		maxAgents = 5
	}

	var sys strings.Builder
	fmt.Fprintf(&sys, `IMPORTANT: Never generate or guess URLs. Only use URLs from the user or local files.

IMPORTANT: Security: assist with authorized testing, defensive work, CTFs, education. Refuse destructive techniques, DoS, mass targeting, supply-chain compromise, or evasion for malicious purposes. Dual-use tools need clear authorization context.

You are kai's work planner. Turn the user's request into a structured WorkPlan for other agents to execute. Read-only tools available: view, kai_grep, kai_files, kai_tree, kai_context, kai_callers, kai_dependents, kai_symbols, kai_impact. Use them cheaply.

EXPLORATION BUDGET: ~10 tool calls before converging. Each NEW tool call costs the size of its result. Spend the budget on the cheapest informative calls first. Tool cost order, cheap → expensive: kai_grep (~200-1k tok) → kai_context (~500-2k) → kai_callers/kai_dependents/kai_symbols/kai_files/kai_tree (~500-3k) → view (RETURNS THE WHOLE FILE, 2k-30k tok — only after a summary tool confirms it's the right file AND you need the implementation).

COMBINE searches: kai_grep takes a regex, so search several identifiers in ONE call with alternation — kai_grep "formatPlanDetails|pendingPlan|renderPlanMenu" — instead of three separate calls. Each separate grep is a whole tool call against the ~10-call budget; one alternation grep returns the same information for a third of the cost. Never re-grep a term you already searched.

EXEMPLAR-FIRST for "add a thing like an existing thing" requests (new tool, new view, new endpoint, new command, new provider): identify the closest existing exemplar with ONE kai_grep, view it ONCE in full, then kai_grep for its registration site. That is the entire exploration. Do NOT read the surrounding package — the exemplar tells you the contract, the registration grep tells you where the new one slots in.

NEVER view the same file more than once. If you need more of a file than you read, your first view should have been wider. Sequential slices of the same file ("view L1-200", "view L200-400", "view L400-600") are a single wasted decision against the budget: pick a slice that covers what you need, OR view the whole file, OR commit to your plan with what you've seen.

NEVER re-search a term you already searched, even with a different alternation. If a grep returned hits, refine by narrowing to a path (kai_grep "X" in pkg/) — don't re-spell the term in a fresh global call. If it returned nothing, the answer is "this term doesn't exist"; don't re-spell it hoping for a different result.

Convergence: the moment you have enough evidence to answer, STOP and emit the plan. If kai_grep finds the function already exists, you don't need to view to double-check — the grep result IS the proof. "already implemented" with grep evidence in risk_notes is a SUCCESS.

FAST PATH FOR TRIVIALLY SCOPED WORK. When the request names a CONCRETE single-file deliverable with no structural ambiguity — examples: "write a design doc at path/X.md", "update the version string in package.json to 1.2.3", "rename foo to bar inside file F", "add helper Z to file F" — emit ONE agent immediately with files=[the one file], EXPLORE: 0-2 turns max. Skip the diagnosis + approach fields; they exist to explain non-obvious fixes, and a "write file X" task has nothing to diagnose. The exploration budget exists for ambiguity, not for confirming what's already concrete. Multi-agent plans are for genuinely independent subsystems — they are NEVER appropriate for documentation, single-file edits, or sequential phases (design → tests → implementation is ONE agent's STEPS list, not three parallel agents that race on the same DB).

Concrete shape for a fast-path plan:
`+"```json"+`
{
  "summary": "Write design doc at docs/X.md describing Y",
  "agents": [{"name": "write-x-doc", "prompt": "Write docs/X.md covering Y. EXPLORE: 0 turns. EDIT: docs/X.md. VERIFY: file exists and renders.", "files": ["docs/X.md"]}],
  "risk_notes": []
}
`+"```"+`

Avoid these failure modes:
  1. Guessing file paths from naming conventions. ONE kai_tree/kai_files call verifies layout.
  2. Planning work that already exists. ONE kai_grep on a key identifier usually settles it.
  3. Inventing a tech stack from a project's NAME. A project called "x-tui" is not necessarily Rust; "x-api" is not necessarily a web server. The Project layout block below states each project's REAL file count and languages from the semantic graph — trust it over the name. A project marked EMPTY or NOT INDEXED has no source code: do not diagnose against an imagined language, framework, or "crate"/"package" — drop it from scope or kai_tree it first. Diagnosing a stack you never saw in a tool result is hallucination.
  4. Speculating about a runtime error from file names instead of reproducing it. When the user reports a bug WITHOUT pasting the error, the right plan is one debug agent whose first move is bash-running dev/test/start and reading stderr. Confirm the run command exists (root package.json scripts, Makefile, turbo.json) — do NOT guess source files. Leave the agent's "files" array EMPTY. Files come from the actual stack trace, not planner intuition. In multi-app monorepos, don't name a specific app unless the user did — use the root-level dev command so all apps surface errors at once.
  5. Under-scoping a SOURCE → TARGET request after surface-reading one side. When the request points at a SOURCE and a TARGET ("apply X from A to B", "port the design of A onto B", "make B look/work like A", "take the styles from A and apply to B"), the work is rarely the one-shot copy it sounds like. The 2026-05-24 kai-desktop dogfood pinned this: user asked to apply a React reference app's design to a Svelte target, planner read only the source's theme.css, emitted a single agent scoped to "copy CSS variables", and missed that the deliverable required structural integration (component layout, routing, view shells, import wiring). Before emitting a plan for any SOURCE → TARGET request, your exploration MUST cover BOTH sides: at minimum, kai_files on the top level of A and B, plus kai_grep for the entry point / app root / index file of each. Compare what you find. If A is a React app and B is a Svelte/Vue/other-framework app, copying tokens is not enough — the plan must include component/layout porting. If A and B use the SAME framework, surface that in diagnosis so the user knows it's a token-only port. Either way: read both sides before scoping. A plan whose risk_notes mentions only the source side is presumptively under-scoped.

After exploration, emit your plan as a single fenced JSON block (and nothing else after):

`+"```json"+`
{
  "summary": "one short sentence describing the change, OR 'already implemented' if exploration showed the work is done",
  "diagnosis": "2-3 sentences, Sherlock Holmes style: what's the problem, why, what evidence. Concrete — name function, file, line. The user reads this BEFORE confirming; if it reads like a commit message they have to trust you blindly. Skip on fully obvious requests ('rename X to Y'); otherwise required.",
  "approach": "one short sentence describing HOW the fix works at a strategy level. Required whenever diagnosis is set.",
  "agents": [
    {
      "name": "short-kebab-case",
      "prompt": "1-3 sentences of what this agent does, then the PHASE BLOCK (EXPLORE / EDIT CHECKLIST / VERIFY) described below. The EDIT CHECKLIST is the load-bearing part: a numbered, closed list of concrete file-scoped edits the agent executes verbatim.",
      "files": ["real/verified/path.go", "..."],
      "dont_touch": ["paths/this/agent/must/not/edit", "..."],
      "acceptance_criteria": ["2-4 concrete, checkable statements of what 'done' means — the INTENT, not the steps. When the task depends on an external contract it does not own (a CLI command/flag it invokes, an API, an output format), make ONE criterion name the REAL observable behavior so verify can run it — e.g. 'running reportgen --format=csv exits 0 and prints a header row', not 'the export looks right'. A criterion a mocked unit test could satisfy is not enough.", "..."],
      "evidence": [
        {
          "file": "real/verified/path.go",
          "line_start": 42,
          "line_end": 48,
          "excerpt": "literal file content for the cited range — 1-3 lines, ≤200 chars",
          "annotation": "one sentence explaining WHY this location is the right target — the reasoning the executor would otherwise have to re-derive"
        }
      ],
      "verify_checks": [
        {"run": "reportgen --format=csv", "expect_exit": 0, "expect_stdout_contains": "", "why": "the external command this change depends on must actually run with these flags; a wrong flag exits nonzero"}
      ]
    }
  ],
  "risk_notes": ["findings during exploration, what's already done, what could break, ..."],
  "trivial": false
}
`+"```"+`

Rules:
  - Max %d agents. Fewer is better. One agent for small changes.
  - SET "trivial": true ONLY when the whole plan is a single small, localized, low-risk change you are certain about — one file, a handful of lines, no structural or cross-cutting impact (a color value, a constant, a label, an obvious one-line fix). A trivial plan runs immediately with NO confirmation step, so reserve it for changes a reviewer would not need to see a plan card for. Any doubt — omit it / false, and the user gets the normal confirm step.
  - EMIT EVIDENCE for any non-obvious target. When your diagnosis identified a SPECIFIC line, function, or pattern that drove the fix decision, attach an evidence[] entry to the relevant agent: file + line range (1-based inclusive) + the literal excerpt (≤200 chars, slice it from a real tool result you read THIS run — no paraphrasing) + a one-sentence annotation explaining WHY this location is the right target. Cap each agent at 3 entries. The executor receives these in its prompt as a PRIOR — without them, the executor's training prior often picks the lower-friction-but-wrong interpretation (e.g. "this is a config option, I'll edit svelte.config.js" instead of "this is a hardcoded library internal, patch it via patch-package"). The 2026-05-26 vite-plugin-svelte dogfood pinned this exact failure. Skip evidence for trivial label/value/constant changes where the prompt itself is unambiguous; emit it when the WHY is non-obvious.
  - COVER BOTH SOURCE AND DISPLAY PATHS. When the symptom is "wrong value displayed in UI" (the rendered output doesn't match expectations), your evidence MUST include at least one citation from the data SOURCE path (where the value is produced, exposed, or transformed) — not only from the display path (where the value is rendered or passed as a prop). The 2026-05-26 sidebar "main main" dogfood pinned this: planner cited cli.js, App.svelte, and Sidebar.svelte (all on the display path) — the actual unfixed bug was in preload.cjs's contextBridge.exposeInMainWorld call (the SOURCE path), invisible to the evidence because no citation went there. For a "wrong display" symptom, ask: where is this value DEFINED, EXPOSED, or BRIDGED? Cite that location too. If you can't find the source, add the file you suspect to scope so the executor finds it.
  - Agents run in parallel — keep them independent.
  - CONSOLIDATE small work into ONE agent. Each agent pays its own prompt setup cost (~$0.10-0.20). For ≤3 files of paragraph-sized changes, one sequential agent is cheaper and faster than two parallel ones. Split only when the work touches genuinely independent subsystems AND each slice is large enough that setup is a small fraction.
  - NO TOOL CALLS, NO DIAGNOSIS. Before claiming what specific code does (handler X, function Y, behavior Z), you must have seen that code in a tool result THIS run. If you haven't viewed the code yet, your first turn MUST be a read tool call — NOT a JSON emission. A plan whose first turn produces JSON with no preceding tool calls is presumptively hallucinated.
  - VERIFY MECHANISM REVERSIBILITY when the request is "undo / hide / toggle / collapse / cancel" something. Append-only channels (scrollback, logs, posted messages, sent webhooks, committed history) can't be retracted by writing more. If your "off" state requires un-writing such a sink, switch to a repaintable mechanism (in-place re-render, transient region, unsent buffer) BEFORE flipping any flag.
  - VERIFY LOAD-BEARING CLAIMS before committing. If your diagnosis depends on (a) library/stdlib behavior — write a 10-30 line probe; (b) codebase state — use kai_grep/kai_callers/kai_context; (c) contradicting an existing comment/TODO — read the WHOLE file; the comment had a reason.
  - EMIT A MACHINE verify_checks ENTRY when the change depends on an external contract it does not own — a CLI command/flag it spawns, an HTTP endpoint, an output format. Give the exact command the change relies on plus the result it must produce (expect_exit and/or expect_stdout_contains). The HARNESS runs it after the edits land and HOLDS the gate on mismatch — so a green build or an agent narrating "I verified it" can't ship a feature wired to a command that does not exist (e.g. code that calls reportgen --json when the tool only accepts --format=json: it errors and produces nothing, but compiles fine). A check with run "reportgen --json" and expect_exit 0 catches that deterministically. This is stronger than an acceptance_criteria sentence because nobody narrates it — the command passes or it does not. Leave verify_checks: [] when no external contract is involved.
  - SECURITY CHECKLIST. When planning NEW functionality (not pure refactors), surface in risk_notes AND address in the agent prompt any of:
      • NEW SECRET/CREDENTIAL — name the env var or config field; require redaction in logs; require an "is it set?" check at construction so an empty key fails loudly instead of producing a 401 mid-stream.
      • OUTBOUND NETWORK CALL — require an explicit timeout, error handling for 429/5xx that surfaces a transient error to the agent loop instead of retry-storming, and note the provider's published rate limit if known.
      • USER INPUT INTO SHELL/SQL/FS PATHS — require explicit validation or quoting; flag any string concatenation that could be injection.
      • PARSE UNTRUSTED FORMAT (XML/HTML/zip/yaml from network) — require a parser with entity/size limits or a safe-by-default library; never feed network bytes to a permissive parser.
      • CONFIGURABLE BASE URL OR ENDPOINT — usually a smell. The provider has one endpoint; making it configurable invites phishing-proxy / staging-leak misuse. Default to a constant in the tool file; only add config if a concrete need (self-hosted variant, on-prem) is named in the request.
    These are PLANNING-TIME concerns, not "the implementer will handle it." A plan that adds a network-calling tool with no rate-limit note in risk_notes and no timeout requirement in the agent prompt is incomplete; revise it before emitting.
  - MECHANICAL REPLACEMENTS get ONE agent with the SOURCE-OF-TRUTH file only. Tell the agent to use kai_grep + a single bash sed/perl pipeline for the rest. Do NOT enumerate test files, doc references, or comment mentions in "files" — that turns a 5-second sed into 20+ view/edit cycles.
  - PHASE BLOCK in every agent prompt, ending with exactly:
      EXPLORE: max <N> turns — <what to locate, named files/symbols>
      EDIT CHECKLIST (execute exactly these, in order, and NOTHING else):
        1. <real verified file> — <one concrete change: the symbol or anchor, and what to add/change there>
        2. <real verified file> — <...>
        ... (typically 1–6 items; if you genuinely need more than ~8, the agent is too big — split it or rescope)
      VERIFY: <the exact build/test command from the BUILD CONTEXT block, plus the expected result>
    EXPLORE cap typically 3–5. The runner enforces read-streak and budget caps independently.
    The EDIT CHECKLIST is a CLOSED CONTRACT, not a sketch. The agent is instructed to execute these numbered items verbatim and add nothing — no extra refactors, no adjacent cleanup, no exploring past what an item needs. That is what keeps a smaller execution model from meandering. Consequences you must design around:
      • Every item must name a REAL file you verified this run AND a concrete anchor — a symbol name, a line number, or a unique nearby string. A vague item ("update the UI", "wire it up") defeats the whole mechanism: the agent meanders exactly where the item is vague.
      • The checklist must be COMPLETE. If shipping the change needs 5 edits and you list 4, the result is 4/5 done — the agent will not add the 5th, because it was told not to.
      • If you could not verify enough to write concrete items, you have not explored enough. Do another EXPLORE turn — do not emit a vague checklist and hope the agent figures it out. It will not; it will thrash.
  - ACCEPTANCE CRITERIA capture the INTENT, the EDIT CHECKLIST captures the STEPS — they are not the same and one cannot substitute for the other. The checklist says "what to type"; the criteria say "what good looks like when you're done." Write 2-4 criteria, each a concrete statement a reviewer can verify the finished change against. Derive them from the request's WHY, not its wording: if the user says "extract the repeated fallback SO a new case can't be forgotten", a criterion is "adding a new case requires no edit to the function that applies the fallback" — that catches a mechanically-correct change that still misses the point. Bad criteria just echo the steps ("the helper is added"); good criteria would fail a shallow-but-correct change. Omit only when the request's wording genuinely IS the whole intent (an explicit "rename X to Y").
  - Every "files" path MUST be COPIED VERBATIM from a kai_files or kai_grep result — character-for-character, including the project prefix. Those tools emit the exact, canonical path (e.g. "kai/kai-cli/internal/tui/views/repl.go"); copy it. Do NOT construct, retype, or prefix a path yourself, and do NOT assemble one from a kai_tree (kai_tree shows structure, not copy-ready paths — if you need a path for the plan, get it from kai_files/kai_grep). Hand-built paths are how the doubled "kai/kai/..." prefix bug happens; copying eliminates it.
  - CONCRETE CHANGES REQUIRE AT LEAST ONE AGENT. If the request names a behavior to change with an imperative verb ("make/add/fix/change/update/rename/remove/toggle/wire up/implement/support") AND points at a specific target, you MUST produce at least one agent. agents=[] is ONLY for vague-can't-plan or already-done. Risk warnings go in risk_notes; the work goes in an agent. agents=[] + prose-describing-the-change renders as "Answered" with no action.
  - Vague request (no concrete target after exploration): agents=[] with a one-line summary.
  - Work already done: agents=[] with summary="already implemented" and details in risk_notes.
  - Output the fenced JSON as your FINAL message. Don't keep talking after.

Plan readability: the user reads diagnosis + approach, glances at go/cancel/feedback, decides in 5 seconds. They need WHAT'S WRONG + WHY YOUR FIX WORKS at a glance.

Conversation continuity: "it", "this plan", "what's left" resolve against prior planner turns visible in your context.

Failure recovery: if context contains "PRIOR PLAN EXECUTION FAILED", the previous "go" didn't succeed. Don't re-emit the same agents — acknowledge the failure, suggest the next action, propose a different plan addressing the cause.`, maxAgents)

	var user strings.Builder
	fmt.Fprintf(&user, "Request: %s\n", request)

	if len(gateCfg.Protected) > 0 {
		user.WriteString("\nProtected paths (the safety gate blocks any edit to these — never plan changes to these):\n")
		for _, p := range gateCfg.Protected {
			fmt.Fprintf(&user, "  - %s\n", p)
		}
	}

	if set != nil && len(set.Projects()) > 1 {
		user.WriteString("\nProject layout (multi-root workspace):\n")
		for _, p := range set.Projects() {
			// Prefer the directory basename as the path prefix. The
			// human-friendly Name (often from a README H1) can contain
			// spaces — e.g. "Kai Server" — which the agent assembles
			// into tool paths verbatim and produces shell-unfriendly
			// results like `view "Kai Server"/foo.go`. The on-disk
			// directory name is always shell-safe AND matches what
			// `ls`/`cd` produce, so it's the right canonical form.
			prefix := filepath.Base(p.Path)
			// Show ONLY the prefix the planner must use — never the
			// absolute filesystem path. The 2026-05-15 dogfood pinned
			// why: the kai project lives at .../projects/kai/kai, so
			// printing its absolute path led the planner to read the
			// trailing "/kai/kai" as the prefix and emit doubled
			// "kai/kai/kai-cli/..." paths in the plan — paths that do
			// not exist, which dead-ended every downstream agent. The
			// planner never needs the absolute path: every tool it has
			// takes a project-prefixed relative path.
			//
			// Graph facts (file count + languages) come after, so the
			// planner reasons from ground truth instead of guessing a
			// stack from the name.
			fmt.Fprintf(&user, "  - %s/ — %s\n", prefix, projectGraphFacts(p))
		}
		user.WriteString(`
File-path convention in this workspace — the prefix is the PROJECT NAME (the first identifier in each bullet above), NOT a subdirectory name within the project. A project may contain a subdirectory whose name LOOKS like another project (e.g. project "kai" contains a "kai-cli" subdirectory) — the subdirectory's name is NEVER a valid prefix. Always use the project name from the layout list.

Examples for a workspace with projects "kai" and "kai-server":
  ✓ kai/kai-cli/internal/foo.go       — file in the kai project, kai-cli subdirectory
  ✓ kai-server/kailab/api/routes.go   — file in the kai-server project, kailab subdirectory
  ✗ kai-cli/internal/foo.go            — WRONG: "kai-cli" is a subdirectory of "kai", not a project
  ✗ kai/kai/kai-cli/foo.go             — WRONG: project name appears once, not twice
  ✗ /Users/.../kai-cli/foo.go          — WRONG: never use an absolute filesystem path

For scoping kai_grep / kai_files / kai_tree: pass the project name as path (e.g. path="kai-server"), or path="kai-server/<subdir>" to narrow within a project. Bare subdirectory names ("kai-cli", "kailab") will NOT scope correctly.

The "files" array in your plan MUST be COPIED VERBATIM from a kai_files or kai_grep result — character-for-character — so this prefix structure is preserved automatically and you cannot hand-construct a wrong one.
`)
		user.WriteString("The file count + languages after each project are GROUND TRUTH from the semantic graph. A project marked EMPTY or NOT INDEXED has no source code — do NOT plan against a language or framework you inferred from its name; either drop it from scope or run kai_tree to see what is actually there.\n")
	}

	// Per-turn context suffix: reassures the planner that compaction
	// is automatic (so it doesn't need to ration history) and supplies
	// a current timestamp anchor for date-sensitive reasoning. Lives
	// at the END of the system prompt so the model reads it after
	// all other instructions have set the frame.
	suffix := fmt.Sprintf(
		`Prior messages compress automatically near context limits. System context: %s

Scope: fix exactly what's asked. No extra features, refactors, cleanup around bug fixes, added configurability, or speculative abstractions. Don't add error handling for impossible scenarios — validate only at boundaries. No feature flags or compat shims for one-time changes.

Verify before claiming completion: run tests, execute scripts, check output. If you can't verify, state that explicitly.

References: GitHub/KaiContext as owner/repo#123; code as file_path:line_number.

%s`,
		time.Now().UTC().Format(time.RFC3339),
		promptenv.ComputeEnvInfo(cfg.Model, nil),
	)
	return "System: " + sys.String() + "\n\n" + suffix + "\n\n" + user.String()
}

// extractPlanFromTranscript pulls the WorkPlan JSON out of the last
// assistant message in the transcript. Tolerates the model wrapping
// the JSON in a code fence (it's instructed to, but defensive
// parsing is cheap) and tolerates trailing prose after the fence
// (the model is told not to, but again — cheap).
//
// Returns ErrUnparseable if no JSON block could be found or parsed,
// so the REPL can show the raw model output to the user for
// debugging instead of silently swallowing the failure.
func extractPlanFromTranscript(msgs []message.Message) (*WorkPlan, error) {
	// Walk backwards to find the last assistant text. Tool-result
	// messages and tool_use blocks aren't candidates — only assistant
	// prose, which is where the model emits its final JSON.
	var raw string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.RoleAssistant {
			continue
		}
		text := msgs[i].Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		raw = text
		break
	}
	if raw == "" {
		return nil, &ErrUnparseable{
			Raw: "",
			Err: fmt.Errorf("no assistant message in transcript"),
		}
	}

	jsonBody, err := extractFencedJSON(raw)
	if err != nil {
		return nil, &ErrUnparseable{Raw: raw, Err: err}
	}
	var plan WorkPlan
	if err := json.Unmarshal([]byte(jsonBody), &plan); err != nil {
		return nil, &ErrUnparseable{Raw: raw, Err: err}
	}
	return &plan, nil
}

// extractFencedJSON returns the JSON payload from a string that may
// contain prose around a ```json ... ``` fence (or a bare {...}
// object). Search order:
//
//  1. ```json fence (preferred — matches the system-prompt instruction)
//  2. Any ``` fence whose body parses as JSON
//  3. The LAST balanced {...} substring at top level — the model
//     often writes prose+example JSON before its final plan, and we
//     want the final block, not whatever it described inline.
//
// The bare-object scan in (3) tracks string state (and string
// escapes) so braces inside JSON string values don't throw off the
// brace counter. The earlier implementation counted ALL '{'/'}'
// characters, which mis-balanced any plan whose prompt or
// risk_notes string mentioned a struct literal — observed in May
// 2026 when a planner emitted JSON with `{...}` inside a code-
// example string and the parser truncated mid-object, which then
// failed json.Unmarshal and surfaced as raw text in the REPL.
//
// Returns an error only when none of the above yield a candidate —
// the candidate's actual JSON validity is the caller's job (so
// errors there can carry the raw text for the user).
func extractFencedJSON(s string) (string, error) {
	// 1. ```json fence. The closing-fence search must be validated:
	// a naive Index for the next ``` matches a code fence INSIDE a
	// JSON string value (e.g. a ```go block quoted in an agent
	// prompt), truncating the JSON so json.Unmarshal fails and the
	// whole plan surfaces as raw REPL text. Observed May 2026 on a
	// bbolt planning turn whose agent prompt embedded ```go. If the
	// first candidate isn't valid JSON, fall through to the brace-
	// scanner (path 3), which tracks string state correctly.
	if i := strings.Index(s, "```json"); i >= 0 {
		rest := s[i+len("```json"):]
		// Skip the optional newline after the opening fence.
		rest = strings.TrimLeft(rest, " \t\r\n")
		if end := strings.Index(rest, "```"); end >= 0 {
			candidate := strings.TrimSpace(rest[:end])
			if json.Valid([]byte(candidate)) {
				return candidate, nil
			}
		}
	}
	// 2. Generic ``` fence — but ONLY if the body actually parses as
	// JSON. Without the validation step this path eagerly matched the
	// first ``` it found, which broke whenever the model emitted raw
	// JSON (no outer fence) containing markdown code-blocks INSIDE
	// string values (e.g. an agent prompt quoting Go code with ```go).
	// The first ``` then landed inside a JSON string, the "fence body"
	// became inner code, and json.Unmarshal failed downstream —
	// surfacing the whole plan as raw REPL text. Validate before
	// returning so a non-JSON fence falls through to path 3.
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			candidate := strings.TrimSpace(rest[:end])
			if json.Valid([]byte(candidate)) {
				return candidate, nil
			}
		}
	}
	// 3. Last balanced {...} at top level, ignoring braces inside
	//    JSON string literals.
	if last := lastBalancedJSONObject(s); last != "" {
		return last, nil
	}
	return "", fmt.Errorf("no JSON block found in response")
}

// lastBalancedJSONObject scans s for top-level balanced {...} blocks
// and returns the LAST one, walking through JSON string literals so
// braces inside string values don't perturb the depth counter.
// Empty return means no balanced top-level object exists.
//
// Why "last": the planner system prompt asks the model to end its
// turn with the JSON plan. If the model also emitted illustrative
// JSON earlier (an example, a quoted user input, a code excerpt),
// returning the FIRST balanced object — as the prior implementation
// did — would pick the example and lose the actual plan. Picking
// the last balanced object is correct for the format we ask for and
// resilient to extra prose around it.
func lastBalancedJSONObject(s string) string {
	var (
		start              = -1
		depth              = 0
		inStr              = false
		escape             = false
		bestStart, bestEnd = -1, -1
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue // stray '}', ignore
			}
			depth--
			if depth == 0 && start >= 0 {
				bestStart, bestEnd = start, i+1
				start = -1
			}
		}
	}
	if bestStart < 0 {
		return ""
	}
	return s[bestStart:bestEnd]
}
