package agent

import (
	"regexp"
	"strings"

	"kai/internal/agent/message"
)

// The dangle guard prevents a coding-mode agent from "explaining the
// change" instead of making it. Real failure mode (2026-05-12): user
// asks "make the `?` key toggleable", the worker spends ~30 turns on
// view / kai_grep / kai_symbols, then ends with "Based on my
// investigation, here are the changes needed in repl.go:" and a prose
// description — zero write/edit calls. The orchestrator reports "no
// changes" and the user has to nudge it to actually do the work.
//
// The guard intercepts the agent's final answer when:
//   - the run made zero write/edit tool calls, AND
//   - the final text reads as a change-description ("you should
//     modify", "here are the changes needed", etc.), AND
//   - it's NOT an explicit block ("I'm blocked because X").
//
// Fire-at-most-once per run, same shape as absence/hallucination
// guards. Coding mode only — debug mode already has the equivalent
// rule baked into its system prompt; planning/review/conversation are
// intentionally non-mutating.

// dangleGuardNudge is the user-role message injected into the
// transcript when the guard fires. Phrased as coaching so the model
// revises its behavior rather than arguing the description was the
// deliverable.
const dangleGuardNudge = `You described changes that should be made but never called write or edit — the run produced no actual file modifications. Describing the change isn't the deliverable; making the change is.

If you have a clear edit to make, call edit (or write) now with the concrete change. If you genuinely need information from the user before proceeding, say so explicitly with the shape "I'm blocked because <X> — I need <Y> from you" and ask the specific question. Do not end this turn with another description.`

// changeDescriptionPhrases matches phrasings a model uses when it's
// describing a change instead of making it. Designed to over-trigger
// on the final answer: false positives cost one extra turn, false
// negatives let the dangling-turn bug slip through.
var changeDescriptionPhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bchanges?\s+(?:are\s+)?needed\b`),
	regexp.MustCompile(`(?i)\bedits?\s+(?:are\s+)?needed\b`),
	// Allow an optional adjective between "the" and the change-noun:
	// "here are the EXACT edits needed", "here is the PROPOSED change",
	// "here are the SPECIFIC changes". Without this, a single adjective
	// slips past the guard (2026-05-13 round-14 dogfood).
	regexp.MustCompile(`(?i)\bhere(?:'s| is| are)\s+(?:the\s+(?:\w+\s+){0,2})?(?:changes?|edits?|modifications?|fix(?:es)?|patch(?:es)?|update(?:s)?)\b`),
	regexp.MustCompile(`(?i)\b(?:you|we|i)\s+(?:should|need to|would|could|can|must)\s+(?:change|modify|update|edit|add|remove|delete|replace|rename|refactor)\b`),
	regexp.MustCompile(`(?i)\bto\s+(?:implement|fix|address|resolve)\s+this[^.]*\b(?:change|modify|update|edit|add|replace)\b`),
	regexp.MustCompile(`(?i)\bthe\s+fix\s+is\s+to\b`),
	regexp.MustCompile(`(?i)\b(?:proposed|suggested|recommended)\s+(?:change|edit|fix|patch|diff)\b`),
	regexp.MustCompile(`(?i)\bwould\s+(?:look\s+like|involve|require)\b.*\b(?:change|edit|add|modify)\b`),
	regexp.MustCompile(`(?i)\bbased\s+on\s+(?:my|the)\s+investigation\b`),
	regexp.MustCompile(`(?i)\bhere(?:'s| is)\s+(?:what|how)\s+(?:to|you)\b`),
	regexp.MustCompile(`(?i)\bthe\s+(?:approach|plan|solution)\s+is\s+to\b`),
	// Markdown-fenced patch blocks in the final message are a strong
	// signal: the model wrote a diff instead of applying one.
	regexp.MustCompile("(?m)^```(?:diff|patch)\\b"),
}

// blockedPhrases matches the "I'm blocked because X" shape the
// coding-mode prompt names as the acceptable non-edit terminal state.
// When the final text matches one of these, the dangle guard suppresses
// itself — the agent has explicitly signaled it needs input rather than
// silently dangling.
var blockedPhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bi['’]?m\s+blocked\b`),
	regexp.MustCompile(`(?i)\bi\s+am\s+blocked\b`),
	regexp.MustCompile(`(?i)\bblocked\s+(?:because|on|by|until)\b`),
	regexp.MustCompile(`(?i)\bi\s+need\s+(?:you|the\s+user)\s+to\b`),
	regexp.MustCompile(`(?i)\bcould\s+you\s+(?:clarify|confirm|tell\s+me|let\s+me\s+know)\b`),
	regexp.MustCompile(`(?i)\bwhich\s+(?:one|approach|option)\s+(?:would\s+you|do\s+you|should\s+i)\b`),
	regexp.MustCompile(`(?i)\bcan\s+you\s+(?:clarify|confirm|tell\s+me)\b`),
}

// IsChangeDescription reports whether the text reads as a "here are
// the changes you should make" terminal message. Operates on the
// assistant's final answer text only.
func IsChangeDescription(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, re := range changeDescriptionPhrases {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// IsExplicitBlock reports whether the text explicitly signals the
// model needs more information from the user before proceeding. When
// true, the dangle guard suppresses itself.
func IsExplicitBlock(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, re := range blockedPhrases {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// EditToolCalls counts the number of write/edit tool invocations
// across the full transcript. Used by the dangle guard to distinguish
// "agent made some edits and is describing the rest" (no fire — likely
// genuine wrap-up prose) from "agent made zero edits" (fire — the
// classic dangling-turn shape).
func EditToolCalls(msgs []message.Message) int {
	n := 0
	for _, m := range msgs {
		for _, p := range m.Parts {
			if tc, ok := p.(message.ToolCall); ok {
				if tc.Name == "write" || tc.Name == "edit" {
					n++
				}
			}
		}
	}
	return n
}
