package planner

import (
	"strings"
	"testing"
)

func TestAnnotatePlannerFailure_TagsSummaryAndRiskNotes(t *testing.T) {
	plan := &WorkPlan{
		Summary:   "garbled empty plan",
		Agents:    []AgentTask{{Name: "ignored"}},
		RiskNotes: []string{"original note"},
	}
	out := annotatePlannerFailure(plan, "imperative plan with zero tool calls", repromptExploration)

	if !strings.HasPrefix(out.Summary, "planner failed") {
		t.Errorf("summary should be replaced with failure message, got: %q", out.Summary)
	}
	if !strings.Contains(out.Summary, "/model") {
		t.Errorf("failure summary should hint at /model swap: %q", out.Summary)
	}
	if len(out.Agents) != 0 {
		t.Errorf("failure annotation should clear agents, got %d", len(out.Agents))
	}
	if len(out.RiskNotes) != 2 || !strings.Contains(out.RiskNotes[0], "planner failed") {
		t.Errorf("first RiskNote should be the failure header, got: %v", out.RiskNotes)
	}
	if !strings.Contains(out.RiskNotes[0], "imperative plan with zero tool calls") {
		t.Errorf("failure header should carry the reject reason, got: %q", out.RiskNotes[0])
	}
	// Original RiskNote preserved at position 1.
	if out.RiskNotes[1] != "original note" {
		t.Errorf("original RiskNotes should be preserved, got: %v", out.RiskNotes)
	}
}

func TestAnnotatePlannerFailure_NilPlanSafe(t *testing.T) {
	out := annotatePlannerFailure(nil, "", repromptNone)
	if out == nil {
		t.Fatalf("nil plan should be promoted to empty WorkPlan, not nil")
	}
	if !strings.HasPrefix(out.Summary, "planner failed") {
		t.Errorf("nil-plan annotation should still set the failure summary, got: %q", out.Summary)
	}
}
