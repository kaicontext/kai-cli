package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"kai/api/message"
	"kai/api/provider"
)

// classifierSystemPrompt instructs the model to route a request to one
// of two destinations. It is deliberately terse: the classifier is a
// hot-path call on every substantive turn, so the prompt stays small
// and the model is asked for a single word to keep output (and cost)
// minimal.
const classifierSystemPrompt = `You classify a developer's request for a coding assistant.

Answer CHAT if the user is asking a question, wants an explanation, a
discussion, an opinion, or anything that does not require editing files.

Answer CODE if the user wants the assistant to make a change to the
codebase — write, edit, fix, refactor, add, or remove code.

Reply with exactly one word: CHAT or CODE. Nothing else.`

// classifyTimeout caps the classifier round-trip. The classifier sits
// in front of every substantive turn; if it can't answer quickly the
// caller falls back to the local heuristic rather than making the user
// wait on a slow model.
const classifyTimeout = 20 * time.Second

// classifyRequest asks the classifier model whether request is a
// conversation ("CHAT") or a code-change ("CODE"). It returns isChat,
// or an error when the provider is missing, the call fails, or the
// reply isn't a recognizable verdict — in every error case the caller
// is expected to fall back to the isQuestion heuristic so routing
// never wedges on a model hiccup.
func classifyRequest(ctx context.Context, prov provider.Provider, model, request string) (isChat bool, err error) {
	if prov == nil {
		return false, fmt.Errorf("classify: no provider")
	}
	if strings.TrimSpace(model) == "" {
		return false, fmt.Errorf("classify: no classifier model")
	}

	ctx, cancel := context.WithTimeout(ctx, classifyTimeout)
	defer cancel()

	resp, err := prov.Send(ctx, provider.Request{
		Model:  model,
		System: classifierSystemPrompt,
		Messages: []message.Message{{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: request}},
		}},
		// One word of output is all we need; keep the cap tiny so a
		// chatty model can't turn the classifier into a real expense.
		MaxTokens: 16,
	})
	if err != nil {
		return false, fmt.Errorf("classify: %w", err)
	}

	// Response.Parts mirrors Message.Parts; reuse Message.Text to
	// concatenate the text content rather than re-walking the slice.
	answer := strings.ToUpper(strings.TrimSpace(message.Message{Parts: resp.Parts}.Text()))
	switch {
	case strings.Contains(answer, "CODE"):
		return false, nil
	case strings.Contains(answer, "CHAT"):
		return true, nil
	default:
		return false, fmt.Errorf("classify: unrecognized verdict %q", answer)
	}
}

// routeToChat decides whether request should go straight to the chat
// agent instead of the planner. It asks the classifier model first —
// the "always use a strong model to decide the type of conversation"
// path — and falls back to the local isQuestion heuristic when the
// provider is missing or the call fails, so a model hiccup degrades
// routing gracefully rather than wedging it.
//
// Callers must gate this behind `forced == agent.ModeUnknown` and the
// cheap pre-filters (greeting / short-affirmative) so trivial input
// never pays for a classifier round-trip.
func routeToChat(ctx context.Context, s *PlannerServices, request string) bool {
	isChat, err := classifyRequest(ctx, s.OrchestratorCfg.AgentProvider, s.ClassifierModel, request)
	if err != nil {
		return isQuestion(request)
	}
	return isChat
}
