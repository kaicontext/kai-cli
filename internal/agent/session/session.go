// Package session persists kai-agent conversations to the same
// SQLite file kai uses for the semantic graph (`<kaiDir>/db.sqlite`).
// One DB, one backup story; sessions naturally join with kai's other
// per-repo state.
//
// Tables, kept hand-rolled (no sqlc) because there are only a handful
// of queries:
//
//	agent_sessions(id, task_name, workspace, model, started_at,
//	               ended_at, status, total_tokens_in/out)
//	agent_messages(id, session_id, ordinal, role, parts_json,
//	               finished, tokens_in, tokens_out, created_at)
//
// Slice 5 contract: persistence happens automatically when the runner
// is given a Store + (optionally) a session id. Auto-resume into the
// TUI's REPL is a follow-up — Slice 5 just ensures the rows exist.
package session

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"kai/internal/agent/message"
)

// Store is the minimal SQLite handle the session layer needs. The
// methods match what `*kai/internal/graph.DB` already exposes, so
// graph.DB satisfies it directly — see EnsureSchema's caller in
// `cmd/kai/tui.go`.
type Store interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

// Status values for agent_sessions.status. "active" rows are live
// sessions a future TUI feature can offer to resume.
const (
	StatusActive  = "active"
	StatusEnded   = "ended"
	StatusErrored = "errored"
)

// Session is a handle to one conversation. New writes go through it;
// History reads return the full transcript in ordinal order.
type Session struct {
	ID        string
	TaskName  string
	Workspace string
	Model     string
	StartedAt time.Time
	Status    string
	store     Store
}

// EnsureSchema creates the agent_sessions / agent_messages tables and
// indexes if they don't exist. Idempotent; safe to call on every TUI
// startup. Mirrors the convention in `internal/graph/graph.go` where
// migrations live next to the code that uses them.
func EnsureSchema(db Store) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_sessions (
			id               TEXT PRIMARY KEY,
			task_name        TEXT NOT NULL,
			workspace        TEXT NOT NULL,
			model            TEXT NOT NULL DEFAULT '',
			started_at       INTEGER NOT NULL,
			ended_at         INTEGER,
			status           TEXT NOT NULL DEFAULT 'active',
			total_tokens_in  INTEGER NOT NULL DEFAULT 0,
			total_tokens_out INTEGER NOT NULL DEFAULT 0,
			prev_mode        TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS agent_sessions_active
			ON agent_sessions(status, started_at DESC)`,
		`CREATE TABLE IF NOT EXISTS agent_messages (
			id           TEXT PRIMARY KEY,
			session_id   TEXT NOT NULL,
			ordinal      INTEGER NOT NULL,
			role         TEXT NOT NULL,
			parts_json   TEXT NOT NULL,
			finished     TEXT NOT NULL DEFAULT '',
			tokens_in    INTEGER NOT NULL DEFAULT 0,
			tokens_out   INTEGER NOT NULL DEFAULT 0,
			created_at   INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS agent_messages_session
			ON agent_messages(session_id, ordinal)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("session: ensuring schema: %w", err)
		}
	}
	// Migration for DBs created before prev_mode landed. SQLite's
	// ALTER TABLE ADD COLUMN has no IF NOT EXISTS — try it and
	// swallow the "duplicate column" error so this stays idempotent.
	if _, err := db.Exec(
		`ALTER TABLE agent_sessions ADD COLUMN prev_mode TEXT NOT NULL DEFAULT ''`,
	); err != nil && !isDuplicateColumnErr(err) {
		return fmt.Errorf("session: adding prev_mode column: %w", err)
	}
	return nil
}

// isDuplicateColumnErr matches SQLite's "duplicate column name"
// message. We keep this loose (substring) because the underlying
// driver wraps the error string in different shapes across versions.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists")
}

// New begins a session and inserts an "active" row. The returned
// Session is ready for AppendMessage / History calls.
//
// Retries on transient SQLITE_BUSY / database-locked errors using
// the same backoff schedule AppendMessage uses (see comment block
// there). Without this, three workers spawned in parallel by the
// orchestrator race for the agent_sessions table's write lock; the
// first one wins, the other two die instantly with "database is
// locked (5)" and the whole subplan fails before doing any work.
// 2026-05-15 dogfood pinned this on a 3-agent speculative-dispatch
// plan: all three failed in <100ms total, two with SQLITE_BUSY.
func New(db Store, taskName, workspace, model string) (*Session, error) {
	if db == nil {
		return nil, errors.New("session.New: nil store")
	}
	s := &Session{
		ID:        uuid.NewString(),
		TaskName:  taskName,
		Workspace: workspace,
		Model:     model,
		StartedAt: time.Now().UTC(),
		Status:    StatusActive,
		store:     db,
	}
	if _, err := execWithBusyRetry(db, "session.New: insert",
		`INSERT INTO agent_sessions
			(id, task_name, workspace, model, started_at, status)
			VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.TaskName, s.Workspace, s.Model, s.StartedAt.UnixMilli(), s.Status,
	); err != nil {
		return nil, err
	}
	return s, nil
}

// RecentSession describes a candidate for resume-on-boot: a session
// that's still marked 'active' (the previous run didn't call End,
// which is what happens when the TUI is SIGKILL'd or the terminal
// goes away) and had a message in the last maxAge window.
//
// LastMessageAge is "how long ago the last message was appended" —
// the actual "is this still warm?" signal. StartedAt alone would
// surface sessions that were opened and then sat idle for hours.
type RecentSession struct {
	ID             string
	TaskName       string
	StartedAt      time.Time
	LastMessageAge time.Duration
	MessageCount   int
}

// FindRecent looks for the most recently-active session in this
// workspace that (a) is still marked active, (b) had a message within
// maxAge, and (c) has at least one message (skip empty shells that
// were created but never wrote anything). Returns nil, nil when no
// candidate exists.
//
// Implementation note: an earlier version of this used a single
// JOIN + GROUP BY agent_sessions × agent_messages with HAVING and
// ORDER BY MAX(created_at). On a workspace that had accumulated many
// "active" sessions (because previous TUI runs were SIGKILL'd before
// End() could fire), that query forces SQLite to scan every row in
// agent_messages. On a memory-starved host the sort buffer
// allocation could trigger a SIGKILL on the kai process itself
// before it could even render the resume prompt — observed during
// 2026-05-14 dogfood. The two-query form below scans at most one
// agent_sessions row plus one targeted message lookup.
//
// We also bound the candidate window via started_at: sessions whose
// row predates (now - 24h) are not considered, regardless of message
// activity. The 24h bound is a sanity cap; the real recency check
// is on the message timestamp.
func FindRecent(db Store, workspace string, maxAge time.Duration) (*RecentSession, error) {
	if db == nil || workspace == "" {
		return nil, errors.New("session.FindRecent: nil store or empty workspace")
	}
	startedSince := time.Now().Add(-24 * time.Hour).UnixMilli()
	row := db.QueryRow(
		`SELECT id, task_name, started_at
		 FROM agent_sessions
		 WHERE status = ?
		   AND workspace = ?
		   AND started_at >= ?
		 ORDER BY started_at DESC
		 LIMIT 1`,
		StatusActive, workspace, startedSince,
	)
	var (
		id, taskName string
		startedAt    int64
	)
	if err := row.Scan(&id, &taskName, &startedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("session.FindRecent: candidate lookup: %w", err)
	}

	// Second query: cheap targeted aggregate on the messages of the
	// single candidate. Bounded by session_id so SQLite uses the
	// agent_messages_session index (no full-table scan).
	var (
		lastMsg  sql.NullInt64
		msgCount int
	)
	if err := db.QueryRow(
		`SELECT MAX(created_at), COUNT(*)
		 FROM agent_messages
		 WHERE session_id = ?`,
		id,
	).Scan(&lastMsg, &msgCount); err != nil {
		return nil, fmt.Errorf("session.FindRecent: message aggregate: %w", err)
	}
	if msgCount == 0 || !lastMsg.Valid {
		return nil, nil
	}
	lastT := time.UnixMilli(lastMsg.Int64).UTC()
	age := time.Since(lastT)
	if age > maxAge {
		return nil, nil
	}
	return &RecentSession{
		ID:             id,
		TaskName:       taskName,
		StartedAt:      time.UnixMilli(startedAt).UTC(),
		LastMessageAge: age,
		MessageCount:   msgCount,
	}, nil
}

// Resume loads an existing session by id. Returns ErrNotFound if the
// id has no row, so callers can fall back to New cleanly.
var ErrNotFound = errors.New("session: not found")

func Resume(db Store, id string) (*Session, error) {
	if db == nil || id == "" {
		return nil, errors.New("session.Resume: nil store or empty id")
	}
	row := db.QueryRow(
		`SELECT task_name, workspace, model, started_at, status
		 FROM agent_sessions WHERE id = ?`, id)
	s := &Session{ID: id, store: db}
	var startedAt int64
	if err := row.Scan(&s.TaskName, &s.Workspace, &s.Model, &startedAt, &s.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("session.Resume: %w", err)
	}
	s.StartedAt = time.UnixMilli(startedAt).UTC()
	return s, nil
}

// AppendMessage stores one message at the next ordinal. tokensIn /
// tokensOut accumulate on the session row so a quick "how much did
// this run cost" query doesn't have to sum agent_messages.
//
// Retries on transient SQLITE_BUSY / database-locked errors. The DB
// is opened with busy_timeout=5000 (graph.Open) which handles most
// contention at the driver layer; this wraps that with a bounded
// Go-side retry for the cases where the wait exceeds the internal
// budget (parent TUI + spawned worker writing the same DB during a
// long capture-side write, etc.). Without this, a 5s busy spell on
// one INSERT kills the entire agent run with an opaque
// 'session.AppendMessage: insert: database is locked' error and
// the whole turn's work is lost. 2026-05-14 dogfood pinned this on
// a rename-file-tools run.
func (s *Session) AppendMessage(m message.Message, tokensIn, tokensOut int) error {
	if s == nil || s.store == nil {
		return errors.New("session.AppendMessage: nil session or store")
	}
	parts, err := encodeParts(m.Parts)
	if err != nil {
		return err
	}
	// Atomically claim the next ordinal. SQLite's COALESCE+MAX is
	// race-free under a single writer.
	var res sql.Result
	for attempt := 0; ; attempt++ {
		res, err = s.store.Exec(
			`INSERT INTO agent_messages
				(id, session_id, ordinal, role, parts_json, finished,
				 tokens_in, tokens_out, created_at)
				SELECT ?, ?, COALESCE(MAX(ordinal), -1) + 1, ?, ?, ?, ?, ?, ?
				FROM agent_messages WHERE session_id = ?`,
			uuid.NewString(), s.ID, string(m.Role), parts, string(m.Finished),
			tokensIn, tokensOut, time.Now().UnixMilli(), s.ID,
		)
		if err == nil {
			break
		}
		if !isSQLiteBusy(err) || attempt >= maxAppendRetries {
			return fmt.Errorf("session.AppendMessage: insert: %w", err)
		}
		// Exponential-ish backoff: 100ms, 250ms, 600ms, 1.4s, 3s.
		// Total worst-case wait ~5.4s on top of the driver's
		// busy_timeout window — gives ~10s of total resilience
		// before we give up. Keeps the turn alive across a hefty
		// concurrent capture without spinning forever.
		time.Sleep(appendRetryDelay(attempt))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("session.AppendMessage: no rows inserted")
	}

	if tokensIn > 0 || tokensOut > 0 {
		if _, err := execWithBusyRetry(s.store, "session.AppendMessage: token totals",
			`UPDATE agent_sessions
			 SET total_tokens_in = total_tokens_in + ?,
			     total_tokens_out = total_tokens_out + ?
			 WHERE id = ?`,
			tokensIn, tokensOut, s.ID,
		); err != nil {
			return err
		}
	}
	return nil
}

// History returns every message for the session in ordinal order.
// The runner calls this on Resume to seed the model with prior turns.
func (s *Session) History() ([]message.Message, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("session.History: nil session or store")
	}
	rows, err := s.store.Query(
		`SELECT role, parts_json, finished, tokens_in, tokens_out, created_at
		 FROM agent_messages WHERE session_id = ?
		 ORDER BY ordinal`, s.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("session.History: %w", err)
	}
	defer rows.Close()
	var out []message.Message
	for rows.Next() {
		var role, partsJSON, finished string
		var tIn, tOut, createdAt int64
		if err := rows.Scan(&role, &partsJSON, &finished, &tIn, &tOut, &createdAt); err != nil {
			return nil, fmt.Errorf("session.History: scan: %w", err)
		}
		parts, err := decodeParts(partsJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, message.Message{
			Role:     message.Role(role),
			Parts:    parts,
			Finished: message.FinishReason(finished),
			Time:     time.UnixMilli(createdAt).UTC(),
		})
	}
	return out, rows.Err()
}

// UserVisibleHistory returns a conversation-shaped view of the
// session: user prompts, assistant prose, plus a one-line synthesized
// summary in place of each tool-call / tool-result cluster. System
// messages are dropped entirely. Returned messages may have synthesized
// Parts (TextContent only) and a tagged Role that matches the original,
// so a chat agent loading this view sees a clean user/assistant
// conversation instead of the raw planner / executor scratchpad.
//
// Why this exists (2026-05-26 architecture spec, item #3): session IDs
// are reused across task types. When the chat agent resumes turn-N
// after a planner ran on turn-(N-1), it gets the planner's full
// transcript — planning system prompts, JSON tool calls + results,
// fenced plan emit — even though its own conversation-mode prompt
// expects conversational follow-ups. Format mismatch degrades recall.
// This view filters at the read side so other tasks (planner,
// executor, critic) still get the unfiltered transcript via History().
//
// The synthesizer cluster rule: contiguous assistant tool calls + the
// user-role tool results that satisfy them collapse into one synthetic
// line. A planner turn that calls (view, kai_grep) and gets back two
// results becomes: "[planner: view, kai_grep × 2]". The exact format is
// less important than the property that the chat agent sees ONE line
// per phase-of-work instead of dozens of JSON payloads.
func (s *Session) UserVisibleHistory() ([]message.Message, error) {
	full, err := s.History()
	if err != nil {
		return nil, err
	}
	if len(full) == 0 {
		return nil, nil
	}
	out := make([]message.Message, 0, len(full))
	// Buffer for pending tool-call cluster. When we hit an assistant
	// message containing tool calls, we start collecting; we flush
	// (emit a single synthesized line) when we hit the next message
	// that's NOT a tool-result or a continuation of the cluster.
	type clusterAcc struct {
		active     bool
		toolNames  []string // names of tools the assistant called in this cluster
		assistText string   // any assistant text emitted alongside the calls
		results    []string // factual content from tool-result user messages
		whenStart  time.Time
	}
	var cluster clusterAcc
	// maxResultLen caps each tool-result snippet in RESUMED history.
	// Smaller than first-pass results: those run once with full output;
	// the historic version only needs to preserve the FACTS, not the
	// full body, and bloated history is what blew the 300K token cap
	// in the 2026-05-27 graph-export-scaffold run (used 311607 / cap
	// 300000 — over by ~4%, partly because retained 4KB-each tool
	// results in long multi-turn sessions added up). 1KB lands kai_stats
	// JSON, kai_grep hits (usually <500B), bash one-liners, and the
	// preview line of any larger output. Long file views get clipped
	// to the leading 1KB plus a truncation marker.
	const maxResultLen = 1024
	flush := func() {
		if !cluster.active {
			return
		}
		var line string
		if len(cluster.toolNames) == 1 {
			line = fmt.Sprintf("[tool: %s]", cluster.toolNames[0])
		} else if len(cluster.toolNames) > 1 {
			// Bucket repeats: "view × 3, kai_grep × 2".
			counts := map[string]int{}
			order := []string{}
			for _, n := range cluster.toolNames {
				if counts[n] == 0 {
					order = append(order, n)
				}
				counts[n]++
			}
			parts := make([]string, 0, len(order))
			for _, n := range order {
				if counts[n] > 1 {
					parts = append(parts, fmt.Sprintf("%s × %d", n, counts[n]))
				} else {
					parts = append(parts, n)
				}
			}
			line = "[tools: " + strings.Join(parts, ", ") + "]"
		}
		text := strings.TrimSpace(cluster.assistText)
		if text != "" && line != "" {
			text = text + "\n" + line
		} else if line != "" {
			text = line
		}
		// Keep factual tool RESULTS in the resumed history. Without
		// these, a chat that ran `kai stats --json` and observed
		// edge_count=776463 loses that fact on the next turn — the
		// assistant's prose conclusion survives but the underlying
		// numbers don't, forcing rediscovery on every continuation.
		// 2026-05-27 dogfood: same question rediscovered the same
		// blocker across 3 auto-retries because tool results were
		// dropped from UserVisibleHistory.
		if len(cluster.results) > 0 {
			var resultBlock strings.Builder
			resultBlock.WriteString("\n[results]")
			for _, r := range cluster.results {
				resultBlock.WriteString("\n")
				if len(r) > maxResultLen {
					resultBlock.WriteString(r[:maxResultLen])
					resultBlock.WriteString(fmt.Sprintf("\n…(truncated %d more bytes)", len(r)-maxResultLen))
				} else {
					resultBlock.WriteString(r)
				}
			}
			text += resultBlock.String()
		}
		if text != "" {
			out = append(out, message.Message{
				Role:  message.RoleAssistant,
				Parts: []message.ContentPart{message.TextContent{Text: text}},
				Time:  cluster.whenStart,
			})
		}
		cluster = clusterAcc{}
	}

	for _, m := range full {
		switch m.Role {
		case message.RoleSystem:
			// Drop. System messages are scaffolding for the original
			// task; a follow-up chat agent has its own system prompt.
			flush()
			continue
		case message.RoleUser:
			// Two cases: a true user prompt (text content), or a
			// tool-result wrapper (Anthropic convention: tool results
			// arrive as user-role messages containing ToolResult
			// parts). The text case flushes the pending cluster and
			// emits the user turn. The tool-result case feeds the
			// cluster (we already counted the tool calls; results are
			// implicit — don't double-add to the line).
			hasText := false
			hasResult := false
			for _, p := range m.Parts {
				switch p.(type) {
				case message.TextContent:
					hasText = true
				case message.ToolResult:
					hasResult = true
				}
			}
			if hasText && !hasResult {
				flush()
				text := strings.TrimSpace(m.Text())
				if text != "" {
					out = append(out, message.Message{
						Role:  message.RoleUser,
						Parts: []message.ContentPart{message.TextContent{Text: text}},
						Time:  m.Time,
					})
				}
				continue
			}
			// Tool-result message: fold the result content into the
			// active cluster so flush() can include factual outputs
			// in the synthesized line. If the cluster wasn't active
			// (rare — shouldn't happen in well-formed sessions) drop
			// silently.
			if cluster.active {
				for _, p := range m.Parts {
					if tr, ok := p.(message.ToolResult); ok {
						content := strings.TrimSpace(tr.Content)
						if content != "" {
							cluster.results = append(cluster.results, content)
						}
					}
				}
			}
			continue
		case message.RoleAssistant:
			var toolNames []string
			var assistText string
			for _, p := range m.Parts {
				switch v := p.(type) {
				case message.TextContent:
					assistText += v.Text
				case message.ToolCall:
					toolNames = append(toolNames, v.Name)
				}
			}
			if len(toolNames) == 0 {
				// Pure prose turn. Flush any prior cluster and emit
				// the assistant text directly.
				flush()
				text := strings.TrimSpace(assistText)
				if text != "" {
					out = append(out, message.Message{
						Role:  message.RoleAssistant,
						Parts: []message.ContentPart{message.TextContent{Text: text}},
						Time:  m.Time,
					})
				}
				continue
			}
			// Assistant with tool calls: extend the active cluster
			// (or start one). Subsequent tool-result user messages
			// will fold in implicitly.
			if !cluster.active {
				cluster.active = true
				cluster.whenStart = m.Time
			}
			cluster.toolNames = append(cluster.toolNames, toolNames...)
			cluster.assistText += assistText
		}
	}
	flush()
	return out, nil
}

// LookupMode reads agent_sessions.prev_mode for the given id. Returns
// "" with no error if the row is missing — callers (the REPL dispatch)
// treat that as ModeUnknown so a fresh or expired session falls back
// to clean detection. Empty id also returns "" with no error so
// dispatch code can call this unconditionally before resuming.
func LookupMode(db Store, id string) (string, error) {
	if db == nil || id == "" {
		return "", nil
	}
	var mode string
	err := db.QueryRow(
		`SELECT prev_mode FROM agent_sessions WHERE id = ?`, id,
	).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("session.LookupMode: %w", err)
	}
	return mode, nil
}

// SaveMode persists the resolved mode for the next turn's sticky/soft
// resolution. Called by the dispatch layer after agent.Run returns
// with the mode that was actually used. Empty id is a no-op (the
// session wasn't being persisted).
func SaveMode(db Store, id, mode string) error {
	if db == nil || id == "" {
		return nil
	}
	_, err := execWithBusyRetry(db, "session.SaveMode",
		`UPDATE agent_sessions SET prev_mode = ? WHERE id = ?`,
		mode, id,
	)
	return err
}

// End marks the session terminal. The TUI calls this when the agent
// loop exits cleanly (status="ended") or aborts (status="errored").
// Idempotent — calling twice doesn't break anything.
func (s *Session) End(status string) error {
	if s == nil || s.store == nil {
		return errors.New("session.End: nil session or store")
	}
	if status == "" {
		status = StatusEnded
	}
	_, err := execWithBusyRetry(s.store, "session.End",
		`UPDATE agent_sessions
		 SET status = ?, ended_at = ?
		 WHERE id = ?`,
		status, time.Now().UnixMilli(), s.ID,
	)
	if err != nil {
		return err
	}
	s.Status = status
	return nil
}

// TruncateAfterLastUser deletes every message after the highest
// ordinal whose role is "user". Used by the critic auto-retry to
// purge a failed assistant turn (and any of its tool calls/results)
// from session history so the retry doesn't see its own bad answer.
//
// Returns the number of messages deleted. Zero is fine — means there
// was no assistant turn after the last user message, so nothing to
// remove.
//
// If there is no user message in the session at all, this is a
// no-op (we never want to wipe a session blindly; the contract is
// strictly "keep up to and including the last user turn").
func (s *Session) TruncateAfterLastUser() (int, error) {
	if s == nil || s.store == nil {
		return 0, errors.New("session.TruncateAfterLastUser: nil session or store")
	}
	// Find the highest ordinal for a user message. If none exists,
	// abort with 0 — refusing to delete is the safer default than
	// nuking the whole session.
	var maxUserOrd sql.NullInt64
	row := s.store.QueryRow(
		`SELECT MAX(ordinal) FROM agent_messages
		 WHERE session_id = ? AND role = 'user'`, s.ID)
	if err := row.Scan(&maxUserOrd); err != nil {
		return 0, fmt.Errorf("session.TruncateAfterLastUser: scan: %w", err)
	}
	if !maxUserOrd.Valid {
		return 0, nil
	}
	res, err := execWithBusyRetry(s.store, "session.TruncateAfterLastUser",
		`DELETE FROM agent_messages
		 WHERE session_id = ? AND ordinal > ?`, s.ID, maxUserOrd.Int64)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- ContentPart JSON encoding ---------------------------------------
//
// message.ContentPart is an interface; standard json.Marshal on a
// slice of interfaces produces inert empty objects unless we
// serialize each variant with a `type` discriminator. Done by hand
// here so the package stays free of reflection magic.

type partEnvelope struct {
	Type     string          `json:"type"`
	Raw      json.RawMessage `json:"data"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
}

const (
	partTypeText      = "text"
	partTypeReasoning = "reasoning"
	partTypeToolCall  = "tool_call"
	partTypeToolResult = "tool_result"
)

func encodeParts(parts []message.ContentPart) (string, error) {
	out := make([]partEnvelope, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case message.TextContent:
			b, _ := json.Marshal(v)
			out = append(out, partEnvelope{Type: partTypeText, Raw: b})
		case message.ReasoningContent:
			b, _ := json.Marshal(v)
			out = append(out, partEnvelope{Type: partTypeReasoning, Raw: b})
		case message.ToolCall:
			b, _ := json.Marshal(v)
			out = append(out, partEnvelope{Type: partTypeToolCall, Raw: b})
		case message.ToolResult:
			b, _ := json.Marshal(v)
			out = append(out, partEnvelope{Type: partTypeToolResult, Raw: b})
		default:
			// Forward-compat: unknown variants get serialized as
			// empty text so we don't drop ordering. Logging would
			// be ideal but session is a leaf package.
			b, _ := json.Marshal(message.TextContent{Text: ""})
			out = append(out, partEnvelope{Type: partTypeText, Raw: b})
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("session: encoding parts: %w", err)
	}
	return string(body), nil
}

func decodeParts(s string) ([]message.ContentPart, error) {
	var envs []partEnvelope
	if s == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(s), &envs); err != nil {
		return nil, fmt.Errorf("session: decoding parts: %w", err)
	}
	out := make([]message.ContentPart, 0, len(envs))
	for _, env := range envs {
		switch env.Type {
		case partTypeText:
			var v message.TextContent
			_ = json.Unmarshal(env.Raw, &v)
			out = append(out, v)
		case partTypeReasoning:
			var v message.ReasoningContent
			_ = json.Unmarshal(env.Raw, &v)
			out = append(out, v)
		case partTypeToolCall:
			var v message.ToolCall
			_ = json.Unmarshal(env.Raw, &v)
			out = append(out, v)
		case partTypeToolResult:
			var v message.ToolResult
			_ = json.Unmarshal(env.Raw, &v)
			out = append(out, v)
		}
	}
	return out, nil
}

// maxAppendRetries caps the Go-side retry loop on top of the driver-
// level busy_timeout. Five retries with the schedule below gives a
// worst-case extra wait of ~5.4 seconds; combined with the driver's
// 5s busy_timeout we get ~10s of total resilience before failing the
// turn, which covers a typical concurrent kai capture without
// spinning forever on a genuinely wedged DB.
const maxAppendRetries = 5

// execWithBusyRetry runs a single INSERT/UPDATE/DELETE and retries
// on SQLITE_BUSY using the standard backoff schedule. Use this for
// writes that may race with other agents writing to the same DB
// (anything in the parallel-spawn path); a non-racy write can call
// db.Exec directly. Returns the underlying error wrapped with the
// supplied operation label so callers can produce consistent
// "<op>: <driver-message>" errors. Mirrors the inline pattern
// AppendMessage uses for its primary INSERT.
func execWithBusyRetry(db Store, op, query string, args ...any) (sql.Result, error) {
	for attempt := 0; ; attempt++ {
		res, err := db.Exec(query, args...)
		if err == nil {
			return res, nil
		}
		if !isSQLiteBusy(err) || attempt >= maxAppendRetries {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		time.Sleep(appendRetryDelay(attempt))
	}
}

// appendRetryDelay returns the back-off duration before retry N.
// Roughly exponential: 100ms, 250ms, 600ms, 1.4s, 3s.
func appendRetryDelay(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 100 * time.Millisecond
	case 1:
		return 250 * time.Millisecond
	case 2:
		return 600 * time.Millisecond
	case 3:
		return 1400 * time.Millisecond
	default:
		return 3 * time.Second
	}
}

// isSQLiteBusy reports whether err is the transient SQLITE_BUSY /
// SQLITE_LOCKED case worth retrying. Matches the driver's error
// string rather than a sqlite3.Error type so we stay compatible with
// both modernc.org/sqlite and mattn/go-sqlite3 — kai has historically
// switched between drivers and we don't want the retry logic to
// silently stop working when the driver swaps.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "(5)") // SQLITE_BUSY error code
}
