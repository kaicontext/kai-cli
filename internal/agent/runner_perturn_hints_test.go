package agent

import (
	"strings"
	"testing"

	"kai/internal/agent/message"
)

// withPerTurnHints appends per-turn ephemera (graph block,
// convergence nudge) as a brand-new user-role message at the tail
// of a copy of history. The previous implementation augmented the
// last user message in-place (via shallow copy), but that broke
// prompt caching: the augmented message's bytes differed between
// the turn that emitted the hint and the next turn that replayed
// it without one, invalidating Anthropic's cache prefix at exactly
// that index. See runner.go's withPerTurnHints comment for the
// run-log evidence.
//
// The runner pairs this with `provider.Request.EphemeralTailMessages`
// so the provider's cache_control breakpoint is placed BEFORE the
// hint message — the canonical history prefix stays cacheable, the
// hint is fresh-write each turn but doesn't break anything.

func TestWithPerTurnHints_AppendsAsNewMessage(t *testing.T) {
	history := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "first prompt"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "ok"}}},
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "follow-up"}}},
	}
	out := withPerTurnHints(history, "GRAPH BLOCK + CONVERGENCE")

	if len(out) != len(history)+1 {
		t.Fatalf("expected len+1 messages (got %d, want %d)", len(out), len(history)+1)
	}
	// Original messages must be byte-identical — that's the whole
	// point of the new shape: the cache prefix stays stable.
	for i := 0; i < len(history); i++ {
		if len(out[i].Parts) != len(history[i].Parts) {
			t.Errorf("message %d Parts changed (got %d, want %d)",
				i, len(out[i].Parts), len(history[i].Parts))
		}
	}
	tail := out[len(out)-1]
	if tail.Role != message.RoleUser {
		t.Errorf("hint message role: got %q want user", tail.Role)
	}
	if len(tail.Parts) != 1 {
		t.Fatalf("hint message Parts: got %d want 1", len(tail.Parts))
	}
	tc, ok := tail.Parts[0].(message.TextContent)
	if !ok {
		t.Fatalf("hint part wrong type: %T", tail.Parts[0])
	}
	if !strings.Contains(tc.Text, "GRAPH BLOCK + CONVERGENCE") {
		t.Errorf("hint text missing: %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "[runner:") {
		t.Errorf("expected [runner: ...] sentinel framing, got: %q", tc.Text)
	}
}

func TestWithPerTurnHints_EmptyHintsIsNoOp(t *testing.T) {
	history := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "x"}}},
	}
	out := withPerTurnHints(history, "")
	if len(out) != 1 || out[0].Role != message.RoleUser {
		t.Errorf("expected unchanged history, got %+v", out)
	}
}

func TestWithPerTurnHints_EmptyHistoryIsNoOp(t *testing.T) {
	out := withPerTurnHints(nil, "hints")
	if len(out) != 0 {
		t.Errorf("expected empty out for empty history, got %+v", out)
	}
}

// TestWithPerTurnHints_DoesNotMutateInputHistory pins that we never
// touch the caller's slice — the runner reuses `history` across the
// turn loop and a mutation would corrupt the persisted session and
// re-inject hints retroactively on resume.
func TestWithPerTurnHints_DoesNotMutateInputHistory(t *testing.T) {
	history := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "a"}}},
	}
	originalLen := len(history)
	originalParts := len(history[0].Parts)
	_ = withPerTurnHints(history, "anything")
	if len(history) != originalLen {
		t.Errorf("history length mutated: %d -> %d", originalLen, len(history))
	}
	if len(history[0].Parts) != originalParts {
		t.Errorf("history[0].Parts length mutated: %d -> %d", originalParts, len(history[0].Parts))
	}
}
