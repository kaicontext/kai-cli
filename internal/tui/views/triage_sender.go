package views

import (
	"context"

	"kai/api/message"
	"kai/api/provider"
	"kai/api/triage"
)

// triageSender adapts a provider.Provider to triage.Sender so the
// triage package stays free of the provider / message types — which
// keeps its unit tests trivial (canned text in, canned text out).
// Mirrors internal/agent.consultSender.
type triageSender struct {
	p provider.Provider
}

func (s triageSender) Send(ctx context.Context, req triage.SenderRequest) (triage.SenderResponse, error) {
	resp, err := s.p.Send(ctx, provider.Request{
		Model:  req.Model,
		System: req.System,
		Messages: []message.Message{
			{
				Role:  message.RoleUser,
				Parts: []message.ContentPart{message.TextContent{Text: req.UserText}},
			},
		},
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return triage.SenderResponse{}, err
	}
	var out string
	for _, part := range resp.Parts {
		if t, ok := part.(message.TextContent); ok {
			out += t.Text
		}
	}
	return triage.SenderResponse{Text: out}, nil
}
