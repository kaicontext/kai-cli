package views

import (
	"context"
	"errors"
	"testing"

	"kai/api/message"
	"kai/api/provider"
)

// stubClassifierProvider is a provider.Provider that returns a fixed
// reply (or error) — enough to exercise classifyRequest's parsing
// without a network call.
type stubClassifierProvider struct {
	reply string
	err   error
}

func (s stubClassifierProvider) Send(ctx context.Context, req provider.Request) (provider.Response, error) {
	if s.err != nil {
		return provider.Response{}, s.err
	}
	return provider.Response{
		Parts:        []message.ContentPart{message.TextContent{Text: s.reply}},
		FinishReason: message.FinishReasonEndTurn,
	}, nil
}

func TestClassifyRequest_Verdicts(t *testing.T) {
	cases := []struct {
		name     string
		reply    string
		wantChat bool
		wantErr  bool
	}{
		{"plain chat", "CHAT", true, false},
		{"plain code", "CODE", false, false},
		{"lowercase chat", "chat", true, false},
		{"verdict with prose", "CODE — the user wants an edit", false, false},
		{"unrecognized verdict falls to error", "maybe?", false, true},
		{"empty reply is an error", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isChat, err := classifyRequest(context.Background(),
				stubClassifierProvider{reply: c.reply}, "test-model", "do a thing")
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if err == nil && isChat != c.wantChat {
				t.Errorf("isChat = %v, want %v", isChat, c.wantChat)
			}
		})
	}
}

func TestClassifyRequest_GuardClauses(t *testing.T) {
	if _, err := classifyRequest(context.Background(), nil, "m", "x"); err == nil {
		t.Error("nil provider should error")
	}
	if _, err := classifyRequest(context.Background(),
		stubClassifierProvider{reply: "CHAT"}, "  ", "x"); err == nil {
		t.Error("empty model should error")
	}
	provErr := stubClassifierProvider{err: errors.New("boom")}
	if _, err := classifyRequest(context.Background(), provErr, "m", "x"); err == nil {
		t.Error("provider error should propagate")
	}
}

// TestRouteToChat_FallsBackToHeuristic confirms routeToChat degrades
// to the isQuestion heuristic when the classifier call fails — a
// model hiccup must not wedge routing.
func TestRouteToChat_FallsBackToHeuristic(t *testing.T) {
	// Provider that always errors → classifyRequest fails → routeToChat
	// must consult isQuestion instead.
	s := &PlannerServices{ClassifierModel: "test-model"}
	s.OrchestratorCfg.AgentProvider = stubClassifierProvider{err: errors.New("down")}

	if !routeToChat(context.Background(), s, "what does this function do?") {
		t.Error("a question should route to chat via the heuristic fallback")
	}
	if routeToChat(context.Background(), s, "add a retry to the upload path") {
		t.Error("a code request should route to the planner via the heuristic fallback")
	}
}
