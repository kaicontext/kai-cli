package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// kaiConsultTool escalates a stuck exploration to a stronger model for
// a single diagnosis-only exchange. The cheap model passes its goal,
// what it tried, and what's blocking it; the strong model returns a
// structured "diagnosis / files / actions" reply that the cheap model
// then acts on.
//
// Why this exists: cheap models thrash when stuck — they widen
// searches, retry failed paths, fall back to bash, and burn 20+ turns
// producing nothing. They are bad at self-diagnosis. A stronger model
// can usually pinpoint the problem in one turn for ~$0.02–0.03; that
// beats $0.20+ of burned cheap-model budget that produces no edit.
//
// Diagnosis-only by contract: the strong model is forbidden to write
// code in its reply. It points; the cheap model writes. This keeps the
// expensive call small (~500 output tokens) and preserves ownership of
// the edit in the cheap model.
type kaiConsultTool struct {
	provider Sender
	model    string
	// workspace is the agent's CWD. Injected into the consult prompt
	// so the strong model sees the actual working directory rather
	// than having to trust the agent's recollection — the most common
	// stuck-agent failure (May 2026 dogfood) was reading at a
	// non-existent path because the agent guessed wrong about CWD.
	workspace string
	// mode is the agent's mode name ("coding", "debug", etc.).
	// Optional context the strong model uses to calibrate its
	// suggestions — e.g. don't recommend "edit X" to a planner.
	mode string
}

type kaiConsultParams struct {
	// Goal is the one-sentence statement of what the agent is trying
	// to do. Required. Without this the strong model has no anchor.
	Goal string `json:"goal"`
	// Tried is the agent's recollection of what it has already
	// attempted, one line per attempt. Format suggestion:
	// "<tool> <args> → <result>". Required, even if just one entry,
	// because the absence of "tried" almost always means the agent
	// hasn't actually tried much yet — the right move then is
	// usually one more read, not an escalation.
	Tried []string `json:"tried"`
	// BlockedBy is one sentence on what is blocking the agent. Why
	// is the next read not obvious? Required for the same reason as
	// Tried — if the agent can't articulate the blocker, it isn't
	// stuck enough to need this tool.
	BlockedBy string `json:"blocked_by"`
}

func (t *kaiConsultTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_consult",
		Description: "Escalate to a stronger model for a single diagnosis when you are " +
			"stuck. Use this when 5+ reads have not converged on a clear next action — " +
			"the stronger model returns a focused diagnosis (where to look, what to do " +
			"next) but does NOT write code. Required fields: goal (what you're trying " +
			"to do), tried (what you've already attempted, one per line), blocked_by " +
			"(what is blocking you). Returns: a short diagnosis the cheap model then " +
			"acts on. Costs roughly 10–30× a normal turn — use when stuck, not as a " +
			"first move.",
		Parameters: map[string]any{
			"goal": map[string]any{
				"type":        "string",
				"description": "One sentence: what are you trying to do? Be concrete (file/symbol if known).",
			},
			"tried": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "What you have already tried, one entry per attempt. Suggested format: \"<tool> <args> → <result>\". List your last 3–5 attempts.",
			},
			"blocked_by": map[string]any{
				"type":        "string",
				"description": "One sentence: what is blocking you? Why is the next read not obvious?",
			},
		},
		Required: []string{"goal", "tried", "blocked_by"},
	}
}

func (t *kaiConsultTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.provider == nil {
		return NewTextErrorResponse("kai_consult: no consult provider configured for this run"), nil
	}
	var p kaiConsultParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_consult: invalid input json: " + err.Error()), nil
		}
	}
	if strings.TrimSpace(p.Goal) == "" {
		return NewTextErrorResponse("kai_consult: goal required"), nil
	}
	if strings.TrimSpace(p.BlockedBy) == "" {
		return NewTextErrorResponse("kai_consult: blocked_by required"), nil
	}
	if len(p.Tried) == 0 {
		return NewTextErrorResponse("kai_consult: tried required (list at least one prior attempt — if you haven't tried anything, you aren't stuck yet)"), nil
	}

	req := SenderRequest{
		Model:     t.model,
		System:    consultSystemPrompt,
		UserText:  buildConsultUserPrompt(p, t.workspace, t.mode),
		MaxTokens: 1024,
	}
	resp, err := t.provider.Send(ctx, req)
	if err != nil {
		return NewTextErrorResponse(fmt.Sprintf("kai_consult: provider call failed: %v", err)), nil
	}

	out := strings.TrimSpace(resp.Text)
	if out == "" {
		return NewTextErrorResponse("kai_consult: provider returned empty response"), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "kai_consult (model: %s):\n\n", t.model)
	b.WriteString(out)
	return NewTextResponse(b.String()), nil
}

const consultSystemPrompt = `You are a senior engineer being consulted by an automated agent that is stuck on a coding task. Your job is to diagnose the problem and tell the agent exactly where to look and what to do next.

Constraints:
- DO NOT write code. The agent will write the code. You point.
- Be concrete: name actual file paths and symbols, not categories.
- If you do not have enough information to pinpoint the issue, say so explicitly and tell the agent the ONE specific piece of information you would need.
- If the agent's "tried" list shows a wrong assumption (wrong working directory, wrong file path, wrong package layout), name that as the diagnosis — that is usually the answer.

Respond in this exact format:

DIAGNOSIS: <one sentence — what is actually wrong>
FILES: <up to 3 file paths the agent should read, comma-separated, or "none — verify the assumption above first">
ACTIONS:
1. <concrete step>
2. <concrete step>
3. <concrete step, optional>

Keep the whole reply under 300 words.`

func buildConsultUserPrompt(p kaiConsultParams, workspace, mode string) string {
	var b strings.Builder
	// Runner-supplied context first — these are the facts the agent
	// can't lie about. Strong model can compare the agent's
	// "tried" entries against the actual workspace layout to spot
	// wrong-path / wrong-CWD failures, which is the dominant
	// stuck-agent pattern.
	if workspace != "" {
		fmt.Fprintf(&b, "Working directory: %s\n", workspace)
		if layout := topLevelLayout(workspace); layout != "" {
			fmt.Fprintf(&b, "Top-level layout:\n%s", layout)
		}
	}
	if mode != "" {
		fmt.Fprintf(&b, "Agent mode: %s\n", mode)
	}
	if workspace != "" || mode != "" {
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Goal: %s\n\n", strings.TrimSpace(p.Goal))
	b.WriteString("What I have tried:\n")
	for _, t := range p.Tried {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		fmt.Fprintf(&b, "  - %s\n", t)
	}
	fmt.Fprintf(&b, "\nWhat is blocking me: %s\n", strings.TrimSpace(p.BlockedBy))
	return b.String()
}

// topLevelLayout renders one shallow level of the workspace as a
// hint for the strong model. Cheap (single ReadDir, no recursion)
// and bounded (cap at 50 entries — anything bigger means the
// workspace is a monorepo and even a flat listing is informative
// enough to spot wrong-CWD).
//
// Returns "" on any error so the prompt just omits the section
// rather than leaking a "<error>" placeholder to the strong model.
func topLevelLayout(workspace string) string {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return ""
	}
	const cap = 50
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	truncated := false
	if len(names) > cap {
		names = names[:cap]
		truncated = true
	}
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	if truncated {
		fmt.Fprintf(&b, "  ... (%d more entries omitted)\n", len(entries)-cap)
	}
	return b.String()
}
