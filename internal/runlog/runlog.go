// Package runlog persists a structured per-turn record of agent runs
// for debuggability. The artifact answers two questions that are
// otherwise invisible:
//
//  1. "Why did this turn cost N tokens?" — broken down by prompt
//     section (system, tools, message history, latest message),
//     including a per-section hash so the next-turn diff can name
//     which section invalidated the prompt cache.
//
//  2. "What did the agent actually do?" — every tool call with its
//     name, input bytes, output bytes, duration, and exit/error so
//     long runs are auditable after the fact.
//
// The design is "dump everything to disk in structured form" — the
// CLI viewer (`kai run last`, `kai run diff`) prints summaries from
// the artifact, but a developer can also grep / jq the JSON directly
// without going through any UI. No TUI surface depends on this.
//
// Artifacts live under <KaiDir>/runs/<sessionID>/<turn>.json. When
// KAI_DEBUG_RUNS=1 is set, the full request and response bodies are
// also written to <turn>.request.json / <turn>.response.json so the
// turn can be replayed verbatim. They're large (often >1MB on
// cache-heavy sessions) so they're off by default.
package runlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"kai/internal/agent/provider"
	"github.com/kaicontext/kai-engine/tools"
)

// Section is one measured piece of the prompt. Chars is the raw
// length of the serialized section; EstTokens is chars/4 — a coarse
// heuristic, fine for relative comparison across turns. Hash is the
// SHA-256 of the serialized section, prefix-truncated to 8 bytes (16
// hex chars) since we only ever compare it for equality.
type Section struct {
	Chars     int    `json:"chars"`
	EstTokens int    `json:"est_tokens"`
	Hash      string `json:"hash"`
}

// Sections holds all the measured prompt parts. Keys are stable so
// the diff viewer can report drift by name.
type Sections struct {
	System   Section `json:"system"`
	Tools    Section `json:"tools"`
	Messages Section `json:"messages"` // full message history
	Message  Section `json:"message"`  // latest single user/tool-result turn
}

// Usage mirrors provider.Response's billing-relevant fields, plus
// two derived percentages. Both matter and they tell different
// stories:
//
//   - ReusePct = cache_read / (input + cache_write + cache_read).
//     This is what you actually want — the fraction of the prompt
//     that was served from a previous turn's cache at read rate
//     (~10% of normal input cost). Healthy: 80%+ on multi-turn
//     sessions.
//
//   - CachedPct = (cache_write + cache_read) / total. This is
//     "fraction that touched the cache infrastructure," which is a
//     deceptive metric on its own: a turn that writes 10kB fresh
//     and reads 0 reports 99% "cached" but is the worst case for
//     cost (every byte paid the write premium AND nothing was
//     reused). Kept for completeness; ReusePct is the headline.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CachedPct                int `json:"cached_pct"`
	ReusePct                 int `json:"reuse_pct"`
}

// ToolCall captures one tool dispatched during this turn. InputBytes /
// OutputBytes are sizes of the JSON payloads, not token counts —
// they're cheap to measure and good enough to spot the
// "kai_callers returned 47kB of context" smell.
type ToolCall struct {
	Name        string `json:"name"`
	InputBytes  int    `json:"input_bytes"`
	OutputBytes int    `json:"output_bytes"`
	DurationMs  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
}

// Turn is the per-turn artifact written to disk. One file per turn;
// turn-level diffs work by reading two files.
type Turn struct {
	SchemaVersion  int        `json:"schema_version"`
	SessionID      string     `json:"session_id"`
	Turn           int        `json:"turn"`
	Timestamp      string     `json:"ts"` // RFC3339
	Model          string     `json:"model"`
	TaskName       string     `json:"task_name,omitempty"`
	DurationMs     int64      `json:"duration_ms"` // wall time from Send → response
	Sections       Sections   `json:"sections"`
	Usage          Usage      `json:"usage"`
	ToolCalls      []ToolCall `json:"tool_calls"`
	FinishReason   string     `json:"finish_reason,omitempty"`
	AssistantText  string     `json:"assistant_text,omitempty"`  // first ~600 chars of the model's prose, for grep-ability
	RequestPath    string     `json:"request_path,omitempty"`    // populated when KAI_DEBUG_RUNS=1
	ResponsePath   string     `json:"response_path,omitempty"`
}

const schemaVersion = 1

// Recorder accumulates turn data inside the runner. The runner calls
// Begin at the start of each model call, AddToolCall after each tool
// dispatch, and End once the response lands; End writes the JSON.
//
// Safe for concurrent AddToolCall (tools may run in parallel from a
// single response). Begin/End are called from the runner's main
// goroutine and don't need locking.
type Recorder struct {
	dir       string // <KaiDir>/runs/<sessionID>; "" disables writing
	sessionID string
	taskName  string

	current *turnState
	mu      sync.Mutex // guards current.Tools (concurrent tool dispatch)
}

type turnState struct {
	turn        int
	model       string
	startedAt   time.Time
	sections    Sections
	tools       []ToolCall
	rawRequest  *provider.Request // retained for debug-bodies dump
}

// New returns a Recorder writing to <kaiDir>/runs/<sessionID>. A nil
// Recorder is fine — every method is a no-op on nil so the runner
// doesn't have to guard each call. Returns nil when kaiDir or
// sessionID is empty (in-memory sessions skip persistence; matches
// the SessionStore contract).
func New(kaiDir, sessionID, taskName string) *Recorder {
	if kaiDir == "" || sessionID == "" {
		return nil
	}
	dir := filepath.Join(kaiDir, "runs", sessionID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		// Failure to create the dir disables logging for this run
		// rather than failing the run. Run-log is observability,
		// not a load-bearing dependency.
		return nil
	}
	return &Recorder{dir: dir, sessionID: sessionID, taskName: taskName}
}

// Begin records the start of a turn and measures the request's
// section sizes. Stores the raw request so End can dump it when the
// debug-bodies env var is set. Subsequent AddToolCall / End operate
// on this turn.
func (r *Recorder) Begin(turn int, req provider.Request) {
	if r == nil {
		return
	}
	r.current = &turnState{
		turn:       turn,
		model:      req.Model,
		startedAt:  time.Now(),
		sections:   measureSections(req),
		rawRequest: &req,
	}
}

// AddToolCall records one tool dispatch outcome. Called from the
// runner after each tool's BaseTool.Run returns. Safe to call from
// multiple goroutines if the runner ever parallelizes tool calls.
func (r *Recorder) AddToolCall(tc ToolCall) {
	if r == nil || r.current == nil {
		return
	}
	r.mu.Lock()
	r.current.tools = append(r.current.tools, tc)
	r.mu.Unlock()
}

// End records the response's usage + finish reason and writes the
// JSON artifact. After End the recorder is ready for another Begin.
// Safe to call with a nil resp on early-exit error paths — usage
// will be zeroed and the artifact still lands so the developer sees
// the failed turn.
func (r *Recorder) End(resp *provider.Response, err error) {
	if r == nil || r.current == nil {
		return
	}
	st := r.current
	r.current = nil

	t := Turn{
		SchemaVersion: schemaVersion,
		SessionID:     r.sessionID,
		Turn:          st.turn,
		Timestamp:     st.startedAt.UTC().Format(time.RFC3339),
		Model:         st.model,
		TaskName:      r.taskName,
		DurationMs:    time.Since(st.startedAt).Milliseconds(),
		Sections:      st.sections,
		ToolCalls:     append([]ToolCall(nil), st.tools...),
	}
	if resp != nil {
		t.Usage = Usage{
			InputTokens:              resp.InputTokens,
			OutputTokens:             resp.OutputTokens,
			CacheCreationInputTokens: resp.CacheCreationTokens,
			CacheReadInputTokens:     resp.CacheReadTokens,
			CachedPct:                cachedPct(resp.InputTokens, resp.CachedInputTokens),
			ReusePct:                 reusePct(resp.InputTokens, resp.CacheCreationTokens, resp.CacheReadTokens),
		}
		t.FinishReason = string(resp.FinishReason)
		t.AssistantText = firstAssistantText(resp.Parts, 600)
	}
	if err != nil {
		// Stash the error in the assistant_text slot when there's
		// no response — keeps the artifact one-stop for grep.
		if t.AssistantText == "" {
			t.AssistantText = "ERROR: " + err.Error()
		}
	}

	// Optional bodies dump for full replay fidelity. Off by default
	// because cache-heavy sessions ship MBs of history per turn and
	// most debugging questions are answered by the summary alone.
	if os.Getenv("KAI_DEBUG_RUNS") == "1" {
		if st.rawRequest != nil {
			path := filepath.Join(r.dir, fmt.Sprintf("%d.request.json", st.turn))
			writeJSON(path, requestForDump(*st.rawRequest))
			t.RequestPath = path
		}
		if resp != nil {
			path := filepath.Join(r.dir, fmt.Sprintf("%d.response.json", st.turn))
			writeJSON(path, responseForDump(*resp))
			t.ResponsePath = path
		}
	}

	writeJSON(filepath.Join(r.dir, fmt.Sprintf("%d.json", st.turn)), t)
}

// Dir returns the on-disk path of this recorder's session log dir.
// Useful for the CLI viewer to print "see <dir> for raw artifacts".
func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

func measureSections(req provider.Request) Sections {
	var s Sections
	s.System = measureString(req.System)
	s.Tools = measureJSON(stableTools(req.Tools))
	s.Messages = measureJSON(req.Messages)
	if n := len(req.Messages); n > 0 {
		s.Message = measureJSON(req.Messages[n-1])
	}
	return s
}

func measureString(s string) Section {
	h := sha256.Sum256([]byte(s))
	return Section{
		Chars:     len(s),
		EstTokens: len(s) / 4,
		Hash:      hex.EncodeToString(h[:8]),
	}
}

func measureJSON(v interface{}) Section {
	b, err := json.Marshal(v)
	if err != nil {
		return Section{}
	}
	h := sha256.Sum256(b)
	return Section{
		Chars:     len(b),
		EstTokens: len(b) / 4,
		Hash:      hex.EncodeToString(h[:8]),
	}
}

// stableTools returns the tools list sorted by name so a Go-map-
// driven tool registry doesn't produce a different hash on every turn
// just because of map iteration order — which would falsely register
// as a cache-invalidating drift in every diff.
func stableTools(in []tools.ToolInfo) []tools.ToolInfo {
	out := append([]tools.ToolInfo(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func cachedPct(input, cached int) int {
	total := input + cached
	if total == 0 {
		return 0
	}
	return cached * 100 / total
}

// reusePct is the fraction of input tokens that came from a previous
// turn's cache (cache_read), separated from cache_creation. This is
// the metric that actually correlates with cost on a multi-turn
// session: high reuse = cheap, low reuse = paying the write rate
// every turn even if cachedPct looks healthy.
func reusePct(input, write, read int) int {
	total := input + write + read
	if total == 0 {
		return 0
	}
	return read * 100 / total
}

func firstAssistantText(parts []message.ContentPart, max int) string {
	for _, p := range parts {
		if t, ok := p.(message.TextContent); ok {
			s := strings.TrimSpace(t.Text)
			if s == "" {
				continue
			}
			if len(s) > max {
				s = s[:max] + "…"
			}
			return s
		}
	}
	return ""
}

// requestForDump / responseForDump produce a marshal-friendly view of
// the heavy structs. Both contain Go interfaces that don't round-trip
// through plain json.Marshal in a way that's easy to read; we wrap
// them in a wider struct so future fields (Tool param schemas, raw
// content parts) can be added without breaking older artifacts.
func requestForDump(r provider.Request) interface{} {
	return map[string]interface{}{
		"model":      r.Model,
		"system":     r.System,
		"tools":      stableTools(r.Tools),
		"messages":   r.Messages,
		"max_tokens": r.MaxTokens,
	}
}

func responseForDump(r provider.Response) interface{} {
	return map[string]interface{}{
		"finish_reason":          string(r.FinishReason),
		"input_tokens":           r.InputTokens,
		"output_tokens":          r.OutputTokens,
		"cache_creation_tokens":  r.CacheCreationTokens,
		"cache_read_tokens":      r.CacheReadTokens,
		"cached_input_tokens":    r.CachedInputTokens,
		"parts":                  r.Parts,
		"provider_note":          r.ProviderNote,
	}
}

func writeJSON(path string, v interface{}) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
