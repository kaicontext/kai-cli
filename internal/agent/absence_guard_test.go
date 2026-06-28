package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

func TestIsNegativeClaim(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", false},
		{"whitespace", "   \n\t  ", false},
		{"plain affirmative", "It exists at internal/foo.go:42.", false},
		{"work in progress", "Let me check whether this exists.", false}, // hedged, not a claim
		{"not found", "The function is not found in the codebase.", true},
		{"no X found", "I found no callers of FooBar.", true},
		{"doesnt exist", "This config flag doesn't exist anywhere.", true},
		{"does not exist", "The handler does not exist in this version.", true},
		{"smart-quote apostrophe", "doesn’t exist in the planner package.", true},
		{"not implemented", "This feature is not implemented yet.", true},
		{"not present", "The cache hook is not present in the runner.", true},
		{"no evidence", "There is no evidence of cache invalidation.", true},
		{"couldnt find", "I couldn't find any reference to this symbol.", true},
		{"could not find", "Could not find the function definition.", true},
		{"cant find", "I can't find it anywhere in the tree.", true},
		{"cannot find", "Cannot find a registered handler.", true},
		{"unable to find", "I am unable to find this in the repo.", true},
		{"nothing matches", "Nothing matches the requested name.", true},
		{"appears to be absent", "It appears to be absent from this codebase.", true},
		{"is missing", "The implementation is missing.", true},
		{"no such function", "No such function exists in this module.", true},
		{"affirmative with the word 'not'", "I am not sure whether to keep this comment.", false},

		// Dismissal class — added after kai-code closed a real bug
		// with "already implemented (no bug)" on 2026-05-11.
		{"already implemented", "✓ Already done — nothing to do. Already implemented (no bug)", true},
		{"already exists", "The feature already exists in the codebase.", true},
		{"no bug", "This is no bug, just configuration.", true},
		{"not a bug", "After investigation, this is not a bug.", true},
		{"by design", "kai code opens kai/kai by design via the fixxy worker.", true},
		{"works as intended", "The current behavior works as intended.", true},
		{"working as designed", "It's working as designed — no changes required.", true},
		{"no changes needed", "No code changes are needed — environment issue.", true},
		{"nothing to do", "Already implemented — nothing to do.", true},
		{"nothing to fix", "Looked at it; nothing to fix here.", true},
		{"affirmative implementation", "The feature is implemented at runner.go:1543.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsNegativeClaim(c.text)
			if got != c.want {
				t.Errorf("IsNegativeClaim(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

// mkToolCall is a test helper that builds a message containing one
// tool-use part with the given name and JSON input. Mirrors what the
// runner constructs from streaming tool_use blocks.
func mkToolCall(name, input string) message.Message {
	return message.Message{
		Role:  message.RoleAssistant,
		Parts: []message.ContentPart{message.ToolCall{Name: name, Input: input}},
	}
}

func TestSearchCalls(t *testing.T) {
	msgs := []message.Message{
		// Affirmative messages aren't tool calls — should be ignored.
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "find the cache invalidator"}}},

		// Real kai_grep call — should be captured with its query.
		mkToolCall("kai_grep", `{"query":"extractMutatedPaths"}`),
		// kai_grep with no query field — captured with empty Query.
		mkToolCall("kai_grep", `{}`),
		// kai_callers takes a symbol — captured via the symbol field.
		mkToolCall("kai_callers", `{"symbol":"FooBar"}`),
		// view takes file_path — counts as a search.
		mkToolCall("view", `{"file_path":"internal/agent/runner.go"}`),
		// Unrelated tool — should be ignored.
		mkToolCall("write", `{"file_path":"foo.go","content":"hello"}`),
		// bash with grep — captured.
		mkToolCall("bash", `{"command":"grep -rn invalidateCache internal/"}`),
		// bash with non-search command — ignored.
		mkToolCall("bash", `{"command":"ls -la"}`),
		// bash with rg (alias).
		mkToolCall("bash", `{"command":"rg \"cache hook\" internal/"}`),
		// Malformed input JSON — graceful skip.
		mkToolCall("kai_grep", `not json`),
	}

	got := SearchCalls(msgs)

	// Expected: kai_grep(extractMutatedPaths), kai_grep(""),
	// kai_callers(FooBar), view(internal/agent/runner.go),
	// bash(invalidateCache), bash(cache hook), kai_grep("").
	if len(got) != 7 {
		t.Fatalf("got %d search calls, want 7: %+v", len(got), got)
	}
	if got[0].Tool != "kai_grep" || got[0].Query != "extractMutatedPaths" {
		t.Errorf("call[0] = %+v", got[0])
	}
	if got[2].Tool != "kai_callers" || got[2].Query != "FooBar" {
		t.Errorf("call[2] = %+v", got[2])
	}
	if got[3].Tool != "view" || got[3].Query != "internal/agent/runner.go" {
		t.Errorf("call[3] = %+v", got[3])
	}
	if got[4].Tool != "bash" || got[4].Query != "invalidateCache" {
		t.Errorf("call[4] = %+v", got[4])
	}
	if got[5].Tool != "bash" || got[5].Query != "cache hook" {
		t.Errorf("call[5] = %+v", got[5])
	}
}

func TestRelevantSearches(t *testing.T) {
	cases := []struct {
		name  string
		claim string
		calls []SearchCall
		want  int
	}{
		{
			name:  "empty claim never matches",
			claim: "",
			calls: []SearchCall{{Tool: "kai_grep", Query: "anything"}},
			want:  0,
		},
		{
			name:  "exact symbol match",
			claim: "extractMutatedPaths not found in runner",
			calls: []SearchCall{
				{Tool: "kai_grep", Query: "extractMutatedPaths"},
			},
			want: 1,
		},
		{
			name:  "camelCase sub-token match",
			claim: "no cache invalidation logic anywhere",
			calls: []SearchCall{
				{Tool: "kai_grep", Query: "invalidateCache"},
			},
			want: 1,
		},
		{
			name:  "snake_case sub-token match",
			claim: "the auto-test pass is missing",
			calls: []SearchCall{
				{Tool: "kai_grep", Query: "test_pass"},
			},
			want: 1,
		},
		{
			name:  "irrelevant search doesnt count",
			claim: "no kai_impact tool registered",
			calls: []SearchCall{
				{Tool: "kai_grep", Query: "completely unrelated query"},
				{Tool: "bash", Query: "ls"},
			},
			want: 0,
		},
		{
			name:  "stopword-only overlap doesnt count",
			claim: "the function does not exist",
			calls: []SearchCall{
				{Tool: "kai_grep", Query: "the and or"},
			},
			want: 0,
		},
		{
			name:  "empty-query call is uncounted",
			claim: "extractMutatedPaths is missing",
			calls: []SearchCall{
				{Tool: "kai_grep", Query: ""},
			},
			want: 0,
		},
		{
			name:  "three relevant searches count separately",
			claim: "no cache invalidation in runner",
			calls: []SearchCall{
				{Tool: "kai_grep", Query: "cache"},
				{Tool: "kai_grep", Query: "invalidation"},
				{Tool: "view", Query: "internal/agent/runner.go"},
			},
			want: 3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RelevantSearches(c.claim, c.calls)
			if got != c.want {
				t.Errorf("RelevantSearches(%q, …) = %d, want %d", c.claim, got, c.want)
			}
		})
	}
}

// TestSearchCalls_CountsInjectedContextLookup covers the Layer-2
// integration: a context_lookup tool result with "Entry: X" lines
// produces one synthetic SearchCall per entry. Critical for the
// graph-powered context injection — without this credit, the agent
// would be nudged to re-search what the planner already provided.
func TestSearchCalls_CountsInjectedContextLookup(t *testing.T) {
	transcript := []message.Message{
		// The synthetic pair the runner injects.
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:   contextLookupCallID,
					Name: contextLookupToolName,
				},
			},
		},
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{
				message.ToolResult{
					ToolCallID: contextLookupCallID,
					Name:       contextLookupToolName,
					Content: "Pre-resolved entry points...\n\n" +
						"Entry: kai code → runCodeTUI (via command index)\n" +
						"  → projects.Discover (discover.go)\n\n" +
						"Entry: Primary (via symbol index)\n",
				},
			},
		},
	}

	got := SearchCalls(transcript)
	if len(got) != 2 {
		t.Fatalf("expected 2 synthetic search calls, got %d: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Tool != contextLookupToolName {
			t.Errorf("synthetic call tool = %q, want %q", c.Tool, contextLookupToolName)
		}
	}
	if got[0].Query != "kai code" || got[1].Query != "Primary" {
		t.Errorf("unexpected queries: %+v", got)
	}
}

// TestExtractInjectedEntryQueries_NoMatches covers the negative
// path: bodies without "Entry:" lines produce zero queries (don't
// false-credit the agent for an empty injection).
func TestExtractInjectedEntryQueries_NoMatches(t *testing.T) {
	for _, body := range []string{
		"",
		"some random text without entries",
		"Entry without colon should not match",
	} {
		if got := extractInjectedEntryQueries(body); len(got) != 0 {
			t.Errorf("extractInjectedEntryQueries(%q) = %v, want empty", body, got)
		}
	}
}

// TestAbsenceGuard_KaiCodeRegression replays the exact bug we caught:
// kai-code claimed cache invalidation wasn't implemented after a
// single grep for "invalidateCache" / "dropCache" missed the actual
// name "extractMutatedPaths". The guard should now refuse to accept
// that conclusion until the agent has done ≥3 relevant searches.
func TestAbsenceGuard_KaiCodeRegression(t *testing.T) {
	claim := "Cache invalidation after writes is not implemented — no invalidateCache or dropCache found."
	transcript := []message.Message{
		mkToolCall("kai_grep", `{"query":"invalidateCache"}`),
		mkToolCall("kai_grep", `{"query":"dropCache"}`),
	}

	if !IsNegativeClaim(claim) {
		t.Fatal("expected claim to be detected as negative")
	}
	calls := SearchCalls(transcript)
	if got := RelevantSearches(claim, calls); got >= 3 {
		t.Errorf("RelevantSearches = %d, want < 3 (guard would let the false-negative through)", got)
	}

	// After the agent retries with additional searches whose queries
	// share tokens with the claim ("cache", "invalidation"), the guard
	// releases. In real life the agent would *also* probably restate
	// its claim — that path is exercised by IsNegativeClaim returning
	// false on the new text. Here we just verify the counter side.
	transcript = append(transcript,
		mkToolCall("kai_grep", `{"query":"cache"}`),
		mkToolCall("kai_grep", `{"query":"invalidation"}`),
		mkToolCall("kai_grep", `{"query":"invalidate after write"}`),
	)
	calls = SearchCalls(transcript)
	if got := RelevantSearches(claim, calls); got < 3 {
		t.Errorf("RelevantSearches after retry = %d, want ≥ 3", got)
	}
}

// TestRunLoop_AbsenceGuardFiresOnUnsupportedNegative covers the
// runner-level wiring: if the agent's final text is a negative claim
// without enough supporting searches, the runner injects a nudge and
// loops once more. Uses the same fakeProvider scaffold as
// runner_test.go's other integration tests.
func TestRunLoop_AbsenceGuardFiresOnUnsupportedNegative(t *testing.T) {
	ws := t.TempDir()

	p := &fakeProvider{queue: []provider.Response{
		// Turn 1: model emits a thin "not found" answer with no
		// supporting searches. Guard should fire.
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "The cache invalidation hook is not implemented in this codebase."},
			},
			FinishReason: message.FinishReasonEndTurn,
			OutputTokens: 10,
		},
		// Turn 2: model has now (notionally) been nudged, so it
		// returns a revised non-negative answer. Guard should NOT
		// fire again (per-run scoping).
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "After broader search, the hook is in runner.go as extractMutatedPaths."},
			},
			FinishReason: message.FinishReasonEndTurn,
			OutputTokens: 15,
		},
	}}

	res, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "Does cache invalidation exist?",
		Model:     "claude-sonnet-4-6",
		Provider:  p,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.calls != 2 {
		t.Errorf("expected 2 provider calls (1 negative + 1 retry), got %d", p.calls)
	}

	// Transcript: user prompt → assistant negative → user nudge → assistant final.
	if got := len(res.Transcript); got != 4 {
		t.Fatalf("expected 4 transcript entries, got %d", got)
	}
	nudge, _ := res.Transcript[2].Parts[0].(message.TextContent)
	if !strings.Contains(nudge.Text, "doesn't exist") {
		t.Errorf("third entry should be the absence-guard nudge, got: %q", nudge.Text)
	}
}

// TestRunLoop_AbsenceGuardSkippedWhenDisabled verifies that
// NoAbsenceGuard short-circuits the guard. Same canned thin negative
// answer; with the flag set, the runner accepts it on turn 1.
func TestRunLoop_AbsenceGuardSkippedWhenDisabled(t *testing.T) {
	ws := t.TempDir()

	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "Not found in the codebase."},
			},
			FinishReason: message.FinishReasonEndTurn,
			OutputTokens: 5,
		},
	}}

	res, err := Run(context.Background(), Options{
		Workspace:      ws,
		Prompt:         "Find X.",
		Model:          "claude-sonnet-4-6",
		Provider:       p,
		NoAbsenceGuard: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("expected 1 provider call with guard disabled, got %d", p.calls)
	}
	if res.FinishReason != message.FinishReasonEndTurn {
		t.Errorf("expected end_turn, got %s", res.FinishReason)
	}
}

// TestRunLoop_InjectsContextLookup covers the graph-powered context
// injection path: when Options.InjectedContext is set, the runner
// splices a synthetic context_lookup tool_use + tool_result pair
// into the transcript immediately after the user prompt. Verifies
// the structure end-to-end: provider sees a request with the
// injection in place, transcript persists the pair.
func TestRunLoop_InjectsContextLookup(t *testing.T) {
	ws := t.TempDir()

	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "Got the context. Done."},
			},
			FinishReason: message.FinishReasonEndTurn,
			OutputTokens: 5,
		},
	}}

	res, err := Run(context.Background(), Options{
		Workspace:       ws,
		Prompt:          "why did kai code open the wrong project",
		Model:           "claude-sonnet-4-6",
		Provider:        p,
		InjectedContext: "Entry: kai code → runCodeTUI",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Transcript: user prompt → assistant(tool_use ctx_lookup) →
	// user(tool_result) → assistant(final text).
	if got := len(res.Transcript); got != 4 {
		t.Fatalf("expected 4 transcript entries, got %d: %+v", got, res.Transcript)
	}

	// Entry 1 (index 1) is the synthetic assistant turn.
	a1 := res.Transcript[1]
	if a1.Role != message.RoleAssistant {
		t.Errorf("entry 1 role = %s, want assistant", a1.Role)
	}
	if len(a1.Parts) != 1 {
		t.Fatalf("entry 1 should have 1 part, got %d", len(a1.Parts))
	}
	tc, ok := a1.Parts[0].(message.ToolCall)
	if !ok {
		t.Fatalf("entry 1 part should be ToolCall, got %T", a1.Parts[0])
	}
	if tc.Name != contextLookupToolName {
		t.Errorf("tool call name = %q, want %q", tc.Name, contextLookupToolName)
	}

	// Entry 2 is the matching tool result.
	r2 := res.Transcript[2]
	if r2.Role != message.RoleUser {
		t.Errorf("entry 2 role = %s, want user", r2.Role)
	}
	tr, ok := r2.Parts[0].(message.ToolResult)
	if !ok {
		t.Fatalf("entry 2 part should be ToolResult, got %T", r2.Parts[0])
	}
	if tr.ToolCallID != contextLookupCallID {
		t.Errorf("tool result id = %q, want %q", tr.ToolCallID, contextLookupCallID)
	}
	if !strings.Contains(tr.Content, "runCodeTUI") {
		t.Errorf("tool result content missing injection body: %q", tr.Content)
	}
}

// TestRunLoop_NoInjectionWhenContextEmpty verifies the negative
// case: an empty InjectedContext leaves the transcript clean (no
// synthetic tool_use appears).
func TestRunLoop_NoInjectionWhenContextEmpty(t *testing.T) {
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "done"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	res, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "do a thing",
		Model:     "claude-sonnet-4-6",
		Provider:  p,
		// InjectedContext deliberately not set.
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, m := range res.Transcript {
		for _, p := range m.Parts {
			if tc, ok := p.(message.ToolCall); ok && tc.Name == contextLookupToolName {
				t.Errorf("found context_lookup tool call in transcript when InjectedContext was empty")
			}
		}
	}
}

// TestRunLoop_AbsenceGuardFiresAtMostOncePerRun verifies the per-run
// scoping rule: even if the model keeps emitting negative claims with
// no new searches, the guard fires once then steps out of the way.
// Without this, an agent stuck in a "not found" mood would loop until
// maxTurns and burn the budget.
func TestRunLoop_AbsenceGuardFiresAtMostOncePerRun(t *testing.T) {
	ws := t.TempDir()

	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "Not found."},
			},
			FinishReason: message.FinishReasonEndTurn,
		},
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "Still not found."},
			},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}

	res, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "Does X exist?",
		Model:     "claude-sonnet-4-6",
		Provider:  p,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Exactly 2 calls: original + one guard-driven retry. The
	// second "not found" is accepted because the guard already
	// fired once for this run.
	if p.calls != 2 {
		t.Errorf("expected 2 provider calls (guard fires once), got %d", p.calls)
	}
	if res.FinishReason != message.FinishReasonEndTurn {
		t.Errorf("expected end_turn after second negative, got %s", res.FinishReason)
	}
}
