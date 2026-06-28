package agent

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

func TestClassifyTurnSearchVsBash(t *testing.T) {
	tests := []struct {
		name        string
		calls       []message.ToolCall
		wantBash    bool
		wantSearch  bool
	}{
		{
			name:        "empty turn",
			calls:       nil,
			wantBash:    false,
			wantSearch:  false,
		},
		{
			name:       "kai_grep alone",
			calls:      []message.ToolCall{{Name: "kai_grep"}},
			wantSearch: true,
		},
		{
			name:       "kai_search alone",
			calls:      []message.ToolCall{{Name: "kai_search"}},
			wantSearch: true,
		},
		{
			name:       "kai_files alone",
			calls:      []message.ToolCall{{Name: "kai_files"}},
			wantSearch: true,
		},
		{
			name:     "verification bash counts (real CLI invocation)",
			calls:    []message.ToolCall{{Name: "bash", Input: `{"command": "kai stats --json"}`}},
			wantBash: true,
		},
		{
			name: "hygiene bash does NOT count (which kai)",
			calls: []message.ToolCall{
				{Name: "bash", Input: `{"command": "which kai 2>/dev/null || echo not found"}`},
			},
			wantBash: false,
		},
		{
			name: "hygiene bash does NOT count (pwd)",
			calls: []message.ToolCall{
				{Name: "bash", Input: `{"command": "pwd"}`},
			},
			wantBash: false,
		},
		{
			name: "cd + verification → counts (cd-stripped to kai invocation)",
			calls: []message.ToolCall{
				{Name: "bash", Input: `{"command": "cd /Users/foo/kai-desktop && kai snapshot list --json"}`},
			},
			wantBash: true,
		},
		{
			name: "mixed in same turn — verification bash present resets streak",
			calls: []message.ToolCall{
				{Name: "kai_grep"},
				{Name: "bash", Input: `{"command": "kai --help"}`},
			},
			wantBash:   true,
			wantSearch: true,
		},
		{
			name: "mixed turn with hygiene bash — search still counts, bash does not",
			calls: []message.ToolCall{
				{Name: "kai_grep"},
				{Name: "bash", Input: `{"command": "ls -la"}`},
			},
			wantBash:   false,
			wantSearch: true,
		},
		{
			name:       "view does not count as search",
			calls:      []message.ToolCall{{Name: "view"}},
			wantSearch: false,
		},
		{
			name:       "edit/write do not count",
			calls:      []message.ToolCall{{Name: "write"}, {Name: "edit"}},
			wantSearch: false,
			wantBash:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotBash, gotSearch := classifyTurnSearchVsBash(tc.calls)
			if gotBash != tc.wantBash || gotSearch != tc.wantSearch {
				t.Errorf("classifyTurnSearchVsBash() = (bash=%v, search=%v), want (bash=%v, search=%v)",
					gotBash, gotSearch, tc.wantBash, tc.wantSearch)
			}
		})
	}
}

// TestBashIsVerification pins which bash invocations count as
// verification (resetting the search-without-bash streak) and which
// are shell hygiene (don't reset). The 2026-05-26 edges dogfood
// gamed the naive "any bash resets" rule with `which kai`; the new
// classifier excludes shell-hygiene leading commands.
func TestBashIsVerification(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		// Verification — real CLI invocations / probes.
		{"kai stats --json", `{"command": "kai stats --json"}`, true},
		{"kai snapshot list --help", `{"command": "kai snapshot --help"}`, true},
		{"npm test", `{"command": "npm test"}`, true},
		{"go build", `{"command": "go build ./..."}`, true},
		{"cd then real cmd", `{"command": "cd /foo && kai stats --json"}`, true},
		{"cd then real cmd, semicolon", `{"command": "cd /foo; npm run dev"}`, true},
		{"git status", `{"command": "git status"}`, true},

		// Hygiene — should NOT count.
		{"which kai", `{"command": "which kai"}`, false},
		{"which kai with shell-or fallback", `{"command": "which kai 2>/dev/null || echo not found"}`, false},
		{"pwd", `{"command": "pwd"}`, false},
		{"ls -la", `{"command": "ls -la"}`, false},
		{"cat file", `{"command": "cat README.md"}`, false},
		{"echo hello", `{"command": "echo hello"}`, false},
		{"cd alone", `{"command": "cd /foo"}`, false},
		{"absolute path which", `{"command": "/usr/bin/which kai"}`, false},

		// Edge cases.
		{"empty input", ``, false},
		{"unparseable JSON", `not json`, false},
		{"empty command", `{"command": ""}`, false},
		{"whitespace-only command", `{"command": "   "}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bashIsVerification(c.input); got != c.want {
				t.Errorf("bashIsVerification(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

// TestSearchWithoutBashNudgeText pins the user-visible nudge shape.
// The dogfood-citation anchor and the "RUN THE COMMAND" imperative
// are both load-bearing — they're what the model reads to decide
// whether to change behavior.
func TestSearchWithoutBashNudgeText(t *testing.T) {
	out := searchWithoutBashNudgeText(5)
	for _, want := range []string{
		"5+",
		"kai_grep",
		"kai_search",
		"kai_files",
		"INVOKE THE COMMAND",
		"kai stats --json",
		"bundle",
		"snake_case",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nudge text missing %q; got:\n%s", want, out)
		}
	}
}
