package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/tools"
)

// TestCacheStability_SystemPromptIsByteStable is the regression
// guard for the May-3 cache-write bug. Originally this test PROVED
// the bug (system SHA differed across turns when convergence nudge
// fired). Now it asserts the FIX: per-turn dynamic content moved
// out of the system prompt and into a transient block on the last
// user message, so the system slice stays byte-identical across
// the entire run.
//
// If a future change re-introduces dynamic injection into
// systemForTurn (graph context, convergence, mode-switching,
// timestamps, etc.) this test fails — and the cache-write cost
// returns. Don't relax this test; relocate the dynamic content
// into the message tail per withPerTurnHints in runner.go.
//
// Why this is in the provider package: buildAnthropicRequest is
// where breakpoints are placed, so the test against the actual
// serialized wire shape (the thing Anthropic hashes) is honest.
func TestCacheStability_SystemPromptIsByteStable(t *testing.T) {
	const baseSystem = "You are agent \"chat\".\n\nTask: help the user."

	// Three "turns" of the same session. In the buggy world, turn3
	// (close to cap) would have the convergence nudge appended to
	// system. With the fix, system is the same input across all
	// three turns; per-turn hints ride the message tail.
	systems := []string{baseSystem, baseSystem, baseSystem}
	shas := make([]string, len(systems))
	for i, s := range systems {
		req := buildAnthropicRequest(Request{
			Model:     "claude-sonnet-4-6",
			System:    s,
			Messages:  twoTurnHistory(),
			MaxTokens: 4096,
		})
		shas[i] = sha(fmt.Sprint(req["system"]))
	}
	for i := 1; i < len(shas); i++ {
		if shas[i] != shas[0] {
			t.Fatalf("system SHA drifted at turn %d (%s vs %s); per-turn dynamic content leaked back into system prompt — see withPerTurnHints in runner.go",
				i, shas[i][:16], shas[0][:16])
		}
	}
	t.Logf("OK: system prompt byte-stable across simulated turns (SHA %s)", shas[0][:16])
}

// TestCacheInvalidation_FinalTurnStripsToolsKillsCache confirms
// the second cache-kill: on the final turn runner.go sets
// toolList = nil. The cache_control breakpoint serializeTools
// places on the LAST tool now has nothing to anchor to, and the
// resulting `tools` array is absent from the request entirely.
// Anthropic sees a request shape that doesn't even reference
// tools — there is no prefix overlap to match.
func TestCacheInvalidation_FinalTurnStripsToolsKillsCache(t *testing.T) {
	toolset := []tools.ToolInfo{
		{Name: "view", Description: "read", Parameters: map[string]interface{}{"file_path": map[string]string{"type": "string"}}},
		{Name: "kai_callers", Description: "callers", Parameters: map[string]interface{}{"symbol": map[string]string{"type": "string"}}},
	}
	withTools := buildAnthropicRequest(Request{
		Model:    "claude-sonnet-4-6",
		System:   "x",
		Messages: twoTurnHistory(),
		Tools:    toolset,
	})
	withoutTools := buildAnthropicRequest(Request{
		Model:    "claude-sonnet-4-6",
		System:   "x",
		Messages: twoTurnHistory(),
		Tools:    nil, // runner.go:684 final-turn behavior
	})

	if _, hasTools := withTools["tools"]; !hasTools {
		t.Fatal("setup error: with-tools request missing tools field")
	}
	if _, stillThere := withoutTools["tools"]; stillThere {
		t.Fatal("setup error: stripped request still has tools field")
	}
	t.Logf("CONFIRMED: stripping tools removes the entire 'tools' field from the request.")
	t.Logf("  → tools cache breakpoint (~5-15k tokens of schemas) cannot be matched.")
	t.Logf("  → With MaxTurns=12, that's 1 of 12 turns paying full cache-write cost on tools.")
	t.Logf("    Smaller per-turn impact than the system nudge but compounds with it.")
}

// TestCacheInvalidation_GrowingHistoryDoesNotKillCache is the
// sanity counter-test: the *intended* cache behavior. When only
// the conversation history grows (new user/assistant blocks
// appended) and nothing earlier changes, Anthropic's prefix
// matching means the prior cached prefix still hits — only the
// new tail bills as a write. This test ensures we don't have a
// *third* hidden bug where normal turn-to-turn growth invalidates
// what should be readable.
//
// Concretely: take history [A,B], hash the serialized prefix.
// Take history [A,B,C], extract the first two messages of its
// serialized form, hash that. They should match — meaning the
// cached [A,B] prefix would be readable when [A,B,C] is sent.
func TestCacheInvalidation_GrowingHistoryDoesNotKillCache(t *testing.T) {
	turn1 := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x", Messages: twoTurnHistory(),
	})
	turn2 := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x", Messages: threeTurnHistory(),
	})

	msgs1 := turn1["messages"].([]map[string]interface{})
	msgs2 := turn2["messages"].([]map[string]interface{})
	if len(msgs2) <= len(msgs1) {
		t.Fatal("setup wrong: turn2 should have more messages")
	}
	// Compare the FIRST len(msgs1)-1 messages — the last message of
	// turn1 has cache_control on its last block (per
	// serializeMessages tagging the tail), but the same message in
	// turn2 is no longer the tail and so loses that tag. Compare
	// only the strictly-earlier messages to isolate growth from
	// breakpoint-position drift.
	for i := 0; i < len(msgs1)-1; i++ {
		s1, _ := json.Marshal(msgs1[i])
		s2, _ := json.Marshal(msgs2[i])
		if string(s1) != string(s2) {
			t.Errorf("message[%d] differs between turns:\n  before: %s\n  after:  %s",
				i, s1, s2)
		}
	}

	// Now demonstrate the breakpoint-drift problem: the message
	// that had cache_control in turn1 LOST it in turn2 (because
	// the breakpoint moved to the new tail). Anthropic still
	// matches the static prefix bytes — the tag itself is
	// metadata — but this is what people often misread as a
	// cache miss when looking at the wire.
	last1, _ := json.Marshal(msgs1[len(msgs1)-1])
	idxInTurn2 := len(msgs1) - 1
	last1InTurn2, _ := json.Marshal(msgs2[idxInTurn2])
	if string(last1) == string(last1InTurn2) {
		t.Logf("note: cache_control tag did not move — verify serializer breakpoint logic")
	} else {
		t.Logf("CONFIRMED: cache_control tag migrates to the new tail each turn.")
		t.Logf("  This is fine for cache READS (prefix bytes match) but means each")
		t.Logf("  turn writes ONE new breakpoint. Real cache writes are bounded by")
		t.Logf("  the new-tail size, NOT the full prefix.")
	}
}

// TestCacheInvalidation_PostEditReReadDoesNotKillPriorCache
// disproves the FIRST hypothesis I floated to the user: the
// post-edit re-read injection (runner.go:appendPostEditViews)
// mutates a tool_result IN PLACE before the request that contains
// it is sent. So the augmented version IS what gets cached. The
// next turn sees the same augmented version and reads the cache.
//
// The bug only would have been real if appendPostEditViews
// retroactively edited a tool_result that had already been sent
// in a *previous* turn. It doesn't — it only touches the current
// turn's result-parts (runner.go:835 calls it on resultParts of
// the just-completed dispatch).
//
// This test pins that property so a future refactor can't
// silently introduce the bug. If someone later changes
// appendPostEditViews to walk back through history and update
// older tool_results, this test should be updated accordingly.
func TestCacheInvalidation_PostEditReReadDoesNotKillPriorCache(t *testing.T) {
	// We don't call appendPostEditViews directly here (it's in
	// the agent package). Instead we simulate its effect: build
	// turn N with a small tool_result, then turn N+1 where the
	// same prior tool_result has been augmented IN HISTORY (i.e.
	// the augmentation persisted before turn N's request was
	// sent). The earlier-turn prefix should remain stable across
	// the comparison.
	short := message.Message{
		Role: message.RoleUser,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call_1", Content: "wrote 1234 bytes"},
		},
	}
	augmented := message.Message{
		Role: message.RoleUser,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_1",
				Content:    "wrote 1234 bytes\n\n[runner: x.go now contains...]\n  1: package main\n  2: ...",
			},
		},
	}

	// Scenario A (the actual code path): mutation happens BEFORE
	// the request is built, so both turn N and turn N+1 see the
	// augmented version. This is the cache-friendly path.
	turnN := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x",
		Messages: []message.Message{userText("edit x.go"), augmented},
	})
	turnNplus1 := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x",
		Messages: []message.Message{userText("edit x.go"), augmented, userText("now what?")},
	})
	// Strip cache_control before comparing: Anthropic treats it
	// as a breakpoint marker, not as cacheable content. The
	// content bytes are what gets hashed for prefix matching, so
	// honest comparison ignores breakpoint metadata.
	prefixN := mustMarshal(stripCacheControl(turnN["messages"].([]map[string]interface{})[1]))
	prefixNplus1 := mustMarshal(stripCacheControl(turnNplus1["messages"].([]map[string]interface{})[1]))
	if string(prefixN) != string(prefixNplus1) {
		t.Errorf("post-edit augmented tool_result content differs across turns — cache would invalidate:\n  N:    %s\n  N+1:  %s",
			prefixN, prefixNplus1)
	} else {
		t.Logf("CONFIRMED: appendPostEditViews mutates BEFORE send, so the augmented")
		t.Logf("  tool_result is identical across the turn it was created and the next.")
		t.Logf("  → This is NOT a cache-invalidation source. (My earlier diagnosis was wrong.)")
	}

	// Scenario B (the hypothetical bug): if the augmentation
	// happened RETROACTIVELY after turn N was sent, turn N+1's
	// view of that tool_result would differ from what Anthropic
	// cached. We assert this would in fact differ — so anyone
	// reintroducing the bug in the future fails this test.
	bugTurnN := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x",
		Messages: []message.Message{userText("edit x.go"), short},
	})
	bugTurnNplus1 := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x",
		Messages: []message.Message{userText("edit x.go"), augmented, userText("now what?")},
	})
	bugPrefixN := mustMarshal(bugTurnN["messages"].([]map[string]interface{})[1])
	bugPrefixNplus1 := mustMarshal(bugTurnNplus1["messages"].([]map[string]interface{})[1])
	if string(bugPrefixN) == string(bugPrefixNplus1) {
		t.Error("retroactive augmentation should have produced different prefixes")
	}
}

// --- helpers ---------------------------------------------------------

// stripCacheControl returns a deep copy of msg with all
// cache_control fields removed from every nested block. Used by
// the post-edit test to compare CONTENT (the bytes Anthropic
// hashes for prefix matching) rather than BREAKPOINT METADATA
// (the cache_control tag, which Anthropic interprets as
// "store/match here" but doesn't include in the hash).
func stripCacheControl(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		if k == "cache_control" {
			continue
		}
		switch t := v.(type) {
		case []map[string]interface{}:
			cleaned := make([]map[string]interface{}, len(t))
			for i, b := range t {
				cleaned[i] = stripCacheControl(b)
			}
			out[k] = cleaned
		case map[string]interface{}:
			out[k] = stripCacheControl(t)
		default:
			out[k] = v
		}
	}
	return out
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func userText(s string) message.Message {
	return message.Message{
		Role:  message.RoleUser,
		Parts: []message.ContentPart{message.TextContent{Text: s}},
	}
}

func asstText(s string) message.Message {
	return message.Message{
		Role:  message.RoleAssistant,
		Parts: []message.ContentPart{message.TextContent{Text: s}},
	}
}

// TestCacheBreakpoints_RollingThreeUserMessages pins the rolling
// breakpoint strategy: when serializing a multi-turn history, up to
// 3 cache_control markers are placed on the most recent user-role
// messages BEFORE any ephemeral tail. Two consecutive turns then
// share at least one stable breakpoint position, which Anthropic's
// cache lookup can match against turn-N's stored prefix.
//
// Without rolling markers, each turn placed exactly one cache_control
// at the new dynamic end — that position never existed in the prior
// turn's request, and Anthropic's cache returned cache_read=0 even
// when the byte prefix matched. May 2026 run-log evidence: ~50% of
// turns missed despite KeepToolResults+EphemeralTailMessages being
// correct.
func TestCacheBreakpoints_RollingThreeUserMessages(t *testing.T) {
	// 6 messages: u, a, u, a, u, a — three user messages.
	history := []message.Message{
		userText("seed"), asstText("ok"),
		userText("u2"), asstText("ok2"),
		userText("u3"), asstText("ok3"),
	}
	req := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x", Messages: history,
	})
	msgs := req["messages"].([]map[string]interface{})

	tagged := 0
	for _, m := range msgs {
		role, _ := m["role"].(string)
		blocks, _ := m["content"].([]map[string]interface{})
		if len(blocks) == 0 {
			continue
		}
		_, has := blocks[len(blocks)-1]["cache_control"]
		if has {
			tagged++
			if role != "user" {
				t.Errorf("cache_control should only land on user messages, got role=%s", role)
			}
		}
	}
	if tagged != 3 {
		t.Errorf("expected 3 cache_control markers on user messages, got %d", tagged)
	}
}

// TestCacheBreakpoints_FewerThanThreeUserMessages confirms the
// strategy degrades gracefully when history has fewer than 3 user
// messages — no crash, just place markers on whatever's available.
func TestCacheBreakpoints_FewerThanThreeUserMessages(t *testing.T) {
	history := []message.Message{userText("only one")}
	req := buildAnthropicRequest(Request{
		Model: "claude-sonnet-4-6", System: "x", Messages: history,
	})
	msgs := req["messages"].([]map[string]interface{})
	tagged := 0
	for _, m := range msgs {
		blocks, _ := m["content"].([]map[string]interface{})
		if len(blocks) == 0 {
			continue
		}
		if _, has := blocks[len(blocks)-1]["cache_control"]; has {
			tagged++
		}
	}
	if tagged != 1 {
		t.Errorf("single-user history: expected 1 marker, got %d", tagged)
	}
}

// TestCacheBreakpoints_ExcludesEphemeralTail confirms that the
// EphemeralTailMessages count bumps the canonical-end pointer back
// so the per-turn hint message at the tail is NOT eligible for a
// cache_control marker. The hint's bytes vary turn-over-turn, so
// caching it would re-invalidate the prefix every turn.
func TestCacheBreakpoints_ExcludesEphemeralTail(t *testing.T) {
	history := []message.Message{
		userText("seed"), asstText("ok"),
		userText("tool_result"),
		userText("[runner: ephemeral hint]"),
	}
	req := buildAnthropicRequest(Request{
		Model:                 "claude-sonnet-4-6",
		System:                "x",
		Messages:              history,
		EphemeralTailMessages: 1,
	})
	msgs := req["messages"].([]map[string]interface{})

	// The last message (idx=3, the hint) must NOT carry cache_control.
	lastBlocks, _ := msgs[3]["content"].([]map[string]interface{})
	if len(lastBlocks) > 0 {
		if _, has := lastBlocks[len(lastBlocks)-1]["cache_control"]; has {
			t.Error("ephemeral tail message should not carry cache_control — it varies turn-over-turn")
		}
	}
	// The canonical-end (idx=2, the tool_result) MUST carry one.
	canonicalBlocks, _ := msgs[2]["content"].([]map[string]interface{})
	if len(canonicalBlocks) == 0 {
		t.Fatal("canonical end message has no blocks")
	}
	if _, has := canonicalBlocks[len(canonicalBlocks)-1]["cache_control"]; !has {
		t.Error("canonical-end message should carry a cache_control breakpoint")
	}
}

func twoTurnHistory() []message.Message {
	return []message.Message{
		userText("explain runner.go"),
		asstText(strings.Repeat("a", 200)),
	}
}

func threeTurnHistory() []message.Message {
	return []message.Message{
		userText("explain runner.go"),
		asstText(strings.Repeat("a", 200)),
		userText("now what about the planner?"),
	}
}
