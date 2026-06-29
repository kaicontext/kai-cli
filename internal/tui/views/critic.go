// Package views: satisfaction-gate critic. Every non-trivial run
// gets a one-shot critic call after the trailer prints — the
// critic re-reads the original request and the agent's reply (or
// summary of file changes) and answers "does this satisfy the
// ask?" The same model that confabulated CSS values in the
// 2026-05-24 kai-desktop dogfood produced sharp accurate
// self-critique when asked to grade its own work; the critic
// formalizes that pattern so the grading happens automatically
// before the run is declared done.
//
// Surface UX:
//   PASS  → one dim trailer line: "✓ critic: looks good"
//   FAIL  → visible critique block + "press r to retry"
//
// On FAIL the REPL stashes a pendingCriticRetry so a single 'r'
// keypress dispatches a new run with the critique appended to the
// next prompt — the model gets a concrete diagnosis of what it
// missed without the user having to retype the request.
package views

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"kai/api/agent"
	tea "github.com/charmbracelet/bubbletea"
)

// criticMaxRetries caps how many auto-retries the critic can
// trigger in a single chain. The critic fires after each chat
// turn and on FAIL the REPL dispatches a retry automatically;
// without a cap, a critic that keeps marking FAIL on a
// genuinely-unfulfillable request would burn LLM calls forever.
// Two retries strike a balance: enough to recover from a model
// that missed a step on the first attempt, few enough to bail
// out on a request the model can't satisfy.
// criticDefaultModel is the hardcoded fallback when no env var or
// config override is set. Different model from the chat agent's
// default (DeepSeek-V4-Pro) so the critic doesn't share the same
// training prior — the whole point of having a critic is independent
// judgment, and same-model self-critique rationalizes its own
// patterns (2026-05-28 dogfood). Kimi-K2.6 is in the kailab
// allowlist (kailab_openai.go:43) and routes through the proxy.
const criticDefaultModel = "moonshotai/kimi-k2.6"

// resolveCriticModel picks the model the critic runs on. Resolution:
//
//  1. KAI_CRITIC_MODEL env var (operator escape hatch)
//  2. PlannerServices.CriticModel (config-resolved at startup)
//  3. criticDefaultModel (hardcoded different-family default)
//  4. ChatModel (last resort if nothing else available)
//
// Returns empty only when even ChatModel is unset — caller should
// treat empty as "let the provider pick its default" rather than
// blocking the critic.
func resolveCriticModel(s *PlannerServices) string {
	if v := strings.TrimSpace(envCriticModel()); v != "" {
		return v
	}
	if s != nil && strings.TrimSpace(s.CriticModel) != "" {
		return s.CriticModel
	}
	if criticDefaultModel != "" {
		return criticDefaultModel
	}
	if s != nil {
		return s.ChatModel
	}
	return ""
}

// envCriticModel reads KAI_CRITIC_MODEL with whitespace trimmed.
// Factored out so tests can stub it without touching the OS env.
var envCriticModel = func() string {
	return os.Getenv("KAI_CRITIC_MODEL")
}

const criticMaxRetries = 2

// forbiddenGenericOpenings is the cheap pre-critic check: chat replies
// to workspace questions that BEGIN with one of these phrases bypass
// the critic LLM round-trip and go straight to a forced FAIL with a
// canned critique. The model has shown (2026-05-28 dogfood) that
// even with workspace-evidence in hand, it will still synthesize a
// generic-patterns answer if its training prior is strong enough.
// The critic LLM eventually catches this but burns a round-trip;
// fast-path detection is ~1ms and saves ~5-15s of latency.
//
// Each entry is a case-insensitive prefix match on the trimmed reply.
// Keep the list tight — false positives FAIL a legitimately useful
// reply and force a retry the user didn't ask for. These are the
// failure shapes observed in real dogfoods, not speculative additions.
var forbiddenGenericOpenings = []string{
	"most coding agents",
	"most agentic coding tools",
	"most agentic",
	"most agent ",
	"most agents",
	"most projects",
	"most systems",
	"most tools",
	"most implementations",
	"most ai assistants",
	"most llm",
	"the agent typically",
	"agents like this",
	"coding agents handle",
	"coding agents use",
	"agent implementations",
	"in general,",
	"typically, ",
	"typically systems",
	"common patterns include",
	"this depends on what",
	"it depends on what",
	"if you'd like, i can look",
	"if you can point me",
	"i'd need to check",
}

// deferredInvestigationMarkers catch the "I'll look at it if you ask"
// hedge that lets the model bypass tool-call requirement by promising
// future action. Substring match anywhere in the reply (not just
// prefix) — the model uses these at the END of a generic answer to
// punt actual investigation.
var deferredInvestigationMarkers = []string{
	"if you'd like, i can look at",
	"i can look at the relevant code",
	"if your project uses a specific",
	"if you can point me to",
	"want me to look at",
	"point me to the specific",
}

// workspaceContextWords are user-question signals that the question
// IS about the workspace (not a generic conceptual question). When
// any of these appear in the original request, a generic-opening reply
// is a clear failure; otherwise the model may have legitimately
// answered an abstract question with abstract content.
var workspaceContextWords = []string{
	"this repo", "this codebase", "this project", "this code",
	"this directory", "in this", "how does this", "how do you",
	"how is this", "where in this",
	// Evidence-shaped follow-ups: when the user asks for proof / source /
	// evidence after the assistant made any factual claim, the referent
	// is the prior turn's claims. These are workspace-grounding requests
	// even when the user doesn't repeat "in this codebase". 2026-05-28
	// dogfood: chat answered a workspace question with a mechanisms list,
	// user typed "where is your proof", chat asked "what proof?" instead
	// of citing files for the claims it just made. The gate has to route
	// these to workspace so OBSERVED kicks in.
	"where is your proof", "where's your proof", "where is the proof",
	"show me the proof", "prove it", "your source", "your sources",
	"cite your", "citation for", "evidence for that", "show me where",
	"point to where", "back that up", "back it up",
}

// implementationActionPhrases are user inputs that mean "stop
// describing, actually do the work." When detected on a turn with
// no explicit slash-mode override, the REPL routes the dispatch
// to ModeCoding so the model has edit tools available. Without this
// override, conversation mode (read-only by design) responds with
// another description and the user gets a wall of markdown code
// blocks instead of files being written.
//
// Tight phrase list — false positives route a chat-shaped request
// to /code, which has more capability than chat (edit tools registered
// but unused if not needed), so the cost is low. False negatives
// keep the existing chat-mode behavior. Asymmetry justifies the
// permissive list.
var implementationActionPhrases = []string{
	"implement it", "implement this", "implement that", "implement them",
	"please implement", "yes please implement", "yes implement",
	"go implement", "go ahead and implement",
	"write the code", "write the code now", "write it",
	"ship it", "ship this",
	"build it", "build it now", "build this",
	"do it", "do it now", "just do it",
	"make the change", "make the changes", "make those changes",
	"apply it", "apply the change", "apply the changes",
	"go ahead", "yes go ahead", "ok go ahead",
	"yes please do that", "yes do that",
	"yes please scaffold", "scaffold it",
}

// isImplementationActionRequest returns true when the user's input
// looks like a "do the work" directive — short phrase, action verb,
// no question. Phrase-list match is case-insensitive and trimmed;
// requires the request to be reasonably short (under ~80 chars) so
// a longer message that incidentally contains "do it" mid-sentence
// doesn't false-trigger.
func isImplementationActionRequest(request string) bool {
	r := strings.TrimSpace(strings.ToLower(request))
	if r == "" || len(r) > 80 {
		return false
	}
	// Strip terminal punctuation so "do it." and "do it!" match.
	r = strings.TrimRight(r, ".!?")
	for _, phrase := range implementationActionPhrases {
		if r == phrase {
			return true
		}
		// Prefix match for slight variations ("implement it now",
		// "ship it please") but only when phrase is the LEADING text.
		if strings.HasPrefix(r, phrase+" ") {
			return true
		}
	}
	return false
}

// classifyWorkspaceTurn returns true when the user's request shape
// indicates a workspace-grounded question (one whose correct answer
// depends on this codebase's specific code). Phase 1 of the workspace
// grounding spec: signal is request-text-based only. Phase 2 will add
// the tool-use and context-injection signals from agent.Result for
// turns where the user's phrasing is ambiguous but the model
// investigated anyway.
//
// Pass-through default (false) — when in doubt, treat as conceptual.
// The cost of falsely routing a workspace turn to conceptual is one
// missed enforcement; the cost of falsely routing a conceptual turn
// to workspace is flagging a correct generic answer as failure. The
// asymmetry justifies the conservative default.
func classifyWorkspaceTurn(request string) bool {
	q := strings.ToLower(request)
	for _, w := range workspaceContextWords {
		if strings.Contains(q, w) {
			return true
		}
	}
	return false
}

// detectGenericOpening returns a non-empty canned critique when the
// reply starts with a forbidden generic-patterns phrase AND the user's
// request looked workspace-specific. Returns "" otherwise (let the
// real critic decide).
func detectGenericOpening(reply, request string) string {
	r := strings.TrimSpace(strings.ToLower(reply))
	if r == "" {
		return ""
	}
	// Strip leading markdown bullets / dim markers the renderer added.
	r = strings.TrimLeft(r, "•·* \t")
	// Phase 1 gate: only fire on workspace turns. Conceptual turns
	// are pass-through (the prior version inlined this check; lifting
	// it to a named classifier prepares for Phase 2 where the signal
	// gains tool-use + context-injection inputs).
	if !classifyWorkspaceTurn(request) {
		return ""
	}
	for _, phrase := range forbiddenGenericOpenings {
		if strings.HasPrefix(r, phrase) {
			return "my last reply opened with \"" + phrase + "\" — a generic-patterns answer. The user asked about THIS workspace; I should have synthesized FROM the file:line evidence I already collected, not from my training prior."
		}
	}
	// Substring sweep for delayed-investigation hedges. These bypass
	// the prefix check because they typically appear at the END of a
	// generic answer ("...check the specific implementation. If you'd
	// like, I can look at the relevant code"). Same failure shape:
	// the model dodged investigating now by promising to investigate
	// later.
	for _, marker := range deferredInvestigationMarkers {
		if strings.Contains(r, marker) {
			return "my last reply offered to investigate later (\"" + marker + "\") instead of doing it now. The tools were available this turn; deferring an answerable workspace question is the failure shape, not a polite hedge."
		}
	}
	// Positive criterion: workspace answer must cite SOMETHING in the
	// workspace. If the reply contains zero file-path-shaped references
	// (foo.ts, src/bar/, constants/prompts.ts:42, package.json), it's
	// generic regardless of opening phrase. Stops the whack-a-mole on
	// new forbidden openings — the model can't write a real workspace
	// answer without referencing SOMETHING in it.
	//
	// Pattern: a word followed by '/' or '.' followed by more word
	// characters. Matches: "src/foo", "foo.ts", "tools/Grep/GrepTool.ts",
	// "package.json:42". Does NOT match: "1) Pagination", "e.g.",
	// abstract enumerations.
	if !hasFileReference(reply) {
		return "my last reply contained no file-path or directory references — for a workspace question, that means I answered from generic priors without grounding the answer in this codebase. The answer must cite at least one specific file or directory I read."
	}
	return ""
}

// fileRefPattern matches path-shaped tokens: word boundaries around
// word/word, word.ext, or word/word.ext patterns. Lowercase tolerant.
// Examples that match: foo.ts, src/bar, src/bar.go, constants/prompts.ts:42
// Examples that don't: "1)", "e.g.", "etc.", standalone words.
var fileRefPattern = regexp.MustCompile(`(?i)\b[\w.-]+\.[a-z]{1,6}\b|\b[\w.-]+/[\w./-]+`)

// commonFalsePositives are abbreviations / shapes that fileRefPattern
// would flag as file refs but aren't workspace evidence. e.g. is the
// big one — "e.g." is matched by the .ext pattern.
var commonFalsePositives = map[string]bool{
	"e.g.": true,
	"i.e.": true,
	"etc.": true,
	"vs.":  true,
	"u.s.": true,
}

func hasFileReference(reply string) bool {
	matches := fileRefPattern.FindAllString(strings.ToLower(reply), -1)
	for _, m := range matches {
		if commonFalsePositives[m] {
			continue
		}
		// Looks like a real file reference.
		return true
	}
	return false
}

// criticWallClockBudget caps the critic's wall-clock. The critic
// is a one-shot prose call (no tools), so this is generous —
// reasoning models still need headroom for their <think> phase.
const criticWallClockBudget = 90 * time.Second

// criticSystemPrompt is the critic's instructions. Positive
// framing throughout: tell the critic what to do, not what not
// to do (see feedback-positive-instructions-only). The critic
// emits a PASS/FAIL token on its own line so the parser is
// trivial and unambiguous.
const criticSystemPrompt = `You are kai's own self-reflection layer. You read what kai just produced and decide whether it satisfied what the user asked for. Crucially, you write in kai's FIRST-PERSON voice — as if kai is honestly assessing its own last reply and naming what it will do next. The user reads your output as kai talking to itself out loud. Self-correction, not external judgment.

Read the ORIGINAL REQUEST (what the user asked) and the AGENT WORK SUMMARY (what kai produced). Decide whether the work addresses the ask. Be specific.

Voice rules — load-bearing:
- First person, kai's voice: "my last response...", "I described what I'd do instead of doing it...", "I'm doing X now in this turn..." — NOT "you asked", NOT "kai produced", NOT "the agent's response".
- Honest, terse, action-oriented. No groveling, no apologies, no "I should have" hedging.
- When the verdict is FAIL, the critique names the gap AND the next action in one self-message, as if kai is pivoting mid-conversation.
- When the verdict is PASS, brief acknowledgment — "the wiring is in place, the panel renders" — written as kai confirming its own work.

Output format (exactly this shape, nothing else):

CRITIQUE: <2-4 sentences in kai's first-person voice. Name the gap or confirm completion. Specific. Cite what was asked, what got produced, where the gap is.>
VERDICT: <PASS or FAIL>
RETRY_HINT: <one sentence in kai's first-person voice, framed as "Here's what I'm doing now, in this turn: ...". Only if VERDICT=FAIL.>

Standards for PASS: kai's work clearly addresses the ask. Surface-level / incomplete / "kind of did it but missed the structural part" → FAIL.

Standards for FAIL: be precise about the gap. Vague self-critique ("could be better") is unhelpful — name the missing or wrong piece.

ALSO FAIL when kai bounced an answerable question. If the AGENT WORK SUMMARY is a clarifying question ("what specifically...", "which one...", "could you provide more detail..."), check RECENT CONVERSATION first: was the referent resolvable from the prior turn? A continuation request like "fix it in the sidebar" after a prior turn about "fix the TitleBar to show current directory" — "it" is the same operation, the location is the new variable. RETRY_HINT: "I'm reading the prior turn — 'it' refers to the same operation; acting on the continuation now instead of asking for restatement."

ALSO FAIL when the bounce is answerable from the WORKSPACE, not just the conversation. "run it" / "run the app" / "run the desktop app" → the project on disk names the command (a package.json with a dev/start script — including in an obvious sub-package like client/ or app/ when the root is just a workspace manifest; a Cargo.toml, go.mod, or Makefile target). "kill the port" / "restart it" → the port and process are determinable from the dev script's config and the process kai already launched as a managed process. Asking "which project / app / command / port / directory?" when kai has kai_tree / kai_files / view and the running-process list is treating answerable context as ambiguity. The agent must LOOK (read package.json scripts, list the tree, check the managed process) and ACT, not ask. RETRY_HINT: "I'm reading the project — the run command / port is determinable from package.json and the process I launched; doing it now instead of asking."

ALSO FAIL when kai's summary contradicts something visible in RECENT CONVERSATION. If a prior turn shows "Done: 1 auto-promoted" and the current summary says "no code changes appear to have been merged", that's a verifiable lie. RETRY_HINT: "I'm reading the trailer from the prior turn — the work landed; reporting it accurately now."

ALSO FAIL when the reply leaks a PLAN SCHEMA to the user as prose. Specifically: a triple-backtick json fenced block AND the content contains plan-schema keys like "summary":, "agents":, "diagnosis":, "acceptance_criteria":, "EDIT CHECKLIST". That's visual noise — the plan card renders it separately. RETRY_HINT: "I'm reissuing as prose only — dropping the JSON plan block; the plan card already shows it."

Do NOT fail when the reply contains code fences for go, sql, bash, javascript, typescript, python, ts, js, sh, yaml, html, css, or json-without-plan-keys — those are implementation excerpts shown to the developer for review, not structured plan output. Fenced go, sql, or bash blocks in an implementation discussion are exactly what the developer wants to see. Distinguishing rule: it is a plan-schema leak ONLY if the fenced block is tagged json AND the content matches the plan schema shape (summary/agents/diagnosis/acceptance_criteria/EDIT CHECKLIST keys). Anything else is legitimate code/output — leave it alone.

Examples of the right voice (illustrative shapes, not domain-specific):

  Described-instead-of-did (action was asked, words were produced):
    CRITIQUE: my last response described the change in words and stopped. The ask was to make the change, not explain it — that's the gap.
    VERDICT: FAIL
    RETRY_HINT: Here's what I'm doing now, in this turn: making the edit, running the build, and reporting the result.

  Partial completion (one surface touched, others left):
    CRITIQUE: my edit landed in the primary file, but the related call sites and tests still reference the old name. The rename is half-done.
    VERDICT: FAIL
    RETRY_HINT: Here's what I'm doing now, in this turn: updating the remaining call sites and tests so the rename is complete across all surfaces.

  Bounced an answerable question (asked the user to restate what was already in scope):
    CRITIQUE: my last reply asked the user to clarify a continuation that the prior turn already specified. The referent was resolvable; I treated context as ambiguity.
    VERDICT: FAIL
    RETRY_HINT: Here's what I'm doing now, in this turn: acting on the continuation as the prior turn intended, no restatement needed.

  Clean PASS (concise acknowledgment):
    CRITIQUE: the change landed in the file the user named, the build passes, and the related tests still pass.
    VERDICT: PASS

Examples of the WRONG voice (do not produce):
  CRITIQUE: You asked kai to do X. Kai delivered Y instead... [second-person external judgment — write as kai's own self-reflection, first person]
  CRITIQUE: The user asked the agent to do X. The agent produced Y... [third-person narration — same issue]`

// pendingCriticRetry holds the latest FAIL critique so a single
// 'r' keypress can dispatch a retry with the critique appended.
type pendingCriticRetry struct {
	originalRequest string
	critique        string
	retryHint       string
	// pendingActionText carries the structured proposal that the
	// failed turn was meant to execute. When set, the retry prompt
	// restores the same "you offered X, user confirmed X, execute
	// X" framing the original dispatch used — so a retry of a
	// confirmation-stall has different inputs than the failed
	// attempt and isn't doomed to replay the stall. P1-3 from
	// the 2026-05-26 spec.
	pendingActionText string
}

// retryPrompt assembles the prompt for a critic-driven retry. The
// original user request leads, then a clearly-marked critique
// block tells the agent what the prior attempt missed. The model
// gets a concrete diagnosis instead of just "try again."
//
// When pendingActionText is set the retry restores the pending-
// action wrap (same shape as the original dispatch's preamble)
// before the critique, so the model sees the same structural
// "execute this proposal" framing AND a specific note on what
// went wrong last time.
func (p *pendingCriticRetry) retryPrompt() string {
	var b strings.Builder
	if p.pendingActionText != "" {
		b.WriteString(wrapPendingActionPrompt(&pendingAction{text: p.pendingActionText}, p.originalRequest))
		b.WriteString("\n\n")
	} else {
		b.WriteString(p.originalRequest)
		b.WriteString("\n\n")
	}
	b.WriteString("[Prior attempt critique — do not repeat the gaps below]\n")
	b.WriteString(p.critique)
	if strings.TrimSpace(p.retryHint) != "" {
		b.WriteString("\n\n[Concrete next step]\n")
		b.WriteString(p.retryHint)
	}
	return b.String()
}

// truncateCritique trims a critique to fit on a single trailer
// line. Used for PASS verdicts where the full critique would be
// visual noise — the user just needs a heartbeat that quality
// was checked.
func truncateCritique(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// runCritic invokes the satisfaction-gate critic and returns its
// verdict as a tea.Cmd that emits CriticReadyMsg. Always returns a
// command, even on configuration errors — the message just carries
// the error so the REPL can surface it (or silently skip).
//
// summary is the agent's reply text (for chat runs) OR a short
// description of what changed (for code runs, e.g. "Created
// kai-desktop/src/kai-theme.css; modified kai-desktop/index.html").
// Empty summary skips the critic — there's nothing to evaluate.
func runCritic(s *PlannerServices, originalRequest, summary, sessionID string) tea.Cmd {
	originalRequest = strings.TrimSpace(originalRequest)
	summary = strings.TrimSpace(summary)
	if originalRequest == "" || summary == "" {
		return nil
	}
	if s == nil || s.OrchestratorCfg.AgentProvider == nil {
		return nil
	}
	return func() tea.Msg {
		// Deterministic short-circuit: a fenced ```json (or ``` json)
		// block in the reply means the planner leaked its structured
		// payload into the prose render. The plan card already shows
		// the JSON; double-rendering it is the dogfood-reported visual
		// noise. Fail immediately without spending an LLM call — the
		// criterion is in the system prompt below as a backstop for
		// fence-less / partial-JSON cases the regex misses, but the
		// fenced case is the common one and unambiguous.
		if containsJSONFence(summary) {
			return CriticReadyMsg{
				OriginalRequest: originalRequest,
				Pass:            false,
				Critique:        "Your reply included the structured JSON plan block as prose. The plan card already renders that; showing it twice is noise.",
				RetryHint:       "Re-issue your reply as prose only — drop the ```json fence and its contents; keep the human-readable summary sentence(s).",
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), criticWallClockBudget)
		defer cancel()

		// The critic is conversational: no tools, single shot.
		// Set MaxTurns=1 explicitly so even a confused model
		// can't loop.
		//
		// Recent-turn context (2026-05-26): without prior turns
		// in scope, the critic evaluates each turn in isolation
		// and can't catch the "you bounced X as ambiguous but the
		// prior turn established the referent" failure mode. The
		// dogfood case: chat agent claimed "only a plan was
		// created, no actual code changes appear to have been
		// merged" directly contradicting the "Done: 1 auto-
		// promoted" trailer visible in scrollback — critic
		// passed it because critic couldn't see the contradicting
		// prior turn. Pulling the last 4 user/assistant turns in
		// (bounded at 800 chars each) gives the critic enough
		// state to flag those.
		recent := RecentSessionTurns(s, sessionID, 4, 800)
		var contextBlock string
		if len(recent) > 0 {
			contextBlock = "\n\nRECENT CONVERSATION (oldest → newest, for resolving 'it' / 'also' / 'the same' references and detecting contradictions with prior turns):\n"
			for _, t := range recent {
				contextBlock += "- " + t + "\n"
			}
		}
		prompt := fmt.Sprintf(
			"System: %s%s\n\nORIGINAL REQUEST:\n%s\n\nAGENT WORK SUMMARY:\n%s\n\nEvaluate now.",
			criticSystemPrompt, contextBlock, originalRequest, summary,
		)
		res, err := agent.Run(ctx, agent.Options{
			Projects:  s.Projects,
			Workspace: s.MainRepo,
			ReadOnly:  true,
			MaxTurns:  1,
			Prompt:    prompt,
			Model:     resolveCriticModel(s),
			Provider:  s.OrchestratorCfg.AgentProvider,
			Mode:      agent.ModeConversation,
			TaskName:  "critic",
		})
		if err != nil {
			return CriticReadyMsg{OriginalRequest: originalRequest, Err: err}
		}
		critique, pass, hint := parseCriticOutput(res.FinalText)
		return CriticReadyMsg{
			OriginalRequest: originalRequest,
			Pass:            pass,
			Critique:        critique,
			RetryHint:       hint,
		}
	}
}

// containsJSONFence reports whether a reply leaks a ```json (or ``` json)
// fenced block — the deterministic signal that the planner's structured
// payload landed in the prose render. Used by runCritic as a pre-LLM
// fast-fail so the obvious case doesn't burn an LLM call. The critic
// system prompt also catches fence-less leaks as a backstop.
func containsJSONFence(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "```json") || strings.Contains(lower, "``` json")
}

// parseCriticOutput extracts the three structured fields from the
// critic's response. The critic is told to emit a fixed shape, so
// parsing is line-prefixed scanning. Whitespace tolerant.
//
// On a malformed response we default Pass=true so a flaky critic
// run doesn't surface a false FAIL to the user. The trade-off:
// false negatives (real FAILs missed) over false positives (good
// runs marked FAIL). The dogfood evidence showed the critic
// itself is usually clean; the silent-failure mode is the right
// safe direction.
// The CRITIQUE and RETRY_HINT fields are MULTI-LINE aware: a model
// frequently emits the label on its own line and the content on the
// following lines ("CRITIQUE:\n<two sentences>\nVERDICT: FAIL"). The
// original same-line-only capture returned an empty critique in that
// case — which then tripped the validator's fail-closed path and
// replaced a correct, grounded verdict with a generic "couldn't
// verify" message (2026-06-01 trace). So each section accumulates
// continuation lines until the next recognized label. VERDICT is a
// single token and closes any open section.
func parseCriticOutput(text string) (critique string, pass bool, hint string) {
	pass = true
	if strings.TrimSpace(text) == "" {
		return
	}
	var critiqueLines, hintLines []string
	section := "" // "critique" | "hint" | "" (none / after verdict)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "CRITIQUE:"):
			section = "critique"
			if rest := strings.TrimSpace(strings.TrimPrefix(line, "CRITIQUE:")); rest != "" {
				critiqueLines = append(critiqueLines, rest)
			}
		case strings.HasPrefix(line, "VERDICT:"):
			section = ""
			verdict := strings.TrimSpace(strings.TrimPrefix(line, "VERDICT:"))
			pass = !strings.EqualFold(verdict, "FAIL")
		case strings.HasPrefix(line, "RETRY_HINT:"):
			section = "hint"
			if rest := strings.TrimSpace(strings.TrimPrefix(line, "RETRY_HINT:")); rest != "" {
				hintLines = append(hintLines, rest)
			}
		case line == "":
			// Blank line is a soft separator; keep the section open so
			// a paragraph break inside a critique doesn't truncate it.
		default:
			switch section {
			case "critique":
				critiqueLines = append(critiqueLines, line)
			case "hint":
				hintLines = append(hintLines, line)
			}
		}
	}
	critique = strings.TrimSpace(strings.Join(critiqueLines, " "))
	hint = strings.TrimSpace(strings.Join(hintLines, " "))
	return
}
