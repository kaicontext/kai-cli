package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
)

// kaiStatusTool exposes `kai status` to the agent so it can query
// kai's own VC layer: pending changesets, capture state, push state —
// the kai-equivalent of "what haven't I committed/pushed yet" that
// sits above the git level.
//
// This is the companion to kai_git_state. That tool answers "what
// does git think about this repo?" — uncommitted dirt, branch,
// ahead/behind. This tool answers "what does kai think about this
// repo?" — snapshots taken, changesets ready to push, divergence
// from last integrated state. Both matter. An agent that only checks
// git dirt may miss "clean but kai hasn't captured a snapshot yet."
type kaiStatusTool struct {
	// kaiBinary is the absolute path to the kai executable. Empty
	// disables the tool (Run returns a clear error). The runner
	// wires this from cfg in cmd/kai/tui.go.
	kaiBinary string
	// workspace is the cwd to run `kai status` in. The repo root.
	workspace string
}

type kaiStatusParams struct {
	// Against is an optional baseline ref/selector (e.g. "@snap:last",
	// "@snap:prev"). Passed through to `kai status --against`.
	// Empty means use the CLI default.
	Against string `json:"against"`
}

func (t *kaiStatusTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_status",
		Description: "Run `kai status` to show kai's VC-layer state: pending changesets " +
			"(captured but not integrated), snapshot divergence, and push state. " +
			"Use this as the kai-level companion to kai_git_state — git may be clean " +
			"while kai still has unbundled changes or unpushed snapshots. " +
			"Accepts an optional `against` ref (e.g. \"@snap:last\") to compare " +
			"against a specific baseline; defaults to the last integrated snapshot.",
		Parameters: map[string]any{
			"against": map[string]any{
				"type":        "string",
				"description": "Optional baseline ref/selector (e.g. \"@snap:last\"). Defaults to the CLI default.",
			},
		},
	}
}

func (t *kaiStatusTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.kaiBinary == "" || t.workspace == "" {
		return NewTextErrorResponse(
			"kai_status: not configured (binary or workspace missing). "+
				"Restart kai or report this as a bug.",
		), nil
	}
	var p kaiStatusParams
	if len(call.Input) > 0 {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_status: invalid input json: " + err.Error()), nil
		}
	}

	args := []string{"status", "--json"}
	if p.Against != "" {
		args = append(args, "--against", p.Against)
	}

	cmd := exec.CommandContext(ctx, t.kaiBinary, args...)
	cmd.Dir = t.workspace
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return NewTextErrorResponse(
			"kai_status: " + err.Error() + ": " + strings.TrimSpace(string(out)),
		), nil
	}

	// --json output: pass through as-is. The model can read JSON.
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return NewTextResponse("kai_status: no output (everything up to date)"), nil
	}
	return NewTextResponse(trimmed), nil
}