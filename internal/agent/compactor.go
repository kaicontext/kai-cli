package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/kaicontext/kai-engine/message"
	"kai/internal/agent/provider"
)

// compactThreshold is the share of the model's context window after
// which the runner triggers compaction. 0.80 leaves headroom for the
// next turn's prompt + response without cutting it close enough that
// a single large tool result tips the request over the limit.
const compactThreshold = 0.80

// defaultRecentWindow is the number of trailing messages preserved
// verbatim across a compaction. Six covers ~3 user/assistant turns
// — enough for the model to stay coherent while still letting the
// summary reclaim most of the window.
const defaultRecentWindow = 6

// toolResultRecentWindow is how many trailing messages keep their
// tool-result content verbatim. Older tool results get rewritten
// to a one-line summary so they stop costing the full result-byte
// price on every subsequent turn. Smaller than the compaction
// recentWindow because tool results are the heaviest single item
// in history (a 60-line kai_grep, a 100-line bash output) — we
// want to bound them aggressively. The model still sees the
// "what happened" headline, just not the full payload.
const toolResultRecentWindow = 4

// toolResultTrimMarker is the suffix flushed onto trimmed content
// so the trimmer recognizes its own work and skips re-trimming on
// subsequent passes. Keeping the marker stable also gives a
// debugging breadcrumb in case the trimmed shape ever surprises a
// reader.
const toolResultTrimMarker = " […trimmed by kai to save tokens]"

// defaultCompactionModel is the cheap, fast model used to generate
// the summary. Haiku is the right tool: compaction is a sub-task,
// not the main reasoning path, so we don't want to burn Sonnet/Opus
// budget on it.
const defaultCompactionModel = "claude-haiku-4-5-20251001"

// contextLimitFor returns the model's input-context window in
// tokens. Conservative defaults — better to compact slightly early
// than to 4xx the request when usage_input_tokens overshoots our
// estimate. New models added here as they ship.
func contextLimitFor(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "1m"):
		return 1_000_000
	case strings.Contains(m, "haiku"):
		return 200_000
	case strings.Contains(m, "sonnet"), strings.Contains(m, "opus"):
		return 200_000
	default:
		return 200_000
	}
}

// shouldCompact reports whether the previous turn's input-token
// count crossed the compaction threshold for this model. The check
// is done before the next provider.Send so the compacted history is
// what gets sent — this is the only chance to shrink the prompt
// before it's billed and (potentially) rejected.
func shouldCompact(lastInputTokens int, model string) bool {
	if lastInputTokens <= 0 {
		return false
	}
	limit := contextLimitFor(model)
	return float64(lastInputTokens) >= compactThreshold*float64(limit)
}

// budgetCompactThreshold is the share of the per-run MaxTotalTokens
// budget after which we proactively compact, separate from the
// per-turn context-window check. 0.70 leaves the remaining 30% of
// the budget to spend on shorter post-compaction turns before the
// hard cap trips. Without this, long planner runs blow past
// MaxTotalTokens (228k > 200k cap) even when no single turn ever
// gets close to 80% of the model's context window — the cumulative
// re-send of history across many turns burns the budget while every
// individual call stays under the per-turn threshold.
//
// 2026-05-14 dogfood: an "explain how X works" run hit
// "token budget exceeded (used 228333, cap 200000)" after the user
// asked successive follow-up questions; this trigger fires before
// that cliff.
const budgetCompactThreshold = 0.70

// shouldCompactByBudget reports whether the cumulative in+out token
// usage has crossed the budget threshold relative to the per-run
// MaxTotalTokens cap. Independent of model context size — purely a
// dollars-per-run guard. Returns false when no cap is configured
// (MaxTotalTokens == 0): caps are opt-in, so callers without a
// budget can't accidentally trigger this.
func shouldCompactByBudget(usedTokens, maxTotalTokens int) bool {
	if maxTotalTokens <= 0 || usedTokens <= 0 {
		return false
	}
	return float64(usedTokens) >= budgetCompactThreshold*float64(maxTotalTokens)
}

// compact summarizes the prefix of history before the recent window
// via a single LLM call (using a cheap model) and returns a new
// history with the summary inserted as a synthetic user/assistant
// pair followed by the recent turns verbatim.
//
// Graph-context blocks aren't preserved — they're re-injected fresh
// every turn by graphContextInjector, so the agent always has
// up-to-date structural knowledge. What survives is what the agent
// did: files modified, decisions made, errors resolved.
//
// The synthetic pair keeps the alternating-role invariant Anthropic
// requires. If the recent window happens to start on an assistant
// message we drop that one message rather than synthesize a fake
// user turn before it.
func compact(ctx context.Context, p provider.Provider, summarizerModel string, history []message.Message, recentWindow int) ([]message.Message, error) {
	if recentWindow < 1 {
		recentWindow = defaultRecentWindow
	}
	if len(history) <= recentWindow {
		return history, nil
	}
	cut := len(history) - recentWindow
	older := history[:cut]
	recent := history[cut:]

	summary, err := summarize(ctx, p, summarizerModel, older)
	if err != nil {
		return nil, err
	}

	pair := []message.Message{
		{
			Role: message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{
				Text: "Summary of earlier work in this session (older turns were compacted to free context):\n\n" + summary,
			}},
		},
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{message.TextContent{
				Text: "Acknowledged. Continuing from there.",
			}},
		},
	}
	if len(recent) > 0 && recent[0].Role == message.RoleAssistant {
		// Anthropic requires user→assistant alternation; the synthetic
		// assistant turn above means recent must lead with a user
		// message. Drop the orphan rather than fake one.
		recent = recent[1:]
	}
	out := make([]message.Message, 0, len(pair)+len(recent))
	out = append(out, pair...)
	out = append(out, recent...)
	return out, nil
}

// summarize calls the provider with a tight summarization prompt and
// returns the assistant's text reply. Capped at a small MaxTokens
// because the summary is meant to be terse — we want a few hundred
// tokens of decisions+files, not a paraphrase of the whole
// transcript.
func summarize(ctx context.Context, p provider.Provider, model string, older []message.Message) (string, error) {
	if model == "" {
		model = defaultCompactionModel
	}
	transcript := renderTranscriptForSummary(older)
	prompt := summaryInstructions + "\n\n--- TRANSCRIPT ---\n" + transcript

	req := provider.Request{
		Model:  model,
		System: "You are a terse, factual summarizer of agent sessions.",
		Messages: []message.Message{
			{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: prompt}}},
		},
		MaxTokens: 2048,
	}
	resp, err := p.Send(ctx, req)
	if err != nil {
		return "", fmt.Errorf("compaction: summarizer call: %w", err)
	}
	var b strings.Builder
	for _, part := range resp.Parts {
		if t, ok := part.(message.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("compaction: summarizer returned empty text")
	}
	return out, nil
}

// summaryInstructions matches the contract documented in the
// compaction spec: actions and decisions only, no codebase
// description (graph context handles that), no code snippets.
const summaryInstructions = `Summarize this agent session. Include only:
- Files created or modified (with paths)
- Functions added, changed, or deleted
- Decisions made and why
- Any errors hit and how they were resolved
Do not include code snippets. Do not describe the codebase structure.`

// renderTranscriptForSummary turns a slice of messages into a plain-
// text transcript the summarizer can read. Tool calls are condensed
// to "[tool: name] input"; tool results are truncated aggressively
// because raw bash output and file contents add up fast and the
// summarizer doesn't need them verbatim.
func renderTranscriptForSummary(msgs []message.Message) string {
	const maxToolResultLen = 400
	var b strings.Builder
	for _, m := range msgs {
		role := string(m.Role)
		if text := strings.TrimSpace(m.Text()); text != "" {
			b.WriteString(role)
			b.WriteString(": ")
			b.WriteString(text)
			b.WriteString("\n\n")
			continue
		}
		// No prose — likely a pure tool-call or tool-result message.
		// Keep just enough breadcrumb for the summarizer to know what
		// happened without reproducing payloads.
		for _, p := range m.Parts {
			switch v := p.(type) {
			case message.ToolCall:
				b.WriteString("[tool call ")
				b.WriteString(v.Name)
				b.WriteString("] ")
				b.WriteString(v.Input)
				b.WriteString("\n")
			case message.ToolResult:
				c := v.Content
				if len(c) > maxToolResultLen {
					c = c[:maxToolResultLen] + "...[truncated]"
				}
				if v.IsError {
					b.WriteString("[tool error] ")
				} else {
					b.WriteString("[tool result] ")
				}
				b.WriteString(c)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// trimOldToolResults rewrites the Content of every ToolResult
// part in history older than recentMsgs trailing messages to a
// terse one-liner. Tool results dominate context-window pressure
// in agent loops — a single kai_grep / bash run is hundreds of
// tokens, and that cost is paid again on every subsequent
// provider call until full compaction fires. Trimming early
// bounds that growth without losing the headline the model
// needs to remember "I called X and got 12 hits across 3 files."
//
// Mutates history in place. Idempotent: entries already carrying
// the trim marker are left alone.
func trimOldToolResults(history []message.Message, recentMsgs int) {
	if recentMsgs < 0 {
		recentMsgs = 0
	}
	if len(history) <= recentMsgs {
		return
	}
	cutoff := len(history) - recentMsgs
	for i := 0; i < cutoff; i++ {
		m := &history[i]
		for j, part := range m.Parts {
			tr, ok := part.(message.ToolResult)
			if !ok {
				continue
			}
			if strings.Contains(tr.Content, toolResultTrimMarker) {
				continue
			}
			m.Parts[j] = message.ToolResult{
				ToolCallID: tr.ToolCallID,
				Name:       tr.Name,
				Content:    summarizeToolResultContent(tr.Content),
				Metadata:   tr.Metadata,
				IsError:    tr.IsError,
			}
		}
	}
}

// summarizeToolResultContent reduces a tool result body to its
// headline (first non-blank line) plus a marker indicating how
// much got dropped. Most kai_* tools and bash already lead with
// a self-describing first line ("kai_grep: 12 hits across 3
// files", "kai_files: 47 match(es)"), so the headline is
// frequently all the model needs to recall what happened.
func summarizeToolResultContent(content string) string {
	if content == "" {
		return "(empty)" + toolResultTrimMarker
	}
	lines := strings.Split(content, "\n")
	head := ""
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			head = s
			break
		}
	}
	const maxHead = 200
	if len(head) > maxHead {
		head = head[:maxHead] + "…"
	}
	if head == "" {
		head = "(non-text content)"
	}
	return fmt.Sprintf("%s [orig: %d lines]%s", head, len(lines), toolResultTrimMarker)
}
