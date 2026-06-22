// Planner-session memory of execution failures.
//
// Without this, when a user clicks "go" on a plan and the
// orchestrator fails (e.g. preflight.no_snapshots — see
// errors.log on May-2026 for the actual case), the failure
// vanishes from the planner's perspective. The user re-prompts,
// the planner agent picks up the same session and sees the same
// prior plan, with no signal that anything went wrong — so it
// often proposes the SAME plan again. The user reported seeing
// "the same plan three times" before this landed.
//
// Fix: append a single system-role message to the planner
// session noting the classified failure. The next planner Run
// resumes the session, sees that message in context, and
// (per a new line in buildPlannerPrompt) is told NOT to
// re-propose the same plan without addressing the failure.
package views

import (
	"fmt"

	"kai/api/message"
	"kai/api/session"
	errpkg "kai/internal/tui/errors"
)

// recordExecuteFailureForPlanner appends a system message to
// the planner session so the next planner turn sees that the
// previous plan execution failed. No-op when any of the
// inputs are missing — best-effort. Errors swallowed
// because surfacing them would compete with the
// already-rendered classifier message.
func recordExecuteFailureForPlanner(store session.Store, sessionID string, ue errpkg.UserError) {
	if store == nil || sessionID == "" {
		return
	}
	// Skip auto-repairable infrastructure failures. The workspace
	// recovers from these in the background (missing blobs get
	// rebuilt, snapshots get created) without the planner needing
	// to do anything different. Recording them as "PRIOR PLAN
	// EXECUTION FAILED" was the bug surfaced in the 2026-05-25
	// dogfood: the snapshot loop kept firing missing_blobs,
	// which got recorded as a plan failure, which made the
	// planner discard its prior reasoning on every follow-up
	// turn and restart investigation from scratch. The auto-
	// repair runs in parallel and the user's natural re-ask is
	// what re-triggers planning; nothing about the plan itself
	// was wrong.
	if isAutoRepairableKind(ue.Kind) {
		return
	}
	sess, err := session.Resume(store, sessionID)
	if err != nil || sess == nil {
		return
	}
	body := fmt.Sprintf(
		"PRIOR PLAN EXECUTION FAILED.\n"+
			"Kind: %s\n"+
			"Headline: %s\n"+
			"Detail: %s\n"+
			"Action the user was told to take: %s\n\n"+
			"On the next plan: do NOT re-propose the same agents verbatim. "+
			"Either (a) address this failure (suggest the user run the action above and replan after), "+
			"or (b) ask for clarification if the failure means the original task is no longer well-defined.",
		ue.Kind, ue.Headline, ue.Detail, ue.Action,
	)
	_ = sess.AppendMessage(message.Message{
		Role:  message.RoleSystem,
		Parts: []message.ContentPart{message.TextContent{Text: body}},
	}, 0, 0)
}
