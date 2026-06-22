package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// kai_history.go: agent tools that expose Kai's snapshot/authorship
// history by shelling out to the `kai` CLI, mirroring kaiDiffTool.
// These cover the provenance side git can't: `kai blame` (AI vs human
// authorship per line) and `kai log` (the snapshot timeline, where each
// snapshot is labeled with the orchestrator run that produced it — the
// only place "which run introduced this" is recorded, since absorbed
// changes are often uncommitted in git).

// --- kai_blame -------------------------------------------------------

type kaiBlameTool struct {
	kaiBinary string
	workspace string
}

type kaiBlameParams struct {
	File string `json:"file"`
}

func (t *kaiBlameTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_blame",
		Description: "Show AI-vs-human authorship per line for a file (who/what last wrote each " +
			"line). Use it to tell whether code under a bug was AI-generated or hand-written " +
			"before deciding how much to trust it. Note: authorship is recorded from edit " +
			"checkpoints; lines with no checkpoint show as unattributed.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Path of the file to blame, relative to the repo root.",
			},
		},
		Required: []string{"file"},
	}
}

func (t *kaiBlameTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.kaiBinary == "" || t.workspace == "" {
		return NewTextErrorResponse("kai_blame: not configured (binary or workspace missing)."), nil
	}
	var p kaiBlameParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_blame: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.File) == "" {
		return NewTextErrorResponse("kai_blame: file required"), nil
	}
	out, err := runKaiCLI(ctx, t.kaiBinary, t.workspace, "blame", p.File)
	if err != nil {
		return NewTextErrorResponse("kai_blame: " + err.Error() + ": " + strings.TrimSpace(out)), nil
	}
	return NewTextResponse(strings.TrimSpace(out)), nil
}

// --- kai_log ---------------------------------------------------------

type kaiLogTool struct {
	kaiBinary string
	workspace string
}

type kaiLogParams struct {
	Limit int `json:"limit"`
}

func (t *kaiLogTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_log",
		Description: "Show the snapshot timeline — Kai's history of captures, each labeled with " +
			"the change message or the ORCHESTRATOR RUN that produced it (e.g. \"orchestrator: " +
			"add-gate-list-json-flag\"). This is the provenance git diff can't give: it traces " +
			"WHEN and by which run a change was introduced. Pair it with kai_diff {since, until} " +
			"to see exactly what a given snapshot changed. Newest first.",
		Parameters: map[string]any{
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max number of snapshots to show (default 20). Older entries are omitted.",
			},
		},
	}
}

func (t *kaiLogTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.kaiBinary == "" || t.workspace == "" {
		return NewTextErrorResponse("kai_log: not configured (binary or workspace missing)."), nil
	}
	var p kaiLogParams
	if len(call.Input) > 0 {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_log: invalid input json: " + err.Error()), nil
		}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 20
	}
	out, err := runKaiCLI(ctx, t.kaiBinary, t.workspace, "log")
	if err != nil {
		return NewTextErrorResponse("kai_log: " + err.Error() + ": " + strings.TrimSpace(out)), nil
	}
	return NewTextResponse(truncateSnapshotLog(out, limit)), nil
}

// truncateSnapshotLog keeps the first `limit` snapshot entries of
// `kai log` output. Entries start with a line beginning "snap "; we
// count those and cut before the (limit+1)-th, appending a note so the
// agent knows to raise the limit if it needs older history.
func truncateSnapshotLog(out string, limit int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	count := 0
	var kept []string
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimLeft(ln, " \t"), "snap ") {
			count++
			if count > limit {
				kept = append(kept, "… ("+strconv.Itoa(limit)+"-snapshot limit reached; pass a larger \"limit\" for older history)")
				break
			}
		}
		kept = append(kept, ln)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n")
}

// runKaiCLI runs `kai <args...>` in the workspace and returns combined
// output. Shared by the shell-backed history tools.
func runKaiCLI(ctx context.Context, bin, workspace string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, bin, args...)
	c.Dir = workspace
	c.Env = os.Environ()
	o, e := c.CombinedOutput()
	return string(o), e
}
