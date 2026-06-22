package views

import (
	"strings"
	"testing"
)

// TestChatActivity_ToolEventQuietByDefault pins the v0.31.24
// behavior: tool events do NOT write to scrollback by default —
// the screen-busy complaint from the 2026-05-24 dogfood was that
// 15-30 "→ tool" lines per exploration phase flood the screen.
// Quiet mode counts the events instead and surfaces them via the
// spinner thinking line + an end-of-run footer.
func TestChatActivity_ToolEventQuietByDefault(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", nil)
	r.SetSize(80, 20)
	pre := len(r.pendingPrints)
	if r.verboseTools {
		t.Fatalf("expected verboseTools=false by default")
	}

	r2, _ := r.Update(ChatActivityMsg{Event: ChatActivityEvent{
		Kind:    "tool",
		Summary: "bash: ls -la",
	}})

	if len(r2.pendingPrints) > pre {
		// We allow ONE pendingPrint from the spinner re-render; what
		// we forbid is the tool-line landing in scrollback. The
		// spinner does its own appending in renderSpinner, so check
		// that no scrollback line contains the tool args.
		added := strings.Join(r2.pendingPrints[pre:], "\n")
		if strings.Contains(added, "ls -la") {
			t.Errorf("quiet mode should NOT write tool args to scrollback; got: %q", added)
		}
	}
	if r2.suppressedToolEvents != 1 {
		t.Errorf("expected suppressedToolEvents=1, got %d", r2.suppressedToolEvents)
	}
	if r2.lastToolSummary != "bash: ls -la" {
		t.Errorf("expected lastToolSummary captured for spinner; got %q", r2.lastToolSummary)
	}
}

// TestChatActivity_ToolEventVerboseRendersToScrollback pins the
// opposite path: when the user has enabled verbose mode via
// /verbose, tool events DO land in scrollback like they did
// pre-v0.31.24.
func TestChatActivity_ToolEventVerboseRendersToScrollback(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", nil)
	r.SetSize(80, 20)
	r.verboseTools = true
	pre := len(r.pendingPrints)

	r2, _ := r.Update(ChatActivityMsg{Event: ChatActivityEvent{
		Kind:    "tool",
		Summary: "bash: ls -la",
	}})

	if len(r2.pendingPrints) <= pre {
		t.Fatalf("verbose tool event produced no scrollback line; pendingPrints unchanged (len=%d)", len(r2.pendingPrints))
	}
	added := strings.Join(r2.pendingPrints[pre:], "\n")
	for _, want := range []string{"→", "bash", "ls -la"} {
		if !strings.Contains(added, want) {
			t.Errorf("expected %q in verbose tool line, got: %q", want, added)
		}
	}
}

// TestFormatToolEvent_NoColonFallsBackToBareTool covers the
// degenerate "Summary is just a tool name" path. We shouldn't
// crash or render an empty line; the arrow + name is enough.
func TestFormatToolEvent_NoColonFallsBackToBareTool(t *testing.T) {
	out := formatToolEvent(ChatActivityEvent{Kind: "tool", Summary: "compile"}, 80)
	if !strings.Contains(out, "→") || !strings.Contains(out, "compile") {
		t.Errorf("expected '→ compile' line, got %q", out)
	}
}

// TestFormatToolEvent_EmptySummary should still produce something
// non-empty rather than a bare arrow with no label.
func TestFormatToolEvent_EmptySummary(t *testing.T) {
	out := formatToolEvent(ChatActivityEvent{Kind: "tool"}, 80)
	if strings.TrimSpace(out) == "" {
		t.Error("formatToolEvent must not return empty for empty summary")
	}
	if !strings.Contains(out, "tool") {
		t.Errorf("expected fallback 'tool' label, got %q", out)
	}
}

// TestFormatToolEvent_StripsLeadingArrow pins the defensive strip
// for producers that already prefix Summary with "→ " (the
// orchestrator bridge in app.go is the canonical case). Without the
// strip we double-prepend and the feed renders "→ → spawn-name: ...".
func TestFormatToolEvent_StripsLeadingArrow(t *testing.T) {
	out := formatToolEvent(ChatActivityEvent{
		Kind:    "tool",
		Summary: "→ bracket-tool-name: bash cd kai-cli && go test",
	}, 80)
	if strings.HasPrefix(out, "→ → ") {
		t.Errorf("double arrow not stripped: %q", out)
	}
	if !strings.HasPrefix(out, "→ bracket-tool-name") {
		t.Errorf("expected single-arrow head followed by tool name, got %q", out)
	}
}
