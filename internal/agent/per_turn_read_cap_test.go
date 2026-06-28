package agent

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

func TestPerTurnReadCap_FirstNDispatch_RestBlocked(t *testing.T) {
	// Simulate the decide function's per-turn counter directly by
	// invoking partitionCalls with a custom decider that mirrors
	// the runner's logic. Pure-helper test — no model, no dispatch.
	cap := 3
	readsSoFar := 0
	calls := []message.ToolCall{
		{ID: "r1", Name: "view"},
		{ID: "r2", Name: "kai_grep"},
		{ID: "r3", Name: "view"},
		{ID: "r4", Name: "view"},     // first over-cap → blocked
		{ID: "e1", Name: "edit"},     // non-read → passes through
		{ID: "r5", Name: "kai_grep"}, // counted, but cap already hit → blocked
	}
	decide := func(c message.ToolCall) interceptDecision {
		if readOnlyTools[c.Name] {
			readsSoFar++
			if readsSoFar > cap {
				return interceptDecision{Intercept: true, Content: perTurnReadCapMessage(cap)}
			}
		}
		return interceptDecision{}
	}
	_, decisions := partitionCalls(calls, decide)

	wantIntercept := []bool{false, false, false, true, false, true}
	for i, want := range wantIntercept {
		if decisions[i].Intercept != want {
			t.Errorf("decisions[%d] (%s): got intercept=%v, want %v",
				i, calls[i].Name, decisions[i].Intercept, want)
		}
	}
	if !strings.Contains(decisions[3].Content, "Per-turn read cap reached") {
		t.Errorf("blocked entry should carry the cap message, got %q", decisions[3].Content)
	}
}

func TestPerTurnReadCap_ZeroDisables(t *testing.T) {
	// Cap=0 (Options default) means uncapped — every call passes.
	cap := 0
	readsSoFar := 0
	calls := []message.ToolCall{
		{ID: "r1", Name: "view"},
		{ID: "r2", Name: "view"},
		{ID: "r3", Name: "view"},
		{ID: "r4", Name: "view"},
		{ID: "r5", Name: "view"},
		{ID: "r6", Name: "view"},
		{ID: "r7", Name: "view"},
	}
	decide := func(c message.ToolCall) interceptDecision {
		if readOnlyTools[c.Name] {
			readsSoFar++
			if cap > 0 && readsSoFar > cap {
				return interceptDecision{Intercept: true, Content: perTurnReadCapMessage(cap)}
			}
		}
		return interceptDecision{}
	}
	_, decisions := partitionCalls(calls, decide)
	for i, d := range decisions {
		if d.Intercept {
			t.Errorf("cap=0 should allow every read, but decisions[%d] was intercepted", i)
		}
	}
}

func TestPerTurnReadCapMessage_NamesCap(t *testing.T) {
	msg := perTurnReadCapMessage(5)
	if !strings.Contains(msg, "5 read-only tool calls") {
		t.Errorf("cap message should name the cap value, got: %q", msg)
	}
	if !strings.Contains(msg, "Next turn the cap resets") {
		t.Errorf("cap message should explain the turn-grain reset, got: %q", msg)
	}
}
