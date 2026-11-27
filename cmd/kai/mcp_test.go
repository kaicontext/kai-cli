package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mcpFakeRun records each child invocation and answers an estimate or a
// review bundle.
type mcpFakeRun struct {
	calls    [][]string
	estimate float64
	bundle   string
	stderr   string
	err      error
}

func (f *mcpFakeRun) run(_ context.Context, flags []string) (string, string, error) {
	f.calls = append(f.calls, flags)
	for _, fl := range flags {
		if fl == "--estimate" {
			out, _ := json.Marshal(rcLocalEstimateReport{Depth: "deep", EstimateUSD: f.estimate, CapUSD: 5, Models: []string{rcLocalDeepModel}, ChangedFiles: 3})
			return string(out), "", nil
		}
	}
	return f.bundle, f.stderr, f.err
}

const mcpTestBundle = `{"review":"Add now subtracts.","depth":"fast","intent":{"risks":["calc.go:3 — Add subtracts b (severity: high)"]},
	"local":{"provider":"anthropic-local-review","models":["claude-haiku-4-5"],"spentUsd":0.031,"capUsd":0.5,"calls":2,"base":"origin/main","workingTree":true}}`

func has(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// A fast review runs straight away and comes back as findings the agent can
// act on, with what it cost and what it is not.
func TestMCPReviewFast(t *testing.T) {
	f := &mcpFakeRun{bundle: mcpTestBundle}
	text, isErr := mcpReview(context.Background(), f.run, mcpReviewArgs{Base: "origin/dev", MaxCostUSD: 0.25})
	if isErr {
		t.Fatalf("error result: %s", text)
	}
	if len(f.calls) != 1 || has(f.calls[0], "--deep") || has(f.calls[0], "--yes") || has(f.calls[0], "--estimate") {
		t.Fatalf("child calls: %v", f.calls)
	}
	if got := strings.Join(f.calls[0], " "); got != "--base origin/dev --max-cost 0.25" {
		t.Errorf("flags = %q", got)
	}
	for _, want := range []string{"against origin/main", "Add now subtracts.", "Findings (1):", "1. calc.go:3", "$0.031", "claude-haiku-4-5", "check of record"} {
		if !strings.Contains(text, want) {
			t.Errorf("result is missing %q:\n%s", want, text)
		}
	}
}

// No findings from a fast review is reported as such, and not as an all-clear.
func TestMCPReviewFastNoFindings(t *testing.T) {
	f := &mcpFakeRun{bundle: `{"review":"Looks consistent.","depth":"fast","intent":{}}`}
	text, isErr := mcpReview(context.Background(), f.run, mcpReviewArgs{})
	if isErr || !strings.Contains(text, "Findings: none.") || !strings.Contains(text, "not an all-clear") {
		t.Errorf("isErr=%v\n%s", isErr, text)
	}
}

// A deep review is never started by the first call: the agent gets the
// estimate and has to come back with the figure the user agreed to. A stale
// or invented figure below the current estimate gets the estimate again.
func TestMCPReviewDeepNeedsTheUsersYes(t *testing.T) {
	for _, confirmed := range []float64{0, 0.10, 1.19} {
		f := &mcpFakeRun{estimate: 1.20, bundle: mcpTestBundle}
		text, isErr := mcpReview(context.Background(), f.run, mcpReviewArgs{Deep: true, ConfirmedUSD: confirmed})
		if isErr {
			t.Fatalf("confirmed=%v: error result: %s", confirmed, text)
		}
		if len(f.calls) != 1 || !has(f.calls[0], "--estimate") || has(f.calls[0], "--yes") {
			t.Errorf("confirmed=%v: child calls %v, want only the estimate", confirmed, f.calls)
		}
		if !strings.Contains(text, "$1.20") || !strings.Contains(text, "Nothing has been run") || !strings.Contains(text, "confirmed_cost_usd 1.20") {
			t.Errorf("confirmed=%v: %s", confirmed, text)
		}
	}
	f := &mcpFakeRun{estimate: 1.20, bundle: strings.Replace(mcpTestBundle, `"depth":"fast"`, `"depth":"grounded"`, 1)}
	text, isErr := mcpReview(context.Background(), f.run, mcpReviewArgs{Deep: true, ConfirmedUSD: 1.20, Commit: "abc123"})
	if isErr || len(f.calls) != 2 {
		t.Fatalf("confirmed deep review: isErr=%v calls=%v\n%s", isErr, f.calls, text)
	}
	run := f.calls[1]
	if !has(run, "--deep") || !has(run, "--yes") || has(run, "--estimate") || run[len(run)-2] != "--" || run[len(run)-1] != "abc123" {
		t.Errorf("deep run flags: %v", run)
	}
	if !strings.Contains(text, "Kai local review (grounded)") {
		t.Errorf("result: %s", text)
	}
}

// A review that could not start says why, from the child's own last words.
func TestMCPReviewFailureIsExplained(t *testing.T) {
	f := &mcpFakeRun{err: errors.New("exit status 1"),
		stderr: "Update available: 0.1 → 0.2 (run `kai update`)\nError: local review: no Anthropic API key\nlocal review: no Anthropic API key\n"}
	text, isErr := mcpReview(context.Background(), f.run, mcpReviewArgs{})
	if !isErr || !strings.Contains(text, "did not run") || !strings.Contains(text, "no Anthropic API key") ||
		strings.Contains(text, "Update available") || strings.Count(text, "no Anthropic API key") != 1 {
		t.Errorf("isErr=%v\n%s", isErr, text)
	}
}

func mcpCall(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Name = "kai_review"
	req.Params.Arguments = args
	return req
}

// An argument cannot smuggle a flag into the child, and depth is one of two
// words.
func TestMCPReviewHandlerRejectsFlagLikeValues(t *testing.T) {
	f := &mcpFakeRun{bundle: mcpTestBundle}
	h := mcpReviewHandler(f.run)
	for _, args := range []map[string]any{
		{"base": "--yes"}, {"commit": "--max-cost=100"}, {"depth": "thorough"},
	} {
		res, err := h(context.Background(), mcpCall(args))
		if err != nil || res == nil || !res.IsError {
			t.Errorf("%v: accepted (res=%+v err=%v)", args, res, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a rejected call ran the child: %v", f.calls)
	}
	if res, err := h(context.Background(), mcpCall(map[string]any{})); err != nil || res.IsError || len(f.calls) != 1 {
		t.Errorf("a plain call: res=%+v err=%v calls=%v", res, err, f.calls)
	}
}

type mcpFakeTool struct{ name string }

func (f mcpFakeTool) Info() tools.ToolInfo {
	return tools.ToolInfo{Name: f.name, Description: "d", Parameters: map[string]any{"symbol": map[string]any{"type": "string"}}, Required: []string{"symbol"}}
}

func (f mcpFakeTool) Run(_ context.Context, c tools.ToolCall) (tools.ToolResponse, error) {
	if strings.Contains(c.Input, "boom") {
		return tools.NewTextErrorResponse("no such symbol"), nil
	}
	return tools.NewTextResponse(f.name + " got " + c.Input), nil
}

// Only the graph queries are served; the engine's other tools (mail, shell,
// memory) are not an MCP client's to call.
func TestMCPServesOnlyGraphTools(t *testing.T) {
	srv := server.NewMCPServer("kai", "test")
	n := mcpAddGraphTools(srv, []tools.BaseTool{
		mcpFakeTool{"kai_callers"}, mcpFakeTool{"kai_impact"}, mcpFakeTool{"kai_gmail"}, mcpFakeTool{"kai_bash"}, mcpFakeTool{"kai_memory"},
	})
	if n != 2 {
		t.Errorf("served %d tools, want 2", n)
	}
	h := mcpEngineHandler(mcpFakeTool{"kai_callers"})
	res, err := h(context.Background(), mcpCall(map[string]any{"symbol": "Add"}))
	if err != nil || res.IsError {
		t.Fatalf("call: %+v %v", res, err)
	}
	if text := res.Content[0].(mcp.TextContent).Text; text != `kai_callers got {"symbol":"Add"}` {
		t.Errorf("tool saw %q", text)
	}
	if res, _ := h(context.Background(), mcpCall(map[string]any{"symbol": "boom"})); !res.IsError {
		t.Error("a tool's error came back as a success")
	}
}

// A cancelled tool call returns even when something the child started still
// holds its output pipe: the wait on the pipes is bounded.
func TestMCPChildWaitIsBoundedAfterCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	old := mcpChildWaitDelay
	mcpChildWaitDelay = 300 * time.Millisecond
	defer func() { mcpChildWaitDelay = old }()

	ctx, cancel := context.WithCancel(context.Background())
	// The shell is killed on cancel; the sleep it started keeps stdout open.
	c := mcpChild(ctx, "sh", []string{"-c", "sleep 30 & echo started; wait"}, t.TempDir())
	if c.WaitDelay != mcpChildWaitDelay {
		t.Fatalf("WaitDelay = %v, want %v", c.WaitDelay, mcpChildWaitDelay)
	}
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled child was still being waited on after 10s")
	}
}

// The provider argument reaches the child as --local-provider, the estimate
// names the vendor whose key it is for, and a value outside the three is
// refused before anything runs.
func TestMCPReviewProvider(t *testing.T) {
	f := &mcpFakeRun{bundle: mcpTestBundle}
	if _, isErr := mcpReview(context.Background(), f.run, mcpReviewArgs{Provider: "openai"}); isErr {
		t.Fatal("fast review on openai failed")
	}
	if len(f.calls) != 1 || !has(f.calls[0], "--local-provider") || !has(f.calls[0], "openai") {
		t.Errorf("child flags: %v", f.calls)
	}
	if got := mcpVendor("openai-local-review"); got != "OpenAI" {
		t.Errorf("vendor of a bundle provider = %q", got)
	}
	if got := mcpVendor("openrouter"); got != "OpenRouter" {
		t.Errorf("vendor of a provider name = %q", got)
	}
	ran := false
	h := mcpReviewHandler(func(context.Context, []string) (string, string, error) { ran = true; return "", "", nil })
	res, err := h(context.Background(), mcpCall(map[string]any{"provider": "gemini"}))
	if err != nil || res == nil || !res.IsError || ran {
		t.Errorf("provider gemini: res=%+v err=%v ran=%v", res, err, ran)
	}
}
