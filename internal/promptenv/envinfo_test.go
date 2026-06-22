package promptenv

import (
	"strings"
	"testing"
)

func TestComputeEnvInfo_ContainsExpectedSections(t *testing.T) {
	got := ComputeEnvInfo("claude-opus-4-7", nil)

	wantSubstrings := []string{
		"Working directory:",
		"Is directory a git repo:",
		"Platform:",
		"Shell:",
		"OS Version:",
		"<env>",
		"</env>",
		"You are powered by the model claude-opus-4-7.",
		"Assistant knowledge cutoff is January 2026.",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("ComputeEnvInfo output missing %q\nfull output:\n%s", w, got)
		}
	}
}

func TestComputeEnvInfo_AdditionalDirectories(t *testing.T) {
	got := ComputeEnvInfo("", []string{"/foo", "/bar"})
	if !strings.Contains(got, "Additional working directories: /foo, /bar") {
		t.Errorf("additional dirs not rendered:\n%s", got)
	}
}

func TestComputeEnvInfo_NoModelID(t *testing.T) {
	got := ComputeEnvInfo("", nil)
	if strings.Contains(got, "You are powered by") {
		t.Errorf("model line should not render without a modelID:\n%s", got)
	}
	if strings.Contains(got, "knowledge cutoff") {
		t.Errorf("cutoff line should not render without a modelID:\n%s", got)
	}
}

func TestComputeEnvInfo_UnknownModelID_NoFakeCutoff(t *testing.T) {
	got := ComputeEnvInfo("some-unrecognized-model", nil)
	if !strings.Contains(got, "You are powered by the model some-unrecognized-model.") {
		t.Errorf("model line should render even for unrecognized IDs:\n%s", got)
	}
	if strings.Contains(got, "knowledge cutoff") {
		t.Errorf("cutoff line must NOT render for unrecognized models (avoid hallucinated dates):\n%s", got)
	}
}

func TestKnowledgeCutoff(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-7":             "January 2026",
		"claude-sonnet-4-6":           "August 2025",
		"claude-opus-4-6":             "May 2025",
		"claude-haiku-4-5-20251001":   "October 2025",
		"claude-haiku-4":              "February 2025",
		"qwen-something":              "",
		"":                            "",
	}
	for in, want := range cases {
		if got := KnowledgeCutoff(in); got != want {
			t.Errorf("KnowledgeCutoff(%q) = %q, want %q", in, got, want)
		}
	}
}
