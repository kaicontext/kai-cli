package orchestrator

// multiroot_e2e_test.go is the cross-project counterpart to
// TestExecuteE2E_SpawnAndInProcessAgent (e2e_test.go). It drives the
// real orchestrator.Execute end-to-end against TWO temp projects
// with a scripted LLM provider whose tool calls write files in each.
//
// SKIPPED unless KAI_BIN is set, same as e2e_test.go.

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kai/internal/agent/message"
	"kai/internal/agent/provider"
	"kai/internal/agentprompt"
	"kai/internal/graph"
	"kai/internal/planner"
	"kai/internal/projects"
	"kai/internal/safetygate"
)

func TestExecuteE2E_MultiRootSpawnAndAbsorb(t *testing.T) {
	kaiBin := os.Getenv("KAI_BIN")
	if kaiBin == "" {
		t.Skip("KAI_BIN not set — skipping multi-root e2e (set to a built kai binary path)")
	}
	if _, err := os.Stat(kaiBin); err != nil {
		t.Skipf("KAI_BIN=%s not stat-able: %v", kaiBin, err)
	}

	root := t.TempDir()
	kaiDir := filepath.Join(root, "kai")
	srvDir := filepath.Join(root, "kai-server")
	for _, p := range []string{kaiDir, srvDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(kaiDir, "a.txt"), []byte("kai v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srvDir, "b.txt"), []byte("srv v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{kaiDir, srvDir} {
		mustRun(t, p, kaiBin, "init")
		mustRun(t, p, kaiBin, "capture", "-m", "baseline")
	}

	primaryProj := &projects.Project{
		Name:    "kai",
		Path:    kaiDir,
		KaiDir:  filepath.Join(kaiDir, ".kai"),
	}
	secondaryProj := &projects.Project{
		Name:    "kai-server",
		Path:    srvDir,
		KaiDir:  filepath.Join(srvDir, ".kai"),
	}
	set := projects.New(root, []*projects.Project{primaryProj, secondaryProj})
	if err := set.Open(); err != nil {
		t.Fatalf("opening project DBs: %v", err)
	}
	defer set.Close()

	if primaryProj.DB == nil || secondaryProj.DB == nil {
		t.Fatalf("Set.Open didn't populate DBs (primary=%v secondary=%v)",
			primaryProj.DB != nil, secondaryProj.DB != nil)
	}

	beforePrimary, _ := resolveLatestSnap(primaryProj.DB)
	beforeSecondary, _ := resolveLatestSnap(secondaryProj.DB)
	if len(beforePrimary) == 0 || len(beforeSecondary) == 0 {
		t.Fatalf("baseline snap.latest missing: primary=%v secondary=%v",
			hex.EncodeToString(beforePrimary), hex.EncodeToString(beforeSecondary))
	}

	plan := &planner.WorkPlan{
		Summary: "fake cross-project change",
		Agents: []planner.AgentTask{
			{
				Name:   "cross-project-writer",
				Prompt: "overwrite a.txt and b.txt in their respective projects",
				Files: []string{
					"kai/a.txt",
					"kai-server/b.txt",
				},
			},
		},
	}

	fakeLLM := &scriptedProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "w1",
					Name:  "write",
					Input: `{"file_path":"kai/a.txt","content":"kai v2 from agent\n"}`,
					Type:  "tool_use",
				},
			},
			FinishReason: message.FinishReasonToolUse,
		},
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "w2",
					Name:  "write",
					Input: `{"file_path":"kai-server/b.txt","content":"srv v2 from agent\n"}`,
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
		AgentTimeout:  60 * time.Second,
		KaiBinary:     kaiBin,
		SpawnPrefix:   filepath.Join(t.TempDir(), "kai-multiroot-e2e-"),
		PushRemote:    "",
		GateConfig:    safetygate.DefaultConfig(),
		AgentProvider: fakeLLM,
		Projects:      set,
		MainGraph:     primaryProj.DB,
		PromptContext: agentprompt.Context{RepoRoot: kaiDir},
	}

	res, err := Execute(context.Background(), plan, cfg, primaryProj.DB, kaiDir, primaryProj.KaiDir)
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

	body, err := os.ReadFile(filepath.Join(kaiDir, "a.txt"))
	if err != nil {
		t.Fatalf("reading absorbed kai/a.txt: %v", err)
	}
	if !strings.Contains(string(body), "kai v2 from agent") {
		t.Errorf("kai/a.txt did not absorb agent's write: %q", string(body))
	}
	body, err = os.ReadFile(filepath.Join(srvDir, "b.txt"))
	if err != nil {
		t.Fatalf("reading absorbed kai-server/b.txt: %v", err)
	}
	if !strings.Contains(string(body), "srv v2 from agent") {
		t.Errorf("kai-server/b.txt did not absorb agent's write: %q", string(body))
	}

	afterPrimary, _ := resolveLatestSnap(primaryProj.DB)
	if hex.EncodeToString(afterPrimary) == hex.EncodeToString(beforePrimary) {
		t.Errorf("primary snap.latest did not advance past baseline")
	}
	// Secondary's snap.latest may have rolled back on Held verdict;
	// what we DO require is that a new snapshot node was captured.
	secondarySnapshots, err := secondaryProj.DB.GetNodesByKind(graph.KindSnapshot)
	if err != nil {
		t.Fatalf("listing secondary snapshots: %v", err)
	}
	if len(secondarySnapshots) < 2 {
		t.Errorf("secondary project should have ≥2 snapshots (baseline + agent), got %d", len(secondarySnapshots))
	}

	if run.Verdict == nil {
		t.Errorf("expected non-nil run.Verdict, got nil (IntegrateErr=%v)", run.IntegrateErr)
	}

	// Some project's snap.latest may have rolled back to baseline if
	// its verdict was non-Auto (v0.31.7); in that case the
	// gate-decorated node is the OTHER snapshot (not the one pointed
	// to by snap.latest). So scan all snapshots and require AT LEAST
	// one per project carries a populated gateVerdict.
	hasDecoratedSnap := func(db *graph.DB) (bool, []string) {
		snaps, _ := db.GetNodesByKind(graph.KindSnapshot)
		var verdicts []string
		for _, n := range snaps {
			if v, ok := n.Payload["gateVerdict"].(string); ok && v != "" {
				verdicts = append(verdicts, v)
			}
		}
		return len(verdicts) > 0, verdicts
	}
	if ok, vs := hasDecoratedSnap(primaryProj.DB); !ok {
		t.Errorf("primary has no snapshot with gateVerdict (v0.31.4 decoration missing); verdicts=%v", vs)
	}
	if ok, vs := hasDecoratedSnap(secondaryProj.DB); !ok {
		t.Errorf("secondary has no snapshot with gateVerdict (v0.31.4 per-project decoration missing for sibling); verdicts=%v", vs)
	}
}
