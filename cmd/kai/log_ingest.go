package main

// `kai log ingest` — parse an agent session transcript (Claude Code or Codex)
// into a structured loop record at <repo>/.kai/loops/<session>.json. This is
// the machine-readable counterpart to the human Markdown logger; it powers the
// Kai desktop Overview / Loops views.
//
// Two modes:
//   • hook mode: a Stop/SessionEnd event JSON arrives on stdin (transcript_path,
//     cwd, session_id, hook_event_name). Registered by `kai init`.
//   • CLI mode:  `kai log ingest --transcript <path> [--cwd <dir>] [--source ...]
//     [--status ...]` — used for backfill.
//
// It is gated implicitly: a loop record is only written when the session's cwd
// is inside a kai repo (nearest .kai ancestor). Always silent, never errors —
// it must never disrupt a session.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	ingestTranscript string
	ingestCwd        string
	ingestSource     string
	ingestStatus     string
)

var logIngestCmd = &cobra.Command{
	Use:           "ingest",
	Short:         "Ingest an agent session transcript into a loop record (.kai/loops/<session>.json)",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLogIngest,
}

func init() {
	logIngestCmd.Flags().StringVar(&ingestTranscript, "transcript", "", "path to the session JSONL (CLI/backfill mode)")
	logIngestCmd.Flags().StringVar(&ingestCwd, "cwd", "", "session working dir (defaults to the transcript-encoded path or $PWD)")
	logIngestCmd.Flags().StringVar(&ingestSource, "source", "claude-code", "claude-code | codex")
	logIngestCmd.Flags().StringVar(&ingestStatus, "status", "", "override the computed status (e.g. done)")
	logCmd.AddCommand(logIngestCmd)
}

// ---- loop record schema (see ~/projects/kai/logging/INGEST.md) ----

type loopDecision struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}
type loopPrompt struct {
	Ts   string `json:"ts"`
	Text string `json:"text"`
}
type evidenceCheck struct {
	Cmd    string `json:"cmd"`
	Kind   string `json:"kind"`   // test | build | typecheck | lint
	Status string `json:"status"` // passed | failed | ran
}
type loopEvidence struct {
	ByKind map[string]int  `json:"byKind"` // command kind -> count (all Bash)
	Checks []evidenceCheck `json:"checks"` // verification commands, in order
}

// segCmd is one Bash command run within a work segment, with its real exit.
type segCmd struct {
	Cmd    string `json:"cmd"`
	Kind   string `json:"kind,omitempty"`
	Status string `json:"status,omitempty"` // passed | failed | ran
}

// loopEvent is one row of the session timeline: either a user "prompt" or a
// "work" summary of everything the agent did between two prompts. The full
// file and command lists let the UI expand a segment.
type loopEvent struct {
	Kind      string   `json:"kind"` // prompt | work
	Ts        string   `json:"ts,omitempty"`
	Text      string   `json:"text,omitempty"` // prompt text
	Files     int      `json:"files,omitempty"`
	FileList  []string `json:"fileList,omitempty"`
	Commands  int      `json:"commands,omitempty"`
	CmdList   []segCmd `json:"cmdList,omitempty"`
	Passed    int      `json:"passed,omitempty"` // verification checks only
	Failed    int      `json:"failed,omitempty"`
	Decisions int      `json:"decisions,omitempty"`
}
type loopRecord struct {
	SchemaVersion int            `json:"schemaVersion"`
	ID            string         `json:"id"`
	Source        string         `json:"source"`
	Repo          string         `json:"repo"`
	Cwd           string         `json:"cwd"`
	Goal          string         `json:"goal"`
	Status        string         `json:"status"`
	StartedAt     string         `json:"startedAt"`
	UpdatedAt     string         `json:"updatedAt"`
	DurationMs    int64          `json:"durationMs"`
	PromptCount   int            `json:"promptCount"`
	TurnCount     int            `json:"turnCount"`
	Tools         map[string]int `json:"tools"`
	ToolTotal     int            `json:"toolTotal"`
	FilesChanged  []string       `json:"filesChanged"`
	Decisions     []loopDecision `json:"decisions"`
	Prompts       []loopPrompt   `json:"prompts"`
	Evidence      loopEvidence   `json:"evidence"`
	Timeline      []loopEvent    `json:"timeline"`
	LastActivity  string         `json:"lastActivity"`
}

// ---- transcript line shapes (only the fields we need) ----

type tLine struct {
	Type         string          `json:"type"`
	Timestamp    string          `json:"timestamp"`
	PromptSource json.RawMessage `json:"promptSource"`
	Message      struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}
type tBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Text      string          `json:"text"`
	IsError   bool            `json:"is_error"` // authoritative: command exited non-zero
}
type tInput struct {
	FilePath  string `json:"file_path"`
	Command   string `json:"command"`
	Questions []struct {
		Question string `json:"question"`
		Header   string `json:"header"`
	} `json:"questions"`
}

var liAnswerRe = regexp.MustCompile(`=\s*"([^"]+)"`)

// Claude Code injects several non-prompt messages into the conversation as
// "user" turns — some even carry a promptSource (e.g. task-notification). We
// strip system-reminder blocks from real prompts and drop messages that are
// purely a Claude/harness XML envelope so they never become a loop's goal.
var (
	reSystemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	reSystemEnvelope = regexp.MustCompile(`^<(task-notification|user-prompt-submit-hook|local-command-[a-z]+|command-name|command-message|command-args|bash-input|bash-stdout|bash-stderr)\b`)
)

// cleanPrompt returns the real user text, or "" if the message is system-injected.
func cleanPrompt(text string) string {
	t := strings.TrimSpace(reSystemReminder.ReplaceAllString(text, ""))
	if t == "" || reSystemEnvelope.MatchString(t) {
		return ""
	}
	return t
}

// Bash command classification (heuristic, lowercased input). Order matters:
// test/typecheck/lint are checked before the broader build/run patterns.
var (
	reTest      = regexp.MustCompile(`vitest|jest|pytest|go test|gotestsum|cargo test|rspec|mocha|playwright|npm (run )?test|yarn test|pnpm (run )?test`)
	reTypecheck = regexp.MustCompile(`\btsc\b|type-?check|\bmypy\b|\bflow check\b`)
	reLint      = regexp.MustCompile(`eslint|\blint\b|\bruff\b|golangci|clippy|prettier|go vet`)
	reBuild     = regexp.MustCompile(`vite build|webpack|go build|cargo build|npm run build|yarn build|pnpm build|\bmake\b`)
	reInstall   = regexp.MustCompile(`npm i\b|npm install|pnpm i\b|pnpm install|yarn add|yarn install|pip install|go mod|cargo add|brew install`)
	reGit       = regexp.MustCompile(`^\s*git\b`)
	reRun       = regexp.MustCompile(`npm run|npm start|electron|\bnode \b|\bvite\b|serve\b|\bdev\b`)
)

var reLeadingCd = regexp.MustCompile(`^\s*cd\s+\S+\s*(&&|;)\s*`)
var inspectTools = map[string]bool{
	"ls": true, "grep": true, "cat": true, "find": true, "echo": true, "head": true,
	"tail": true, "which": true, "pgrep": true, "wc": true, "sed": true, "awk": true,
	"rg": true, "printf": true, "true": true, "sleep": true, "cp": true, "mv": true, "rm": true,
}

// firstCmdWord returns the leading command word, skipping a `cd <dir> &&` prefix.
func firstCmdWord(c string) string {
	s := c
	for reLeadingCd.MatchString(s) {
		s = reLeadingCd.ReplaceAllString(s, "")
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\n"); i > 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

func classifyCmd(c string) string {
	// A command that starts with a pure inspect/file tool is never a
	// verification check, even if it mentions a test runner (e.g. grep playwright).
	if inspectTools[firstCmdWord(c)] {
		return "other"
	}
	s := strings.ToLower(c)
	switch {
	case reTest.MatchString(s):
		return "test"
	case reTypecheck.MatchString(s):
		return "typecheck"
	case reLint.MatchString(s):
		return "lint"
	case reBuild.MatchString(s):
		return "build"
	case reInstall.MatchString(s):
		return "install"
	case reGit.MatchString(s):
		return "git"
	case reRun.MatchString(s):
		return "run"
	default:
		return "other"
	}
}

func isVerificationKind(k string) bool {
	return k == "test" || k == "build" || k == "typecheck" || k == "lint"
}

func liTrunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func runLogIngest(cmd *cobra.Command, args []string) error {
	defer func() { _ = recover() }() // never disrupt a session

	source := ingestSource
	if source == "" {
		source = "claude-code"
	}
	statusOverride := ingestStatus
	var transcript, cwd string

	if ingestTranscript != "" {
		transcript = ingestTranscript
		cwd = ingestCwd
		if cwd == "" {
			// derive cwd from the encoded project dir (…/-Users-foo-bar/sess.jsonl)
			enc := filepath.Base(filepath.Dir(transcript))
			if strings.HasPrefix(enc, "-") {
				cwd = strings.ReplaceAll(enc, "-", "/")
			} else {
				cwd, _ = os.Getwd()
			}
		}
	} else {
		data, err := io.ReadAll(os.Stdin)
		if err != nil || len(data) == 0 {
			return nil
		}
		var ev struct {
			HookEventName  string `json:"hook_event_name"`
			Cwd            string `json:"cwd"`
			TranscriptPath string `json:"transcript_path"`
			SessionID      string `json:"session_id"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		if ev.HookEventName != "Stop" && ev.HookEventName != "SessionEnd" {
			return nil
		}
		cwd = ev.Cwd
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		transcript = ev.TranscriptPath
		if ev.HookEventName == "SessionEnd" {
			statusOverride = "done"
		}
	}

	if transcript == "" {
		return nil
	}
	if _, err := os.Stat(transcript); err != nil {
		return nil
	}
	rec := buildLoopRecord(transcript, cwd, source, statusOverride)
	if rec == nil {
		return nil
	}
	writeLoopRecord(rec)
	return nil
}

func buildLoopRecord(transcript, cwd, source, statusOverride string) *loopRecord {
	repoRoot := liFindRepoRoot(cwd)
	if repoRoot == "" {
		return nil
	}
	f, err := os.Open(transcript)
	if err != nil {
		return nil
	}
	defer f.Close()

	rec := &loopRecord{
		SchemaVersion: 1,
		ID:            strings.TrimSuffix(filepath.Base(transcript), ".jsonl"),
		Source:        source,
		Repo:          filepath.Base(repoRoot),
		Cwd:           cwd,
		Status:        "idle",
		Tools:         map[string]int{},
		FilesChanged:  []string{},
		Decisions:     []loopDecision{},
		Prompts:       []loopPrompt{},
		Evidence:      loopEvidence{ByKind: map[string]int{}, Checks: []evidenceCheck{}},
	}
	files := map[string]bool{}
	askQuestions := map[string][]string{} // AskUserQuestion tool_use id -> clean questions
	checkIdx := map[string]int{}          // Bash tool_use id -> index into Evidence.Checks

	// Timeline segment accumulator: the work done between two prompts.
	var segFiles map[string]bool
	var segFileList []string
	var segCmds []segCmd
	var segCmdIdx map[string]int // Bash tool_use id -> index in segCmds (this segment)
	var segDec int
	var segActive bool
	resetSeg := func() {
		segFiles = map[string]bool{}
		segFileList = nil
		segCmds = nil
		segCmdIdx = map[string]int{}
		segDec, segActive = 0, false
	}
	resetSeg()
	flushSeg := func() {
		if !segActive {
			return
		}
		fl, cl := segFileList, segCmds
		if len(fl) > 60 {
			fl = fl[:60]
		}
		if len(cl) > 60 {
			cl = cl[:60]
		}
		passed, failed := 0, 0
		for _, c := range segCmds {
			if isVerificationKind(c.Kind) {
				if c.Status == "passed" {
					passed++
				} else if c.Status == "failed" {
					failed++
				}
			}
		}
		rec.Timeline = append(rec.Timeline, loopEvent{
			Kind: "work", Files: len(segFiles), FileList: fl,
			Commands: len(segCmds), CmdList: cl, Passed: passed, Failed: failed, Decisions: segDec,
		})
		resetSeg()
	}

	// repo-relative, or "" when outside the repo (a per-repo record only tracks
	// files within its own repo).
	rel := func(p string) string {
		if p == "" {
			return ""
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, abs)
		}
		r, err := filepath.Rel(repoRoot, abs)
		if err != nil || strings.HasPrefix(r, "..") || filepath.IsAbs(r) {
			return ""
		}
		return r
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024) // assistant lines can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l tLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if l.Timestamp != "" {
			if rec.StartedAt == "" {
				rec.StartedAt = l.Timestamp
			}
			rec.UpdatedAt = l.Timestamp
		}

		switch l.Type {
		case "user":
			var s string
			if json.Unmarshal(l.Message.Content, &s) == nil && liHasJSON(l.PromptSource) {
				t := cleanPrompt(s)
				if t != "" {
					flushSeg() // close out the work done since the previous prompt
					rec.Timeline = append(rec.Timeline, loopEvent{Kind: "prompt", Ts: l.Timestamp, Text: t})
					if rec.Goal == "" {
						rec.Goal = t
					}
					rec.Prompts = append(rec.Prompts, loopPrompt{Ts: l.Timestamp, Text: t})
					rec.PromptCount++
					rec.LastActivity = "Prompt"
				}
			} else {
				var blocks []tBlock
				if json.Unmarshal(l.Message.Content, &blocks) == nil {
					for _, b := range blocks {
						if b.Type != "tool_result" {
							continue
						}
						if qs, ok := askQuestions[b.ToolUseID]; ok {
							answers := liAnswerRe.FindAllStringSubmatch(liToolResultText(b), -1)
							for i, q := range qs {
								a := ""
								if i < len(answers) {
									a = answers[i][1]
								}
								rec.Decisions = append(rec.Decisions, loopDecision{Question: q, Answer: a})
							}
							segDec += len(qs)
							segActive = true
						}
						if idx, ok := checkIdx[b.ToolUseID]; ok {
							if b.IsError {
								rec.Evidence.Checks[idx].Status = "failed"
							} else {
								rec.Evidence.Checks[idx].Status = "passed"
							}
						}
						if i, ok := segCmdIdx[b.ToolUseID]; ok && i < len(segCmds) {
							if b.IsError {
								segCmds[i].Status = "failed"
							} else {
								segCmds[i].Status = "passed"
							}
						}
					}
				}
			}
		case "assistant":
			rec.TurnCount++
			var blocks []tBlock
			if json.Unmarshal(l.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type != "tool_use" {
						continue
					}
					n := b.Name
					if n == "" {
						n = "tool"
					}
					rec.Tools[n]++
					rec.ToolTotal++
					var inp tInput
					_ = json.Unmarshal(b.Input, &inp)
					switch {
					case (n == "Write" || n == "Edit" || n == "NotebookEdit") && inp.FilePath != "":
						if rp := rel(inp.FilePath); rp != "" {
							files[rp] = true
							rec.LastActivity = n + " " + rp
							if !segFiles[rp] {
								segFiles[rp] = true
								segFileList = append(segFileList, rp)
							}
							segActive = true
						}
					case n == "Bash" && inp.Command != "":
						rec.LastActivity = "Bash: " + liTrunc(inp.Command, 48)
						segActive = true
						kind := classifyCmd(inp.Command)
						rec.Evidence.ByKind[kind]++
						clean := liTrunc(strings.Join(strings.Fields(inp.Command), " "), 100)
						segIdx := len(segCmds)
						segCmds = append(segCmds, segCmd{Cmd: clean, Kind: kind, Status: "ran"})
						if b.ID != "" {
							segCmdIdx[b.ID] = segIdx
						}
						if isVerificationKind(kind) && b.ID != "" {
							checkIdx[b.ID] = len(rec.Evidence.Checks)
							rec.Evidence.Checks = append(rec.Evidence.Checks, evidenceCheck{
								Cmd: clean, Kind: kind, Status: "ran",
							})
						}
					case n == "AskUserQuestion":
						var qs []string
						for _, q := range inp.Questions {
							switch {
							case q.Question != "":
								qs = append(qs, q.Question)
							case q.Header != "":
								qs = append(qs, q.Header)
							default:
								qs = append(qs, "(question)")
							}
						}
						if b.ID != "" {
							askQuestions[b.ID] = qs
						}
					}
				}
			}
		}
	}

	flushSeg() // close out the final work segment

	for fl := range files {
		rec.FilesChanged = append(rec.FilesChanged, fl)
	}
	sort.Strings(rec.FilesChanged)
	if len(rec.Prompts) > 50 {
		rec.Prompts = rec.Prompts[len(rec.Prompts)-50:]
	}
	if len(rec.Evidence.Checks) > 40 {
		rec.Evidence.Checks = rec.Evidence.Checks[len(rec.Evidence.Checks)-40:]
	}
	if len(rec.Timeline) > 120 {
		rec.Timeline = rec.Timeline[len(rec.Timeline)-120:]
	}

	start := liParseTime(rec.StartedAt)
	end := liParseTime(rec.UpdatedAt)
	if !start.IsZero() && !end.IsZero() {
		rec.DurationMs = end.Sub(start).Milliseconds()
	}
	switch {
	case statusOverride != "":
		rec.Status = statusOverride
	case !end.IsZero() && time.Since(end) < 10*time.Minute:
		rec.Status = "active"
	default:
		rec.Status = "idle"
	}
	return rec
}

func writeLoopRecord(rec *loopRecord) {
	repoRoot := liFindRepoRoot(rec.Cwd)
	if repoRoot == "" {
		return
	}
	dir := filepath.Join(repoRoot, ".kai", "loops")
	if os.MkdirAll(dir, 0755) != nil {
		return
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, rec.ID+".json"), data, 0644)
}

// liFindRepoRoot returns the project root for session-ingest artifacts: the
// nearest ancestor of start holding a repo marker (.kai or .git). The user's
// global ~/.kai config dir is explicitly NOT a repo root — otherwise every
// session run under $HOME would misfile its loop record (and self-healed
// hooks) into the home directory instead of the actual project. Accepting
// .git as well means a kai-init'd git repo ingests even before it grows a
// local .kai (these are gated separately by the hook only being installed
// where the user opted in).
func liFindRepoRoot(start string) string {
	home, _ := os.UserHomeDir()
	dir := start
	for dir != "" && dir != "/" {
		if dir != home {
			if fi, err := os.Stat(filepath.Join(dir, ".kai")); err == nil && fi.IsDir() {
				return dir
			}
			if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func liHasJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

func liToolResultText(b tBlock) string {
	if b.Text != "" {
		return b.Text
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var arr []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(b.Content, &arr) == nil {
		var sb strings.Builder
		for _, x := range arr {
			sb.WriteString(x.Text)
		}
		return sb.String()
	}
	return ""
}

func liParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// ---- agent-hook installation (called from `kai init`) ----

// stableIngestCommand is the hook command string to bake into agent settings:
// `"<kai>" log ingest`, using a durable binary path (see stableKaiPath).
func stableIngestCommand() string {
	return fmt.Sprintf(`"%s" log ingest`, stableKaiPath())
}

// stableKaiPath returns a durable path to the kai binary for embedding in
// agent hooks. os.Executable() alone is unsafe here: during a Claude Code run
// kai often executes from an ephemeral copy in a temp scratchpad that gets
// wiped between sessions, and that dead path was silently baked into the hook.
// Preference order: the canonical install (~/.kai/bin/kai), then the running
// binary if it's not in a temp dir, then a bare "kai" (PATH lookup).
func stableKaiPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".kai", "bin", "kai"); isExecutableFile(p) {
			return p
		}
	}
	if exe, err := os.Executable(); err == nil && !isTempPath(exe) {
		return exe
	}
	return "kai"
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0111 != 0
}

// isTempPath reports whether p lives under a temp directory (throwaway).
func isTempPath(p string) bool {
	p = filepath.Clean(p)
	roots := []string{filepath.Clean(os.TempDir()), "/tmp", "/private/tmp", "/var/folders"}
	for _, r := range roots {
		if r != "" && (p == r || strings.HasPrefix(p, r+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// writeAgentHooks registers (and self-heals) the session-ingest hook with
// Claude Code and Codex, project-local, so every agent session in this repo
// updates .kai/loops/. Reconcile-not-clobber and idempotent: it prunes stale
// ingest entries (dead temp paths, old installs) and ensures the current one
// is present. Best-effort — never fails the caller.
func writeAgentHooks(repoRoot string) {
	command := stableIngestCommand()
	// These files embed an absolute, machine-specific path — they are purely
	// local tooling and must never be committed. gitignore them first so they
	// can't be swept into a commit (e.g. the headless autofix loop once
	// shipped a PR whose entire diff was .codex/hooks.json).
	ensureGitignored(repoRoot, ".claude/settings.local.json", ".codex/hooks.json")
	reconcileHookEvents(filepath.Join(repoRoot, ".claude", "settings.local.json"), []string{"Stop", "SessionEnd"}, command)
	reconcileHookEvents(filepath.Join(repoRoot, ".codex", "hooks.json"), []string{"Stop"}, command) // Codex has no SessionEnd
}

// selfHealAgentHooks re-asserts the ingest hook in the kai repo containing the
// current working dir, on every kai invocation. It heals a stale binary path
// or a removed entry with no user action. No-op outside a kai repo; writes
// only when reconcile finds something to fix.
func selfHealAgentHooks() {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	root := liFindRepoRoot(wd)
	if root == "" {
		return
	}
	// Repair-only: fix an existing ingest hook whose binary path went stale
	// (e.g. baked to a temp copy). Don't create a fresh install where the user
	// never opted in — that's what `kai init` / `kai doctor --fix` are for.
	want := stableIngestCommand()
	files := []struct {
		path   string
		events []string
	}{
		{filepath.Join(root, ".claude", "settings.local.json"), []string{"Stop", "SessionEnd"}},
		{filepath.Join(root, ".codex", "hooks.json"), []string{"Stop"}},
	}
	for _, f := range files {
		if len(ingestHookCommands(f.path, f.events)) > 0 {
			reconcileHookEvents(f.path, f.events, want)
		}
	}
}

// checkAgentIngest is a `kai doctor` probe: it verifies the Claude/Codex
// session-ingest hook is installed in this repo, points to a live binary, and
// is actually producing loop records. With `fix` it re-asserts the hook first
// (prune stale + install current). Mirrors runDoctor's ok/warn/bad prefixes.
func checkAgentIngest(ok, warn, bad string, fix bool) {
	root := liFindRepoRoot(mustGetwd())
	if root == "" {
		fmt.Printf("%s session-ingest: not a kai repo (skipped)\n", warn)
		return
	}
	if fix {
		writeAgentHooks(root)
	}

	want := stableIngestCommand()
	settings := filepath.Join(root, ".claude", "settings.local.json")
	installed := ingestHookCommands(settings, []string{"Stop", "SessionEnd"})

	// Check 1: hook installed?
	if len(installed) == 0 {
		fmt.Printf("%s session-ingest hook not installed — run 'kai doctor --fix' or 'kai init'\n", bad)
		return
	}
	fmt.Printf("%s session-ingest hook installed (.claude/settings.local.json)\n", ok)

	// Check 2: does every installed hook point to a live binary?
	allLive := true
	for _, c := range installed {
		bin := hookBinaryPath(c)
		if bin != "kai" && !isExecutableFile(bin) {
			allLive = false
			fmt.Printf("%s ingest hook points to a dead binary: %s\n", bad, bin)
		} else if c != want {
			fmt.Printf("%s ingest hook binary reachable but stale (want %s)\n", warn, want)
		}
	}
	if allLive {
		fmt.Printf("%s ingest binary reachable\n", ok)
	} else {
		fmt.Printf("%s run 'kai doctor --fix' to repoint the ingest hook to %s\n", warn, want)
	}

	// Check 3: is output actually being produced?
	loops := filepath.Join(root, ".kai", "loops")
	entries, _ := os.ReadDir(loops)
	n := 0
	var newest time.Time
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
			if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
	}
	if n > 0 {
		fmt.Printf("%s ingest producing records (%d loop(s), newest %s)\n", ok, n, newest.Format("2006-01-02 15:04"))
	} else {
		fmt.Printf("%s no loop records yet (appears after your next Claude/Codex session ends)\n", warn)
	}
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

// ingestHookCommands returns the command strings of kai ingest hooks found in
// the given settings file under any of `events`.
func ingestHookCommands(path string, events []string) []string {
	var out []string
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return out
	}
	hooks, _ := root["hooks"].(map[string]any)
	for _, ev := range events {
		arr, _ := hooks[ev].([]any)
		for _, g := range arr {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); isIngestHookCommand(c) {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

// hookBinaryPath extracts the binary path from a `"<path>" log ingest` (or
// `<path> log ingest`) command string.
func hookBinaryPath(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : 1+end]
		}
	}
	if fields := strings.Fields(cmd); len(fields) > 0 {
		return fields[0]
	}
	return cmd
}

// ensureGitignored appends any of entries not already present to the repo's
// .gitignore, under a labeled section. Idempotent (exact-line match) and
// best-effort: skips non-git repos and never fails the caller.
func ensureGitignored(repoRoot string, entries ...string) {
	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err != nil {
		return // not a git repo — nothing to ignore against
	}
	path := filepath.Join(repoRoot, ".gitignore")
	existing := map[string]bool{}
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			existing[strings.TrimSpace(line)] = true
		}
	}
	var missing []string
	for _, e := range entries {
		if !existing[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n# kai agent-session hooks (machine-local; do not commit)\n")
	for _, e := range missing {
		fmt.Fprintln(f, e)
	}
}

// reconcileHookEvents makes the ingest hook in `path` match `command` exactly,
// for each of `events`: it prunes any of OUR stale ingest entries (a kai
// `log ingest` command pointing somewhere other than `command` — e.g. a dead
// temp path or a prior install) and ensures the current one is present. It
// never touches non-ingest hooks. Writes only when something changed.
func reconcileHookEvents(path string, events []string, command string) {
	var root map[string]any
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &root)
	}
	if root == nil {
		root = map[string]any{}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	changed := false
	for _, ev := range events {
		arr, _ := hooks[ev].([]any)
		kept := make([]any, 0, len(arr))
		for _, g := range arr {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			live := make([]any, 0, len(hs))
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				c, _ := hm["command"].(string)
				if isIngestHookCommand(c) && c != command {
					changed = true // drop a stale ingest entry
					continue
				}
				live = append(live, h)
			}
			if len(live) == 0 {
				continue // group was entirely stale ingest hooks
			}
			gm["hooks"] = live
			kept = append(kept, gm)
		}
		if !hookCommandPresent(kept, command) {
			group := map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 30}},
			}
			kept = append(kept, group)
			changed = true
		}
		hooks[ev] = kept
	}
	if !changed {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0755) != nil {
		return
	}
	if data, err := json.MarshalIndent(root, "", "  "); err == nil {
		_ = os.WriteFile(path, data, 0644)
	}
}

// isIngestHookCommand reports whether a hook command is one of kai's own
// session-ingest hooks (any kai binary path + "log ingest"), so we can prune
// stale copies without disturbing unrelated hooks (TokenBar, prompt-logger…).
func isIngestHookCommand(cmd string) bool {
	return strings.Contains(cmd, "log ingest")
}

func hookCommandPresent(arr []any, command string) bool {
	for _, g := range arr {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); c == command {
				return true
			}
		}
	}
	return false
}
