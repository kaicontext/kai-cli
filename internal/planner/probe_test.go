//go:build probe

// Package planner — live probe (build-tagged so it can't run in CI by
// accident). Drives the real LLM with the user's stored kailab token
// to answer empirical questions about how the planner decomposes a
// given prompt. Run with:
//
//	go test ./internal/planner -tags probe -run TestProbe -v
//
// The build tag means `go test ./...` never picks this up; only an
// explicit invocation runs it. Costs real LLM tokens.

package planner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"kai/internal/planner"
	"kai/internal/remote"
	"kai/internal/safetygate"
)

// TestProbe_MultiComponentPlan asks the planner to decompose a
// realistic 4-component request and prints the result so we can see
// whether the LLM produces multi-agent plans for prompts of this
// shape, or punts (returns ErrTooVague) and pushes the user to chat
// mode. Call out: nil graph means no semantic-graph context flows
// into the planner — the LLM is judging on the prompt alone, no
// repo-specific files. Real production calls always supply a graph,
// so this is a pessimistic baseline (less context = more likely to
// punt). If the planner produces N>1 here, production will too.
func TestProbe_MultiComponentPlan(t *testing.T) {
	creds, err := remote.LoadCredentials()
	if err != nil {
		t.Skipf("no kailab credentials at ~/.kai/credentials.json: %v — run `kai auth login` first", err)
	}
	if creds.ServerURL == "" || creds.AccessToken == "" {
		t.Skipf("credentials missing ServerURL or AccessToken")
	}

	model := os.Getenv("KAI_PROBE_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	completer := planner.NewServerCompleter(creds.ServerURL, creds.AccessToken, model)

	cases := []struct {
		name    string
		request string
	}{
		{
			name: "four_component",
			request: "Add a /metrics endpoint to backend that returns JSON with " +
				"request count and uptime, a button in the frontend Header " +
				"component that fetches and displays it, documentation in " +
				"docs/metrics.md, and a test for the endpoint.",
		},
		{
			name:    "vague_build",
			request: "build something cool with a frontend, backend, docs, and tests",
		},
		{
			name:    "concrete_single",
			request: "Add input validation to the loginHandler in routes.go",
		},
	}

	cfg := planner.Config{Model: model, MaxAgents: 6, MaxTokens: 4096}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := planner.Plan(context.Background(),
				c.request, nil /* graph */, safetygate.DefaultConfig(), cfg, completer)
			if err != nil {
				fmt.Printf("\n=== %s ===\nrequest: %s\nerror:   %v\n",
					c.name, c.request, err)
				return
			}
			out, _ := json.MarshalIndent(plan, "", "  ")
			fmt.Printf("\n=== %s ===\nrequest: %s\nagents:  %d\nplan:    %s\n",
				c.name, c.request, len(plan.Agents), out)
		})
	}
}
