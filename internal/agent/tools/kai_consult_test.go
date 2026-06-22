package tools

import (
	"context"
	"strings"
	"testing"
)

// fakeConsultSender records the request it received and returns a
// canned reply. Lets us assert what the tool sends to the strong
// model without standing up a real provider.
type fakeConsultSender struct {
	got   SenderRequest
	reply string
}

func (f *fakeConsultSender) Send(ctx context.Context, req SenderRequest) (SenderResponse, error) {
	f.got = req
	return SenderResponse{Text: f.reply}, nil
}

func TestKaiConsult_RejectsMissingFields(t *testing.T) {
	tool := &kaiConsultTool{
		provider: &fakeConsultSender{reply: "DIAGNOSIS: x"},
		model:    "claude-sonnet-4-6",
	}
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"missing goal", `{"tried":["x"],"blocked_by":"y"}`, "goal required"},
		{"missing blocked_by", `{"goal":"g","tried":["x"]}`, "blocked_by required"},
		{"empty tried", `{"goal":"g","blocked_by":"y","tried":[]}`, "tried required"},
		{"invalid json", `{not-json`, "invalid input json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := tool.Run(context.Background(), ToolCall{Input: tc.input})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !r.IsError {
				t.Fatalf("want IsError=true, got false (content=%q)", r.Content)
			}
			if !strings.Contains(r.Content, tc.want) {
				t.Errorf("error message %q does not contain %q", r.Content, tc.want)
			}
		})
	}
}

func TestKaiConsult_BuildsPromptAndReturnsReply(t *testing.T) {
	fake := &fakeConsultSender{reply: "DIAGNOSIS: wrong CWD\nFILES: cmd/kai/main.go\nACTIONS:\n1. cd to repo root"}
	tool := &kaiConsultTool{
		provider:  fake,
		model:     "claude-sonnet-4-6",
		workspace: "/tmp/some/repo",
		mode:      "coding",
	}
	input := `{"goal":"add config show command","tried":["kai_grep configCmd → 0 hits","view cmd/kai/main.go → no config command"],"blocked_by":"can't find where to wire cobra subcommands"}`
	r, err := tool.Run(context.Background(), ToolCall{Input: input})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.IsError {
		t.Fatalf("got error response: %s", r.Content)
	}
	// Reply was passed through.
	if !strings.Contains(r.Content, "DIAGNOSIS: wrong CWD") {
		t.Errorf("reply missing from tool output: %q", r.Content)
	}
	// Model id is in the header so the agent (and dogfood logs) can
	// see which strong model produced the diagnosis.
	if !strings.Contains(r.Content, "claude-sonnet-4-6") {
		t.Errorf("model id missing from header: %q", r.Content)
	}
	// Runner-supplied context (workspace, mode) should be in the
	// prompt the strong model received — that's the whole point of
	// slice 1.5 over the agent-only fields.
	if !strings.Contains(fake.got.UserText, "/tmp/some/repo") {
		t.Errorf("workspace not in prompt: %q", fake.got.UserText)
	}
	if !strings.Contains(fake.got.UserText, "coding") {
		t.Errorf("mode not in prompt: %q", fake.got.UserText)
	}
	// Agent-supplied fields preserved verbatim.
	if !strings.Contains(fake.got.UserText, "add config show command") {
		t.Errorf("goal not in prompt: %q", fake.got.UserText)
	}
	if !strings.Contains(fake.got.UserText, "configCmd → 0 hits") {
		t.Errorf("tried not in prompt: %q", fake.got.UserText)
	}
	// The system prompt enforces the diagnosis-only contract.
	if !strings.Contains(fake.got.System, "DO NOT write code") {
		t.Errorf("system prompt missing diagnosis-only constraint: %q", fake.got.System)
	}
}

func TestKaiConsult_NoProvider(t *testing.T) {
	tool := &kaiConsultTool{model: "claude-sonnet-4-6"}
	r, err := tool.Run(context.Background(), ToolCall{Input: `{"goal":"g","tried":["x"],"blocked_by":"y"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.IsError || !strings.Contains(r.Content, "no consult provider") {
		t.Errorf("expected nil-provider error, got: %q", r.Content)
	}
}
