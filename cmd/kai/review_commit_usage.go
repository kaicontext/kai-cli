package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/kaicontext/kai-engine/provider"
)

type rcUsageStageKey struct{}

func rcUsageStage(ctx context.Context, stage string) context.Context {
	return context.WithValue(ctx, rcUsageStageKey{}, stage)
}

type rcUsageCall struct {
	ID         int      `json:"id"`
	Stage      string   `json:"stage"`
	Model      string   `json:"model"`
	Started    string   `json:"started"`
	Seconds    float64  `json:"seconds"`
	Input      int      `json:"inputTokens"`
	Output     int      `json:"outputTokens"`
	CacheRead  int      `json:"cacheReadTokens"`
	CacheWrite int      `json:"cacheWriteTokens"`
	Reasoning  int      `json:"reasoningTokens"`
	Estimate   *float64 `json:"estimatedUSD"`
	Error      string   `json:"error,omitempty"`
}
type rcUsageSnapshot struct {
	ReviewID         string        `json:"reviewId"`
	Calls            []rcUsageCall `json:"calls"`
	KnownEstimate    float64       `json:"knownEstimatedUSD"`
	UnknownCostCalls int           `json:"unknownCostCalls"`
	Complete         bool          `json:"costComplete"`
	Limitation       string        `json:"limitation"`
}
type rcUsageMeter struct {
	persistMu sync.Mutex
	path      string
	mu        sync.Mutex
	id        string
	next      int
	calls     []rcUsageCall
}
type rcUsageProvider struct {
	base  provider.Provider
	meter *rcUsageMeter
}
type rcUsageStreamProvider struct {
	*rcUsageProvider
	stream provider.Streamer
}

func rcMeterProvider(base provider.Provider, id string) (provider.Provider, *rcUsageMeter) {
	m := &rcUsageMeter{id: id, path: os.Getenv("KAI_REVIEW_USAGE_PATH")}
	p := &rcUsageProvider{base: base, meter: m}
	if stream, ok := base.(provider.Streamer); ok {
		return &rcUsageStreamProvider{p, stream}, m
	}
	return p, m
}
func (p *rcUsageProvider) SupportsCacheForModel(model string) bool {
	return provider.SupportsCacheForModel(p.base, model)
}
func (p *rcUsageProvider) SupportsCache() bool          { return provider.SupportsCache(p.base) }
func (p *rcUsageProvider) DailyUsage() (int, int, bool) { return provider.DailyUsage(p.base) }
func (p *rcUsageProvider) begin(ctx context.Context, req provider.Request) func(provider.Response, error) {
	started := time.Now()
	stage, _ := ctx.Value(rcUsageStageKey{}).(string)
	if stage == "" {
		stage = "unattributed"
	}
	p.meter.mu.Lock()
	p.meter.next++
	id := p.meter.next
	p.meter.mu.Unlock()
	p.meter.persist()
	return func(resp provider.Response, err error) {
		c := rcUsageCall{ID: id, Stage: stage, Model: req.Model, Started: started.UTC().Format(time.RFC3339Nano), Seconds: time.Since(started).Seconds(), Input: resp.InputTokens, Output: resp.OutputTokens, CacheRead: resp.CacheReadTokens, CacheWrite: resp.CacheCreationTokens, Reasoning: resp.ReasoningTokens}
		if resp.EstimatedCostUSD > 0 {
			v := resp.EstimatedCostUSD
			c.Estimate = &v
		}
		if err != nil {
			c.Error = err.Error()
		}
		p.meter.mu.Lock()
		p.meter.calls = append(p.meter.calls, c)
		p.meter.mu.Unlock()
		p.meter.persist()
	}
}
func (p *rcUsageProvider) Send(ctx context.Context, req provider.Request) (provider.Response, error) {
	finish := p.begin(ctx, req)
	r, e := p.base.Send(ctx, req)
	finish(r, e)
	return r, e
}
func (p *rcUsageStreamProvider) SendStream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	finish := p.begin(ctx, req)
	ch, err := p.stream.SendStream(ctx, req)
	if err != nil {
		finish(provider.Response{}, err)
		return nil, err
	}
	out := make(chan provider.StreamEvent, 1)
	go func() {
		defer close(out)
		done := false
		defer func() {
			if !done {
				err := ctx.Err()
				if err == nil {
					err = fmt.Errorf("stream ended without usage result")
				}
				finish(provider.Response{}, err)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				if !done && (e.Kind == "done" || e.Kind == "error") {
					r := provider.Response{}
					if e.Final != nil {
						r = *e.Final
					}
					finish(r, e.Err)
					done = true
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
func (m *rcUsageMeter) snapshot() *rcUsageSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &rcUsageSnapshot{ReviewID: m.id, Calls: append([]rcUsageCall(nil), m.calls...), Complete: true, Limitation: "Per-provider-call estimates, not invoice charges. Internal provider retries may be hidden. Failed calls and missing prices can have unreported spend; judges and compute excluded."}
	for _, c := range s.Calls {
		if c.Estimate == nil {
			s.UnknownCostCalls++
			s.Complete = false
		} else {
			s.KnownEstimate += *c.Estimate
		}
		if c.Error != "" {
			s.Complete = false
		}
	}
	if len(s.Calls) != m.next {
		s.Complete = false
	}
	return s
}

// Persist after each completed call, including errors, so an interrupted review
// retains partial accounting. No prompts, credentials, or generated text.
func (m *rcUsageMeter) persist() {
	if m.path == "" {
		return
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	data, err := json.Marshal(m.snapshot())
	if err != nil {
		return
	}
	tmp := m.path + ".tmp"
	if err = os.WriteFile(tmp, data, 0644); err == nil {
		err = os.Rename(tmp, m.path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "review usage write failed: %v\n", err)
	}
}
