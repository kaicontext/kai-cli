package freeplan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serverDefaults has the shape of the server's per-role defaults, with
// placeholder ids.
var serverDefaults = map[string]string{
	"chat": "free/model-fast", "robot": "free/model-strong",
	"planner": "free/model-strong", "coder": "free/model-strong",
	"agent": "free/model-strong", "jeff": "free/model-fast",
	"images": "free/model-fast", "titles": "free/model-small",
}

// NewServer is a fake kai-server model list: with free and defaults only
// when free, as kai-server's freeCatalog sends them.
func NewServer(t *testing.T, free bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/llm/models" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		out := map[string]any{"data": []map[string]string{{"id": "anthropic/claude-opus-5"}, {"id": "z-ai/glm-5.1"}}}
		if free {
			out = map[string]any{"data": []map[string]string{{"id": "free/model-fast"}, {"id": "free/model-strong"}},
				"free": true, "defaults": serverDefaults}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch(t *testing.T) {
	if p := Fetch(context.Background(), NewServer(t, false).URL, "tok"); p.Free || p.Default("chat") != "" || !p.Allows("anthropic/claude-opus-5") {
		t.Fatalf("no free key read as Free: %+v", p)
	}
	p := Fetch(context.Background(), NewServer(t, true).URL, "tok")
	if !p.Free || p.Default("chat") != "free/model-fast" || p.Default("titles") != "free/model-small" {
		t.Fatalf("Free list: %+v", p)
	}
	if p.Default("review") != serverDefaults[CatchAll] {
		t.Fatalf("a role with no key takes %s's: %q", CatchAll, p.Default("review"))
	}
	for m, want := range map[string]bool{
		"free/model-fast": true, "free/model-strong:reasoning": true,
		"free/model-small":        true, // named as a default
		"anthropic/claude-opus-5": false, "free/model-not-listed": false,
	} {
		if p.Allows(m) != want {
			t.Errorf("Allows(%s) = %v", m, !want)
		}
	}
	for _, bad := range []string{"", "http://127.0.0.1:1"} {
		if p := Fetch(context.Background(), bad, "tok"); p.Free {
			t.Errorf("unreachable %q read as Free", bad)
		}
	}
}

func TestResolveAndCheck(t *testing.T) {
	srv := NewServer(t, true)
	p := Fetch(context.Background(), srv.URL, "tok")
	if m := p.Resolve("planner", "", "deepseek/deepseek-v4-pro"); m != "free/model-strong" {
		t.Errorf("unset role: %s", m)
	}
	if m := p.Resolve("planner", "anthropic/claude-opus-5", "deepseek/deepseek-v4-pro"); m != "anthropic/claude-opus-5" {
		t.Errorf("the person's model lost: %s", m)
	}
	if m := (Plan{}).Resolve("planner", "", "deepseek/deepseek-v4-pro"); m != "deepseek/deepseek-v4-pro" {
		t.Errorf("off Free: %s", m)
	}
	want := "anthropic/claude-opus-5 isn't available on the Free plan. Choose another model, or upgrade: " + srv.URL + "/billing."
	if err := p.Check([]string{"free/model-fast", "anthropic/claude-opus-5", "z-ai/glm-5.1"}); err == nil || err.Error() != want {
		t.Errorf("check: %v", err)
	}
	if err := p.Check([]string{"free/model-fast", "free/model-small"}); err != nil {
		t.Errorf("included models refused: %v", err)
	}
	if err := (Plan{}).Check([]string{"anthropic/claude-opus-5"}); err != nil {
		t.Errorf("refused off Free: %v", err)
	}
}
