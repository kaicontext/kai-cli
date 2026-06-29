package views

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// TestVisualInputRows_WrapParity guards the "text disappears on
// overflow" fix. taWrap must agree, row-for-row, with the textarea's
// own word-wrap; otherwise the input box is sized off the wrong count
// and wrapped overflow scrolls out of the viewport. We compare against
// textarea.LineInfo().Height, which for a single logical line is the
// number of soft-wrapped rows the textarea actually renders.
func TestVisualInputRows_WrapParity(t *testing.T) {
	ta := textarea.New()
	ta.SetWidth(40)
	ta.SetHeight(1)
	ta.Focus()

	cases := []string{
		"short",
		"this is a line that is long enough to wrap onto a second visual row for sure",
		strings.Repeat("word ", 40),
		"supercalifragilisticexpialidocioussupercalifragilisticexpialidocious",
	}

	for _, in := range cases {
		ta.SetValue(in)
		// Move cursor to the end so LineInfo reflects the last (and
		// only) logical line in full.
		ta.CursorEnd()
		want := ta.LineInfo().Height
		got := len(taWrap([]rune(in), ta.Width()))
		if got != want {
			t.Errorf("taWrap row count = %d, textarea reports %d for %q (width %d)",
				got, want, in, ta.Width())
		}
	}
}

// TestVisualInputRows_GrowsOnSoftWrap is the direct regression for the
// reported bug: typing past the box width must report more than one row
// so the box grows instead of hiding the overflow.
func TestVisualInputRows_GrowsOnSoftWrap(t *testing.T) {
	r := &REPL{input: textarea.New()}
	r.input.SetWidth(20)
	r.input.SetHeight(1)

	r.input.SetValue("x")
	if rows := r.visualInputRows(); rows != 1 {
		t.Fatalf("short input: visualInputRows = %d, want 1", rows)
	}

	r.input.SetValue("this text is definitely wider than twenty columns")
	if rows := r.visualInputRows(); rows < 2 {
		t.Fatalf("overflowing input: visualInputRows = %d, want >= 2", rows)
	}
}

// TestReplUpdate_OverflowKeepsTopRowVisible is the end-to-end
// regression for the paste/overflow report: when typed text wraps, the
// box must grow AND show the beginning of the text, not scroll the
// first wrapped rows out of a viewport sized while it was still one row
// tall. We drive REPL.Update with the offending paste and assert the
// rendered textarea still contains the start of the line.
func TestReplUpdate_OverflowKeepsTopRowVisible(t *testing.T) {
	r := REPL{input: textarea.New()}
	r.input.Focus()
	r.SetSize(60, 24)

	const pasted = "right now in @kai-tui when the text overflows on the to " +
		"the next line it should immediately make the text input larger. " +
		"Instead it looks like the text dissapears."

	// A bracketed-paste / multi-rune key, the way a terminal delivers a
	// paste to bubbletea.
	r2, _ := r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted)})

	if got := r2.visualInputRows(); got < 2 {
		t.Fatalf("expected the box to grow past 1 row, got %d", got)
	}
	view := r2.input.View()
	if !strings.Contains(view, "right now in") {
		t.Fatalf("first wrapped row scrolled out of view; textarea View did not contain the start of the text:\n%s", view)
	}
	if !strings.Contains(view, "dissapears.") {
		t.Fatalf("last row (with cursor) not visible:\n%s", view)
	}
}
