package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// rcTestPromptText is the prompt the model reads: the text parts of the first
// message, joined.
func rcTestPromptText(req provider.Request) string {
	var b strings.Builder
	for _, p := range req.Messages[0].Parts {
		if t, ok := p.(message.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// rcCacheCalls records the requests of a gate run.
type rcCacheCalls struct {
	mu   sync.Mutex
	reqs []provider.Request
}

func (c *rcCacheCalls) send(t *testing.T) func(context.Context, provider.Request) (provider.Response, error) {
	return func(ctx context.Context, req provider.Request) (provider.Response, error) {
		c.mu.Lock()
		c.reqs = append(c.reqs, req)
		c.mu.Unlock()
		return rcAnswerAllIssues(t, req), nil
	}
}

// rcAnswerAllIssues refutes whatever the prompt asks it to check.
func rcAnswerAllIssues(t *testing.T, req provider.Request) provider.Response {
	t.Helper()
	_, issues, _, _, _, _ := rcParseReviewOutput(rcTestDraftOf(rcTestPromptText(req)))
	ans := rcChallengeAnswer{Scope: []string{"the sources"}, IntentMatch: "partial", MergeReady: 3}
	for _, is := range issues {
		ans.Checks = append(ans.Checks, rcIssueCheck{Issue: is, Verdict: "refuted", Reason: "not what the code does", Remedy: "none", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}})
	}
	return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, ans)}}}
}

func rcTestDraftOf(prompt string) string {
	_, rest, _ := strings.Cut(prompt, "DRAFT (claims to challenge):\n")
	draft, _, _ := strings.Cut(rest, "\n\nISSUES TO CHECK:")
	return draft
}

func rcTestIssues(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("file%d.go:%d — defect number %d", i, i+1, i)
	}
	return out
}

// Every batch must lead with a byte-identical evidence block, with the
// breakpoint right after it and its own claims behind it. That is the whole of
// what makes one batch's cache write readable by the next.
func TestChallengeBatchesShareOneCachedEvidenceBlock(t *testing.T) {
	calls := &rcCacheCalls{}
	p := rcChallengeProvider{send: calls.send(t)}
	sources := []rcSource{rcRowSource("shared line one\nshared line two"), rcRowSource("second shared source")}
	if _, err := rcChallengeBatchesRaw(context.Background(), p, "anthropic/claude-sonnet-5.5", rcTestReview(rcTestIssues(12)...), sources, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(calls.reqs) < 3 {
		t.Fatalf("12 allegations ran as %d calls, want batches", len(calls.reqs))
	}
	evidence := ""
	for i, req := range calls.reqs {
		parts := req.Messages[0].Parts
		if len(parts) != 2 {
			t.Fatalf("call %d: %d parts, want evidence then claims: %+v", i, len(parts), parts)
		}
		ev, claims := parts[0].(message.TextContent), parts[1].(message.TextContent)
		if !ev.CacheBreakpoint {
			t.Errorf("call %d: no breakpoint after the evidence", i)
		}
		if claims.CacheBreakpoint {
			t.Errorf("call %d: a breakpoint on the claims would be this batch's alone", i)
		}
		if evidence == "" {
			evidence = ev.Text
		} else if ev.Text != evidence {
			t.Errorf("call %d: evidence differs between batches:\n%q\n%q", i, ev.Text, evidence)
		}
		if strings.Contains(ev.Text, "DRAFT (claims") || strings.Contains(ev.Text, "defect number") {
			t.Errorf("call %d: claims leaked into the shared block", i)
		}
		if !strings.Contains(claims.Text, "ISSUES TO CHECK") || !strings.Contains(claims.Text, "defect number") {
			t.Errorf("call %d: claims are not after the evidence: %q", i, claims.Text)
		}
	}
	if !strings.HasPrefix(evidence, "SOURCES (the evidence") || !strings.Contains(evidence, "second shared source") {
		t.Errorf("evidence block lost sources: %q", evidence)
	}
}

// What the model reads must not change. This spells out the prompt as it was
// when it was one string, so a change to either side of the split shows up.
func TestChallengePromptTextIsUnchangedBySplit(t *testing.T) {
	var got provider.Request
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		got = req
		return rcAnswerAllIssues(t, req), nil
	}}
	sources := []rcSource{rcRowSource("alpha"), rcRowSource("beta")}
	extra := rcRowSource("gamma")
	draft := rcTestReview(rcFalseCDIssue)
	all := append(append([]rcSource(nil), sources...), extra)
	if _, err := rcChallengeDraftShared(context.Background(), p, "anthropic/claude-sonnet-5.5", draft, all, len(sources), nil); err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	want.WriteString("SOURCES (the evidence; cite by source number and line range):\n")
	for i, s := range all {
		fmt.Fprintf(&want, "\n%s", rcRenderSource(i+1, s))
	}
	fmt.Fprintf(&want, "\n\nDRAFT (claims to challenge):\n%s\n\nISSUES TO CHECK:\n- %s\n", draft, rcFalseCDIssue)
	if text := rcTestPromptText(got); text != want.String() {
		t.Fatalf("prompt changed:\n got %q\nwant %q", text, want.String())
	}
	ev := got.Messages[0].Parts[0].(message.TextContent)
	if !strings.HasSuffix(ev.Text, rcRenderSource(2, sources[1])) || strings.Contains(ev.Text, "gamma") {
		t.Fatalf("the breakpoint is not at the end of the shared sources: %q", ev.Text)
	}
}

// A batch's own diff chunks come after the breakpoint: inside the shared block
// they would give every batch a different one.
func TestChallengeBatchExtraSourcesStayOutOfTheSharedBlock(t *testing.T) {
	calls := &rcCacheCalls{}
	p := rcChallengeProvider{send: calls.send(t)}
	issues := rcTestIssues(10)
	extra := []rcSource{rcRowSource("+++ b/file0.go\nchunk for file0"), rcRowSource("+++ b/file9.go\nchunk for file9")}
	if _, err := rcChallengeBatchesRaw(context.Background(), p, "anthropic/claude-sonnet-5.5", rcTestReview(issues...), []rcSource{rcRowSource("base")}, extra, nil); err != nil {
		t.Fatal(err)
	}
	seenChunk := 0
	for i, req := range calls.reqs {
		parts := req.Messages[0].Parts
		ev, rest := parts[0].(message.TextContent), parts[1].(message.TextContent)
		if strings.Contains(ev.Text, "chunk for") {
			t.Errorf("call %d: a batch's own chunk is in the shared block", i)
		}
		if strings.Contains(rest.Text, "chunk for") {
			seenChunk++
		}
	}
	if seenChunk == 0 {
		t.Fatal("no batch received its diff chunk")
	}
}

func TestChallengeWithoutSharedSourcesHasNoBreakpoint(t *testing.T) {
	parts := rcChallengePromptParts("whole prompt", 0)
	if len(parts) != 1 || parts[0].(message.TextContent).CacheBreakpoint {
		t.Fatalf("parts = %+v", parts)
	}
}

// A retry round's follow-ups and the first turn must carry one session, which
// is what keeps them on the upstream holding the entry.
func TestChallengeCallsOfOneReviewShareACacheSession(t *testing.T) {
	calls := &rcCacheCalls{}
	p := rcChallengeProvider{send: calls.send(t)}
	sources := []rcSource{rcRowSource("base")}
	run := func() string {
		calls.reqs = nil
		if _, err := rcChallengeBatches(context.Background(), p, "anthropic/claude-sonnet-5.5", rcTestReview(rcTestIssues(12)...), sources, nil, nil); err != nil {
			t.Fatal(err)
		}
		session := calls.reqs[0].CacheSession
		if session == "" {
			t.Fatal("no cache session")
		}
		for i, req := range calls.reqs {
			if req.CacheSession != session {
				t.Errorf("call %d: session %q, want %q", i, req.CacheSession, session)
			}
		}
		return session
	}
	if a, b := run(), run(); a == b {
		t.Errorf("two reviews share session %q", a)
	}
}

// rcHoldFirst holds the first call of a run until released, and records what
// started meanwhile.
type rcHoldFirst struct {
	t       *testing.T
	release chan struct{}
	mu      sync.Mutex
	started int
}

func (h *rcHoldFirst) send(ctx context.Context, req provider.Request) (provider.Response, error) {
	h.mu.Lock()
	h.started++
	first := h.started == 1
	h.mu.Unlock()
	if first {
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
	return rcAnswerAllIssues(h.t, req), nil
}

func (h *rcHoldFirst) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}

func rcTestBatches(n int) [][]string {
	out := make([][]string, n)
	for i := range out {
		out[i] = []string{fmt.Sprintf("file%d.go:1 — defect number %d", i, i)}
	}
	return out
}

func rcTestRepoCtx() context.Context {
	return rcWithRepo(context.Background(), &rcRepo{})
}

// The others stay back until the first call has returned, however long the
// wait would otherwise have been.
func TestCacheWarmHoldsTheOtherBatchesUntilTheFirstReturns(t *testing.T) {
	defer func(d time.Duration) { rcCacheWarmWait = d }(rcCacheWarmWait)
	rcCacheWarmWait = time.Hour
	h := &rcHoldFirst{t: t, release: make(chan struct{})}
	p := rcChallengeProvider{send: h.send}
	ctx := rcTestRepoCtx()
	done := make(chan struct{})
	go func() {
		rcRunBatches(ctx, p, "anthropic/claude-sonnet-5.5", "prose", nil, 0, rcTestBatches(4), []rcSource{rcRowSource("base")}, nil, nil, rcNewCacheWarm(ctx, "anthropic/claude-sonnet-5.5", 4))
		close(done)
	}()
	for deadline := time.Now().Add(5 * time.Second); h.count() < 1 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.count(); n != 1 {
		t.Fatalf("%d calls started before the first returned, want 1", n)
	}
	close(h.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the other batches never started")
	}
	if n := h.count(); n != 4 {
		t.Fatalf("%d calls, want 4", n)
	}
}

// A slow first call must not stall the review: the entry is readable long
// before it finishes, so the others go when the wait runs out.
func TestCacheWarmWaitIsBounded(t *testing.T) {
	defer func(d time.Duration) { rcCacheWarmWait = d }(rcCacheWarmWait)
	rcCacheWarmWait = 20 * time.Millisecond
	h := &rcHoldFirst{t: t, release: make(chan struct{})}
	defer close(h.release)
	p := rcChallengeProvider{send: h.send}
	ctx, cancel := context.WithCancel(rcTestRepoCtx())
	defer cancel()
	go rcRunBatches(ctx, p, "anthropic/claude-sonnet-5.5", "prose", nil, 0, rcTestBatches(3), []rcSource{rcRowSource("base")}, nil, nil, rcNewCacheWarm(ctx, "anthropic/claude-sonnet-5.5", 3))
	for deadline := time.Now().Add(5 * time.Second); h.count() < 3; {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 3 calls started while the first was still running", h.count())
		}
		time.Sleep(time.Millisecond)
	}
}

// A first batch that fails opens the way at once.
func TestCacheWarmOpensWhenTheFirstCallFails(t *testing.T) {
	defer func(d time.Duration) { rcCacheWarmWait = d }(rcCacheWarmWait)
	rcCacheWarmWait = time.Hour
	var mu sync.Mutex
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			return provider.Response{}, fmt.Errorf("upstream said no")
		}
		return rcAnswerAllIssues(t, req), nil
	}}
	ctx := rcTestRepoCtx()
	done := make(chan struct{})
	go func() {
		rcRunBatches(ctx, p, "anthropic/claude-sonnet-5.5", "prose", nil, 0, rcTestBatches(3), []rcSource{rcRowSource("base")}, nil, nil, rcNewCacheWarm(ctx, "anthropic/claude-sonnet-5.5", 3))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the other batches waited on a first batch that had failed")
	}
}

// No wait where it buys nothing, or where the clock cannot afford it.
func TestCacheWarmOnlyWhereItPays(t *testing.T) {
	repo := rcTestRepoCtx()
	for name, w := range map[string]*rcCacheWarm{
		"one batch":     rcNewCacheWarm(repo, "anthropic/claude-sonnet-5.5", 1),
		"GLM":           rcNewCacheWarm(repo, "z-ai/glm-5.2", 4),
		"the fast pass": rcNewCacheWarm(context.Background(), "anthropic/claude-sonnet-5.5", 4),
	} {
		if w != nil {
			t.Errorf("%s: waits, and should not", name)
		}
	}
	if rcNewCacheWarm(repo, "anthropic/claude-sonnet-5.5", 2) == nil {
		t.Error("Claude batches in the grounded review do not wait")
	}
}

// rcWireBodies runs the gate through the real provider against a fake proxy and
// returns the request bodies as they would leave the machine.
func rcWireBodies(t *testing.T, model string) []map[string]interface{} {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"not an answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	p := &provider.Kailab{BaseURL: srv.URL, AuthToken: "test", HTTPClient: srv.Client(), MaxAttempts: 1}
	// The answers are refused; what is under test is what was sent.
	_, _ = rcChallengeBatches(context.Background(), p, model, rcTestReview(rcTestIssues(7)...), []rcSource{rcRowSource("base evidence")}, nil, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("nothing reached the proxy")
	}
	return bodies
}

func rcWireUserMessage(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	for _, m := range body["messages"].([]interface{}) {
		if msg := m.(map[string]interface{}); msg["role"] == "user" {
			return msg
		}
	}
	t.Fatalf("no user message: %v", body)
	return nil
}

// The production fact-check spelling goes out on the OpenAI shape. It is the
// path a breakpoint at the end of a message was never going to help.
func TestChallengeWireShapeForClaude(t *testing.T) {
	for _, body := range rcWireBodies(t, "anthropic/claude-sonnet-5.5") {
		parts, ok := rcWireUserMessage(t, body)["content"].([]interface{})
		if !ok || len(parts) != 2 {
			t.Fatalf("content = %v, want evidence and claims", rcWireUserMessage(t, body)["content"])
		}
		ev := parts[0].(map[string]interface{})
		if ev["cache_control"] == nil || !strings.Contains(ev["text"].(string), "base evidence") {
			t.Errorf("evidence block on the wire: %v", ev)
		}
		if !strings.Contains(parts[1].(map[string]interface{})["text"].(string), "ISSUES TO CHECK") {
			t.Errorf("claims block on the wire: %v", parts[1])
		}
		if s, _ := body["session_id"].(string); !strings.HasPrefix(s, "kai-review-") {
			t.Errorf("session_id = %v", body["session_id"])
		}
		if pref, ok := body["provider"].(map[string]interface{}); !ok || pref["order"] != nil {
			t.Errorf("provider = %v: without one the proxy adds an order, which turns stickiness off", body["provider"])
		}
	}
}

// GLM keeps its one string, its routing and no markers, so a provider caching
// by prefix sees the request it saw before.
func TestChallengeWireShapeForOpenModels(t *testing.T) {
	for _, body := range rcWireBodies(t, "z-ai/glm-5.2") {
		content, ok := rcWireUserMessage(t, body)["content"].(string)
		if !ok {
			t.Fatalf("content = %T, want the plain string", rcWireUserMessage(t, body)["content"])
		}
		if !strings.HasPrefix(content, "SOURCES (the evidence") || !strings.Contains(content, "ISSUES TO CHECK") {
			t.Errorf("content = %q", content)
		}
		for _, k := range []string{"session_id", "provider"} {
			if _, has := body[k]; has {
				t.Errorf("sent %s for GLM: %v", k, body[k])
			}
		}
	}
}
