package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/tools"
)

// Local-model tool-call text fallback.
//
// Many local models (Llama 3.1, Hermes-tuned, Qwen 2.5) reliably
// emit tool calls but don't populate the OpenAI structured
// `tool_calls` field — they put the call inline in the assistant
// content instead. Our agent loop sees no tool dispatch, treats
// the turn as a plain reply, and stalls. This file extracts those
// inline calls so the loop can proceed.
//
// We support multiple wire formats because there isn't a standard:
// each model family invented its own. Users opt into the format
// their model uses with KAI_OPENAI_TOOL_FORMAT. Default is "raw"
// which only matches well-formed `{"name":..., "arguments":...}`
// JSON, the most common shape. A wrong/unset format on a model
// that needs one means the inline call is missed and the model's
// content surfaces as a plain reply — easy to diagnose, hard to
// silently corrupt.
//
// Allowed values:
//   "raw"     (default) → bare JSON object or markdown ```json fence
//   "hermes"  → <tool_call>{...}</tool_call>
//   "llama3"  → <|python_tag|>{...} or {"name":..., "parameters":...}

// extractToolCallsFromText scans assistant content for inline tool
// calls in the configured wire format. Returns the parts to ADD to
// the response (TextContent + ToolCalls in order) and a bool
// indicating whether anything was extracted (callers replace the
// default text-only parsing only when this is true).
//
// allowed names the catalog of tools the agent has on this turn
// (Request.Tools). We never dispatch a name that wasn't offered —
// a model hallucinating "do_the_thing" doesn't get to invoke it.
func extractToolCallsFromText(content string, allowed []tools.ToolInfo, format string) (parts []message.ContentPart, extracted bool) {
	if content == "" || len(allowed) == 0 {
		return nil, false
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		allowedSet[t.Name] = true
	}

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "hermes":
		return extractHermes(content, allowedSet)
	case "llama3":
		return extractLlama3(content, allowedSet)
	default:
		return extractRaw(content, allowedSet)
	}
}

// rawToolCallRe finds a JSON object containing both "name" and
// either "arguments" or "parameters". Tolerant of nested JSON in
// the arguments field — the regex only locates the *start*; we
// then balance braces to find the end. A pure regex can't handle
// nested braces reliably.
var rawNameMarkerRe = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)

// extractRaw handles the default format: bare JSON or markdown
// fenced JSON. We strip ```json ... ``` fences first, then walk
// the text looking for JSON objects with the right shape.
func extractRaw(content string, allowed map[string]bool) ([]message.ContentPart, bool) {
	stripped := stripJSONFences(content)
	calls, residual := scanJSONToolCalls(stripped, allowed)
	if len(calls) == 0 {
		return nil, false
	}
	return assembleParts(residual, calls), true
}

// hermesRe matches <tool_call>...</tool_call>. Multiple calls in
// one response are allowed. Inner content is trimmed and parsed
// the same way as raw.
var hermesRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

func extractHermes(content string, allowed map[string]bool) ([]message.ContentPart, bool) {
	matches := hermesRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil, false
	}
	var calls []message.ToolCall
	residual := content
	// Walk in reverse so index-based removals don't shift later
	// match indices.
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		raw := content[m[2]:m[3]]
		if tc, ok := parseToolCallJSON(raw, allowed); ok {
			calls = append([]message.ToolCall{tc}, calls...) // prepend to preserve order
			residual = residual[:m[0]] + residual[m[1]:]
		}
	}
	if len(calls) == 0 {
		return nil, false
	}
	return assembleParts(residual, calls), true
}

// llama3Re matches Llama 3.1's preferred wire format. The
// <|python_tag|> sentinel may or may not be present depending on
// the system prompt and tokenizer; both shapes flow through.
var llama3SentinelRe = regexp.MustCompile(`<\|python_tag\|>\s*`)

func extractLlama3(content string, allowed map[string]bool) ([]message.ContentPart, bool) {
	stripped := llama3SentinelRe.ReplaceAllString(content, "")
	stripped = stripJSONFences(stripped)
	calls, residual := scanJSONToolCalls(stripped, allowed)
	if len(calls) == 0 {
		return nil, false
	}
	return assembleParts(residual, calls), true
}

// scanJSONToolCalls walks `text` looking for JSON objects whose
// "name" field matches an allowed tool. Returns the extracted
// calls in source order and `text` with those JSON objects
// removed (so the residual prose can be surfaced as a normal
// TextContent without the JSON re-rendering as model output).
func scanJSONToolCalls(text string, allowed map[string]bool) ([]message.ToolCall, string) {
	var calls []message.ToolCall
	residual := text

	// Iterate: find a "name" marker, locate the enclosing JSON
	// object via brace-balancing, attempt to parse, advance past
	// it. We loop because a single response can contain multiple
	// inline calls (rare but real on planning-heavy turns).
	for {
		loc := rawNameMarkerRe.FindStringIndex(residual)
		if loc == nil {
			break
		}
		// Walk backward from the marker to find the enclosing '{'.
		objStart := -1
		for i := loc[0]; i >= 0; i-- {
			if residual[i] == '{' {
				objStart = i
				break
			}
		}
		if objStart < 0 {
			// No enclosing object — strip past this marker and
			// keep scanning. Avoids infinite loop on malformed
			// content that has "name":"..." outside any object.
			residual = residual[loc[1]:]
			continue
		}
		objEnd := matchBalancedBrace(residual, objStart)
		if objEnd < 0 {
			// Unbalanced — abandon the rest. Better to surface
			// content as text than dispatch a half-parsed call.
			break
		}
		jsonChunk := residual[objStart : objEnd+1]
		if tc, ok := parseToolCallJSON(jsonChunk, allowed); ok {
			calls = append(calls, tc)
			residual = residual[:objStart] + residual[objEnd+1:]
		} else {
			// Parsed but didn't qualify (unknown tool, malformed
			// args). Move past this object so we don't loop on it.
			residual = residual[:objStart] + residual[objEnd+1:]
		}
	}
	return calls, residual
}

// matchBalancedBrace returns the index of the matching '}' for the
// '{' at start, or -1 if unbalanced. Handles nested braces and
// braces inside string literals (skips escaped quotes inside
// strings so {"x":"a\"b"} parses correctly).
func matchBalancedBrace(s string, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// parseToolCallJSON parses a JSON chunk into a message.ToolCall.
// Accepts both `arguments` (OpenAI canonical) and `parameters`
// (Llama 3.1 idiom). Returns ok=false when the name isn't allowed
// or required fields are missing — never panics, never errors out.
func parseToolCallJSON(raw string, allowed map[string]bool) (message.ToolCall, bool) {
	var probe struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return message.ToolCall{}, false
	}
	if probe.Name == "" || !allowed[probe.Name] {
		return message.ToolCall{}, false
	}
	args := probe.Arguments
	if len(args) == 0 {
		args = probe.Parameters
	}
	argsStr := strings.TrimSpace(string(args))
	if argsStr == "" {
		argsStr = "{}"
	}
	// Sanity-check the arguments parse as an object — a string or
	// number where an object is expected won't dispatch correctly,
	// and surfacing it as text is more honest than passing junk
	// to the tool.
	var probeArgs map[string]interface{}
	if err := json.Unmarshal([]byte(argsStr), &probeArgs); err != nil {
		return message.ToolCall{}, false
	}
	return message.ToolCall{
		// No id from the wire — synthesize one. Tools don't
		// inspect the id beyond echoing it in tool_result, so any
		// stable string works.
		ID:       fmt.Sprintf("text_%s_%d", probe.Name, len(argsStr)),
		Name:     probe.Name,
		Input:    argsStr,
		Type:     "tool_use",
		Finished: true,
	}, true
}

// stripJSONFences removes ```json ... ``` and ``` ... ``` markdown
// fences from content. The interior is preserved so the JSON
// scanner can find tool calls inside them. Idempotent — running
// it twice produces the same result.
var fenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

func stripJSONFences(s string) string {
	return fenceRe.ReplaceAllString(s, "$1")
}

// assembleParts wraps the residual prose (if non-empty) followed
// by extracted ToolCall parts into a ContentPart slice ready to
// drop into Response.Parts. Trims leading/trailing whitespace from
// the prose since extraction usually leaves dangling newlines.
func assembleParts(residual string, calls []message.ToolCall) []message.ContentPart {
	parts := make([]message.ContentPart, 0, len(calls)+1)
	if t := strings.TrimSpace(residual); t != "" {
		parts = append(parts, message.TextContent{Text: t})
	}
	for _, c := range calls {
		parts = append(parts, c)
	}
	return parts
}

// textCallsWarned ensures the "model emitted tool call as text"
// warning fires at most once per process. Local-model users hit
// this every turn; spamming would be hostile. atomic.Bool keeps
// it goroutine-safe (multiple agent runs may race the first
// extraction).
var textCallsWarned atomic.Bool

// noteFirstTextExtraction logs a one-shot stderr warning the first
// time the fallback fires. Subsequent extractions are silent. We
// log to stderr (not the agent debug log) so the user sees it
// regardless of where they enabled debug — local-model debugging
// is the whole reason this code exists.
func noteFirstTextExtraction() {
	if textCallsWarned.CompareAndSwap(false, true) {
		fmt.Fprintln(os.Stderr,
			"kai: model emitted tool call as text (not structured); "+
				"tool-use quality may be limited. "+
				"Set KAI_OPENAI_TOOL_FORMAT=hermes|llama3 if needed.")
	}
}

// applyTextCallFallback inspects a parsed Response and, if it
// contains text content but no structured tool_calls, attempts to
// extract tool calls from the text. On success it REPLACES
// Response.Parts with the extracted parts (residual text +
// extracted ToolCalls) and updates FinishReason to ToolUse so the
// runner dispatches the call instead of treating the turn as a
// plain reply.
//
// No-op when:
//   - the response already has a structured ToolCall part
//   - there's no text content to scan
//   - no tools are allowed (req.Tools empty)
//   - extraction returns no calls
func applyTextCallFallback(resp *Response, allowed []tools.ToolInfo) {
	if resp == nil || len(allowed) == 0 {
		return
	}
	// If a structured tool_use is already present, nothing to do.
	for _, p := range resp.Parts {
		if _, ok := p.(message.ToolCall); ok {
			return
		}
	}
	// Concatenate any text parts as the scanning surface. Real
	// responses are usually a single TextContent; concatenation
	// covers the (rare) multi-block case.
	var buf strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			buf.WriteString(t.Text)
		}
	}
	if buf.Len() == 0 {
		return
	}
	parts, ok := extractToolCallsFromText(buf.String(), allowed, toolFormatFromEnv())
	if !ok {
		return
	}
	resp.Parts = parts
	resp.FinishReason = message.FinishReasonToolUse
	noteFirstTextExtraction()
}

// toolFormatFromEnv reads KAI_OPENAI_TOOL_FORMAT. Empty defaults
// to "raw" (the most common shape). Read at every call rather than
// at startup so users tweaking env mid-debug don't need to restart.
func toolFormatFromEnv() string {
	return strings.TrimSpace(os.Getenv("KAI_OPENAI_TOOL_FORMAT"))
}
