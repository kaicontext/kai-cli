package views

import (
	"strings"
	"testing"
)

// TestValidatorPrompt_HollowGreenLens pins the hollow-green guidance: a
// passing test only counts as proof when it exercises the real
// production path. Guards against a future edit silently dropping it —
// the 2026-06-08 validation run marked a feature "done" off a test that
// used transliterated keys while production loaded a Hebrew-keyed file.
func TestValidatorPrompt_HollowGreenLens(t *testing.T) {
	p := validatorSystemPrompt
	for _, want := range []string{
		"GREEN TESTS ARE NOT PROOF",
		"SKIPPED",
		"PRODUCTION source of truth",
		"reach the CHANGED function",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("validator prompt missing hollow-green marker %q", want)
		}
	}
}

// TestParseValidatorOutputFailsClosed pins the safety property: the
// validator passes ONLY on an explicit, critique-backed VERDICT: PASS.
// Empty output, a verdict-less reply, or a PASS with no supporting
// critique must NOT pass — the 2026-06-01 trace had an 8-second empty
// result default to PASS and let a wrong answer through.
func TestParseValidatorOutputFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantPass bool
	}{
		{
			name:     "empty output fails closed",
			text:     "",
			wantPass: false,
		},
		{
			name:     "no verdict line fails closed",
			text:     "I looked at some files and it seems fine.",
			wantPass: false,
		},
		{
			name:     "PASS with no critique fails closed",
			text:     "VERDICT: PASS",
			wantPass: false,
		},
		{
			name:     "explicit PASS with critique passes",
			text:     "CRITIQUE: I ran the command and it succeeded with the expected output.\nVERDICT: PASS",
			wantPass: true,
		},
		{
			name:     "explicit FAIL with critique does not pass",
			text:     "CRITIQUE: I ran the cited command and it returned unknown flag.\nVERDICT: FAIL\nRETRY_HINT: fixing it now.",
			wantPass: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			critique, pass, _ := parseValidatorOutput(tc.text)
			if pass != tc.wantPass {
				t.Fatalf("parseValidatorOutput(%q) pass=%v, want %v", tc.text, pass, tc.wantPass)
			}
			// A non-passing verdict must always carry a critique so the
			// user/retry has something to act on — never a silent empty FAIL.
			if !pass && strings.TrimSpace(critique) == "" {
				t.Fatalf("parseValidatorOutput(%q) returned empty critique on non-pass", tc.text)
			}
		})
	}
}

// TestParseValidatorMultiLineCritique pins the 2026-06-01 bug: the
// model put the CRITIQUE content on the line(s) AFTER the "CRITIQUE:"
// label, the same-line-only parser read it as empty, and fail-closed
// replaced a correct grounded FAIL with a generic "couldn't verify"
// message. The real critique must survive parsing.
func TestParseValidatorMultiLineCritique(t *testing.T) {
	// Exact shape from the trace: label alone, content on following
	// lines, blank line, then VERDICT and RETRY_HINT.
	out := "CRITIQUE:  \nI ran the actual spawn command kai live --json and it returned Error: unknown flag: --json with exit code 1.\nThe IPC chain downstream is dead on arrival.\n\nVERDICT: FAIL\n\nRETRY_HINT: The live subcommand does not accept --json; checking the real output format now."
	critique, pass, hint := parseValidatorOutput(out)
	if pass {
		t.Fatalf("expected FAIL, got pass=true")
	}
	if !strings.Contains(critique, "unknown flag") {
		t.Fatalf("multi-line critique was lost; got %q", critique)
	}
	if !strings.Contains(hint, "output format") {
		t.Fatalf("multi-line retry hint was lost; got %q", hint)
	}

	// Multi-line PASS must also carry its real critique through.
	okOut := "CRITIQUE:\nI ran kit live status and it returned the expected rollup with the fields the code parses.\nVERDICT: PASS"
	c2, p2, _ := parseValidatorOutput(okOut)
	if !p2 {
		t.Fatalf("expected PASS for a multi-line critique-backed PASS, got fail; critique=%q", c2)
	}
	if !strings.Contains(c2, "expected rollup") {
		t.Fatalf("multi-line PASS critique was lost; got %q", c2)
	}
}

// TestShouldValidateClaims pins the routing between the tool-using
// validator and the prose critic. Replies that make checkable claims
// about the codebase (cite a file, or answer a workspace question)
// must route to the validator; conceptual replies with nothing to
// run must not.
func TestShouldValidateClaims(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		request string
		want    bool
	}{
		{
			// The canonical miss: an answer asserting how a wiring
			// works, citing concrete files. Must be validated.
			name:    "cites files about wiring",
			reply:   "ActivityBar.svelte reads the event from preload, and main.cjs spawns kai live to stream it. The wiring is correct.",
			request: "is the activity bar updating correctly?",
			want:    true,
		},
		{
			// Workspace question even if the reply happens not to
			// name a path — the request shape routes it.
			name:    "workspace question by request shape",
			reply:   "Yes, it is wired up and working as intended end to end.",
			request: "how does this repo handle live updates?",
			want:    true,
		},
		{
			// Pure conceptual answer, no artifact to run/grep/read.
			name:    "conceptual answer, nothing to run",
			reply:   "In general, event-driven systems decouple producers from consumers using a message bus.",
			request: "what is an event bus?",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldValidateClaims(tc.reply, tc.request); got != tc.want {
				t.Fatalf("shouldValidateClaims(%q, %q) = %v, want %v", tc.reply, tc.request, got, tc.want)
			}
		})
	}
}

func TestIsStructuralQuestion(t *testing.T) {
	yes := []string{
		"List everything that would break if I delete: LocalRuntime.runStreamLoop",
		"who calls runTurn?",
		"what depends on the Client interface?",
		"is parseConfig dead code / unused?",
		"what's the blast radius of changing Run's signature?",
	}
	for _, q := range yes {
		if !isStructuralQuestion(q) {
			t.Errorf("expected structural: %q", q)
		}
	}
	no := []string{
		"what is this repo?",
		"fix the microphone network error",
		"does kai live --json work?",
	}
	for _, q := range no {
		if isStructuralQuestion(q) {
			t.Errorf("expected NOT structural: %q", q)
		}
	}
}
