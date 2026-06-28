package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/projects"
)

// TestKaiGrep_AutoPromotesAlternationOnZeroHits pins the 2026-05-20
// fix: when a kai_grep query contains regex metacharacters AND the
// literal search returns zero matches AND the regex would compile,
// the tool auto-retries as a regex and notes the promotion in the
// result. This catches the "agent typed `foo|bar` expecting
// alternation, got `no matches`, concluded the feature doesn't
// exist" failure mode observed in the 2026-05-20 review-mode trace.
func TestKaiGrep_AutoPromotesAlternationOnZeroHits(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("brave search\nBRAVE_API_KEY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &kaiGrepTool{workspace: ws}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_grep",
		Input: `{"query":"brave|BRAVE"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	// Both "brave" and "BRAVE" lines should match.
	for _, want := range []string{"brave search", "BRAVE_API_KEY"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("expected match %q in output, got: %s", want, resp.Content)
		}
	}
	// And the response should tell the agent what happened so it
	// learns to pass regex=true explicitly next time.
	if !strings.Contains(resp.Content, "auto-promoted to regex") {
		t.Errorf("expected auto-promotion note in output, got: %s", resp.Content)
	}
}

// TestKaiGrep_LiteralStaysLiteralWhenNoMetachars confirms that
// queries WITHOUT regex metacharacters don't get the auto-promote
// treatment — a plain word that genuinely doesn't exist still
// reports "no matches" without invoking the retry path.
func TestKaiGrep_LiteralStaysLiteralWhenNoMetachars(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &kaiGrepTool{workspace: ws}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_grep",
		Input: `{"query":"nonexistent"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, `no matches for "nonexistent"`) {
		t.Errorf("expected no-matches response, got: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "auto-promoted") {
		t.Errorf("plain word should not trigger auto-promote, got: %s", resp.Content)
	}
}

// TestKaiGrep_InvalidRegexStaysLiteral confirms that when a literal
// query happens to contain a regex metacharacter but the resulting
// regex is invalid (e.g. unmatched paren), the auto-promote silently
// fails and we still return the literal "no matches" — never break
// the response shape on a query the agent might have meant literally.
func TestKaiGrep_InvalidRegexStaysLiteral(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &kaiGrepTool{workspace: ws}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_grep",
		Input: `{"query":"unclosed("}`, // valid literal, invalid regex
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "no matches") {
		t.Errorf("invalid-regex literal should produce no-matches, got: %s", resp.Content)
	}
}

// TestKaiGrep_ExplicitRegexUnchanged confirms that callers who
// explicitly set regex=true continue to work — the auto-promote
// path doesn't double-process or alter explicit regex queries.
func TestKaiGrep_ExplicitRegexUnchanged(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("brave\nBRAVE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &kaiGrepTool{workspace: ws}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "kai_grep",
		Input: `{"query":"brave|BRAVE","regex":true}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	// Both matches present, no auto-promotion note (we didn't auto-promote;
	// the user passed regex=true explicitly).
	if !strings.Contains(resp.Content, "brave") || !strings.Contains(resp.Content, "BRAVE") {
		t.Errorf("explicit regex should match both alternations, got: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "auto-promoted") {
		t.Errorf("explicit regex=true should not produce an auto-promote note, got: %s", resp.Content)
	}
}

var _ = projects.Set{} // touch import so test file compiles independently
