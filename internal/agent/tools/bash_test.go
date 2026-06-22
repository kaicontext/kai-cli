package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBash_RunsSuccessfulCommand: minimal happy path — `echo hi` runs,
// captures stdout, exit 0.
func TestBash_RunsSuccessfulCommand(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	tool := &BashTool{Workspace: ws}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"echo hi"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "hi") {
		t.Errorf("output missing 'hi': %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "exit=0") {
		t.Errorf("expected exit=0 in header: %q", resp.Content)
	}
}

// TestBash_NonzeroExitMarksError: bash returns 2; tool reports
// IsError so the agent loop can react.
func TestBash_NonzeroExitMarksError(t *testing.T) {
	requireBash(t)
	tool := &BashTool{Workspace: t.TempDir()}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"exit 2"}`,
	})
	if !resp.IsError {
		t.Errorf("expected IsError for exit 2, got: %+v", resp)
	}
	if !strings.Contains(resp.Content, "exit=2") {
		t.Errorf("header should show exit=2: %q", resp.Content)
	}
}

// TestBash_RunsInWorkspace: pwd should print the workspace dir, not
// the test process's cwd.
func TestBash_RunsInWorkspace(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	tool := &BashTool{Workspace: ws}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"pwd"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	// macOS sometimes returns /private prefix on tempdirs.
	if !strings.Contains(resp.Content, ws) && !strings.Contains(resp.Content, "/private"+ws) {
		t.Errorf("pwd didn't run in workspace: %q (ws=%s)", resp.Content, ws)
	}
}

func TestBash_EmptyCommandRejected(t *testing.T) {
	tool := &BashTool{Workspace: t.TempDir()}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"  "}`,
	})
	if !resp.IsError || !strings.Contains(resp.Content, "command required") {
		t.Errorf("expected 'command required' error, got: %+v", resp)
	}
}

func TestBash_AllowlistRejectsUnlisted(t *testing.T) {
	// Pick a command that's:
	//   - NOT in the allowlist (so the allowlist check fires)
	//   - NOT destructive (rm/git rm now hit the fail-closed
	//     no-Approve-hook guard BEFORE allowlist eval)
	//   - NOT a kai-tool-shadowing utility (cat/grep/find/etc. are
	//     intercepted by rejectIfShadowsKaiTool)
	// `date` is a bare utility with no kai equivalent.
	tool := &BashTool{Workspace: t.TempDir(), Allow: []string{"echo", "ls"}}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"date"}`,
	})
	if !resp.IsError || !strings.Contains(resp.Content, "not in allowlist") {
		t.Errorf("expected allowlist rejection, got: %+v", resp)
	}
}

func TestBash_AllowlistAcceptsListed(t *testing.T) {
	requireBash(t)
	tool := &BashTool{Workspace: t.TempDir(), Allow: []string{"echo"}}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"echo ok"}`,
	})
	if resp.IsError {
		t.Errorf("expected success for listed command, got: %s", resp.Content)
	}
}

func TestBash_AllowlistSkipsEnvAssignments(t *testing.T) {
	requireBash(t)
	tool := &BashTool{Workspace: t.TempDir(), Allow: []string{"echo"}}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"FOO=bar echo ok"}`,
	})
	if resp.IsError {
		t.Errorf("env-assignment prefix should be skipped for allowlist: %s", resp.Content)
	}
}

// TestBash_OutputTruncated: write more bytes than MaxOutputBytes;
// expect a "(truncated…)" tail and the tail bytes dropped.
func TestBash_OutputTruncated(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	tool := &BashTool{Workspace: ws, MaxOutputBytes: 100}
	// Use dd to produce the truncatable stream — `head -c N` would
	// also work but `head` is in the shadow list (it's a file-read
	// command kai_view covers). dd is shell-specific stream-fu, not
	// a kai-tool alternative, so it stays allowed.
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"dd if=/dev/zero bs=5000 count=1 2>/dev/null"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "truncated") {
		t.Errorf("output should be truncated: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "byte-capped") {
		t.Errorf("header should note byte-cap truncation: %q", resp.Content)
	}
}

// TestBash_TimeoutFromParam: the agent supplies a 1s timeout, sleep
// 5 should be killed.
func TestBash_TimeoutFromParam(t *testing.T) {
	requireBash(t)
	tool := &BashTool{Workspace: t.TempDir()}
	start := time.Now()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"sleep 5","timeout":1}`,
	})
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("timeout didn't fire — took %v", elapsed)
	}
	if !resp.IsError {
		t.Errorf("expected error response on timeout: %+v", resp)
	}
}

// TestBash_FilesEditedDuringRun: a bash command that writes a file
// produces the file in the workspace. Confirms cwd plumbing.
func TestBash_FilesEditedDuringRun(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	tool := &BashTool{Workspace: ws}
	_, _ = tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"echo content > out.txt"}`,
	})
	body, err := os.ReadFile(filepath.Join(ws, "out.txt"))
	if err != nil {
		t.Fatalf("expected out.txt in workspace: %v", err)
	}
	if !strings.HasPrefix(string(body), "content") {
		t.Errorf("unexpected body: %q", string(body))
	}
}

func TestTruncateLines_BelowThreshold(t *testing.T) {
	in := []byte("a\nb\nc\nd\n")
	out, truncated, total, dropped := truncateLines(in, 40, 40)
	if truncated {
		t.Errorf("4 lines should not be truncated with head=40 tail=40")
	}
	if string(out) != string(in) {
		t.Errorf("output must be unchanged when below threshold")
	}
	if total != 4 || dropped != 0 {
		t.Errorf("total=%d dropped=%d, want 4/0", total, dropped)
	}
}

func TestTruncateLines_HeadTailSplit(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	in := []byte(strings.Join(lines, "\n") + "\n")
	out, truncated, total, dropped := truncateLines(in, 5, 5)
	if !truncated {
		t.Fatalf("200 lines should be truncated with head=5 tail=5")
	}
	if total != 200 {
		t.Errorf("total=%d, want 200", total)
	}
	if dropped != 190 {
		t.Errorf("dropped=%d, want 190", dropped)
	}
	s := string(out)
	// First 5 lines preserved.
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("line %d", i)
		if !strings.Contains(s, want) {
			t.Errorf("missing head line %q in output", want)
		}
	}
	// Last 5 lines preserved.
	for i := 195; i < 200; i++ {
		want := fmt.Sprintf("line %d", i)
		if !strings.Contains(s, want) {
			t.Errorf("missing tail line %q in output", want)
		}
	}
	// Middle dropped — line 100 must be gone.
	if strings.Contains(s, "line 100") {
		t.Errorf("middle line still present after truncation")
	}
	// Separator with the count.
	if !strings.Contains(s, "--- 190 lines truncated ---") {
		t.Errorf("missing or wrong separator: %q", s)
	}
}

func TestTruncateLines_NoTrailingNewline(t *testing.T) {
	// Last line without "\n" should still count and be preserved.
	in := []byte("a\nb\nc\nd\ne")
	out, truncated, total, _ := truncateLines(in, 2, 2)
	if !truncated {
		t.Fatal("5 lines with head=2 tail=2 should truncate")
	}
	if total != 5 {
		t.Errorf("total=%d, want 5", total)
	}
	s := string(out)
	if !strings.Contains(s, "a\nb\n") {
		t.Errorf("head missing: %q", s)
	}
	if !strings.HasSuffix(s, "e") {
		t.Errorf("tail missing or trailing-newline added: %q", s)
	}
}

func TestFirstCommandToken(t *testing.T) {
	cases := map[string]string{
		"npm test":              "npm",
		"  go build ./...":      "go",
		"FOO=bar npm install":   "npm",
		"./scripts/build":       "scripts/build",
		"":                      "",
	}
	for in, want := range cases {
		if got := firstCommandToken(in); got != want {
			t.Errorf("firstCommandToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTranslationHint covers the bash → kai_* translator that fires
// when shadow-rejection rejects a command. Goal: the rejection
// message should include the literal kai_* call so the agent can
// copy-paste on the next turn instead of reverse-engineering args
// from prose.
func TestTranslationHint(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"cat-simple", "cat foo.go", `view {"file_path":"foo.go"}`},
		{"cat-with-flags", "cat -n -B 5 foo.go", `view {"file_path":"foo.go"}`},
		{"head-numeric-flag", "head -n 50 README.md", `view {"file_path":"README.md"}`},
		{"grep-bare-pattern", `grep "needle" .`, `kai_grep {"query":"needle"}`},
		{"grep-with-flags", `grep -rn ParseConfig src/`, `kai_grep {"query":"ParseConfig"}`},
		{"rg-pipe-untranslatable", `rg foo | head`, ""},
		{"find-name-glob", `find . -name "*.go"`, `kai_files {"glob":"*.go"}`},
		{"find-complex-untranslatable", `find . -newer x -size +1M`, ""},
		{"ls-bare", "ls", `kai_tree {"path":"."}`},
		{"ls-dir", "ls internal/", `kai_tree {"path":"internal/"}`},
		{"unknown-tool-no-hint", "sed -i s/x/y/ foo", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := firstCommandToken(c.cmd)
			got := translationHint(first, c.cmd)
			if got != c.want {
				t.Errorf("translationHint(%q) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}

// TestRejectIfShadows_IncludesTranslation: end-to-end check that the
// rejection message ships the kai_* literal alongside the prose.
func TestRejectIfShadows_IncludesTranslation(t *testing.T) {
	got := rejectIfShadowsKaiTool("cat foo.go")
	if !strings.Contains(got, "Equivalent call:") {
		t.Errorf("rejection should include translation header: %q", got)
	}
	if !strings.Contains(got, `view {"file_path":"foo.go"}`) {
		t.Errorf("rejection should include literal view call: %q", got)
	}
}

// TestRejectIfShadowsKaiTool_ChainSegments pins the chain-walker
// fix from 2026-05-14 dogfood. Previous impl only checked the first
// token of the command, so `cd kai-cli && grep "X"` slipped past
// (cd is not shadowed) and the agent shelled out to grep. The fix
// walks every segment separated by &&/||/;/|.
func TestRejectIfShadowsKaiTool_ChainSegments(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool // true if the rejection message should fire
	}{
		{"subdir-cd-then-grep", `cd kai-cli && grep -n "X"`, true},
		{"subdir-cd-then-sed", "cd kai-cli && sed -i 's/x/y/' foo.go", true},
		{"piped-cat-grep", "cat foo | grep bar", true},
		{"piped-into-grep-from-allowed-tool", "go test ./... | grep FAIL", true},
		{"semicolon-chain-with-find", "echo hi; find . -name '*.go'", true},
		{"or-chain-with-ls", "make build || ls -la", true},
		{"clean-go-pipeline-not-rejected", "go test ./... 2>&1", false},
		{"clean-make-chain-not-rejected", "make build && make test", false},
		{"clean-tee-not-rejected", "go test ./... | tee out.log", false},
		// 2026-05-15 dogfood: agent fell back to python3 / perl / wc
		// to walk source files after kai tools hit the read budget.
		// These are now hard-blocked at first-token, including the
		// inline-script (-c) and stdin (/dev/stdin) variants. Each
		// flavor must reject regardless of where it sits in a chain.
		{"python3-inline-script", `python3 -c 'open("foo.go").read()'`, true},
		{"python3-stdin-script", "python3 /dev/stdin", true},
		{"python-bare", "python -c 'print(1)'", true},
		{"python2-bare", "python2 -c 'print 1'", true},
		{"perl-inline-script", `perl -e 'print 1'`, true},
		{"wc-line-count", "wc -l foo.go", true},
		{"cd-then-python3", "cd kai-cli && python3 -c 'x'", true},
		{"piped-into-python", "echo foo | python3 -c 'import sys; print(sys.stdin.read())'", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rejectIfShadowsKaiTool(c.cmd)
			if (got != "") != c.want {
				t.Errorf("rejectIfShadowsKaiTool(%q) = %q; want fire=%v", c.cmd, got, c.want)
			}
		})
	}
}

// TestStripLeadingCdToWorkspace covers the cd-prefix stripper that
// defends against the agent prepending `cd <main-repo> && ...` to
// its bash commands. This was the root cause of two distinct
// failures: the "wrote then reverted" symptom (file tools edited
// spawn while bash mutated main), and the 2026-05-24 install-loop
// (18+ identical `cd /Users/... && go build` retries because the cd
// silently dropped and `go build` ran in the spawn dir's no-go.mod
// root).
//
// The fix returns (cmd, escapeReason). escapeReason is non-empty
// only when the cd targets a path OUTSIDE the workspace; the caller
// (BashTool.Run) refuses the command in that case rather than
// silently stripping the cd. Tests assert both values.
func TestStripLeadingCdToWorkspace(t *testing.T) {
	ws := "/spawn/abc"
	cases := []struct {
		name       string
		in         string
		wantCmd    string
		wantEscape bool // expect escapeReason to be non-empty
	}{
		{"no-cd-passthrough", "make", "make", false},
		// Escape cases — cmd unchanged, escapeReason set; caller refuses.
		{"cd-to-elsewhere-escapes", "cd /tmp/main && make", "cd /tmp/main && make", true},
		{"cd-with-semicolon-escapes", "cd /tmp/main; make", "cd /tmp/main; make", true},
		{"cd-quoted-path-escapes", `cd "/tmp/main" && make 2>&1`, `cd "/tmp/main" && make 2>&1`, true},
		{"cd-with-cat-then-escapes", "cd /tmp/main && cat foo.go", "cd /tmp/main && cat foo.go", true},
		// Redundant cd to our own workspace: dropped, not an escape.
		{"cd-to-own-workspace-stripped", "cd /spawn/abc && make", "make", false},
		// Relative cd: preserved (legitimate subdir entry).
		{"cd-relative-preserved", "cd subdir && make", "cd subdir && make", false},
		// Bare cd: dropped (a fresh shell cd accomplishes nothing).
		{"bare-cd-dropped", "cd /tmp/main", "", false},
		{"compound-not-led-by-cd", "make && echo done", "make && echo done", false},
		// The 2026-05-14 dogfood bug: agents copy the absolute path
		// they see in a prompt and cd into a workspace SUBDIR. Used
		// to be stripped (treated as escape attempt) and the command
		// then ran at workspace root with no go.mod. Now preserved.
		{"cd-to-workspace-subdir-preserved", "cd /spawn/abc/kai-core && go test ./cas/", "cd /spawn/abc/kai-core && go test ./cas/", false},
		{"cd-to-workspace-deeper-subdir-preserved", "cd /spawn/abc/kai-server/kailab && go test ./pack/", "cd /spawn/abc/kai-server/kailab && go test ./pack/", false},
		{"cd-to-workspace-subdir-quoted-preserved", `cd "/spawn/abc/kai-core" && go test`, `cd "/spawn/abc/kai-core" && go test`, false},
		// Path traversal attempt that resolves inside workspace via
		// "..": filepath.Rel still says it's inside, so this is
		// preserved. Conservative — there are easier ways to escape
		// the spawn dir than this. The cd-prefix strip is best-
		// effort, not a sandbox boundary.
		{"cd-with-dotdot-resolves-inside-preserved", "cd /spawn/abc/sub/../other && make", "cd /spawn/abc/sub/../other && make", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotCmd, gotEscape := stripLeadingCdToWorkspace(c.in, ws)
			if gotCmd != c.wantCmd {
				t.Errorf("stripLeadingCdToWorkspace(%q): cmd = %q, want %q", c.in, gotCmd, c.wantCmd)
			}
			haveEscape := gotEscape != ""
			if haveEscape != c.wantEscape {
				t.Errorf("stripLeadingCdToWorkspace(%q): escape=%v, want %v (reason=%q)", c.in, haveEscape, c.wantEscape, gotEscape)
			}
		})
	}
}

// TestBash_CdPrefixOutsideWorkspaceRefusesLoudly is the integration
// canary for the install-loop bug (2026-05-24). The previous fix
// stripped `cd /tmp/elsewhere` silently and ran `cat foo.go` against
// the workspace; the shadow check then rejected the cat. That left
// two distinct rejection paths depending on whether the rest of the
// command was a shadowed tool, so cd-to-elsewhere with a non-shadowed
// follow-up (`go build`) silently dropped and ran in the wrong
// directory. The fix: any cd outside the workspace is refused at
// the bash tool's entry, before any further dispatch.
func TestBash_CdPrefixOutsideWorkspaceRefusesLoudly(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	tool := &BashTool{Workspace: ws}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"cd /tmp/elsewhere && cat foo.go"}`,
	})
	if !resp.IsError {
		t.Fatalf("expected rejection, got success: %q", resp.Content)
	}
	// Refusal must explain the sandbox so the agent can adapt.
	for _, want := range []string{
		"outside your workspace",
		"isolated CoW workspace",
		"STOP and report",
		// 2026-05-26: refusal must also coach toward the common
		// case — dropping `cd` and invoking the binary directly —
		// because that's what the edges dogfood tripped over.
		"COMMON CASE",
		"DROP",
	} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("rejection missing %q: %q", want, resp.Content)
		}
	}
}

// TestBash_CdPrefixForcesWorkspaceCwd: agent issues
//   cd /elsewhere && pwd > out
// and the fix forces it to run in the configured workspace. The pwd
// in the resulting file should be the workspace path, not /elsewhere.
// This is the closest unit-level analog to the absorb-side bug —
// without the strip, pwd would write the agent-supplied path.
func TestBash_CdPrefixForcesWorkspaceCwd(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	tool := &BashTool{Workspace: ws}
	// Two cases — a workspace-internal cd is honored, an absolute
	// outside cd is refused. Pre-2026-05-24 the outside cd was
	// silently stripped and `pwd` ran in the workspace — that's
	// what we no longer do.
	t.Run("internal cd is honored", func(t *testing.T) {
		// Create a real subdir so the workspace cd succeeds.
		sub := filepath.Join(ws, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		resp, _ := tool.Run(context.Background(), ToolCall{
			Name:  "bash",
			Input: `{"command":"cd sub && pwd"}`,
		})
		if resp.IsError {
			t.Fatalf("unexpected error: %s", resp.Content)
		}
		wantSub, _ := filepath.EvalSymlinks(sub)
		if !strings.Contains(resp.Content, wantSub) {
			t.Errorf("expected pwd to land in %q, got: %q", wantSub, resp.Content)
		}
	})
	t.Run("outside cd is refused, not silently dropped", func(t *testing.T) {
		resp, _ := tool.Run(context.Background(), ToolCall{
			Name:  "bash",
			Input: `{"command":"cd /tmp/somewhere-else && pwd"}`,
		})
		if !resp.IsError {
			t.Fatalf("expected refusal, got success: %q", resp.Content)
		}
		if !strings.Contains(resp.Content, "outside your workspace") {
			t.Errorf("refusal should name the sandbox boundary: %q", resp.Content)
		}
		// Regression guard: the response must be just the error, not
		// the refusal text PLUS a pwd output. The bash tool returns
		// either an error or command output, never both.
		if !strings.HasPrefix(resp.Content, "bash: refusing") {
			t.Errorf("refusal must not run the rest of the command: %q", resp.Content)
		}
	})
}

// requireBash skips the test if /bin/bash isn't available (Windows
// CI). All bash tests use real bash for fidelity; mocking would
// defeat the purpose.
func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash tool tests skip on Windows")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skipf("/bin/bash unavailable: %v", err)
	}
}

// TestMustReachApproval verifies the rm / git rm carve-out predicate.
// These commands must always reach the human approval prompt — the
// allowlist auto-approve lane never applies to them.
func TestMustReachApproval(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"rm foo", true},
		{"rm -rf /tmp/x", true},
		{"  rm foo", true}, // leading whitespace
		{"git rm foo", true},
		{"git rm --cached foo", true},
		// Negatives: must NOT trigger carve-out.
		{"ls", false},
		{"git status", false},
		{"git commit -m 'rm'", false}, // "rm" in commit msg, not the action
		{"echo 'rm -rf'", false},
		{"trim", false}, // doesn't start with "rm "/"rm$"
	}
	for _, c := range cases {
		if got := mustReachApproval(c.in); got != c.want {
			t.Errorf("mustReachApproval(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestDestructiveWarning verifies the warning-label detection for
// recursive / force rm variants. Empty string means no warning.
func TestDestructiveWarning(t *testing.T) {
	cases := []struct {
		in       string
		nonEmpty bool
	}{
		{"rm -rf foo", true},
		{"rm -r foo", true},
		{"rm -f foo", true},
		{"rm -fr foo", true}, // flag-cluster reversed
		{"rm -rfv foo", true},
		// Negatives.
		{"rm foo", false},       // no destructive flag
		{"rm foo.tmp", false},   // no flags
		{"git rm foo", false},   // git rm doesn't take POSIX rm flags
		{"echo 'rm -rf'", false},
		{"ls -rf", false},       // -rf on different command
	}
	for _, c := range cases {
		got := destructiveWarning(c.in)
		if (got != "") != c.nonEmpty {
			t.Errorf("destructiveWarning(%q) = %q, want non-empty=%v", c.in, got, c.nonEmpty)
		}
	}
}

// TestRejectDestructiveFind pins the find-with-mutating-flags hard
// block. These commands are rejected outright — no approval prompt,
// no warning, just "we don't do this."
func TestRejectDestructiveFind(t *testing.T) {
	cases := []struct {
		in       string
		rejected bool
	}{
		{"find . -delete", true},
		{"find . -name '*.tmp' -delete", true},
		{`find . -exec rm {} \;`, true},
		{`find . -execdir rm {} \;`, true},
		{`find . -ok rm {} \;`, true},
		{`find . -okdir rm {} \;`, true},
		// Negatives.
		{"find . -name '*.go'", false},
		{"find . -type f", false},
		{"echo 'find . -delete'", false}, // -delete in quoted echo arg, not the find call
		{"ls find-and-delete.go", false}, // word-boundary check
	}
	for _, c := range cases {
		got := rejectIfDestructiveFind(c.in)
		if (got != "") != c.rejected {
			t.Errorf("rejectIfDestructiveFind(%q) returned %q, want rejected=%v", c.in, got, c.rejected)
		}
	}
}

// TestRun_RmCarveoutReachesApprove verifies that even when `rm` is on
// the allowlist (auto-approve lane), an actual rm command still
// triggers the Approve callback. Without the carve-out, the
// allowlist would short-circuit Approve and the destructive command
// would run silently.
func TestRun_RmCarveoutReachesApprove(t *testing.T) {
	requireBash(t)
	ws := t.TempDir()
	target := filepath.Join(ws, "doomed.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	approveCalled := false
	tool := &BashTool{
		Workspace: ws,
		Allow:     []string{"rm"}, // first-token allowlist would normally auto-approve
		Approve: func(_ context.Context, cmd, warning string) (bool, error) {
			approveCalled = true
			// Refuse so the rm doesn't actually run.
			return false, nil
		},
	}
	_, _ = tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"rm doomed.txt"}`,
	})
	if !approveCalled {
		t.Error("rm on the allowlist should still reach Approve — carve-out failed")
	}
	// File must still exist because Approve returned false.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target file should survive (Approve returned false): %v", err)
	}
}

// TestRun_FindDeleteBlocked verifies the find-with-mutating-flags
// path: command is rejected with an error before any approval prompt
// fires. Reaching Approve at all is a regression.
func TestRun_FindDeleteBlocked(t *testing.T) {
	requireBash(t)
	approveCalled := false
	tool := &BashTool{
		Workspace: t.TempDir(),
		Approve: func(_ context.Context, cmd, warning string) (bool, error) {
			approveCalled = true
			return true, nil
		},
	}
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: `{"command":"find . -delete"}`,
	})
	if !resp.IsError {
		t.Error("find -delete should produce an error response")
	}
	if approveCalled {
		t.Error("find -delete must be rejected BEFORE the approval prompt")
	}
	if !strings.Contains(resp.Content, "find") || !strings.Contains(resp.Content, "blocked") {
		t.Errorf("rejection message should name `find` and `blocked`: %q", resp.Content)
	}
}

// TestBash_RejectsDestructiveWithoutApproveHook pins the fail-closed
// defense: any BashTool wired without an Approve hook must refuse rm /
// git rm at the door, not silently run them. This closes the
// 2026-05-20 dogfood hole where the TUI chat-mode dispatcher built a
// BashTool with no OnBashConfirm hook AND an empty allowlist, so an
// `rm -rf <wrong-path>` slipped through the destructive carve-out
// (which only fires on auto-allowlist matches) and ran without
// prompting the user.
func TestBash_RejectsDestructiveWithoutApproveHook(t *testing.T) {
	requireBash(t)
	cases := []string{
		"rm foo.tmp",
		"rm -rf /tmp/anything",
		"rm -r dir",
		"git rm path/to/file",
		"git rm -rf dir",
	}
	for _, cmd := range cases {
		// Approve deliberately unset. Allow deliberately empty.
		tool := &BashTool{Workspace: t.TempDir()}
		input, _ := json.Marshal(map[string]string{"command": cmd})
		resp, _ := tool.Run(context.Background(), ToolCall{
			Name:  "bash",
			Input: string(input),
		})
		if !resp.IsError {
			t.Errorf("%q: expected IsError when no Approve hook is wired", cmd)
			continue
		}
		// Diagnostic message must name the failure mode AND point at
		// the fix, so a developer hitting this in CI knows exactly
		// what to do.
		for _, want := range []string{
			"refusing to run destructive command",
			"OnBashConfirm",
			"github.com/kaicontext/kai/issues",
		} {
			if !strings.Contains(resp.Content, want) {
				t.Errorf("%q: rejection message missing %q. Full: %s", cmd, want, resp.Content)
			}
		}
	}
}

// TestBash_AllowsDestructiveWhenApproveHookWired confirms the gate
// still ACCEPTS destructive commands when the hook is correctly
// wired — the fail-closed defense should not break the legitimate
// approval flow.
func TestBash_AllowsDestructiveWhenApproveHookWired(t *testing.T) {
	requireBash(t)
	target := t.TempDir() + "/scratch.txt"
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var approved bool
	tool := &BashTool{
		Workspace: t.TempDir(),
		Approve: func(_ context.Context, cmd, warning string) (bool, error) {
			approved = true
			return true, nil
		},
	}
	input, _ := json.Marshal(map[string]string{"command": "rm " + target})
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "bash",
		Input: string(input),
	})
	if !approved {
		t.Errorf("Approve hook should have been called for rm; got resp: %s", resp.Content)
	}
	if resp.IsError {
		t.Errorf("approved rm should succeed; got: %s", resp.Content)
	}
}

func TestGitDiffShadow_RedirectsWorkingTreeDiffs(t *testing.T) {
	redirected := []string{
		"git diff",
		"git diff internal/foo.go",
		"git diff --stat",
		"git diff --name-only",
		"cd kai-cli && git diff cmd/kai/main.go",
	}
	for _, c := range redirected {
		if got := rejectIfShadowsKaiTool(c); got == "" || !strings.Contains(got, "kai_diff") {
			t.Errorf("%q: expected redirect to kai_diff, got %q", c, got)
		}
	}
	allowed := []string{
		"git diff main..feature",
		"git diff HEAD~3..HEAD",
		"git diff --cached",
		"git diff --staged",
		"git status",
		"git log --oneline",
	}
	for _, c := range allowed {
		if got := gitDiffShadow(c); got != "" {
			t.Errorf("%q: expected allow (no redirect), got %q", c, got)
		}
	}
}
