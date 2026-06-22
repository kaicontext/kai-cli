package agent

import (
	"strings"
	"testing"
)

// TestCodingSystemPrompt_ReuseExistingPatterns pins the
// "REUSE EXISTING PATTERNS" section in the coding-mode system
// prompt. The 2026-05-14 dogfood produced a worker run where a
// trivial "add a hex assertion" task got 0 net changes because
// the worker introduced testify into a file that used plain
// t.Errorf — the test file ended in a half-rewritten, broken
// state and the edit was reverted. The section tells the worker
// to check existing patterns first; this test makes sure the
// section can't be silently dropped in a future prompt refactor.
//
// We don't pin exact wording (the section may evolve), just the
// load-bearing anchors that prove the guidance survived.
func TestCodingSystemPrompt_ReuseExistingPatterns(t *testing.T) {
	anchors := map[string]string{
		"section header":  "REUSE EXISTING PATTERNS",
		"testify example": "testify",
		"plain t.Errorf":  "t.Errorf",
		"error wrapping":  "error-wrapping",
		"is a refactor":   "is a refactor",
	}
	for name, needle := range anchors {
		if !strings.Contains(codingSystemPrompt, needle) {
			t.Errorf("codingSystemPrompt missing %s (expected substring %q)", name, needle)
		}
	}
}
