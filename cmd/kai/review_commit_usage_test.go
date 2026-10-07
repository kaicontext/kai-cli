package main

import (
	"context"
	"errors"
	"github.com/kaicontext/kai-engine/provider"
	"testing"
)

type rcUsageTestStream struct{ rcChallengeProvider }

func (p rcUsageTestStream) SendStream(context.Context, provider.Request) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, 2)
	ch <- provider.StreamEvent{Kind: "text_delta", Text: "unchanged"}
	ch <- provider.StreamEvent{Kind: "done", Final: &provider.Response{InputTokens: 10, OutputTokens: 2, EstimatedCostUSD: 0.01}}
	close(ch)
	return ch, nil
}
func TestUsagePreservesStreamingAndUnknownCosts(t *testing.T) {
	base := rcUsageTestStream{rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{}, errors.New("rate limit")
	}}}
	p, m := rcMeterProvider(base, "review-id")
	streamer, ok := p.(provider.Streamer)
	if !ok {
		t.Fatal("lost streaming")
	}
	ch, err := streamer.SendStream(rcUsageStage(context.Background(), "main"), provider.Request{Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for range ch {
		count++
	}
	if count != 2 {
		t.Fatal("changed stream")
	}
	p.Send(rcUsageStage(context.Background(), "sweep"), provider.Request{Model: "model"})
	s := m.snapshot()
	if len(s.Calls) != 2 || s.Complete || s.UnknownCostCalls != 1 || s.KnownEstimate != 0.01 || s.Calls[0].Stage != "main" || s.Calls[1].Stage != "sweep" || s.Calls[1].Estimate != nil {
		t.Fatalf("bad usage: %+v", s)
	}
}
