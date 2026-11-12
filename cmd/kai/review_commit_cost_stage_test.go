package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"sync"
	"testing"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/provider"
)

// The server stores a stage only when it is 1 to 40 characters of [a-z0-9_].
var serverStageRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

func TestCostStagesAreValidAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range rcCostStages {
		if !serverStageRe.MatchString(s) {
			t.Errorf("stage %q would be stored blank by the server", s)
		}
		if seen[s] {
			t.Errorf("stage %q listed twice", s)
		}
		seen[s] = true
	}
}

// stageRecorder is a Kailab endpoint that records each request's stage.
func stageRecorder(t *testing.T) (*provider.Kailab, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-Kai-Stage"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return provider.NewKailab(srv.URL, "tok"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), got...)
		sort.Strings(out)
		return out
	}
}

func TestReviewCallsSendTheirStage(t *testing.T) {
	const model = "z-ai/glm-5.2"
	ctx := context.Background()
	patches := map[string]string{"a.go": "@@ -1 +1 @@\n-a\n+b\n"}
	for _, tc := range []struct {
		want string
		call func(p provider.Provider)
	}{
		{rcStageIntent, func(p provider.Provider) { _, _ = rcInferIntent(ctx, p, model, "subject", "", "diff") }},
		{rcCostStageDescription, func(p provider.Provider) { _, _ = rcWriteDescription(ctx, p, model, "author", "diff") }},
		{rcStageSweep, func(p provider.Provider) { rcRunSweep(ctx, p, model, "", []string{"a.go"}, patches) }},
		{rcStageSweep2, func(p provider.Provider) { rcRunSweepWith(ctx, p, model, rcStageSweep2, "", []string{"a.go"}, patches) }},
		// The fast pass's fact-check has no repository; its first send and
		// the correction it asks for both carry the stage.
		{rcStageQuickFactcheck, func(p provider.Provider) {
			_, _ = rcChallengeReview(ctx, p, model, rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
		}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			p, got := stageRecorder(t)
			tc.call(p)
			stages := got()
			if len(stages) == 0 {
				t.Fatal("no request was made")
			}
			for _, s := range stages {
				if s != tc.want {
					t.Errorf("X-Kai-Stage = %q, want %q", s, tc.want)
				}
			}
		})
	}
}

// A caller's stage never leaks into a call that names its own.
func TestCallStageOverridesInheritedStage(t *testing.T) {
	p, got := stageRecorder(t)
	ctx := rcCostStage(context.Background(), rcStageMain)
	_, _ = rcInferIntent(ctx, p, "z-ai/glm-5.2", "subject", "", "diff")
	if s := got(); len(s) != 1 || s[0] != rcStageIntent {
		t.Fatalf("X-Kai-Stage = %q, want [%q]", s, rcStageIntent)
	}
}

// The fast pass's fact-check runs without the repository; the grounded
// review's runs with it.
func TestGateStage(t *testing.T) {
	if s := rcGateStage(context.Background()); s != rcStageQuickFactcheck {
		t.Errorf("without a repo: %q, want %q", s, rcStageQuickFactcheck)
	}
	if s := rcGateStage(rcWithRepo(context.Background(), &rcRepo{})); s != rcStageFactcheck {
		t.Errorf("with a repo: %q, want %q", s, rcStageFactcheck)
	}
}

func TestCostClientVersion(t *testing.T) {
	prev := agent.ClientVersion()
	t.Cleanup(func() { agent.SetClientVersion(prev) })
	setCostClientVersion()
	if got := agent.ClientVersion(); got != Version {
		t.Fatalf("ClientVersion = %q, want %q", got, Version)
	}
}
