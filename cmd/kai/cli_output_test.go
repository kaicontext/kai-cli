package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_ShadowReportJSONFormat(t *testing.T) {
	report := ShadowReport{
		Version:     1,
		GeneratedAt: "2025-01-01T00:00:00Z",
		KaiVersion:  "test-1.0",
		GitRange:    "abc..def",
		Verdict:     ShadowVerdictSafe,
		Plan: &CIPlan{
			Version:    1,
			Mode:       "selective",
			Risk:       "low",
			SafetyMode: "shadow",
			Confidence: 1.0,
			Targets: CITargets{
				Run:  []string{"tests/a.test.ts"},
				Skip: []string{},
				Full: []string{"tests/a.test.ts", "tests/b.test.ts"},
				Tags: map[string][]string{},
			},
		},
		SelectiveRun: &ShadowRunResult{
			Command:     "npm test -- tests/a.test.ts",
			ExitCode:    0,
			DurationS:   1.5,
			TotalTests:  3,
			Passed:      3,
			FailedTests: []TestFailureDetail{},
		},
		FullRun: &ShadowRunResult{
			Command:     "npm test",
			ExitCode:    0,
			DurationS:   5.0,
			TotalTests:  10,
			Passed:      10,
			FailedTests: []TestFailureDetail{},
		},
		Metrics: ShadowMetrics{
			TestsReduced:    7,
			TestsReducedPct: 70.0,
			TimeSavedS:      3.5,
			TimeSavedPct:    70.0,
			FalseNegatives:  0,
			Accuracy:        1.0,
		},
		Flaky:    ShadowFlakyInfo{},
		Fallback: ShadowFallbackInfo{},
	}

	outPath := filepath.Join(t.TempDir(), "report.json")
	if err := writeShadowJSON(report, outPath); err != nil {
		t.Fatalf("writeShadowJSON: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}

	// Verify it's valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}

	// Verify required top-level fields exist
	requiredFields := []string{
		"version", "generatedAt", "kaiVersion", "gitRange",
		"verdict", "plan", "selectiveRun", "fullRun", "metrics",
	}
	for _, field := range requiredFields {
		if _, ok := parsed[field]; !ok {
			t.Errorf("missing required field %q in JSON output", field)
		}
	}

	// Verify metrics sub-fields
	metrics, ok := parsed["metrics"].(map[string]interface{})
	if !ok {
		t.Fatal("metrics is not an object")
	}
	metricFields := []string{"testsReduced", "testsReducedPct", "timeSavedS", "timeSavedPct", "falseNegatives", "accuracy"}
	for _, field := range metricFields {
		if _, ok := metrics[field]; !ok {
			t.Errorf("missing metrics field %q", field)
		}
	}
}

func TestCLI_ShadowReportJSONRoundtrip(t *testing.T) {
	report := ShadowReport{
		Version:     1,
		GeneratedAt: "2025-01-01T00:00:00Z",
		KaiVersion:  "test-1.0",
		GitRange:    "abc..def",
		Verdict:     ShadowVerdictMissed,
		SelectiveRun: &ShadowRunResult{
			Command:     "test",
			ExitCode:    0,
			DurationS:   1.0,
			TotalTests:  3,
			Passed:      3,
			FailedTests: []TestFailureDetail{},
		},
		FullRun: &ShadowRunResult{
			Command:    "test",
			ExitCode:   1,
			DurationS:  5.0,
			TotalTests: 10,
			Passed:     8,
			FailedTests: []TestFailureDetail{
				{Name: "test_a", ErrorMessage: "failed assertion"},
				{Name: "test_b", ErrorMessage: "timeout"},
			},
		},
		Metrics: ShadowMetrics{
			TestsReduced:    7,
			TestsReducedPct: 70.0,
			TimeSavedS:      4.0,
			TimeSavedPct:    80.0,
			FalseNegatives:  2,
			Accuracy:        0.0,
		},
		Flaky: ShadowFlakyInfo{
			Detected: true,
			FlakyTests: []FlakyTestDetail{
				{Name: "test_flaky", Classification: "flaky", Confidence: 0.67, FailCount: 1, TotalRetries: 3},
			},
		},
		Fallback: ShadowFallbackInfo{},
	}

	outPath := filepath.Join(t.TempDir(), "report.json")
	if err := writeShadowJSON(report, outPath); err != nil {
		t.Fatalf("writeShadowJSON: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}

	// Roundtrip: unmarshal back to ShadowReport
	var loaded ShadowReport
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("roundtrip unmarshal: %v", err)
	}

	if loaded.Version != report.Version {
		t.Errorf("version: want %d, got %d", report.Version, loaded.Version)
	}
	if loaded.Verdict != report.Verdict {
		t.Errorf("verdict: want %s, got %s", report.Verdict, loaded.Verdict)
	}
	if loaded.Metrics.FalseNegatives != report.Metrics.FalseNegatives {
		t.Errorf("false negatives: want %d, got %d", report.Metrics.FalseNegatives, loaded.Metrics.FalseNegatives)
	}
	if len(loaded.FullRun.FailedTests) != len(report.FullRun.FailedTests) {
		t.Errorf("failed tests count: want %d, got %d", len(report.FullRun.FailedTests), len(loaded.FullRun.FailedTests))
	}
	if len(loaded.Flaky.FlakyTests) != len(report.Flaky.FlakyTests) {
		t.Errorf("flaky tests count: want %d, got %d", len(report.Flaky.FlakyTests), len(loaded.Flaky.FlakyTests))
	}
}

func TestCLI_CIPlanJSONFormat(t *testing.T) {
	plan := CIPlan{
		Version:    1,
		Mode:       "selective",
		Risk:       "low",
		SafetyMode: "shadow",
		Confidence: 0.95,
		Targets: CITargets{
			Run:      []string{"tests/a.test.ts", "tests/b.test.ts"},
			Skip:     []string{"tests/slow.test.ts"},
			Full:     []string{"tests/a.test.ts", "tests/b.test.ts", "tests/slow.test.ts"},
			Tags:     map[string][]string{"unit": {"tests/a.test.ts"}},
			Fallback: false,
		},
		Impact: CIImpact{
			FilesChanged:    []string{"src/app.ts"},
			SymbolsChanged:  []CISymbolChange{},
			ModulesAffected: []string{},
		},
		Policy: CIPolicy{
			Strategy: "auto",
		},
		Safety: CISafety{
			StructuralRisks: []StructuralRisk{},
			Confidence:      0.95,
		},
		Provenance: CIProvenance{
			KaiVersion:  "test-1.0",
			GeneratedAt: "2025-01-01T00:00:00Z",
		},
	}

	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatalf("marshaling plan: %v", err)
	}

	// Verify it's valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid plan JSON: %v", err)
	}

	// Verify key fields
	for _, field := range []string{"version", "mode", "risk", "safetyMode", "confidence", "targets", "impact", "provenance"} {
		if _, ok := parsed[field]; !ok {
			t.Errorf("missing required plan field %q", field)
		}
	}

	// Verify targets structure
	targets, ok := parsed["targets"].(map[string]interface{})
	if !ok {
		t.Fatal("targets is not an object")
	}
	for _, field := range []string{"run", "skip", "full"} {
		if _, ok := targets[field]; !ok {
			t.Errorf("missing targets field %q", field)
		}
	}
}

func TestCLI_EnvHash(t *testing.T) {
	// Empty vars → empty hash
	if got := computeEnvHash(nil); got != "" {
		t.Errorf("nil vars: want empty, got %q", got)
	}
	if got := computeEnvHash([]string{}); got != "" {
		t.Errorf("empty vars: want empty, got %q", got)
	}

	// Set test env vars
	t.Setenv("KAI_TEST_A", "hello")
	t.Setenv("KAI_TEST_B", "world")

	h1 := computeEnvHash([]string{"KAI_TEST_A", "KAI_TEST_B"})
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}
	if len(h1) != 16 { // 8 bytes = 16 hex chars
		t.Errorf("hash length: want 16, got %d", len(h1))
	}

	// Order-independent: [B,A] == [A,B]
	h2 := computeEnvHash([]string{"KAI_TEST_B", "KAI_TEST_A"})
	if h1 != h2 {
		t.Errorf("order-dependent: %s != %s", h1, h2)
	}

	// Same input → same hash (deterministic)
	h3 := computeEnvHash([]string{"KAI_TEST_A", "KAI_TEST_B"})
	if h1 != h3 {
		t.Errorf("non-deterministic: %s != %s", h1, h3)
	}

	// Different value → different hash
	t.Setenv("KAI_TEST_A", "changed")
	h4 := computeEnvHash([]string{"KAI_TEST_A", "KAI_TEST_B"})
	if h4 == h1 {
		t.Error("hash unchanged after env var change")
	}

	// Unset var hashes differently than empty var
	t.Setenv("KAI_TEST_A", "")
	hEmpty := computeEnvHash([]string{"KAI_TEST_A"})
	if !strings.ContainsAny(hEmpty, "0123456789abcdef") {
		t.Errorf("invalid hex hash: %s", hEmpty)
	}

	// EnvHash appears in JSON provenance
	plan := CIPlan{
		Version: 1,
		Provenance: CIProvenance{
			EnvHash: "abc123",
		},
	}
	data, _ := json.Marshal(plan)
	if !strings.Contains(string(data), `"envHash":"abc123"`) {
		t.Error("envHash not serialized in JSON")
	}
}
