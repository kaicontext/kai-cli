package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

const sweepFinding = `{"findings":[{"file":"a.go","line":1,"claim":"nil dereference","evidence":["caller passes nil"]}]}`

func TestSweepIgnoresUnrelatedVerdicts(t *testing.T) {
	for _, raw := range []string{sweepFinding, strings.TrimSuffix(sweepFinding, "}") + `,"intent_match":"not applicable","merge_ready":"irrelevant"}`, "```json\n" + sweepFinding + "\n```"} {
		issues, err := rcDecodeSweep(raw, map[string]bool{"a.go": true})
		if err != nil || len(issues) != 1 {
			t.Fatalf("lost valid finding: %v %v", issues, err)
		}
	}
	for _, raw := range []string{`{}`, `{"findings":null}`, `{"findings":"none"}`, `{"findings":["a.go:0 — bug"]}`, `{"findings":[{"file":"../a.go","line":1,"claim":"bad","evidence":[]}]}`, strings.ReplaceAll(sweepFinding, "a.go", "other.go")} {
		if _, err := rcDecodeSweep(raw, map[string]bool{"a.go": true}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if got, err := rcDecodeSweep(`{"findings":[]}`, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty result: %v %v", got, err)
	}
}

func TestSweepRepairIsBoundedAndRetainsBothResponses(t *testing.T) {
	for _, repairOK := range []bool{true, false} {
		calls := 0
		p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
			calls++
			if len(req.OutputJSONSchema) > 0 {
				t.Fatal("provider grammar enabled")
			}
			raw := "not json"
			if repairOK && calls == 2 {
				raw = sweepFinding
			}
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: raw}}, FinishReason: message.FinishReasonEndTurn}, nil
		}}
		issues, attempts, err := rcSweepChunk(context.Background(), p, provider.Request{}, map[string]bool{"a.go": true}, 3)
		if calls != 2 || len(attempts) != 2 || attempts[0].Error == "" || attempts[0].Raw != "not json" || attempts[1].Stage != "sweep_repair" || attempts[1].Chunk != 3 {
			t.Fatalf("lost diagnostics or unbounded repair: %d %+v", calls, attempts)
		}
		if repairOK && (err != nil || len(issues) != 1) {
			t.Fatalf("repair failed: %v", err)
		}
		if !repairOK && err == nil {
			t.Fatal("invalid repair accepted")
		}
	}
}

func TestSweepProviderFailureAndTruncationAreNotRepaired(t *testing.T) {
	for _, truncated := range []bool{true, false} {
		calls := 0
		p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
			calls++
			if !truncated {
				return provider.Response{}, errors.New("429 rate limit")
			}
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: sweepFinding}}, FinishReason: message.FinishReasonMaxTokens}, nil
		}}
		_, attempts, err := rcSweepChunk(context.Background(), p, provider.Request{}, map[string]bool{"a.go": true}, 1)
		if calls != 1 || err == nil || len(attempts) != 1 || attempts[0].Error == "" {
			t.Fatalf("lost failure: %d %+v %v", calls, attempts, err)
		}
	}
}

func TestSweepFailureSurvivesMergeAndCompletionRecord(t *testing.T) {
	a := rcSweepResult{Chunks: 1, Failed: 1, Runs: []rcSweepRun{{Model: "first", Chunks: 1, Failed: 1}}}
	b := rcSweepResult{Chunks: 1, Issues: []string{"a.go:1 — bug"}, Runs: []rcSweepRun{{Model: "second", Chunks: 1}}}
	sw := rcMergeSweeps(a, b)
	if sw.Failed != 1 || len(sw.Runs) != 2 || len(sw.Issues) != 1 {
		t.Fatalf("failure hidden: %+v", sw)
	}
	inc := &rcIncomplete{Execution: &rcExecution{Discovery: "completed", Verification: "completed"}}
	inc.recordSweep(sw)
	if inc.Execution.Discovery != "incomplete" || inc.Execution.Verification != "completed" || len(inc.Execution.Sweeps) != 2 {
		t.Fatalf("bad completeness: %+v", inc.Execution)
	}
}
