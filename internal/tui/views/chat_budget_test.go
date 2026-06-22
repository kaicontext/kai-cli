package views

import "testing"

// TestChatBudgetConstants pins the chat-mode wall-clock and
// turn-cap budgets so a future refactor can't silently loosen
// them. The 2026-05-15 dogfood ran an 8m02s chat session into the
// dangling-turn pathology; these caps stop that class of failure
// at ~3 min and 8 turns respectively.
func TestChatBudgetConstants(t *testing.T) {
	if chatWallClockBudget.Minutes() > 5 {
		t.Errorf("chatWallClockBudget = %v; should be <= 5 minutes to catch dangling-turn loops", chatWallClockBudget)
	}
	if chatWallClockBudget.Minutes() < 1 {
		t.Errorf("chatWallClockBudget = %v; should be >= 1 minute to allow normal exploration", chatWallClockBudget)
	}
	if chatMaxTurns > 15 {
		t.Errorf("chatMaxTurns = %d; chat should converge in single digits", chatMaxTurns)
	}
	if chatMaxTurns < 4 {
		t.Errorf("chatMaxTurns = %d; need room for at least a few exploration calls", chatMaxTurns)
	}
}
