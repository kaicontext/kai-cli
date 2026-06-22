package views

import (
	"errors"
	"strings"
	"testing"
)

// TestDetectHostCommandError covers the error-shape scanner used by
// the host-command tail-after-launch path. Two distinct modes:
//  1. process exited non-zero, NOT detached → surface last non-empty
//     line as the error summary.
//  2. process detached (still running) OR exited zero → scan output
//     for error/exception/failed/unexpected keyword lines.
func TestDetectHostCommandError(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		err      error
		detached bool
		want     string // empty = expect no detection; substring otherwise
	}{
		{
			name:     "vite detached error from compiler",
			output:   "\n  VITE v5.4.21  ready in 333ms\n[plugin:vite-plugin-svelte] Error while preprocessing AgentsView.svelte:25:12 - Unexpected block closing tag",
			detached: true,
			want:     "Unexpected block closing tag",
		},
		{
			name:   "npm install ERESOLVE non-zero exit",
			output: "npm error code ERESOLVE\nnpm error ERESOLVE could not resolve\nnpm error",
			err:    errors.New("exit 1"),
			want:   "npm error",
		},
		{
			name:     "no error in detached vite output",
			output:   "  VITE v5.4.21  ready in 333ms\n  ➜  Local:   http://localhost:5173/\n",
			detached: true,
			want:     "", // no error keyword → no detection
		},
		{
			name:   "clean exit, empty detection",
			output: "Hello\nWorld",
			want:   "", // err==nil, !detached → no scan, empty
		},
		{
			name:   "empty output",
			output: "",
			err:    errors.New("exit 127"),
			want:   "",
		},
		{
			name:     "detected line truncated past cap",
			output:   "error: " + strings.Repeat("x", 500),
			detached: true,
			want:     "…", // truncation marker present
		},
		{
			name:     "exception keyword fires",
			output:   "Traceback (most recent call last):\n  File 'x.py'\nException: oh no",
			detached: true,
			want:     "Exception: oh no",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectHostCommandError(c.output, c.err, c.detached)
			if c.want == "" && got != "" {
				t.Errorf("expected no detection, got %q", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("expected detection to contain %q, got %q", c.want, got)
			}
		})
	}
}

// TestDetectHostCommandErrorViteContextBlock pins the v0.32.11 fix:
// vite-style errors must surface the full multi-line context block
// (Plugin line + numbered source-line excerpt + carat), not just
// the single error-keyword line. The buggy CONTENT lives in that
// excerpt — when svelte-preprocess shifts template line numbers,
// the agent has no other way to see the actual offending code.
func TestDetectHostCommandErrorViteContextBlock(t *testing.T) {
	output := strings.Join([]string{
		"[vite] Pre-transform error: AgentsView.svelte:181:63 Unexpected token",
		"  Plugin: vite-plugin-svelte",
		"  File: AgentsView.svelte:181:63",
		"  179 |    <div class=\"diff-line diff-context\">  agent.cost.toFixed(2)</div>",
		"  180 |    <div class=\"diff-line diff-add\">+ if showFlag {</div>",
		"  181 |    + if showFlag {",
		"                            ^",
		"  182 |    fmt.Println(\"hi\")",
		"",
		"some-other-output-line",
	}, "\n")
	got := detectHostCommandError(output, nil, true)
	mustContain := []string{
		"Pre-transform error",
		"Plugin: vite-plugin-svelte",
		"+ if showFlag {",
		"^",
	}
	for _, m := range mustContain {
		if !strings.Contains(got, m) {
			t.Errorf("expected block to contain %q\ngot:\n%s", m, got)
		}
	}
	if strings.Contains(got, "some-other-output-line") {
		t.Errorf("block should stop at non-context line, got:\n%s", got)
	}
}

// TestDetectHostCommandErrorPlainErrorStillSingleLine guards
// against the vite-context capture accidentally swallowing
// non-vite errors. A plain "Error: ..." line with no vite/plugin
// markers nearby should fall through to single-line behavior.
func TestDetectHostCommandErrorPlainErrorStillSingleLine(t *testing.T) {
	output := "starting build\nError: something broke\nmore output\nfinal line"
	got := detectHostCommandError(output, nil, true)
	if got != "Error: something broke" {
		t.Errorf("expected plain single-line capture, got %q", got)
	}
}
