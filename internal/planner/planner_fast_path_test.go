package planner

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/safetygate"
)

// TestPlannerPrompt_FastPathSectionPresent pins the
// "FAST PATH FOR TRIVIALLY SCOPED WORK" guidance in the planner
// system prompt so it can't be silently dropped in a future prompt
// refactor. Motivation: 2026-05-15 dogfood — a "write a design
// doc" task was split into 3 parallel agents and exploded into a
// 4m execution with SQLITE_BUSY failures because the planner
// treated documentation work the same as a multi-module refactor.
// The fast-path section tells the planner that single-file
// deliverables get ONE agent with minimal exploration.
//
// Anchors only — exact wording may evolve. The test asserts that
// the core directive (single-file → one agent, 0-2 exploration
// turns, no multi-agent for docs/single-file edits) survives.
func TestPlannerPrompt_FastPathSectionPresent(t *testing.T) {
	prompt := buildPlannerPrompt(
		"write a design doc",
		safetygate.Config{},
		Config{MaxAgents: 5},
		nil,
	)
	anchors := map[string]string{
		"section header":    "FAST PATH FOR TRIVIALLY SCOPED WORK",
		"single-file":       "single-file",
		"one-agent rule":    "ONE agent",
		"exploration cap":   "0-2 turns",
		"no multi-agent for docs": "documentation, single-file edits",
		"sequential anti-pattern": "design → tests → implementation",
	}
	for name, needle := range anchors {
		if !strings.Contains(prompt, needle) {
			t.Errorf("planner prompt missing %s (expected substring %q)", name, needle)
		}
	}
}
