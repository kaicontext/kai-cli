package agent

import (
	"fmt"

	"github.com/kaicontext/kai-engine/tools"
)

// convergence.go: mode-aware convergence policy for the runner. The
// runner injects a convergence notice when turnsLeft drops below
// convergeTurnsBefore, and strips tools entirely on the final turn.
//
// Until now both behaviors were mode-agnostic, written for the planner
// case ("emit JSON as your reply"). For coding agents the same
// instructions are catastrophic: "produce the final answer" reads as
// "write a prose reply" and the model dutifully writes prose
// describing the edits instead of calling edit/write. On the final
// turn the model literally cannot edit — tools are gone — and the run
// completes with zero changes (round-15 dogfood, 2026-05-13).
//
// Splitting the policy by mode here keeps runner.go's main loop simple
// while making the behavior unit-testable.

// convergenceHint returns the per-turn user-role hint string to inject
// when turnsLeft is within the convergence window. Returns empty when
// no hint should fire. The hint is mode-aware:
//   - Coding: "make the edits via edit/write now; prose is not the deliverable"
//   - Other:  "stop exploring and produce your final answer" (existing)
//
// Caller is responsible for the turnsLeft <= convergeTurnsBefore check;
// this function only differentiates by turnsLeft >=2 vs. final-turn.
func convergenceHint(mode Mode, turnsLeft int) string {
	switch ResolveMode(mode) {
	case ModeCoding:
		if turnsLeft <= 1 {
			return "FINAL TURN. Make the edits NOW by calling the edit or write tool. " +
				"Describing edits in prose is NOT the deliverable — calling edit/write IS. " +
				"If you genuinely cannot make a specific edit, end with `I'm blocked because <X>` " +
				"and the specific question — no prose summary, no markdown table of edits, " +
				"no 'here are the changes' description. The run will be marked failed if you " +
				"produce a text answer instead of tool calls."
		}
		return fmt.Sprintf("Convergence notice: %d turns left. Make any remaining edits NOW via edit/write. "+
			"Stop gathering context — you have enough. Prose descriptions of edits will not be accepted as the deliverable.",
			turnsLeft)
	default:
		if turnsLeft <= 1 {
			return "FINAL TURN — NO TOOLS AVAILABLE. You have NO tools and NO further turns. Produce the FINAL ANSWER right now using only what you've learned so far. " +
				"If the task asked for JSON: emit the JSON object directly, in a fenced ```json``` block, as your entire reply. " +
				"If the task asked for prose: write the prose answer directly. " +
				"DO NOT write 'let me check', 'let me verify', or any phrase that implies further action — there is no further action available. " +
				"DO NOT describe what you would do; just do it. " +
				"A short, decisive answer with the evidence you have is correct; a 'let me look at one more thing' response is INCORRECT and will be discarded."
		}
		return fmt.Sprintf("Convergence notice: you have %d turns left. Stop exploring and produce your final answer. The user values a partial-but-shipped answer over a complete-but-incomplete one.", turnsLeft)
	}
}

// windDownThreshold is the turns-left value at and below which the
// wind-down hint fires. Distinct from convergeTurnsBefore (3) so the
// wind-down message lands one turn earlier than convergence — the
// agent has one warned-but-still-flexible turn to decide whether to
// finish, checkpoint partial progress, or revert. After that
// convergence takes over with stricter "make the edit now" language.
const windDownThreshold = 4

// windDownHint returns the budget-warning text to inject when the run
// is within windDownThreshold turns of the cap. Distinct from the
// convergence hint: convergence says "produce the deliverable";
// wind-down says "leave the workspace in a coherent state — a clean
// workspace is better than a broken half-change." Returns empty for
// turnsLeft above the threshold; caller is responsible for the gate.
func windDownHint(turnsLeft int) string {
	return fmt.Sprintf(`[runner] BUDGET WARNING: %d turns remaining. Do NOT start new exploration or new files.
- If your current edit is incomplete: finish it, run the build, checkpoint.
- If you have uncommitted working changes: checkpoint what compiles, revert what doesn't.
- If nothing compiles: revert all changes and leave the workspace clean.

A clean workspace with no changes is better than a broken workspace with half a change.`, turnsLeft)
}

// finalTurnTools returns the tool list to expose to the model on the
// final turn. For non-coding modes this is nil — same as before:
// stripping tools forces the model to produce a text reply instead of
// emitting another tool_use it has no slot to follow up on.
//
// For coding mode, we keep only the deliverable tools (edit, write) and
// strip the research tools (view, kai_grep, kai_callers, etc.). The
// model still can't loop back into exploration, but it has the means to
// produce the actual deliverable when the convergence hint says
// "make the edits now."
func finalTurnTools(mode Mode, full []tools.ToolInfo) []tools.ToolInfo {
	if ResolveMode(mode) != ModeCoding {
		return nil
	}
	var keep []tools.ToolInfo
	for _, t := range full {
		if isEditingTool(t.Name) {
			keep = append(keep, t)
		}
	}
	return keep
}

// isEditingTool reports whether a tool name represents a file-mutating
// deliverable tool (as opposed to a read-only research tool). Used by
// finalTurnTools to filter the registry on the final turn in coding
// mode. The list is intentionally narrow — adding a new editing tool
// requires explicitly updating this list, so we never silently let a
// new tool through the final-turn filter without thinking about it.
func isEditingTool(name string) bool {
	switch name {
	case "edit", "write":
		return true
	}
	return false
}
