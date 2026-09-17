package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Deterministic regression tests for the kai-desktop #429 defect, exercising
// the ACTUAL source: the construction statement is read from
// testdata/pr429/app.js.excerpt — copied verbatim from the PR head (see
// PROVENANCE), located by its text, and evaluated under node exactly as
// written, with `wsPath` bound to each literal input and `command` to `pwd`.
// The fidelity harness executes the generated command unchanged and records
// the working directory it leaves behind.
//
// These are regression tests for THIS defect, not a universal review
// solution. Each case states its expected result. A failed directory-equality
// assertion on `$` or a backtick is the defect observation — recorded as a
// completed experiment whose expected behavior failed, distinct from an
// experiment that could not run.
//
//	KAI_REVIEW_SANDBOX_TEST=1 KAI_REVIEW_SANDBOX_IMAGE=node@sha256:<digest> \
//	  GOWORK=off go test ./cmd/kai -run '^TestPR429' -count=1 -v

// rcPR429Statement returns the real construction statement from the vendored
// excerpt. It fails loudly if the excerpt no longer contains it, so a change to
// the source under test cannot silently turn this into a test of something
// else.
func rcPR429Statement(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/pr429/app.js.excerpt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "const full = ") && strings.Contains(line, "JSON.stringify(wsPath)") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("the vendored #429 excerpt no longer contains the construction statement; see testdata/pr429/PROVENANCE:\n%s", raw)
	return ""
}

// shSingleQuote quotes s for POSIX sh so every character, including $ and
// backticks, is literal. Used only to CREATE the test directories.
func shSingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestPR429ActualSourceIsVendoredVerbatim(t *testing.T) {
	stmt := rcPR429Statement(t)
	const want = `const full = wsPath ? 'cd ' + JSON.stringify(wsPath) + ' && ' + command : command;`
	if stmt != want {
		t.Fatalf("construction statement differs from the PR #429 head:\n got %q\nwant %q", stmt, want)
	}
}

func TestPR429ActualSourceWorkingDirectoryByInput(t *testing.T) {
	sb := forensicsSandbox(t)
	stmt := rcPR429Statement(t)
	for _, tc := range []struct {
		name   string
		wsPath string
		// lands: the generated cd reaches the intended directory. false means
		// the defect reproduces for this input.
		lands bool
	}{
		{"plain", "/tmp/ws-plain", true},
		{"space", "/tmp/ws dir", true},
		{"double quote", `/tmp/ws"dir`, true},
		{"single quote", "/tmp/ws'dir", true},
		{"dollar", "/tmp/ws$HOME", false},
		{"command substitution", "/tmp/ws$(id -u)", false},
		{"backtick", "/tmp/ws`echo X`", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lit, _ := json.Marshal(tc.wsPath) // a JS string literal for the binding only
			construct := "const wsPath = " + string(lit) + ";\nconst command = 'pwd';\n" + stmt + "\nprocess.stdout.write(full);\n"
			input, _ := json.Marshal(rcExperimentParams{
				Setup:      "mkdir -p " + shSingleQuote(tc.wsPath),
				Construct:  construct,
				Assertions: []rcAssertion{{Kind: "pwd", Value: tc.wsPath}},
			})
			rec, err := sb.runExperiment(context.Background(), string(input))
			if err != nil || !rec.completed() {
				t.Fatalf("experiment could not run (not an observation): rec=%+v err=%v", rec, err)
			}
			t.Logf("generated=%q exit=%d observedPwd=%q assertion=%+v", rec.GeneratedCommand, rec.ExitCode, rec.ObservedPWD, rec.Assertions[0])
			// The harness must have executed the real statement's output.
			if !strings.HasPrefix(rec.GeneratedCommand, "cd ") || !strings.HasSuffix(rec.GeneratedCommand, " && pwd") {
				t.Fatalf("generated command is not the statement's output: %q", rec.GeneratedCommand)
			}
			got := rec.Assertions[0].Passed
			if got != tc.lands {
				t.Fatalf("input %q: expected lands=%v, observed pwd %q (exit %d)", tc.wsPath, tc.lands, rec.ObservedPWD, rec.ExitCode)
			}
			if !tc.lands && rec.ExitCode == 0 && rec.ObservedPWD == tc.wsPath {
				t.Fatalf("input %q: expected the defect to reproduce", tc.wsPath)
			}
		})
	}
}
