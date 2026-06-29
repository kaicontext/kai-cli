package views

import (
	"strings"
	"testing"
)

// TestChatSystemPrompt_LoadBearing checks that the trimmed prompt
// still carries the load-bearing rules. Anchors are deliberately
// short so future edits to wording don't break the test.
func TestChatSystemPrompt_LoadBearing(t *testing.T) {
	required := map[string]string{
		"url policy":          "Never generate or guess URLs",
		"security policy":     "authorized testing",
		"refuse-destructive":  "Refuse destructive",
		"discovery rule":      "Discovery rule",
		"parallel tool calls": "Parallel tool calls",
		"not read-only":       "NOT read-only",
		"voice section":       "Voice:",
		"style section":       "Style:",
		"no markdown headers": "markdown headers",
		"summarize-not-paste": "Summarize",
		"kai_search primary":  "PRIMARY",
		"kai_grep fallback":   "FALLBACK",
		"bash restriction":    "Never bash",
	}
	for name, needle := range required {
		if !strings.Contains(chatSystemPrompt, needle) {
			t.Errorf("chatSystemPrompt missing %s (expected substring %q)", name, needle)
		}
	}
}

// TestWorkspaceGroundingGuidance_AuditLenses guards the audit/review
// investigation lenses that fix the surface the original comparison ran
// on — they live in the workspace-grounding guidance (audits are
// codebase questions), alongside the OBSERVED discipline.
func TestWorkspaceGroundingGuidance_AuditLenses(t *testing.T) {
	required := map[string]string{
		"operand-read":  "READ THE OPERANDS",
		"forward-trace": "TRACE A MUTATION FORWARD",
		"pricing tool":  "kit analyze pricing",
	}
	for name, needle := range required {
		if !strings.Contains(workspaceGroundingGuidance, needle) {
			t.Errorf("workspaceGroundingGuidance missing %s (expected substring %q)", name, needle)
		}
	}
}

// TestChatSystemPrompt_VoiceAnchors keeps the Voice section honest
// without pinning specific example phrases.
func TestChatSystemPrompt_VoiceAnchors(t *testing.T) {
	anchors := []string{"pair-programming", "Before a tool call", "After a result"}
	for _, a := range anchors {
		if !strings.Contains(chatSystemPrompt, a) {
			t.Errorf("Voice section missing anchor %q", a)
		}
	}
}

// TestChatOverviewGuidance_Dynamic confirms the overview guidance is
// kept in its own constant (so the dispatcher can append it only on
// the first turn) and carries the budget rule.
func TestChatOverviewGuidance_Dynamic(t *testing.T) {
	if !strings.Contains(chatOverviewGuidance, "Exploration budget") {
		t.Errorf("chatOverviewGuidance missing 'Exploration budget' rule")
	}
	if !strings.Contains(chatOverviewGuidance, "Project overview") {
		t.Errorf("chatOverviewGuidance missing 'Project overview' framing")
	}
	// The overview-specific paragraphs MUST NOT live in the static
	// prompt — they're the dead-weight-after-turn-1 we trimmed.
	if strings.Contains(chatSystemPrompt, "Exploration budget") {
		t.Errorf("chatSystemPrompt still carries 'Exploration budget' — should live only in chatOverviewGuidance")
	}
}
