package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestRunWsCurrentJSON(t *testing.T) {
	// Save and restore stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	wsCurrentJSON = true
	runWsCurrent(nil, nil)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("expected valid JSON output, got: %q, err: %v", output, err)
	}
	if _, ok := result["workspace"]; !ok {
		t.Errorf("expected JSON output to contain 'workspace' key, got: %v", result)
	}
}

func TestRunWsCurrentPlainText(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	wsCurrentJSON = false
	runWsCurrent(nil, nil)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Plain text output should NOT be valid JSON object
	var result map[string]interface{}
	err := json.Unmarshal([]byte(output), &result)
	if err == nil {
		t.Errorf("expected plain text output (not JSON), but got valid JSON: %q", output)
	}
}