package agent

import (
	"strings"
	"testing"
)

func TestWindDownHint_IncludesRemainingCount(t *testing.T) {
	out := windDownHint(4)
	if !strings.Contains(out, "4 turns remaining") {
		t.Errorf("hint should include the remaining count, got: %q", out)
	}
	if !strings.Contains(out, "clean workspace") {
		t.Errorf("hint should mention clean-workspace preference, got: %q", out)
	}
}

func TestWindDownThreshold_AboveConvergence(t *testing.T) {
	// Wind-down must fire at least one turn before convergence so the
	// agent gets a warned-but-still-flexible turn before convergence
	// demands the deliverable. The convergence threshold in runner.go
	// is convergeTurnsBefore=3; wind-down should be strictly higher.
	if windDownThreshold <= 3 {
		t.Errorf("windDownThreshold (%d) must exceed convergeTurnsBefore (3) so wind-down fires first",
			windDownThreshold)
	}
}
