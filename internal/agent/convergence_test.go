package agent

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/tools"
)

func TestConvergenceHint_Coding(t *testing.T) {
	// Coding mode must direct the model at edit/write tools and
	// explicitly reject prose-as-deliverable. Round-15 failure shape:
	// the model emitted a beautifully-structured prose answer with
	// markdown code blocks for every edit, and the run shipped 0
	// changes.
	t.Run("convergence window", func(t *testing.T) {
		h := convergenceHint(ModeCoding, 3)
		if !strings.Contains(h, "edit/write") {
			t.Errorf("coding convergence must name edit/write tools, got: %q", h)
		}
		if strings.Contains(h, "final answer") {
			t.Errorf("coding convergence must NOT say 'final answer' (reads as prose-deliverable), got: %q", h)
		}
	})

	t.Run("final turn", func(t *testing.T) {
		h := convergenceHint(ModeCoding, 1)
		if !strings.Contains(h, "edit") || !strings.Contains(h, "write") {
			t.Errorf("coding final-turn must name edit and write, got: %q", h)
		}
		if !strings.Contains(h, "deliverable") {
			t.Errorf("coding final-turn must call out deliverable shape, got: %q", h)
		}
		if !strings.Contains(h, "blocked") {
			t.Errorf("coding final-turn must offer the I'm blocked escape hatch, got: %q", h)
		}
	})
}

func TestConvergenceHint_PlannerUnchanged(t *testing.T) {
	// Regression guard for the existing planner/chat path. The
	// planner's final-turn behavior depends on the "emit JSON as
	// your reply" instruction — changing it would re-break every
	// planner round we just spent the day stabilizing.
	t.Run("convergence window", func(t *testing.T) {
		h := convergenceHint(ModePlanning, 3)
		if !strings.Contains(h, "final answer") {
			t.Errorf("non-coding convergence must keep 'final answer' wording, got: %q", h)
		}
	})

	t.Run("final turn", func(t *testing.T) {
		h := convergenceHint(ModePlanning, 1)
		if !strings.Contains(h, "NO TOOLS AVAILABLE") {
			t.Errorf("non-coding final-turn must keep the NO TOOLS prefix, got: %q", h)
		}
		if !strings.Contains(h, "JSON") {
			t.Errorf("non-coding final-turn must mention JSON for planner case, got: %q", h)
		}
	})
}

func TestFinalTurnTools_CodingKeepsEditWrite(t *testing.T) {
	// On final turn in coding mode, research tools (view, kai_grep,
	// kai_callers, bash, etc.) must be stripped, but edit and write
	// MUST survive — otherwise the model has no way to comply with
	// "make the edits now." Round-15: tools=0 on final turn meant
	// the model wrote "I'm blocked because I have no tools."
	full := []tools.ToolInfo{
		{Name: "view"}, {Name: "kai_grep"}, {Name: "kai_callers"},
		{Name: "bash"}, {Name: "edit"}, {Name: "write"},
	}
	out := finalTurnTools(ModeCoding, full)
	names := map[string]bool{}
	for _, t := range out {
		names[t.Name] = true
	}
	if !names["edit"] {
		t.Errorf("edit must survive final-turn strip in coding mode")
	}
	if !names["write"] {
		t.Errorf("write must survive final-turn strip in coding mode")
	}
	if names["view"] || names["kai_grep"] || names["bash"] {
		t.Errorf("research tools must be stripped in coding final-turn, got: %v", names)
	}
}

func TestFinalTurnTools_NonCodingStripsAll(t *testing.T) {
	// Existing behavior for planner/review/debug: strip everything.
	// These modes don't deliver via tool calls — they deliver via the
	// text response — so removing all tools forces the model to commit
	// to a textual answer. (ModeConversation is NOT in this set: it
	// merged into coding as of 2026-05-29, so it keeps the editing
	// tools on the final turn like any coding run.)
	full := []tools.ToolInfo{
		{Name: "view"}, {Name: "edit"}, {Name: "write"}, {Name: "kai_grep"},
	}
	for _, mode := range []Mode{ModePlanning, ModeReview, ModeDebug} {
		if got := finalTurnTools(mode, full); got != nil {
			t.Errorf("mode %q must strip all tools on final turn, got: %v", mode, got)
		}
	}

	// ModeConversation now resolves to coding → keeps editing tools.
	if got := finalTurnTools(ModeConversation, full); got == nil {
		t.Errorf("ModeConversation merged into coding; must keep editing tools on final turn, got nil")
	}
}

func TestIsEditingTool(t *testing.T) {
	for _, name := range []string{"edit", "write"} {
		if !isEditingTool(name) {
			t.Errorf("%q must be classified as editing", name)
		}
	}
	for _, name := range []string{"view", "kai_grep", "bash", "kai_symbols", ""} {
		if isEditingTool(name) {
			t.Errorf("%q must NOT be classified as editing", name)
		}
	}
}
