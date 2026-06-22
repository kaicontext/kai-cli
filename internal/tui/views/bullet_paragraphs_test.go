package views

import "testing"

func TestBulletParagraphs_Prose(t *testing.T) {
	in := "First paragraph.\n\nSecond paragraph.\n\nThird paragraph."
	want := "• First paragraph.\n\n• Second paragraph.\n\n• Third paragraph."
	if got := bulletParagraphs(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBulletParagraphs_LeavesListsAlone(t *testing.T) {
	in := "Intro.\n\n- one\n- two\n- three\n\nOutro."
	want := "• Intro.\n\n- one\n- two\n- three\n\n• Outro."
	if got := bulletParagraphs(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBulletParagraphs_NumberedListsLeftAlone(t *testing.T) {
	in := "1. step one\n2. step two\n10. step ten"
	if got := bulletParagraphs(in); got != in {
		t.Errorf("numbered list mutated: got %q", got)
	}
}

func TestBulletParagraphs_HeadingsLeftAlone(t *testing.T) {
	in := "# Title\n\nbody\n\n## Subtitle\n\nmore body"
	want := "# Title\n\n• body\n\n## Subtitle\n\n• more body"
	if got := bulletParagraphs(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBulletParagraphs_CodeFencesLeftAlone(t *testing.T) {
	in := "Before code.\n\n```go\nfunc Foo() {}\nfunc Bar() {}\n```\n\nAfter code."
	want := "• Before code.\n\n```go\nfunc Foo() {}\nfunc Bar() {}\n```\n\n• After code."
	if got := bulletParagraphs(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBulletParagraphs_TildeFencesLeftAlone(t *testing.T) {
	in := "Pre.\n\n~~~\nfree-form code\n~~~\n\nPost."
	want := "• Pre.\n\n~~~\nfree-form code\n~~~\n\n• Post."
	if got := bulletParagraphs(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBulletParagraphs_BlockquotesLeftAlone(t *testing.T) {
	in := "Above.\n\n> a quoted line\n\nBelow."
	want := "• Above.\n\n> a quoted line\n\n• Below."
	if got := bulletParagraphs(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBulletParagraphs_EmptyInput(t *testing.T) {
	if got := bulletParagraphs(""); got != "" {
		t.Errorf("empty input mutated: %q", got)
	}
}

func TestBulletParagraphs_StarListLeftAlone(t *testing.T) {
	// "* " is a list marker; "*emphasis*" is not (no space after).
	// Both should be left alone since the second isn't a paragraph
	// line that starts a fresh paragraph (it's inline emphasis).
	in := "* item one\n* item two"
	if got := bulletParagraphs(in); got != in {
		t.Errorf("star list mutated: got %q", got)
	}
}

func TestBulletParagraphs_StartsWithEmphasisGetsBullet(t *testing.T) {
	// A paragraph that incidentally starts with `*foo*` (emphasis)
	// is still prose — should get a bullet.
	in := "*important* note here"
	want := "• *important* note here"
	if got := bulletParagraphs(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
