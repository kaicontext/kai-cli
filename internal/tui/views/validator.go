// Package views: claim-validation pass. After the chat agent
// produces a factual answer about the workspace, the validator
// re-checks the answer's claims by RUNNING tools — not by
// reasoning about them. It runs the commands the answer cites,
// greps the symbols it asserts, and reads the file:lines it
// points to, then compares what the tools actually returned to
// what the answer claimed.
//
// Why this exists, and why it is NOT the prose critic:
// confabulation can't be instructed away — an LLM emits
// verification-shaped tokens ("verified via kai live --help")
// whether or not the thing is true, so the chat agent's own
// self-certification is worthless as proof. The structural fix is
// to not trust the narrative: let a tool the HARNESS runs decide.
// The 2026-05-31 news-ticker arc is the canonical miss — three
// contradictory confident answers about a feature that spawns
// `kai live --format json`, a flag that does not exist. The prose
// critic (no tools) couldn't catch it because catching it requires
// RUNNING the flag and seeing "unknown flag", which is exactly
// what this pass does.
//
// The validator emits a CriticReadyMsg so it reuses the critic's
// FAIL → auto-retry → restore-retracted-answer pipeline wholesale.
// On the chat path it REPLACES the prose critic for replies that
// make checkable claims about the codebase (replies that cite a
// file/dir); conceptual replies still go to the prose critic.
package views

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"kai/api/agent"
	"kai/api/planner"
	"kai/api/provider"
	tea "github.com/charmbracelet/bubbletea"
)

// validatorWallClockBudget caps the validator's wall clock. Unlike
// the prose critic (a single no-tool shot), the validator runs real
// commands and greps over several turns, so it needs more room —
// but it is still bounded so a hung `kai live` or a slow grep can't
// strand the user behind a spinner.
const validatorWallClockBudget = 180 * time.Second

// validatorMaxTurns caps the validator's tool loop. Enough to run a
// handful of checks (run the cited command, grep the asserted
// symbol on both ends, read the cited lines) and then emit the
// verdict; tight enough that it converges instead of exploring.
const validatorMaxTurns = 14

// validatorBashLivePreviewLines caps how many output lines per bash
// call get streamed to the live UI. The full output is always in the
// debug trace; the live feed just shows the first few so a verbose
// `--help` doesn't bury the spinner.
const validatorBashLivePreviewLines = 4

// validatorSystemPrompt instructs the validation pass. The contract
// mirrors the critic's CRITIQUE/VERDICT/RETRY_HINT shape so
// parseCriticOutput and the retry pipeline are reused unchanged, but
// the discipline is different: the verdict must come from tool
// output the validator actually ran, never from reasoning about the
// answer. Positive framing throughout (tell it what to do). The
// concrete examples are from a neutral domain on purpose — a build
// tool's `--json` flag, a "record-updated" event — so the rule
// generalizes and never reflects the user's in-flight feature back
// at the agent.
const validatorSystemPrompt = `You are kai's verification pass. You are handed an answer kai just produced and the request it answered. Your job: decide whether the answer's CONCLUSION is TRUE — and you decide by RUNNING tools, not by reasoning. You write in kai's first-person voice, as kai checking its own work against reality before it stands behind it.

Anchor on the QUESTION, not on the answer's framing. Before you trust how the answer broke the problem down, ask: what would have to be true for this answer to be correct — INCLUDING facts the answer never mentioned? An answer can be built entirely from true statements and still reach a false conclusion by omitting the one load-bearing fact. You are judging the truth of the conclusion, not the truth of the sentences the answer chose to write. So derive the checks from the question yourself, then see whether the answer survives them.

The crux is usually upstream of where the answer looked. When the conclusion is that a command, flag, integration, or feature works / is used / is wired correctly, the load-bearing fact is whether the actual invocation works — and an answer will often trace the easy DOWNSTREAM plumbing (the handlers, consumers, render path) and never run the upstream command. So: GREP for where that command/flag is actually invoked in the code (the spawn/exec/call site), and RUN it exactly as invoked. Confirming the code downstream of a command is NOT confirming the command works. (Neutral shape: asked "is the dashboard using reportgen correctly?", an answer may verify how the dashboard renders reportgen's output and conclude "correct" — while the code spawns 'reportgen --json', a flag that errors. Reading the renderer proves nothing; running 'reportgen --json' does.)

VERIFY THE FIX, NOT JUST THE DIAGNOSIS. When the answer claims it FIXED something, or proposes a new command/flag/approach/code change as the solution, that solution is ITSELF a contract you must verify — confirming the OLD approach was broken proves nothing about whether the NEW one works. Read the changed code, identify the external commands, flags, and output shapes the NEW code now depends on, and RUN them exactly as the new code invokes them. Confirm the real output actually contains the fields/lines the new code parses or expects. A fix that swaps a broken command for another command you never ran — or that parses fields you never confirmed exist in the real output — is unverified, and an unverified fix does not pass. (Neutral shape: an answer "fixes" a broken 'reportgen --json' by switching to parsing 'reportgen status' text for a field it calls "Total". You must run 'reportgen status' and confirm a line "Total" actually appears — if the real output says "total_count", the new code parses nothing and the fix is just as broken as what it replaced.)

GREEN TESTS ARE NOT PROOF THE FEATURE WORKS. When the answer concludes a feature is DONE, an acceptance criterion is MET, or "the tests pass so it works" — a passing test counts as proof ONLY if it actually exercises the real production path. Before you accept green, open the test the claim rests on and check three things: (1) it is not SKIPPED — a t.Skip, build tag, or early return on a missing env var / credential / fixture means the test ran nothing, so it is not coverage and the criterion is unmet, not met; (2) its fixtures/mocks match the PRODUCTION source of truth — if the test builds an in-memory input keyed or shaped differently than the config/data file the production code actually loads, the green proves the test's private fixture works, not the real path; (3) its assertions actually reach the CHANGED function on a realistic input, not a value the test itself planted. If any of these fails, the "done"/"passes"/"met" claim is UNVERIFIED — say which test and which gap, and FAIL. Re-running the suite and seeing green is NOT enough; you must read what the green actually exercises. (Neutral shape: an answer marks a term-substitution feature done because its unit test passes — but the test feeds an in-memory map keyed on the already-substituted form, while production loads a data file keyed on the ORIGINAL form and substitutes only AFTER a transform that never reproduces the original. The test is green and the feature is a no-op. Read the data file the code loads and confirm the test uses the same keys it does.)

WHEN THE CONTRACT CAN'T BE RUN HERE. Some fixes cannot be exercised in your environment: GUI runtime behavior (a desktop-app window opening, a screen-capture or permission dialog, a camera/mic/display device), or a long-running interactive server you would have to click through. You have no display and no hands — launching a GUI app prints "Not supported" / "no xvfb", and waiting on a dev server just burns your whole budget and strands the user behind a spinner. Do NOT try to run those. Instead verify everything that IS checkable from here — and it is more than it looks:
- The build / typecheck actually passes — run it (e.g. the project's build or tsc), read the real exit code.
- The NEW code calls a REAL, CURRENT API for the platform. A GUI fix can compile and still use a removed or wrong API. Confirm the symbol/option the fix now depends on actually exists in the INSTALLED version — grep the shipped types/dist under node_modules (or the vendored dep), not just the source. (Neutral shape: a fix switches a capture call to an option like chromeMediaSource:"desktop"; grep node_modules for that option to confirm the installed framework version still accepts it rather than having dropped it two majors ago.)
- The code path is wired end to end (producer → bridge/preload → consumer) — grep all the way through.
Then emit a real VERDICT. If the checkable parts hold, PASS — and in the critique say plainly which parts you machine-verified and which single step the user must test by hand (e.g. "build passes and getDesktopSources exists in the installed electron; click Record once to confirm the OS permission dialog appears"). A contract you cannot run here is NOT an automatic failure: an honest "verified X by tool, hand-test Y" is the correct verdict — never spend the budget relaunching a GUI and then fall back to "couldn't verify."

Then check the answer's stated claims too. For each concrete, checkable claim, run the matching tool and read the REAL result:

- A claim about how a command or flag behaves ("reportgen build --json emits JSON lines") — RUN that exact command with bash and read its real exit code and output. A command that exits non-zero, or prints "unknown flag" / "command not found" / a usage error, FALSIFIES any claim that depends on it working. Reading the code that calls a command is NOT running the command — run it.
- A STRUCTURAL claim — who calls a function, what depends on it, its blast radius / impact, whether a symbol is unused/unexported/dead, "everything that breaks if I change/delete X", how many callers there are — is verified with the GRAPH TOOLS, not by grepping. Run kai_callers / kai_dependents / kai_impact / kai_context on the symbol and compare the graph's set to the answer's set. The graph is complete and exact (it catches interface/indirect dispatch and transitive callers that grep misses, and it won't over-count on name collisions). For a completeness claim ("nothing else breaks", "N callers, no more"), kai_impact's full set IS the check — diff it against what the answer listed. Do this FIRST for structural claims; it is one fast precise query versus an unbounded grep hunt.
- A claim that a symbol, file, flag, or string merely exists, is used, or is absent (non-structural) — GREP for it. For a wiring claim about a channel/event/identifier (e.g. a "record-updated" event), grep the connecting name and confirm BOTH ends: the producer that emits it and the consumer that handles it. "Zero presence" or "it's correct" without searching the other end is not verified.
- A claim that points to a specific file and line — open and READ those lines and confirm they say what the answer claims.
- A claim about runtime behavior you cannot reproduce with a tool (a race, a timing window, what a user visually sees) — mark it UNVERIFIED. Do not let it pass as confirmed fact, and do not invent a check that "confirms" it.

Run the checks first. Only after you have the real tool output do you decide.

STAY IN SCOPE. Verify the answer's CENTRAL conclusion and the few load-bearing claims it rests on — not every secondary detail, aside, or comparison the answer happens to draw. Do not follow the answer's references into other projects or code trees unless they are load-bearing for the conclusion. You have only a few minutes of wall-clock; spend them confirming the things that would actually FLIP the verdict, not exhaustively re-deriving the whole answer. Running out of time produces NO verdict, which is worse than a focused verdict on the claims that matter.

CONVERGE — this is load-bearing. Run each distinct check ONCE. The first time you run a command and read its output, that output IS your evidence: do not re-run the same command in another form to re-confirm it (a second run of the same flag with a different exit-code capture tells you nothing new). The moment you have run the checks the question needs, STOP and emit CRITIQUE / VERDICT / RETRY_HINT. Re-verifying a result you already hold burns your turn budget and risks producing NO verdict at all — and a missing verdict is treated as a failure to verify, which wrongly rejects a correct answer. A handful of checks then a verdict; never a loop.

Output format (exactly this shape, nothing else):

CRITIQUE: <2-4 sentences, kai's first-person voice. On PASS, name the checks you actually ran and what they returned ("I ran X and got Y"). On FAIL, name each claim the tools contradicted and QUOTE the real tool output that contradicts it.>
VERDICT: <PASS or FAIL>
RETRY_HINT: <one sentence, kai's first-person voice, framed "Here's what I'm doing now, in this turn: ..." — only when VERDICT=FAIL. Say what the corrected answer is, grounded in the tool output you saw.>

Standards for PASS: you ran the checks the question demanded and none contradicted the answer. When the conclusion is that a command/flag/integration/feature works or is used correctly, PASS requires that you actually located and RAN its invocation and saw it succeed — confirming downstream plumbing alone is not enough. When the answer proposes a FIX, PASS also requires that you RAN the fix's own new commands and confirmed their real output contains what the new code depends on. A claim you could not check at all does not earn a PASS for the surrounding answer if the answer stated it as fact.

Standards for FAIL: any tool output contradicts a claim, OR the answer concludes a command/feature is correct while the actual invocation you ran errors, OR the answer's proposed fix depends on a command or output shape you ran and found to error or not match, OR the answer states as established fact a behavior that no tool you ran could confirm, OR the answer calls a feature done / a criterion met on the strength of a test you found is skipped, fixture-only, or keyed/shaped differently than the production data it claims to cover (a green test that does not exercise the real path is not proof). Be precise — quote the command you ran and the bytes it returned.

Voice: first person, kai's own ("I ran...", "my last answer claimed... but..."), terse, no apologies.`

// runValidator invokes the tool-using claim-validation pass and
// returns its verdict as a tea.Cmd that emits CriticReadyMsg —
// the same message the prose critic emits, so the REPL's existing
// FAIL → auto-retry → restore-retracted-answer handling applies
// unchanged. Always returns a command (or nil when there is
// nothing to validate / no provider); errors ride on the message.
//
// reply is the chat agent's answer text. The validator runs in the
// same workspace with the same bash + kai-binary + graph wiring the
// chat agent had, so any command the answer cites is runnable here,
// but on a DIFFERENT model (the critic model family) so it doesn't
// share the chat agent's training prior and rationalize the same
// claims.
func runValidator(s *PlannerServices, originalRequest, reply, sessionID string) tea.Cmd {
	originalRequest = strings.TrimSpace(originalRequest)
	reply = strings.TrimSpace(reply)
	if originalRequest == "" || reply == "" {
		return nil
	}
	if s == nil || s.OrchestratorCfg.AgentProvider == nil {
		return nil
	}
	return func() tea.Msg {
		// Deterministic short-circuit, same as the prose critic: a
		// fenced ```json plan block leaked into the prose render is
		// a presentation failure, not a truth question — fail it
		// without spending a tool loop.
		if containsJSONFence(reply) {
			return CriticReadyMsg{
				OriginalRequest: originalRequest,
				Pass:            false,
				Critique:        "Your reply included the structured JSON plan block as prose. The plan card already renders that; showing it twice is noise.",
				RetryHint:       "Re-issue your reply as prose only — drop the ```json fence and its contents; keep the human-readable summary sentence(s).",
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), validatorWallClockBudget)
		defer cancel()

		// Dedicated debug log so a validator run is traceable — the
		// TUI swallows stderr, CriticReadyMsg{Err} is silently dropped
		// by the REPL, and the validator shares no log with the chat
		// agent (chat-debug.log is chat-only). Without this a validator
		// PASS, FAIL, or silent crash are indistinguishable from the
		// outside. Writes <kaiDir>/validator-debug.log; best-effort
		// (nil DebugLog no-ops every method when KaiDir is unset).
		dbg, _ := planner.OpenDebugLogNamed(s.KaiDir, originalRequest, "validator-debug.log", "VALIDATOR")
		defer dbg.Close()

		// kai_diff and any cited `kai ...` command need the running
		// kai binary path — same resolution runChatAgent uses.
		kaiBin := "kai"
		if exe, err := os.Executable(); err == nil {
			kaiBin = exe
		}

		// Hand the validator a runnable kai binary explicitly. The
		// 2026-05-31 trace showed the validator correctly decided to
		// run the cited `kai live --json` but couldn't: `which kai`
		// returns nothing in the TUI's bash PATH (the binary lives in
		// ~/bin or is named `kit`, off PATH), and the app's own
		// relative kaiPath wasn't obviously executable to it. So it
		// hunted and gave up on the one check that decides the answer.
		// Telling it the absolute path the running process resolved
		// removes the hunt — a command-correctness verdict can't be
		// reached without actually running the command.
		binGuidance := "\n\nRUNNABLE KAI BINARY: a working kai CLI is at " + kaiBin +
			" — invoke kai commands through it (e.g. to check a flag the code spawns, run `" + kaiBin +
			" live --json` and read the real exit code and output). Do not search PATH for `kai`; use this path. If the code under review resolves its OWN binary (an env var like KAI_PATH, or a relative path that exists and is executable), you may run that exact one instead; otherwise use this one."

		claimGuidance := ""
		if looksLikeDoneClaim(reply) {
			claimGuidance = doneClaimGuidance
		}
		prompt := "System: " + validatorSystemPrompt + binGuidance + claimGuidance +
			"\n\nORIGINAL REQUEST:\n" + originalRequest +
			"\n\nANSWER TO VALIDATE:\n" + reply +
			"\n\nValidate now. Run the checks, then emit CRITIQUE / VERDICT / RETRY_HINT."

		// Per-bash-call output line counter for the live-feed cap below
		// (reset in OnToolCall on each new bash dispatch).
		var bashLineCount int

		res, err := agent.Run(ctx, agent.Options{
			Projects:    s.Projects,
			Workspace:   s.MainRepo,
			SharedPaths: s.SharedPaths,
			// Validator inspects; it must not mutate. ReadOnly drops
			// write/edit file tools. Bash stays on (a verification
			// pass that can't run the command it's verifying is
			// useless) — BashAllow still bounds it.
			ReadOnly:   true,
			MaxTurns:   validatorMaxTurns,
			Prompt:     prompt,
			Model:      resolveCriticModel(s),
			Provider:   s.OrchestratorCfg.AgentProvider,
			// Graph ON, but its per-turn "Files in scope" injection OFF
			// (NoGraphContextInjection). The validator needs the graph
			// TOOLS — kai_callers/kai_dependents/kai_impact/kai_context —
			// to verify STRUCTURAL claims (callers, blast radius, "X is
			// unused/unexported") precisely and instantly, instead of
			// grep-and-read exploring (which timed it out on a 51-caller
			// blast-radius answer, 2026-06-02). But the scope-hint block
			// misled it onto phantom files (2026-06-01), so we take the
			// tools without the hint.
			Graph:                   s.OrchestratorCfg.MainGraph,
			NoGraphContextInjection: true,
			EnableBash:              true,
			BashAllow:  s.OrchestratorCfg.AgentBashAllow,
			KaiBinary:  kaiBin,
			// Let the validator read the managed dev-server's recent
			// output when a claim is about what that process printed.
			ManagedProcLogger: NewManagedProcLogger(s),
			MaxTotalTokens:    s.OrchestratorCfg.MaxAgentTokens,
			Mode:              agent.ModeConversation,
			TaskName:          "validator",
			// The validator must not stand behind a verdict it reached
			// without searching — GroundAnswers blocks a terminal
			// prose answer that has no tool call behind it, which is
			// exactly the discipline this pass enforces on itself.
			GroundAnswers: true,
			// Preserve full tool-result content across turns. This is
			// LOAD-BEARING for the validator: its verdict must be grounded
			// in what the commands ACTUALLY returned, but the runner's
			// default trims older tool results to a one-line stub to save
			// tokens. The 2026-06-02 dogfood caught the consequence — the
			// validator ran `npm test`, it PASSED, the result got trimmed
			// to "[orig: 25 lines] [trimmed]", and on its verdict turn the
			// model could no longer see the pass and confabulated "no test
			// files found", false-failing a true claim. The pass built to
			// stop confabulation was itself confabulating because the trim
			// ate its evidence. Every other tool-using pass (chat, planner,
			// gate review) already sets this; the validator needs it most.
			KeepToolResults: true,
			// Reading and running checks IS the validator's job, so
			// the default read-streak guard (which nudges an agent to
			// stop reading and answer) works against it. The 2026-05-31
			// trace showed the validator getting "5+ consecutive
			// read-only turns" hard-nudges while it was still trying to
			// locate and run the cited command. Give it generous room;
			// MaxTurns + the wall-clock budget remain the real bounds.
			ReadStreakSoft: validatorMaxTurns,
			ReadStreakHard: validatorMaxTurns,
			MaxReadsPerTurn: 0,
			// Two sinks per hook: the debug log (durable trace) AND the
			// REPL's live-activity channel (so the user watches the
			// validator work in real time instead of staring at a frozen
			// gray line). emit is non-blocking — a slow renderer drops
			// events rather than stalling the validator loop, same
			// contract the chat agent uses.
			Hooks: agent.Hooks{
				OnRequest: func(turn int, req provider.Request) { dbg.Request(turn, req) },
				OnToolCall: func(name, inputJSON string) {
					if name == "bash" {
						bashLineCount = 0 // reset the per-call output cap
					}
					dbg.Tool(name, inputJSON)
					emitValidatorActivity(s, ChatActivityEvent{Kind: "tool", Summary: summarizeToolCall(name, inputJSON), When: time.Now()})
				},
				OnBashOutput: func(line string) {
					dbg.Routing("bash> " + line) // full output stays in the durable trace
					// Live feed: cap each bash call at the first few lines
					// so a verbose `--help` (20+ lines) doesn't flood the
					// transcript and defeat the readable-activity goal.
					bashLineCount++
					if bashLineCount <= validatorBashLivePreviewLines {
						emitValidatorActivity(s, ChatActivityEvent{Kind: "bash", Summary: line, When: time.Now()})
					} else if bashLineCount == validatorBashLivePreviewLines+1 {
						emitValidatorActivity(s, ChatActivityEvent{Kind: "bash", Summary: "  …(more output in the trace)", When: time.Now()})
					}
				},
				OnAssistantText:  func(text string) { dbg.Text(text) },
				OnReasoningDelta: func(delta string) { dbg.ReasoningDelta(delta) },
				OnTurnComplete:   func(tIn, tOut, tCached int) { dbg.Turn(tIn, tOut, tCached) },
			},
		})
		if err != nil {
			dbg.Errorf("agent.Run failed: %v", err)
			return CriticReadyMsg{OriginalRequest: originalRequest, Err: err}
		}
		critique, pass, hint := parseValidatorOutput(res.FinalText)
		dbg.Text(fmt.Sprintf("VALIDATOR VERDICT pass=%v critique=%q hint=%q", pass, critique, hint))
		return CriticReadyMsg{
			OriginalRequest: originalRequest,
			Pass:            pass,
			Critique:        critique,
			RetryHint:       hint,
		}
	}
}

// parseValidatorOutput extracts the validator's verdict, FAILING
// CLOSED. Unlike parseCriticOutput (which defaults Pass=true so a
// flaky prose critic doesn't surface a false FAIL), the validator's
// entire purpose is "nothing passes as fact unless a tool confirmed
// it" — so the safe default is the OPPOSITE. A PASS is honored only
// when the output explicitly says VERDICT: PASS *and* carries a
// non-empty critique describing the checks that backed it. Anything
// else — empty output, no VERDICT line, a PASS with no supporting
// critique (the 2026-06-01 trace: an 8-second empty result that
// defaulted to PASS and let the wrong ticker answer through), or an
// explicit FAIL — does not pass. When the model produced no usable
// verdict, we synthesize a "couldn't verify" critique so the
// auto-retry re-runs the checks instead of standing behind an
// unverified answer.
func parseValidatorOutput(text string) (critique string, pass bool, hint string) {
	c, p, h := parseCriticOutput(text)
	sawVerdict := false
	for _, raw := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "VERDICT:") {
			sawVerdict = true
			break
		}
	}
	// Fail closed: an explicit, critique-backed PASS is the only path
	// to pass. No verdict line, or a verdict with no critique, means
	// the validator did not actually confirm anything.
	if !sawVerdict || strings.TrimSpace(c) == "" {
		return "I couldn't complete the verification — I didn't produce a clear verdict backed by checks I ran, so I'm not standing behind the prior answer as verified.",
			false,
			"Here's what I'm doing now, in this turn: re-running the checks the question actually needs — locating and running the real command/invocation, grepping both ends — before I answer."
	}
	return c, p, h
}

// emitValidatorActivity sends a live-activity event to the REPL's
// chat-activity channel (the same feed the chat agent uses), so the
// validator's tool calls and bash output render in real time under
// the spinner. Non-blocking: a full/absent channel drops the event
// rather than stalling the validator's agent loop.
func emitValidatorActivity(s *PlannerServices, ev ChatActivityEvent) {
	if s == nil || s.ChatActivityCh == nil {
		return
	}
	select {
	case s.ChatActivityCh <- ev:
	default:
	}
}

// structuralQuestionSignals are request-text markers of a question whose
// correct answer is a function of the semantic graph — callers,
// dependents, impact/blast-radius, dead/unused code, "what breaks if I
// change/delete X". For these, kit's edge is that the graph computes the
// answer exactly; the model just phrases it. Literal substrings only.
var structuralQuestionSignals = []string{
	"who calls", "what calls", "callers of", "called by", "call sites",
	"depends on", "dependents", "depend on", "what uses", "used by",
	"blast radius", "impact of", "what would break", "what breaks",
	"if i delete", "if i remove", "if i change", "safe to delete",
	"safe to remove", "unused", "dead code", "everything that breaks",
	"everything that would break",
}

// isStructuralQuestion reports whether the request is a code-structure
// question best answered (and self-verified) from the graph.
func isStructuralQuestion(request string) bool {
	q := strings.ToLower(request)
	for _, s := range structuralQuestionSignals {
		if strings.Contains(q, s) {
			return true
		}
	}
	return false
}

// structuralGroundingGuidance is the GENERATION-TIME grounding for a
// structural question — the upstream version of what the validator does
// after the fact. If the answer is computed from the graph, there is
// nothing left to verify: the graph already verified it. So we force the
// graph query here, at generation, instead of letting the model narrate
// freely and re-checking later.
const structuralGroundingGuidance = `STRUCTURAL QUESTION — ANSWER FROM THE GRAPH, DO NOT GUESS. This question is about code structure: who calls a function, what depends on it, its callers / blast radius / impact, whether a symbol is used / unused / dead, or what would break if it changed or were deleted. Answer it from the semantic graph, NOT from reading a few files and NOT from memory. Run kai_impact / kai_callers / kai_dependents / kai_context on the exact target symbol and build your answer FROM that output — the graph is complete and exact: it captures interface and indirect dispatch and transitive callers that grep misses, and it won't over-count on name collisions. State NO caller, dependency, count, or blast-radius fact you did not get from a graph query. For a completeness claim ("nothing else breaks", "N callers and no more"), kai_impact's full set IS your evidence — report exactly what it returns, nothing added.`

// shouldValidateClaims decides whether a chat reply should go to the
// tool-using validator instead of the prose critic. The validator is
// the right gate when the answer makes claims this codebase can be
// queried to confirm or refute — i.e. it references concrete
// artifacts (a file or directory) or the request was a workspace
// question. Conceptual answers ("most systems do X") have nothing to
// run, so they stay with the prose critic + the generic-opening
// fast-path.
//
// hasFileReference and classifyWorkspaceTurn are the same robust
// signals the critic already uses — no new heuristic is introduced
// here; the routing reuses them.
func shouldValidateClaims(reply, request string) bool {
	return hasFileReference(reply) || classifyWorkspaceTurn(request)
}

// doneClaimSignals are phrases by which a chat answer asserts the
// requested work is ALREADY complete / needs nothing. Lowercased,
// substring match. Kept tight to "it's already done" assertions —
// not "I did X" (that's a normal completion report, validated by the
// usual path), and not "X is done loading" style incidental uses.
var doneClaimSignals = []string{
	"already done", "already implemented", "already exist", "already in place",
	"already complete", "already wired", "already supported", "already handled",
	"nothing to do", "nothing to implement", "no changes needed", "no work needed",
	"no changes required", "is already", "are already",
}

// looksLikeDoneClaim reports whether a substantive chat reply asserts
// the requested change is already implemented. These answers are the
// ones that must be verified against the ENFORCEMENT/usage code, not
// just the definitions — the 2026-06-09 starter-tier "✓ Already done"
// false positive (tiers defined in config + tierRank, but the commit
// limit unenforced, no checkout path, webhooks unrouted) is the case
// this gate exists to catch.
func looksLikeDoneClaim(reply string) bool {
	q := strings.ToLower(reply)
	for _, s := range doneClaimSignals {
		if strings.Contains(q, s) {
			return true
		}
	}
	return false
}

// doneClaimGuidance is appended to the validator's prompt when the
// answer it's checking is an "already done" claim. It forbids the
// shallow trap — "the names exist, therefore it's done" — and demands
// verification of the wiring.
const doneClaimGuidance = "\n\nCOMPLETION-CLAIM CHECK. The answer claims the requested feature/change is ALREADY implemented, or that there is nothing to do. Do NOT accept that from the EXISTENCE of definitions: a config field, a constant, an enum/struct field, a switch case, or a type EXISTING is not the feature working. Verify it is wired END-TO-END:\n- find where the thing is ENFORCED or USED, not just declared — grep the symbol's USES, not its definition;\n- check that every guard/branch/reject handles the NEW case, not only the old ones (a reject still keyed on the old value, a checkout that only routes the old price, a webhook that falls back to the old default);\n- for a user-facing capability, confirm there is a real path to TRIGGER it end to end (it can be set, sold, returned, displayed).\nIf the definitions exist but the enforcement/usage is missing or only covers the old values, the \"already done\" claim is FALSE — VERDICT: FAIL and list the specific unwired gaps (file + what is missing). Only VERDICT: PASS if the feature is genuinely wired end to end."
