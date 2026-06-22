package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kai/internal/agent/message"
	"kai/internal/agent/provider"
	"kai/internal/agentprompt"
	"kai/internal/graph"
	"kai/internal/planner"
	"kai/internal/safetygate"
)

// TestExecuteE2E_SpawnAndInProcessAgent runs the orchestrator against a
// real kai binary using the in-process agent runner with a fake LLM
// provider. Stops short of push/pull/integrate because kai's remote
// is HTTP-only (kailab) and a mock kailab server is more harness than
// the value justifies in this test.
//
// What this verifies post-Slice 6 (the external-subprocess path is gone):
//
//   - orchestrator.Execute calls `kai spawn` correctly
//   - the in-process runner is invoked with the workspace + prompt
//   - the agent's tool calls (write/edit) actually modify files in
//     the spawn workspace
//   - push/pull/integrate fail predictably with no remote configured,
//     proving we reached the integrate phase
//
// Skipped unless KAI_BIN points at a buildable kai binary. CI sets
// KAI_BIN so the test runs there; locally it's opt-in.
func TestExecuteE2E_SpawnAndInProcessAgent(t *testing.T) {
	kaiBin := os.Getenv("KAI_BIN")
	if kaiBin == "" {
		t.Skip("KAI_BIN not set — skipping e2e (set to a built kai binary path)")
	}
	if _, err := os.Stat(kaiBin); err != nil {
		t.Skipf("KAI_BIN=%s not stat-able: %v", kaiBin, err)
	}

	// 1. Set up a temp source repo + capture a baseline so spawn has
	//    a snapshot to clone from.
	src := t.TempDir()
	mustRun(t, src, kaiBin, "init")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, src, kaiBin, "capture", "-m", "baseline")

	kaiDir := filepath.Join(src, ".kai")
	db, err := graph.Open(filepath.Join(kaiDir, "db.sqlite"), filepath.Join(kaiDir, "objects"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()

	// 2. Plan + fake provider that scripts a single tool-use turn
	//    (call `write` to overwrite hello.txt) followed by end_turn.
	plan := &planner.WorkPlan{
		Summary: "fake change for e2e",
		Agents: []planner.AgentTask{
			{Name: "writer", Prompt: "overwrite hello.txt", Files: []string{"hello.txt"}},
		},
	}

	fakeLLM := &scriptedProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "c1",
					Name:  "write",
					Input: `{"file_path":"hello.txt","content":"v2 from agent\n"}`,
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

	cfg := Config{
		AgentTimeout:  30 * time.Second,
		KaiBinary:     kaiBin,
		SpawnPrefix:   filepath.Join(t.TempDir(), "kai-e2e-"),
		PushRemote:    "origin", // nothing configured; push will fail
		GateConfig:    safetygate.DefaultConfig(),
		AgentProvider: fakeLLM,
		PromptContext: agentprompt.Context{RepoRoot: src},
	}

	res, err := Execute(context.Background(), plan, cfg, db, src, kaiDir)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(res.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(res.Runs))
	}
	run := res.Runs[0]

	if run.SpawnDir == "" {
		t.Fatal("spawn dir not populated; spawn step failed")
	}
	if run.ExitErr != nil {
		t.Fatalf("agent exited with error: %v", run.ExitErr)
	}

	// Agent should have overwritten hello.txt with "v2 from agent".
	body, err := os.ReadFile(filepath.Join(run.SpawnDir, "hello.txt"))
	if err != nil {
		t.Fatalf("reading spawn hello.txt: %v", err)
	}
	if !strings.Contains(string(body), "v2 from agent") {
		t.Errorf("agent's write didn't land: %q", string(body))
	}

	// Phase B — without a kailab remote configured, push fails.
	// That's the expected outcome: it tells us the pipeline got past
	// the agent and reached integrate.
	if run.IntegrateErr == nil {
		t.Errorf("expected integrate err (no remote configured), got nil")
	}
	if run.Verdict != nil {
		t.Errorf("expected nil verdict (integrate skipped on push fail), got %+v", run.Verdict)
	}
	if res.Failed != 1 {
		t.Errorf("expected res.Failed=1, got %d (auto=%d held=%d)", res.Failed, res.AutoPromoted, res.Held)
	}
}

// scriptedProvider returns canned Responses in order. Tiny duplicate
// of the runner's fakeProvider — kept here to avoid cross-package
// test fixture sharing (which Go discourages).
type scriptedProvider struct {
	queue []provider.Response
}

func (s *scriptedProvider) Send(_ context.Context, _ provider.Request) (provider.Response, error) {
	if len(s.queue) == 0 {
		return provider.Response{
			Parts:        []message.ContentPart{message.TextContent{Text: "queue empty"}},
			FinishReason: message.FinishReasonEndTurn,
		}, nil
	}
	r := s.queue[0]
	s.queue = s.queue[1:]
	return r, nil
}

// mustRun execs cmd in dir, fatal on non-zero. Used by the e2e test
// for setup steps where any failure would invalidate everything.
func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s in %s failed: %v\n%s", name, strings.Join(args, " "), dir, err, string(out))
	}
}
