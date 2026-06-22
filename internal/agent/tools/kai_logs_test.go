package tools

import (
	"context"
	"strings"
	"testing"
)

// stubLogger implements ManagedProcLogger for tests.
type stubLogger struct {
	cmd     string
	output  string
	running bool
}

func (s *stubLogger) RecentLogs() (string, string, bool) {
	return s.cmd, s.output, s.running
}

func TestKaiLogs_NoProcessRunning(t *testing.T) {
	tool := &kaiLogsTool{logger: &stubLogger{running: false}}
	resp, err := tool.Run(context.Background(), ToolCall{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "No managed process is currently running") {
		t.Errorf("expected no-process message, got: %s", resp.Content)
	}
}

func TestKaiLogs_ReturnsRecentOutput(t *testing.T) {
	tool := &kaiLogsTool{logger: &stubLogger{
		cmd:     "npm run dev",
		output:  "line1\nline2\nline3\nline4\n",
		running: true,
	}}
	resp, err := tool.Run(context.Background(), ToolCall{Input: `{"lines": 2}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "npm run dev") {
		t.Errorf("expected command in response, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "line4") {
		t.Errorf("expected newest line, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "line3") {
		t.Errorf("expected second-newest line, got: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "line1") {
		t.Errorf("did not expect oldest line with lines=2, got: %s", resp.Content)
	}
}

func TestKaiLogs_NotConfigured(t *testing.T) {
	tool := &kaiLogsTool{logger: nil}
	resp, err := tool.Run(context.Background(), ToolCall{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "not configured") {
		t.Errorf("expected not-configured message, got: %s", resp.Content)
	}
	if !resp.IsError {
		t.Errorf("expected IsError=true, got false")
	}
}

func TestKaiLogs_InvalidJSON(t *testing.T) {
	tool := &kaiLogsTool{logger: &stubLogger{running: true}}
	resp, err := tool.Run(context.Background(), ToolCall{Input: `{not valid}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError on bad input")
	}
}

func TestTailLines_KeepsLastN(t *testing.T) {
	in := "a\nb\nc\nd\ne\n"
	got := tailLines(in, 3)
	if got != "c\nd\ne" {
		t.Errorf("expected last 3 lines, got %q", got)
	}
}

func TestTailLines_NSkipsEmpty(t *testing.T) {
	in := "a\n\nb\n\nc\n"
	got := tailLines(in, 2)
	// Walks back counting non-empty: c, b → stops at b's index.
	// Result includes the empty line between b and c.
	if !strings.Contains(got, "b") || !strings.Contains(got, "c") {
		t.Errorf("expected last 2 non-empty lines preserved, got %q", got)
	}
}

func TestTailLines_ZeroReturnsAll(t *testing.T) {
	in := "a\nb\nc"
	got := tailLines(in, 0)
	if got != in {
		t.Errorf("n=0 should return unchanged, got %q", got)
	}
}
