package views

import (
	"strings"
	"testing"
)

// TestReplHistory_RoundTripPersistsAcrossSessions is the canary for
// "I press up after a new session and recall the previous session's
// last prompt." Two appends, simulating session 1; load again,
// simulating session 2; up-arrow equivalent (last entry) should be
// the most recent session-1 prompt.
func TestReplHistory_RoundTripPersistsAcrossSessions(t *testing.T) {
	dir := t.TempDir()

	// Session 1: two submissions.
	appendReplHistory(dir, "fix the login bug")
	appendReplHistory(dir, "add a function called add(a,b)")

	// Session 2: load. The last entry is what up-arrow recalls first.
	got := loadReplHistory(dir)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries after persist, got %d: %v", len(got), got)
	}
	if got[1] != "add a function called add(a,b)" {
		t.Errorf("most recent prompt mismatch: got %q", got[1])
	}
	if got[0] != "fix the login bug" {
		t.Errorf("first prompt mismatch: got %q", got[0])
	}
}

// TestReplHistory_MultilineRoundTrip: alt+enter in the textarea
// produces multi-line prompts. Storage must escape and unescape
// newlines so the file stays line-delimited.
func TestReplHistory_MultilineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prompt := "explain this:\nline two\nline three"
	appendReplHistory(dir, prompt)

	got := loadReplHistory(dir)
	if len(got) != 1 || got[0] != prompt {
		t.Errorf("multiline round-trip failed: got %q", got)
	}
}

// TestReplHistory_BackslashEscape: literal backslashes must round-trip
// cleanly. Otherwise paths like `C:\Users\foo` decode wrong.
func TestReplHistory_BackslashEscape(t *testing.T) {
	dir := t.TempDir()
	prompt := `windows path: C:\Users\foo\bar`
	appendReplHistory(dir, prompt)

	got := loadReplHistory(dir)
	if len(got) != 1 || got[0] != prompt {
		t.Errorf("backslash round-trip failed: got %q", got)
	}
}

// TestReplHistory_CapEnforced: the file caps at replHistoryMax. This
// test uses a smaller synthetic load to keep runtime cheap; real cap
// is exercised by replHistoryMax behavior in the helpers.
func TestReplHistory_CapEnforced(t *testing.T) {
	dir := t.TempDir()
	// Write replHistoryMax + 5 entries; expect only the last
	// replHistoryMax to survive after the trim-rewrite kicks in.
	for i := 0; i < replHistoryMax+5; i++ {
		appendReplHistory(dir, "prompt-"+pad(i))
	}
	got := loadReplHistory(dir)
	if len(got) != replHistoryMax {
		t.Fatalf("cap not enforced: got %d entries", len(got))
	}
	// First retained entry should be #5 (entries 0..4 dropped).
	if !strings.HasSuffix(got[0], "-"+pad(5)) {
		t.Errorf("oldest retained entry should be #5, got %q", got[0])
	}
	// Last entry should be the very last write.
	if !strings.HasSuffix(got[len(got)-1], "-"+pad(replHistoryMax+4)) {
		t.Errorf("most recent entry mismatch: got %q", got[len(got)-1])
	}
}

// TestReplHistory_MissingFileReturnsNil: a fresh project has no file.
// Load should silently return nil rather than erroring; the REPL
// gracefully starts with empty history.
func TestReplHistory_MissingFileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	got := loadReplHistory(dir)
	if got != nil {
		t.Errorf("expected nil for missing file, got %v", got)
	}
}

func pad(i int) string {
	s := ""
	if i < 10 {
		s = "000"
	} else if i < 100 {
		s = "00"
	} else if i < 1000 {
		s = "0"
	}
	return s + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
