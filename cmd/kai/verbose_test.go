package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestVerboseFlag tests that the --verbose flag is registered on root command
func TestVerboseFlag(t *testing.T) {
	flag := rootCmd.PersistentFlags().Lookup("verbose")
	if flag == nil {
		t.Fatal("rootCmd should have --verbose persistent flag")
	}
	if flag.Shorthand != "v" {
		t.Errorf("--verbose should have shorthand 'v', got %q", flag.Shorthand)
	}
	if flag.DefValue != "false" {
		t.Errorf("--verbose should default to false, got %s", flag.DefValue)
	}
}

// TestDebugf_VerboseOn tests that debugf writes to stderr when verbose is true
func TestDebugf_VerboseOn(t *testing.T) {
	// Save and restore verbose state
	oldVerbose := verbose
	defer func() { verbose = oldVerbose }()

	verbose = true

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	debugf("test message %d", 42)

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "[kai debug]") {
		t.Errorf("expected '[kai debug]' prefix, got: %s", output)
	}
	if !strings.Contains(output, "test message 42") {
		t.Errorf("expected 'test message 42' in output, got: %s", output)
	}
	if !strings.HasSuffix(output, "\n") {
		t.Errorf("expected output to end with newline, got: %q", output)
	}
}

// TestDebugf_VerboseOff tests that debugf produces no output when verbose is false
func TestDebugf_VerboseOff(t *testing.T) {
	oldVerbose := verbose
	defer func() { verbose = oldVerbose }()

	verbose = false

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	debugf("should not appear %d", 42)

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if output != "" {
		t.Errorf("expected no output when verbose=false, got: %q", output)
	}
}

// TestDebugf_Timestamp tests that debugf output includes a timestamp
func TestDebugf_Timestamp(t *testing.T) {
	oldVerbose := verbose
	defer func() { verbose = oldVerbose }()

	verbose = true

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	debugf("timestamp check")

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Should contain HH:MM:SS.mmm format between prefix and message
	// e.g. "[kai debug] 14:30:05.123 timestamp check\n"
	parts := strings.SplitN(output, " ", 4)
	if len(parts) < 4 {
		t.Fatalf("expected at least 4 space-separated parts, got %d: %q", len(parts), output)
	}
	timestamp := parts[2]
	// Check timestamp looks like HH:MM:SS.mmm
	if len(timestamp) < 10 || timestamp[2] != ':' || timestamp[5] != ':' {
		t.Errorf("expected timestamp in HH:MM:SS.mmm format, got %q", timestamp)
	}
}

// TestDebugf_FormatStrings tests that debugf handles various format strings
func TestDebugf_FormatStrings(t *testing.T) {
	oldVerbose := verbose
	defer func() { verbose = oldVerbose }()

	verbose = true

	tests := []struct {
		name     string
		format   string
		args     []any
		contains string
	}{
		{"string arg", "hello %s", []any{"world"}, "hello world"},
		{"int arg", "count=%d", []any{42}, "count=42"},
		{"float arg", "score=%.2f", []any{0.95}, "score=0.95"},
		{"multiple args", "%s has %d items", []any{"list", 3}, "list has 3 items"},
		{"no args", "simple message", nil, "simple message"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldStderr := os.Stderr
			r, w, _ := os.Pipe()
			os.Stderr = w

			debugf(tt.format, tt.args...)

			w.Close()
			os.Stderr = oldStderr

			var buf bytes.Buffer
			buf.ReadFrom(r)
			output := buf.String()

			if !strings.Contains(output, tt.contains) {
				t.Errorf("expected output to contain %q, got: %q", tt.contains, output)
			}
		})
	}
}

// TestKAIVerboseEnvVar tests that KAI_VERBOSE env var enables verbose mode
func TestKAIVerboseEnvVar(t *testing.T) {
	oldVerbose := verbose
	defer func() { verbose = oldVerbose }()

	tests := []struct {
		name     string
		envValue string
		want     bool
	}{
		{"set to 1", "1", true},
		{"set to true", "true", true},
		{"set to 0", "0", false},
		{"set to false", "false", false},
		{"set to empty", "", false},
		{"set to yes", "yes", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verbose = false
			os.Setenv("KAI_VERBOSE", tt.envValue)
			defer os.Unsetenv("KAI_VERBOSE")

			// Simulate PersistentPreRun logic
			rootCmd.PersistentPreRun(rootCmd, nil)

			if verbose != tt.want {
				t.Errorf("KAI_VERBOSE=%q: verbose=%v, want %v", tt.envValue, verbose, tt.want)
			}
		})
	}
}

// TestKAIVerboseEnvVar_FlagTakesPrecedence tests that --verbose flag takes precedence
func TestKAIVerboseEnvVar_FlagTakesPrecedence(t *testing.T) {
	oldVerbose := verbose
	defer func() { verbose = oldVerbose }()

	// If flag is already set to true, env var should not override
	verbose = true
	os.Setenv("KAI_VERBOSE", "0")
	defer os.Unsetenv("KAI_VERBOSE")

	rootCmd.PersistentPreRun(rootCmd, nil)

	if !verbose {
		t.Error("--verbose flag should take precedence over KAI_VERBOSE=0")
	}
}
