package main

import (
	"strings"
	"testing"

	"kai/internal/util"
)

// TestHeldByGateHint pins the F-17 fix: when the safety gate holds an
// integration, the hint must point users at the command that actually unblocks a
// held change (`kai gate approve <id>`) and must NOT point at `kai review`, which
// errors with "cannot transition from draft to approved". The id must be the
// held snapshot's short id so the suggested command is runnable verbatim.
func TestHeldByGateHint(t *testing.T) {
	snap := make([]byte, 32)
	for i := range snap {
		snap[i] = byte(i)
	}
	hint := heldByGateHint(snap)

	if !strings.Contains(hint, "kai gate approve") {
		t.Errorf("hint must name `kai gate approve`, got: %q", hint)
	}
	if strings.Contains(hint, "kai review") {
		t.Errorf("hint still points at `kai review` (the wrong command — F-17): %q", hint)
	}
	shortID := util.BytesToHex(snap)[:12]
	if !strings.Contains(hint, shortID) {
		t.Errorf("hint must embed the held snapshot's short id %q so the command is runnable, got: %q", shortID, hint)
	}
}
