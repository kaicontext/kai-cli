package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// BashTool runs shell commands inside the agent's workspace and
// captures stdout/stderr. It is intentionally minimal — kai's safety
// gate is the chokepoint that decides whether the agent's overall
// changes promote, so we don't need OpenCode's per-command permission
// prompt here. The optional Allow allowlist is a defense-in-depth
// trip-wire for catastrophically wrong commands (rm -rf /, curl |
// sh, etc.) and is matched on the first whitespace-separated token.
//
// Output is bounded at MaxOutputBytes so a chatty command (npm
// install, go test ./... -v) doesn't blow the model's context. Both
// stdout and stderr are interleaved into one buffer the way the
// model sees it on a real terminal.
type BashTool struct {
	// Workspace is the absolute path the command runs in (cwd).
	// Same value as FileTools.Workspace.
	Workspace string

	// Allow is an optional allowlist of command-name prefixes. When
	// non-empty, the first token of the command must match one of
	// these (e.g. "npm", "go", "git"). Empty allows everything.
	Allow []string

	// DefaultTimeout caps how long a single command can run. 0 picks
	// 60s. The agent can override via the `timeout` parameter in
	// the call (subject to MaxTimeout).
	DefaultTimeout time.Duration

	// MaxOutputBytes caps the captured output length. 0 picks
	// DefaultMaxOutputBytes (30 KiB). Trimmed output gets a
	// "(truncated …)" tail.
	MaxOutputBytes int

	// OnOutput, when set, fires once per line as the command writes
	// to stdout/stderr. Lets the TUI stream progress (brew install
	// progress, npm test scrolling, etc.) inline instead of leaving
	// the user staring at a frozen pane until the command exits.
	// Must be safe for concurrent calls — output may interleave from
	// multiple goroutines reading the two pipes.
	OnOutput func(line string)

	// OnFilesChanged fires once after each command, with the
	// workspace-relative paths of files whose mtime changed during
	// the run. Used by the agent runner to gate bash-driven
	// mutations (cat heredoc → file, sed -i, npm install touching
	// node_modules) the same way write/edit calls are gated.
	// Detection is mtime-based against a pre-run snapshot; the
	// scan skips the usual noisy paths (.git, node_modules, .kai).
	OnFilesChanged func(paths []string)

	// Approve, when set, gates each command behind a user prompt
	// (continue / cancel) before it runs. Allowlist-passing commands
	// SKIP this — the allowlist is the auto-approve lane. Approve
	// runs only for commands the allowlist either passes (when empty)
	// or doesn't apply to. Implementation must be safe to call from
	// the agent goroutine; the TUI flavor blocks on a tea.Msg
	// round-trip until the user responds.
	//
	// warning is a non-empty informational label when the command
	// matched a destructive pattern (e.g. "may recursively force-
	// remove files" for `rm -rf`). The renderer should display it
	// alongside the command. Empty warning means no label.
	//
	// Returning false aborts the command with a non-error response
	// so the agent can re-plan instead of treating cancellation as a
	// failure to retry.
	Approve func(ctx context.Context, cmd, warning string) (bool, error)
}

const (
	defaultBashTimeout    = 60 * time.Second
	maxBashTimeout        = 10 * time.Minute
	defaultMaxOutputBytes = 30000
	// Head+tail line-based truncation for the LLM-facing result.
	// 40+40 covers ~95% of "I need the start AND the end" cases
	// (dev-server startup banner + first error; test-suite header
	// + final summary). Bigger numbers just feed the verbatim-paste
	// failure mode; smaller risks losing the actual error.
	defaultBashHeadLines = 40
	defaultBashTailLines = 40
)

type bashParams struct {
	Command string `json:"command"`
	// Timeout is in seconds; clamped to [1, 600].
	Timeout int `json:"timeout"`
}

// Info returns the tool descriptor for the LLM. Description is
// careful to call out:
//   - allowed shell features (no interactive prompts; one-shot only)
//   - what's filtered (allowlist if configured)
//   - timeout semantics
//   - output truncation
//
// so the agent doesn't waste turns on unsupported usage.
func (b *BashTool) Info() ToolInfo {
	desc := "Run a shell command in the workspace and return stdout+stderr. " +
		"Commands run non-interactively under `bash -c`. " +
		"Output is capped at ~30 KB; long commands should redirect to a file " +
		"and view it with `view`. " +
		"DO NOT use bash for tasks the kai_* tools cover: file listings " +
		"(use kai_files / kai_tree, NOT find/ls), text search (use kai_grep, " +
		"NOT grep/rg/ag), reading files (use view, NOT cat/head/tail), or " +
		"editing files (use write/edit, NOT sed/awk/echo>). The kai_* tools " +
		"are faster, cheaper, and run without per-command approval prompts. " +
		"Use bash for actions that genuinely need a shell: running tests, " +
		"build/dev servers, package managers, git, curl, kubectl, etc."
	if len(b.Allow) > 0 {
		desc += " Only commands beginning with one of these names are permitted: " +
			strings.Join(b.Allow, ", ") + "."
	}
	return ToolInfo{
		Name:        "bash",
		Description: desc,
		Parameters: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command to execute. Use `&&` / `||` / pipelines as normal.",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Optional timeout in seconds (1–600). Default 60.",
				"default":     60,
			},
		},
		Required: []string{"command"},
	}
}

// Run executes the command, enforcing the allowlist + timeout, and
// returns truncated combined output as a text response.
func (b *BashTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p bashParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("bash: invalid input json: " + err.Error()), nil
	}
	cmd := strings.TrimSpace(p.Command)
	if cmd == "" {
		return NewTextErrorResponse("bash: command required"), nil
	}
	if b.Workspace == "" {
		return NewTextErrorResponse("bash: workspace not set"), nil
	}

	// Strip any agent-prepended `cd <abs-path> && ...`. The agent
	// occasionally hard-codes the main repo path (which it sees in the
	// "Working directory:" prompt line) and prepends `cd <main-repo>
	// && make` — that breaks spawn-dir isolation. After absorb, file-
	// tool edits (in spawn) and bash mutations (in main) end up
	// referencing different filesystems and the integration looks like
	// "wrote then reverted". Stripping forces every command into the
	// configured workspace; allowed-target preserves the rare valid
	// `cd <subdir>` case where the agent enters a workspace subdirectory.
	//
	// Critically, when the cd points OUTSIDE the workspace, we refuse
	// the entire command loudly rather than silently dropping the cd
	// and running the rest. Silent-drop was the previous behavior and
	// it caused 18-turn `cd /Users/... && go build` loops in the
	// install-kai dogfood (2026-05-24): cd was a no-op, go build ran
	// in the spawn dir with no go.mod, and the agent had no signal
	// that its cd wasn't taking effect.
	var cdEscape string
	cmd, cdEscape = stripLeadingCdToWorkspace(cmd, b.Workspace)
	if cdEscape != "" {
		return NewTextErrorResponse("bash: " + cdEscape), nil
	}

	// Hard reject for find with -delete / -exec / -execdir / -ok /
	// -okdir. Runs BEFORE the shadow-tool check so the more specific
	// error wins — "find with -delete is blocked" is more useful
	// than "use kai_files instead of find", which is the message
	// the shadow check would emit. Also runs before allowlist +
	// approval: no flag combination of find-and-mutate is something
	// we want behind a one-keystroke approval.
	if reason := rejectIfDestructiveFind(cmd); reason != "" {
		return NewTextErrorResponse("bash: " + reason), nil
	}

	// Interpreter scan runs FIRST and scans every token (not just
	// segment heads) so it catches the chaining/wrapping bypasses a
	// first-token check misses: `bash -c 'python3 ...'`,
	// `echo $(python3 ...)`, `env python3 ...`, `xargs python3`,
	// `/usr/bin/python3 ...`, newline-separated commands. The
	// 2026-05-15 dogfood pinned this — the agent chained bash
	// commands to route python3 around the segment-head check.
	if interp := scanForBlockedInterpreter(cmd); interp != "" {
		return NewTextErrorResponse(fmt.Sprintf("bash: %q is blocked — scripting-language "+
			"interpreters bypass the read budget by reading/searching many files in one "+
			"call. Use view (single file) or kai_grep (content search) instead. This block "+
			"applies however the command is chained, substituted, or path-qualified.", interp)), nil
	}

	// Shadow-check is applied AFTER cd-stripping so a chained
	// `cd <main> && cat x.txt` now correctly resolves to `cat x.txt`
	// and gets rejected. Before stripping, firstCommandToken returned
	// "cd" which isn't shadowed, and the cat slipped through.
	if reason := rejectIfShadowsKaiTool(cmd); reason != "" {
		return NewTextErrorResponse("bash: " + reason), nil
	}

	// Fail-closed defense for destructive commands. Before this guard
	// existed, the bash tool ran ANY command silently when both:
	//   - Allow was empty (chat-mode default, allowReason = "")
	//   - Approve was nil (caller forgot to wire OnBashConfirm)
	// The destructive carve-out below only fires on auto-allowlist
	// matches, so an empty allowlist made it a no-op. Net result: an
	// agent could `rm -rf <anything>` and the user would see only the
	// completion message. Observed in the 2026-05-20 dogfood when an
	// agent silently ran `rm -rf <wrong-path>` against a misinterpreted
	// project prefix.
	//
	// The architectural fix: destructive commands MUST have a human
	// approval channel. If no channel is wired, refuse the command
	// rather than fall through to "run it." Any caller that wires a
	// BashTool without OnBashConfirm now gets a hard error on the
	// first rm/git rm, which is its own bug report — the failure mode
	// is loud and the call site is named.
	if mustReachApproval(cmd) && b.Approve == nil {
		return NewTextErrorResponse(
			"bash: refusing to run destructive command without an approval channel.\n" +
				"This command (rm or git rm) requires human confirmation, but the\n" +
				"calling code wired a BashTool with no Approve hook. That is a bug\n" +
				"in the integration — BashTool.Approve must be set (typically via\n" +
				"agent.Hooks.OnBashConfirm) whenever destructive commands may be\n" +
				"issued. Please report at https://github.com/kaicontext/kai/issues.\n\n" +
				"Command refused: " + cmd), nil
	}

	allowReason, allowed := b.checkAllowDetail(cmd)
	if !allowed {
		return NewTextErrorResponse("bash: " + allowReason), nil
	}

	// rm / git rm carve-out: even if the first-token allowlist would
	// auto-approve these, force them to the human prompt. Allowlist
	// auto-approve is fine for `rm foo.tmp` in the moment but the
	// session-wide effect means a later `rm -rf .` would also pass
	// silently. Reaching Approve for every rm closes the hole.
	if allowReason == "auto" && mustReachApproval(cmd) {
		allowReason = "manual-required-destructive"
	}

	warning := destructiveWarning(cmd)

	// Approval gate. Allowlist-passing commands (allowReason == "auto")
	// skip the prompt — that's the user's pre-blessed set. Anything
	// else either has an empty allowlist (Approve is the gate) or
	// explicitly passed via "no allowlist configured" — both should
	// prompt when Approve is set.
	if b.Approve != nil && allowReason != "auto" {
		ok, err := b.Approve(ctx, cmd, warning)
		if err != nil {
			return NewTextErrorResponse("bash: approval failed: " + err.Error()), nil
		}
		if !ok {
			// Cancelled is a non-error response so the agent can
			// reconsider rather than treat it as a tool fault.
			return NewTextResponse(fmt.Sprintf("$ %s\n(cancelled by user)\n", oneLine(cmd))), nil
		}
	}

	timeout := b.DefaultTimeout
	if timeout <= 0 {
		timeout = defaultBashTimeout
	}
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Second
	}
	if timeout > maxBashTimeout {
		timeout = maxBashTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Pre-run mtime snapshot so we can identify files the command
	// touched. Cheap (stat only), bounded by the ignore filter to
	// the workspace's actually-interesting files — skips .git,
	// node_modules, .kai, vendor.
	preSnap := snapshotMtimes(b.Workspace)

	c := exec.CommandContext(runCtx, "bash", "-c", cmd)
	c.Dir = b.Workspace
	// Stdin from /dev/null: agents run non-interactively, so any
	// command that prompts for input (brew install confirmations,
	// `read`, ssh password prompts) would otherwise block until the
	// timeout fires. Closing stdin makes the prompting program see
	// EOF immediately and either fail loudly or proceed with its
	// non-interactive default — both better than a silent hang.
	if devnull, err := os.Open(os.DevNull); err == nil {
		c.Stdin = devnull
		defer devnull.Close()
	}
	// Tell common tools they're not on a TTY. Belt-and-suspenders:
	// brew honors NONINTERACTIVE; many CLIs check CI=true.
	c.Env = append(os.Environ(),
		"NONINTERACTIVE=1",
		"CI=1",
		"DEBIAN_FRONTEND=noninteractive",
		"HOMEBREW_NO_AUTO_UPDATE=1",
	)

	// Capture for the model's tool result AND tee each line to the
	// streaming hook so the user sees live progress in the TUI.
	var buf bytes.Buffer
	maxBytes := b.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutputBytes
	}
	bufWriter := newCappedBuffer(&buf, maxBytes)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return NewTextErrorResponse("bash: stdout pipe: " + err.Error()), nil
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return NewTextErrorResponse("bash: stderr pipe: " + err.Error()), nil
	}
	start := time.Now()
	if err := c.Start(); err != nil {
		return NewTextErrorResponse("bash: start: " + err.Error()), nil
	}

	// Two scanners running concurrently, one per pipe. Each line is
	// written to the capped buffer (for the tool result) and to the
	// OnOutput hook (for live TUI display). A mutex protects the
	// shared buffer; the hook is invoked outside the lock so a slow
	// renderer doesn't serialize the readers.
	var mu sync.Mutex
	var wg sync.WaitGroup
	stream := func(r io.Reader) {
		defer wg.Done()
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 64*1024), 1024*1024)
		for s.Scan() {
			line := s.Text()
			mu.Lock()
			bufWriter.WriteString(line)
			bufWriter.WriteByte('\n')
			mu.Unlock()
			if b.OnOutput != nil {
				b.OnOutput(line)
			}
		}
	}
	wg.Add(2)
	go stream(stdout)
	go stream(stderr)
	runErr := c.Wait()
	wg.Wait()
	elapsed := time.Since(start)

	// Post-run snapshot + diff. Fires the OnFilesChanged hook with
	// any path whose mtime advanced or that newly appeared. Skipped
	// when no hook is registered (saves the second walk).
	if b.OnFilesChanged != nil {
		if changed := diffMtimeSnapshots(preSnap, snapshotMtimes(b.Workspace)); len(changed) > 0 {
			b.OnFilesChanged(changed)
		}
	}

	out := buf.Bytes()
	byteTruncated := bufWriter.truncated
	if byteTruncated {
		out = append(out, []byte(fmt.Sprintf("\n…(truncated; output exceeded %d bytes)\n", maxBytes))...)
	}

	// Line-based head+tail truncation for the model's tool result.
	// Long-running commands (dev servers, test suites, typecheck on
	// big monorepos) emit hundreds of lines where the signal is at
	// the start (startup banner / first error) and the end (final
	// status / actual error message) — the middle is usually
	// progress noise. Returning head 40 + tail 40 with a separator
	// gives the model both ends without burning its output budget
	// on output it would summarize away anyway. The streaming
	// OnOutput hook still saw every line; this only affects what
	// gets sent back to the LLM.
	out, lineTruncated, totalLines, droppedLines := truncateLines(out, defaultBashHeadLines, defaultBashTailLines)

	exitCode := 0
	if runErr != nil {
		// ExitError carries the real exit code; everything else
		// (timeout, missing binary, etc.) we surface as an error
		// response with code -1 so the model can react.
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	header := fmt.Sprintf("$ %s\n(exit=%d, %s",
		oneLine(cmd), exitCode, elapsed.Round(time.Millisecond))
	if byteTruncated {
		header += ", byte-capped"
	}
	if lineTruncated {
		header += fmt.Sprintf(", %d/%d lines (head %d + tail %d, %d dropped)",
			defaultBashHeadLines+defaultBashTailLines, totalLines,
			defaultBashHeadLines, defaultBashTailLines, droppedLines)
	}
	header += ")\n"

	body := string(out)
	// Cost trailer. When the output is large enough that the model
	// will pay real input tokens for it on the next turn (~4 chars
	// per token; 4 KB ≈ 1k tokens), append a one-line nudge so the
	// agent learns to redirect-and-view rather than firehose stdout
	// back through the LLM. Skipped on small outputs (no signal) and
	// on errors (the agent needs the full text to debug).
	if !lineTruncated && exitCode == 0 && len(body) > 4000 {
		approxTokens := len(body) / 4
		body += fmt.Sprintf("\n[hint: this returned ~%dk tokens. For long-running commands, redirect to a file (`%s > /tmp/out.log 2>&1`) and `view` it with offset/limit — far cheaper than rereading every line through the model.]\n",
			approxTokens/1000+1, oneLine(firstCommandToken(cmd)))
	}
	resp := NewTextResponse(header + body)
	if exitCode != 0 {
		resp.IsError = true
	}
	return resp, nil
}

// alwaysApprove names commands that must always reach the human
// approval prompt, regardless of allowlist or session auto-allow.
// rm and `git rm` delete files; an allowlist-auto-approve here would
// let an agent silently `rm -rf .` if the user had auto-allowed `rm`
// once for a tame `rm foo.tmp`. Returning the destructive command to
// the user for explicit re-confirmation closes that hole.
//
// Detection: first-token match on `rm` covers all rm variants.
// `git rm` is detected separately as two-token `git rm ...` because
// `git` itself is broad enough that auto-approving the prefix is
// reasonable for most git operations — we just carve out the
// deleting subcommand.
func mustReachApproval(cmd string) bool {
	first := firstCommandToken(cmd)
	if first == "rm" {
		return true
	}
	if first == "git" {
		fields := strings.Fields(cmd)
		if len(fields) >= 2 && fields[1] == "rm" {
			return true
		}
	}
	return false
}

// destructiveRmRegex matches rm invocations with -r, -f, or -rf in
// any flag-cluster order (-r -f, -fr, -rfv, etc). Used to surface an
// informational warning at the approval prompt; the warning never
// blocks or alters the decision — it's a label so the user reads the
// command with their eyes open.
//
// Standalone-rm only: `git rm` doesn't take POSIX rm flags and the
// "recursive" semantic doesn't apply to its index-removal mode.
var destructiveRmRegex = regexp.MustCompile(`(?i)^\s*rm\b[^|;&]*\s-[a-zA-Z]*(?:r|f)`)

// destructiveWarning returns a one-line label to display alongside
// the approval prompt for commands that may recursively force-remove
// files. Empty string means no warning.
func destructiveWarning(cmd string) string {
	if destructiveRmRegex.MatchString(cmd) {
		return "may recursively force-remove files"
	}
	return ""
}

// findDestructiveFlagRegex matches the destructive find flags
// (-delete, -exec, -execdir, -ok, -okdir) word-anchored. The
// caller checks this against each command segment AFTER verifying
// the segment's head is literally `find`, so a quoted echo like
// `echo 'find . -delete'` doesn't false-match — echo is the head
// of that segment, not find.
var findDestructiveFlagRegex = regexp.MustCompile(`\s-(?:delete|exec|execdir|ok|okdir)\b`)

// rejectIfDestructiveFind returns a non-empty rejection reason when
// the command embeds a find with mutating flags. Walks pipeline /
// chain segments the same way firstShadowedTokenInChain does so
// that `something | find . -delete` and `cd foo && find -delete`
// both catch, while `echo 'find -delete'` does NOT (echo's first
// token isn't find).
func rejectIfDestructiveFind(cmd string) string {
	for _, seg := range splitChainSegments(cmd) {
		first := firstCommandToken(seg)
		if first != "find" {
			continue
		}
		if findDestructiveFlagRegex.MatchString(seg) {
			return "`find` with -delete / -exec / -execdir / -ok / -okdir is blocked. Use kai_files or kai_tree for traversal, then issue an explicit `rm` (which reaches the approval prompt) for any files you actually want removed."
		}
	}
	return ""
}

// splitChainSegments slices cmd on shell chain operators (&&, ||,
// ;, |) so callers can examine each segment's head independently.
// Quoted strings are not parsed — a `;` inside single quotes still
// splits, which is fine for our purposes because we only test for
// destructive flags on segments whose head is literally `find`,
// and a quoted-string segment can't have `find` as its head.
func splitChainSegments(cmd string) []string {
	parts := []string{""}
	i := 0
	for j := 0; j < len(cmd); j++ {
		switch {
		case j+1 < len(cmd) && (cmd[j] == '&' && cmd[j+1] == '&'):
			parts = append(parts, "")
			i++
			j++
		case j+1 < len(cmd) && (cmd[j] == '|' && cmd[j+1] == '|'):
			parts = append(parts, "")
			i++
			j++
		case cmd[j] == ';' || cmd[j] == '|':
			parts = append(parts, "")
			i++
		default:
			parts[i] += string(cmd[j])
		}
	}
	return parts
}

// shadowedTools maps the first-token of bash commands that duplicate
// kai_* tools to the tool the agent should call instead. Hard-rejected
// in Run() so the model can't sidestep the system-prompt nudge by
// running 40 cat invocations: the rejection itself names the right
// tool, which counts as in-context retraining for the rest of the turn.
//
// Pipelines and `&&` chains: only the FIRST token gets checked, same
// as the allowlist. A command like `cat foo.go | grep bar` is matched
// at "cat" and rejected; the model sees the message and switches to
// view + kai_grep on the retry.
var shadowedTools = map[string]string{
	"cat":  "view",
	"head": "view",
	"tail": "view",
	"less": "view",
	"more": "view",
	"find": "kai_files (or kai_tree for a single directory)",
	"ls":   "kai_files (or kai_tree)",
	"tree": "kai_tree",
	"grep": "kai_grep",
	"rg":   "kai_grep",
	"ag":   "kai_grep",
	"ack":  "kai_grep",
	"sed":  "edit",
	"awk":  "view + edit",
	"wc":   "view (its line numbers tell you the count directly)",
	// Scripting-language exploration is the highest-leverage abuse
	// of the bash escape hatch: a single `python3 -c '...'` can
	// open(), readlines(), and grep an arbitrary number of files
	// in one call, sidestepping the per-tool read budget entirely.
	// The 2026-05-15 dogfood pinned this — an agent ran 8+ python3
	// /dev/stdin scripts to walk source files after the kai tools
	// hit the read gate. Block at the first-token level so every
	// flavor (python, python3, python2, python3.11, perl) gets the
	// same rejection.
	"python":  "view (single file) or kai_grep (content search) — bash scripting languages bypass the read budget",
	"python2": "view (single file) or kai_grep (content search) — bash scripting languages bypass the read budget",
	"python3": "view (single file) or kai_grep (content search) — bash scripting languages bypass the read budget",
	"perl":    "view (single file) or kai_grep (content search) — bash scripting languages bypass the read budget",
}

func rejectIfShadowsKaiTool(cmd string) string {
	// Walk every command position in the chain — start of string and
	// after each `&&`, `||`, `;`, `|`. The 2026-05-14 dogfood pinned
	// this: the agent wrote `cd kai-cli && grep -n -E "..."` and the
	// guard saw `cd` (not shadowed) as the first token and let the
	// grep through. Checking every segment catches subdir-cd chains
	// and piped reads like `something | grep pattern` too.
	first, suggested := firstShadowedTokenInChain(cmd)
	if first == "" {
		// Not a single-token shadow. Check the two-token `git diff` /
		// `git show` family separately — it's the highest-frequency
		// escape from the semantic tooling and git diff can't see the
		// snapshot/run provenance kai_diff exposes.
		return gitDiffShadow(cmd)
	}
	base := fmt.Sprintf("don't shell out to %q for tasks the kai tools cover — call %s instead. The kai tool is faster, cheaper, and runs without an approval prompt.",
		first, suggested)
	// Translate the rejected command into the equivalent kai_* call so
	// the agent has a literal copy-paste path on the next turn. Saves
	// the round-trip where it would otherwise have to re-parse the
	// shadow message and reverse-engineer the args. Best-effort: if
	// translation isn't trivial (compound pipes, exotic flags), we
	// fall through to the base message.
	if hint := translationHint(first, cmd); hint != "" {
		return base + " Equivalent call:\n  " + hint
	}
	return base + fmt.Sprintf(" (If you genuinely need %q for something the kai tool can't do, that's a sign the request is wrong-shaped.)", first)
}

// gitDiffShadow redirects `git diff` to kai_diff for the working-tree /
// file diffs the agent reaches for out of habit. kai_diff renders the
// SEMANTIC change view (which functions/types/contracts changed) plus
// the patch, and can diff snapshot-to-snapshot with the run provenance
// git diff structurally can't see — the orchestrator absorbs each run's
// changes into the working tree (often uncommitted), so git has no
// per-run boundary to attribute them to. Returns "" (allow) for the
// git-specific cases kai has no equivalent for: a ref-RANGE diff
// (`a..b`) over git history kai may not have snapshotted, and
// `--cached`/`--staged` index diffs (kai has no staging area).
func gitDiffShadow(cmd string) string {
	norm := cmd
	for _, sep := range []string{"&&", "||", ";", "|"} {
		norm = strings.ReplaceAll(norm, sep, "\x00")
	}
	for _, seg := range strings.Split(norm, "\x00") {
		fields := strings.Fields(seg)
		if len(fields) < 2 || fields[0] != "git" || fields[1] != "diff" {
			continue
		}
		allow := false
		for _, a := range fields[2:] {
			if strings.Contains(a, "..") || a == "--cached" || a == "--staged" {
				allow = true
				break
			}
		}
		if allow {
			continue
		}
		return "don't shell out to \"git diff\" for working-tree or file diffs — call kai_diff instead. " +
			"kai_diff returns the SEMANTIC change view (which functions/types/contracts changed), not just +/- lines, " +
			"plus the patch, and can diff snapshot-to-snapshot with run provenance git diff can't see.\n" +
			"  Equivalent call:\n" +
			"    kai_diff {}                                  (all changed files)\n" +
			"    kai_diff {\"file\":\"path/to/file.go\"}          (scope to one file)\n" +
			"    kai_diff {\"since\":\"<snap>\",\"until\":\"<snap>\"}  (compare two snapshots)\n" +
			"  (A git ref-range diff like `git diff a..b`, or `git diff --cached`, that kai can't serve is the exception — those still use git.)"
	}
	return ""
}

// translationHint returns the literal kai_* tool invocation that
// matches a rejected bash command, when the translation is
// unambiguous. Renders as JSON-shaped tool_use the model can copy
// directly. Empty when the command is too complex to map cleanly
// (multi-arg find, piped grep, etc.) — the base rejection message
// still trains the right behavior in those cases.
func translationHint(first, cmd string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmd), first))
	switch first {
	case "cat", "head", "tail", "less", "more":
		// Single-arg form: `cat foo.go`. Strip common pagination
		// flags so `head -n 50 foo` still translates. The view tool
		// has offset/limit; we don't try to map exact line counts —
		// the agent can pass them itself if needed.
		path := stripFlagsAndQuotes(rest)
		if path == "" || strings.ContainsAny(path, "|;&<>") {
			return ""
		}
		return fmt.Sprintf(`view {"file_path":%q}`, path)
	case "grep", "rg", "ag", "ack":
		// `grep "pattern" path` or `grep -rn pattern path`. Take the
		// first non-flag token as the query. We don't try to extract
		// the path — kai_grep walks the workspace by default.
		query := firstNonFlagToken(rest)
		if query == "" || strings.ContainsAny(rest, "|;") {
			return ""
		}
		query = strings.Trim(query, `"'`)
		return fmt.Sprintf(`kai_grep {"query":%q}`, query)
	case "ls", "tree":
		path := stripFlagsAndQuotes(rest)
		if path == "" {
			path = "."
		}
		if strings.ContainsAny(path, "|;&<>") {
			return ""
		}
		return fmt.Sprintf(`kai_tree {"path":%q}`, path)
	case "find":
		// Only handle the common `find <path> -name "*.go"` shape.
		// Anything more (multi-predicate, -exec, depth flags) falls
		// through; the model can read the rejection message and use
		// kai_files manually.
		if i := strings.Index(rest, "-name"); i >= 0 {
			pattern := firstNonFlagToken(rest[i+len("-name"):])
			pattern = strings.Trim(pattern, `"'`)
			if pattern != "" {
				return fmt.Sprintf(`kai_files {"glob":%q}`, pattern)
			}
		}
		return ""
	}
	return ""
}

// stripFlagsAndQuotes returns the first non-flag, non-flag-arg token
// from `args`, with surrounding quotes removed. Used by
// translationHint for `cat -n -B 5 foo.go` → "foo.go". Crude:
// assumes flag args are single tokens with no embedded spaces, which
// covers ~all real cases.
func stripFlagsAndQuotes(args string) string {
	fields := strings.Fields(args)
	skipNext := false
	for i, t := range fields {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(t, "-") {
			// `-n 50 foo` — `-n` consumes a numeric arg. Only skip
			// the following token when it isn't itself a flag, so
			// `-n -B 5 foo` correctly returns "foo" not "5".
			if len(t) <= 2 && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
				skipNext = true
			}
			continue
		}
		return strings.Trim(t, `"'`)
	}
	return ""
}

// firstNonFlagToken returns the first whitespace-separated token
// that isn't a flag (no leading `-`). Used for grep-style commands
// where the pattern is the first positional arg.
func firstNonFlagToken(args string) string {
	for _, t := range strings.Fields(args) {
		if !strings.HasPrefix(t, "-") {
			return t
		}
	}
	return ""
}

// checkAllow returns "" when the command passes the allowlist (or the
// allowlist is empty). Otherwise returns a reason string.
func (b *BashTool) checkAllow(cmd string) string {
	reason, allowed := b.checkAllowDetail(cmd)
	if allowed {
		return ""
	}
	return reason
}

// checkAllowDetail is the structured form of checkAllow. The reason
// string distinguishes:
//
//	"auto"  — first-token matched the configured allowlist (skip Approve)
//	""      — no allowlist configured (Approve is the only gate)
//	other   — first-token rejected; allowed=false
//
// Callers use the "auto" sentinel to decide whether the per-command
// approval prompt fires.
func (b *BashTool) checkAllowDetail(cmd string) (string, bool) {
	if len(b.Allow) == 0 {
		return "", true
	}
	first := firstCommandToken(cmd)
	for _, a := range b.Allow {
		if first == a {
			return "auto", true
		}
	}
	return fmt.Sprintf("command %q not in allowlist (%s); ask the human to run it manually or extend `agent.bash_allow` in .kai/config.yaml",
		first, strings.Join(b.Allow, ", ")), false
}

// stripLeadingCdToWorkspace inspects a leading `cd <path> && ` (or
// `cd <path>; `) and decides how to handle it. Returns:
//
//   - rewritten == the command (unchanged) — no cd, or a legitimate
//     subdir cd we should preserve
//   - rewritten with the cd stripped — the cd was a redundant `cd
//     <workspace>` and we removed it
//   - escapeReason non-empty — the cd targets a path OUTSIDE the
//     workspace (a host path the agent hardcoded from the prompt).
//     The caller MUST refuse the command and surface escapeReason
//     verbatim to the agent rather than silently dropping the cd
//     and running the rest. Silent-drop was the previous behavior
//     and it caused agents to loop forever: bash would `cd
//     /Users/... && go build` repeatedly, the cd was silently
//     ignored, `go build` ran in the spawn dir with no go.mod, and
//     the agent never realised its cd wasn't taking effect.
//
// Preserves relative cd ("cd kai-cli && ...") and absolute cd into a
// workspace subdir ("cd <ws>/kai-core && ..."). The latter matters
// because the 2026-05-14 "quality-nits-fix" dogfood burned ~25 turns
// when those legitimate sub-module cds were stripped.
func stripLeadingCdToWorkspace(cmd, workspace string) (string, string) {
	trimmed := strings.TrimSpace(cmd)
	if !strings.HasPrefix(trimmed, "cd ") {
		return cmd, ""
	}
	rest := strings.TrimSpace(trimmed[len("cd "):])
	// Find the chain operator. `&&` first because `;` appears inside
	// `&&` chains we DON'T want to mishandle (`a && b ; c` — uncommon
	// but we'd rather err toward leaving things alone).
	var after string
	if i := strings.Index(rest, "&&"); i >= 0 {
		after = rest[i+2:]
		rest = rest[:i]
	} else if i := strings.Index(rest, ";"); i >= 0 {
		after = rest[i+1:]
		rest = rest[:i]
	} else {
		// Bare `cd <path>` — no chain. A bash run that only cd's
		// accomplishes nothing in our model (each run is a fresh
		// shell). Drop entirely; this isn't an escape, just a no-op.
		return "", ""
	}
	path := strings.TrimSpace(rest)
	path = strings.Trim(path, `"'`) // strip quotes if any
	if path == "" {
		return cmd, ""
	}
	if !filepath.IsAbs(path) {
		// Relative — agent staying inside workspace; preserve.
		return cmd, ""
	}
	cleanPath := filepath.Clean(path)
	cleanWS := filepath.Clean(workspace)
	if cleanPath == cleanWS {
		// Redundant cd to our own workspace; safe to drop.
		return strings.TrimSpace(after), ""
	}
	// Absolute path INSIDE the workspace — legitimate subdir cd
	// (nested module root, script dir, etc.). Preserve.
	if rel, err := filepath.Rel(cleanWS, cleanPath); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		return cmd, ""
	}
	// Absolute path that escapes the workspace. Refuse loudly via
	// the caller — silent drop here is what caused the install-loop
	// failure mode.
	reason := fmt.Sprintf(
		"refusing `cd %s && ...`: that path is outside your workspace (%s).\n"+
			"\n"+
			"You are running inside an isolated CoW workspace, not the host filesystem. "+
			"The repo's mirror lives at the workspace root (%s) — use a relative path "+
			"(e.g. `cd <subdir> && ...`) or an absolute path that stays inside the "+
			"workspace.\n"+
			"\n"+
			"COMMON CASE: if you were trying to invoke a CLI binary that lives outside "+
			"the workspace (e.g., `cd /path/to/project && ./kai stats --json`), just DROP "+
			"the `cd` and invoke the binary directly. The binary is almost always on PATH "+
			"and operates on the current working directory (your workspace), which is "+
			"usually what you want. Try: `kai stats --json` (no cd, no ./). The 2026-05-26 "+
			"edges dogfood pinned this — the model wrote `cd /Users/.../kai && ./kai stats "+
			"--json`, got refused, gave up on bash entirely. The right call was just "+
			"`kai stats --json` from the workspace dir.\n"+
			"\n"+
			"If your task requires writing OUTSIDE the workspace — installing a binary "+
			"to ~/go/bin, modifying user config, touching ~/, registering an MCP server, "+
			"or anything that mutates the host system — STOP and report what you need. "+
			"Do not loop trying to escape; the sandbox will keep refusing.",
		path, cleanWS, cleanWS)
	return cmd, reason
}

// firstCommandToken returns the first whitespace-separated word of
// the command, after stripping any leading env-var assignments
// (`FOO=bar npm test` → `npm`). Crude — doesn't handle pipelines or
// `&&` chains specially; the allowlist is a first-token guard, not a
// full sandbox.
func firstCommandToken(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, tok := range strings.Fields(cmd) {
		if strings.Contains(tok, "=") {
			continue // env-var assignment, skip
		}
		// Strip leading `./` so `./scripts/build` allowlists as `scripts/build`
		// (fine for our purposes — the allowlist is best-effort).
		return strings.TrimPrefix(tok, "./")
	}
	return ""
}

// firstShadowedTokenInChain walks every command position in the chain
// — start of string and after each `&&`, `||`, `;`, `|` — and returns
// the first shadowed tool name it finds (and the suggested kai
// replacement). Empty first return means the chain is clean.
//
// Why every position: agents commonly write `cd kai-cli && grep ...`
// or `cat foo | grep bar`. firstCommandToken only sees the head of
// the chain; the actual shadowed tool runs in a later segment and
// slips past the guard.
// byteInspectionTools start a chain whose downstream segments are
// pardoned from the shadow check. These are byte-level inspection
// commands with no kai-tool substitute: when the model needs to
// verify what's actually in a file at the byte level (typically
// because view's output is ambiguous in template-engine files —
// see braceEscapeNotice in file.go), it pipes one of these
// through standard filters (head/tail/grep) to focus the output.
// Banning the downstream filters in that workflow blocks
// legitimate byte verification with no offered alternative. The
// 2026-05-25 chat-debug log pinned this: the model tried xxd|grep,
// xxd alone, head, tail — all blocked — and timed out before
// reaching the obvious diagnosis it would have found in seconds
// with a hex dump.
var byteInspectionTools = map[string]bool{
	"xxd":     true,
	"hexdump": true,
	"od":      true,
	"file":    true,
}

func firstShadowedTokenInChain(cmd string) (token, suggested string) {
	// Replace the multi-byte chain separators with a single common
	// delimiter, then split. Slightly imprecise (a quoted "||" in a
	// string literal would also split) but the alternative is a full
	// shell parser, which is overkill for a deterrent that doesn't
	// claim to be a sandbox.
	cmd = strings.ReplaceAll(cmd, "&&", "\x00")
	cmd = strings.ReplaceAll(cmd, "||", "\x00")
	cmd = strings.ReplaceAll(cmd, ";", "\x00")
	cmd = strings.ReplaceAll(cmd, "|", "\x00")
	// Walk segments left-to-right. Once a byte-inspection segment
	// (xxd / hexdump / od / file) appears, downstream segments are
	// pardoned — they're filtering byte output, not substituting
	// for kai tools.
	pardonRemaining := false
	for _, seg := range strings.Split(cmd, "\x00") {
		first := firstCommandToken(seg)
		if first == "" {
			continue
		}
		if byteInspectionTools[first] {
			pardonRemaining = true
			continue
		}
		if pardonRemaining {
			continue
		}
		if s, ok := shadowedTools[first]; ok {
			// A content reader used to WRITE/create a file (cat > f, heredoc)
			// is not a read the kai tools cover — don't corner the agent.
			if fileReaderTools[first] && (strings.Contains(seg, ">") || strings.Contains(seg, "<<")) {
				if strings.Contains(seg, "<<") {
					pardonRemaining = true
				}
				continue
			}
			return first, s
		}
		if strings.Contains(seg, "<<") {
			pardonRemaining = true
		}
	}
	return "", ""
}

// fileReaderTools are content readers that double as file writers via
// redirection/heredoc; when writing they aren't a read substitution.
var fileReaderTools = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
}

// blockedInterpreters are scripting-language interpreters that must
// never run via bash. Unlike the shadowedTools CLIs (grep, cat) whose
// names also appear as flag values (`git log --grep`), interpreter
// names essentially never occur as legitimate non-command tokens —
// so scanForBlockedInterpreter can scan EVERY token by basename,
// which is what closes the chaining/wrapping bypasses a segment-head
// check misses.
var blockedInterpreters = map[string]bool{
	"python":  true,
	"python2": true,
	"python3": true,
	"perl":    true,
	"ruby":    true,
}

// scanForBlockedInterpreter returns the first blocked interpreter name
// found anywhere in cmd, matching by path basename so `python3`,
// `/usr/bin/python3`, and `./python3` all hit. Returns "" when clean.
//
// It tokenizes on whitespace AND shell metacharacters (| & ; ( ) ` ' "
// < >) so an interpreter hidden inside `$(...)`, backticks, a
// `bash -c '...'` argument, an `xargs`/`env` wrapper, or a
// newline-separated command is still seen as its own token. This is a
// deterrent, not a sandbox — base64-decode-and-pipe-to-sh would still
// evade it — but it closes every bypass an agent reaches for when
// it's simply trying to route around the budget, not maliciously
// obfuscating.
//
// Tradeoff: a literal interpreter name inside a quoted string (e.g.
// `git commit -m "fix python bug"`) is also flagged. Accepted —
// rewording a commit message is cheap; the alternative (a quote-aware
// mini-parser) is far more code for a rare, low-cost false positive.
func scanForBlockedInterpreter(cmd string) string {
	fields := strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '|', '&', ';', '(', ')', '`', '\'', '"', '<', '>':
			return true
		}
		return false
	})
	for _, tok := range fields {
		tok = strings.TrimPrefix(tok, "./")
		if i := strings.LastIndexByte(tok, '/'); i >= 0 {
			tok = tok[i+1:]
		}
		if blockedInterpreters[tok] {
			return tok
		}
	}
	return ""
}

// bashIgnoreDirs is the set of directory names the mtime walker
// skips when snapshotting the workspace. These are the heavyweight
// areas where a single bash command can touch tens of thousands of
// files (npm install in node_modules, git operations) — walking
// them on every bash call would be prohibitive. Source files at
// arbitrary depth above these still get tracked.
var bashIgnoreDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".kai":         true,
	"vendor":       true,
	".venv":        true,
	"__pycache__":  true,
	"target":       true,
	"dist":         true,
	"build":        true,
}

// snapshotMtimes walks the workspace returning a map of relative
// path → modification timestamp (UnixNano). Returns nil on error so
// the caller's diff sees an empty pre-snapshot and reports every
// file as "changed" — degraded but not broken.
//
// The walker bails on any single entry's error (Lstat failure on a
// permission-denied file, etc.) rather than failing the whole
// snapshot; one un-stat-able file shouldn't poison the rest.
func snapshotMtimes(workspace string) map[string]int64 {
	if workspace == "" {
		return nil
	}
	out := make(map[string]int64, 256)
	_ = filepath.WalkDir(workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip this entry, keep walking
		}
		name := d.Name()
		if d.IsDir() {
			if bashIgnoreDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip hidden files and symlinks — both add noise; mtime
		// doesn't always reflect symlink target changes anyway.
		if strings.HasPrefix(name, ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(workspace, p)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = info.ModTime().UnixNano()
		return nil
	})
	return out
}

// diffMtimeSnapshots returns the workspace-relative paths whose
// mtime advanced (or that appeared anew) between pre and post.
// Sorted so output ordering is stable across runs — callers that
// log the list will see deterministic output even when the
// underlying map iteration is randomized.
func diffMtimeSnapshots(pre, post map[string]int64) []string {
	if len(post) == 0 {
		return nil
	}
	var changed []string
	for path, t := range post {
		if priorT, ok := pre[path]; !ok || t > priorT {
			changed = append(changed, path)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	// Stable order — paths sort lexicographically for readable
	// downstream output.
	sortStrings(changed)
	return changed
}

// sortStrings is a tiny adapter so we don't pull "sort" into a
// file that doesn't otherwise need it. Stable-enough for our
// purposes (paths are unique, so stability is moot).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// cappedBuffer wraps a *bytes.Buffer and stops writing once it has
// reached the cap. Used by the streaming bash runner so the buffer
// fed back to the model can't grow without bound on a chatty
// command (npm install in a fresh repo can dump megabytes); the
// streaming hook still sees every line.
type cappedBuffer struct {
	buf       *bytes.Buffer
	cap       int
	truncated bool
}

func newCappedBuffer(buf *bytes.Buffer, cap int) *cappedBuffer {
	return &cappedBuffer{buf: buf, cap: cap}
}

func (c *cappedBuffer) WriteString(s string) {
	if c.buf.Len() >= c.cap {
		c.truncated = true
		return
	}
	remaining := c.cap - c.buf.Len()
	if len(s) > remaining {
		s = s[:remaining]
		c.truncated = true
	}
	c.buf.WriteString(s)
}

// WriteByte mirrors bytes.Buffer.WriteByte but stops once the cap is
// reached. It carries the canonical io.ByteWriter signature (returning
// error) so go vet's stdmethods analyzer doesn't flag the name; the
// error is always nil and callers may ignore it.
func (c *cappedBuffer) WriteByte(b byte) error {
	if c.buf.Len() >= c.cap {
		c.truncated = true
		return nil
	}
	return c.buf.WriteByte(b)
}

// truncateOutput keeps the first maxBytes of output and appends a
// truncation marker if anything was dropped. Returning the head
// rather than the tail matches what `head -c` would do — most
// command failures surface near the start (configuration errors,
// missing dependencies); long tails are usually noise.
func truncateOutput(b []byte, maxBytes int) ([]byte, bool) {
	if len(b) <= maxBytes {
		return b, false
	}
	out := make([]byte, 0, maxBytes+64)
	out = append(out, b[:maxBytes]...)
	out = append(out, []byte(fmt.Sprintf("\n…(truncated; output exceeded %d bytes)\n", maxBytes))...)
	return out, true
}

// truncateLines keeps the first head and last tail lines of out,
// dropping the middle and inserting a "--- N lines truncated ---"
// separator. Returns (newOut, didTruncate, totalLines, droppedLines).
//
// When totalLines <= head+tail nothing is dropped — the original
// bytes are returned unchanged so callers don't pay for the work.
//
// The split point is line-based, not byte-based: cuts only happen at
// '\n' boundaries so we never emit half a line. The trailing '\n' on
// the last line is preserved if it was there originally — the
// model's renderer expects newline-terminated blocks.
func truncateLines(out []byte, head, tail int) ([]byte, bool, int, int) {
	if head <= 0 || tail <= 0 || len(out) == 0 {
		return out, false, 0, 0
	}
	// Count lines by '\n'. A trailing newline counts as a line
	// terminator, not an empty line, so "a\nb\n" is 2 lines not 3.
	totalLines := 0
	for _, b := range out {
		if b == '\n' {
			totalLines++
		}
	}
	// If the last byte isn't '\n', the final partial line still
	// counts.
	if len(out) > 0 && out[len(out)-1] != '\n' {
		totalLines++
	}
	if totalLines <= head+tail {
		return out, false, totalLines, 0
	}

	// Find the byte offset just after the head-th newline.
	headEnd := 0
	seen := 0
	for i, b := range out {
		if b == '\n' {
			seen++
			if seen == head {
				headEnd = i + 1
				break
			}
		}
	}
	// Find the byte offset of the start of the tail-th-from-end line.
	// Walk backward counting newlines; tailStart points to the byte
	// AFTER the (totalLines - tail)-th newline.
	tailStart := len(out)
	seen = 0
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] == '\n' && i != len(out)-1 {
			seen++
			if seen == tail {
				tailStart = i + 1
				break
			}
		}
	}
	// Defensive: if the offsets crossed (shouldn't given the count
	// check above), bail out and return the original.
	if headEnd >= tailStart {
		return out, false, totalLines, 0
	}

	dropped := totalLines - head - tail
	separator := []byte(fmt.Sprintf("\n--- %d lines truncated ---\n", dropped))
	combined := make([]byte, 0, headEnd+len(separator)+(len(out)-tailStart))
	combined = append(combined, out[:headEnd]...)
	combined = append(combined, separator...)
	combined = append(combined, out[tailStart:]...)
	return combined, true, totalLines, dropped
}

// oneLine collapses multi-line commands into a single line for the
// echo-back header. Keeps the response readable when the agent
// pastes a heredoc or backslash-continued command.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}
