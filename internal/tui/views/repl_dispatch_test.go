package views

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"kai/api/agent"
	"kai/api/session"
	"kai/api/orchestrator"
	"kai/api/planner"

)

// dbAdapter wraps *sql.DB to satisfy session.Store. session.Store is
// a minimal subset of *sql.DB's surface so tests can swap in an
// in-memory SQLite without spinning up a real graph.DB.
type dbAdapter struct{ *sql.DB }

// newInMemoryStore opens an in-memory SQLite, applies the agent
// session schema, and returns a Store. Used by the /status
// persisted-mode and slash-override-persist tests.
func newInMemoryStore(t *testing.T) session.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := &dbAdapter{db}
	if err := session.EnsureSchema(store); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return store
}

// TestDispatch_SlashRoutesToShellout: a leading "/" identifies the
// input as a kai subcommand, regardless of planner state.
func TestDispatch_SlashRoutesToShellout(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, cmd := r.dispatch("/gate list")
	if cmd == nil {
		t.Fatal("expected a tea.Cmd for shellout")
	}
	if r2.planning {
		t.Error("slash-prefixed input should not enter planning state")
	}
}

// TestDispatch_NoSlashGoesToPlanner: anything that isn't slash-prefixed
// is treated as a natural-language request and routed to the planner.
func TestDispatch_NoSlashGoesToPlanner(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, cmd := r.dispatch("Update README.md to mention the new TUI")
	if cmd == nil {
		t.Fatal("expected tea.Cmd")
	}
	if !r2.planning {
		t.Error("expected planning=true for an unprefixed sentence")
	}
}

// TestDispatch_NoServicesShellsOut: without a planner configured, even
// non-slash input shells out so the user sees kai's own usage error
// rather than silent no-op.
func TestDispatch_NoServicesShellsOut(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", nil)
	r2, cmd := r.dispatch("anything")
	if cmd == nil {
		t.Fatal("expected tea.Cmd")
	}
	if r2.planning {
		t.Error("with no services, planning state must not engage")
	}
}

// TestDispatch_PendingPlanGo: with a pending plan, "go" triggers
// orchestrator.Execute regardless of slash prefix (slash inside
// pending-plan state is irrelevant).
func TestDispatch_PendingPlanGo(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.pendingPlan = &planner.WorkPlan{Agents: []planner.AgentTask{{Name: "x", Prompt: "p"}}}
	r.originalReq = "do something"
	r2, cmd := r.dispatch("go")
	if cmd == nil {
		t.Fatal("expected tea.Cmd")
	}
	if !r2.executing {
		t.Error("expected executing=true after go")
	}
}

// TestDispatch_PendingPlanCancel clears the pending plan.
func TestDispatch_PendingPlanCancel(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.pendingPlan = &planner.WorkPlan{Agents: []planner.AgentTask{{Name: "x"}}}
	r.originalReq = "earlier"
	r2, cmd := r.dispatch("cancel")
	if cmd != nil {
		t.Errorf("cancel should not produce a tea.Cmd")
	}
	if r2.pendingPlan != nil || r2.originalReq != "" {
		t.Error("pendingPlan/originalReq should be cleared after cancel")
	}
}

// TestDispatch_PendingPlanFeedbackReplans: anything that isn't go/cancel
// while a plan is pending becomes feedback for replan.
func TestDispatch_PendingPlanFeedbackReplans(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.pendingPlan = &planner.WorkPlan{Agents: []planner.AgentTask{{Name: "x"}}}
	r.originalReq = "add rate limiting"
	r2, cmd := r.dispatch("only the public endpoints")
	if cmd == nil {
		t.Fatal("expected tea.Cmd for replan")
	}
	if !r2.planning {
		t.Error("expected planning=true after replan")
	}
}

// TestDispatch_ModeSlashOverridesSetForcedMode: /code, /debug, /review,
// /plan, /chat are TUI-internal — they set forcedMode, do not shell
// out, and do not produce a tea.Cmd.
func TestDispatch_ModeSlashOverridesSetForcedMode(t *testing.T) {
	cases := map[string]agent.Mode{
		"/code":   agent.ModeCoding,
		"/debug":  agent.ModeDebug,
		"/review": agent.ModeReview,
		"/plan":   agent.ModePlanning,
		"/chat":   agent.ModeConversation,
	}
	for input, want := range cases {
		r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
		r2, cmd := r.dispatch(input)
		if cmd != nil {
			t.Errorf("%s: expected no tea.Cmd (TUI-internal), got one", input)
		}
		if r2.forcedMode != want {
			t.Errorf("%s: forcedMode = %v, want %v", input, r2.forcedMode, want)
		}
	}
}

// TestDispatch_ForcedModeConsumedOnNextRequest: a slash override sets
// forcedMode; the next non-slash input consumes it (passing it into
// runPlan) and resets forcedMode so further turns flow through normal
// sticky/soft resolution.
func TestDispatch_ForcedModeConsumedOnNextRequest(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, _ := r.dispatch("/debug")
	if r2.forcedMode != agent.ModeDebug {
		t.Fatalf("expected forcedMode=Debug after /debug, got %v", r2.forcedMode)
	}
	r3, cmd := r2.dispatch("trace this")
	if cmd == nil {
		t.Fatal("expected tea.Cmd for natural-language request")
	}
	if r3.forcedMode != agent.ModeUnknown {
		t.Errorf("forcedMode should be reset after consumption, got %v", r3.forcedMode)
	}
	if !r3.planning {
		t.Error("expected planning=true after non-slash input")
	}
}

// TestDispatch_StatusCommandPrintsReport: /status renders a status
// block into the print buffer and does not shell out.
func TestDispatch_StatusCommandPrintsReport(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, cmd := r.dispatch("/status")
	if cmd != nil {
		t.Error("/status should be TUI-internal (no tea.Cmd)")
	}
	if !pendingContains(r2, "mode:") {
		t.Errorf("/status missing mode line: %v", r2.pendingPrints)
	}
	if !pendingContains(r2, "tokens:") {
		t.Errorf("/status missing tokens line: %v", r2.pendingPrints)
	}
}

// TestDispatch_StatusReflectsForcedMode: after /debug, /status shows
// debug mode.
func TestDispatch_StatusReflectsForcedMode(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, _ := r.dispatch("/debug")
	r3, _ := r2.dispatch("/status")
	if !pendingContains(r3, "debug") {
		t.Errorf("/status after /debug missing 'debug': %v", r3.pendingPrints)
	}
}

// TestStreamingDelta_NoCopyPanic: feeding multiple delta events
// through Update must not panic. Bubble Tea passes the model by
// value on every Update; a non-zero strings.Builder field would
// panic on the second copy. Pinned because we hit this exact bug
// twice — once for `buf`, again for `streamBuf` after the
// streaming work landed. Plain strings only on REPL.
func TestStreamingDelta_NoCopyPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Update panicked on streaming delta: %v", rec)
		}
	}()
	r := NewREPL("/usr/bin/true", "/tmp", nil)
	r.SetSize(80, 20)
	for _, chunk := range []string{"Hello", " ", "world", "!"} {
		r2, _ := r.Update(ChatActivityMsg{Event: ChatActivityEvent{
			Kind:  "delta",
			Delta: chunk,
		}})
		r = r2
	}
	if !strings.Contains(r.streamBuf, "Hello world!") {
		t.Errorf("streamed text not in streamBuf: %q", r.streamBuf)
	}
}

// TestPlanReadyMsg_ChatReplyRendersInline: when the planner falls
// back to a conversational answer (request was too vague to plan),
// the REPL writes the reply as inline text and does NOT enter
// pending-plan state. The user can keep typing.
func TestPlanReadyMsg_ChatReplyRendersInline(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.planning = true
	r2, _ := r.Update(PlanReadyMsg{
		Request:   "hi",
		ChatReply: "Hey! Tell me which file to change and I'll plan it.",
	})
	if r2.planning {
		t.Error("planning should be cleared on ChatReply")
	}
	if r2.pendingPlan != nil {
		t.Error("ChatReply should NOT set pendingPlan")
	}
	if !pendingContains(r2, "Hey!") {
		t.Errorf("chat reply missing from pendingPrints: %v", r2.pendingPrints)
	}
}

// TestPlanReadyMsg_PopulatesPendingPlan: after PlanReadyMsg lands the
// REPL holds the plan and renders it.
func TestPlanReadyMsg_PopulatesPendingPlan(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.planning = true
	r2, _ := r.Update(PlanReadyMsg{
		Request: "add a thing",
		Plan: &planner.WorkPlan{
			Summary: "thing added",
			Agents:  []planner.AgentTask{{Name: "a", Prompt: "p"}},
		},
	})
	if r2.planning {
		t.Error("planning should be cleared on PlanReadyMsg")
	}
	if r2.pendingPlan == nil {
		t.Error("pendingPlan should be set on PlanReadyMsg")
	}
	// Header phrasing changed in May-2026 to use the emoji
	// banner format ("📋 Plan — N agent(s)"). Match the
	// agent-count token rather than the literal old prefix.
	if !pendingContains(r2, "1 agent") {
		t.Errorf("scrollback missing plan: %v", r2.pendingPrints)
	}
}

// TestPlanReadyMsg_ErrorPath: an LLM/parse error clears state
// and renders a CLASSIFIED user-facing message — never the raw
// error text. The raw form lives in .kai/errors.log and (when
// telemetry is on) in PostHog. This is the May-5 polish change:
// "object store is missing blobs" must never reach the screen
// even on error paths the classifier doesn't have a specific
// rule for; the unknown fallback says "Something unexpected" +
// "kai diagnose" instead.
func TestPlanReadyMsg_ErrorPath(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.planning = true
	r2, _ := r.Update(PlanReadyMsg{
		Request: "x",
		Err:     errStub("api down"),
	})
	if r2.planning {
		t.Error("planning should be cleared on error")
	}
	if r2.pendingPlan != nil {
		t.Error("pendingPlan should remain nil on error")
	}
	// The classifier's friendly headline must appear.
	if !pendingContains(r2, "Something unexpected") {
		t.Errorf("expected classifier fallback headline, got: %v", r2.pendingPrints)
	}
	// May-2026 contract change: for the internal.unknown
	// fallback ONLY, we DO surface a one-line excerpt of the
	// raw error under "details:". Without this, the user has
	// nothing actionable for unmapped failure modes — they
	// reported the original "doesn't say anything specific"
	// pain. Known kinds (preflight.no_snapshots, etc.) still
	// hide their raw form per the original classifier design.
	if !pendingContains(r2, "api down") {
		t.Errorf("expected raw error excerpt under 'details:' for unknown fallback, got: %v", r2.pendingPrints)
	}
	// The /copy hint should be present so the user has a path to
	// share what happened. (The companion `kai diagnose` reference
	// was stripped: the command never landed.)
	if !pendingContains(r2, "/copy 4") {
		t.Errorf("expected /copy 4 action hint, got: %v", r2.pendingPrints)
	}
}

// pendingContains is a small test helper: returns true if any
// queued tea.Println line in the REPL contains the given substring.
// Replaces the old "search the shadow buf" pattern now that
// completed lines flush to the terminal's scrollback rather than
// living in a managed buffer.
func pendingContains(r REPL, sub string) bool {
	for _, line := range r.pendingPrints {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

type errStub string

func (e errStub) Error() string { return string(e) }

// TestRenderMarkdown_TransformsLists confirms glamour is wired in.
// Tests don't run in a TTY so glamour's auto-style falls back to a
// no-color theme — bold markers may stay as `**...**` (no ANSI to
// make them bold). What we CAN assert: list-bullet rewriting
// happens (- → •), and content is preserved. That proves the
// renderer is doing real work.
func TestRenderMarkdown_TransformsLists(t *testing.T) {
	md := "Here's the summary:\n\n- one\n- two\n- three\n"
	out, err := renderMarkdown(md, 80)
	if err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("rendered output empty")
	}
	if strings.Contains(out, "- one") || strings.Contains(out, "- two") {
		t.Errorf("dash bullets weren't rewritten: %q", out)
	}
	if !strings.Contains(out, "•") {
		t.Errorf("expected bullet glyph in output: %q", out)
	}
	for _, item := range []string{"one", "two", "three"} {
		if !strings.Contains(out, item) {
			t.Errorf("list item %q missing from output", item)
		}
	}
}

// TestREPLWriteMarkdown_FallsBackOnError: the pane should never go
// silent because glamour misbehaved on weird input. Empty input is
// the easy degenerate case.
func TestREPLWriteMarkdown_FallsBackOnError(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", nil)
	r.SetSize(40, 10)
	pre := len(r.pendingPrints)
	// Glamour can render empty just fine but the helper handles
	// zero-output and falls back. Use a string that surfaces
	// non-empty input through write() either way.
	r.writeMarkdown("hello")
	if len(r.pendingPrints) <= pre {
		t.Errorf("nothing was queued for flush; pendingPrints unchanged")
	}
}

// TestWrapToWidth covers the basic line-wrap cases. Long words at the
// boundary, multiple spaces, embedded newlines.
func TestWrapToWidth(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"hello world", 80, "hello world"},
		{"hello world", 5, "hello\nworld"},
		{"this is a longer line", 10, "this is a\nlonger\nline"},
		{"a b c d", 3, "a b\nc d"},
		{"first\nsecond", 80, "first\nsecond"},
		{"", 10, ""},
		{"unbreakable", 5, "unbreakable"}, // single word exceeds width — keep on its own line
	}
	for _, c := range cases {
		got := wrapToWidth(c.in, c.width)
		if got != c.want {
			t.Errorf("wrapToWidth(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

// TestWrapToWidth_DisabledForZeroWidth: write() defaults to 80 if
// the pane width hasn't been set yet, but the helper itself should
// pass through unchanged for non-positive widths.
func TestWrapToWidth_DisabledForZeroWidth(t *testing.T) {
	in := "this is a long line that won't be wrapped because width is zero"
	if got := wrapToWidth(in, 0); got != in {
		t.Errorf("expected pass-through for width=0, got %q", got)
	}
}

// TestREPLWrite_WrapsAtPaneWidth: a long line written after SetSize
// is word-wrapped at the pane width. (The constructor's greeting
// pre-dates SetSize and uses the 80-col default; that's a separate
// "re-wrap on resize" follow-up — the test here focuses on lines
// added once the size is known.)
func TestREPLWrite_WrapsAtPaneWidth(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", nil)
	r.SetSize(20, 10)
	preLen := len(r.pendingPrints)
	r.write("this is a fairly long line that should wrap")
	if len(r.pendingPrints) <= preLen {
		t.Fatalf("expected a queued print, got %d entries", len(r.pendingPrints))
	}
	added := r.pendingPrints[preLen]

	if !strings.Contains(added, "\n") {
		t.Errorf("expected wrapped line in appended segment, got: %q", added)
	}
	for _, segment := range strings.Split(added, "\n") {
		if utf8RuneLen(segment) > 18 { // 20 width - 2 margin
			t.Errorf("segment too long after wrap: %q (%d runes)", segment, utf8RuneLen(segment))
		}
	}
}

// TestDispatch_ClearResetsForcedMode confirms /clear scrubs any
// pending slash override along with the chat session and scrollback.
// Without this, a developer who typed /debug then /clear to start
// fresh would still be in Debug on their next turn.
func TestDispatch_ClearResetsForcedMode(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, _ := r.dispatch("/debug")
	if r2.forcedMode != agent.ModeDebug {
		t.Fatalf("setup: expected Debug after /debug")
	}
	r3, _ := r2.dispatch("/clear")
	if r3.forcedMode != agent.ModeUnknown {
		t.Errorf("/clear must reset forcedMode, got %v", r3.forcedMode)
	}
}

// TestDispatch_StatusForcedSourceWins: when both forcedMode and a
// persisted session prev_mode are present, the forced override is
// what /status reports — slash overrides outrank persisted state
// per the spec's precedence ordering.
func TestDispatch_StatusForcedSourceWins(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.forcedMode = agent.ModePlanning
	r.sessionID = "fake-session"
	r2, _ := r.dispatch("/status")
	// The forced source label should appear; persisted should not.
	found := false
	for _, line := range r2.pendingPrints {
		if strings.Contains(line, "planning") && strings.Contains(line, "forced") {
			found = true
		}
	}
	if !found {
		t.Errorf("/status didn't surface forced=planning: %v", r2.pendingPrints)
	}
}

// TestDispatch_StatusDefaultWhenNoSession: with no chat session and
// no forced override, /status falls back to "coding (default)".
func TestDispatch_StatusDefaultWhenNoSession(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, _ := r.dispatch("/status")
	found := false
	for _, line := range r2.pendingPrints {
		if strings.Contains(line, "coding (default)") {
			found = true
		}
	}
	if !found {
		t.Errorf("/status default mode missing from output: %v", r2.pendingPrints)
	}
}

// TestDispatch_SlashOverrideMessageWritten: a slash override should
// echo "mode → debug" (or similar) into the scrollback so the
// developer sees it took. Silent state changes are bad UX.
func TestDispatch_SlashOverrideMessageWritten(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r2, _ := r.dispatch("/review")
	if !pendingContains(r2, "mode → review") {
		t.Errorf("missing mode-change confirmation: %v", r2.pendingPrints)
	}
}

// TestDispatch_StatusReadsPersistedMode: with a session row carrying
// prev_mode="planning", /status falls back to that value when no
// override is forced. This exercises the LookupMode path.
func TestDispatch_StatusReadsPersistedMode(t *testing.T) {
	store := newInMemoryStore(t)
	sess, err := session.New(store, "chat", "/tmp", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if err := session.SaveMode(store, sess.ID, "planning"); err != nil {
		t.Fatalf("SaveMode: %v", err)
	}
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{
		OrchestratorCfg: orchestrator.Config{AgentSessionStore: store},
	})
	r.sessionID = sess.ID
	r2, _ := r.dispatch("/status")
	saw := false
	for _, line := range r2.pendingPrints {
		if strings.Contains(line, "planning") && strings.Contains(line, "persisted") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("/status didn't surface persisted=planning: %v", r2.pendingPrints)
	}
}

// TestDispatch_SlashOverridePersistsToSession: when a chat session
// already exists, /debug should write "debug" into agent_sessions.prev_mode
// immediately, so a TUI restart that resumes the session keeps the override.
func TestDispatch_SlashOverridePersistsToSession(t *testing.T) {
	store := newInMemoryStore(t)
	sess, err := session.New(store, "chat", "/tmp", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{
		OrchestratorCfg: orchestrator.Config{AgentSessionStore: store},
	})
	r.sessionID = sess.ID
	r.dispatch("/debug")
	got, err := session.LookupMode(store, sess.ID)
	if err != nil {
		t.Fatalf("LookupMode: %v", err)
	}
	if got != "debug" {
		t.Errorf("session prev_mode = %q, want debug", got)
	}
}

// TestResolveChatMode_ForcedWins: a slash override outranks both the
// persisted prev_mode and detection. This is the "escape hatch"
// guarantee from the spec — the developer always wins.
func TestResolveChatMode_ForcedWins(t *testing.T) {
	store := newInMemoryStore(t)
	sess, _ := session.New(store, "chat", "/tmp", "claude-sonnet-4-6")
	_ = session.SaveMode(store, sess.ID, "planning")
	got := resolveChatMode(store, sess.ID, "panic at auth.go:5", agent.ModeReview)
	if got != agent.ModeReview {
		t.Errorf("forced should win: got %v, want Review", got)
	}
}

// TestResolveChatMode_PersistedFlowsThroughDetect: with no forced
// override, prev_mode loaded from the session steers DetectMode's
// sticky/soft resolution. Debug is sticky, so a clarifying question
// stays in Debug instead of dropping to Conversation.
func TestResolveChatMode_PersistedFlowsThroughDetect(t *testing.T) {
	store := newInMemoryStore(t)
	sess, _ := session.New(store, "chat", "/tmp", "claude-sonnet-4-6")
	_ = session.SaveMode(store, sess.ID, "debug")
	// "what does that mean" detects as Conversation (soft); Debug
	// is sticky, so resolution should keep Debug.
	got := resolveChatMode(store, sess.ID, "what does that mean", agent.ModeUnknown)
	if got != agent.ModeDebug {
		t.Errorf("persisted Debug should stick across clarifying question: got %v", got)
	}
}

// TestResolveChatMode_DetectFallback: no forced, no session, no
// persisted mode → DetectMode runs unconstrained against ModeUnknown.
// Error signatures still steer Debug.
func TestResolveChatMode_DetectFallback(t *testing.T) {
	got := resolveChatMode(nil, "", "panic: nil pointer at auth.go:5", agent.ModeUnknown)
	if got != agent.ModeDebug {
		t.Errorf("detection should fire when nothing forces or persists: got %v", got)
	}
	got = resolveChatMode(nil, "", "edit auth.go please", agent.ModeUnknown)
	if got != agent.ModeCoding {
		t.Errorf("default should be Coding: got %v", got)
	}
}

// TestResolveChatMode_NoSessionStoreSkipsLookup: a nil store should
// not panic and should fall through to detection.
func TestResolveChatMode_NoSessionStoreSkipsLookup(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil store panicked: %v", r)
		}
	}()
	got := resolveChatMode(nil, "any-id", "fix it", agent.ModeUnknown)
	if got != agent.ModeCoding {
		t.Errorf("nil store with detection-fallback should give Coding, got %v", got)
	}
}

// TestPlanReadyMsg_LateTokensDoNotReanimateTrailer pins the
// May-4 race fix: a `tokens` ChatActivityMsg arriving AFTER
// PlanReadyMsg already finalized the run must NOT restart the
// tween. Without the trailerRendered guard the late event
// re-set tokenTarget* to non-zero and animated the live
// transient line, leaving an orphan "X fresh / Y out / Z%
// reused" indicator dangling below "─ end ─" until the next
// submit.
func TestPlanReadyMsg_LateTokensDoNotReanimateTrailer(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.planning = true

	// 1. PlanReadyMsg with a chat reply lands → trailer
	//    rendered, tween reset, trailerRendered flips true.
	r2, _ := r.Update(PlanReadyMsg{
		Request:       "yo",
		ChatReply:     "hey",
		ChatTokensIn:  6,
		ChatTokensOut: 130,
	})
	if !r2.trailerRendered {
		t.Fatal("trailerRendered must be true after PlanReadyMsg with chat reply")
	}
	if r2.tokenTargetIn != 0 || r2.tokenTargetOut != 0 {
		t.Errorf("tween targets must reset on PlanReadyMsg, got in=%d out=%d",
			r2.tokenTargetIn, r2.tokenTargetOut)
	}

	// 2. Late `tokens` event arrives (the racing OnTurnComplete
	//    callback that fired after the channel-deliver of
	//    PlanReadyMsg). Must be DROPPED — no target updates,
	//    no re-armed animation.
	r3, cmd := r2.Update(ChatActivityMsg{Event: ChatActivityEvent{
		Kind:         "tokens",
		TokensIn:     3,
		TokensOut:    189,
		TokensCached: 11000,
	}})
	if r3.tokenTargetIn != 0 || r3.tokenTargetOut != 0 || r3.tokenTargetCached != 0 {
		t.Errorf("late tokens event leaked into tween targets: in=%d out=%d cached=%d",
			r3.tokenTargetIn, r3.tokenTargetOut, r3.tokenTargetCached)
	}
	if r3.tokenAnimating {
		t.Error("late tokens event should NOT have re-armed the tween")
	}
	if cmd != nil {
		t.Error("late tokens event should NOT have scheduled a tick")
	}
}

// TestStartRun_RearmsLateTokensGuard ensures a fresh user submit
// resets trailerRendered so the next run actually receives its
// `tokens` events instead of having them all dropped.
func TestStartRun_RearmsLateTokensGuard(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.trailerRendered = true // simulate post-PlanReadyMsg state
	r.startRun()
	if r.trailerRendered {
		t.Error("startRun must reset trailerRendered to false")
	}
	// Confirm that a normal tokens event after startRun DOES
	// take effect (otherwise the guard would silently break the
	// live counter for every run after the first).
	r2, _ := r.Update(ChatActivityMsg{Event: ChatActivityEvent{
		Kind: "tokens", TokensIn: 1, TokensOut: 2, TokensCached: 3,
	}})
	if r2.tokenTargetIn != 1 {
		t.Errorf("post-startRun tokens event should set targets, got in=%d", r2.tokenTargetIn)
	}
}

// TestExecuteDoneMsg_ErrorRoutesThroughClassifier pins the
// May-5 fix for the orchestrator preflight leak. Before this:
// formatExecuteResult rendered "orchestrator: <err.Error()>"
// verbatim — the canonical "object store is missing blobs"
// message reached the user as raw text. Now ExecuteDoneMsg
// errors flow through the same classifier the planner-side
// error handler uses, so the user sees the friendly headline
// (or the "Something unexpected — kai diagnose" fallback)
// and the raw form lands in .kai/errors.log.
func TestExecuteDoneMsg_ErrorRoutesThroughClassifier(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.executing = true
	r2, _ := r.Update(ExecuteDoneMsg{
		Err: errStub("object store is missing blobs the snapshot references"),
	})
	if r2.executing {
		t.Error("executing should clear after ExecuteDoneMsg")
	}
	// The classifier's known-pattern headline for this kind
	// must appear (proves the rule fired).
	if !pendingContains(r2, "Snapshot needs rebuilding") {
		t.Errorf("expected classifier headline for missing_blobs, got: %v", r2.pendingPrints)
	}
	// Raw error MUST NOT leak into the render. The whole
	// point of routing through the classifier.
	if pendingContains(r2, "object store is missing blobs") {
		t.Errorf("raw error leaked into render: %v", r2.pendingPrints)
	}
	// Also: the old "orchestrator: " prefix must be gone.
	if pendingContains(r2, "orchestrator:") {
		t.Errorf("legacy 'orchestrator:' prefix leaked: %v", r2.pendingPrints)
	}
}

// TestExecuteDoneMsg_MissingBlobsFiresAutoRepair pins the
// May-5 wiring: when Classify returns kind
// "preflight.missing_blobs", the dispatcher MUST set the
// "Reindexing…" transient and return a non-nil tea.Cmd that
// performs the actual reindex. Before this fix, the
// classifier rendered the friendly "Reindexing the workspace…"
// promise but no repair ever ran — the user saw the message
// hang forever ("does it ever finish rebuilding the workspace?").
//
// We require MainRepo to be set since runAutoRepair bails when
// it's empty; that's intentional (no point shelling out
// without a workspace), but it means tests must populate it.
func TestExecuteDoneMsg_MissingBlobsFiresAutoRepair(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{MainRepo: "/tmp"})
	r.executing = true
	r2, cmd := r.Update(ExecuteDoneMsg{
		Err: errStub("object store is missing blobs the snapshot references"),
	})
	if cmd == nil {
		t.Fatal("expected a tea.Cmd kicking off auto-repair, got nil")
	}
	if !strings.Contains(r2.transient, "Reindexing") {
		t.Errorf("expected 'Reindexing…' transient set, got %q", r2.transient)
	}
}

// TestAutoRepairDoneMsg_ClearsTransientOnSuccess pins the
// completion side: AutoRepairDoneMsg with no error must clear
// the transient and write a "✓ workspace reindexed" line so
// the user sees the loop close.
func TestAutoRepairDoneMsg_ClearsTransientOnSuccess(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.transient = "· Reindexing the workspace…"
	r2, _ := r.Update(AutoRepairDoneMsg{
		Kind:    "preflight.missing_blobs",
		Err:     nil,
		Elapsed: 1234 * time.Millisecond,
	})
	if r2.transient != "" {
		t.Errorf("transient should clear on AutoRepairDoneMsg success, got %q", r2.transient)
	}
	if !pendingContains(r2, "workspace reindexed") {
		t.Errorf("expected success line, got: %v", r2.pendingPrints)
	}
}

// TestAutoRepairDoneMsg_ReportsFailure: if the repair itself
// fails (capture errored), the user MUST see that — silent
// failure would leave them staring at a cleared transient
// with no idea why their next request still fails.
func TestAutoRepairDoneMsg_ReportsFailure(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.transient = "· Reindexing the workspace…"
	r2, _ := r.Update(AutoRepairDoneMsg{
		Kind:   "preflight.missing_blobs",
		Err:    errStub("capture: boom"),
		Output: "stderr: something exploded",
	})
	if r2.transient != "" {
		t.Errorf("transient should clear even on failure, got %q", r2.transient)
	}
	if !pendingContains(r2, "auto-repair") {
		t.Errorf("expected auto-repair failure line: %v", r2.pendingPrints)
	}
	if !pendingContains(r2, "capture: boom") {
		t.Errorf("expected underlying error in render: %v", r2.pendingPrints)
	}
}

// TestExecuteDoneMsg_UnknownErrorRendersFallback covers the
// fallthrough: an error the classifier doesn't have a rule
// for must STILL be hidden behind the friendly fallback,
// not leak verbatim.
func TestExecuteDoneMsg_UnknownErrorRendersFallback(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.executing = true
	r2, _ := r.Update(ExecuteDoneMsg{
		Err: errStub("some weird internal failure mode"),
	})
	if !pendingContains(r2, "Something unexpected") {
		t.Errorf("expected fallback headline, got: %v", r2.pendingPrints)
	}
	// May-2026: internal.unknown now surfaces a one-line raw
	// excerpt under "details:" so the user has something
	// concrete to diagnose. Known kinds still hide raw form.
	if !pendingContains(r2, "weird internal failure") {
		t.Errorf("expected raw excerpt under 'details:' for unknown fallback, got: %v", r2.pendingPrints)
	}
	if !pendingContains(r2, "/copy 4") {
		t.Errorf("expected /copy 4 hint in fallback, got: %v", r2.pendingPrints)
	}
}

// TestPlanReadyMsg_RecordsTurnInRing pins the May-5 fix:
// Plan-path turns must land in the recent-turns ring so a
// follow-up "no sir i don't like it" has context to forward
// to claude. Before this, recordTurn only fired on the chat-
// reply path; planner-Plan responses left the ring empty
// and fixxy bailed with "no recent turns to review."
func TestPlanReadyMsg_RecordsTurnInRing(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.planning = true

	plan := &planner.WorkPlan{
		Summary: "Add a /health endpoint to server.go",
		Agents:  []planner.AgentTask{{Name: "x", Prompt: "p"}},
	}
	r2, _ := r.Update(PlanReadyMsg{
		Request: "add a health endpoint",
		Plan:    plan,
	})
	if len(r2.recentTurns) != 1 {
		t.Fatalf("expected 1 recorded turn, got %d", len(r2.recentTurns))
	}
	if r2.recentTurns[0].UserRequest != "add a health endpoint" {
		t.Errorf("user request mismatch: %q", r2.recentTurns[0].UserRequest)
	}
	if r2.recentTurns[0].KaiReply != "Add a /health endpoint to server.go" {
		t.Errorf("reply (plan summary) mismatch: %q", r2.recentTurns[0].KaiReply)
	}
}

// TestPlanReadyMsg_EmptyPlanStillRecords covers the empty-
// agents case (the "Already done / Answered" headline path):
// even though there's no executable plan, the fact that kai
// produced an answer should land in the ring so fixxy
// feedback works.
func TestPlanReadyMsg_EmptyPlanStillRecords(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.planning = true
	plan := &planner.WorkPlan{
		Summary:   "Directory structure is already detailed in the project overview.",
		Agents:    nil,
		RiskNotes: []string{"top-level dirs include kai-cli, kai-core"},
	}
	r2, _ := r.Update(PlanReadyMsg{Request: "what's here?", Plan: plan})
	if len(r2.recentTurns) != 1 {
		t.Fatalf("empty-plan turn should still record, got %d turns", len(r2.recentTurns))
	}
}

// TestIsPlanAffirmative pins the affirmative phrases that should
// be treated as "execute the plan" rather than feedback. Regression
// driver: user picks "feedback" from the menu, types "go ahead",
// expects execution; previously only the literal "go" worked, so
// natural-language affirmations got re-planned.
func TestIsPlanAffirmative(t *testing.T) {
	yes := []string{
		"go", "yes", "y", "ok", "okay", "sure",
		"go ahead", "yes please", "yep", "yeah",
		"ship it", "make it so", "proceed", "let's go",
		// trailing punctuation gets trimmed
		"go!", "yes.", "go ahead!!", "okay?",
		// case-folded by the caller
	}
	for _, s := range yes {
		if !isPlanAffirmative(s) {
			t.Errorf("expected affirmative match for %q", s)
		}
	}
	no := []string{
		// Substrings or near-misses — these are real user feedback,
		// not confirmation. The dispatcher's replan branch must
		// still get them.
		"go to the planner page and add a button",
		"yes but also rename the function",
		"sure, except keep the old API",
		"ok do it but with X instead of Y",
		"",
	}
	for _, s := range no {
		if isPlanAffirmative(s) {
			t.Errorf("did NOT expect affirmative match for %q (real feedback gets re-planned)", s)
		}
	}
}

// TestIsPlanCancel mirrors the affirmative test for the cancel
// path. Less risky because real feedback rarely starts with a
// bare "no", but worth pinning so we don't break it later.
func TestIsPlanCancel(t *testing.T) {
	yes := []string{"cancel", "no", "abort", "nope", "stop", "never mind"}
	for _, s := range yes {
		if !isPlanCancel(s) {
			t.Errorf("expected cancel match for %q", s)
		}
	}
	no := []string{
		"no, but also try X",
		"don't do that one — try the other",
	}
	for _, s := range no {
		if isPlanCancel(s) {
			t.Errorf("did NOT expect cancel match for %q (real feedback gets re-planned)", s)
		}
	}
}

// TestStuckHint covers the wall-clock thresholds. The hint should
// stay silent until 20s of no activity, then escalate at 2min and
// 5min. Drove the implementation: 2026-05-12 dogfood had a plan
// stuck for 7m30s with the spinner just ticking, no signal to the
// user that anything was wrong.
func TestStuckHint(t *testing.T) {
	cases := []struct {
		name string
		idle time.Duration
		want string // "" means empty
	}{
		{"fresh activity", 5 * time.Second, ""},
		{"under threshold", 15 * time.Second, ""},
		{"just past threshold", 25 * time.Second, "no activity"},
		{"under 2 minutes", 90 * time.Second, "no activity"},
		{"past 2 minutes escalates", 3 * time.Minute, "may be on a slow model turn"},
		{"past 5 minutes is direct", 8 * time.Minute, "likely wedged"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := REPL{lastActivity: time.Now().Add(-c.idle)}
			got := r.stuckHint()
			if c.want == "" {
				if got != "" {
					t.Errorf("expected empty hint, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("expected hint containing %q, got %q", c.want, got)
			}
		})
	}
}

// TestStuckHint_ZeroLastActivity: before the first activity event,
// lastActivity is the zero time. The hint must not fire — otherwise
// users see "no activity for 4000h" at startup.
func TestStuckHint_ZeroLastActivity(t *testing.T) {
	r := REPL{}
	if got := r.stuckHint(); got != "" {
		t.Errorf("zero lastActivity must produce empty hint, got %q", got)
	}
}

// TestPlanReadyMsg_CriticRetryFailure_RestoresPriorAnswer: when an
// auto-retry the critic dispatched fails (e.g. "Model returned no
// text"), the REPL must restore the answer it retracted rather than
// strand the user with an error and nothing.
func TestPlanReadyMsg_CriticRetryFailure_RestoresPriorAnswer(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.criticRetryCount = 1
	r.retractedAnswer = "the prior answer is preserved"
	r2, _ := r.Update(PlanReadyMsg{
		Request: "can this be improved?",
		Err:     errors.New("Model returned no text"),
	})
	if !pendingContains(r2, "preserved") {
		t.Errorf("expected the retracted answer to be restored, got: %v", r2.pendingPrints)
	}
	if r2.criticRetryCount != 0 {
		t.Errorf("criticRetryCount should reset to 0, got %d", r2.criticRetryCount)
	}
	if r2.retractedAnswer != "" {
		t.Errorf("retractedAnswer should be cleared after restore, got %q", r2.retractedAnswer)
	}
}

// TestPlanReadyMsg_NonRetryError_DoesNotRestore: an ordinary error
// (not from a critic auto-retry — criticRetryCount==0) must go through
// the normal error path, NOT restore some stale retractedAnswer.
func TestPlanReadyMsg_NonRetryError_DoesNotRestore(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{})
	r.criticRetryCount = 0
	r.retractedAnswer = "STALE_SHOULD_NOT_APPEAR"
	r2, _ := r.Update(PlanReadyMsg{
		Request: "do a thing",
		Err:     errors.New("some upstream failure"),
	})
	if pendingContains(r2, "STALE_SHOULD_NOT_APPEAR") {
		t.Errorf("must NOT restore retractedAnswer outside an active retry: %v", r2.pendingPrints)
	}
}
