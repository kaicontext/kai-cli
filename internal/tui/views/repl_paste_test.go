package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestReplUpdate_LargePasteDiverted is the end-to-end check for the
// large-paste feature: a bracketed paste of many lines must NOT dump
// the raw text into the input. Instead the input shows a compact
// placeholder, and the full content is retrievable (and re-expandable)
// from the store so dispatch can recover it.
func TestReplUpdate_LargePasteDiverted(t *testing.T) {
	r := NewREPL("kai", t.TempDir(), nil)
	r.SetSize(80, 24)

	big := strings.Repeat("a line of pasted content\n", 40)

	// Bracketed paste: bubbletea sets Paste and packs the whole chunk
	// into Runes.
	r2, _ := r.Update(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(big)})

	val := r2.InputValue()
	if strings.Contains(val, "a line of pasted content") {
		t.Fatalf("raw paste leaked into the input; want a placeholder, got:\n%q", val)
	}
	if !pastePlaceholderRe.MatchString(val) {
		t.Fatalf("input does not contain a paste placeholder, got %q", val)
	}
	if !strings.Contains(val, "+40 lines") {
		t.Fatalf("placeholder should report 40 lines, got %q", val)
	}

	// The placeholder re-expands to exactly what was pasted.
	if got := r2.pastes.expand(val); got != big {
		t.Fatalf("expand did not recover the original paste:\n got: %q\nwant: %q", got, big)
	}
}

// TestReplUpdate_SmallPasteInline confirms the diversion only fires for
// large pastes — a short paste stays inline so the user sees exactly
// what they pasted.
func TestReplUpdate_SmallPasteInline(t *testing.T) {
	r := NewREPL("kai", t.TempDir(), nil)
	r.SetSize(80, 24)

	const small = "kai"
	r2, _ := r.Update(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(small)})

	if got := r2.InputValue(); got != small {
		t.Fatalf("small paste should land inline verbatim, got %q", got)
	}
	if pastePlaceholderRe.MatchString(r2.InputValue()) {
		t.Fatalf("small paste should not produce a placeholder")
	}
}

// TestReplUpdate_PasteAroundTypedText checks that a placeholder coexists
// with surrounding typed prose and that submitLine's expansion path
// rebuilds the full prompt while the echoed/history line stays compact.
func TestReplUpdate_PasteAroundTypedText(t *testing.T) {
	r := NewREPL("kai", t.TempDir(), nil)
	r.SetSize(80, 24)

	// Type a lead-in, paste a big block, then type a trailer.
	r, _ = r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("review this: ")})
	big := strings.Repeat("a line of code goes here\n", 50) // > 1000 chars
	r, _ = r.Update(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(big)})
	r, _ = r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" thanks")})

	val := r.InputValue()
	if !strings.HasPrefix(val, "review this: ") || !strings.HasSuffix(val, " thanks") {
		t.Fatalf("typed prose was lost around the placeholder: %q", val)
	}
	if strings.Contains(val, "a line of code goes here") {
		t.Fatalf("raw paste leaked into the input: %q", val)
	}

	expanded := r.pastes.expand(val)
	if !strings.Contains(expanded, big) {
		t.Fatalf("expanded prompt is missing the pasted block:\n%q", expanded)
	}
	if !strings.HasPrefix(expanded, "review this: ") || !strings.HasSuffix(expanded, " thanks") {
		t.Fatalf("expanded prompt lost the surrounding prose:\n%q", expanded)
	}
}
