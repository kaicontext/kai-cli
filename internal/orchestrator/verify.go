package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"

	"kai/internal/agent"
)

// shouldVerify decides whether a finished agent run is a candidate
// for the auto-verify follow-up pass. Three conditions:
//
//  1. The agent ran in debug mode (verify is meaningful when the
//     goal was "fix a runtime problem"; coding/review/conversation
//     don't have a natural "did the fix actually work" check).
//  2. The agent applied at least one edit (no edits = nothing to
//     verify; the run was diagnostic-only).
//  3. The agent issued at least one bash call (we need a command
//     to re-run; verify-without-a-command is just the user re-asking
//     "did you fix it" with no concrete check).
//
// All three must be true. False on any condition skips verify
// silently — the agent's normal end-of-turn flow runs.
//
// 2026-05-25: extended to ModeCoding. The kai-desktop dogfood
// pinned the gap — a coding agent applied edits, ran a failing
// build, declared end-turn, and the orchestrator auto-promoted
// because no verify pass ran for coding mode. Coding agents
// genuinely have a "did the fix work" check (the build / test /
// dev command they invoked); excluding them from verify was a
// missed opportunity. Cost: one extra LLM call per coding run
// that issued bash — worth it.
func shouldVerify(mode string, editsApplied bool, firstBashCmd string) bool {
	if !editsApplied {
		return false
	}
	if strings.TrimSpace(firstBashCmd) == "" {
		return false
	}
	m := agent.ResolveMode(agent.ParseMode(mode))
	return m == agent.ModeDebug || m == agent.ModeCoding
}

// extractBashCommand pulls the .command field out of the bash tool's
// input JSON. Returns "" if the input doesn't parse or doesn't have
// a command — we'd rather skip verify than feed it garbage.
func extractBashCommand(inputJSON string) string {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.Command)
}

// buildVerifyPrompt composes the prompt for the verify-pass agent.
// The structure mirrors the manual paste-template the user proposed:
//
//	1. State the situation (previous agent applied a fix).
//	2. Give the original request as context (so the verifier knows
//	   what "fixed" was supposed to mean).
//	3. List the files that were actually changed so the verifier can
//	   compare the agent's plan against its delivery — catches the
//	   "claimed three edits, only made two" failure mode the
//	   completeness check is designed to surface.
//	4. Suggest the run command to start with (cheap nudge — the
//	   agent can override if it has a better idea).
//	5. Decision rubric with a VERIFIED sentinel for the happy path,
//	   plus an INCOMPLETE: prefix for "the runtime fix held but the
//	   planned work didn't all happen."
//
// Kept terse — the verify agent inherits the debug-mode system
// prompt (which already has investigation discipline + the
// summarize-don't-paste rule), so this prompt only needs to set up
// the verification framing.
//
// changedPaths is the deduped list of relpaths the main agent's
// edits touched (in capture order, sort-stable). Empty when the
// agent ran without edits — caller is responsible for not invoking
// verify in that case; buildVerifyPrompt is defensive and handles
// the empty list gracefully.
func buildVerifyPrompt(originalRequest, firstBashCmd string, changedPaths []string) string {
	var b strings.Builder
	b.WriteString("System: You are running a verification pass after another agent applied a fix.\n\n")
	b.WriteString("The user originally reported / the task description was:\n  ")
	b.WriteString(strings.TrimSpace(originalRequest))
	b.WriteString("\n\nThe previous agent applied edits intended to address this. Files it changed:\n")
	if len(changedPaths) == 0 {
		b.WriteString("  (none recorded — unusual; review the transcript before trusting this)\n")
	} else {
		for _, p := range changedPaths {
			b.WriteString("  - ")
			b.WriteString(p)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nYour job has two parts:\n\n")
	b.WriteString("PART A — Plan completeness check. Re-read the task description above and compare against the file list. ")
	b.WriteString("If the description called for multiple distinct changes and only some are reflected in the files changed, name the missing pieces. ")
	b.WriteString("Examples of incompleteness to look for: \"update X and Y\" but only X is in the list; \"add tests\" with no test files touched; \"export A from package P and import it in Q\" with only P changed and Q untouched. ")
	b.WriteString("If you find missing pieces and they're mechanical, apply them with edit/write. If they're judgment calls, list them in your final answer.\n\n")
	b.WriteString("PART B — Runtime verification. Re-run the command the previous agent used to surface the original problem:\n  ")
	b.WriteString("$ ")
	b.WriteString(firstBashCmd)
	b.WriteString("\n\nVERIFY EXTERNAL CONTRACTS, NOT JUST EXIT CODES. If this change depends on a contract it does not own — a CLI command or flag it spawns, an HTTP endpoint, an IPC channel, an output format — run the REAL contract (the exact command with its exact flags) and confirm it produces the shape the code consumes. A unit test that MOCKS that contract does NOT verify it: the mock encodes your assumption, so the test confirms the assumption instead of checking it. \"It compiles\", \"the process launched\", and \"a mocked test passed\" are NOT evidence the integration works — only observing the real output is. If the command/flag does not exist or emits a different shape, the feature is broken even though it builds and the host process starts cleanly — e.g. code that calls `reportgen --json` when the tool only accepts `--format=json`: it exits nonzero and produces nothing, yet the build is green.\n")
	b.WriteString("\nThen decide:\n")
	b.WriteString("  - If the run is clean AND the plan is complete → respond with the single word VERIFIED on its own line, plus one short sentence noting what you checked.\n")
	b.WriteString("  - If errors remain → fix them with edit/write tools, then re-run the command to confirm.\n")
	b.WriteString("  - If the command runs cleanly but the output looks wrong (missing styles, broken layout, empty content, wrong response shape) → fix it and re-run.\n")
	b.WriteString("  - If the runtime is clean but planned actions are missing → say \"INCOMPLETE: <one-line list of what's missing>\" and either apply the remaining edits or stop with the list.\n")
	b.WriteString("  - If you can't tell or you're blocked → say \"BLOCKED: <one-sentence reason>\" and stop.\n\n")
	b.WriteString("Do NOT redo the original investigation. The previous agent already did that work. Trust the edits unless the run command output contradicts them OR the plan/diff comparison reveals a gap. ")
	b.WriteString("If the original error is gone but a new one appeared, that's still a fix worth applying — name the new error in your response.\n")
	return b.String()
}

// verifyOutcome classifies the verify agent's final response.
type verifyOutcome int

const (
	verifyUnknown    verifyOutcome = iota
	verifyPassed                   // contained VERIFIED sentinel
	verifyBlocked                  // contained BLOCKED: prefix
	verifyApplied                  // applied additional edits (treated as "still working on it")
	verifyIncomplete               // runtime ok but plan-completeness check failed
)

// classifyVerifyTranscript scans the agent run's final assistant
// text for the sentinel markers. We look at the LAST assistant
// message because verify agents tend to investigate, then conclude
// — the conclusion is what counts.
//
// Priority order matters: additional edits beat any sentinel (the
// loop hasn't closed). INCOMPLETE: beats VERIFIED because a verify
// agent that found a plan gap AND ran the fix command cleanly will
// often emit both — we want to surface the gap, not the green.
func classifyVerifyTranscript(finalText string, additionalEdits int) verifyOutcome {
	if additionalEdits > 0 {
		// Even if VERIFIED appears, applied edits mean the verify
		// pass found and patched a real issue — flag it so the TUI
		// can surface that the loop hadn't fully closed.
		return verifyApplied
	}
	upper := strings.ToUpper(finalText)
	if strings.Contains(upper, "INCOMPLETE:") {
		return verifyIncomplete
	}
	if strings.Contains(upper, "BLOCKED:") {
		return verifyBlocked
	}
	if strings.Contains(upper, "VERIFIED") {
		return verifyPassed
	}
	return verifyUnknown
}

// verifySummary returns the one-line summary the orchestrator
// surfaces back to the TUI for the run's verify block. Stays short
// — the TUI puts it on a single line under the "─ verify ─" header.
func verifySummary(outcome verifyOutcome, additionalEdits int) string {
	switch outcome {
	case verifyPassed:
		return "✓ verified — the fix holds and the plan is complete"
	case verifyApplied:
		return fmt.Sprintf("⚠ verify applied %d additional edit(s) — re-run if you want to re-verify the latest state", additionalEdits)
	case verifyIncomplete:
		return "⚠ verify found planned work missing — see the verify block above for what wasn't done"
	case verifyBlocked:
		return "⚠ verify could not run — see the verify block above for the reason"
	default:
		return "⚠ verify finished without a clear pass/fail signal — review the output above"
	}
}
