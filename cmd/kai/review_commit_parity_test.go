package main

// With none of the new settings sent (KAI_REVIEW_FALLBACK_MODELS,
// KAI_REVIEW_SUMMARY_MODEL, KAI_REVIEW_PARTIAL), a review must make the same
// model calls and write the same bundle as main did before them. The
// recording in testdata/review_parity_main.json was made by running this test
// on main (KAI_UPDATE_PARITY=1); this file and review_commit_fakemodels_test.go
// use only what main already has, so it can be made again the same way.

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const rcParityGolden = "testdata/review_parity_main.json"

// rcParityScenarios are the reviews compared: a healthy one, a hollow one
// (the finder writes prose with no verdict and the conclusion writes stray
// tool-call text), and one whose model answers nothing, which the engine's
// own failover chain rescues.
var rcParityScenarios = map[string]rcFakeScenario{
	"healthy": {Main: "review"},
	"hollow":  {Main: "prose", Conclusion: "stray"},
	"empty_main_model": {Main: "review", Fault: func(step, model string, _ *rand.Rand) string {
		if step == rcFakeAgent && model == "z-ai/glm-5.2" {
			return rcFaultEmpty
		}
		return rcFaultNone
	}},
}

// rcParityRecord is one scenario's calls, in a stable order, and its bundle.
type rcParityRecord struct {
	Calls  []rcFakeCall    `json:"calls"`
	Err    string          `json:"err"`
	Bundle json.RawMessage `json:"bundle"`
}

func rcParityRun(t *testing.T, sc rcFakeScenario) rcParityRecord {
	t.Helper()
	f := newRcFakeModelServer(t, sc)
	f.root = rcFakeRepo(t)
	rcFakeReviewEnv(t, f, "z-ai/glm-5.2", nil)
	out, err := rcFakeRunReview(t)
	calls := f.Calls()
	// Stages run side by side, so arrival order is not stable.
	sort.SliceStable(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		if a.Step != b.Step {
			return a.Step < b.Step
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Messages != b.Messages {
			return a.Messages < b.Messages
		}
		return a.Prompt < b.Prompt
	})
	rec := rcParityRecord{Calls: calls}
	if err != nil {
		rec.Err = err.Error()
	}
	var bundle any
	if strings.TrimSpace(out) != "" {
		if jerr := json.Unmarshal([]byte(out), &bundle); jerr != nil {
			t.Fatalf("bundle: %v\n%s", jerr, out)
		}
	}
	rec.Bundle, _ = json.Marshal(bundle)
	return rec
}

func TestReviewWithNoNewSettingsMatchesMain(t *testing.T) {
	got := map[string]rcParityRecord{}
	for name, sc := range rcParityScenarios {
		t.Run(name, func(t *testing.T) { got[name] = rcParityRun(t, sc) })
	}
	if os.Getenv("KAI_UPDATE_PARITY") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.MkdirAll(filepath.Dir(rcParityGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rcParityGolden, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skipf("recorded %s", rcParityGolden)
	}
	raw, err := os.ReadFile(rcParityGolden)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]rcParityRecord
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for name := range rcParityScenarios {
		g, w := got[name], want[name]
		if len(g.Calls) != len(w.Calls) {
			t.Errorf("%s: %d model calls, main made %d", name, len(g.Calls), len(w.Calls))
		}
		for i := range min(len(g.Calls), len(w.Calls)) {
			if g.Calls[i] != w.Calls[i] {
				t.Errorf("%s: call %d = %+v\nmain made %+v", name, i, g.Calls[i], w.Calls[i])
			}
		}
		if g.Err != w.Err {
			t.Errorf("%s: err = %q, main returned %q", name, g.Err, w.Err)
		}
		if !jsonEqual(g.Bundle, w.Bundle) {
			t.Errorf("%s: bundle differs from main's\ngot:  %s\nmain: %s", name, g.Bundle, w.Bundle)
		}
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}
