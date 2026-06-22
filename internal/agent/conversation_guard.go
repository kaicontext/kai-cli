package agent

import "strings"

// conversation_guard.go: the chat-mode "search first" enforcement.
//
// ModeConversation is read-only Q&A about the workspace. The mode
// prompt already pleads with the model to ground every workspace
// answer in a tool call (the WORKSPACE-COUNTERFACTUAL TEST and friends
// in mode.go), but prompt pleading is best-effort — dogfood kept
// catching confident prose answers built from training priors with
// zero tool calls behind them.
//
// This guard makes it deterministic: in conversation mode, an answer
// that tries to finalize without ANY codebase tool call behind it
// (for the whole run) is rejected and the model is sent back to search
// first. The one exception is a pure pleasantry ("hi", "thanks", "ok")
// — forcing a grep before "you're welcome" is pointless ceremony.
//
// The runner fires it at most once per run (conversationSearchGuardFired)
// so a model that still refuses after one nudge is allowed to answer
// rather than spinning the loop. One forced re-prompt is the whole
// enforcement: it converts "decide whether to look" into "look."

// conversationSearchGuardNudge is the message appended to history when
// the guard fires. Positive, concrete instruction (per the project's
// "tell the model what TO do" guidance): search now, cite file:line.
const conversationSearchGuardNudge = `Before answering, search the codebase. This is a question about THIS workspace, so the answer must come from the actual code — not from memory or general patterns.

Make at least one codebase tool call now (kai_search / kai_grep / kai_tree / view / kai_context), then answer from what the results actually say and cite the file:line you relied on. Do not answer from priors.`

// truncatedAnswerNudge is sent when a reasoning model trails off
// mid-thought and ends its turn (finish=end_turn) leaving a visibly
// incomplete answer — observed 2026-05-29 with DeepSeek-V4-Pro: it
// spent ~1250 of 1401 output tokens on hidden reasoning, then emitted
// "...After checking the main command code, I can…" and stopped. The
// user saw a cut-off non-answer even though the question was answerable.
const truncatedAnswerNudge = `Your previous reply ended mid-thought — it trailed off without finishing. Give your COMPLETE final answer now: the full response, not a fragment, and don't end on an ellipsis. You've already done the investigation; just state the answer.`

// looksTruncated reports whether a final answer appears cut off
// mid-thought. Deliberately narrow to keep false positives near zero:
// it only fires when the trimmed text ends on a literal ellipsis
// ("…" or "..."), which a complete answer almost never does. Trailing
// whitespace and a closing code-fence newline are tolerated. Empty
// text is not "truncated" (the empty-response path handles that).
func looksTruncated(text string) bool {
	t := strings.TrimRight(text, " \t\r\n")
	if t == "" {
		return false
	}
	return strings.HasSuffix(t, "…") || strings.HasSuffix(t, "...")
}

// pleasantryWords is the set of tokens a message may be composed
// entirely of and still count as a pleasantry (greeting / thanks /
// acknowledgement). Kept deliberately tight: anything that looks like
// a real question must NOT be exempt, so words like "yes"/"no" (which
// usually mean "do it"/"don't") are intentionally absent.
var pleasantryWords = map[string]bool{
	"hi": true, "hello": true, "hey": true, "yo": true, "hiya": true,
	"thanks": true, "thank": true, "you": true, "thx": true, "ty": true,
	"ok": true, "okay": true, "k": true, "kk": true,
	"cool": true, "nice": true, "great": true, "awesome": true, "perfect": true,
	"got": true, "it": true, "gotcha": true,
	"sounds": true, "good": true, "lgtm": true,
	"np": true, "cheers": true, "please": true,
	"bye": true, "goodbye": true, "ciao": true,
}

// isPleasantry reports whether msg is a pure social nicety with no
// substantive request — a greeting, a thanks, or a short
// acknowledgement. Used to exempt such messages from the conversation
// search guard so the model can reply "you're welcome" without first
// grepping the tree.
//
// The test is conservative: the message must be non-empty and EVERY
// word (after stripping punctuation) must be in pleasantryWords. A
// single content word ("ok so how does auth work") fails the test and
// the guard engages.
func isPleasantry(msg string) bool {
	fields := strings.Fields(strings.ToLower(msg))
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		w := strings.TrimFunc(f, func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
		})
		if w == "" {
			// Pure punctuation/emoji token ("!", "🙏") — ignore it.
			continue
		}
		if !pleasantryWords[w] {
			return false
		}
	}
	return true
}
