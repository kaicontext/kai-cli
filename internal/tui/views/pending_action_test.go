package views

import (
	"strings"
	"testing"
)

// TestExtractPendingAction_TrailerOnly checks the canonical case:
// a chat reply that ends with the exact trailer string. The
// proposal text up to the trailer is returned; the trailer itself
// is excluded.
func TestExtractPendingAction_TrailerOnly(t *testing.T) {
	reply := "Add a `repoName` prop to TitleBar and pass it from App.svelte.\n\nReply 'yes' and I'll apply it."
	pa := extractPendingAction(reply)
	if pa == nil {
		t.Fatal("expected pendingAction, got nil")
	}
	if strings.Contains(pa.text, trailerLine) {
		t.Errorf("trailer should be stripped from action text, got: %q", pa.text)
	}
	if !strings.Contains(pa.text, "repoName") {
		t.Errorf("action text should contain proposal body, got: %q", pa.text)
	}
}

// TestExtractPendingAction_NoTrailer returns nil for replies that
// don't carry the trailer — these are pure questions, clarifications,
// or explanations with no offered action.
func TestExtractPendingAction_NoTrailer(t *testing.T) {
	for _, reply := range []string{
		"",
		"Just a clarifying question — which file did you mean?",
		"Here's what kai_ws_list returns: ...",
	} {
		if pa := extractPendingAction(reply); pa != nil {
			t.Errorf("expected nil for %q, got %+v", reply, pa)
		}
	}
}

// TestExtractPendingAction_EmptyBody — a reply that's just the
// trailer with nothing before it is degenerate; we return nil so
// the wrap doesn't get sent with empty content.
func TestExtractPendingAction_EmptyBody(t *testing.T) {
	if pa := extractPendingAction(trailerLine); pa != nil {
		t.Errorf("expected nil for trailer-only reply, got %+v", pa)
	}
}

// TestExtractPendingAction_OfferPatternFallback verifies the
// broader pattern set catches offers when the model deviates from
// the exact trailer. The 2026-05-26 dogfood failure: chat agent
// emitted "Want me to switch to a mode that can apply these?" and
// the strict-string detector missed it, so "yes" routed to
// conversation mode (no edit tools) with "I can't edit in this
// mode" as the result.
func TestExtractPendingAction_OfferPatternFallback(t *testing.T) {
	cases := []string{
		"Two fixes needed in cli.js. Want me to switch to a mode that can apply these?",
		"I can refactor this. Shall I apply the change?",
		"Looks like a one-line fix in App.svelte. Should I proceed?",
		"There are 3 places to update. Want me to make the edits?",
	}
	for _, reply := range cases {
		pa := extractPendingAction(reply)
		if pa == nil {
			t.Errorf("expected pendingAction for offer-phrasing reply: %q", reply)
			continue
		}
		if pa.text == "" {
			t.Errorf("action text empty for: %q", reply)
		}
	}
}

// TestExtractPendingAction_OfferPatternMisses confirms that prose
// containing offer-phrase substrings inside other contexts
// doesn't false-positive. (Currently the heuristic IS permissive
// — if these turn out to fire, tighten or carve out here.)
func TestExtractPendingAction_OfferPatternMisses(t *testing.T) {
	// These should NOT trigger — none contain any offer phrase.
	for _, reply := range []string{
		"Here's what's happening: the planner ran for 2 minutes.",
		"The bug is in line 42 of cli.js, in the catch block.",
		"You can see the diff with `git diff HEAD~1`.",
	} {
		if pa := extractPendingAction(reply); pa != nil {
			t.Errorf("unexpected pendingAction for %q: %+v", reply, pa)
		}
	}
}

// TestWrapPendingActionPrompt covers the structural shape of the
// preamble: the proposal is fenced, the confirmation token is
// echoed back, and the instruction to execute is present.
func TestWrapPendingActionPrompt(t *testing.T) {
	pa := &pendingAction{text: "edit file X to add Y"}
	out := wrapPendingActionPrompt(pa, "yes")
	for _, want := range []string{
		"pending action",
		"edit file X to add Y",
		"user confirmed",
		"yes",
		"Execute exactly what you proposed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wrapped prompt missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestIsShortNegative pins the affirmative/negative split — false
// matches here would silently consume pending actions instead of
// cancelling them.
func TestIsShortNegative(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"no", true},
		{"NO!", true},
		{"n", true},
		{"cancel", true},
		{"stop", true},
		{"nevermind", true},
		{"never mind", true},
		{"yes", false},
		{"go", false},
		{"", false},
		{"please don't apply that, do something else instead", false}, // >20 chars
	}
	for _, c := range cases {
		if got := isShortNegative(c.in); got != c.want {
			t.Errorf("isShortNegative(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
