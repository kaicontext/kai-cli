package agent

import (
	"context"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/tools"
)

// consultSender adapts a provider.Provider to the tools.Sender
// interface so the tools package can invoke an LLM without importing
// internal/agent/provider — that would be a cycle (provider already
// imports tools for ToolInfo).
//
// Returns nil when the underlying provider is nil so KaiTools.All()
// can skip kai_consult registration cleanly with a single nil check
// (rather than holding a non-nil wrapper around a nil provider, which
// would fail at call time instead of at registration).
type consultSender struct {
	p provider.Provider
}

func newConsultSender(p provider.Provider) tools.Sender {
	if p == nil {
		return nil
	}
	return &consultSender{p: p}
}

func (s *consultSender) Send(ctx context.Context, req tools.SenderRequest) (tools.SenderResponse, error) {
	pReq := provider.Request{
		Model:  req.Model,
		System: req.System,
		Messages: []message.Message{
			{
				Role:  message.RoleUser,
				Parts: []message.ContentPart{message.TextContent{Text: req.UserText}},
			},
		},
		MaxTokens: req.MaxTokens,
	}
	resp, err := s.p.Send(ctx, pReq)
	if err != nil {
		return tools.SenderResponse{}, err
	}
	var out string
	for _, part := range resp.Parts {
		if t, ok := part.(message.TextContent); ok {
			out += t.Text
		}
	}
	return tools.SenderResponse{Text: out}, nil
}
