package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kai/internal/authorship"
)

// kaiCheckpointTool records per-edit authorship so `kai blame` can
// attribute lines to specific agents/sessions. Coding and Debug
// modes both list this tool; the system prompt for those modes asks
// the model to call it after each edit.
//
// We deliberately use the existing authorship.CheckpointWriter
// rather than re-implementing the file format. The writer manages
// the .kai/checkpoints/<session>/NNNNNN.json layout, sequence
// numbers, and atomic rename — none of which the agent needs to
// know about.
type kaiCheckpointTool struct {
	writer *authorship.CheckpointWriter
	// agent is the agent label for the AuthorType="ai" attribution
	// (e.g. "kai-agent", or the orchestrator's task name). Threaded
	// from the runner so multi-agent sessions can blame distinctly.
	agent string
	// model is the LLM model id captured on each checkpoint so
	// `kai blame` can show "lines authored by claude-sonnet-4-6".
	model string
}

type kaiCheckpointParams struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Action    string `json:"action"`
}

type kaiCheckpointResult struct {
	CheckpointID string `json:"checkpoint_id"`
	Recorded     bool   `json:"recorded"`
}

func (t *kaiCheckpointTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_checkpoint",
		Description: "Record an edit checkpoint so `kai blame` can attribute the affected " +
			"lines to this agent/session. Call this AFTER every successful file edit (write " +
			"or edit tool call) with the line range you actually changed. Skipping it makes " +
			"the resulting code appear human-authored in attribution stats.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "File path relative to repo root.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "First line affected (1-indexed).",
			},
			"end_line": map[string]any{
				"type":        "integer",
				"description": "Last line affected (1-indexed, inclusive).",
			},
			"action": map[string]any{
				"type":        "string",
				"description": "One of: created, modified, deleted, conflict.",
			},
		},
		Required: []string{"file", "start_line", "end_line", "action"},
	}
}

func (t *kaiCheckpointTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.writer == nil {
		// Tool was registered but the runner didn't supply a writer
		// (no .kai dir or session id). Surface a clear error instead
		// of silently succeeding so the developer notices the gap.
		return NewTextErrorResponse(
			"kai_checkpoint: not configured (no .kai directory or session id available)",
		), nil
	}
	var p kaiCheckpointParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_checkpoint: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.File) == "" {
		return NewTextErrorResponse("kai_checkpoint: file required"), nil
	}
	if p.StartLine < 1 || p.EndLine < p.StartLine {
		return NewTextErrorResponse(fmt.Sprintf(
			"kai_checkpoint: invalid line range start=%d end=%d (need 1 ≤ start ≤ end)",
			p.StartLine, p.EndLine,
		)), nil
	}
	action := strings.ToLower(strings.TrimSpace(p.Action))
	if !validCheckpointAction(action) {
		return NewTextErrorResponse(
			"kai_checkpoint: action must be one of: created, modified, deleted, conflict; got " + p.Action,
		), nil
	}

	rec := authorship.CheckpointRecord{
		File:       p.File,
		StartLine:  p.StartLine,
		EndLine:    p.EndLine,
		Action:     action,
		AuthorType: "ai",
		Agent:      t.agent,
		Model:      t.model,
		Timestamp:  time.Now().UnixMilli(),
	}
	seq, err := t.writer.Write(rec)
	if err != nil {
		return NewTextErrorResponse("kai_checkpoint: write: " + err.Error()), nil
	}
	out := kaiCheckpointResult{
		CheckpointID: fmt.Sprintf("cp_%06d", seq),
		Recorded:     true,
	}
	body, err := json.Marshal(out)
	if err != nil {
		return NewTextErrorResponse("kai_checkpoint: marshal: " + err.Error()), nil
	}
	return NewTextResponse(string(body)), nil
}

// validCheckpointAction matches authorship.CheckpointRecord.Action's
// accepted values. The agent prompt should align with these so the
// model uses the right vocabulary; a typo gets rejected with the
// list above.
func validCheckpointAction(s string) bool {
	switch s {
	case "created", "modified", "deleted", "conflict",
		// "insert" is a synonym some prompts use; map to
		// "modified" downstream callers expect from blame stats
		// rather than rejecting.
		"insert":
		return true
	}
	return false
}
