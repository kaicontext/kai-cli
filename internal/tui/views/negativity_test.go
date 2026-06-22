package views

import (
	"reflect"
	"sort"
	"testing"
)

func TestMatchNegativity(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// Single tokens.
		{"wtf is happening", []string{"wtf"}},
		{"WTF IS HAPPENING", []string{"wtf"}},
		{"this is shit", []string{"shit"}},
		{"so shitty", []string{"shitty"}},
		// Multi-word phrases. The matcher consumes the longer span,
		// so "shit" inside "piece of shit" doesn't double-count —
		// FindAllString advances past the matched range. Telemetry-wise
		// that's actually the right signal: "piece of shit" is a
		// stronger frustration cue than the bare token.
		{"this is a piece of shit", []string{"piece of shit"}},
		{"fucking broken thing", []string{"fucking broken"}},
		{"what the hell", []string{"what the hell"}},
		{"so frustrating", []string{"so frustrating"}},
		// Word-boundary protection: must not false-match inside other words.
		{"the stuffs are fine", nil},        // contains "ffs" as substring
		{"the width here", nil},             // contains "wth"
		{"my dumbasserie", nil},             // contains "dumbass"
		{"horriblescreen.go", nil},          // contains "horrible" but not word-boundary
		// Dedupe: same phrase twice still counts once.
		{"wtf wtf wtf", []string{"wtf"}},
		// Empty / clean inputs.
		{"", nil},
		{"please refactor the auth module", nil},
		// Mixed-case with surrounding punctuation.
		{"OMFG!!! this sucks.", []string{"omfg", "this sucks"}},
	}

	for _, c := range cases {
		got := matchNegativity(c.in)
		sort.Strings(got)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("matchNegativity(%q):\n  got  %v\n  want %v", c.in, got, want)
		}
	}
}

func TestNegativityKey(t *testing.T) {
	if got, want := negativityKey("wtf"), "match_wtf"; got != want {
		t.Errorf("negativityKey(%q) = %q, want %q", "wtf", got, want)
	}
	if got, want := negativityKey("piece of shit"), "match_piece_of_shit"; got != want {
		t.Errorf("negativityKey: got %q, want %q", got, want)
	}
}
