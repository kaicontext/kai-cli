package orchestrator

import (
	"errors"
	"strings"
	"testing"

	"kai/internal/planner"
	"github.com/kaicontext/kai-engine/safetygate"
	"kai/internal/workspace"
)

func mkRun(name string, verdict string, exitErr, integrateErr error) AgentRun {
	r := AgentRun{
		Task:         planner.AgentTask{Name: name},
		ExitErr:      exitErr,
		IntegrateErr: integrateErr,
	}
	if verdict != "" {
		r.Verdict = &workspace.IntegrationDecision{Verdict: verdict}
	}
	return r
}

func TestDemoteAutoPromotedSiblings_NoOpWhenAllAuto(t *testing.T) {
	runs := []AgentRun{
		mkRun("a", string(safetygate.Auto), nil, nil),
		mkRun("b", string(safetygate.Auto), nil, nil),
	}
	demoteAutoPromotedSiblings(runs)
	for _, r := range runs {
		if r.Verdict.Verdict != string(safetygate.Auto) {
			t.Errorf("clean plan should leave Auto verdicts alone, got %q for %s", r.Verdict.Verdict, r.Task.Name)
		}
	}
}

func TestDemoteAutoPromotedSiblings_DemotesWhenPeerHeld(t *testing.T) {
	runs := []AgentRun{
		mkRun("impl-agent", string(safetygate.Auto), nil, nil),
		mkRun("test-agent", string(safetygate.Review), nil, nil),
	}
	demoteAutoPromotedSiblings(runs)

	implVerdict := runs[0].Verdict
	if implVerdict.Verdict != string(safetygate.Review) {
		t.Errorf("auto-promoted impl should be demoted to Review when test agent held, got %q", implVerdict.Verdict)
	}
	joined := strings.Join(implVerdict.Reasons, " | ")
	if !strings.Contains(joined, "atomic integrate") || !strings.Contains(joined, "test-agent") {
		t.Errorf("demotion reason missing context: %v", implVerdict.Reasons)
	}
	// Held peer is left untouched.
	if runs[1].Verdict.Verdict != string(safetygate.Review) {
		t.Errorf("held peer verdict should be untouched, got %q", runs[1].Verdict.Verdict)
	}
}

func TestDemoteAutoPromotedSiblings_DemotesWhenPeerExitErr(t *testing.T) {
	runs := []AgentRun{
		mkRun("impl-agent", string(safetygate.Auto), nil, nil),
		mkRun("test-agent", "", errors.New("worker crashed"), nil),
	}
	demoteAutoPromotedSiblings(runs)

	implVerdict := runs[0].Verdict
	if implVerdict.Verdict != string(safetygate.Review) {
		t.Errorf("auto-promoted impl should be demoted when test agent exit-errored, got %q", implVerdict.Verdict)
	}
}

func TestDemoteAutoPromotedSiblings_SingleRunUnchanged(t *testing.T) {
	runs := []AgentRun{
		mkRun("solo", string(safetygate.Auto), nil, nil),
	}
	demoteAutoPromotedSiblings(runs)
	if runs[0].Verdict.Verdict != string(safetygate.Auto) {
		t.Errorf("single-run plan should not be touched, got %q", runs[0].Verdict.Verdict)
	}
}

