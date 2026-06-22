package agent

import (
	"fmt"
	"regexp"
	"strings"

	"kai/internal/agent/message"
)

// formatHallucinationNudge plugs the offending filename(s) into the
// template so the model knows what specifically it has to take back.
// Quoting the names in backticks matches the way file paths are
// represented elsewhere in the chat (consistent with the TUI's
// rendering) and helps the model echo them naturally.
func formatHallucinationNudge(fabricated []string) string {
	quoted := make([]string, 0, len(fabricated))
	for _, f := range fabricated {
		quoted = append(quoted, "`"+f+"`")
	}
	return fmt.Sprintf(hallucinationGuardNudge, strings.Join(quoted, ", "))
}

// The hallucination guard prevents an agent from confidently naming
// files it never actually saw. Real failure mode (2026-05-12): user
// asks a chat question, opus-4-6 replies "It looks like you have an
// index.js and package.json here — want me to take a look?" — when
// the workspace is the kai monorepo with no index.js anywhere. The
// agent called 0 read-tools, didn't consult the auto-injected
// project overview, and improvised plausible-sounding filenames to
// be helpful. The guard intercepts this pattern and sends the agent
// back to consult kai_tree / view / kai_files first.
//
// Shape mirrors absence_guard.go: pure predicates here, runner
// wiring there.

// hallucinationGuardNudge is the user-role message injected into
// the transcript when the guard fires. Phrased as coaching ("your
// previous answer named X without seeing it") rather than
// contradiction so the model accepts it and revises rather than
// arguing.
const hallucinationGuardNudge = `Your previous answer named one or more files (%s) that do NOT appear in this conversation's context — neither in a tool result, the auto-injected project overview, nor the user's message. Naming files you haven't actually observed is hallucination; it produces confidently-wrong claims.

Before answering questions about what files exist or what the project contains:
  - Call kai_tree on the path you want to describe (depth=2 covers most "what's here" questions in one call).
  - Or call kai_files with a glob if you're testing for a specific shape ({"glob":"**/*.json"}).
  - Or call view on a file you have an exact path for.

Then re-answer using ONLY filenames that appear in a tool result or the project overview. If the user asked a question that doesn't actually need filenames (e.g. "why did you do X"), answer the question directly without inventing context.`

// fileMentionPattern matches code-shaped tokens in the final
// response that look like filenames: anything with a recognized
// source/config extension, possibly wrapped in backticks. Designed
// to OVER-match — false positives cost one extra turn, false
// negatives let the bug we're trying to fix slip through. The
// model uses backticks heavily in TUI output (the system prompt
// instructs it to), so most real file mentions land inside `…`.
//
// We do NOT match bare words without an extension ("Makefile",
// "Dockerfile") — too high false-positive rate against the
// agent's casual prose. The kai-tui case had extensions.
// Trailing \b anchors the extension so partial matches don't win
// over longer ones. Without it, RE2's left-to-right alternation
// matches "package.json" as "package.js" + leftover "on" because
// `js` comes before `json` in the alternation. \b requires a
// word-boundary after the extension, forcing the longer match.
var fileMentionPattern = regexp.MustCompile(
	"`?([A-Za-z0-9_./-]+\\.(?:" +
		"go|py|rs|tsx|ts|jsx|mjs|cjs|js|java|kt|swift|cc|cpp|cxx|c|hpp|h" +
		"|rb|php|cs|scala|ex|exs|erl|clj|hs|sh|bash|zsh|fish" +
		"|yaml|yml|toml|json|mdx|md|rst|txt|html|htm|scss|sass|css" +
		"|sql|graphql|proto|tfvars|tf|env|lock" +
		"))\\b`?",
)

// ExtractFileMentions pulls out file-shaped tokens from the
// assistant's final text. Each match's filename (without backticks)
// goes into the returned slice, deduplicated. Order preserved so
// the guard's nudge can mention them in the order the model wrote
// them.
func ExtractFileMentions(text string) []string {
	matches := fileMentionPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		f := m[1]
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// FabricatedFileMentions returns the subset of `mentions` that do
// NOT appear anywhere in the conversation context. "Context" =
// every prior user message (carries the project overview), every
// tool result, and every tool call input. We scan as raw text:
// substring match is enough because filenames are distinctive
// enough that a real reference won't be missed by simple
// containment, and the alternative (parsing JSON tool outputs) is
// over-engineering for a guard whose job is to catch confident
// fabrication.
//
// Returns an empty slice when every mention is grounded in
// context — meaning the agent saw the file somewhere before
// referencing it.
func FabricatedFileMentions(mentions []string, history []message.Message) []string {
	if len(mentions) == 0 {
		return nil
	}
	var sb strings.Builder
	for _, m := range history {
		// The final assistant message is the one being checked;
		// the runner passes history up to but not including it.
		// Walk every part-shape to collect the full text surface.
		sb.WriteString(m.Text())
		sb.WriteByte('\n')
		for _, p := range m.Parts {
			switch v := p.(type) {
			case message.ToolCall:
				sb.WriteString(v.Input)
				sb.WriteByte('\n')
			case message.ToolResult:
				sb.WriteString(v.Content)
				sb.WriteByte('\n')
			}
		}
	}
	context := sb.String()
	var fabricated []string
	for _, m := range mentions {
		if !strings.Contains(context, m) {
			fabricated = append(fabricated, m)
		}
	}
	return fabricated
}
