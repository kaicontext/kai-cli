package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Deterministic replays of the experiments GLM-5.2 ran in the end-to-end #429
// run that REFUTED the real JSON.stringify defect (e2e R5 attempt 3; see
// docs/review-evidence-glm-runs.md). Each replay states its expected result, so
// a bad experiment can be told apart from a bad reading of a sound one. These
// run in the actual restricted container and need a Node-capable image:
//
//	KAI_REVIEW_SANDBOX_TEST=1 KAI_REVIEW_SANDBOX_IMAGE=node@sha256:<digest> \
//	  GOWORK=off go test ./cmd/kai -run '^TestForensics' -count=1 -v
//
// They do not change the gate. They pin what the evidence actually shows.
func forensicsSandbox(t *testing.T) *rcShellSandbox {
	t.Helper()
	if os.Getenv("KAI_REVIEW_SANDBOX_TEST") != "1" {
		t.Skip("set KAI_REVIEW_SANDBOX_TEST=1 with a preloaded Node-capable KAI_REVIEW_SANDBOX_IMAGE")
	}
	sb := rcConfiguredSandbox()
	if sb == nil {
		t.Fatal("KAI_REVIEW_SANDBOX_IMAGE required")
	}
	return sb
}

func forensicsRun(t *testing.T, sb *rcShellSandbox, script string) string {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"script": script})
	got, err := sb.run(context.Background(), string(input))
	if err != nil {
		t.Fatalf("experiment did not run: %v", err)
	}
	t.Logf("---- script ----\n%s---- result ----\n%s", script, got)
	return got
}

// GLM's Source 3, "Tests 2–4", replayed exactly as it wrote them. The paths
// inside `sh -c '…'` carry BACKSLASH-ESCAPED metacharacters (a backslash before
// the dollar sign and before each backtick), which
// POSIX sh reads as literals inside double quotes. So every one succeeds —
// EXPECTED. But JSON.stringify never emits those backslashes, so this
// experiment tested a hand-escaped string, not the code's output. This is a
// construction error: the experiment did not reproduce the alleged input.
func TestForensicsGLMEscapedCDTestsSucceedBecauseTheyAreHandEscaped(t *testing.T) {
	sb := forensicsSandbox(t)
	script := `mkdir -p "/tmp/test\$dir"
mkdir -p '/tmp/test` + "`dir`" + `'
mkdir -p '/tmp/test$(dir)'
sh -c 'cd "/tmp/test\$dir" && pwd && echo "SUCCESS: dollar"' || echo "FAILED: dollar"
sh -c 'cd "/tmp/test\` + "`dir\\`" + `" && pwd && echo "SUCCESS: backticks"' || echo "FAILED: backticks"
sh -c 'cd "/tmp/test\$(dir)" && pwd && echo "SUCCESS: command-sub"' || echo "FAILED: command-sub"
`
	got := forensicsRun(t, sb, script)
	for _, want := range []string{"SUCCESS: dollar", "SUCCESS: backticks", "SUCCESS: command-sub"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected GLM's hand-escaped test to succeed (%q) — it tests an escaped literal, not JSON.stringify output:\n%s", want, got)
		}
	}
}

// The corrected counterpart: build the cd command the way the code under
// review does — 'cd ' + JSON.stringify(wsPath) + ' && pwd' — with the REAL
// JSON.stringify output (no backslash before $), against a directory literally
// named test$dir. EXPECTED: $dir expands (to empty), the cd targets
// /tmp/test/…, which does not exist, so the cd FAILS and pwd is not reached.
// The alleged behavior reproduces. GLM never ran this variant.
func TestForensicsRealJSONStringifyOutputMisdirectsCD(t *testing.T) {
	sb := forensicsSandbox(t)
	script := `mkdir -p '/tmp/test$dir'
node -e 'process.stdout.write("cd " + JSON.stringify("/tmp/test$dir") + " && pwd && echo REACHED")' > /tmp/cmd.sh
echo "generated: $(cat /tmp/cmd.sh)"
sh /tmp/cmd.sh || echo "CD_FAILED exit=$?"
echo "---- single-quoted control ----"
sh -c "cd '/tmp/test\$dir' && pwd && echo REACHED_LITERAL"
`
	got := forensicsRun(t, sb, script)
	if !strings.Contains(got, `generated: cd "/tmp/test$dir" && pwd && echo REACHED`) {
		t.Fatalf("JSON.stringify did not emit the unescaped $ the code emits:\n%s", got)
	}
	if !strings.Contains(got, "CD_FAILED") || strings.Contains(got, "\nREACHED\n") {
		t.Fatalf("expected the real JSON.stringify-built cd to be misdirected and fail:\n%s", got)
	}
	if !strings.Contains(got, "REACHED_LITERAL") {
		t.Fatalf("single-quoted control should reach the literal directory:\n%s", got)
	}
}

// GLM's Source 2, replayed exactly: print JSON.stringify for a list of paths.
// This experiment is SOUND. Its output plainly shows `$`, backticks and `$()`
// passing through UNESCAPED inside the double-quoted cd argument, while `\`
// and `"` are escaped. GLM cited these lines as showing the quoting is safe.
// That is a misreading of a correct experiment: the evidence supports the
// allegation.
func TestForensicsGLMSource2ShowsMetacharactersUnescaped(t *testing.T) {
	sb := forensicsSandbox(t)
	script := `cat << 'EOF' > /tmp/test_json_escape.js
const testPaths = [
  "/normal/path",
  "/path/with spaces",
  "/path/with$dollar",
  "/path/with` + "`backticks`" + `",
  "/path/with$(command)",
  "/path/with'quotes'",
  '/path/with"doublequotes"',
  "/path/with\\backslash"
];
testPaths.forEach(path => {
  const jsonEscaped = JSON.stringify(path);
  console.log(` + "`" + `cd command: cd ${jsonEscaped} && echo test` + "`" + `);
});
EOF
node /tmp/test_json_escape.js
`
	got := forensicsRun(t, sb, script)
	for _, want := range []string{
		`cd "/path/with$dollar" && echo test`,          // $ unescaped
		"cd \"/path/with`backticks`\" && echo test",    // backtick unescaped
		`cd "/path/with$(command)" && echo test`,       // $() unescaped
		`cd "/path/with\\backslash" && echo test`,      // backslash IS escaped
		`cd "/path/with\"doublequotes\"" && echo test`, // quotes ARE escaped
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected JSON.stringify output %q:\n%s", want, got)
		}
	}
}

// The #429 case in FIDELITY mode: the harness runs the code's own construction
// and feeds the generated string verbatim into sh, then evaluates explicit
// assertions. EXPECTED: with the real JSON.stringify output, the assertion that
// the cd lands in the intended directory FAILS and the exit assertion shows the
// cd failed — so this experiment cannot back "the quoting is safe"; the
// single-quote fix, asserted the same way, PASSES. The model is not in the loop
// between construction and execution.
func TestForensicsFidelityModeAssertsPathEqualityForJSONStringify(t *testing.T) {
	sb := forensicsSandbox(t)
	run := func(construct string, assertions []rcAssertion) *rcExperimentRecord {
		t.Helper()
		input, _ := json.Marshal(rcExperimentParams{Setup: "mkdir -p '/tmp/test$dir'", Construct: construct, Assertions: assertions})
		rec, err := sb.runExperiment(context.Background(), string(input))
		if err != nil {
			t.Fatalf("fidelity experiment did not run: %v", err)
		}
		t.Logf("generated=%q exit=%d pwd=%q allPassed=%v assertions=%+v", rec.GeneratedCommand, rec.ExitCode, rec.ObservedPWD, rec.AllPassed, rec.Assertions)
		return rec
	}
	// The code under review's construction, verbatim.
	unsafe := run(`process.stdout.write('cd ' + JSON.stringify("/tmp/test$dir") + ' && pwd')`,
		[]rcAssertion{{Kind: "pwd", Value: "/tmp/test$dir"}, {Kind: "exit", Value: "0"}})
	if unsafe.GeneratedCommand != `cd "/tmp/test$dir" && pwd` {
		t.Fatalf("harness did not execute the code's own generated command: %q", unsafe.GeneratedCommand)
	}
	if unsafe.Assertions[0].Passed || unsafe.Assertions[1].Passed || unsafe.ExitCode == 0 || unsafe.ObservedPWD == "/tmp/test$dir" {
		t.Fatalf("expected the JSON.stringify-built cd to be misdirected with FAILED assertions: %+v", unsafe)
	}
	// With the intended behavior asserted, that failure is the violation
	// observed — evidence of the defect, not a reason to discard the run.
	if obs, why := unsafe.observation("intended"); obs != rcObservedViolation || !strings.Contains(why, `pwd "/tmp/test$dir"`) {
		t.Fatalf("misdirected cd not derived as a violation naming the path check: obs=%q why=%q", obs, why)
	}
	// The single-quote fix, constructed and asserted the same way, PASSES —
	// conformance for that input.
	fixed := run(`const p = "/tmp/test$dir"; process.stdout.write("cd '" + p.replace(/'/g, "'\\''") + "' && pwd")`,
		[]rcAssertion{{Kind: "pwd", Value: "/tmp/test$dir"}, {Kind: "exit", Value: "0"}, {Kind: "stdout_contains", Value: "/tmp/test$dir"}})
	if obs, _ := fixed.observation("intended"); obs != rcObservedConformance || fixed.ObservedPWD != "/tmp/test$dir" || !fixed.AllPassed {
		t.Fatalf("expected the single-quoted construction to pass every assertion (conformance): %+v", fixed)
	}
	// A free-form script with assertions is refused: assertions need fidelity mode.
	input, _ := json.Marshal(rcExperimentParams{Script: "pwd", Assertions: []rcAssertion{{Kind: "exit", Value: "0"}}})
	if _, err := sb.runExperiment(context.Background(), string(input)); err == nil {
		t.Fatal("assertions were accepted on a free-form script")
	}
}

// The shell fact GLM stated as its reason ("double-quoted strings in POSIX sh
// prevent expansion of these characters") is the opposite of POSIX behavior.
// EXPECTED: inside double quotes $HOME expands and a backtick runs a command;
// inside single quotes neither does.
func TestForensicsDoubleQuotesDoNotSuppressExpansion(t *testing.T) {
	sb := forensicsSandbox(t)
	script := `HOME=/fakehome
echo "dq: /tmp/a$HOME"
echo 'sq: /tmp/a$HOME'
echo "dq-bt: ` + "`echo RAN`" + `"
echo 'sq-bt: ` + "`echo RAN`" + `'
`
	got := forensicsRun(t, sb, script)
	for _, want := range []string{"dq: /tmp/a/fakehome", "sq: /tmp/a$HOME", "dq-bt: RAN", "sq-bt: `echo RAN`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q — double quotes do NOT suppress $ or backtick expansion in POSIX sh:\n%s", want, got)
		}
	}
}
