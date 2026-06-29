package views

import (
	"strings"
	"testing"
)

func TestPasteStoreShouldStore(t *testing.T) {
	ps := newPasteStore()
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"short single line", "a quick note", false},
		{"many short lines under cap", strings.Repeat("line\n", 20), false},
		{"just under cap", strings.Repeat("x", pasteCharThreshold-1), false},
		{"exactly cap", strings.Repeat("x", pasteCharThreshold), true},
		{"long single line", strings.Repeat("x", pasteCharThreshold+500), true},
		{"multibyte under cap", strings.Repeat("é", pasteCharThreshold-1), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ps.shouldStore(tc.content); got != tc.want {
				t.Fatalf("shouldStore(%q-ish) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestPasteStoreRoundTrip(t *testing.T) {
	ps := newPasteStore()
	content := "func main() {\n\tprintln(\"hi\")\n}\n"
	e := ps.store(content)

	ph := ps.placeholder(e)
	if !pastePlaceholderRe.MatchString(ph) {
		t.Fatalf("placeholder %q does not match the expected format", ph)
	}
	if !strings.Contains(ph, "#1") {
		t.Fatalf("first placeholder should carry id #1, got %q", ph)
	}

	// A draft mixing prose and the placeholder expands back to the
	// original content in place, leaving the surrounding prose intact.
	draft := "please review:\n" + ph + "\nthanks"
	got := ps.expand(draft)
	want := "please review:\n" + content + "\nthanks"
	if got != want {
		t.Fatalf("expand mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestPasteStoreMultiplePlaceholders(t *testing.T) {
	ps := newPasteStore()
	a := ps.store("AAA\nAAA")
	b := ps.store("BBB\nBBB")
	if a.id == b.id {
		t.Fatalf("ids should be distinct, both = %d", a.id)
	}
	draft := ps.placeholder(b) + " vs " + ps.placeholder(a)
	got := ps.expand(draft)
	if want := "BBB\nBBB vs AAA\nAAA"; got != want {
		t.Fatalf("expand mismatch: got %q want %q", got, want)
	}
}

func TestPasteStoreExpandLeavesUnknownAndPlainText(t *testing.T) {
	ps := newPasteStore()
	ps.store("real content here\nx")

	// No placeholder at all → unchanged (and the fast path holds).
	plain := "just a normal prompt with #1 and [brackets]"
	if got := ps.expand(plain); got != plain {
		t.Fatalf("plain text changed: %q", got)
	}

	// Unknown id (never stored in this process) is left verbatim
	// rather than silently dropped.
	unknown := "see [Pasted text #999 +12 lines] above"
	if got := ps.expand(unknown); got != unknown {
		t.Fatalf("unknown placeholder should be preserved, got %q", got)
	}
}

func TestPasteStoreNilSafe(t *testing.T) {
	var ps pasteStore // zero value, nil map
	if got := ps.expand("anything [Pasted text #1 +2 lines]"); got != "anything [Pasted text #1 +2 lines]" {
		t.Fatalf("nil-map expand should pass through, got %q", got)
	}
	// store() lazily initializes the map.
	e := ps.store("a\nb\nc")
	if e.lines != 3 {
		t.Fatalf("lines = %d, want 3", e.lines)
	}
}

func TestCountLines(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"one":         1,
		"one\n":       1,
		"one\ntwo":    2,
		"one\ntwo\n":  2,
		"a\nb\nc\n\n": 3,
	}
	for in, want := range cases {
		if got := countLines(in); got != want {
			t.Fatalf("countLines(%q) = %d, want %d", in, got, want)
		}
	}
}
