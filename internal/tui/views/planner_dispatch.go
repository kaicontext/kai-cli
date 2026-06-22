package views

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"kai/api/agent"
	"kai/api/message"
	"kai/api/session"
	"kai/api/agentprompt"
	"kai/api/authorship"
	"kai/api/graph"
	"kai/api/provider"
	"kai/api/orchestrator"
	"kai/api/planner"
	"kai/api/promptenv"
	"kai/api/triage"
	"kai/internal/tui/fixxy"
	"kai/api/projects"
	"kai/api/safetygate"
)

// HostCommandDoneMsg carries the result of a host-shell command kai
// ran on the user's behalf after they approved the host-task fast
// path. Output is the combined stdout+stderr, Err is the run error
// (non-nil for non-zero exit; the message still surfaces it so the
// user can see what went wrong).
//
// DetectedError, when non-empty, holds the first error-shaped line
// kai found in the output. The REPL uses this to auto-dispatch a
// follow-up chat turn so the agent sees what went wrong without the
// user having to copy/paste — provided the user hasn't already moved
// on (same guard as the critic auto-retry).
//
// Detached is true when the command was still running when the
// capture window expired. Kai released the process (it keeps
// running in the user's shell) and surfaced what it had so far.
type HostCommandDoneMsg struct {
	Command       string
	Output        string
	Err           error
	DetectedError string
	Detached      bool
}

// runHostCommand returns a tea.Cmd that executes cmd via `bash -c`
// in kai's own process context — i.e. the user's actual shell with
// the user's actual permissions. NOT inside a CoW spawn. This is
// what makes `cd kai-cli && make install` work: kai writes the
// resulting binary to ~/go/bin/kai exactly the way the user would.
//
// The 60s timeout is generous for the kai-side recipes (make
// install ~5-10s, kai auth login is interactive — see note below).
//
// Interactive commands: `kai auth login` opens a browser flow
// expecting stdin. Running it captured this way will not work
// well — stdin is /dev/null and the user can't enter the magic
// token. For the v1 of host-task execution we accept that limit;
// the dismiss path remains usable for the user to run interactive
// commands themselves. A future revision can either spawn an
// inherited-tty subprocess or short-circuit interactive recipes
// to "please run in your terminal."
// hostCommandCaptureWindow is the default window for quick
// commands (make install, npm install, file ops). 12s is generous
// for anything synchronous — the command usually exits well within
// it, and we get the full output and an honest exit code.
const hostCommandCaptureWindow = 12 * time.Second

// hostCommandDevServerWindow is the extended window for commands
// matching dev-server shape (npm run dev, vite, webpack, electron,
// watch). Those typically take 15-30s to surface their first error
// — vite finishes its initial transform AFTER reporting "ready",
// and the Electron renderer's first page load triggers the actual
// pre-transform errors. 35s gives that whole sequence room to fire
// without the user thinking kai is hung. Trade-off accepted: dev
// commands hold the prompt for ~3x longer than a fast install.
const hostCommandDevServerWindow = 35 * time.Second

// hostCommandWindowFor picks the capture window based on the
// command shape. Plain string matching — no regex — against the
// command tokens. Dev-server-y commands get the extended window;
// everything else gets the default.
//
// The pattern check is lightweight: lowercase the command, look
// for known dev-mode tokens. We intentionally match permissively
// — false positives (a quick command labeled "dev") just hold
// the prompt a bit longer, which is harmless. False negatives
// (a real dev server not matching the list) miss errors, which
// is what we're trying to fix.
func hostCommandWindowFor(cmd string) time.Duration {
	low := strings.ToLower(cmd)
	devTokens := []string{
		"npm run dev", "npm start", "npm run start",
		"yarn dev", "yarn start",
		"pnpm dev", "pnpm start",
		"vite", "webpack serve", "next dev", "next start",
		"electron .", "electron ./", "watch", "nodemon",
		"hugo server", "jekyll serve", "go run", "python -m http",
	}
	for _, t := range devTokens {
		if strings.Contains(low, t) {
			return hostCommandDevServerWindow
		}
	}
	return hostCommandCaptureWindow
}

func runHostCommand(cmd string) tea.Cmd {
	return func() tea.Msg {
		// Use Start() + waiter goroutine + select-on-deadline so we
		// can detach long-running processes instead of killing them.
		// CombinedOutput() would block until exit; for `npm run dev`
		// that means the dev server dies the moment kai's context
		// times out, and the user has no app to interact with.
		c := exec.Command("bash", "-c", cmd)
		var buf safeBuf
		c.Stdout = &buf
		c.Stderr = &buf
		if err := c.Start(); err != nil {
			return HostCommandDoneMsg{Command: cmd, Err: err}
		}

		done := make(chan error, 1)
		go func() { done <- c.Wait() }()

		var (
			err      error
			detached bool
		)
		select {
		case err = <-done:
			// Command exited within the window. Output is whatever
			// the buffer collected; err carries exit status.
		case <-time.After(hostCommandWindowFor(cmd)):
			// Still running at deadline. Release the process so the
			// user keeps their app running, surface what we have.
			detached = true
			if c.Process != nil {
				_ = c.Process.Release()
			}
		}

		output := buf.String()
		detected := detectHostCommandError(output, err, detached)
		return HostCommandDoneMsg{
			Command:       cmd,
			Output:        output,
			Err:           err,
			DetectedError: detected,
			Detached:      detached,
		}
	}
}

// safeBuf is a thread-safe bytes.Buffer wrapper so the goroutine
// writing stdout/stderr can't race with the main goroutine reading
// String() at deadline. The standard library's bytes.Buffer is
// explicitly NOT safe for concurrent access; without the mutex
// race detector flags this immediately.
type safeBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// detectHostCommandError scans the captured output for an error
// worth surfacing back to the agent. Returns either a single
// summary line or a multi-line context block when the error is
// vite/svelte-style (Plugin + numbered source lines + carat).
//
// 2026-05-25 dogfood pinned the multi-line need: svelte-preprocess
// with lang="ts" strips the script block during preprocessing,
// shifting subsequent line numbers. Vite's error reports
// file:line:col against the PREPROCESSED file, but the agent's
// view tool reads SOURCE — the numbers don't reconcile. When the
// auto-followup pastes only "Pre-transform error: foo.svelte:181:63
// Unexpected token", the agent looks up source line 181, gets
// different content from what vite's context block shows, and
// confidently misdiagnoses. The context block (the numbered
// source-line excerpt + carat that vite ALREADY emits in its
// output) carries the actual buggy content — once the model sees
// THAT, no line-number reconciliation is needed.
//
// Priority: explicit non-zero exit (when the process actually
// exited) means the process intended failure — surface the last
// few lines of output. When the process was detached (still
// running), we can't trust exit status, so scan the output for
// error-keyword lines instead.
func detectHostCommandError(output string, err error, detached bool) string {
	if strings.TrimSpace(output) == "" {
		return ""
	}
	// Process exited non-zero — surface the LAST non-empty line as
	// the error summary (it's usually the most specific). Bounded
	// to 240 chars so a verbose stack trace doesn't dominate.
	if err != nil && !detached {
		lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			if len(line) > 240 {
				line = line[:240] + "…"
			}
			return line
		}
	}
	// Detached or zero-exit: scan for error-keyword lines. Long-
	// running processes (vite, webpack, npm run dev) frequently
	// keep the parent alive while their child compiler reports an
	// error — exit status is unreliable, output content is the
	// only signal.
	lines := strings.Split(output, "\n")
	for idx, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "exception") ||
			strings.Contains(lower, "failed") ||
			strings.Contains(lower, "unexpected") {
			// Vite/Plugin-style error: capture the multi-line
			// context block (Plugin line + numbered source-line
			// excerpt + carat). This carries the actual buggy
			// CONTENT, which the agent's view tool can't recover
			// when a preprocessor has shifted line numbers.
			if ctx := captureViteErrorContext(lines, idx); ctx != "" {
				return ctx
			}
			// Non-vite-style: single line, capped.
			if len(line) > 240 {
				line = line[:240] + "…"
			}
			return line
		}
	}
	return ""
}

// captureViteErrorContext returns the full vite/plugin error
// context block when the line at idx begins a recognizable vite
// error. The block typically looks like:
//
//	[vite] Pre-transform error: file.svelte:181:63 ... Unexpected token
//	  Plugin: vite-plugin-svelte
//	  File: file.svelte:181:63
//	   179 |    ...
//	   180 |    ...
//	   181 |    + if showFlag {
//	                          ^
//	   182 |    ...
//
// We capture from the error line until we hit either: a non-
// context line (no leading whitespace AND not numbered/Plugin/File),
// a blank-line followed by something unrelated, or
// viteContextMaxLines (15) lines total.
//
// Returns "" when the line at idx doesn't look vite-shaped — caller
// falls through to single-line capture. The vite-shaped check
// requires either "[vite]" or "[plugin:" or "vite-plugin-" in the
// trigger line OR within the next 3 lines, so plain "Error: foo"
// without vite context returns single-line.
func captureViteErrorContext(lines []string, idx int) string {
	if !viteErrorNearby(lines, idx) {
		return ""
	}
	end := idx
	for end < len(lines) && end-idx < viteContextMaxLines {
		end++
		if end >= len(lines) {
			break
		}
		l := lines[end]
		// Continuation: blank, indented (numbered source/Plugin/File/carat),
		// or contains "|" within first ~10 chars (numbered source).
		if l == "" || isViteContextLine(l) {
			continue
		}
		// Non-continuation: stop here, don't include this line.
		break
	}
	block := strings.TrimRight(strings.Join(lines[idx:end], "\n"), "\n")
	// Cap total block length so a pathological output can't blow
	// the model's token budget. Per-block 1200 chars ≈ ~300 tokens —
	// generous for a vite error's 6-8 numbered source lines.
	if len(block) > viteContextMaxBytes {
		block = block[:viteContextMaxBytes] + "\n…"
	}
	return block
}

const (
	viteContextMaxLines = 15
	viteContextMaxBytes = 1200
)

func viteErrorNearby(lines []string, idx int) bool {
	hi := idx + 4
	if hi > len(lines) {
		hi = len(lines)
	}
	for i := idx; i < hi; i++ {
		lower := strings.ToLower(lines[i])
		if strings.Contains(lower, "[vite]") ||
			strings.Contains(lower, "[plugin:") ||
			strings.Contains(lower, "vite-plugin-") ||
			strings.Contains(lower, "pre-transform error") ||
			strings.Contains(lower, "internal server error") {
			return true
		}
	}
	return false
}

// isViteContextLine returns true for the continuation lines that
// appear inside a vite error block: indented metadata (Plugin:,
// File:), numbered source lines (e.g. "  181 | ..."), and the
// carat-only line ("              ^"). Used to decide where the
// block ends.
func isViteContextLine(s string) bool {
	t := strings.TrimLeft(s, " \t")
	if t == "" {
		return true // blank inside block — keep going
	}
	// Leading-space lines are continuation.
	if len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		return true
	}
	// "Plugin: ..." / "File: ..." at column 0 without leading space.
	if strings.HasPrefix(t, "Plugin:") || strings.HasPrefix(t, "File:") {
		return true
	}
	return false
}

// triageRequest sends the user's request through the triage LLM
// and returns its routing decision. On transport / parse failure
// returns Track=TrackPlan, the safe default that still goes through
// the planner confirm step. /plan forced mode short-circuits to
// TrackPlan inside triage.Classify without any LLM call.
//
// The previous version of this code used a regex pre-filter (see
// the deleted host_task.go) to detect TrackHost requests without
// burning an LLM round-trip. It was brittle the same way the
// rename gate (deleted 0.30.27) was brittle — every phrasing not
// in the pattern table fell through, and the maintenance cost of
// growing the table only added more brittle patterns. The LLM
// path is more reliable and produces the literal host_command
// inline (the triage prompt teaches it the kai-specific recipes
// for install / MCP register / kai auth login).
func triageRequest(ctx context.Context, s *PlannerServices, request, sessionID string, forced agent.Mode) triage.Result {
	if s == nil || s.OrchestratorCfg.AgentProvider == nil {
		return triage.Result{Track: triage.TrackPlan, Reason: "no triage sender available"}
	}
	model := s.OrchestratorCfg.AgentModel
	if model == "" {
		// triage.Classify accepts the empty string and the provider
		// will fall back to its own default model. Mirror that;
		// don't force a specific model here.
	}
	var forcedStr string
	if forced == agent.ModePlanning {
		forcedStr = "plan"
	}
	res, _ := triage.Classify(ctx, triageSender{p: s.OrchestratorCfg.AgentProvider}, model, triage.Request{
		UserRequest:  request,
		ForcedMode:   forcedStr,
		ProjectHints: collectProjectHints(s.MainRepo),
		// Context-window for triage. Without it, triage is purely
		// stateless — a continuation request like "can you also fix
		// it in the top left of the sidebar?" looks ambiguous in
		// isolation and gets classified as TrackClarify, which
		// short-circuits to triage's own "what specifically?" reply
		// without ever invoking the session-aware planner or chat
		// agent. 2026-05-26 dogfood pinned this: turn 1 successfully
		// edited TitleBar to show the current directory; turn 2's
		// "fix it in the sidebar" got bounced by triage with no
		// memory of turn 1. Passing the last user/assistant exchange
		// gives triage just enough state to resolve "it" / "also" /
		// "the same" references and route correctly to plan instead
		// of clarify.
		RecentTurns: recentTurnsForTriage(s, sessionID),
	})
	return res
}

// recentTurnsForTriage pulls the last 2 user/assistant exchanges
// from the session store as compact "role: text" strings, suitable
// for triage's RecentTurns slice. Bounded at 4 messages × 600
// chars — see RecentSessionTurns for the why on these knobs.
func recentTurnsForTriage(s *PlannerServices, sessionID string) []string {
	return RecentSessionTurns(s, sessionID, 4, 600)
}

// RecentSessionTurns pulls the last user/assistant text messages
// from a session store as compact "role: text" strings. Returns
// nil when no session exists, the store is missing, or history is
// empty. Text-only: system messages, tool calls, and tool results
// are dropped — callers want the conversational thread, not the
// agent's working scratchpad.
//
// Knobs (caller-chosen because triage and critic have different
// budgets):
//   - maxMessages: how many of the most-recent user/assistant
//     turns to include. Walk is newest → oldest, output is
//     chronological.
//   - maxCharsPerMessage: per-message truncation cap. Long text
//     gets clipped with an ellipsis suffix; the message itself
//     is kept, just shortened.
//
// Used by triage (fast routing, needs ~2 exchanges to resolve
// "it" / "also" references) and the critic (judgment call on
// continuation requests, needs the same exchanges to know
// whether a clarifying question was answerable from context).
func RecentSessionTurns(s *PlannerServices, sessionID string, maxMessages, maxCharsPerMessage int) []string {
	if s == nil || sessionID == "" {
		return nil
	}
	store := s.OrchestratorCfg.AgentSessionStore
	if store == nil {
		return nil
	}
	sess, err := session.Resume(store, sessionID)
	if err != nil || sess == nil {
		return nil
	}
	hist, err := sess.History()
	if err != nil || len(hist) == 0 {
		return nil
	}
	var picked []string
	for i := len(hist) - 1; i >= 0 && len(picked) < maxMessages; i-- {
		m := hist[i]
		if m.Role != message.RoleUser && m.Role != message.RoleAssistant {
			continue
		}
		text := strings.TrimSpace(m.Text())
		if text == "" {
			continue
		}
		if len(text) > maxCharsPerMessage {
			text = text[:maxCharsPerMessage] + "…"
		}
		picked = append(picked, fmt.Sprintf("%s: %s", m.Role, text))
	}
	// Reverse to chronological.
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked
}

// trivialActionFastPath returns a host command for the obvious
// bare-action-verb cases where a triage LLM round-trip adds no
// value. Examples it catches:
//
//	"run"           "run it"      "start"      "start it"
//	"build"         "build it"    "test"       "test it"
//	"launch"        "launch it"   "dev"        "fire it up"
//	"run the app"   "start the dev server"
//
// On a hit, returns (command, label). On a miss, returns ("", "")
// and the caller falls through to the regular triage LLM call.
//
// The fast path is narrow: short prompts (≤4 words) that contain
// exactly one of the action verbs in the table. If the prompt
// adds any code-edit signal ("fix the run handler", "write a test
// for build") the fast path bails and triage handles it.
//
// Manifest selection: package.json's scripts win when the verb
// matches a script name; otherwise we use the standard idiom
// (cargo run, go run, make <target>). A workspace with multiple
// managers (Go AND Node) breaks the tie by preferring the one
// whose verb maps to an existing target.
func trivialActionFastPath(prompt, workspace string) (cmd, label string) {
	verb := matchActionVerb(prompt)
	if verb == "" || workspace == "" {
		return "", ""
	}
	// Don't fire when the prompt is clearly a code-edit request
	// that happens to contain an action verb.
	lc := strings.ToLower(prompt)
	for _, neg := range []string{
		"write a", "add a", "fix the", "refactor", "implement",
		"handler", "function", "endpoint", "test for ", "tests for ",
	} {
		if strings.Contains(lc, neg) {
			return "", ""
		}
	}
	// Try the package.json scripts first — best signal because the
	// project author has named the verb explicitly.
	if data, err := os.ReadFile(filepath.Join(workspace, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if err := json.Unmarshal(data, &pkg); err == nil && len(pkg.Scripts) > 0 {
			// Verb-to-script-name preference order.
			preferences := map[string][]string{
				"run":    {"dev", "start", "serve"},
				"start":  {"start", "dev", "serve"},
				"dev":    {"dev"},
				"launch": {"dev", "start"},
				"build":  {"build"},
				"test":   {"test"},
			}
			candidates := preferences[verb]
			for _, name := range candidates {
				if _, ok := pkg.Scripts[name]; ok {
					return "npm run " + name, "package.json: " + name
				}
			}
		}
	}
	// Cargo.toml — cargo run / cargo build / cargo test.
	if _, err := os.Stat(filepath.Join(workspace, "Cargo.toml")); err == nil {
		switch verb {
		case "run", "start", "launch", "dev":
			return "cargo run", "Cargo.toml: cargo run"
		case "build":
			return "cargo build", "Cargo.toml: cargo build"
		case "test":
			return "cargo test", "Cargo.toml: cargo test"
		}
	}
	// go.mod — go run / go build / go test.
	if _, err := os.Stat(filepath.Join(workspace, "go.mod")); err == nil {
		switch verb {
		case "run", "start", "launch", "dev":
			return "go run ./...", "go.mod: go run"
		case "build":
			return "go build ./...", "go.mod: go build"
		case "test":
			return "go test ./...", "go.mod: go test"
		}
	}
	// Makefile — only fire when the verb matches a target name
	// EXACTLY (avoids the "test" verb firing on a Makefile that
	// has no test target).
	if data, err := os.ReadFile(filepath.Join(workspace, "Makefile")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if len(line) == 0 || line[0] == '\t' || line[0] == '#' || line[0] == ' ' {
				continue
			}
			if i := strings.Index(line, ":"); i > 0 {
				name := strings.TrimSpace(line[:i])
				if strings.ContainsAny(name, " =$%") {
					continue
				}
				if name == verb {
					return "make " + verb, "Makefile: " + verb
				}
			}
		}
	}
	return "", ""
}

// matchActionVerb returns the action verb (run / start / build /
// test / launch / dev) when prompt is a bare invocation of one,
// possibly with a short qualifier ("it", "this", "the app", "the
// dev server"). Returns "" for anything longer or more complex
// — those go through triage.
func matchActionVerb(prompt string) string {
	p := strings.ToLower(strings.TrimSpace(prompt))
	p = strings.TrimRight(p, "!.?")
	// Strip conversational openers BEFORE the word-count check. "can
	// you run the app" is 5 words and would fail the cap, but it's the
	// same intent as the bare "run the app". 2026-05-25 dogfood pinned
	// this: user typed "can you run the app?" and the fast-path
	// missed; the request flowed to triage and the agent then
	// fabricated a fake launch narrative. Stripping the niceties lets
	// the natural conversational shape hit the fast-path.
	p = stripLeadingNicety(p)
	// Whole-word verb at the start, optionally followed by a short
	// qualifier (it / this / that / the X). Cap at 4 words total.
	if wordCount(p) > 4 {
		return ""
	}
	verbs := []string{"run", "start", "build", "test", "launch", "dev"}
	for _, v := range verbs {
		if p == v {
			return v
		}
		if strings.HasPrefix(p, v+" ") {
			rest := p[len(v)+1:]
			// "run it" / "run this" / "run the X" / "run the dev server" / "run the app"
			if rest == "it" || rest == "this" || rest == "that" ||
				rest == "the app" || rest == "the project" ||
				rest == "the dev server" || rest == "the server" ||
				strings.HasPrefix(rest, "the ") {
				return v
			}
		}
	}
	// "fire it up" / "kick it off" — informal run.
	if p == "fire it up" || p == "kick it off" {
		return "run"
	}
	return ""
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}

// stripLeadingNicety removes conversational opener phrases from the
// start of a prompt so the verb-match logic sees the bare action.
// "can you run the app" → "run the app". Plain string prefix
// matching, no regex. Applied repeatedly so chained openers
// ("please could you start it") also strip cleanly.
//
// Niceties are matched in lowercase only — caller is expected to
// have already lowercased the prompt.
func stripLeadingNicety(p string) string {
	niceties := []string{
		"can you ",
		"could you ",
		"would you ",
		"will you ",
		"please ",
		"pls ",
	}
	// Repeatedly strip so "please can you " collapses to "" prefix
	// in two passes. Bounded loop (no nicety appears twice in
	// realistic prompts; cap at 3 just so we can't loop forever
	// on adversarial input).
	for i := 0; i < 3; i++ {
		stripped := false
		for _, n := range niceties {
			if strings.HasPrefix(p, n) {
				p = p[len(n):]
				stripped = true
				break
			}
		}
		if !stripped {
			break
		}
	}
	return p
}

// collectProjectHints scans the workspace root for known build /
// run manifest files and produces one-line summaries the triage LLM
// uses to map "run it" / "build it" / "test it" intents to concrete
// commands. Cheap (5-10 stat calls + a few small reads); short-circuits
// silently on any error so a degraded workspace doesn't block triage.
//
// We keep the list intentionally tight — three or four hints is
// enough for the prompt; piling in every manifest in a multi-package
// monorepo would bloat the triage call for no gain.
func collectProjectHints(workspace string) []string {
	if workspace == "" {
		return nil
	}
	var hints []string
	// package.json — Node/Bun projects. Pull the "scripts" keys
	// (just the keys, not the values) so the LLM knows which verbs
	// are wired without us having to ship the whole package.json.
	if data, err := os.ReadFile(filepath.Join(workspace, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if err := json.Unmarshal(data, &pkg); err == nil && len(pkg.Scripts) > 0 {
			keys := make([]string, 0, len(pkg.Scripts))
			for k := range pkg.Scripts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			cap := 8
			if len(keys) > cap {
				keys = keys[:cap]
			}
			hints = append(hints, "package.json: scripts "+strings.Join(keys, ", "))
		}
	}
	// Cargo.toml — Rust. Don't parse the toml; just note its presence.
	if _, err := os.Stat(filepath.Join(workspace, "Cargo.toml")); err == nil {
		hints = append(hints, "Cargo.toml present (Rust)")
	}
	// go.mod — Go.
	if _, err := os.Stat(filepath.Join(workspace, "go.mod")); err == nil {
		hints = append(hints, "go.mod present (Go)")
	}
	// Makefile — pull target names from "^name:" lines so the LLM
	// can map "test it" / "build it" / "install it" verbs directly
	// when the project's convention lives in make rules.
	if data, err := os.ReadFile(filepath.Join(workspace, "Makefile")); err == nil {
		var targets []string
		for _, line := range strings.Split(string(data), "\n") {
			if len(line) == 0 || line[0] == '\t' || line[0] == '#' || line[0] == ' ' {
				continue
			}
			if i := strings.Index(line, ":"); i > 0 {
				name := strings.TrimSpace(line[:i])
				// Skip variable assignments (FOO := bar) and
				// pattern rules (%.o).
				if strings.ContainsAny(name, " =$%") {
					continue
				}
				targets = append(targets, name)
				if len(targets) >= 8 {
					break
				}
			}
		}
		if len(targets) > 0 {
			hints = append(hints, "Makefile: targets "+strings.Join(targets, ", "))
		}
	}
	// pyproject.toml — Python.
	if _, err := os.Stat(filepath.Join(workspace, "pyproject.toml")); err == nil {
		hints = append(hints, "pyproject.toml present (Python)")
	}
	return hints
}

// formatTokens renders a one-line trailer for an agent run's token
// usage with an estimated dollar cost. Cache create vs cache read
// are shown separately because they bill at very different rates
// (1.25× input for create, 0.10× for read on Sonnet 4.6) — lumping
// them hides the actual cost driver.
//
// Format with caching:
//
//	· 30 fresh / 8k out · cache: 5k new, 95k read (95% hit) · ~$0.15
//
// Format without caching:
//
//	· 5k in / 200 out · ~$0.02
//
// The dollar estimate uses Sonnet 4.6 prices (Anthropic's published
// rates as of late 2026): $3/M input, $15/M output, $3.75/M cache
// create, $0.30/M cache read. Other models would price differently
// — the trailer is a rough sanity check, not an invoice.
func formatTokens(in, out, cached int) string {
	return formatTokensSplit(in, out, 0, cached)
}

// formatTokensSplit is the cache-aware variant. The cache
// segment is adaptive:
//
//   ≥70% reused → quiet:  "cache: 87% reused"
//   30-70%      → quiet:  "cache: 52% reused"
//   <30%        → loud:   "⚠ cache: only 9% reused — mostly writing fresh ($0.61 above cached baseline)"
//
// Rationale: when the cache is healthy, a one-line headline is
// what users want — they don't need to stare at "162k new, 16k
// read" every turn. When it's broken (the May-3 bug pattern),
// the verbose, plain-English diagnostic is exactly what they
// need to notice and act on. The "above cached baseline" dollar
// figure makes the cost of the broken cache concrete instead of
// asking the user to do mental arithmetic.
func formatTokensSplit(in, out, create, read int) string {
	cached := create + read

	if cached <= 0 {
		// Per-turn dollar amounts dropped from the trailer per
		// the May-5 UX call: cost-per-turn is noise during work,
		// and it created the implicit pressure of "watch the
		// meter" that distracts from coding. Token counts and
		// cache-hit rate stay because they're operational
		// signals (am I burning context unnecessarily); dollars
		// were just decorative anxiety. Cap-related dollar
		// displays still show — those are the gate, not noise.
		if note := providerTrailerNote(); note != "" {
			return fmt.Sprintf("· %s in / %s out %s",
				humanCount(in), humanCount(out), note)
		}
		return fmt.Sprintf("· %s in / %s out",
			humanCount(in), humanCount(out))
	}

	return fmt.Sprintf("· %s fresh / %s out · %s",
		humanCount(in), humanCount(out),
		formatCacheBand(in, create, read))
}

// formatCacheBand renders the adaptive cache segment. Reuse rate
// is computed against (fresh + create + read) — fresh and create
// both count as "not reused" because both bill at ~full input
// rate; only `read` is the cheap path. So the rate answers "of
// every input-equivalent token I sent, how many were near-free?"
//
// The "above cached baseline" figure in the warning band is the
// premium we paid for cache writes that didn't pay off as reads:
// (write_rate - read_rate) × create_tokens. Concretely on Sonnet
// 4.6 that's $3.45/M × create_tokens. If the cache had been
// readable, those same tokens would have billed at $0.30/M
// instead of $3.75/M — the user would have saved ~$3.45/M.
func formatCacheBand(fresh, create, read int) string {
	denom := fresh + create + read
	if denom == 0 {
		return "cache: idle"
	}
	pct := (read * 100) / denom

	// Suppress the loud warning when caching simply isn't being
	// reported. 2026-05-24 trace: a kai-desktop run on
	// deepseek-ai/DeepSeek-V4-Pro showed "cache: 19% reused" in
	// the trailer despite the model's underlying provider not
	// emitting cache_read_input_tokens at all on the first turns
	// of a fresh session. The warning was real (low number) but
	// the cause was provider-side reporting, not a prefix-mutation
	// bug, and the warning misdirected the user toward looking
	// for a non-existent prompt bug.
	//
	// When create == 0 AND read == 0, the provider reported no
	// cache activity. That's either a cold start (turn 0 of a
	// fresh session — expected) or a provider that doesn't
	// emit cache metrics. Either way the warning is misleading;
	// downgrade to a neutral note.
	if create == 0 && read == 0 {
		return "cache: not reported by provider"
	}

	// Threshold note: 70/30 are starting points. If real-world
	// usage shows them flapping (e.g. a turn that should have
	// read mostly hits 28%), bump the warning floor up.
	switch {
	case pct >= 70:
		return fmt.Sprintf("cache: %d%% reused", pct)
	case pct >= 30:
		return fmt.Sprintf("cache: %d%% reused", pct)
	default:
		// Tokens, not dollars: we still want the user to know
		// the cache is broken (the headline signal), but the
		// "$X above cached baseline" gloss was money-display
		// the May-5 cleanup wanted gone. Token volume conveys
		// the same diagnostic — "162k tokens written fresh
		// instead of cached" tells them something concrete is
		// going wrong without putting a dollar number on it.
		return fmt.Sprintf("⚠ cache: only %d%% reused — mostly writing fresh (%s tokens billed at write rate)",
			pct, humanCount(create))
	}
}

// estimateCost returns the dollar cost of a turn at Sonnet 4.6
// rates. Approximate — Anthropic adjusts prices and we don't
// distinguish models — but accurate to within ~20% which is the
// useful resolution for "is this run expensive?"
func estimateCost(in, out, create, read int) float64 {
	const (
		inputPerM    = 3.0  // $/M tokens
		outputPerM   = 15.0
		createPerM   = 3.75
		readPerM     = 0.30
	)
	cost := float64(in)*inputPerM/1_000_000 +
		float64(out)*outputPerM/1_000_000 +
		float64(create)*createPerM/1_000_000 +
		float64(read)*readPerM/1_000_000
	return cost
}

// formatCost renders the estimate. Sub-cent costs read as "<$0.01"
// because rounding to "$0.00" reads as "free" which is misleading
// on a per-turn breakdown.
func formatCost(cost float64) string {
	if cost < 0.01 {
		return "<$0.01"
	}
	if cost < 1 {
		return fmt.Sprintf("~$%.2f", cost)
	}
	return fmt.Sprintf("~$%.2f", cost)
}

// formatRunSummary renders the apples-to-apples summary line
// designed to match Claude Code's status display:
//
//	(14m 15s · ↓ 46.0k tokens)
//
// Wall-clock elapsed since the user's last submit, plus
// cumulative output tokens received from the model across this
// run. Same format Claude Code shows above its own status line
// so a user comparing kai vs Claude Code on equivalent work
// can read off matching numbers without doing arithmetic.
//
// Token semantics: OUTPUT tokens specifically (model→client),
// matching Claude Code's "↓ X tokens" convention. Including
// input here would produce a different number than what users
// see in Claude Code and defeat the comparison.
//
// Empty output when no tokens have arrived yet (start of run,
// pure tool-use turn) — printing "(0s · ↓ 0 tokens)" would just
// be visual noise.
//
// firstResponseAt, when non-zero, is the moment the first provider
// streaming-phase event arrived this turn. The delta start →
// firstResponseAt is the time-to-first-byte the user actually felt
// (i.e. how long the spinner sat there before tokens started
// appearing). Surface as a third segment so the user can read
// "how long until I saw something" vs "total turn time" at a
// glance.
func formatRunSummary(start, firstResponseAt time.Time, outputTokens int) string {
	if outputTokens <= 0 {
		return ""
	}
	elapsed := time.Since(start)
	if start.IsZero() {
		elapsed = 0
	}
	out := fmt.Sprintf("(%s · ↓ %s tokens",
		formatElapsed(elapsed), humanTokens(outputTokens))
	if !firstResponseAt.IsZero() && !start.IsZero() {
		ttfb := firstResponseAt.Sub(start)
		if ttfb > 0 {
			out += fmt.Sprintf(" · first-byte %s", formatElapsed(ttfb))
		}
	}
	return out + ")"
}

// formatElapsed renders a duration the way Claude Code does:
// 14m 15s, 47s, 2h 3m. Drops zero leading units so a 9-second
// turn doesn't read "0h 0m 9s".
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		// Sub-second turns shouldn't render as "0s" — that
		// reads as "no time at all" when in fact we made an API
		// round-trip. Floor to 1 second.
		d = time.Second
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// humanTokens renders Claude-Code-style "46.0k" (one decimal,
// always 'k' suffix above 1000). Different from humanCount which
// switches between "1.2k", "12k", "1.2M" depending on
// magnitude — this stays consistent so the apples-to-apples
// comparison reads cleanly side-by-side.
func humanTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// formatSessionTotal appends a session-progress clause to the
// per-turn trailer. Money was removed in the May-5 cleanup;
// turn count remains because it's a useful "how deep am I in
// this conversation" signal that doesn't carry the meter-
// watching pressure dollars do.
//
// Cost tracking under the hood (REPL.sessionCostUSD) stays —
// it powers the KAI_MAX_SESSION_COST_USD self-imposed cap.
// We just don't render it.
func formatSessionTotal(total float64, turns int) string {
	if turns <= 0 {
		return ""
	}
	suffix := "s"
	if turns == 1 {
		suffix = ""
	}
	return fmt.Sprintf(" · %d turn%s", turns, suffix)
}

// providerTrailerNote returns a short human-facing annotation
// matching the configured provider, or "" when the default
// (kailab/anthropic) is in use and no annotation is needed. We
// read KAI_PROVIDER directly because the trailer is a leaf-level
// renderer and threading the kind through the message types
// would be a much bigger change for a one-line UI signal.
func providerTrailerNote() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAI_PROVIDER"))) {
	case "openai":
		return "(no cache support)"
	default:
		return ""
	}
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// summarizeToolCall renders a one-line, human-readable label for a
// tool dispatch, used in the inline activity stream. The agent's
// inputJSON is structured per-tool — we pluck the most informative
// field (file_path for file tools, command for bash, the kai_* tools'
// target args) so the user sees "→ write package.json" instead of
// "→ write {...}". Falls back to bare tool name on parse failure.
func summarizeToolCall(name, inputJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &args); err != nil {
		return "→ " + name
	}
	pluck := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	// pluckInt reads numeric args. JSON unmarshals numbers into
	// float64, so cast through float64 first. Used by the view
	// formatter to surface offset/limit as a "[start-end]" range.
	pluckInt := func(k string) int {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	// Trim long arg renders so a fat regex or huge prompt
	// doesn't blow out a single trace line. Same cap bash
	// already used (60) for visual consistency.
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 60 {
			return s[:57] + "..."
		}
		return s
	}
	// Render a primary arg + optional secondary modifiers
	// (path, glob, regex flag) so the user can read the actual
	// query, not just the tool name. Quoted form on free-text
	// args makes word boundaries unambiguous.
	switch name {
	case "view", "write", "edit":
		if p := pluck("file_path"); p != "" {
			// Surface offset/limit for view so a paginated read
			// (e.g. view foo.go offset=200, limit=100) doesn't
			// LOOK like a duplicate of an earlier `view foo.go`
			// in the tool log. Without this, the user reading
			// the log sees `view tui.go` three times and assumes
			// the dedupe cache is broken — when actually each
			// call has different offset/limit and is correctly
			// fetching a different slice. write/edit don't have
			// offset/limit so they fall through unchanged.
			if name == "view" {
				offset := pluckInt("offset")
				limit := pluckInt("limit")
				if offset > 0 || limit > 0 {
					start := offset + 1
					if start < 1 {
						start = 1
					}
					end := offset + limit
					if limit <= 0 {
						return fmt.Sprintf("→ view %s [%d-]", p, start)
					}
					return fmt.Sprintf("→ view %s [%d-%d]", p, start, end)
				}
			}
			return "→ " + name + " " + p
		}
	case "bash":
		if c := pluck("command"); c != "" {
			return "→ bash: " + clip(c)
		}
	case "kai_grep":
		q := pluck("query")
		if q == "" {
			return "→ " + name
		}
		out := "→ kai_grep \"" + clip(q) + "\""
		if p := pluck("path"); p != "" {
			out += " in " + p
		}
		if g := pluck("glob"); g != "" {
			out += " (" + g + ")"
		}
		if v, ok := args["regex"].(bool); ok && v {
			out += " (regex)"
		}
		return out
	case "kai_files":
		if p := pluck("pattern"); p != "" {
			return "→ kai_files " + p
		}
	case "kai_tree":
		if p := pluck("path"); p != "" {
			return "→ kai_tree " + p
		}
		return "→ kai_tree ."
	case "kai_symbols":
		// Tool accepts either "file" or "path" — try both.
		if p := pluck("file"); p != "" {
			return "→ kai_symbols " + p
		}
		if p := pluck("path"); p != "" {
			return "→ kai_symbols " + p
		}
	case "kai_callers", "kai_dependents", "kai_context":
		if p := pluck("file_path"); p != "" {
			return "→ " + name + " " + p
		}
		if p := pluck("symbol"); p != "" {
			return "→ " + name + " " + p
		}
		if p := pluck("file"); p != "" {
			return "→ " + name + " " + p
		}
	case "kai_impact":
		if p := pluck("file"); p != "" {
			return "→ kai_impact " + p
		}
		if p := pluck("symbol"); p != "" {
			return "→ kai_impact " + p
		}
	}
	return "→ " + name
}

// PlannerServices is the engine handle the REPL needs to operate the
// natural-language path. The TUI parent constructs this once at
// startup and hands it in via NewREPL. A nil PlannerServices keeps
// the REPL in shell-out-only mode (every input goes to a cobra
// subprocess), which is what tests want.
type PlannerServices struct {
	DB             *graph.DB
	LLM            planner.Completer
	GateConfig     safetygate.Config
	PlannerCfg     planner.Config
	OrchestratorCfg orchestrator.Config
	PromptCtx      agentprompt.Context
	MainRepo       string
	KaiDir         string

	// Projects, when set, advertises a multi-root workspace to the
	// chat agent: file tools route per-path, prompts mention every
	// root by name. Single-root callers may leave this nil — the
	// agent runner synthesizes a single-project Set from MainRepo.
	Projects *projects.Set

	// ChatActivityCh, when non-nil, receives live tool-call and
	// file-change events from the chat-fallback agent so the REPL
	// can render them inline as the agent works. Sends are
	// non-blocking — a full channel drops events rather than
	// stalling the agent loop.
	ChatActivityCh chan<- ChatActivityEvent

	// HostProcEventCh, when non-nil, receives lifecycle events
	// from the managed dev-server process (see host_proc.go).
	// Same non-blocking pattern as ChatActivityCh — a slow TUI
	// drops events rather than stalling the scanner.
	HostProcEventCh chan HostProcEvent

	// managedProcMu / managedProc back the single-slot managed-
	// process state. host_proc.go manipulates this via the
	// SetManagedProc / SwapManagedProc / ManagedProc accessors so
	// the storage is mutex-guarded against the watcher goroutine.
	managedProcMu sync.Mutex
	managedProc   *ManagedProcess

	// cancelMu / currentCancel back the cooperative-cancel path the
	// REPL exposes via Esc while a run is in flight. runPlan stores
	// the cancel func before the agent fires; CancelCurrent() (called
	// from the REPL key handler) trips it so the provider's
	// in-flight HTTP/SSE call ctx-aborts and the agent goroutine
	// returns with a context.Canceled error. The error path is the
	// same as any other run failure — the user just sees a "cancelled"
	// outcome instead of a result.
	cancelMu      sync.Mutex
	currentCancel context.CancelFunc

	// Version is the kai CLI version, used in the startup banner.
	// Empty falls back to "dev".
	Version string

	// Role models. kai routes each kind of LLM work to a model
	// suited to it (see internal/config Default()):
	//   - ClassifierModel decides chat-vs-code for each turn.
	//   - PlannerModel runs the planner agent (reasoning).
	//   - ChatModel runs the conversational chat agent.
	//   - ReviewModel audits a held change in gate review.
	//   - the code-writing agents (and the gate-review fix agent)
	//     use OrchestratorCfg.AgentModel.
	// Resolved once at startup by buildPlannerServices from config
	// + KAI_*_MODEL env overrides. Empty falls back to the relevant
	// config default at the call site.
	ClassifierModel        string
	PlannerModel           string
	PlannerFinalizeModel   string // optional fast writer for the single-turn JSON emission; empty falls back to PlannerModel
	ChatModel              string
	ReviewModel            string
	// CriticModel runs the satisfaction-gate critic. Default is a
	// DIFFERENT model from ChatModel so the critic's training prior
	// doesn't share the chat agent's biases — same-model self-critique
	// rationalizes its own patterns. Falls back to ChatModel if empty.
	// Resolution: KAI_CRITIC_MODEL env > config.Critic.Model >
	// hardcoded default (moonshotai/Kimi-K2.6). 2026-05-28 dogfood
	// pinned this: same-model critic PASS'd a generic-patterns answer
	// because the model defended its own pattern in the critique step.
	CriticModel            string

	// HandsOff selects the hands-off autonomy level: the REPL
	// auto-confirms the plan menu, auto-accepts file/bash actions,
	// and lets the review→fix loop run unattended. The final gate
	// approve still requires the user. Resolved by cmd/kai from the
	// --auto flag, KAI_AUTO env, and config.autonomy.
	HandsOff bool

	// SharedPaths is the session-scoped allowlist of paths OUTSIDE
	// the workspace that the user has explicitly shared via the
	// /share TUI command. Passed into every agent.Run as
	// Options.SharedPaths so the read-only file tools can resolve
	// them. Mutated by /share; reset only on TUI exit.
	SharedPaths []string

	// Fixxy, when non-nil, is the secret fixxy-upper worker
	// (see internal/tui/fixxy). REPL forwards classified
	// errors (mode 1+), feedback-phrase triggers (mode 2+),
	// and per-turn reviews (mode 3) into Fixxy.Trigger().
	// Nil when --fixxy-upper wasn't passed; all triggers
	// safely no-op in that case via fixxy.Worker's nil guard.
	Fixxy     *fixxy.Worker
	FixxyMode fixxy.Mode
}

// PlanReadyMsg is emitted when the LLM returns a parseable plan.
type PlanReadyMsg struct {
	Request string
	Plan    *planner.WorkPlan
	Err     error
	// AutoRun signals the triage quick-track (execute immediately,
	// no confirm). The classifier line does not set it — the live
	// "skip the confirm" signal is Plan.Trivial — but the field is
	// kept so the merged triage code paths compile.
	AutoRun bool
	// ChatReply, when non-empty, indicates the request was too vague
	// to plan and the runner fell back to a conversational answer.
	// REPL renders ChatReply inline as assistant prose instead of
	// surfacing ErrTooVague as an error.
	ChatReply string
	// HostCommand, when non-empty, is the literal shell command kai
	// proposes to run on the user's host (NOT inside a spawn) after
	// approval. Set by the host-task fast path (triage.TrackHost)
	// for the kai-specific recipes (install kai, register MCP, kai
	// login). The REPL renders ChatReply as the explanation, then a
	// boxed view of HostCommand, then a [y]es/[n]o approval prompt.
	// On approve, kai exec's the command via os/exec in the user's
	// shell context with kai's own permissions — same as if the
	// user typed it themselves.
	HostCommand string
	// SessionID is the persisted-session id for the SHARED
	// conversation. Both chat fallback and planner agent runs
	// resume this same session, so cross-turn follow-ups (a chat
	// reply followed by "fix it" routed to planner) inherit the
	// prior transcript. The REPL stickies it across turns within
	// a single TUI run.
	SessionID string
	// ChatTokensIn / ChatTokensOut are the per-turn usage counts the
	// chat-fallback agent reported. Surfaced as a dim trailer line
	// after the reply so the user can see what each turn cost. Zero
	// when the response came from the planner path.
	ChatTokensIn  int
	ChatTokensOut int
	// ChatTokensCached is the run's accumulated cache_read +
	// cache_creation tokens — surfaced in the trailer so users
	// can see prompt caching working. Zero when caching wasn't
	// used or the response came from the planner path.
	ChatTokensCached int

	// PlannerTokensIn / PlannerTokensOut / PlannerTokensCached are
	// the planner agent run's usage. Surfaced as a dim trailer line
	// like the chat path so the user can see what each plan cost.
	// Zero when the response came from the chat fallback.
	PlannerTokensIn          int
	PlannerTokensOut         int
	PlannerTokensCached      int
	PlannerTokensCacheCreate int
	PlannerTokensCacheRead   int
}

// ExecuteDoneMsg is emitted when the orchestrator finishes a plan.
type ExecuteDoneMsg struct {
	Result *orchestrator.Result
	Err    error
}

// CriticReadyMsg is emitted by runCritic after the satisfaction-gate
// critic has evaluated the run. The verdict shapes the trailer (dim
// pass note vs. visible failure + retry offer). When Pass is false,
// REPL stashes the retry hint so a one-key 'r' press dispatches a
// new run with the critique appended to the next prompt.
//
// The kai-desktop dogfood (2026-05-24) showed that the same model
// that produced a confabulated CSS file was sharp and accurate when
// asked to grade itself in a follow-up turn. The critic formalizes
// that grading step: every non-trivial run gets one critic call
// before being declared done.
type CriticReadyMsg struct {
	OriginalRequest string
	Pass            bool
	Critique        string
	RetryHint       string
	Err             error
}

// runPlan emits a tea.Cmd that calls planner.Plan asynchronously so
// the UI stays responsive during the LLM round-trip. The result
// arrives as a PlanReadyMsg the REPL handles in Update.
//
// When the planner returns ErrTooVague (greeting, chitchat, vague
// "make it better"), runPlan falls back to a one-shot Chat reply
// instead of surfacing the error. The REPL renders ChatReply as
// inline prose so the user can keep the conversation flowing.
// CancelCurrent trips the cancel func a running planner/agent has
// stashed on the services struct. No-op when nothing's running.
// Safe to call from any goroutine — the mutex guards the field
// against concurrent assignment from a freshly-starting run.
// SetManagedProc stores mp in the single managed-process slot.
// Caller is responsible for having stopped any prior process
// (StartManagedProcess does this via StopManagedProcess).
func (s *PlannerServices) SetManagedProc(mp *ManagedProcess) {
	if s == nil {
		return
	}
	s.managedProcMu.Lock()
	defer s.managedProcMu.Unlock()
	s.managedProc = mp
}

// SwapManagedProc atomically replaces the managed-process slot
// and returns the previous occupant. Used by StopManagedProcess
// so the kill path can run on the prior process without holding
// the lock for the full SIGTERM/grace/SIGKILL sequence.
func (s *PlannerServices) SwapManagedProc(mp *ManagedProcess) *ManagedProcess {
	if s == nil {
		return nil
	}
	s.managedProcMu.Lock()
	defer s.managedProcMu.Unlock()
	prev := s.managedProc
	s.managedProc = mp
	return prev
}

// ManagedProc returns the current managed-process slot (may be nil).
// Read-only accessor; use SetManagedProc / SwapManagedProc to mutate.
func (s *PlannerServices) ManagedProc() *ManagedProcess {
	if s == nil {
		return nil
	}
	s.managedProcMu.Lock()
	defer s.managedProcMu.Unlock()
	return s.managedProc
}

func (s *PlannerServices) CancelCurrent() {
	if s == nil {
		return
	}
	s.cancelMu.Lock()
	cancel := s.currentCancel
	s.currentCancel = nil
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// armCancel installs a fresh cancellable context for the next run
// and stores the cancel func on the services struct. The cleanup
// func returned MUST be deferred by the caller — it both clears
// the stored cancel (so a stale Esc later in the session can't
// interrupt an unrelated future run) AND calls cancel itself
// (idempotent; satisfies linters that complain about leaked
// CancelFuncs even when the work has finished).
func (s *PlannerServices) armCancel(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	s.cancelMu.Lock()
	prev := s.currentCancel
	s.currentCancel = cancel
	s.cancelMu.Unlock()
	if prev != nil {
		// Defensive: a stale cancel from a prior run shouldn't
		// outlive the new one. Tripping it is a no-op for the
		// finished run.
		prev()
	}
	return ctx, func() {
		s.cancelMu.Lock()
		if s.currentCancel != nil {
			// Only clear if it's still ours — a re-entrant arm
			// from a quickly-following second run would have
			// replaced the field already.
			anyCancel := s.currentCancel
			_ = anyCancel
		}
		s.currentCancel = nil
		s.cancelMu.Unlock()
		cancel()
	}
}

func runPlan(s *PlannerServices, request, sessionID string, forced agent.Mode, pendingActionText string) tea.Cmd {
	if s == nil {
		return func() tea.Msg {
			return PlanReadyMsg{Request: request, Err: fmt.Errorf("planner not configured")}
		}
	}
	return func() tea.Msg {
		ctx, done := s.armCancel(context.Background())
		defer done()

		// Pending-action confirmation short-circuit. REPL already
		// established this is a short-affirmative landing on top of
		// a pending action (chat agent's prior turn ended with the
		// "Reply 'yes' and I'll apply it." trailer). Skip every
		// other routing rule — including triage, sticky-chat, the
		// affirmative branches below — and call the chat agent in
		// coding mode with the prior session AND an explicit
		// preamble that frames the model's own prior proposal as
		// the work to execute. P0 fix from the 2026-05-26
		// confirmation-loop spec: prior shape inherited the session
		// but left the offer implicit in history, so the model
		// failed to bind "yes" to the proposal and bounced it as
		// "what would you like me to do?".
		if pendingActionText != "" {
			wrapped := wrapPendingActionPrompt(&pendingAction{text: pendingActionText}, request)
			reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(
				ctx, s, wrapped, sessionID, agent.ModeCoding)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newChatID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		}

		// Trivial-action fast path. Before paying for a triage LLM
		// round-trip, check whether the request is a bare action verb
		// ("run it", "start", "build it", "test", "launch this")
		// that lines up cleanly with a manifest in the workspace.
		// If so, synthesize the host-task locally and skip both
		// triage and planner — sub-100ms to first user-visible
		// output instead of the 3-5s triage round-trip.
		//
		// Narrow by design: only fires on short prompts that match
		// one of a handful of action verbs. Anything more complex
		// falls through to the LLM triage path. The risk of mis-
		// routing is bounded — even if we propose the wrong command,
		// the user approves with a keystroke before it runs.
		if forced != agent.ModePlanning {
			if cmd, label := trivialActionFastPath(request, s.MainRepo); cmd != "" {
				return PlanReadyMsg{
					Request:     request,
					ChatReply:   fmt.Sprintf("Trivial action recognized (%s) — proposing host command without triage round-trip.", label),
					HostCommand: cmd,
					SessionID:   sessionID,
				}
			}
		}

		// Triage short-circuit. Every non-/plan request gets one cheap
		// classification LLM call before the planner spins up. The
		// triage LLM routes to TrackHost (install / PATH / sudo / MCP
		// register / login — kai runs the command after user approval),
		// TrackAnswer (a pure question — reply inline), TrackClarify
		// (ambiguous — ask the user a follow-up), or TrackPlan /
		// TrackQuick (fall through to the existing planner flow).
		//
		// /plan forces TrackPlan inside Classify itself — no LLM call
		// burned. Transport / parse errors also resolve to TrackPlan,
		// so a broken triage path degrades to today's behavior
		// (planner runs as normal) rather than blocking the user.
		tri := triageRequest(ctx, s, request, sessionID, forced)
		switch tri.Track {
		case triage.TrackHost:
			return PlanReadyMsg{
				Request:     request,
				ChatReply:   tri.Answer,
				HostCommand: tri.HostCommand,
				SessionID:   sessionID,
			}
		case triage.TrackAnswer:
			// A pure pleasantry ("hi", "thanks") can take triage's
			// instant inline answer — no point spinning up an agent to
			// say "you're welcome".
			if isGreeting(request) && tri.Answer != "" {
				return PlanReadyMsg{
					Request:   request,
					ChatReply: tri.Answer,
					SessionID: sessionID,
				}
			}
			// Everything else routes to the in-process agent (runChatAgent)
			// instead of returning triage's inline answer. This is the
			// fix for "code can't maintain the conversation" (2026-05-29):
			// the inline triage answer had no session — it ran from the
			// triage prompt + 2 recent turns, with no tools and nothing
			// persisted, so the next turn ("what did you just say?") had
			// no history to resume. runChatAgent resumes AND appends the
			// session, and gives the answer real codebase tools + the
			// search guard. Costs one extra round-trip; buys grounded,
			// memory-preserving answers.
			reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(
				ctx, s, request, sessionID, agent.ModeCoding)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newChatID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		case triage.TrackClarify:
			if tri.Question != "" {
				return PlanReadyMsg{
					Request:   request,
					ChatReply: tri.Question,
					SessionID: sessionID,
				}
			}
			// Same fallback as TrackAnswer for the empty-question case.
		}
		// TrackQuick and TrackPlan fall through to the existing
		// chat/planner flow below. Both still benefit from the
		// triage call having ruled out the host/answer/clarify
		// branches first.

		// /chat short-circuit — STICKY. When the user explicitly
		// forces conversation mode via /chat, they want to TALK to
		// the chat agent — not have the planner explore and emit a
		// plan labeled "conversation." This applies for the
		// CURRENT turn (forced flag) AND every subsequent turn
		// until the user explicitly switches mode with /code, /plan,
		// /debug, or /review. The stickiness lives in the persisted
		// prev_mode column on agent_sessions: chat sets it to
		// "Conversation" on its first turn, and we consult that
		// state here on every subsequent turn.
		//
		// 2026-05-15 dogfood: user typed /chat then a long meta
		// question (route to chat ✓), then "write a CHANGELOG.md
		// entry" (no forced flag → routed back to planner ✗,
		// emitting a plan card on what should have been a direct
		// edit). Sticky mode keeps the user in chat across turns.
		stickyConversation := false
		if forced == agent.ModeUnknown && s.OrchestratorCfg.AgentSessionStore != nil && sessionID != "" {
			if raw, err := session.LookupMode(s.OrchestratorCfg.AgentSessionStore, sessionID); err == nil {
				if agent.ParseMode(raw) == agent.ModeConversation {
					stickyConversation = true
				}
			}
		}
		if forced == agent.ModeConversation || stickyConversation {
			reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(
				ctx, s, request, sessionID, agent.ModeConversation)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newChatID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		}

		// Permission-handshake escalation. The chat agent's prior
		// turn offered to do work ("want me to apply these?"); the
		// REPL saw a bare "yes" and forced this turn to coding mode.
		// Route it straight to the chat agent WITH edit tools
		// enabled (ModeCoding) — it inherits the prior session's
		// history, so it knows exactly what "yes" consented to and
		// just does it. Sending it to the planner would restart a
		// cold planning loop that has no idea what was offered.
		if forced == agent.ModeCoding && isShortAffirmative(request) {
			reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(
				ctx, s, request, sessionID, agent.ModeCoding)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newChatID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		}

		// Follow-up short-circuit. Single-word affirmatives ("yes",
		// "go", "a", "1") and tiny continuations ("go ahead", "do
		// it", "more") are unambiguously responses to the prior
		// turn's question or A/B choice — never standalone plan
		// requests. Routing them to the planner restarts a cold
		// planning loop that has no idea what "yes" refers to and
		// emits a confusing "request too vague" notice instead of
		// continuing the conversation. Sending them to the chat
		// agent (which inherits the prior session's history)
		// preserves context across turns.
		//
		// 2026-05-14 dogfood: chat agent found a bug, asked
		// "want me to fix it?", user replied "yes" → planner ran
		// cold and demanded clarification across 4 subsequent
		// turns. This short-circuit catches that pattern.
		if forced == agent.ModeUnknown && isShortAffirmative(request) {
			reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(
				ctx, s, request, sessionID, agent.ModeConversation)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newChatID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		}

		// Greeting / chitchat short-circuit. "hi", "hello there",
		// "thanks", etc. should NEVER reach the planner — that
		// burns ~1k tokens fresh just to produce a "no work to
		// plan" headline, which then has to route to chat anyway
		// via the vague-refusal fallback. Skipping the planner
		// entirely is faster, cheaper, and reads correctly
		// (conversational reply, no warning UI).
		//
		// Forced modes (/code, /debug) bypass this so the user can
		// always force a planner run if they want one.
		if forced == agent.ModeUnknown && isGreeting(request) {
			reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(
				ctx, s, request, sessionID, agent.ModeConversation)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newChatID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		}

		// Classifier short-circuit: a strong model decides whether
		// this turn is a conversation or a code change. A "chat"
		// verdict routes straight to the chat agent and skips the
		// planner — the planner would only produce ErrTooVague on a
		// question, then route to chat anyway with a confusing
		// "request too vague" surface in between. The classifier
		// runs only here, AFTER the /chat-sticky, short-affirmative,
		// and greeting pre-filters above, so trivial input never
		// pays for the round-trip; routeToChat falls back to the
		// isQuestion heuristic if the classifier call fails.
		if forced == agent.ModeUnknown && routeToChat(ctx, s, request) {
			reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(
				ctx, s, request, sessionID, agent.ModeConversation)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newChatID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		}

		// Agent-loop planner: drives the model through the full
		// read-only tool set so it can verify file paths, check
		// what's already implemented, and refer back to prior
		// planning turns via the persisted session. Replaces the
		// legacy one-shot planner.Plan() that hallucinated paths
		// and lost context across turns.
		//
		// Falls back to the legacy single-shot Plan() only when
		// the agent path can't be constructed (no provider, no
		// projects.Set) — primarily a safety net for tests that
		// bypass the TUI startup wiring.
		if pa := buildPlannerAgent(s); pa != nil {
			res, err := pa.Run(ctx, request, sessionID)
			if errors.Is(err, planner.ErrTooVague) {
				// Force ModeConversation when the chat fallback
				// fires from a planner-too-vague request. The
				// default ModeCoding tells the model to emit
				// STEPS:/STEP_DONE: markers, which the TUI
				// renders as a checklist of "completed" work —
				// misleading here because the planner has no
				// write tools and isn't actually doing work, just
				// answering. ModeConversation skips the marker
				// protocol so the reply reads as prose.
				//
				// Slash overrides still win (forced != Unknown)
				// so /code or /debug bypass this default.
				chatMode := forced
				if chatMode == agent.ModeUnknown {
					chatMode = agent.ModeConversation
				}
				reply, newChatID, tIn, tOut, tCached, chatErr := runChatAgent(ctx, s, request, sessionID, chatMode)
				if chatErr != nil {
					return PlanReadyMsg{Request: request, Err: chatErr}
				}
				out := PlanReadyMsg{
					Request:          request,
					ChatReply:        reply,
					SessionID:        newChatID,
					ChatTokensIn:     tIn,
					ChatTokensOut:    tOut,
					ChatTokensCached: tCached,
				}
				// Carry the planner's session id forward even on
				// vague-fallback so the next turn can keep
				// referencing the prior planner exchange. Also
				// carry the planner's token spend — the planner
				// agent burned tokens before deciding the
				// request was too vague, and hiding that cost
				// would under-report the trailer.
				if res != nil {
					out.SessionID = res.SessionID
					out.PlannerTokensIn = res.TokensIn
					out.PlannerTokensOut = res.TokensOut
					out.PlannerTokensCached = res.TokensCached
					out.PlannerTokensCacheCreate = res.TokensCacheCreate
					out.PlannerTokensCacheRead = res.TokensCacheRead
				}
				return out
			}
			out := PlanReadyMsg{Request: request, Err: err}
			if res != nil {
				out.Plan = res.Plan
				// Reply is set when the model produced prose
				// instead of a JSON plan (typically because its
				// exploration concluded the work was already
				// done). Surface it as a chat reply so the user
				// reads the analysis instead of an error.
				if res.Reply != "" {
					out.ChatReply = res.Reply
					out.Err = nil
				}
				out.SessionID = res.SessionID
				out.PlannerTokensIn = res.TokensIn
				out.PlannerTokensOut = res.TokensOut
				out.PlannerTokensCached = res.TokensCached
				out.PlannerTokensCacheCreate = res.TokensCacheCreate
				out.PlannerTokensCacheRead = res.TokensCacheRead
			}
			return out
		}

		// Legacy fallback: one-shot planner. Kept so existing tests
		// that don't wire a provider still exercise the parsing
		// path. New behavior should land in the agent-loop branch
		// above.
		plan, err := planner.Plan(ctx, request, s.DB, s.GateConfig, s.PlannerCfg, s.LLM)
		if errors.Is(err, planner.ErrTooVague) {
			reply, newSessionID, tIn, tOut, tCached, chatErr := runChatAgent(ctx, s, request, sessionID, forced)
			if chatErr != nil {
				return PlanReadyMsg{Request: request, Err: chatErr}
			}
			return PlanReadyMsg{
				Request:          request,
				ChatReply:        reply,
				SessionID:        newSessionID,
				ChatTokensIn:     tIn,
				ChatTokensOut:    tOut,
				ChatTokensCached: tCached,
			}
		}
		return PlanReadyMsg{Request: request, Plan: plan, Err: err}
	}
}

// buildPlannerAgent assembles a PlannerAgent from the live REPL
// services. Returns nil when required wiring is missing (no
// provider, no Set) so the caller falls through to the legacy
// one-shot planner path. Constructed per-turn so any config
// changes (model, max-agents) take effect on the next request.
func buildPlannerAgent(s *PlannerServices) *planner.PlannerAgent {
	if s == nil || s.OrchestratorCfg.AgentProvider == nil || s.Projects == nil {
		return nil
	}
	return &planner.PlannerAgent{
		Provider: s.OrchestratorCfg.AgentProvider,
		// kai_web_search auth — same source the chat agent uses
		// (planner_dispatch.go below). Without this the tool's
		// registration guard at tools/kai.go:277 fails and the
		// planner can't look up external facts.
		KailabBaseURL: s.OrchestratorCfg.KailabBaseURL,
		KailabToken:   s.OrchestratorCfg.KailabToken,
		// The planner reasons — it does not write code — so it runs
		// on PlannerModel (QWEN by default), not the code agents'
		// model. buildPlannerServices resolves PlannerModel from
		// config + KAI_PLANNER_MODEL; the per-request Model on the
		// provider call always wins over the provider's own default,
		// so a BYOM user's KAI_*_MODEL choice is still honored.
		Model:         s.PlannerModel,
		FinalizeModel: s.PlannerFinalizeModel, // empty falls back to Model inside PlannerAgent
		Set:           s.Projects,
		GateConfig:   s.GateConfig,
		Cfg:          s.PlannerCfg,
		SessionStore: s.OrchestratorCfg.AgentSessionStore,
		// OnThinking pipes the planner's per-turn assistant
		// narration into the chat-activity channel as a
		// "thinking" event. The REPL renders the latest
		// sentence dimmed below the spinner so the user sees
		// what the planner is working on without it cluttering
		// scrollback. Non-blocking — drops events if the
		// channel is full.
		OnThinking: func(text string) {
			if s.ChatActivityCh == nil {
				return
			}
			select {
			case s.ChatActivityCh <- ChatActivityEvent{
				Kind:    "thinking",
				Summary: text,
				When:    time.Now(),
			}:
			default:
			}
		},
		// OnToolCall surfaces planner tool dispatches into the
		// chat-activity channel as dim "tool" lines. Without this,
		// a planner turn that emits no text (pure tool_use blocks)
		// renders with NOTHING visible to the user — they see the
		// spinner timer tick past 7 minutes with no signal that the
		// agent is doing work. Each dispatch becomes one dim line
		// ("→ kai_grep …" / "→ view path/file.go @0:2000") in the
		// message area as it happens.
		OnToolCall: func(name, inputJSON string) {
			if s.ChatActivityCh == nil {
				return
			}
			select {
			case s.ChatActivityCh <- ChatActivityEvent{
				Kind:    "tool",
				Summary: summarizeToolCall(name, inputJSON),
				When:    time.Now(),
			}:
			default:
			}
		},
		OnProviderState: func(state provider.RequestState) {
			if s.ChatActivityCh == nil {
				return
			}
			select {
			case s.ChatActivityCh <- ChatActivityEvent{
				Kind:    "provider_state",
				Summary: ProviderStateSummary(state),
				When:    state.When,
			}:
			default:
			}
		},
	}
}

// ProviderStateSummary builds the human-readable "what's the call
// doing right now" line the TUI renders below the spinner. Centralized
// so chat-mode and planner-mode produce identical text for the same
// state. Examples:
//
//	"sent · POST /api/v1/llm/messages (stream)"
//	"connected · HTTP 200"
//	"streaming"
//	"stream idle 12s"
//	"error · HTTP 429: rate limited"
func ProviderStateSummary(state provider.RequestState) string {
	switch state.Phase {
	case provider.PhaseStreamIdle:
		return "stream idle " + state.IdleSince.Round(time.Second).String()
	case provider.PhaseError:
		if state.Detail != "" {
			return "error · " + state.Detail
		}
		return "error"
	case provider.PhaseUpstreamSent:
		// "↑" marks the kailab → upstream-provider leg so the user
		// can tell at a glance that the local connection is fine
		// and we're waiting on api.anthropic.com (or api.openai.com).
		if state.Detail != "" {
			return "↑ sent · " + state.Detail
		}
		return "↑ sent"
	case provider.PhaseUpstreamConnected:
		if state.Detail != "" {
			return "↑ connected · " + state.Detail
		}
		return "↑ connected"
	case provider.PhaseUpstreamError:
		if state.Detail != "" {
			return "↑ error · " + state.Detail
		}
		return "↑ error"
	default:
		if state.Detail != "" {
			return string(state.Phase) + " · " + state.Detail
		}
		return string(state.Phase)
	}
}

// runChatAgent handles the chat fallback when the planner says the
// request is too vague. Instead of a one-shot text completion (which
// can't see the user's repo), we run the agent loop against the main
// repo so the model can use `view`, `bash`, edit/write, and the
// kai_* graph tools to actually answer the user. The trust boundary
// is the user's review of the reply — chat-mode runs unsandboxed in
// the working tree, same blast radius as anything the user could do
// at their own shell.
// resolveChatMode decides which mode the next chat agent run should
// use. Precedence (highest first):
//
//  1. `forced` — a slash override (/code, /debug, /review, /plan,
//     /chat) the developer just typed. Outranks everything; the
//     spec calls this out as the user's escape hatch.
//  2. DetectMode(request, prevMode) — keyword / error-signature
//     detection, with sticky/soft resolution against the previous
//     turn's persisted mode.
//
// Extracted from runChatAgent so the precedence is unit-testable
// without spinning up a real provider / agent.Run.
func resolveChatMode(store session.Store, sessionID, request string, forced agent.Mode) agent.Mode {
	if forced != agent.ModeUnknown {
		return forced
	}
	prevMode := agent.ModeUnknown
	if store != nil && sessionID != "" {
		if raw, err := session.LookupMode(store, sessionID); err == nil {
			prevMode = agent.ParseMode(raw)
		}
	}
	return agent.DetectMode(request, prevMode)
}

// systemContextSuffix is appended to the static system prompt at
// runtime. Recomputed per turn so resumed sessions get a fresh
// timestamp and accurate env info.
func systemContextSuffix(modelID string) string {
	return fmt.Sprintf(
		`Prior messages compress automatically near context limits, so the conversation isn't window-limited. System context: %s

Scope discipline: fix exactly what's asked. No extra features, refactors, cleanup around bug fixes, added configurability, or speculative abstractions. Don't add error handling for impossible scenarios — trust internal guarantees and validate only at boundaries (user input, external APIs). No feature flags or compat shims for one-time changes. Three similar lines beats premature abstraction.

Comments: default to zero. Add one only for non-obvious WHYs (hidden constraints, subtle invariants, workarounds, surprising behavior). Never explain WHAT — identifiers do that. Don't reference tasks, fixes, or callers (belongs in PR, rots in code). Don't delete existing comments unless removing their code or they're verifiably wrong.

Verify before claiming completion: run tests, execute scripts, check output. If you can't verify, state that explicitly — don't claim success.

References: GitHub/KaiContext issues/PRs as owner/repo#123 (e.g., anthropics/claude-code#100); code as file_path:line_number (e.g., internal/agent/runner.go:1521).

Answer directly when you can without tools. Lead with the action or answer, not reasoning. Skip filler and preamble. If you can say it in one sentence, don't use three. These user-facing text rules don't apply to code or tool calls.

%s`,
		time.Now().UTC().Format(time.RFC3339),
		promptenv.ComputeEnvInfo(modelID, nil),
	)
}

// chatWallClockBudget caps how long a single chat-mode run can take
// before being hard-cancelled. 5 minutes accommodates legitimate
// long-form chat work — the 2026-05-15 dogfood produced a thorough
// SPECULATIVE_DISPATCH design doc in 2m54s, well-formed and worth
// reading; 3 min would have cut that off mid-doc. 5 min still
// catches the dangling-turn loop pathology (model emitting
// "writing now…" without producing the deliverable, observed
// running for 8m02s before we added any cap) well before the user
// gives up.
//
// The runner's per-turn ctx check converts the cancellation into a
// clean error path; the user sees "chat session exceeded budget"
// instead of a frozen TUI.
const chatWallClockBudget = 5 * time.Minute

// chatWallClockBudgetReasoning is the budget for reasoning-model
// chat runs. DeepSeek-V4-Pro was observed in the 2026-05-24
// kai-desktop dogfood spending 4m52s on a SINGLE turn (472 visible
// output tokens, the rest hidden <think> reasoning). At the 5min
// baseline that left no room for a second turn before
// `context deadline exceeded` fired and the agent's held edits got
// shoved into the gate with no chance to recover. Reasoning families
// (Qwen3, o1/o3/o4, gpt-5, DeepSeek-R*/V4) need ~3x the headroom so
// multi-turn flows complete.
const chatWallClockBudgetReasoning = 15 * time.Minute

// chatWallClockBudgetFor picks the budget based on whether the
// configured chat model is a reasoning family. We can't predict
// per-turn latency before the call goes out, so the budget is
// chosen once at run start.
func chatWallClockBudgetFor(model string) time.Duration {
	if agent.IsReasoningModel(model) {
		return chatWallClockBudgetReasoning
	}
	return chatWallClockBudget
}

// chatMaxTurns caps the agent loop's turn count for chat mode.
// 50 (the default) is appropriate for coding agents that explore-
// then-edit-then-verify across many turns; for chat, the model
// either answers in 1-3 turns or it's stuck in a narration loop.
// Cap at 8 so even a worst-case "had to grep + view a few files
// before answering" run completes; anything beyond is the
// pathology, not the workflow.
const chatMaxTurns = 8

func runChatAgent(ctx context.Context, s *PlannerServices, request, sessionID string, forced agent.Mode) (text, newSessionID string, tokensIn, tokensOut, tokensCached int, err error) {
	if s.OrchestratorCfg.AgentProvider == nil {
		return "", "", 0, 0, 0, fmt.Errorf("chat: agent provider not configured (run `kai auth login`)")
	}

	// Wall-clock budget. If the run blows past the budget, ctx.Done()
	// fires and the runner's per-turn ctx check terminates the loop
	// cleanly with FinishReasonCanceled. Combined with the turn cap
	// below, gives two independent stop conditions. Budget scales by
	// model family — reasoning models burn minutes per turn on
	// hidden <think> tokens and need ~3x the headroom.
	ctx, cancelTimeout := context.WithTimeout(ctx, chatWallClockBudgetFor(s.ChatModel))
	defer cancelTimeout()
	// The system role is sent per-request via req.System (not stored
	// in persisted history), so we re-send it on every turn — both
	// fresh and resumed. Without it the resumed model has no
	// instructions and drifts toward whatever pattern the prior
	// transcript established.
	// Overview-related guidance is dead weight after turn 1 — the
	// Project overview block only injects on the first turn of a
	// fresh session (graph_context.go:90). sessionID=="" is the
	// fresh-session signal; on resume, drop the ~450 tokens.
	systemPrompt := chatSystemPrompt
	if strings.TrimSpace(sessionID) == "" {
		systemPrompt += "\n\n" + chatOverviewGuidance
	}
	// Per-turn workspace reminder. The full Project overview injects
	// only on turn 0 of a fresh session; on resumed sessions the model
	// loses track of the cwd and defaults to its kai-prior. A one-line
	// reminder every turn anchors the workspace identity so questions
	// like "what is this repo?" can't fall back to "the kai coding
	// agent repository" when the user is in some other directory.
	// The 2026-05-28 dogfood pinned this: user in /Users/.../claude/
	// asked "what is this repo?" and chat said "the kai coding agent
	// repository" — pure prior with no tool call.
	systemPrompt += "\n\n" + workspaceReminder(s)
	// Phase 2a of workspace-grounding-spec.md: on workspace turns,
	// require an OBSERVED block prefix. Single-call format with a
	// structural separator. The spec endorses two separate calls as
	// the eventual default (stronger ordering guarantee), but starts
	// with single-call as a measurement step — if OBSERVED is emitted
	// reliably with the inline-format instruction, the 2x call cost
	// of two-call mode is avoidable. If A reliability is poor, the
	// two-call refactor is the next slice.
	if classifyWorkspaceTurn(request) {
		systemPrompt += "\n\n" + workspaceGroundingGuidance
	}
	runPrompt := "System: " + systemPrompt + "\n\n" + systemContextSuffix(s.ChatModel) + "\n\n" + request

	// Live activity: pipe tool dispatches and file mutations into the
	// chat-activity channel so the REPL can render them inline. Sends
	// are non-blocking — a slow renderer drops events instead of
	// stalling the agent loop.
	emit := func(kind, summary string) {
		if s.ChatActivityCh == nil {
			return
		}
		select {
		case s.ChatActivityCh <- ChatActivityEvent{Kind: kind, Summary: summary, When: time.Now()}:
		default:
		}
	}

	// Bracket the run with start/end markers so the status bar can
	// display a live "agents: N" counter. agent_end fires regardless
	// of success/error path via defer.
	emit("agent_start", "")
	defer emit("agent_end", "")

	// Chat-agent debug log: writes <kaiDir>/chat-debug.log with
	// per-turn TURN/TEXT/TOOL entries AND a full REQUEST dump
	// before each api.Send (via OnRequest hook). The dump
	// shows whether the project overview actually reached the
	// model on each turn — answering "did kai inject context"
	// without guessing. Best-effort: nil log when KaiDir isn't
	// set (legacy callers); methods all no-op on nil.
	dbg, _ := planner.OpenChatDebugLog(s.KaiDir, request)
	defer dbg.Close()

	// Track whether the agent's tools touched the workspace so we
	// can fire `kai capture -m <request>` after the run completes.
	// Updated from the OnFileDiff and OnGateVerdict hooks below;
	// either signal is sufficient (file tools fire OnFileDiff;
	// bash-driven mutations fire OnGateVerdict via the runner's
	// post-bash diff scan).
	mutated := false
	// capturedEdits accumulates a bounded summary of file mutations
	// observed via OnFileDiff so the deferred `kai capture -m` can
	// synthesize a richer message than the raw user request. Cap at
	// 8 entries — the message itself only displays up to 6, the extra
	// 2 give us a non-truncated count for the "… and N more" tail
	// without needing a separate counter.
	const capturedEditsCap = 8
	var capturedEdits []editSummary
	capturedTotal := 0
	defer func() {
		if mutated {
			runKaiCapture(s, request, capturedEdits, capturedTotal, emit)
		}
	}()

	resolvedMode := resolveChatMode(s.OrchestratorCfg.AgentSessionStore, sessionID, request, forced)

	// Mode-route notice. When auto-routing picked a non-default mode
	// (i.e. the user didn't explicitly /code-flag the request and we
	// landed somewhere other than coding), emit a one-liner so the
	// user knows what kind of run they're getting AND how to redirect
	// next turn. Skipped when the user explicitly forced the mode —
	// they already know which mode they're in. Also skipped for
	// coding (the default — no surprise to call out).
	if forced == agent.ModeUnknown {
		resolved := agent.ResolveMode(resolvedMode)
		if resolved != agent.ModeCoding {
			emit("mode_route", fmt.Sprintf("⚙ routing to %s mode — type /code next turn to override",
				strings.ToLower(resolved.String())))
		}
	}

	// kai_diff needs the kai binary path to shell out. os.Executable
	// resolves to whatever kai binary the user is running — same
	// resolution the orchestrator uses for `kai capture`/`kai spawn`.
	// On error (rare), fall back to "kai" on PATH.
	kaiBin := "kai"
	if exe, err := os.Executable(); err == nil {
		kaiBin = exe
	}

	// kai_checkpoint needs a per-run authorship writer. We key the
	// checkpoint dir by chatSessionID when one exists (multi-turn
	// chats accumulate to the same dir; matches what `kai blame`
	// expects). Empty sessionID → skip the writer entirely; the
	// tool registers as omitted, the model gets clear "not
	// configured" if it tries to call it.
	var ckpt *authorship.CheckpointWriter
	if s.KaiDir != "" && sessionID != "" {
		ckpt = authorship.NewCheckpointWriter(s.KaiDir, sessionID)
	}

	var bashLineCount int // reset per bash call in OnToolCall; caps TUI bash output at 2 lines

	// Chat merged into code (2026-05-29: "chat mode is code mode"), so
	// the in-process agent gets the full tool set including write/edit
	// — same capabilities as code. It can answer a question OR make the
	// change in the same turn; it's no longer a read-only lane. The
	// search guard (runner.go) still keeps a question-shaped answer
	// grounded, and the dangle guard still catches "described but never
	// edited," so dropping the hard read-only wall doesn't reopen the
	// 2026-05-15 dangling-write loop.
	readOnlyForMode := false

	res, err := agent.Run(ctx, agent.Options{
		Projects:  s.Projects,
		Workspace: s.MainRepo,
		ReadOnly:  readOnlyForMode,
		SharedPaths: s.SharedPaths,
		// Turn cap for chat mode — see chatMaxTurns comment.
		// 0 (zero-value default) for non-conversation chat-path
		// callers would fall back to the runner's 50-turn ceiling,
		// which we don't want to weaken; set explicitly only when
		// resolvedMode is Conversation so /code-routed-to-chat
		// (rare but possible) keeps the larger budget.
		MaxTurns: func() int {
			if resolvedMode == agent.ModeConversation {
				return chatMaxTurns
			}
			return 0
		}(),
		Prompt: runPrompt,
		// The chat agent converses — it does not write code — so it
		// runs on ChatModel (QWEN by default), resolved by
		// buildPlannerServices from config + KAI_CHAT_MODEL. The
		// per-request Model wins over the provider's own default,
		// so a BYOM user's KAI_*_MODEL choice is still honored.
		Model:    s.ChatModel,
		Provider: s.OrchestratorCfg.AgentProvider,
		// kai_consult escalation. Same transport, different model.
		// Empty ConsultModel → tool not registered (graceful skip).
		ConsultProvider:  s.OrchestratorCfg.AgentProvider,
		ConsultModel:     s.OrchestratorCfg.ConsultModel,
		KailabBaseURL:    s.OrchestratorCfg.KailabBaseURL,
		KailabToken:      s.OrchestratorCfg.KailabToken,
		// kai_logs lets the chat agent read the managed process's
		// recent output on demand — answering "do you see the
		// error?" by reading the buffer instead of waiting for
		// the background scanner's auto-followup.
		ManagedProcLogger: NewManagedProcLogger(s),
		Graph:             s.OrchestratorCfg.MainGraph,
		EnableBash:       true,
		BashAllow:        s.OrchestratorCfg.AgentBashAllow,
		MaxTotalTokens:   s.OrchestratorCfg.MaxAgentTokens,
		SessionStore:           s.OrchestratorCfg.AgentSessionStore,
		SessionID:              sessionID,
		TaskName:               "chat",
		UserVisibleHistoryOnly: true,
		// Interactive answer path: keep workspace answers grounded in a
		// real tool call (the search guard). Spawned workers leave this
		// off — their deliverable is edits, covered by the dangle guard.
		GroundAnswers: true,
		GateConfig:       s.GateConfig,
		Mode:             resolvedMode,
		KaiBinary:        kaiBin,
		CheckpointWriter: ckpt,
		// Per-turn run-log artifacts under <KaiDir>/runs/<sessionID>/
		// — drives `kai run last` / `kai run diff`. Cheap (one JSON
		// write per turn), no opt-in: cost-attribution debuggability
		// is more valuable than the few KB of disk per session.
		RunLogDir: s.KaiDir,
		// Disable the pre-caching tool-result trim. With prompt
		// caching on the conversation prefix, trimming each turn
		// rewrites prior tool-result bytes and invalidates the
		// cache breakpoint, forcing every turn to pay the write
		// rate on the full prefix. The planner agent already opts
		// out for the same reason; the chat agent was missing
		// the same fix and was burning ~10–13kB of cache write
		// per turn with cache_read=0. (Confirmed via run-log diff,
		// May-2026.)
		KeepToolResults: true,
		Hooks: agent.Hooks{
			OnToolCall: func(name, inputJSON string) {
				if name == "bash" {
					bashLineCount = 0
				}
				emit("tool", summarizeToolCall(name, inputJSON))
			},
			OnBashOutput: func(line string) {
				// Show only the first 2 lines of bash output per call
				// to keep the TUI from flooding on verbose commands.
				// A suppression notice replaces lines 3+.
				bashLineCount++
				if bashLineCount == 3 {
					emit("bash", "(remaining output suppressed)")
					return
				}
				if bashLineCount > 3 {
					return
				}
				emit("bash", line)
			},
			// OnFileChange is intentionally not wired — OnFileDiff
			// covers the same paths with richer info (we'd be
			// emitting two events per write otherwise).
			OnFileDiff: func(relPath, op, patch string, added, removed int) {
				mutated = true
				capturedTotal++
				if len(capturedEdits) < capturedEditsCap {
					capturedEdits = append(capturedEdits, editSummary{
						Path:    relPath,
						Op:      op,
						Added:   added,
						Removed: removed,
					})
				}
				if s.ChatActivityCh == nil {
					return
				}
				select {
				case s.ChatActivityCh <- ChatActivityEvent{
					Kind:    "diff",
					Path:    relPath,
					Op:      op,
					Diff:    patch,
					Added:   added,
					Removed: removed,
					When:    time.Now(),
				}:
				default:
				}
			},
			OnAssistantDelta: func(delta string) {
				if s.ChatActivityCh == nil {
					return
				}
				select {
				case s.ChatActivityCh <- ChatActivityEvent{
					Kind:  "delta",
					Delta: delta,
					When:  time.Now(),
				}:
				default:
				}
			},
			OnGateVerdict: func(paths []string, verdict string, blastRadius int, reasons []string) {
				if len(paths) > 0 {
					mutated = true
				}
				if s.ChatActivityCh == nil {
					return
				}
				select {
				case s.ChatActivityCh <- ChatActivityEvent{
					Kind:       "gate",
					GatePaths:  paths,
					GateVerdict: verdict,
					GateRadius: blastRadius,
					GateReasons: reasons,
					When:       time.Now(),
				}:
				default:
				}
			},
			OnTurnComplete: func(tIn, tOut, tCached int) {
				dbg.Turn(tIn, tOut, tCached)
				if s.ChatActivityCh == nil {
					return
				}
				select {
				case s.ChatActivityCh <- ChatActivityEvent{
					Kind:         "tokens",
					TokensIn:     tIn,
					TokensOut:    tOut,
					TokensCached: tCached,
					When:         time.Now(),
				}:
				default:
				}
			},
			OnAssistantText: func(text string) {
				dbg.Text(text)
			},
			OnRoutingTrace: dbg.Routing,
			OnProviderState: func(state provider.RequestState) {
				if s.ChatActivityCh == nil {
					return
				}
				select {
				case s.ChatActivityCh <- ChatActivityEvent{
					Kind:    "provider_state",
					Summary: ProviderStateSummary(state),
					When:    state.When,
				}:
				default:
				}
			},
			// OnRequest dumps the FULL request — model, system
			// prompt, tools count, every message with role +
			// part previews. This is the diagnostic for "did
			// the project overview reach the model on this
			// turn?" Run kai code with KAI_PROVIDER=openai +
			// some prompt, then `grep -A 20 REQUEST .kai/chat-debug.log`
			// to see what the chat agent actually shipped.
			OnRequest: func(turn int, req provider.Request) {
				dbg.Request(turn, req)
			},
		},
	})
	// Persist the resolved mode for next turn's sticky/soft logic.
	// We write even on agent error: the mode was the developer's
	// intent for this turn, and the next turn should still see it.
	// Best-effort — a write failure shouldn't block the reply.
	if store := s.OrchestratorCfg.AgentSessionStore; store != nil {
		id := sessionID
		if id == "" && res != nil {
			id = res.SessionID
		}
		_ = session.SaveMode(store, id, resolvedMode.String())
	}
	if err != nil {
		return "", "", 0, 0, 0, err
	}
	if res == nil {
		return "", "", 0, 0, 0, fmt.Errorf("chat: nil result")
	}
	// Use THIS run's final text — never the last assistant message in
	// res.Transcript. On a resumed session Transcript is the whole
	// conversation; walking it backward for "last non-empty assistant
	// message" returns the PRIOR turn's answer whenever the current
	// turn produced an empty completion — a silent replay of stale
	// output (observed: a terse follow-up re-served the previous
	// turn's answer verbatim). res.FinalText is scoped to this run.
	if t := strings.TrimSpace(res.FinalText); t != "" {
		// Phase 2a: if the chat was workspace-gated, the response
		// should begin with an OBSERVED block per workspaceGrounding-
		// Guidance. Strip it from the user-facing reply (logging-only
		// per spec — visible OBSERVED reads as process friction).
		// Also log the presence/absence for metric purposes.
		if classifyWorkspaceTurn(request) {
			stripped, observed := splitObservedBlock(t)
			logGroundingEvent(s, observed, request)
			t = stripped
		}
		return t, res.SessionID, res.TokensIn, res.TokensOut, res.TokensCached, nil
	}

	// Empty completion path. Common cause: the chat model is a
	// reasoning model (Qwen3 family) that consumed the per-turn
	// MaxTokens budget on its silent <think> step and never got to
	// emit visible text. Retry once with the budget doubled and
	// a nudge in the prompt to skip the reasoning prelude. If the
	// second attempt is also empty, surface the original error.
	//
	// Gated on shouldRetryEmpty to keep the retry from firing on
	// legitimate-empty turns (turn was a pure tool-use that
	// finished cleanly with end_turn). Specifically: skip retry
	// when the run made any tool calls or applied any edits; that
	// turn is "done" by design, not failed.
	if res.SessionID != "" {
		sessionID = res.SessionID
	}
	if shouldRetryEmpty(res) {
		emit("info", "model returned no text — retrying with higher token budget (likely reasoning ate the limit)")
		nudgedPrompt := runPrompt + "\n\nReminder: respond with visible text. Do not spend your entire output budget on internal reasoning."
		retryOpts := agent.Options{
			Projects:         s.Projects,
			Workspace:        s.MainRepo,
			ReadOnly:         readOnlyForMode,
			SharedPaths:      s.SharedPaths,
			Prompt:           nudgedPrompt,
			Model:            s.ChatModel,
			Provider:         s.OrchestratorCfg.AgentProvider,
			ConsultProvider:  s.OrchestratorCfg.AgentProvider,
			ConsultModel:     s.OrchestratorCfg.ConsultModel,
			Graph:            s.OrchestratorCfg.MainGraph,
			EnableBash:       true,
			BashAllow:        s.OrchestratorCfg.AgentBashAllow,
			MaxTotalTokens:   s.OrchestratorCfg.MaxAgentTokens,
			MaxTokens:        retryMaxTokens(res),
			SessionStore:           s.OrchestratorCfg.AgentSessionStore,
			SessionID:              sessionID,
			TaskName:               "chat-retry",
			UserVisibleHistoryOnly: true,
			GateConfig:       s.GateConfig,
			Mode:             resolvedMode,
			KaiBinary:        kaiBin,
			CheckpointWriter: ckpt,
			RunLogDir:        s.KaiDir,
		}
		res2, err2 := agent.Run(ctx, retryOpts)
		if err2 == nil && res2 != nil {
			if t := strings.TrimSpace(res2.FinalText); t != "" {
				// Cumulative tokens across both attempts so the trailer
				// reflects what the user actually paid for. Retry usage
				// is real cost; hiding it would understate the turn.
				totIn := res.TokensIn + res2.TokensIn
				totOut := res.TokensOut + res2.TokensOut
				totCached := res.TokensCached + res2.TokensCached
				return t, res2.SessionID, totIn, totOut, totCached, nil
			}
		}
	}

	// Both attempts empty (or retry not applicable). Surface the
	// original error with finish_reason + tokens so the user can see
	// what happened (max_tokens, end_turn-empty, etc.).
	return "", res.SessionID, res.TokensIn, res.TokensOut, res.TokensCached, fmt.Errorf(
		"chat: assistant returned no text (finish=%s, in=%d out=%d cached=%d, turns=%d)",
		res.FinishReason, res.TokensIn, res.TokensOut, res.TokensCached, len(res.Transcript),
	)
}

// shouldRetryEmpty returns true when an empty FinalText looks like
// a recoverable failure (reasoning ate the budget; no tool calls;
// no edits applied), vs a legitimate "I did the work via tools and
// have nothing more to say" turn that we shouldn't waste tokens
// re-running.
//
// The heuristic: a turn that issued zero ToolCalls in its transcript
// AND has FinishReason in (end_turn, max_tokens, length, "") is a
// genuine empty completion worth retrying. A turn that made tool
// calls or applied edits already produced value; retrying would
// duplicate work.
func shouldRetryEmpty(res *agent.Result) bool {
	if res == nil {
		return false
	}
	// Any tool call in this run means real work happened; don't retry.
	for _, t := range res.Transcript {
		for _, p := range t.Parts {
			if _, ok := p.(message.ToolCall); ok {
				return false
			}
		}
	}
	return true
}

// retryMaxTokens decides the budget for the empty-response retry.
// Doubles the in-runner default (16384) to 32768. If the original
// run already reported FinishReason="max_tokens" or "length" — i.e.
// the model truly hit the ceiling — bump further to 49152 to give
// reasoning + visible output room to coexist.
func retryMaxTokens(res *agent.Result) int {
	switch strings.ToLower(string(res.FinishReason)) {
	case "max_tokens", "length":
		return 49152
	default:
		return 32768
	}
}

// editSummary is a compact view of a single OnFileDiff event, kept
// in memory during a chat-agent turn so the deferred `kai capture`
// can synthesize a meaningful commit-style message from the actual
// mutations rather than just echoing back the user's prompt.
type editSummary struct {
	Path    string
	Op      string // "A" (add), "M" (modify), "D" (delete) — matches OnFileDiff
	Added   int
	Removed int
}

// captureHeadline shapes the user request into a one-line headline
// suitable as the first line of the capture message. Conservative —
// trims trailing punctuation/whitespace and truncates to 120 chars.
// No model call; intentionally idempotent.
func captureHeadline(request string) string {
	msg := strings.TrimSpace(request)
	if msg == "" {
		return "chat: agent edits"
	}
	// Collapse internal newlines so the headline stays a single line.
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.TrimRight(msg, " \t.?!,;:")
	if len(msg) > 120 {
		msg = msg[:117] + "..."
	}
	return msg
}

// composeCaptureMessage builds the multi-line `-m` payload: headline,
// blank line, then a `Files:` block listing up to 6 edits with their
// +added/-removed counts. If more edits were captured than fit, a
// trailing "… and N more" line gives the reviewer the true count.
func composeCaptureMessage(headline string, edits []editSummary, total int) string {
	if len(edits) == 0 {
		return headline
	}
	const maxShown = 6
	var b strings.Builder
	b.WriteString(headline)
	b.WriteString("\n\n")
	b.WriteString("Files:\n")
	shown := len(edits)
	if shown > maxShown {
		shown = maxShown
	}
	for i := 0; i < shown; i++ {
		e := edits[i]
		op := e.Op
		if op == "" {
			op = "M"
		}
		b.WriteString("  ")
		b.WriteString(op)
		b.WriteString(" ")
		b.WriteString(e.Path)
		// Show +/- counts. For deletions the added count is typically
		// zero and we omit the +0 to keep lines tight; same for adds.
		if e.Added > 0 && e.Removed > 0 {
			fmt.Fprintf(&b, " (+%d -%d)", e.Added, e.Removed)
		} else if e.Added > 0 {
			fmt.Fprintf(&b, " (+%d)", e.Added)
		} else if e.Removed > 0 {
			fmt.Fprintf(&b, " (-%d)", e.Removed)
		}
		b.WriteString("\n")
	}
	if total > shown {
		fmt.Fprintf(&b, "  … and %d more\n", total-shown)
	}
	return strings.TrimRight(b.String(), "\n")
}

// runKaiCapture shells out to `kai capture -m <message>` against the
// chat agent's main repo so the semantic graph reflects the changes
// the agent just made. Without this, the next turn's graph context
// injection (and any kai_callers tool calls the model issues) would
// see stale call structure — e.g. a function the agent just renamed
// would still appear under its old name.
//
// Best-effort: the agent's edits already landed regardless of
// whether capture succeeds; a capture failure here just delays the
// graph refresh until the next manual capture or watcher tick. We
// surface failures as a dim line in the activity feed so a chronic
// problem (binary missing, db locked) is visible without halting
// the chat flow.
//
// Message: a headline derived from the user's request followed by
// a `Files:` block summarizing up to 6 of the mutations captured
// from OnFileDiff. Cheap and deterministic — no second round-trip
// to the model just to summarize what the model already did.
func runKaiCapture(s *PlannerServices, request string, edits []editSummary, total int, emit func(kind, summary string)) {
	if s == nil || s.MainRepo == "" {
		return
	}
	binary, err := os.Executable()
	if err != nil || binary == "" {
		return // can't self-invoke; bail silently
	}
	headline := captureHeadline(request)
	msg := composeCaptureMessage(headline, edits, total)
	// Activity preview keeps the scrollback clean — only the
	// headline shows here, the `Files:` block is just for the
	// commit-style message that `kai capture` records.
	emit("tool", "→ kai capture: "+headline)

	// Detached: run in a goroutine so the caller's defer doesn't
	// block the TUI on a slow capture (parsing a fresh file tree
	// can take a couple seconds on cold cache). We do still wait
	// inside the goroutine so we can surface the result.
	go func() {
		// Independent ctx — we don't want a TUI-level cancel mid-
		// capture to corrupt the graph state. Bounded at 30s; if
		// capture is still running at that point something is
		// genuinely wrong.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, binary, "capture", "-m", msg)
		c.Dir = s.MainRepo
		out, runErr := c.CombinedOutput()
		if runErr != nil {
			emit("tool", "  kai capture failed: "+strings.TrimSpace(string(out)))
			return
		}
		emit("tool", "  kai capture: graph updated")
	}()
}

// chatSystemPrompt is the model brief for the chat-fallback path.
// Short on purpose — the user typed something conversational; the
// model should match that energy and only call tools when the user
// actually asked an inspection question.
const chatSystemPrompt = `IMPORTANT: Never generate or guess URLs. Only use URLs the user provided or that appear in local files.

IMPORTANT: Security: assist with authorized testing, defensive work, CTFs, and education. Refuse destructive techniques, DoS, mass targeting, supply-chain compromise, or evasion for malicious purposes. Dual-use tools (C2, credential testing, exploit dev) need clear authorization context.

You are a hands-on coding assistant inside the kai CLI with full read/write access to the workspace.

Tools (use schemas for argument details):
  - kai_search — PRIMARY: free-text "where do we set/use/mention X" (BM25 FTS5, multi-root, sub-10ms).
  - kai_grep — FALLBACK regex for queries kai_search can't express, or very recent uncommitted edits.
  - kai_tree / kai_files — directory tree / glob listing. Replaces bash ls/tree/find.
  - kai_symbols — top-level symbols (functions, types, methods) from the parsed AST.
  - kai_callers / kai_dependents / kai_context — semantic graph: who calls X, what depends on Y, what's in this file.
  - kai_impact — blast radius: callers + dependents with risk none/low/medium/high. Call BEFORE editing shared infrastructure.
  - view / write / edit — read with line numbers / create / patch a unique substring.
  - bash — shell ONLY for things kai tools can't do (npm test, git, mkdir, mv, scripts). Never bash ls/cat/head/tail/find/grep/tree/wc — kai equivalents are cheaper and cleaner.

Discovery rule: for "where is X / who calls Y / what's in dir Z", reach for kai_search (free-text), kai_callers/kai_dependents/kai_context (symbol-shaped), kai_tree/kai_files (structure), or view (known file) FIRST. kai_grep is fallback, bash is last resort. Bash exploration burns tokens that pile up across the session.

Parallel tool calls: emit independent lookups as parallel tool_use blocks in a single response. Read-only tools dispatch concurrently; 5 batched lookups cost ~1 turn's wall-clock vs. 5 serial round-trips paying full history each time.

EXPLORATION BUDGET — applies to EVERY chat turn, not just overview questions. The model has a hard token cap (default 200k); runs that exceed it crash mid-turn. Budget for a typical "review / explain / where is X" task: 5-10 tool calls before you converge. Budget for a one-shot edit you can see clearly: 1-3. If you find yourself past 10 calls and still searching, STOP — answer what you know and tell the user what's still uncertain. Answering "I checked these 4 places and they all returned nothing, so either the symbol doesn't exist or my search terms are wrong" is far more useful than burning the rest of the budget on retries.

NEVER view the same file more than once. Cache makes repeat reads cheap in TOKENS but they still cost a TURN against the budget and they don't tell you anything new. If you need more of a file than your first view returned, your first view should have been wider. Slicing the same file ("view L1-100", "view L100-300") is a single wasted decision.

NEVER re-search a term you already searched, even with a different query phrasing. If kai_search returned nothing, the answer is "this term isn't in the FTS index" — escalate to kai_grep (regex, walks the actual files) once and accept that as ground truth. Don't try four phrasings of the same query hoping one lands. If a project's content isn't in the FTS index, kai_search will NEVER find it no matter how you spell the query — switch tools, don't retry the same one.

EXEMPLAR-FIRST when reviewing or extending a feature: find one closest existing exemplar with kai_grep, view it ONCE in full, then look at the file under review. Don't read the surrounding package top-to-bottom — the exemplar tells you the convention, the under-review file shows where it diverges, and three more views won't change the conclusion.

PATH PREFIX in multi-root workspaces: when the workspace lists multiple projects, file paths use the PROJECT NAME as the prefix (e.g. "kai-server/kailab-control/foo.go"), NOT a subdirectory name that looks like a project (e.g. "kai-cli" is a subdirectory inside the "kai" project — files there need "kai/kai-cli/foo.go"). Bare subdirectory names that aren't projects won't resolve.

TOOL ERRORS ARE FACTS, NOT GAPS TO FILL. When a view returns "file not found," a kai_grep returns zero hits, a kai_callers returns an empty list, or any tool errors out — that IS the answer. Say so. Do NOT write a paragraph as if the tool had succeeded. Specifically: never describe file contents, symbol names, line numbers, or counts that didn't come from a successful tool result THIS turn. If you didn't see it in a tool result, say "I didn't see it" — not "I saw X with N callers." A short honest "I tried to view that file and it doesn't exist" beats a confident-sounding paragraph of invented details every time. The user can act on honest uncertainty; they CANNOT recover from confidently-stated falsehoods.

You are NOT read-only. If the user asks you to create a file, run a command, or make a change — just do it. Don't tell the user to run commands themselves.

Audience: assume the user may be new to kai. Don't reference "kai workspace / config / snapshots / modules" — say "this directory / your project / source files / git". Files like .kai/, kai.modules.yaml, kai.projects.yaml are bookkeeping; mention only when directly asked. For empty dirs, treat as brand-new project and offer to scaffold something concrete.

Voice: pair-programming, not a report. Contractions, light connective tissue, occasional "ok" / "hmm" / "right" on real transitions. Before a tool call, narrate briefly in first person ("let me peek at the routes file"). After a result, react to what it revealed ("huh, only one caller — safer than I thought"). No emoji spam, no persona drift, no excessive hedging.

Style: 1–4 sentences after you finish unless detail was asked for. Summarize file contents, don't paste. No markdown headers — TUI renders flat scrollback; use **bold** or prose. For multi-step changes touching several files, suggest re-phrasing as a concrete change request so the planner routes through review; for small one-shot tasks, just do them.`

// chatOverviewGuidance is appended only on the first turn of a
// session, when graph_context.go injects the Project overview block.
// On subsequent turns the overview isn't re-injected, so the
// guidance about it is dead weight — drop it to save ~450 tokens
// per turn.
// workspaceReminder returns a one-line per-turn anchor that names the
// cwd and project so the model can't default to "the kai coding agent
// repository" when the user is in some other directory. Cheap (~50
// tokens per turn) compared to re-injecting the full overview (~450).
// The workspace name comes from the projects.Set's primary project if
// available; otherwise we fall back to the base name of MainRepo.
func workspaceReminder(s *PlannerServices) string {
	cwd := s.MainRepo
	name := filepath.Base(cwd)
	if s.Projects != nil {
		if projs := s.Projects.Projects(); len(projs) > 0 && projs[0] != nil && projs[0].Name != "" {
			name = projs[0].Name
		}
	}
	return fmt.Sprintf(
		"[WORKSPACE REMINDER] cwd: %s — primary project name: %q. This is the user's workspace. When they say \"this repo / codebase / project / directory\" they mean THIS, not the kai source (unless the project name above IS kai/kai-cli/kai-core/kai-tui).",
		cwd, name,
	)
}

// logGroundingEvent persists the OBSERVED-presence signal to
// <kaiDir>/grounding.log so the BubbleTea-swallowed stderr doesn't
// hide the metric we shipped Phase 2a to measure. File grows append-
// only — small (one short line per workspace chat turn).
func logGroundingEvent(s *PlannerServices, observed, request string) {
	if s == nil || s.Projects == nil {
		return
	}
	primary := s.Projects.Primary()
	if primary == nil || primary.KaiDir == "" {
		return
	}
	path := filepath.Join(primary.KaiDir, "grounding.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().Format(time.RFC3339)
	if observed == "" {
		fmt.Fprintf(f, "[%s] OBSERVED-MISSING req=%.80q\n", ts, request)
		return
	}
	// Cap the preview so a model that pours 4KB into OBSERVED doesn't
	// bloat the log; the full content is in chat-debug.log anyway.
	preview := observed
	if len(preview) > 400 {
		preview = preview[:400] + "...(trunc)"
	}
	fmt.Fprintf(f, "[%s] OBSERVED-emitted bytes=%d req=%.80q preview=%q\n", ts, len(observed), request, preview)
}

// splitObservedBlock separates the OBSERVED block (Phase 2a grounding)
// from the user-facing prose. Returns (proseOnly, observedBlock).
// Tolerant of the explicit (none — ...) escape form and of minor
// formatting drift (markdown emphasis around OBSERVED, leading
// whitespace). If no OBSERVED block is present, returns (input, "").
//
// The observed block is logged for measurement; the prose alone is
// returned to the REPL so the user doesn't see the grounding
// scaffolding as visible process friction.
func splitObservedBlock(reply string) (prose, observed string) {
	// Anchor: "OBSERVED:" or "**OBSERVED:**" or "OBSERVED" at line start.
	// Look for it within the first 200 chars (otherwise it's not a
	// leading block, model dropped it).
	idx := -1
	upper := strings.ToUpper(reply)
	for _, anchor := range []string{"OBSERVED:", "**OBSERVED:**", "OBSERVED\n"} {
		if i := strings.Index(upper, anchor); i >= 0 && i < 200 {
			idx = i
			break
		}
	}
	if idx < 0 {
		return reply, ""
	}
	// Find end of OBSERVED: either two consecutive newlines (block end)
	// or "Answer:" / start of next prose section. Look ahead from idx.
	tail := reply[idx:]
	endIdx := strings.Index(tail, "\n\n")
	if endIdx < 0 {
		// No clear separator — treat the whole thing as observed,
		// emit empty prose. This is a malformed response shape; the
		// REPL would show empty, which is correct: the model failed
		// the format and needs the auto-retry path.
		return "", tail
	}
	observed = strings.TrimSpace(tail[:endIdx])
	prose = strings.TrimSpace(tail[endIdx+2:])
	// Also discard anything BEFORE the observed block (the model may
	// have leaked some preamble despite the instruction). The prose
	// after is the only user-facing content.
	return prose, observed
}

// workspaceGroundingGuidance is appended to the chat system prompt
// on workspace-shaped questions (the classifyWorkspaceTurn gate).
// It instructs the model to emit an OBSERVED block before its prose
// answer, listing every file it actually read this turn with concrete
// references. The block is logging-only at the harness level — the
// REPL strips it before display — but its presence in the response
// stream is the leverage point: forcing the model to commit to "what
// I saw" before "what I think" disrupts the synthesis-stage default
// of patterns-list answers ungrounded in the conversation.
//
// The (none — ...) escape path is load-bearing: without it the model
// fabricates a citation to satisfy the format, which is strictly
// worse than a generic answer. The spec (workspace-grounding-spec.md)
// pins this as the single most important detail.
//
// Phase 2a: single-call inline format. Phase 2b: split into two
// separate generation calls for the harder ordering guarantee, plus
// bidirectional containment checks (B-refs ⊆ A-refs hard reject,
// A-refs not in B logged as drift signal).
const workspaceGroundingGuidance = `GROUNDING FOR WORKSPACE QUESTIONS. The user asked about this codebase. Begin your reply with an OBSERVED block listing every file you actually read in this turn or the recent conversation, each with a concrete reference:

OBSERVED:
- path/to/file.ext:42 — what you saw there
- other/path/file.ext — the symbol or pattern that matters

If you genuinely read nothing in this workspace that bears on the question (because the answer is a harness default, a general fact, or something not in the codebase), emit the explicit empty form:

OBSERVED: (none — no workspace file governs this; it's a harness default / general fact)

Then a blank line, then your user-facing answer. The answer must reference ONLY items present in OBSERVED. If OBSERVED is (none …), the answer is permitted and expected to say so plainly ("this isn't in the codebase; it's the harness default") rather than fabricating a path to satisfy the format. A confident fake citation is strictly worse than a generic answer.

VERIFY CONTRACTS YOU ASSERT, DON'T JUST READ THE CALLER. If your answer claims how an external command, flag, API, or output format BEHAVES — not merely that the code calls it — you must have RUN it this turn and put the real result in OBSERVED. Reading the spawn()/exec()/fetch() that invokes it is NOT observing it: the code can call a command or flag that does not exist. Before you assert how a command behaves — say, that some code "pulls records via reportgen --json" — ask yourself: am I doing this correctly, i.e. have I actually run that exact command and seen its output, or am I trusting the code that calls it? If you only read the caller, run the command and confirm before claiming it works; if it errors or emits a different shape, that IS the answer (the feature is broken even though the code compiles).

TRACE A CONNECTION TO BOTH ENDS BEFORE CONCLUDING. For "does X use Y / is A wired to B / where do these events come from" questions, a channel, event, or symbol has a PRODUCER and a CONSUMER. Following one import chain from where you started shows only ONE end. Before you conclude, grep the connecting NAME for ALL its sites. Seeing a component subscribe to a "record-updated" event tells you the consumer; you must also grep "record-updated" to find who EMITS it before you claim the source — and the emitter may be the very thing you're about to say is unused. Concluding "X does not use Y" or "zero presence" without having searched for the other end is a guess; one grep of the connecting name usually settles it.

Do NOT skip the OBSERVED block. Do NOT write the answer first and then list observations after. OBSERVED comes first, every workspace turn, no exceptions.`

const chatOverviewGuidance = `Exploration budget: for "what does this project do / explain this repo / give me an overview" questions, the intended cost is the auto-injected Project overview block + roughly 3-5 tool calls TOTAL. First call is usually kai_tree on root with depth=2 (structure + immediate subdirs + [N children] counts in one shot). Then maybe 1-2 view/kai_tree calls to confirm specifics. Then ANSWER. Don't walk every subdirectory; a child count of 47 is enough to mention the area without descending. Past 5 exploratory calls, you are over-fetching.

When kai injects a "Project overview" block, treat it as authoritative: it carries the manifest, top-level tree, and README. Answer overview-shaped questions directly from it. Don't re-list files, view the manifest you can already see, or re-read the README excerpt that's there. Only call view/bash for code-level detail the overview doesn't expose.`

// runReplan combines the original request and feedback into a fresh
// plan. The original is preserved so the user can iterate without
// retyping the whole prompt.
func runReplan(s *PlannerServices, original, feedback string) tea.Cmd {
	if s == nil {
		return func() tea.Msg {
			return PlanReadyMsg{Request: original, Err: fmt.Errorf("planner not configured")}
		}
	}
	return func() tea.Msg {
		ctx := context.Background()
		plan, err := planner.Replan(ctx, original, feedback, s.DB, s.GateConfig, s.PlannerCfg, s.LLM)
		// Track the combined request as the "original" going forward
		// so further feedback layers correctly.
		req := strings.TrimSpace(original) + " // " + strings.TrimSpace(feedback)
		return PlanReadyMsg{Request: req, Plan: plan, Err: err}
	}
}

// runExecute kicks off orchestrator.Execute. This subprocess + push/
// pull dance can take minutes in real use, but it's still wrapped in
// one tea.Cmd so the message-driven UI keeps working. Subsequent
// keypresses queue while it runs; the REPL guards against starting
// a second execute concurrently via the `executing` flag.
func runExecute(s *PlannerServices, plan *planner.WorkPlan) tea.Cmd {
	if s == nil {
		return func() tea.Msg {
			return ExecuteDoneMsg{Err: fmt.Errorf("orchestrator not configured")}
		}
	}
	return func() tea.Msg {
		ctx := context.Background()
		// Heartbeat ticker. The orchestrator's executor hooks
		// (OnAgentLifecycle / OnAgentBashOutput / OnAgentProviderState /
		// OnFileDiff) DO push events to ChatActivityCh, but they can
		// dry up for minutes during legitimate work: a long single
		// LLM turn with no streamed deltas, a between-phase pause
		// (despawn → integrate → re-spawn), or chatCh overflow (cap
		// 64, non-blocking send) silently dropping bursts of events.
		// Without a baseline "still working" signal:
		//   - r.lastActivity goes stale → stuck-hint nags (annoying)
		//   - the in-flight-flag-gated auto-escalate (v0.32.26) saves
		//     us from the cancel-and-murder case, BUT the user has
		//     no positive indicator that work is happening between
		//     real events.
		// A 5s ticker pushing a synthetic "executing" event keeps
		// the lastActivity stamp fresh AND gives the spinner area a
		// "still alive" cadence. Sentinel kind so the REPL handler
		// can choose to render it (or not — currently invisible,
		// just bumps lastActivity via the generic case in the
		// ChatActivityMsg switch).
		done := make(chan struct{})
		if s.ChatActivityCh != nil {
			go func() {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-done:
						return
					case <-ticker.C:
						select {
						case s.ChatActivityCh <- ChatActivityEvent{
							Kind:    "executor_heartbeat",
							Summary: "executing…",
							When:    time.Now(),
						}:
						default:
							// chatCh full — that itself is evidence
							// of activity; the next consumer will
							// bump lastActivity.
						}
					}
				}
			}()
		}
		res, err := orchestrator.Execute(ctx, plan, s.OrchestratorCfg, s.DB, s.MainRepo, s.KaiDir)
		close(done)
		return ExecuteDoneMsg{Result: res, Err: err}
	}
}

// isGreeting reports whether the input is obvious chitchat — a
// pure greeting / thanks / acknowledgment with no concrete request
// embedded. Used by the dispatcher to short-circuit the planner so
// "hi how are you" doesn't burn a 1k-token planner round-trip just
// to produce a "no work to plan" warning that has to fall back to
// chat anyway.
//
// Conservative: only short, prefix-anchored chitchat triggers. Any
// input over 80 chars or containing an action verb (add, fix,
// etc.) falls through to the planner as before. A misclassified
// "hi could you add X" routes to chat, the chat agent answers
// without editing, and the user can re-ask — better than the
// alternative (planner-styled output to "hi").
// isShortAffirmative recognizes follow-up replies that are
// unambiguously responses to the prior turn — affirmatives ("yes",
// "go", "do it"), choice picks ("a", "b", "1", "2", "option a"),
// and continuations ("more", "continue", "keep going"). These get
// routed to the chat agent which inherits the prior session's
// history; the planner would treat them as standalone requests and
// emit "too vague" because "yes" has no context on its own.
//
// Conservative — we only match very short inputs. Anything that
// might be a real (if terse) plan request like "fix it" or "ship
// it" is left to the planner, since "it" could refer to anything
// and the planner's exploration will resolve scope better than
// chat would.
func isShortAffirmative(request string) bool {
	r := strings.ToLower(strings.TrimSpace(request))
	if r == "" {
		return false
	}
	// Hard cap on length: anything over 20 chars is unlikely to be
	// a bare affirmative. Catches "yes do that", "a, that one", etc.
	if len(r) > 20 {
		return false
	}
	r = strings.TrimRight(r, "!.?,")
	r = strings.Join(strings.Fields(r), " ")

	exact := map[string]bool{
		// Affirmatives
		"yes": true, "yeah": true, "yep": true, "yup": true, "y": true,
		"sure": true, "absolutely": true, "definitely": true,
		"correct": true, "right": true, "exactly": true,
		// Imperatives (clearly directed at the prior turn's offer)
		"go": true, "go ahead": true, "go for it": true,
		"do it": true, "do that": true, "make it so": true,
		"proceed": true, "continue": true, "keep going": true,
		"go explore": true, "explore": true,
		// Choice picks
		"a": true, "b": true, "c": true, "d": true,
		"1": true, "2": true, "3": true, "4": true,
		"first": true, "second": true, "third": true,
		"option a": true, "option b": true, "option c": true,
		"option 1": true, "option 2": true, "option 3": true,
		"the first": true, "the second": true, "the third": true,
		// Continuations
		"more": true, "more please": true, "again": true,
	}
	return exact[r]
}

func isGreeting(request string) bool {
	r := strings.ToLower(strings.TrimSpace(request))
	if r == "" {
		return false
	}
	if len(r) > 80 {
		return false
	}
	// Strip common trailing punctuation (`!`, `.`, `?`) so "hi!" /
	// "hi." / "hi?" all classify as greeting.
	r = strings.TrimRight(r, "!.?,")
	if r == "" {
		return false
	}
	// Normalize interior commas to spaces so "good morning, anything
	// new" matches the "good morning " prefix. Collapse runs of
	// whitespace too. Cheap; doesn't change semantics for other
	// inputs because real code-change requests don't usually carry
	// commas at this position.
	r = strings.ReplaceAll(r, ",", " ")
	r = strings.Join(strings.Fields(r), " ")
	// Standalone greetings: the entire input is one of these.
	standalone := map[string]bool{
		"hi": true, "hello": true, "hey": true, "yo": true, "sup": true,
		"hola": true, "howdy": true, "greetings": true, "salutations": true,
		"thanks": true, "thank you": true, "thx": true, "ty": true,
		"ok": true, "okay": true, "k": true, "kk": true,
		"cool": true, "nice": true, "great": true, "awesome": true,
		"good morning": true, "good afternoon": true, "good evening": true,
		"bye": true, "goodbye": true, "later": true, "cya": true,
	}
	if standalone[r] {
		return true
	}
	// Prefix-anchored: "hi there", "hello how are you", etc. Skip
	// when the input contains a concrete code-change verb — that
	// signals the user is greeting + asking for work in one breath
	// ("hi can you add rate limiting") and we should route to the
	// planner so the work gets planned, not just chatted at.
	prefixes := []string{
		"hi ", "hello ", "hey ", "yo ", "sup ", "hola ", "howdy ",
		"thanks ", "thank you ", "thx ", "ty ",
		"good morning ", "good afternoon ", "good evening ",
		"how are you", "how is it going", "how's it going",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(r, p) || r == strings.TrimSpace(p) {
			if containsActionVerb(r) {
				return false
			}
			return true
		}
	}
	return false
}

// isQuestion classifies an input as a question/discussion (route
// to chat agent) vs. a code-change request (route to planner).
//
// Bias is heavily toward "this is a code change" — we're inside a
// code-change TUI; the user typed something into a planner prompt.
// Routing a code-change request to the chat agent is bad because
// the chat agent runs in ModeConversation (read-only tools), so it
// loops narrating "I'll edit X" while only being able to view —
// see the "Can you do the token expiry one?" pathology that
// prompted this rewrite.
//
// Conservative routing rules:
//   - "?" in the first line and no action verb → question
//   - Starts with explicit question word (what/how/why/who/when/
//     where/explain/describe/list/show me/tell me/suggest/
//     recommend/compare) and no action verb → question
//   - Everything else → planner
//
// "Can you / could you / should I" are NOT treated as questions
// anymore — in this UX they almost always mean "please do." The
// planner can refuse if there's truly nothing to plan.
func isQuestion(request string) bool {
	r := strings.TrimSpace(request)
	if r == "" {
		return false
	}
	low := strings.ToLower(r)
	// Strip inline-code spans before classification. Without this,
	// the literal `?` (referring to a keyboard key) or any other
	// punctuation inside backticks gets mistaken for prose. The
	// 2026-05-12 dogfood pinned this: "I want the `?` key to
	// toggle..." was routed to chat because the backticked ? read
	// as a question mark. Backtick spans almost always name an
	// identifier / key / token, not English text — drop them.
	low = stripInlineCode(low)
	firstLine := strings.SplitN(low, "\n", 2)[0]
	hasQ := strings.Contains(firstLine, "?")
	prefixes := []string{
		"what ", "how ", "why ", "who ", "when ", "where ",
		"which ", "whose ", "whether ",
		"explain ", "describe ", "list ", "show me ", "tell me ",
		"suggest ", "recommend ", "compare ",
	}
	hasQPrefix := false
	for _, p := range prefixes {
		if strings.HasPrefix(low, p) {
			hasQPrefix = true
			break
		}
	}
	// Strongest signal: explicit "?" PLUS a question-word prefix.
	// Both together overrule the action-verb check — "which one
	// should I do first?" contains "do " but is unambiguously a
	// question because the user typed both signals. Same for
	// "what should I add next?".
	if hasQ && hasQPrefix {
		return true
	}
	// "?" alone — fall back to the action-verb check, since
	// "should I add X?" with a question mark might still be a
	// request to do work.
	//
	// EXCEPTION: long meta-discussions about the agent/planner/system
	// often contain action verbs as CONTENT, not as imperatives — e.g.
	// "you as the agent need to do a simple task… is there a way to
	// make sure they stay simple?" The action verbs ("do", "make") are
	// discussion of behavior, not commands. We override the verb check
	// when (a) the prompt is long (>120 chars, suggests prose not a
	// terse imperative), (b) ends with "?", AND (c) contains a
	// meta-discussion marker like "the agent", "the planner", "you as",
	// "is there a way", "way to". 2026-05-15 dogfood pinned this: the
	// user's "is there a way to make simple tasks stay simple?" routed
	// to planner, which exploded into a 6m26s exploration before
	// emitting a no-plan.
	if hasQ && isMetaDiscussion(low) {
		return true
	}
	if hasQ {
		return !containsActionVerb(firstLine)
	}
	// Question-word prefix without "?" — apply the action-verb
	// check to weed out "what would change if we add X" style
	// code-change requests.
	if hasQPrefix {
		snippet := low
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		if containsActionVerb(snippet) {
			return false
		}
		return true
	}
	return false
}

// stripInlineCode removes backtick-delimited spans from a string so
// downstream classifiers see only the prose. Backtick spans almost
// always carry identifiers / keys / tokens that shouldn't influence
// chat-vs-plan routing — e.g. "the `?` key" names a keyboard binding,
// not an interrogative sentence. Handles unmatched trailing backticks
// gracefully (drops everything after the unclosed backtick) rather
// than panicking. Triple-backtick code blocks are also stripped via
// the same loop (each pair of ``` is treated as a backtick pair).
func stripInlineCode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inCode := false
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			inCode = !inCode
			continue
		}
		if !inCode {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isMetaDiscussion reports whether a prompt looks like a question
// ABOUT how the agent/system behaves, rather than a request for the
// agent to do something. The signal is conservative: must be long
// (>120 chars, real prose not a terse imperative) AND contain at
// least one explicit meta marker. Action verbs in such prompts are
// content ("the agent had to UPDATE three files"), not commands.
func isMetaDiscussion(low string) bool {
	// Floor at 80 chars: terse imperatives ("is there a way to add X")
	// fall through to the action-verb check; real discussions are
	// almost always at least 80 chars of prose.
	if len(low) < 80 {
		return false
	}
	markers := []string{
		"the agent", "the planner", "the system",
		"the model", "the tui", "the worker",
		"you as", "you as the agent",
		"is there a way", "is there any way", "any way to",
		"is it possible", "is it normal",
		"how come", "do we have", "do we know",
		"why does", "why is",
		"what controls", "what makes",
		"why does this",
	}
	for _, m := range markers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// containsActionVerb reports whether the snippet has a code-change
// verb that signals "this is actually a request to do work."
// Includes "do" for "can you do X", "make" for "make X", etc.
func containsActionVerb(s string) bool {
	for _, v := range []string{
		"add ", "create ", "implement ", "build ", "write ",
		"fix ", "remove ", "delete ", "rename ", "refactor ",
		"update ", "change ", "modify ", "edit ", "make ",
		"do ", "ship ", "wire ", "hook ", "set up ", "setup ",
		"replace ", "rewrite ", "extract ", "inline ",
	} {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// formatPlan renders a WorkPlan for the REPL scrollback. Layout
// goals: each agent's prompt is broken into sentence-per-line so
// it scans as a checklist instead of a paragraph; risk notes use
// dim styling (not warn yellow) because they're informational
// findings from exploration, not errors; spacing between agents
// makes scanning easier.
//
// Empty-agents plans get a totally different layout — a clear
// "Nothing to do" headline instead of "Plan: 0 agent(s)." That
// case happens when the model's exploration concluded the work
// is already done; the JSON shape (empty agents + risk_notes
// with the evidence) is correct but reads as "did the planner
// fail?" if rendered as a normal plan. The dedicated layout
// makes the conclusion unambiguous.
func formatPlan(p *planner.WorkPlan) string {
	if p == nil {
		return styleError.Render("(no plan)")
	}
	if len(p.Agents) == 0 {
		return formatEmptyPlan(p)
	}
	var b strings.Builder
	// Sherlock-Holmes-style explanation BEFORE the work split.
	// Diagnosis tells the user what's wrong + why; Approach
	// tells them how the fix works. Both are required (per the
	// planner system prompt) for non-trivial fixes; the model
	// may skip them on obvious requests like "rename X to Y".
	// Without this prelude the plan reads like a commit message
	// and the user has to trust the planner blindly when
	// hitting "go". With it they can spot bad reasoning before
	// confirming.
	if d := strings.TrimSpace(p.Diagnosis); d != "" {
		b.WriteByte('\n')
		b.WriteString(stylePlannerBanner.Render("🔍 Diagnosis"))
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "  %s\n\n", wrapText(d, 78))
	}
	if a := strings.TrimSpace(p.Approach); a != "" {
		b.WriteString(stylePlannerBanner.Render("🛠  Approach"))
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "  %s\n\n", wrapText(a, 78))
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "%s\n", stylePlannerBanner.Render(fmt.Sprintf("📋 Plan — %d agent(s)", len(p.Agents))))
	if s := strings.TrimSpace(p.Summary); s != "" {
		fmt.Fprintf(&b, "  %s\n", styleDim.Render(s))
	}
	b.WriteByte('\n')
	// Compact rendering: agent name + headline sentence + files
	// line. Everything else (prompt continuation, don't-touch
	// list, exploration notes) is hidden behind a "?" keystroke
	// to keep the action menu on-screen on a normal terminal.
	// formatPlanDetails renders the long form when the user
	// asks for it.
	for i, a := range p.Agents {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "  • %s\n", a.Name)
		sentences := splitSentences(strings.TrimSpace(a.Prompt))
		if len(sentences) > 0 {
			fmt.Fprintf(&b, "      %s\n", sentences[0])
		}
		if len(a.Files) > 0 {
			fmt.Fprintf(&b, "      %s\n", styleDim.Render("files: "+strings.Join(a.Files, ", ")))
		}
	}
	// Footer hint — only when there's actually more to show.
	hidden := planHiddenLines(p)
	if hidden > 0 {
		b.WriteByte('\n')
		b.WriteString(styleDim.Render(fmt.Sprintf("(%d more lines hidden — press ? for full plan)", hidden)))
		b.WriteByte('\n')
	}
	// The action menu (go / cancel / feedback) is rendered as a
	// live transient by the REPL above the input, not pinned in
	// scrollback — see (*REPL).renderPlanMenu in repl.go. Pinning
	// it here would duplicate it once the user makes a choice.
	return b.String()
}

// splitSentences breaks a paragraph into one sentence per element.
// Cheap heuristic: split on ". " (period + space) followed by
// a capital letter or backtick. Keeps the trailing period. Leaves
// intra-sentence periods (e.g. "kai-cli/file.go") intact because
// the next character isn't a capital after a space.
//
// Returns the input as a single-element slice when no sentence
// boundary is found — better to render as one line than to split
// in the wrong place.
// planHiddenLines counts how many lines of detail formatPlan has
// suppressed. Used by the compact render's footer hint to show a
// truthful "N more lines hidden" rather than a generic "press ? for
// more". A truthful count tells the user upfront whether expansion
// is worth the screen space.
func planHiddenLines(p *planner.WorkPlan) int {
	if p == nil {
		return 0
	}
	n := 0
	for _, a := range p.Agents {
		// Sentences past the first are hidden.
		s := splitSentences(strings.TrimSpace(a.Prompt))
		if len(s) > 1 {
			n += len(s) - 1
		}
		if len(a.DontTouch) > 0 {
			n++
		}
	}
	if len(p.RiskNotes) > 0 {
		n += 1 // header line
		for _, note := range p.RiskNotes {
			n += len(splitSentences(strings.TrimSpace(note)))
		}
	}
	return n
}

// formatPlanDetails renders the FULL plan — agent prompts in full,
// don't-touch lists, all exploration notes. Called when the user
// presses "?" while a plan is pending. Layout matches formatPlan's
// structure so the user can mentally pair the two views.
func formatPlanDetails(p *planner.WorkPlan) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(stylePlannerBanner.Render("─ plan details ─"))
	b.WriteByte('\n')
	// Diagnosis/Approach repeat in the details view for users
	// who jump straight to "?" without reading the compact
	// version. Tiny duplication in exchange for the long view
	// being self-contained.
	if d := strings.TrimSpace(p.Diagnosis); d != "" {
		b.WriteByte('\n')
		b.WriteString(stylePlannerBanner.Render("🔍 Diagnosis"))
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "  %s\n\n", wrapText(d, 78))
	}
	if a := strings.TrimSpace(p.Approach); a != "" {
		b.WriteString(stylePlannerBanner.Render("🛠  Approach"))
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "  %s\n\n", wrapText(a, 78))
	}
	for i, a := range p.Agents {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "  • %s\n", a.Name)
		for _, line := range splitSentences(strings.TrimSpace(a.Prompt)) {
			fmt.Fprintf(&b, "      %s\n", styleDim.Render(line))
		}
		if len(a.Files) > 0 {
			fmt.Fprintf(&b, "      %s\n", styleDim.Render("files: "+strings.Join(a.Files, ", ")))
		}
		if len(a.DontTouch) > 0 {
			fmt.Fprintf(&b, "      %s\n", styleDim.Render("don't touch: "+strings.Join(a.DontTouch, ", ")))
		}
	}
	if len(p.RiskNotes) > 0 {
		b.WriteByte('\n')
		b.WriteString(styleDim.Render("Notes from exploration:") + "\n")
		for _, n := range p.RiskNotes {
			for _, line := range splitSentences(strings.TrimSpace(n)) {
				fmt.Fprintf(&b, "  %s\n", styleDim.Render("· "+line))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// isAlreadyDoneHeadline reports whether the lowercased summary
// genuinely says "the work is already done" — vs accidentally
// containing the substring "implemented" / "done" while saying
// the opposite ("no prior fix has been implemented", "not yet
// done", "this isn't implemented").
//
// The May-5 leak: model returned "No prior spinner animation
// fix has been implemented" → old substring check matched
// "implemented" → headline rendered "✓ Already done" which
// reads as the OPPOSITE of what the model meant. This function
// is the regression guard.
//
// Negation tokens scanned (case-insensitive, leading-edge or
// adjacent to the trigger word): "no ", "not ", "n't ",
// "no prior", "no existing", "haven't", "hasn't", "isn't",
// "wasn't", "never", "nothing has". When any present near a
// trigger ("already" / "implemented" / "done"), the summary
// is rejected — the model is saying the work HASN'T been done.
func isAlreadyDoneHeadline(low string) bool {
	triggers := []string{"already", "implemented", "done"}
	hit := false
	for _, t := range triggers {
		if strings.Contains(low, t) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	// Negation check: if the summary opens with a negation
	// or contains negation tokens, the trigger is being
	// negated, not affirmed.
	negations := []string{
		"no prior", "no existing", "no fix", "no implementation",
		"not yet", "not implemented", "not done", "not been",
		"haven't", "hasn't", "isn't", "wasn't", "weren't",
		"never been", "nothing has been", "nothing was",
	}
	for _, n := range negations {
		if strings.Contains(low, n) {
			return false
		}
	}
	// Catch leading "no " / "not " specifically (don't false-
	// positive on "no other" / "another" / "notable").
	trimmed := strings.TrimSpace(low)
	if strings.HasPrefix(trimmed, "no ") || strings.HasPrefix(trimmed, "not ") {
		return false
	}
	return true
}

// formatEmptyPlan renders the "no agents, nothing to do" case as
// a clear human-readable conclusion rather than a "Plan: 0 agent(s)"
// stub. Three sub-cases based on the summary text:
//
//   - "already implemented" / "already done" → "✓ Already done"
//   - "too vague" / "can't plan" / explicit can't        → "Can't plan this"
//   - anything else                                       → fallback "Nothing to plan"
//
// Risk notes (the evidence the model collected) render below
// the headline so the user can verify the conclusion. The
// "[go / cancel / type feedback]" line is omitted — there's no
// plan to "go" on.
func formatEmptyPlan(p *planner.WorkPlan) string {
	var b strings.Builder

	summary := strings.TrimSpace(p.Summary)
	low := strings.ToLower(summary)

	headline := ""
	subline := summary
	doubt := riskNotesHaveDoubt(p.RiskNotes)
	switch {
	case isAlreadyDoneHeadline(low) && doubt:
		// Round-18 dogfood: the planner returned "already
		// implemented" while ALSO emitting a risk_note saying
		// "the bug may be in renderPlanMenu". The audit reprompt
		// catches that and downgrades, but if reprompt retries
		// exhaust the chain demotes to planAcceptAsEmpty and the
		// plan-with-doubts reaches us anyway. Surface the
		// uncertainty in the headline so the user doesn't read
		// "Already done" and approve without scanning the bullets.
		headline = "⚠ Possibly already done — verify before assuming"
	case isAlreadyDoneHeadline(low):
		// "Already done" framing fits both the literal
		// "already implemented" case AND the broader
		// "answer was in the auto-injected overview, nothing
		// further to do" case (e.g. gpt-4o's "already
		// detailed in the project overview" phrasing).
		headline = "✓ Already done — nothing to do"
	case strings.HasPrefix(low, "planner failed"):
		// Planner exhausted reprompts and still couldn't produce
		// a usable plan. Surface this distinctly from "already
		// done" / "too vague" — the user needs to know the
		// system failed, not that the task is already complete.
		// The summary itself carries the suggestion (re-run,
		// swap model, check log) and renders as the subline.
		headline = "✗ Planner failed — no plan produced"
	case strings.Contains(low, "too vague") || strings.Contains(low, "can't plan") || strings.Contains(low, "cannot plan") || strings.Contains(low, "no concrete"):
		headline = "Couldn't plan this — too vague"
	default:
		// Question-style requests (e.g. "what's here?") that
		// the model answered directly with summary + notes
		// land here. Don't call them "Nothing to plan" — the
		// model DID answer; there was just no work to do.
		// Default to the answered framing so the user reads
		// the summary as the response, not as a failure.
		headline = "Answered"
	}

	// Headline gets the planner-banner color (cyan + bold) so it
	// reads as the conclusion at a glance.
	fmt.Fprintf(&b, "%s\n", stylePlannerBanner.Render(headline))
	if subline != "" && !strings.EqualFold(subline, headline) {
		fmt.Fprintf(&b, "  %s\n", styleDim.Render(subline))
	}

	if len(p.RiskNotes) > 0 {
		b.WriteByte('\n')
		for _, n := range p.RiskNotes {
			for _, line := range splitSentences(strings.TrimSpace(n)) {
				fmt.Fprintf(&b, "  %s\n", styleBody.Render("· "+line))
			}
		}
	}
	// No "[go / cancel]" line — there's no plan to act on.
	// The user can re-ask the planner with new framing if they
	// disagree with the conclusion.
	return b.String()
}

// splitSentences breaks a paragraph into one item per element.
// Recognizes two boundary patterns:
//
//   - Sentence end: ". " (period + space) followed by a capital
//     letter or backtick. Excludes intra-sentence periods like
//     "v1.0 release".
//   - Inline numbered list: "(N) " where N is one or more digits.
//     Models write "Create three files: (1) ..., (2) ..., (3) ..."
//     as one sentence; without splitting on the markers the user
//     gets a wall of text. Each "(N)" lands on its own line.
//
// Returns the input as a single-element slice when no boundary is
// found.
func splitSentences(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	i := 0
	for i < len(s) {
		// Inline-list boundary: "(N) " — open paren, one or
		// more digits, close paren, space.
		if s[i] == '(' && i+2 < len(s) && isDigit(s[i+1]) {
			j := i + 1
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			if j < len(s) && s[j] == ')' && j+1 < len(s) && s[j+1] == ' ' && i > start {
				prev := strings.TrimRight(strings.TrimSpace(s[start:i]), ":,")
				if prev != "" {
					out = append(out, prev)
				}
				start = i
			}
		}
		// Sentence-end boundary: ". " followed by a capital or backtick.
		if i < len(s)-2 && s[i] == '.' && s[i+1] == ' ' {
			next := s[i+2]
			if (next >= 'A' && next <= 'Z') || next == '`' {
				out = append(out, strings.TrimSpace(s[start:i+1]))
				start = i + 2
				i += 2
				continue
			}
		}
		i++
	}
	tail := strings.TrimSpace(s[start:])
	if tail != "" {
		out = append(out, tail)
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// stripPlannerPrefix avoids double-prefixing in the REPL. Errors
// returned by planner.Plan / Replan already start with "planner: "
// (either ErrTooVague's literal or fmt.Errorf("planner: ...")), so
// re-prepending in the REPL renders "planner: planner: ...". Strip
// once if present; leave anything else untouched.
func stripPlannerPrefix(s string) string {
	const p = "planner: "
	if strings.HasPrefix(s, p) {
		return s[len(p):]
	}
	return s
}

// formatExecuteResult renders the final orchestrator output.
func formatExecuteResult(res *orchestrator.Result, err error) string {
	if err != nil {
		return styleError.Render("orchestrator: " + err.Error())
	}
	if res == nil {
		return styleError.Render("(no result)")
	}
	var b strings.Builder
	// 0/0/0 with ≥1 agent that actually ran means every spawned
	// agent terminated without producing observable changes. The
	// raw counts read as quiet success ("nothing failed") but the
	// real situation is "nothing landed." Surface that explicitly
	// before the count line so the user doesn't interpret it as
	// completed work. 2026-05-25 dogfood pinned this: a 5-turn
	// executor run on the kai-desktop redesign produced 0/0/0 and
	// the user reasonably read it as "succeeded but found nothing
	// to do"; the runlog showed the executor died mid-task
	// (DeepSeek-V4-Pro emitted a text-only turn with finish=tool_use
	// but no tool_call block, then 0/0 tokens). Pointing the user
	// at the per-agent runlog makes the diagnosis reachable.
	if res.AutoPromoted == 0 && res.Held == 0 && res.Failed == 0 && len(res.Runs) > 0 {
		fmt.Fprintf(&b, "%s\n",
			styleError.Render(fmt.Sprintf(
				"⚠ %d agent(s) ran but produced no edits — see per-agent runlog for what happened",
				len(res.Runs))))
	}
	// Derive counts from each run's FINAL verdict: the verify pass can apply
	// remaining edits and auto-promote a previously-held change AFTER
	// res.AutoPromoted/Held/Failed were tallied, leaving res.Held stale (it
	// showed "1 held + kai gate approve" while the verdict said "applied" and
	// /gate showed nothing). Recomputing keeps summary, per-run lines, and the
	// gate in agreement.
	autoPromoted, held, failed := 0, 0, 0
	for _, r := range res.Runs {
		switch {
		case r.ExitErr != nil || r.IntegrateErr != nil:
			failed++
		case r.Verdict == nil:
		case r.Verdict.Verdict == string(safetygate.Auto):
			autoPromoted++
		default:
			held++
		}
	}
	fmt.Fprintf(&b, "Done: %d auto-promoted, %d held, %d failed\n",
		autoPromoted, held, failed)
	for _, r := range res.Runs {
		// Compact one-line summary of which files landed; full list
		// in `git status` / `kai status` if the user wants more.
		filesNote := ""
		switch n := len(r.ChangedPaths); {
		case n == 1:
			filesNote = " — " + r.ChangedPaths[0]
		case n > 1:
			filesNote = fmt.Sprintf(" — %d files (incl. %s)", n, r.ChangedPaths[0])
		}

		switch {
		case r.ExitErr != nil:
			fmt.Fprintf(&b, "  • %s — %s\n    %s\n", r.Task.Name,
				styleError.Render("agent error: "+r.ExitErr.Error()),
				styleDim.Render("logs: "+r.SpawnDir+"/.kai/agent.log"))
		case r.IntegrateErr != nil:
			fmt.Fprintf(&b, "  • %s — %s\n    %s\n", r.Task.Name,
				styleError.Render("integrate error: "+r.IntegrateErr.Error()),
				styleDim.Render("logs: "+r.SpawnDir+"/.kai/agent.log"))
		case r.Verdict == nil:
			// Agent ran but produced no observable changes.
			fmt.Fprintf(&b, "  • %s — %s\n", r.Task.Name,
				styleDim.Render("no changes"))
			if r.IntegrateNote != "" {
				fmt.Fprintf(&b, "    %s\n", styleWarn.Render(r.IntegrateNote))
			}
		case r.Verdict.Verdict == string(safetygate.Auto):
			fmt.Fprintf(&b, "  • %s — applied to your repo (snap.latest advanced)%s\n",
				r.Task.Name, filesNote)
		case r.Verdict.Verdict == string(safetygate.Block):
			fmt.Fprintf(&b, "  • %s — %s (blast %d)%s\n", r.Task.Name,
				styleError.Render("BLOCKED — kept in working tree"), r.Verdict.BlastRadius, filesNote)
			for _, reason := range r.Verdict.Reasons {
				fmt.Fprintf(&b, "    %s\n", styleWarn.Render(reason))
			}
		default:
			fmt.Fprintf(&b, "  • %s — %s (blast %d)%s\n", r.Task.Name,
				styleWarn.Render("HELD for review"), r.Verdict.BlastRadius, filesNote)
			// Surface the gate's reasons (blast threshold, protected
			// path, plan-coverage gap, etc.) so the user can see WHY
			// it was held — not just that it was. Round-16 dogfood:
			// plan-coverage flagged a 2/7-signals match but the
			// trailer only showed "blast 4" so the user approved a
			// barely-started edit thinking it was a small clean fix.
			for _, reason := range r.Verdict.Reasons {
				fmt.Fprintf(&b, "    %s\n", styleWarn.Render(reason))
			}
		}

		// Verify summary — surfaced one indent level deeper than the
		// per-agent outcome line, so it visually anchors to the agent
		// it followed. Skipped when verify didn't run for this agent.
		if r.VerifySummary != "" {
			fmt.Fprintf(&b, "    %s\n", styleDim.Render(r.VerifySummary))
		}
		if r.VerifyErr != nil {
			fmt.Fprintf(&b, "    %s\n",
				styleError.Render("verify error: "+r.VerifyErr.Error()))
		}
	}
	if held > 0 {
		b.WriteString(styleDim.Render("Held changes are in your working tree. `kai gate list` to inspect, `kai gate approve <id>` to publish."))
	}
	return b.String()
}

// wrapText breaks a paragraph into lines no longer than width
// runes, breaking on word boundaries. Used by formatPlan's
// Diagnosis/Approach renderers so a long sentence wraps with
// a 2-space hanging indent instead of running off the side
// of the terminal.
//
// Conservative implementation: word-by-word, no hyphenation,
// no markdown awareness. Existing newlines in the input are
// preserved (each line is wrapped independently).
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteString("\n  ")
		}
		col := 0
		for j, word := range strings.Fields(line) {
			if j == 0 {
				out.WriteString(word)
				col = len(word)
				continue
			}
			if col+1+len(word) > width {
				out.WriteString("\n  ")
				out.WriteString(word)
				col = len(word)
			} else {
				out.WriteByte(' ')
				out.WriteString(word)
				col += 1 + len(word)
			}
		}
	}
	return out.String()
}


// riskNotesHaveDoubt reports whether any risk note contains a phrase
// signaling the planner is uncertain about its own "already done"
// verdict. Mirror of internal/planner.auditHasDoubtPhrase — the
// audit prevents most doubt-laden verdicts from reaching the TUI,
// but when the verify reprompt has already been tried (or failed)
// the chain demotes back to planAcceptAsEmpty and the original
// plan-with-doubts arrives here. We downgrade the headline so the
// user actually reads the bullet points instead of trusting a
// confident-looking "Already done".
//
// Phrase list kept in sync with the planner package by convention,
// not by import (views imports planner already for types; the
// audit helper is unexported so duplicating the small list here is
// the cheapest path).
func riskNotesHaveDoubt(notes []string) bool {
	for _, n := range notes {
		low := strings.ToLower(n)
		for _, phrase := range []string{
			"may be in",
			"may not be",
			"might be",
			"could be a bug",
			"should investigate",
			"would be needed",
			"would need to be",
			"if the user confirms",
			"follow-up investigation",
			"need to verify",
			"not yet tested",
			"may have been added very recently",
		} {
			if strings.Contains(low, phrase) {
				return true
			}
		}
	}
	return false
}
