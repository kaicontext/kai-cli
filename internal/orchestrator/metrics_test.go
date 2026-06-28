package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/agent"
	"github.com/kaicontext/kai-engine/message"
)

// TestRecordInjectionMetric_WritesJSONL covers the happy path: a
// run with injection, a real first tool call, and a verify outcome
// produces one line on .kai/metrics.jsonl with the expected fields.
func TestRecordInjectionMetric_WritesJSONL(t *testing.T) {
	ws := t.TempDir()
	// Pre-create .kai/ — the writer no longer auto-creates it
	// (see TestRecordInjectionMetric_DoesNotCreateKaiDir for the
	// regression that drove this change).
	if err := os.MkdirAll(filepath.Join(ws, ".kai"), 0o755); err != nil {
		t.Fatal(err)
	}

	transcript := []message.Message{
		// Synthetic context_lookup pair should NOT be counted as
		// "first agent tool call."
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "ctx_lookup_1", Name: "context_lookup"},
			},
		},
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "ctx_lookup_1", Name: "context_lookup"},
			},
		},
		// Real first tool call — viewing a file in the injected chain.
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "tool_2",
					Name:  "view",
					Input: `{"file_path":"internal/projects/set.go"}`,
				},
			},
		},
	}

	body := "Entry: kai code → runCodeTUI (via command index)\n" +
		"  → projects.Set.Primary (internal/projects/set.go)\n"

	res := &agent.Result{
		InjectedContextChars: len(body),
		AbsenceGuardFired:    false,
	}

	recordInjectionMetric(ws, "task-x", "debug", body, transcript, verifyPassed, res)

	data, err := os.ReadFile(filepath.Join(ws, ".kai", "metrics.jsonl"))
	if err != nil {
		t.Fatalf("metrics.jsonl not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %q", len(lines), data)
	}

	var m injectionMetric
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("decoding line: %v\nline: %s", err, lines[0])
	}
	if m.TaskName != "task-x" {
		t.Errorf("TaskName = %q, want task-x", m.TaskName)
	}
	if m.Mode != "debug" {
		t.Errorf("Mode = %q, want debug", m.Mode)
	}
	if m.InjectedChars != len(body) {
		t.Errorf("InjectedChars = %d, want %d", m.InjectedChars, len(body))
	}
	if m.FirstToolName != "view" {
		t.Errorf("FirstToolName = %q, want view (NOT context_lookup)", m.FirstToolName)
	}
	if m.FirstToolFile != "internal/projects/set.go" {
		t.Errorf("FirstToolFile = %q, want set.go path", m.FirstToolFile)
	}
	if !m.FirstToolInChain {
		t.Errorf("FirstToolInChain = false, want true (set.go IS in the chain)")
	}
	if m.AbsenceGuardFired {
		t.Errorf("AbsenceGuardFired = true, want false")
	}
	if m.VerifyOutcomeName != "passed" {
		t.Errorf("VerifyOutcomeName = %q, want passed", m.VerifyOutcomeName)
	}
	if !m.CompletedFirstPass {
		t.Errorf("CompletedFirstPass = false, want true (verifyPassed)")
	}
}

// TestRecordInjectionMetric_OutOfChainAgent: when the agent's first
// tool call is on a file NOT in the injected chain, FirstToolInChain
// is false. This is the locality signal that flags "injection
// didn't anchor the agent."
func TestRecordInjectionMetric_OutOfChainAgent(t *testing.T) {
	ws := t.TempDir()
	transcript := []message.Message{
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{
				message.ToolCall{
					Name:  "view",
					Input: `{"file_path":"unrelated/other.go"}`,
				},
			},
		},
	}
	body := "Entry: kai code → runCodeTUI\n  → set.Primary (internal/projects/set.go)\n"
	res := &agent.Result{InjectedContextChars: len(body)}

	recordInjectionMetric(ws, "task-y", "debug", body, transcript, verifyUnknown, res)

	data, _ := os.ReadFile(filepath.Join(ws, ".kai", "metrics.jsonl"))
	var m injectionMetric
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(data))), &m)
	if m.FirstToolInChain {
		t.Errorf("FirstToolInChain = true, want false (other.go not in chain)")
	}
}

// TestInjectionFiles_ParsesParenSegments verifies the file-path
// extractor: it should pull paths out of "(path)" segments while
// ignoring decorative parentheses like "(via command index)".
func TestInjectionFiles_ParsesParenSegments(t *testing.T) {
	body := "Entry: kai code → runCodeTUI (via command index)\n" +
		"  → projects.Discover (internal/projects/discover.go)\n" +
		"  → os.Getwd — stdlib, not expanded\n" +
		"  → set.Primary (internal/projects/set.go)\n"

	got := injectionFiles(body)
	want := map[string]bool{
		"internal/projects/discover.go": true,
		"internal/projects/set.go":      true,
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing path %q in extracted set: %v", k, got)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("unexpected path %q in extracted set", k)
		}
	}
}

// TestRecordInjectionMetric_AppendsNotOverwrites: two runs from the
// same workspace produce two lines.
func TestRecordInjectionMetric_AppendsNotOverwrites(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".kai"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := &agent.Result{}
	recordInjectionMetric(ws, "run1", "debug", "", nil, verifyUnknown, res)
	recordInjectionMetric(ws, "run2", "debug", "", nil, verifyUnknown, res)

	data, err := os.ReadFile(filepath.Join(ws, ".kai", "metrics.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines after 2 runs, got %d", len(lines))
	}
}

// TestRecordInjectionMetric_DoesNotCreateKaiDir is the regression
// for the rogue-.kai scattering. recordInjectionMetric used to
// MkdirAll filepath.Join(workspaceRoot, ".kai") unconditionally,
// which created a sibling .kai/ in every workspace whose real
// kaiDir is .git/kai/. Surfaced 2026-05-12 in the kai-server repo
// where the user lost .git/kai-versioned data when I rm -rf'd the
// rogue .kai/ that this writer had silently materialised.
//
// Same shape as TestLogLocal_DoesNotCreateKaiDir over in
// internal/tui/errors — metrics writes are best-effort; missing
// kai dir means skip silently, not fabricate one.
func TestRecordInjectionMetric_DoesNotCreateKaiDir(t *testing.T) {
	ws := t.TempDir()
	// Workspace has no .kai/ and no .git/kai/ — the dangerous shape.

	recordInjectionMetric(
		ws,
		"synthetic.task", "coding",
		"synthetic injection body",
		nil, // empty transcript
		verifyUnknown,
		&agent.Result{},
	)

	for _, c := range []string{
		filepath.Join(ws, ".kai"),
		filepath.Join(ws, ".git", "kai"),
	} {
		if _, err := os.Stat(c); err == nil {
			t.Errorf("recordInjectionMetric created %s; should have skipped silently when no kai dir exists", c)
		}
	}
}

// TestRecordInjectionMetric_WritesToExistingKaiDir is the happy
// path: when .git/kai/ exists (the convention in git repos), the
// metric lands there instead of a sibling .kai/. Without this the
// fix above is too aggressive and metrics are silently dropped for
// every git-tracked workspace.
func TestRecordInjectionMetric_WritesToExistingKaiDir(t *testing.T) {
	ws := t.TempDir()
	gitKai := filepath.Join(ws, ".git", "kai")
	if err := os.MkdirAll(gitKai, 0o755); err != nil {
		t.Fatal(err)
	}

	recordInjectionMetric(
		ws,
		"synthetic.task", "coding",
		"synthetic injection body",
		nil,
		verifyUnknown,
		&agent.Result{},
	)

	body, err := os.ReadFile(filepath.Join(gitKai, "metrics.jsonl"))
	if err != nil {
		t.Fatalf("metrics.jsonl not written to .git/kai/: %v", err)
	}
	if !strings.Contains(string(body), "synthetic.task") {
		t.Errorf("metrics.jsonl missing expected task name, got: %s", body)
	}
	// And the rogue .kai/ MUST NOT have been created alongside.
	if _, err := os.Stat(filepath.Join(ws, ".kai")); err == nil {
		t.Errorf(".kai/ created alongside .git/kai/ — the bug came back")
	}
}
