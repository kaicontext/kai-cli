package agent

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

// TestExtractFileMentions covers the regex's coverage AND its
// false-positive guard rails. The detector must catch backticked
// and bare filenames with recognized extensions, dedupe across
// multiple mentions, and reject bare prose words that happen to
// look filename-shaped.
func TestExtractFileMentions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "kai-tui case: backticked node files",
			in:   "It looks like you have an `index.js` and `package.json` here",
			want: []string{"index.js", "package.json"},
		},
		{
			name: "bare go files",
			in:   "Edit runner.go and agent.go then rerun.",
			want: []string{"runner.go", "agent.go"},
		},
		{
			name: "paths with dirs",
			in:   "See `internal/agent/runner.go` line 42",
			want: []string{"internal/agent/runner.go"},
		},
		{
			name: "dedupes across multiple mentions",
			in:   "edit `foo.go`, run go vet `foo.go`, then commit",
			want: []string{"foo.go"},
		},
		{
			name: "no extension = no match (avoids false positives)",
			in:   "Look at Makefile, Dockerfile, README to see the layout",
			want: nil,
		},
		{
			name: "casual prose with no files = no match",
			in:   "I didn't run that command. Is there something I can help with?",
			want: nil,
		},
		{
			name: "yaml + toml configs caught",
			in:   "kai.projects.yaml controls the workspace; `pyproject.toml` is python's.",
			want: []string{"kai.projects.yaml", "pyproject.toml"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractFileMentions(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestFabricatedFileMentions_KaiTuiRepro is the regression for the
// exact 2026-05-12 failure: opus replies "you have an index.js and
// package.json here" in the kai monorepo, having never observed
// either file in any tool result. The guard MUST flag both as
// fabricated.
func TestFabricatedFileMentions_KaiTuiRepro(t *testing.T) {
	history := []message.Message{
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Multi-root workspace: kai, kai-server, kai-e2e, kai-tui. The kai monorepo at /Users/x/projects/kai/kai."},
			},
		},
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.TextContent{Text: "why did you try to do kai checkout @snap:last~0"},
			},
		},
	}
	mentions := ExtractFileMentions("It looks like you have an `index.js` and `package.json` here")
	got := FabricatedFileMentions(mentions, history)
	if len(got) != 2 {
		t.Fatalf("expected both files flagged, got %v", got)
	}
}

// TestFabricatedFileMentions_GroundedInToolResult: when the agent
// names a file that was visible in a prior tool result, the guard
// MUST NOT fire. The agent has earned the right to mention it.
func TestFabricatedFileMentions_GroundedInToolResult(t *testing.T) {
	history := []message.Message{
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "1", Name: "kai_tree", Input: `{"path":""}`},
			},
		},
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "1", Content: "  internal/agent/runner.go\n  internal/agent/agent.go\n"},
			},
		},
	}
	mentions := ExtractFileMentions("The fix lives in `internal/agent/runner.go`.")
	got := FabricatedFileMentions(mentions, history)
	if len(got) != 0 {
		t.Errorf("file appeared in tool result; guard must not fire, got %v", got)
	}
}

// TestFabricatedFileMentions_GroundedInProjectOverview: the auto-
// injected project overview lists top-level files. Mentioning one
// is grounded — even though no tool was explicitly called this
// turn. The overview lives in the user-message stream.
func TestFabricatedFileMentions_GroundedInProjectOverview(t *testing.T) {
	history := []message.Message{
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.TextContent{Text: "[runner: Project overview]\n  package.json\n  index.js\n  src/handler.ts\n"},
			},
		},
	}
	mentions := ExtractFileMentions("You have `index.js` and `package.json` at the root.")
	got := FabricatedFileMentions(mentions, history)
	if len(got) != 0 {
		t.Errorf("files appeared in project overview; guard must not fire, got %v", got)
	}
}

// TestFabricatedFileMentions_PartialFabrication: model mentions
// three files; two are real (in tool results), one is invented.
// The guard reports only the invented one — minimizing the nudge's
// scope helps the model take just the right correction.
func TestFabricatedFileMentions_PartialFabrication(t *testing.T) {
	history := []message.Message{
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "1", Content: "  cmd/main.go\n  internal/runner.go\n"},
			},
		},
	}
	mentions := ExtractFileMentions("Look at `cmd/main.go`, `internal/runner.go`, and `internal/sidecar.go`.")
	got := FabricatedFileMentions(mentions, history)
	if len(got) != 1 || got[0] != "internal/sidecar.go" {
		t.Errorf("expected only the invented file; got %v", got)
	}
}

// TestFormatHallucinationNudge ensures the nudge body actually
// contains the offending filenames so the model knows what to
// retract. Without the substitution the model would get a generic
// scolding it might dismiss.
func TestFormatHallucinationNudge(t *testing.T) {
	msg := formatHallucinationNudge([]string{"index.js", "package.json"})
	for _, want := range []string{"`index.js`", "`package.json`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("nudge missing %q: %s", want, msg)
		}
	}
}
