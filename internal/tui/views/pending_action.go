// Pending-action slot. When the chat agent ends a turn with the
// "Reply 'yes' and I'll apply it." trailer, kai captures the
// prose preceding the trailer as a structured pending action.
// On the next turn, a short affirmative consumes the slot: the
// prompt is wrapped with an explicit "you offered X, user
// confirmed, execute X" preamble so the model doesn't have to
// scan history and infer which offer "yes" refers to.
//
// Why this exists (2026-05-26 confirmation-loop spec): the
// v0.32.15 trailer detection + mode pre-arm + session
// inheritance got the chat agent into coding mode with the
// prior session loaded — but the action remained as free prose
// in history. The model could (and did) fail to bind "yes" to
// the preceding offer, asking "what would you like me to do?"
// instead. Making the binding structural fixes the inference
// gap.
package views

import "strings"

// trailerLine is the exact handoff text the conversation-mode
// prompt instructs the chat agent to emit when proposing a
// concrete change. Detection and stripping are exact-match
// deterministic on this string.
const trailerLine = "Reply 'yes' and I'll apply it."

// pendingAction is the proposed-action payload waiting for
// confirmation. Single-use: consumed on affirmative, cleared on
// negative or on a new non-affirmative non-negative request.
type pendingAction struct {
	// text is the assistant's prose proposal with the trailer
	// line removed. Wrapped into the next turn's prompt verbatim
	// so the model sees its own previously-emitted plan as the
	// thing it's being asked to execute.
	text string
}

// offerPatterns are the phrases that signal the chat agent is
// offering to do work. The trailer line is the primary signal
// (prompt instructs the model to emit it), but real-world models
// vary the phrasing — DeepSeek says "Want me to switch to a mode
// that can apply these?", GLM says "Shall I apply this?", etc.
// Detecting these as alternatives prevents the "yes" → "I can't
// edit in this mode" failure mode the 2026-05-26 dogfood pinned
// when the strict-string detector missed a semantically-identical
// but structurally-different offer.
//
// All patterns matched case-insensitively. Conservative list:
// every phrase here is one a model uses to ask permission before
// doing work, never as casual prose. Worst-case false positive
// is one over-routed coding-mode turn where the model clarifies
// because there's nothing concrete to execute — recoverable.
var offerPatterns = []string{
	"reply 'yes' and i'll apply it.", // primary trailer (lowercased)
	"want me to apply",
	"want me to switch to",
	"want me to make",
	"want me to do",
	"want me to fix",
	"shall i apply",
	"shall i fix",
	"shall i make",
	"shall i proceed",
	"should i apply",
	"should i go ahead",
	"should i make",
	"should i proceed",
	"should i fix",
	"switch to a mode that can apply",
	"switch to coding mode",
}

// extractPendingAction returns a populated pendingAction when the
// reply offers work — primarily via the exact trailer line, with
// a list of common offer-phrase fallbacks for when the model
// deviates from the literal trailer. The action text is the prose
// preceding the offer marker (or the full reply when only a
// fallback pattern matched and there's no clean split point).
func extractPendingAction(reply string) *pendingAction {
	// Fast path: exact trailer present. Split at trailer so the
	// returned text excludes the trailer line itself.
	if idx := strings.Index(reply, trailerLine); idx >= 0 {
		text := strings.TrimSpace(reply[:idx])
		if text == "" {
			return nil
		}
		return &pendingAction{text: text}
	}
	// Fallback: any offer pattern. Return the whole reply as the
	// action text — these patterns sit in-line in prose, so a
	// clean "everything before the offer" split isn't reliable.
	lower := strings.ToLower(reply)
	for _, p := range offerPatterns[1:] { // [0] already checked above
		if strings.Contains(lower, p) {
			text := strings.TrimSpace(reply)
			if text == "" {
				return nil
			}
			return &pendingAction{text: text}
		}
	}
	return nil
}

// wrapPendingActionPrompt builds the prompt sent to the chat
// agent when the user confirms a pending action. Frames the
// model's own prior proposal as the work it should now execute,
// removing the inference burden of scanning history for "what
// did I offer."
//
// confirmation is the user's literal affirmative ("yes", "y",
// "do it") so the model sees the round-trip and doesn't
// hallucinate other intent.
func wrapPendingActionPrompt(action *pendingAction, confirmation string) string {
	var b strings.Builder
	b.WriteString("[pending action — user just confirmed]\n\n")
	b.WriteString("Your previous reply proposed the following:\n\n---\n")
	b.WriteString(action.text)
	b.WriteString("\n---\n\n")
	b.WriteString("The user confirmed with: ")
	b.WriteString(strings.TrimSpace(confirmation))
	b.WriteString("\n\nExecute exactly what you proposed above. ")
	b.WriteString("You already did the analysis; do not re-explore or ask clarifying questions. ")
	b.WriteString("Make the edits / run the commands now. ")
	b.WriteString("If a step in your proposal genuinely cannot be executed (a path doesn't exist, a tool is missing), state which specific step is blocked and why — never bounce the whole confirmation back as a clarifying question.")
	return b.String()
}

// isShortNegative recognizes follow-up replies that decline the
// pending action — mirror of isShortAffirmative. Used to clear
// the pending action slot with an acknowledgement rather than
// silently consuming it on the next message.
func isShortNegative(request string) bool {
	r := strings.ToLower(strings.TrimSpace(request))
	if r == "" {
		return false
	}
	if len(r) > 20 {
		return false
	}
	r = strings.TrimRight(r, "!.?,")
	r = strings.Join(strings.Fields(r), " ")
	switch r {
	case "no", "nope", "n", "nah",
		"cancel", "stop", "abort",
		"don't", "do not", "skip it", "not now", "never mind", "nevermind":
		return true
	}
	return false
}
