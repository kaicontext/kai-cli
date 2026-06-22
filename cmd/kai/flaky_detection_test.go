package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFlakyScipt creates a shell script that outputs jest-format JSON.
// It fails the first failCount invocations, then passes.
func makeFlakyScript(t *testing.T, dir string, failCount int) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "runner.sh")
	countFile := filepath.Join(dir, "count")
	script := fmt.Sprintf(`#!/bin/sh
COUNT_FILE=%q
C=$(cat "$COUNT_FILE" 2>/dev/null || echo 0)
C=$((C + 1))
echo $C > "$COUNT_FILE"
if [ $C -le %d ]; then
  echo '{"numTotalTests":1,"numPassedTests":0,"numFailedTests":1,"testResults":[{"name":"t1","status":"failed","message":"err"}]}'
  exit 1
fi
echo '{"numTotalTests":1,"numPassedTests":1,"numFailedTests":0,"testResults":[{"name":"t1","status":"passed","message":""}]}'
exit 0
`, countFile, failCount)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return scriptPath
}

// makeAlwaysFailScript creates a script that always fails with jest-format JSON.
func makeAlwaysFailScript(t *testing.T, dir string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "fail_runner.sh")
	script := `#!/bin/sh
echo '{"numTotalTests":1,"numPassedTests":0,"numFailedTests":1,"testResults":[{"name":"t1","status":"failed","message":"always fails"}]}'
exit 1
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return scriptPath
}

// makeAlwaysPassScript creates a script that always passes with jest-format JSON.
func makeAlwaysPassScript(t *testing.T, dir string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "pass_runner.sh")
	script := `#!/bin/sh
echo '{"numTotalTests":1,"numPassedTests":1,"numFailedTests":0,"testResults":[{"name":"t1","status":"passed","message":""}]}'
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return scriptPath
}

func TestFlaky_ConsistentFailure(t *testing.T) {
	dir := t.TempDir()
	script := makeAlwaysFailScript(t, dir)

	failed := []TestFailureDetail{{Name: "t1", ErrorMessage: "always fails"}}
	flaky, real := detectFlakyTests("sh "+script, failed, 3, "jest")

	if len(flaky) != 0 {
		t.Errorf("expected 0 flaky, got %d", len(flaky))
	}
	if len(real) != 1 {
		t.Fatalf("expected 1 real failure, got %d", len(real))
	}
	if real[0].Classification != "real" {
		t.Errorf("expected classification 'real', got %q", real[0].Classification)
	}
	if real[0].Confidence != 1.0 {
		t.Errorf("expected confidence 1.0, got %f", real[0].Confidence)
	}
}

func TestFlaky_AlwaysPasses(t *testing.T) {
	dir := t.TempDir()
	script := makeAlwaysPassScript(t, dir)

	failed := []TestFailureDetail{{Name: "t1", ErrorMessage: "initial failure"}}
	flaky, real := detectFlakyTests("sh "+script, failed, 3, "jest")

	if len(real) != 0 {
		t.Errorf("expected 0 real, got %d", len(real))
	}
	if len(flaky) != 1 {
		t.Fatalf("expected 1 flaky, got %d", len(flaky))
	}
	if flaky[0].Classification != "flaky" {
		t.Errorf("expected classification 'flaky', got %q", flaky[0].Classification)
	}
	if flaky[0].Confidence != 1.0 {
		t.Errorf("expected confidence 1.0, got %f", flaky[0].Confidence)
	}
}

func TestFlaky_PartialFlaky(t *testing.T) {
	dir := t.TempDir()
	// Fails first 1 time, then passes — 1 of 3 retries fails
	script := makeFlakyScript(t, dir, 1)

	failed := []TestFailureDetail{{Name: "t1", ErrorMessage: "err"}}
	flaky, real := detectFlakyTests("sh "+script, failed, 3, "jest")

	if len(real) != 0 {
		t.Errorf("expected 0 real, got %d", len(real))
	}
	if len(flaky) != 1 {
		t.Fatalf("expected 1 flaky, got %d", len(flaky))
	}
	if flaky[0].Classification != "flaky" {
		t.Errorf("expected classification 'flaky', got %q", flaky[0].Classification)
	}
	// 1 fail out of 3 retries → confidence = 1.0 - 1/3 ≈ 0.667
	if flaky[0].Confidence < 0.6 || flaky[0].Confidence > 0.7 {
		t.Errorf("expected confidence ~0.67, got %f", flaky[0].Confidence)
	}
}

func TestFlaky_ZeroRetries(t *testing.T) {
	failed := []TestFailureDetail{{Name: "t1", ErrorMessage: "err"}}
	flaky, real := detectFlakyTests("echo ignored", failed, 0, "jest")

	if flaky != nil {
		t.Errorf("expected nil flaky, got %v", flaky)
	}
	if real != nil {
		t.Errorf("expected nil real, got %v", real)
	}
}

func TestFlaky_EmptyFailedTests(t *testing.T) {
	flaky, real := detectFlakyTests("echo ignored", nil, 3, "jest")

	if flaky != nil {
		t.Errorf("expected nil flaky, got %v", flaky)
	}
	if real != nil {
		t.Errorf("expected nil real, got %v", real)
	}
}

func TestFlaky_TargetedRerun(t *testing.T) {
	dir := t.TempDir()
	// Create a script that echoes its arguments, so we can verify {{tests}} substitution
	scriptPath := filepath.Join(dir, "targeted.sh")
	logPath := filepath.Join(dir, "args.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
echo '{"numTotalTests":1,"numPassedTests":1,"numFailedTests":0,"testResults":[]}'
exit 0
`, logPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	failed := []TestFailureDetail{
		{Name: "test_a", ErrorMessage: "err"},
		{Name: "test_b", ErrorMessage: "err"},
	}
	cmdStr := "sh " + scriptPath + " {{tests}}"
	detectFlakyTests(cmdStr, failed, 1, "jest")

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading args log: %v", err)
	}
	logged := string(logData)
	if logged == "" {
		t.Fatal("script was never called")
	}
	// The {{tests}} placeholder should be replaced with "test_a test_b"
	if !strings.Contains(logged, "test_a") || !strings.Contains(logged, "test_b") {
		t.Errorf("expected test names in command, got: %s", logged)
	}
}
