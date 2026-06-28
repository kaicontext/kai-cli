package agent

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

func TestRejectPrematureCheckpoints_RewritesOnFail(t *testing.T) {
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "e1", Name: "edit", Content: "[file written]\n\n[auto-build: FAIL (go, 120ms)]\nmain.go:5:2: undefined: foo"},
		message.ToolResult{ToolCallID: "cp1", Name: "kai_checkpoint", Content: `{"checkpoint_id":"cp_000001","recorded":true}`},
	}
	rejectPrematureCheckpoints(parts)

	tr := parts[1].(message.ToolResult)
	if !tr.IsError {
		t.Errorf("checkpoint should be marked IsError after rejection")
	}
	if !strings.Contains(tr.Content, "build is currently broken") {
		t.Errorf("rejection content missing build reason: %q", tr.Content)
	}
}

func TestRejectPrematureCheckpoints_LeavesCheckpointOnOKBuild(t *testing.T) {
	originalCheckpoint := `{"checkpoint_id":"cp_000001","recorded":true}`
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "e1", Name: "edit", Content: "[file written]\n\n[auto-build: OK (go, 80ms)]"},
		message.ToolResult{ToolCallID: "cp1", Name: "kai_checkpoint", Content: originalCheckpoint},
	}
	rejectPrematureCheckpoints(parts)

	tr := parts[1].(message.ToolResult)
	if tr.IsError {
		t.Errorf("clean build should not flip checkpoint to error")
	}
	if tr.Content != originalCheckpoint {
		t.Errorf("clean-build checkpoint should be untouched, got %q", tr.Content)
	}
}

func TestRejectPrematureCheckpoints_NoBuildTrailer_NoOp(t *testing.T) {
	// Mixed batch with no build trailer at all (e.g. view-only turn).
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "v1", Name: "view", Content: "file body"},
		message.ToolResult{ToolCallID: "cp1", Name: "kai_checkpoint", Content: `{"checkpoint_id":"cp_000001","recorded":true}`},
	}
	rejectPrematureCheckpoints(parts)

	tr := parts[1].(message.ToolResult)
	if tr.IsError {
		t.Errorf("missing build trailer should not reject the checkpoint")
	}
}
