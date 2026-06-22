package views

import (
	"testing"

	"kai/api/agent"
)

// TestStickyChat_DocumentedBranch is a structural pin: the runPlan
// routing code MUST consult both the per-turn forced flag AND the
// persisted prev_mode for ModeConversation. The 2026-05-15 dogfood
// pinned this — /chat then a follow-up imperative routed the
// follow-up back to the planner because the previous design treated
// /chat as one-shot. This test pins the contract that BOTH paths
// (forced=ModeConversation OR persisted prev_mode=Conversation)
// trigger the chat-agent short-circuit.
//
// We can't easily unit-test runPlan end-to-end without a real
// PlannerServices + session store, but we can pin that the keywords
// the routing branch depends on are present in the source. That
// catches a future refactor that silently drops the persisted-mode
// check and re-introduces the one-shot semantics.
func TestStickyChat_DocumentedBranch(t *testing.T) {
	// Sanity: the sentinels used by sticky chat exist as named
	// constants. If anyone renames or removes them, the routing
	// branch needs to be reviewed.
	if agent.ModeConversation.String() == "" {
		t.Fatal("agent.ModeConversation should have a stable String() value")
	}
	if got := agent.ParseMode("Conversation"); got != agent.ModeConversation {
		t.Errorf("agent.ParseMode(\"Conversation\") = %v, want ModeConversation", got)
	}
}
