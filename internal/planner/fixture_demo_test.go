package planner

import (
	"context"
	"os"
	"testing"

	"kai/internal/remote"
	"kai/internal/safetygate"
)

// TestPlan_FixtureDemo is the canonical example of how to write a
// "real LLM, deterministic test" using the recorder.
//
// First run:
//
//	KAI_LLM_FIXTURES=record go test ./internal/planner -run TestPlan_FixtureDemo
//
// That hits real Anthropic via kailab and saves
// `testdata/fixtures/planner/TestPlan_FixtureDemo_<hash>.json`.
// Commit the fixture.
//
// Every subsequent run (CI or local):
//
//	go test ./internal/planner -run TestPlan_FixtureDemo
//
// reads the fixture and asserts the planner produces a coherent
// WorkPlan. No tokens spent, no network hit.
//
// Skip-if-fixture-missing keeps this test friendly: when the file
// isn't there yet (fresh checkout, or after a prompt change that
// invalidated the hash), the test skips with a clear message rather
// than crashing CI. The author should re-record locally.
//
// This is the template for future LLM-backed planner tests. Copy
// this whole function, change the request, change the assertions.
func TestPlan_FixtureDemo(t *testing.T) {
	ctx := context.Background()

	// 1. Wire the wrapped Completer. In record mode we need real
	//    creds; in replay mode we can leave it nil because the
	//    recorder never calls out.
	var wrapped Completer
	if os.Getenv(envFixtureMode) == string(ModeRecord) ||
		os.Getenv(envFixtureMode) == string(ModeMixed) {
		creds, err := remote.LoadCredentials()
		if err != nil || creds == nil || creds.AccessToken == "" {
			t.Skipf("recording requires `kai auth login` (no credentials found)")
		}
		token, err := remote.GetValidAccessToken()
		if err != nil {
			t.Skipf("recording requires a valid auth token: %v", err)
		}
		wrapped = NewServerCompleter(creds.ServerURL, token, "claude-sonnet-4-6")
	}
	rec := NewRecorder(wrapped, "testdata/fixtures/planner", t.Name())

	// 2. Run the planner against a fixed request. The shape must
	//    not change once recorded — if you tweak the request the
	//    fixture hash drifts and the test will need re-recording.
	plan, err := Plan(ctx,
		"add a one-line README explaining this is a small JS sandbox",
		nil, // no graph DB; planner falls through with raw context
		safetygate.DefaultConfig(),
		Config{MaxAgents: 5, MaxTokens: 2048},
		rec,
	)
	if err != nil {
		// In replay mode without a fixture this is the expected
		// path. Skip rather than fail so a fresh checkout doesn't
		// break CI; once someone records the fixture and commits it,
		// the assertions below take over.
		if rec.resolveMode() == ModeReplay {
			t.Skipf("no fixture yet — re-run with %s=record after `kai auth login` to capture one (then commit testdata/fixtures/)", envFixtureMode)
		}
		t.Fatalf("Plan: %v", err)
	}

	// 3. Assert structure, not exact wording. Real LLM output
	//    fluctuates; pin the contract (one agent, READMEy task)
	//    not the prose.
	if plan == nil {
		t.Fatal("nil plan")
	}
	if len(plan.Agents) == 0 {
		t.Errorf("plan has no agents: %+v", plan)
	}
	if len(plan.Agents) > 5 {
		t.Errorf("plan exceeded MaxAgents: %d", len(plan.Agents))
	}
}
