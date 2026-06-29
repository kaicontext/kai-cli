package views

import "testing"

// TestChatBudgetConstants pins the chat-mode wall-clock and
// turn-cap budgets so a future refactor can't silently loosen
// them. The 2026-05-15 dogfood ran an 8m02s chat session into the
// dangling-turn pathology; the wall-clock cap stops that class of
// failure at ~3 min. The turn cap was 8 when chat was read-only Q&A;
// post the "chat is code mode" merge the path does explore→edit→
// build→verify, so it needs a coding-scale budget (raised to 30,
// 2026-06-07) — but still bounded so a real narration loop can't run
// unbounded.
func TestChatBudgetConstants(t *testing.T) {
	if chatWallClockBudget.Minutes() > 5 {
		t.Errorf("chatWallClockBudget = %v; should be <= 5 minutes to catch dangling-turn loops", chatWallClockBudget)
	}
	if chatWallClockBudget.Minutes() < 1 {
		t.Errorf("chatWallClockBudget = %v; should be >= 1 minute to allow normal exploration", chatWallClockBudget)
	}
	// Ceiling: chat-as-code needs room to explore+edit+build+verify, but
	// must stay bounded (a narration loop past this is pathology the
	// search/dangle/test-fight guards should catch).
	if chatMaxTurns > 40 {
		t.Errorf("chatMaxTurns = %d; chat-as-code is still bounded, not the 50-turn coding default", chatMaxTurns)
	}
	// Floor: must clear the executor's 20-turn coding budget, since
	// chat-as-code folds in the verify pass the executor runs separately.
	if chatMaxTurns < 20 {
		t.Errorf("chatMaxTurns = %d; coding-via-chat needs at least the executor's budget", chatMaxTurns)
	}
}
