package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShadow_MetricsSafeReduction(t *testing.T) {
	selective := &ShadowRunResult{
		TotalTests:  5,
		Passed:      5,
		FailedTests: []TestFailureDetail{},
		DurationS:   2.0,
	}
	full := &ShadowRunResult{
		TotalTests:  20,
		Passed:      20,
		FailedTests: []TestFailureDetail{},
		DurationS:   10.0,
	}
	plan := &CIPlan{
		Targets: CITargets{
			Run: []string{"a.test.ts", "b.test.ts", "c.test.ts", "d.test.ts", "e.test.ts"},
		},
	}

	m := computeShadowMetrics(selective, full, plan, nil)

	if m.TestsReduced != 15 {
		t.Errorf("TestsReduced: want 15, got %d", m.TestsReduced)
	}
	if m.TestsReducedPct != 75.0 {
		t.Errorf("TestsReducedPct: want 75.0, got %f", m.TestsReducedPct)
	}
	if m.TimeSavedS != 8.0 {
		t.Errorf("TimeSavedS: want 8.0, got %f", m.TimeSavedS)
	}
	if m.FalseNegatives != 0 {
		t.Errorf("FalseNegatives: want 0, got %d", m.FalseNegatives)
	}
	if m.Accuracy != 1.0 {
		t.Errorf("Accuracy: want 1.0, got %f", m.Accuracy)
	}
}

func TestShadow_MetricsWithFalseNegatives(t *testing.T) {
	selective := &ShadowRunResult{
		TotalTests:  3,
		Passed:      3,
		FailedTests: []TestFailureDetail{},
		DurationS:   1.0,
	}
	full := &ShadowRunResult{
		TotalTests: 10,
		Passed:     8,
		FailedTests: []TestFailureDetail{
			{Name: "missed_test_1", ErrorMessage: "fail"},
			{Name: "missed_test_2", ErrorMessage: "fail"},
		},
		DurationS: 5.0,
	}
	plan := &CIPlan{
		Targets: CITargets{
			Run: []string{"a.test.ts", "b.test.ts", "c.test.ts"},
		},
	}

	m := computeShadowMetrics(selective, full, plan, nil)

	if m.FalseNegatives != 2 {
		t.Errorf("FalseNegatives: want 2, got %d", m.FalseNegatives)
	}
	// 2 non-flaky failures, 2 missed → accuracy = 1 - 2/2 = 0
	if m.Accuracy != 0.0 {
		t.Errorf("Accuracy: want 0.0, got %f", m.Accuracy)
	}
}

func TestShadow_MetricsWithFlakyExclusion(t *testing.T) {
	selective := &ShadowRunResult{
		TotalTests:  3,
		Passed:      3,
		FailedTests: []TestFailureDetail{},
		DurationS:   1.0,
	}
	full := &ShadowRunResult{
		TotalTests: 10,
		Passed:     8,
		FailedTests: []TestFailureDetail{
			{Name: "flaky_test", ErrorMessage: "intermittent"},
			{Name: "real_miss", ErrorMessage: "real bug"},
		},
		DurationS: 5.0,
	}
	plan := &CIPlan{
		Targets: CITargets{
			Run: []string{"a.test.ts", "b.test.ts", "c.test.ts"},
		},
	}
	flakyTests := []FlakyTestDetail{
		{Name: "flaky_test", Classification: "flaky"},
	}

	m := computeShadowMetrics(selective, full, plan, flakyTests)

	// Only real_miss is a false negative (flaky_test excluded)
	if m.FalseNegatives != 1 {
		t.Errorf("FalseNegatives: want 1, got %d", m.FalseNegatives)
	}
	// 1 non-flaky failure (real_miss), 1 missed → accuracy = 1 - 1/1 = 0
	if m.Accuracy != 0.0 {
		t.Errorf("Accuracy: want 0.0, got %f", m.Accuracy)
	}
}

func TestShadow_MetricsFalseNegativeSelectedNotMissed(t *testing.T) {
	selective := &ShadowRunResult{
		TotalTests: 5,
		Passed:     4,
		FailedTests: []TestFailureDetail{
			{Name: "caught_test", ErrorMessage: "fail"},
		},
		DurationS: 2.0,
	}
	full := &ShadowRunResult{
		TotalTests: 10,
		Passed:     9,
		FailedTests: []TestFailureDetail{
			{Name: "caught_test", ErrorMessage: "fail"},
		},
		DurationS: 5.0,
	}
	plan := &CIPlan{
		Targets: CITargets{
			Run: []string{"caught_test", "a.test.ts", "b.test.ts", "c.test.ts", "d.test.ts"},
		},
	}

	m := computeShadowMetrics(selective, full, plan, nil)

	if m.FalseNegatives != 0 {
		t.Errorf("FalseNegatives: want 0, got %d", m.FalseNegatives)
	}
	if m.Accuracy != 1.0 {
		t.Errorf("Accuracy: want 1.0, got %f", m.Accuracy)
	}
}

func TestShadow_MarkdownOutput(t *testing.T) {
	report := ShadowReport{
		Version:     1,
		GeneratedAt: "2025-01-01T00:00:00Z",
		KaiVersion:  "test-1.0",
		GitRange:    "abc123..def456",
		Verdict:     ShadowVerdictSafe,
		SelectiveRun: &ShadowRunResult{
			Command:     "npm test -- a.test.ts",
			ExitCode:    0,
			DurationS:   2.5,
			TotalTests:  5,
			Passed:      5,
			FailedTests: []TestFailureDetail{},
		},
		FullRun: &ShadowRunResult{
			Command:     "npm test",
			ExitCode:    0,
			DurationS:   10.0,
			TotalTests:  20,
			Passed:      20,
			FailedTests: []TestFailureDetail{},
		},
		Metrics: ShadowMetrics{
			TestsReduced:    15,
			TestsReducedPct: 75.0,
			TimeSavedS:      7.5,
			TimeSavedPct:    75.0,
			FalseNegatives:  0,
			Accuracy:        1.0,
		},
		Flaky:    ShadowFlakyInfo{},
		Fallback: ShadowFallbackInfo{},
	}

	mdPath := filepath.Join(t.TempDir(), "report.md")
	if err := writeShadowMarkdown(report, mdPath); err != nil {
		t.Fatalf("writeShadowMarkdown: %v", err)
	}

	data, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}

	md := string(data)
	checks := []string{
		"# Shadow Run Report",
		"**Verdict:** OK safe",
		"abc123..def456",
		"test-1.0",
		"Tests reduced | 15 (75.0%)",
		"Time saved | 7.5s (75.0%)",
		"False negatives | 0",
		"Accuracy | 100.0%",
		"## Selective Run",
		"## Full Run",
	}
	for _, check := range checks {
		if !strings.Contains(md, check) {
			t.Errorf("markdown missing %q", check)
		}
	}

	if strings.Contains(md, "## Flaky Tests") {
		t.Error("should not have Flaky Tests section when none detected")
	}
	if strings.Contains(md, "## Fallback") {
		t.Error("should not have Fallback section when not triggered")
	}
}

func TestShadow_MarkdownWithFlaky(t *testing.T) {
	report := ShadowReport{
		Version:     1,
		GeneratedAt: "2025-01-01T00:00:00Z",
		KaiVersion:  "test-1.0",
		GitRange:    "abc..def",
		Verdict:     ShadowVerdictFlakySuspect,
		SelectiveRun: &ShadowRunResult{
			Command:     "test",
			TotalTests:  5,
			Passed:      5,
			FailedTests: []TestFailureDetail{},
		},
		FullRun: &ShadowRunResult{
			Command:    "test",
			TotalTests: 10,
			Passed:     9,
			FailedTests: []TestFailureDetail{
				{Name: "flaky_one", ErrorMessage: "sometimes fails"},
			},
		},
		Metrics: ShadowMetrics{Accuracy: 1.0},
		Flaky: ShadowFlakyInfo{
			Detected: true,
			FlakyTests: []FlakyTestDetail{
				{Name: "flaky_one", Classification: "flaky", Confidence: 0.67, FailCount: 1, TotalRetries: 3, ErrorMessage: "sometimes fails"},
			},
			RealTests: []FlakyTestDetail{},
			Retries:   3,
		},
	}

	mdPath := filepath.Join(t.TempDir(), "report.md")
	if err := writeShadowMarkdown(report, mdPath); err != nil {
		t.Fatalf("writeShadowMarkdown: %v", err)
	}

	data, _ := os.ReadFile(mdPath)
	md := string(data)

	if !strings.Contains(md, "## Flaky Tests") {
		t.Error("expected Flaky Tests section")
	}
	if !strings.Contains(md, "flaky_one") {
		t.Error("expected flaky test name in output")
	}
	if !strings.Contains(md, "FLAKY") {
		t.Error("expected FLAKY verdict emoji")
	}
}

func TestShadow_MarkdownWithFallback(t *testing.T) {
	report := ShadowReport{
		Version:     1,
		GeneratedAt: "2025-01-01T00:00:00Z",
		KaiVersion:  "test-1.0",
		GitRange:    "abc..def",
		Verdict:     ShadowVerdictFallback,
		Metrics:     ShadowMetrics{Accuracy: 1.0},
		Flaky:       ShadowFlakyInfo{},
		Fallback: ShadowFallbackInfo{
			Triggered:  true,
			Reason:     "Panic switch activated",
			Confidence: 0.8,
		},
	}

	mdPath := filepath.Join(t.TempDir(), "report.md")
	if err := writeShadowMarkdown(report, mdPath); err != nil {
		t.Fatalf("writeShadowMarkdown: %v", err)
	}

	data, _ := os.ReadFile(mdPath)
	md := string(data)

	if !strings.Contains(md, "## Fallback") {
		t.Error("expected Fallback section")
	}
	if !strings.Contains(md, "Panic switch activated") {
		t.Error("expected fallback reason")
	}
}

func TestShadow_MarkdownWithFalseNegatives(t *testing.T) {
	report := ShadowReport{
		Version:     1,
		GeneratedAt: "2025-01-01T00:00:00Z",
		KaiVersion:  "test-1.0",
		GitRange:    "abc..def",
		Verdict:     ShadowVerdictMissed,
		Plan: &CIPlan{
			Targets: CITargets{
				Run: []string{"selected.test.ts"},
			},
		},
		FullRun: &ShadowRunResult{
			Command:    "test",
			TotalTests: 10,
			Passed:     8,
			FailedTests: []TestFailureDetail{
				{Name: "selected.test.ts", ErrorMessage: "caught"},
				{Name: "missed.test.ts", ErrorMessage: "not caught"},
			},
		},
		Metrics: ShadowMetrics{
			FalseNegatives: 1,
			Accuracy:       0.5,
		},
		Flaky: ShadowFlakyInfo{},
	}

	mdPath := filepath.Join(t.TempDir(), "report.md")
	if err := writeShadowMarkdown(report, mdPath); err != nil {
		t.Fatalf("writeShadowMarkdown: %v", err)
	}

	data, _ := os.ReadFile(mdPath)
	md := string(data)

	if !strings.Contains(md, "## False Negatives") {
		t.Error("expected False Negatives section")
	}
	if !strings.Contains(md, "missed.test.ts") {
		t.Error("expected missed test in false negatives")
	}
	// selected.test.ts was in Run, so should NOT appear in false negatives listing
	// (the markdown lists tests from FullRun.FailedTests that are NOT in selectedSet)
	fnSection := md[strings.Index(md, "## False Negatives"):]
	if strings.Contains(fnSection, "selected.test.ts") {
		t.Error("selected test should not appear in false negatives section")
	}
}

func TestShadow_VerdictDetermination(t *testing.T) {
	tests := []struct {
		name           string
		falseNegatives int
		flakyCount     int
		fallback       bool
		wantVerdict    ShadowVerdict
	}{
		{
			name:        "safe: no issues",
			wantVerdict: ShadowVerdictSafe,
		},
		{
			name:           "missed: false negatives",
			falseNegatives: 2,
			wantVerdict:    ShadowVerdictMissed,
		},
		{
			name:        "flaky suspect: flaky detected, no misses",
			flakyCount:  1,
			wantVerdict: ShadowVerdictFlakySuspect,
		},
		{
			name:        "fallback: triggered, no flaky, no misses",
			fallback:    true,
			wantVerdict: ShadowVerdictFallback,
		},
		{
			name:           "missed takes priority over flaky",
			falseNegatives: 1,
			flakyCount:     2,
			wantVerdict:    ShadowVerdictMissed,
		},
		{
			name:           "missed takes priority over fallback",
			falseNegatives: 1,
			fallback:       true,
			wantVerdict:    ShadowVerdictMissed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := ShadowMetrics{FalseNegatives: tt.falseNegatives}
			var flakyDetails []FlakyTestDetail
			for i := 0; i < tt.flakyCount; i++ {
				flakyDetails = append(flakyDetails, FlakyTestDetail{Name: "f"})
			}
			fallbackInfo := ShadowFallbackInfo{Triggered: tt.fallback}

			// Replicate the verdict logic from runShadowRun
			verdict := ShadowVerdictSafe
			if metrics.FalseNegatives > 0 {
				verdict = ShadowVerdictMissed
			} else if len(flakyDetails) > 0 {
				verdict = ShadowVerdictFlakySuspect
			} else if fallbackInfo.Triggered {
				verdict = ShadowVerdictFallback
			}

			if verdict != tt.wantVerdict {
				t.Errorf("verdict: want %s, got %s", tt.wantVerdict, verdict)
			}
		})
	}
}
