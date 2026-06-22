package orchestrator

import (
	"strings"
	"testing"
)

func TestSummarizeToolCall_PerTool(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"view", `{"file_path":"main.go"}`, "[view] main.go @0:2000"},
		{"view", `{"file_path":"main.go","offset":3700,"limit":180}`, "[view] main.go @3700:3880"},
		{"view", `{"file_path":"main.go","offset":40,"limit":30}`, "[view] main.go @40:70"},
		{"write", `{"file_path":"cmd/kai/main.go"}`, "[write] cmd/kai/main.go"},
		{"edit", `{"file_path":"main.go"}`, "[edit] main.go"},
		{"bash", `{"command":"go build ./..."}`, `[bash] go build ./...`},
		{"bash", `{"command":"cd kai-cli && go test"}`, `[bash] cd kai-cli && go test`},
		{"kai_grep", `{"pattern":"versionCmd"}`, `[kai_grep] "versionCmd"`},
		{"kai_callers", `{"symbol":"renderPlanMenu"}`, "[kai_callers] renderPlanMenu"},
		{"kai_context", `{"file_path":"orchestrator.go"}`, "[kai_context] orchestrator.go"},
		{"kai_files", `{"dir":"kai-cli/internal"}`, "[kai_files] kai-cli/internal"},
		{"kai_checkpoint", `{"file":"main.go","start_line":100,"end_line":120}`, "[kai_checkpoint] main.go:100-120"},
	}
	for _, c := range cases {
		t.Run(c.name+"_"+c.input, func(t *testing.T) {
			got := summarizeToolCall(c.name, c.input)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSummarizeToolCall_FallsBackToBareName(t *testing.T) {
	if got := summarizeToolCall("unknown_tool", `{"foo":"bar"}`); got != "[unknown_tool]" {
		t.Errorf("unknown tool should return bracketed bare name, got %q", got)
	}
	if got := summarizeToolCall("view", ``); got != "[view]" {
		t.Errorf("empty input should return bracketed bare name, got %q", got)
	}
	if got := summarizeToolCall("view", `not-json`); got != "[view]" {
		t.Errorf("invalid JSON should return bracketed bare name, got %q", got)
	}
}

func TestSummarizeToolCall_TruncatesLongArgs(t *testing.T) {
	// Use bash so the truncation happens at end of line (bash has no
	// trailing suffix). For view the truncated path now sits inside
	// the line with the @offset:limit range appended, so the path
	// itself is mid-string when truncated — covered separately below.
	long := strings.Repeat("a", 100)
	got := summarizeToolCall("bash", `{"command":"`+long+`"}`)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long bash command should end with ellipsis, got %q", got)
	}
	if len(got) > summaryArgCap+10 {
		t.Errorf("truncated summary should fit in cap, got len=%d", len(got))
	}
}

func TestSummarizeToolCall_LongViewPathStillEllipsizedBeforeRange(t *testing.T) {
	// View's always-on range suffix means the truncation lands
	// before the @offset:limit, not at end of line. Verify the path
	// portion is still bounded and the range is still present.
	long := strings.Repeat("a", 100)
	got := summarizeToolCall("view", `{"file_path":"`+long+`"}`)
	if !strings.Contains(got, "…") {
		t.Errorf("long view path should contain truncation marker, got %q", got)
	}
	if !strings.HasSuffix(got, "@0:2000") {
		t.Errorf("view summary should still end with the @offset:limit range, got %q", got)
	}
}
