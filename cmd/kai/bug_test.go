package main

import (
	"strings"
	"testing"
)

func TestBugCmd_PrintsVersionAndOS(t *testing.T) {
	bugNoBrowser = true
	defer func() { bugNoBrowser = false }()

	out := captureStdout(t, func() {
		cmd := *bugCmd
		cmd.RunE(&cmd, nil)
	})

	if !strings.Contains(out, "## Bug Report") {
		t.Errorf("bug output missing ## Bug Report header, got %q", out)
	}
	if !strings.Contains(out, Version) {
		t.Errorf("bug output missing version %q, got %q", Version, out)
	}
	if !strings.Contains(out, "/") {
		t.Errorf("bug output missing OS/arch separator '/', got %q", out)
	}
}

func TestBugCmd_ContainsIssueTrackerURL(t *testing.T) {
	bugNoBrowser = true
	defer func() { bugNoBrowser = false }()

	var runErr error
	out := captureStdout(t, func() {
		cmd := *bugCmd
		runErr = cmd.RunE(&cmd, nil)
	})

	if runErr != nil {
		t.Fatalf("runBug returned error: %v", runErr)
	}
	if !strings.Contains(out, "### Description") {
		t.Errorf("bug output missing ### Description section, got %q", out)
	}
	if !strings.Contains(out, "### Steps to Reproduce") {
		t.Errorf("bug output missing ### Steps to Reproduce section, got %q", out)
	}
	if !strings.Contains(out, "### Environment") {
		t.Errorf("bug output missing ### Environment section, got %q", out)
	}
}
