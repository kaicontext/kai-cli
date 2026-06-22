package agent

import (
	"strings"
	"testing"

	"kai/internal/agent/message"
)

// streakDecider mirrors the runner's read-streak hard-block decider,
// extracted here so the partition/splice contract can be tested
// without spinning up a full agent loop.
func streakDecider(block bool) func(message.ToolCall) interceptDecision {
	return func(c message.ToolCall) interceptDecision {
		if block && readOnlyTools[c.Name] {
			return interceptDecision{Intercept: true, Content: readStreakBlockMessage(10)}
		}
		return interceptDecision{}
	}
}

func TestPartitionCalls_Passthrough(t *testing.T) {
	calls := []message.ToolCall{
		{ID: "a", Name: "view"},
		{ID: "b", Name: "edit"},
		{ID: "c", Name: "kai_grep"},
	}
	dispatch, decisions := partitionCalls(calls, streakDecider(false))
	if len(dispatch) != 3 {
		t.Fatalf("passthrough should keep all calls, got %d", len(dispatch))
	}
	for i, d := range decisions {
		if d.Intercept {
			t.Errorf("decisions[%d].Intercept should be false in passthrough mode", i)
		}
	}
}

func TestPartitionCalls_StreakBlocksOnlyReads(t *testing.T) {
	calls := []message.ToolCall{
		{ID: "a", Name: "view"},
		{ID: "b", Name: "edit"},
		{ID: "c", Name: "bash"},
		{ID: "d", Name: "kai_grep"},
	}
	dispatch, decisions := partitionCalls(calls, streakDecider(true))
	// edit + bash should still dispatch; view + kai_grep are blocked.
	if len(dispatch) != 2 {
		t.Fatalf("blocked mode should still dispatch non-reads, got %d", len(dispatch))
	}
	wantIntercept := []bool{true, false, false, true}
	for i, want := range wantIntercept {
		if decisions[i].Intercept != want {
			t.Errorf("decisions[%d].Intercept: got %v, want %v (name=%s)", i, decisions[i].Intercept, want, calls[i].Name)
		}
	}
}

func TestSpliceIntercepted_OrderPreserved(t *testing.T) {
	calls := []message.ToolCall{
		{ID: "a", Name: "view"},
		{ID: "b", Name: "edit"},
		{ID: "c", Name: "view"},
	}
	// dispatch ran only the edit
	dispatched := []message.ContentPart{
		message.ToolResult{ToolCallID: "b", Name: "edit", Content: "ok"},
	}
	decisions := []interceptDecision{
		{Intercept: true, Content: readStreakBlockMessage(10)},
		{Intercept: false},
		{Intercept: true, Content: readStreakBlockMessage(10)},
	}

	merged := spliceIntercepted(calls, dispatched, decisions)
	if len(merged) != 3 {
		t.Fatalf("merged length: got %d, want 3", len(merged))
	}
	for i, want := range []string{"a", "b", "c"} {
		tr, ok := merged[i].(message.ToolResult)
		if !ok {
			t.Fatalf("merged[%d] not a ToolResult", i)
		}
		if tr.ToolCallID != want {
			t.Errorf("merged[%d].ToolCallID: got %q, want %q", i, tr.ToolCallID, want)
		}
	}
	// Intercepted entries must carry their content and IsError.
	if tr := merged[0].(message.ToolResult); !strings.Contains(tr.Content, "Read limit reached") || !tr.IsError {
		t.Errorf("merged[0] should be the streak block message and IsError")
	}
	if tr := merged[2].(message.ToolResult); !strings.Contains(tr.Content, "Read limit reached") || !tr.IsError {
		t.Errorf("merged[2] should be the streak block message and IsError")
	}
}

func TestSpliceIntercepted_NoInterceptsPassthrough(t *testing.T) {
	calls := []message.ToolCall{
		{ID: "a", Name: "view"},
	}
	dispatched := []message.ContentPart{
		message.ToolResult{ToolCallID: "a", Name: "view", Content: "file"},
	}
	decisions := []interceptDecision{{Intercept: false}}
	merged := spliceIntercepted(calls, dispatched, decisions)
	if len(merged) != 1 || merged[0] != dispatched[0] {
		t.Errorf("passthrough splice should return dispatched unchanged")
	}
}

func TestReadStreakThresholds_SoftBeforeHard(t *testing.T) {
	if readStreakSoftNudge >= readStreakHardBlock {
		t.Errorf("soft (%d) must be strictly less than hard (%d)",
			readStreakSoftNudge, readStreakHardBlock)
	}
}
