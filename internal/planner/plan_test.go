package planner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kai/internal/ai"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/safetygate"
)

// fakeLLM returns a canned response. record captures the system + user
// content so tests can assert what the planner sent.
type fakeLLM struct {
	response string
	err      error
	system   string
	user     string
}

func (f *fakeLLM) Complete(system string, msgs []ai.Message, maxTokens int) (string, error) {
	f.system = system
	if len(msgs) > 0 {
		f.user = msgs[0].Content
	}
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func TestPlan_ValidResponse(t *testing.T) {
	llm := &fakeLLM{response: `{
  "summary": "add rate limiting",
  "agents": [
    {"name":"backend","prompt":"add middleware","files":["router.go"]},
    {"name":"tests","prompt":"add tests","files":["tests/rate_test.go"]}
  ],
  "risk_notes": ["router.go has 4 callers"]
}`}
	plan, err := Plan(context.Background(), "add rate limiting", nil, safetygate.DefaultConfig(), Config{MaxAgents: 5}, llm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Summary != "add rate limiting" {
		t.Errorf("summary: %q", plan.Summary)
	}
	if len(plan.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(plan.Agents))
	}
	if plan.Agents[0].Name != "backend" || plan.Agents[1].Name != "tests" {
		t.Errorf("agent names: %v", plan.Agents)
	}
	if len(plan.RiskNotes) != 1 {
		t.Errorf("expected 1 risk note, got %d", len(plan.RiskNotes))
	}
}

func TestPlan_TooVagueWhenZeroAgents(t *testing.T) {
	llm := &fakeLLM{response: `{"summary":"too vague","agents":[]}`}
	_, err := Plan(context.Background(), "make it better", nil, safetygate.DefaultConfig(), Config{MaxAgents: 5}, llm)
	if !errors.Is(err, ErrTooVague) {
		t.Fatalf("expected ErrTooVague, got %v", err)
	}
}

func TestPlan_UnparseableSurfacesRaw(t *testing.T) {
	llm := &fakeLLM{response: "I'm sorry, I can't help with that."}
	_, err := Plan(context.Background(), "anything", nil, safetygate.DefaultConfig(), Config{MaxAgents: 5}, llm)
	var unparse *ErrUnparseable
	if !errors.As(err, &unparse) {
		t.Fatalf("expected ErrUnparseable, got %v", err)
	}
	if !strings.Contains(unparse.Raw, "I'm sorry") {
		t.Errorf("raw response not preserved: %q", unparse.Raw)
	}
}

func TestPlan_StripsCodeFences(t *testing.T) {
	llm := &fakeLLM{response: "```json\n{\"summary\":\"x\",\"agents\":[{\"name\":\"a\",\"prompt\":\"p\"}]}\n```"}
	plan, err := Plan(context.Background(), "do something", nil, safetygate.DefaultConfig(), Config{MaxAgents: 5}, llm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Agents) != 1 || plan.Agents[0].Name != "a" {
		t.Errorf("fence stripping failed: %+v", plan)
	}
}

// TestPlan_ClipsToMaxAgents — defense in depth. The LLM might ignore
// the system prompt's cap; the planner must still enforce it.
func TestPlan_ClipsToMaxAgents(t *testing.T) {
	llm := &fakeLLM{response: `{"summary":"x","agents":[
  {"name":"a","prompt":"p"},
  {"name":"b","prompt":"p"},
  {"name":"c","prompt":"p"},
  {"name":"d","prompt":"p"}
]}`}
	plan, err := Plan(context.Background(), "do something", nil, safetygate.DefaultConfig(), Config{MaxAgents: 2}, llm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Agents) != 2 {
		t.Errorf("expected clip to 2, got %d", len(plan.Agents))
	}
	if !contains(plan.RiskNotes, "MaxAgents=2") {
		t.Errorf("expected truncation notice in risk notes, got %v", plan.RiskNotes)
	}
}

func TestPlan_LLMErrorPropagates(t *testing.T) {
	llm := &fakeLLM{err: errors.New("api down")}
	_, err := Plan(context.Background(), "do something", nil, safetygate.DefaultConfig(), Config{MaxAgents: 5}, llm)
	if err == nil || !strings.Contains(err.Error(), "api down") {
		t.Fatalf("expected wrapped api error, got %v", err)
	}
}

func TestPlan_RejectsEmptyRequest(t *testing.T) {
	_, err := Plan(context.Background(), "   ", nil, safetygate.DefaultConfig(), Config{MaxAgents: 5}, &fakeLLM{})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-request error, got %v", err)
	}
}

func TestPlan_RejectsNilLLM(t *testing.T) {
	_, err := Plan(context.Background(), "do something", nil, safetygate.DefaultConfig(), Config{MaxAgents: 5}, nil)
	if err == nil || !strings.Contains(err.Error(), "no LLM") {
		t.Fatalf("expected nil-llm error, got %v", err)
	}
}

// TestPlan_ContextIncludesProtectedPaths verifies the user message
// surfaces gate config so the LLM doesn't propose touching protected
// files. Sampling the message body is enough.
func TestPlan_ContextIncludesProtectedPaths(t *testing.T) {
	llm := &fakeLLM{response: `{"summary":"x","agents":[{"name":"a","prompt":"p"}]}`}
	cfg := safetygate.Config{Protected: []string{"pkg/auth/**", "pkg/billing/**"}}
	_, err := Plan(context.Background(), "fix stuff", nil, cfg, Config{MaxAgents: 5}, llm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(llm.user, "pkg/auth/**") || !strings.Contains(llm.user, "pkg/billing/**") {
		t.Errorf("protected paths missing from prompt:\n%s", llm.user)
	}
}

// TestPlan_ResolvesFilesFromGraph verifies substring matching against
// graph file nodes. Uses a fake graph with three known files.
func TestPlan_ResolvesFilesFromGraph(t *testing.T) {
	g := newFakeGraph(map[string][]string{
		"middleware/ratelimit.go": nil,
		"router.go":               nil,
		"unrelated.go":            nil,
	})
	llm := &fakeLLM{response: `{"summary":"x","agents":[{"name":"a","prompt":"p"}]}`}
	_, err := Plan(context.Background(), "add a rate limiter to router.go", g, safetygate.DefaultConfig(), Config{MaxAgents: 5}, llm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(llm.user, "router.go") {
		t.Errorf("router.go not surfaced in prompt:\n%s", llm.user)
	}
	if strings.Contains(llm.user, "unrelated.go") {
		t.Errorf("unrelated.go should not appear:\n%s", llm.user)
	}
}

// TestChat_OneShotReply confirms Chat sends a single completion with
// the chat system prompt and returns the trimmed response.
func TestChat_OneShotReply(t *testing.T) {
	llm := &fakeLLM{response: "  hello there!  "}
	got, err := Chat(context.Background(), "hi", Config{}, llm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello there!" {
		t.Errorf("response not trimmed: %q", got)
	}
	if !strings.Contains(llm.system, "fallback") || !strings.Contains(llm.system, "kai CLI") {
		t.Errorf("chat system prompt missing key phrases:\n%s", llm.system)
	}
	if llm.user != "hi" {
		t.Errorf("user message: %q", llm.user)
	}
}

func TestChat_RejectsEmpty(t *testing.T) {
	_, err := Chat(context.Background(), "   ", Config{}, &fakeLLM{})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-request error, got %v", err)
	}
}

func TestChat_RejectsNilLLM(t *testing.T) {
	_, err := Chat(context.Background(), "hi", Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no LLM") {
		t.Fatalf("expected nil-llm error, got %v", err)
	}
}

func TestReplan_AppendsFeedback(t *testing.T) {
	llm := &fakeLLM{response: `{"summary":"x","agents":[{"name":"a","prompt":"p"}]}`}
	_, err := Replan(context.Background(), "original request", "actually use middleware", nil,
		safetygate.DefaultConfig(), Config{MaxAgents: 5}, llm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(llm.user, "original request") || !strings.Contains(llm.user, "actually use middleware") {
		t.Errorf("replan didn't combine inputs:\n%s", llm.user)
	}
}

func contains(xs []string, sub string) bool {
	for _, x := range xs {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

// fakeGraph is a tiny in-memory GraphAccess. Files keyed by path map
// to their inbound callers (used by neighborMap).
type fakeGraph struct {
	files   []*graph.Node
	callers map[string][]string
}

func newFakeGraph(callers map[string][]string) *fakeGraph {
	files := make([]*graph.Node, 0, len(callers))
	for path := range callers {
		files = append(files, &graph.Node{
			ID:      []byte(path),
			Kind:    graph.KindFile,
			Payload: map[string]interface{}{"path": path},
		})
	}
	return &fakeGraph{files: files, callers: callers}
}

func (f *fakeGraph) GetNodesByKind(kind graph.NodeKind) ([]*graph.Node, error) {
	if kind != graph.KindFile {
		return nil, nil
	}
	return f.files, nil
}

func (f *fakeGraph) GetEdgesToByPath(p string, _ graph.EdgeType) ([]*graph.Edge, error) {
	srcs := f.callers[p]
	out := make([]*graph.Edge, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, &graph.Edge{Src: []byte(s)})
	}
	return out, nil
}

func (f *fakeGraph) GetNode(id []byte) (*graph.Node, error) {
	return &graph.Node{
		ID:      id,
		Kind:    graph.KindFile,
		Payload: map[string]interface{}{"path": string(id)},
	}, nil
}

func (f *fakeGraph) GetEdgesOfType(_ graph.EdgeType) ([]*graph.Edge, error) {
	return nil, nil
}

// symGraph is a fakeGraph variant that returns DEFINES_IN edges so we
// can exercise the symbol-aware resolution path. Built around a
// symbol-name → file-path map; everything else (file-kind enum,
// substring match) shares the parent fake's behavior.
type symGraph struct {
	*fakeGraph
	defines map[string]string // fqName -> defining file path
}

func newSymGraph(defines map[string]string) *symGraph {
	files := make([]*graph.Node, 0, len(defines))
	seenFile := make(map[string]bool)
	for _, p := range defines {
		if seenFile[p] {
			continue
		}
		seenFile[p] = true
		files = append(files, &graph.Node{
			ID:      []byte(p),
			Kind:    graph.KindFile,
			Payload: map[string]interface{}{"path": p},
		})
	}
	return &symGraph{
		fakeGraph: &fakeGraph{files: files, callers: nil},
		defines:   defines,
	}
}

func (s *symGraph) GetEdgesOfType(t graph.EdgeType) ([]*graph.Edge, error) {
	if t != graph.EdgeDefinesIn {
		return nil, nil
	}
	out := make([]*graph.Edge, 0, len(s.defines))
	for fq, path := range s.defines {
		out = append(out, &graph.Edge{
			Src: []byte("sym:" + fq),
			Dst: []byte(path),
		})
	}
	return out, nil
}

func (s *symGraph) GetNode(id []byte) (*graph.Node, error) {
	sid := string(id)
	if strings.HasPrefix(sid, "sym:") {
		return &graph.Node{
			ID:      id,
			Kind:    graph.KindSymbol,
			Payload: map[string]interface{}{"fqName": sid[len("sym:"):]},
		}, nil
	}
	return &graph.Node{
		ID:      id,
		Kind:    graph.KindFile,
		Payload: map[string]interface{}{"path": sid},
	}, nil
}

// TestResolveFiles_SymbolAware exercises the path that fixes the
// "fix parseFoo" → wrong file problem. The request mentions a symbol
// (parseConfig) that doesn't appear in any file path; substring match
// returns nothing. The symbol pass should resolve it to its defining
// file via DEFINES_IN.
func TestResolveFiles_SymbolAware(t *testing.T) {
	g := newSymGraph(map[string]string{
		"parseConfig":     "internal/config/parser.go",
		"auth.Login":      "internal/auth/handler.go",
		"unrelatedHelper": "internal/util/helpers.go",
	})

	got := resolveFiles("the parseConfig function is broken", g)
	if !contains(got, "internal/config/parser.go") {
		t.Errorf("expected parser.go in resolved files, got %v", got)
	}
	if contains(got, "internal/util/helpers.go") {
		t.Errorf("unrelatedHelper should not have matched: %v", got)
	}
}

// TestResolveFiles_SymbolTrailing covers the qualified-name case:
// the user types "Login" and the graph has "auth.Login". The
// trailing-component match should still resolve.
func TestResolveFiles_SymbolTrailing(t *testing.T) {
	g := newSymGraph(map[string]string{
		"auth.Login":  "internal/auth/handler.go",
		"db.Login":    "internal/db/session.go",
	})
	got := resolveFiles("fix the Login flow", g)
	if !contains(got, "internal/auth/handler.go") {
		t.Errorf("auth.Login → handler.go missing: %v", got)
	}
	if !contains(got, "internal/db/session.go") {
		t.Errorf("db.Login → session.go missing: %v", got)
	}
}

// TestResolveFiles_StopwordsIgnored confirms common English words
// don't flood the symbol scan. None of "the", "is", "function" should
// produce a hit even if a file path coincidentally contains them.
func TestResolveFiles_StopwordsIgnored(t *testing.T) {
	g := newSymGraph(map[string]string{
		"the":      "the.go",
		"function": "function.go",
		"realSym":  "real.go",
	})
	got := resolveFiles("fix the function", g)
	// "the" and "function" are stopwords — must not resolve via symbol.
	// They might still match "the.go" / "function.go" via the substring
	// pass (file path contains the literal word), which is fine — that's
	// pass 2's job. The symbol pass specifically should skip them.
	syms := resolveSymbolFiles("fix the function", g)
	if len(syms) != 0 {
		t.Errorf("stopwords leaked into symbol resolution: %v", syms)
	}
	_ = got
}

