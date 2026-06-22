package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/agent/message"
	"kai/internal/agent/provider"
	"kai/internal/graph"
)

func TestDetectMode_SlashOverrides(t *testing.T) {
	cases := map[string]Mode{
		"/code":              ModeCoding,
		"/code now please":   ModeCoding,
		"/debug":             ModeDebug,
		"/review":            ModeReview,
		"/plan refactor api": ModePlanning,
		"/chat":              ModeConversation,
	}
	for in, want := range cases {
		if got := DetectMode(in, ModeReview); got != want {
			t.Errorf("DetectMode(%q, Review) = %v, want %v", in, got, want)
		}
	}
}

func TestDetectMode_ErrorSignaturesBeatPlanningKeywords(t *testing.T) {
	in := "plan a fix for this panic: nil pointer at auth.go:47"
	if got := DetectMode(in, ModeUnknown); got != ModeDebug {
		t.Errorf("got %v, want Debug (error signatures outrank planning)", got)
	}
}

func TestDetectMode_PlanningKeywords(t *testing.T) {
	in := "how would you break down the auth rewrite"
	if got := DetectMode(in, ModeUnknown); got != ModePlanning {
		t.Errorf("got %v, want Planning", got)
	}
}

func TestDetectMode_StickyDebugKeepsClarifyingQuestions(t *testing.T) {
	in := "what does that error mean"
	if got := DetectMode(in, ModeDebug); got != ModeDebug {
		t.Errorf("got %v, want Debug (soft Conversation must yield to sticky Debug)", got)
	}
}

func TestDetectMode_StickyPlanningSwitchesOnAnotherSticky(t *testing.T) {
	in := "go ahead, ship it"
	if got := DetectMode(in, ModePlanning); got != ModeCoding {
		t.Errorf("got %v, want Coding (Planning → Coding via approval)", got)
	}
}

func TestDetectMode_FirstTurnKeepsSoftDetection(t *testing.T) {
	in := "what does this function do"
	if got := DetectMode(in, ModeUnknown); got != ModeConversation {
		t.Errorf("got %v, want Conversation (Unknown is non-sticky)", got)
	}
}

func TestDetectMode_GoStackTrace(t *testing.T) {
	in := "goroutine 17 [running]:\n  main.handleRequest(...)\n    /app/main.go:42 +0x1b"
	if got := DetectMode(in, ModeUnknown); got != ModeDebug {
		t.Errorf("got %v, want Debug (Go stack trace)", got)
	}
}

func TestDetectMode_FailWordInProseDoesNotTriggerDebug(t *testing.T) {
	in := "I don't want to fail closed here, refactor the handler"
	if got := DetectMode(in, ModeUnknown); got == ModeDebug {
		t.Errorf("got Debug, want non-Debug (prose 'fail' should not trigger)")
	}
}

func TestModeIsSticky(t *testing.T) {
	for _, m := range []Mode{ModeCoding, ModePlanning, ModeDebug} {
		if !m.IsSticky() {
			t.Errorf("%v should be sticky", m)
		}
	}
	for _, m := range []Mode{ModeConversation, ModeReview, ModeUnknown} {
		if m.IsSticky() {
			t.Errorf("%v should be soft", m)
		}
	}
}

func TestResolveMode(t *testing.T) {
	if ResolveMode(ModeUnknown) != ModeCoding {
		t.Errorf("Unknown should resolve to Coding")
	}
	if ResolveMode(ModeDebug) != ModeDebug {
		t.Errorf("Debug should resolve to itself")
	}
}

func TestModeAllowedTools_PlanningCannotEdit(t *testing.T) {
	m := ModePlanning
	// Planning must still block code modification. bash was added
	// to planning in 0.32.50 so the planner can verify external
	// contracts (run "kai stats --json" before assuming a field
	// exists) — that's read-only verification, not modification.
	for _, banned := range []string{"write", "edit"} {
		if m.ToolAllowed(banned) {
			t.Errorf("Planning must not allow %q", banned)
		}
	}
	for _, allowed := range []string{"view", "kai_callers", "kai_context", "bash"} {
		if !m.ToolAllowed(allowed) {
			t.Errorf("Planning must allow %q", allowed)
		}
	}
}

func TestModeAllowedTools_ReviewIsReadOnly(t *testing.T) {
	m := ModeReview
	for _, banned := range []string{"write", "edit", "bash", "kai_checkpoint"} {
		if m.ToolAllowed(banned) {
			t.Errorf("Review must not allow %q", banned)
		}
	}
	if !m.ToolAllowed("kai_diff") {
		t.Errorf("Review must allow kai_diff")
	}
}

func TestModeAllowedTools_DebugHasCheckpoint(t *testing.T) {
	if !ModeDebug.ToolAllowed("kai_checkpoint") {
		t.Errorf("Debug must allow kai_checkpoint (edits during debug need authorship)")
	}
	if !ModeDebug.ToolAllowed("edit") {
		t.Errorf("Debug must allow edit")
	}
}

func TestGraphDepthAndCap(t *testing.T) {
	if ModeDebug.GraphDepth() != 2 {
		t.Errorf("Debug depth must be 2")
	}
	for _, m := range []Mode{ModeCoding, ModePlanning, ModeReview, ModeConversation} {
		if m.GraphDepth() != 1 {
			t.Errorf("%v depth must be 1", m)
		}
	}
	if max, capped := ModeDebug.GraphCap(); !capped || max != 50 {
		t.Errorf("Debug cap = (%d, %v), want (50, true)", max, capped)
	}
	if _, capped := ModeCoding.GraphCap(); capped {
		t.Errorf("Coding must be uncapped")
	}
}

func TestBuildToolRegistry_ModeFiltersTools(t *testing.T) {
	tmp := t.TempDir()
	planning := buildToolRegistry(Options{Workspace: tmp, Mode: ModePlanning, EnableBash: true}, &gateVerdictBag{})
	for _, banned := range []string{"write", "edit"} {
		if _, ok := planning[banned]; ok {
			t.Errorf("Planning registry must not contain %q", banned)
		}
	}
	for _, allowed := range []string{"view", "bash"} {
		if _, ok := planning[allowed]; !ok {
			t.Errorf("Planning registry must contain %q", allowed)
		}
	}

	coding := buildToolRegistry(Options{Workspace: tmp, Mode: ModeCoding, EnableBash: true}, &gateVerdictBag{})
	for _, allowed := range []string{"view", "write", "edit", "bash"} {
		if _, ok := coding[allowed]; !ok {
			t.Errorf("Coding registry must contain %q", allowed)
		}
	}

	// ModeUnknown resolves to Coding — full toolset.
	zero := buildToolRegistry(Options{Workspace: tmp, EnableBash: true}, &gateVerdictBag{})
	if _, ok := zero["bash"]; !ok {
		t.Errorf("Unknown mode must resolve to Coding (bash present)")
	}
}

func TestParseModeRoundTrip(t *testing.T) {
	for _, m := range []Mode{ModeCoding, ModePlanning, ModeReview, ModeDebug, ModeConversation} {
		if got := ParseMode(m.String()); got != m {
			t.Errorf("ParseMode(%q) = %v, want %v", m.String(), got, m)
		}
	}
	if ParseMode("") != ModeUnknown || ParseMode("garbage") != ModeUnknown {
		t.Errorf("empty/unknown strings must parse to ModeUnknown")
	}
}

func TestRunLoop_ModePromptReachesProvider(t *testing.T) {
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "ok"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	_, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "what should we change",
		Provider:  p,
		Mode:      ModePlanning,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(p.last.System, "planning mode") {
		t.Errorf("System role missing planning prompt: %q", p.last.System)
	}
	if !strings.Contains(p.last.System, "Do NOT write code") {
		t.Errorf("System role missing planning constraint: %q", p.last.System)
	}
}

func TestRunLoop_DebugPromptReachesProvider(t *testing.T) {
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "fixed"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	_, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "panic at auth.go:5",
		Provider:  p,
		Mode:      ModeDebug,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(p.last.System, "debug mode") {
		t.Errorf("System role missing debug prompt: %q", p.last.System)
	}
}

func TestRunLoop_PlanningModeBlocksWriteToolCall(t *testing.T) {
	ws := t.TempDir()
	// Model attempts to write; runner should reply with a
	// not-registered error because Planning's whitelist excludes
	// write. The message also lists available tools so the model
	// can pivot — see unknownToolMessage in runner.go for the
	// rationale.
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "c1",
					Name:  "write",
					Input: marshalInput(map[string]interface{}{"file_path": "x.txt", "content": "nope"}),
					Type:  "tool_use",
				},
			},
			FinishReason: message.FinishReasonToolUse,
		},
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "done"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	res, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "plan a write",
		Provider:  p,
		Mode:      ModePlanning,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The tool result should mark write as unknown.
	var foundUnknown bool
	for _, m := range res.Transcript {
		for _, part := range m.Parts {
			if tr, ok := part.(message.ToolResult); ok {
				// Match the updated message phrasing AND verify the
				// available-tools list is included so the helper
				// stays helpful (regression guard for the
				// 2026-05-11 fix that replaced "unknown tool: X"
				// with the actionable variant).
				if strings.Contains(tr.Content, "isn't registered") &&
					strings.Contains(tr.Content, "Available tools:") &&
					tr.IsError {
					foundUnknown = true
				}
			}
		}
	}
	if !foundUnknown {
		t.Errorf("expected unknown-tool error for write in Planning mode")
	}
	// And the file should NOT have been created.
	if _, err := os.Stat(filepath.Join(ws, "x.txt")); err == nil {
		t.Errorf("Planning mode permitted file creation — write tool was not filtered")
	}
}

func TestRunLoop_CodingModeAllowsWrite(t *testing.T) {
	// Sanity check the inverse: same tool call in Coding mode should
	// succeed. Otherwise the Planning test above is not actually
	// checking mode-driven filtering — just a broken registry.
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "c1",
					Name:  "write",
					Input: marshalInput(map[string]interface{}{"file_path": "ok.txt", "content": "yes"}),
					Type:  "tool_use",
				},
			},
			FinishReason: message.FinishReasonToolUse,
		},
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "done"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	_, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "write it",
		Provider:  p,
		Mode:      ModeCoding,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "ok.txt")); err != nil {
		t.Errorf("Coding mode should permit write: %v", err)
	}
}

func TestDetectMode_PlanningWinsOverReviewWhenBothPresent(t *testing.T) {
	// Both phrases present; precedence #3 (Planning) outranks #4 (Review).
	in := "plan how we'd review the auth changes"
	if got := DetectMode(in, ModeUnknown); got != ModePlanning {
		t.Errorf("got %v, want Planning (precedence)", got)
	}
}

func TestDetectMode_SlashLeadingWhitespaceTrimmed(t *testing.T) {
	if got := DetectMode("  /debug now", ModeUnknown); got != ModeDebug {
		t.Errorf("got %v, want Debug despite leading spaces", got)
	}
}

func TestDetectMode_MixedCaseSlash(t *testing.T) {
	if got := DetectMode("/DEBUG", ModeUnknown); got != ModeDebug {
		t.Errorf("got %v, want Debug (case-insensitive slash)", got)
	}
}

func TestDetectMode_SlashWithTrailingArgs(t *testing.T) {
	if got := DetectMode("/code now please fix this", ModeUnknown); got != ModeCoding {
		t.Errorf("got %v, want Coding (only leading token matters)", got)
	}
}

func TestDetectMode_StickyDebugBeatsPlanningSoft(t *testing.T) {
	// Planning is sticky too, so this case actually switches per
	// "sticky-over-sticky wins". Asserts the correct precedence
	// when prev=Debug and a Planning keyword arrives.
	if got := DetectMode("plan a refactor of the broken module", ModeDebug); got != ModePlanning {
		t.Errorf("got %v, want Planning (sticky-over-sticky)", got)
	}
}

func TestDetectMode_ApprovalPhrasesFallToCoding(t *testing.T) {
	for _, in := range []string{"go", "do it", "ship it", "let's do it"} {
		if got := DetectMode(in, ModePlanning); got != ModeCoding {
			t.Errorf("DetectMode(%q, Planning) = %v, want Coding (approval falls through)", in, got)
		}
	}
}

func TestSystemPrompt_CodingHasExplorationRules(t *testing.T) {
	got := ModeCoding.SystemPrompt()
	for _, want := range []string{"EXPLORATION RULES", "500+ lines", "FILE, the EXACT INSERTION POINT", "budget suffix"} {
		if !strings.Contains(got, want) {
			t.Errorf("Coding prompt missing %q", want)
		}
	}
}

func TestSystemPrompt_CodingNudgesCheckpoint(t *testing.T) {
	got := ModeCoding.SystemPrompt()
	if !strings.Contains(got, "kai_checkpoint") {
		t.Errorf("Coding system prompt must mention kai_checkpoint:\n%s", got)
	}
	if !strings.Contains(got, "After each successful edit") {
		t.Errorf("Coding prompt should instruct after-edit behavior:\n%s", got)
	}
}

func TestSystemPrompt_DebugNudgesCheckpointOnEdits(t *testing.T) {
	got := ModeDebug.SystemPrompt()
	if !strings.Contains(got, "kai_checkpoint") {
		t.Errorf("Debug system prompt must mention kai_checkpoint:\n%s", got)
	}
}

func TestSystemPrompt_PlanningHasNoCheckpointNudge(t *testing.T) {
	// Planning can't edit at all (write/edit/bash filtered out), so
	// the checkpoint instruction would be misleading.
	got := ModePlanning.SystemPrompt()
	if strings.Contains(got, "kai_checkpoint") {
		t.Errorf("Planning prompt should NOT mention kai_checkpoint:\n%s", got)
	}
}

// openMiniGraph returns a graph.DB suitable for the gate-every-write
// path: the runner only needs Graph != nil + the standard schema for
// classifyForGate to run. We don't need any pre-loaded edges — an
// empty graph yields BlastRadius=0 and Verdict=auto, which is exactly
// the trailer the verdict-injection test asserts on.
func openMiniGraph(t *testing.T, _ string) *graph.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	objPath := filepath.Join(dir, "objects")
	if err := os.MkdirAll(objPath, 0o755); err != nil {
		t.Fatalf("mkdir objects: %v", err)
	}
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		t.Fatalf("graph.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS nodes (id BLOB PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS edges (src BLOB NOT NULL, type TEXT NOT NULL, dst BLOB NOT NULL, at BLOB, created_at INTEGER NOT NULL, PRIMARY KEY (src, type, dst, at));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target_id BLOB NOT NULL, target_kind TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS slugs (target_id BLOB PRIMARY KEY, slug TEXT UNIQUE NOT NULL);
CREATE TABLE IF NOT EXISTS logs (kind TEXT NOT NULL, seq INTEGER NOT NULL, id BLOB NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (kind, seq));`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestRunLoop_GateVerdictReachesToolResult(t *testing.T) {
	// End-to-end proof of gate-every-write: a write tool call lands
	// a file, the runner classifies it, and the resulting tool_result
	// the model will read on its NEXT turn carries a [GATE: ...]
	// trailer. Without this trailer the model can't react to held
	// or blocked verdicts — a developer-visible event becomes a
	// model-blind one.
	ws := t.TempDir()
	g := openMiniGraph(t, ws)
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "c1",
					Name:  "write",
					Input: marshalInput(map[string]interface{}{"file_path": "x.txt", "content": "hi"}),
					Type:  "tool_use",
				},
			},
			FinishReason: message.FinishReasonToolUse,
		},
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "done"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	res, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "write x.txt",
		Provider:  p,
		Graph:     g,
		Mode:      ModeCoding,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Find the tool_result message in the transcript.
	var found bool
	for _, m := range res.Transcript {
		for _, part := range m.Parts {
			if tr, ok := part.(message.ToolResult); ok && tr.ToolCallID == "c1" {
				if !strings.Contains(tr.Content, "[GATE:") {
					t.Errorf("write tool_result missing GATE trailer:\n%s", tr.Content)
				}
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no tool_result for write call in transcript")
	}
}

func TestRunLoop_NoGateNoteWhenGraphNil(t *testing.T) {
	// Inverse: without a Graph the runner should NOT pretend to
	// classify. A fabricated trailer would mislead the model and
	// trigger noisy behavior changes (e.g. asking the user about
	// blast radius that wasn't computed).
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "c1",
					Name:  "write",
					Input: marshalInput(map[string]interface{}{"file_path": "x.txt", "content": "hi"}),
					Type:  "tool_use",
				},
			},
			FinishReason: message.FinishReasonToolUse,
		},
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "done"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	res, _ := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "write x.txt",
		Provider:  p,
		Mode:      ModeCoding,
		// No Graph.
	})
	for _, m := range res.Transcript {
		for _, part := range m.Parts {
			if tr, ok := part.(message.ToolResult); ok && tr.ToolCallID == "c1" {
				if strings.Contains(tr.Content, "[GATE:") {
					t.Errorf("trailer should not appear without Graph: %s", tr.Content)
				}
			}
		}
	}
}

func TestFormatVerdictNote_AllVerdicts(t *testing.T) {
	// Format-pinning so the model's prompt-side instructions can
	// rely on stable [GATE:] formatting for parsing.
	cases := []struct {
		verdict  string
		paths    []string
		radius   int
		reasons  []string
		contains []string
	}{
		{"auto", []string{"x.go"}, 0, nil, []string{"auto", "x.go", "0 downstream"}},
		{"review", []string{"y.go"}, 4, nil, []string{"held", "y.go", "4 downstream"}},
		{"block", []string{"z.go"}, 9, []string{"touches protected path: z.go"}, []string{"blocked", "z.go", "Stop and ask"}},
	}
	for _, c := range cases {
		got := formatVerdictNote(c.verdict, c.paths, c.radius, c.reasons)
		for _, want := range c.contains {
			if !strings.Contains(got, want) {
				t.Errorf("verdict=%s: %q missing %q", c.verdict, got, want)
			}
		}
	}
}
