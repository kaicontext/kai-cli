package fixxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Prompt builders. Each takes the structured signal the REPL
// has and renders the prompt body sent to claude. Kept in
// this package so the wording of "what we ask claude to do"
// lives next to the worker that runs it.
//
// All prompts share the same framing: claude is debugging the
// kai CLI, knows it has full edit/bash autonomy in the repo,
// and should fix-and-rebuild without asking questions. The
// per-prompt body adds the specific signal (error / feedback /
// turn).

// errorPromptHeader is the standing instruction wrapped around
// every fixxy invocation. Pinned in code so tweaks to wording
// are reviewable.
const errorPromptHeader = `You are debugging the kai CLI itself. The kai team is dogfooding their own tool and just hit a case where kai-the-tool misbehaved. Your job is to fix kai-the-tool so it handles this kind of request better next time.

CRITICAL — read this twice:
  - You are NOT here to fulfill the user's underlying request. If the user asked kai to "fix the loading spinner" and kai did a bad job, you do NOT go fix the spinner. You fix the planner / agent / prompt / dispatch logic in the kai source so that next time a user makes that kind of request, kai handles it well.
  - The artifact under review is KAI'S OUTPUT, not the user's original task. Look at kai's response, ask "why did kai produce this instead of something useful?", and fix the underlying machinery that produced it.
  - Likely fix sites (in priority order):
      kai-cli/internal/planner/        (planner prompts, vagueness detection, work-plan schema)
      kai-cli/internal/agentprompt/    (system prompts handed to spawned agents)
      kai-cli/internal/agent/          (mode selection, tool whitelisting, dispatch)
      kai-cli/internal/orchestrator/   (agent fan-out, integration)
      kai-cli/internal/tui/views/      (rendering of plans / replies / errors)
  - If after investigating you decide kai actually did fine and the user's complaint isn't actionable in the source, say "nothing to fix in kai source — the request was X but kai did Y, which is reasonable" in one line and exit.

Ground rules:
  - Full autonomy. Don't ask questions. Make the fix.
  - Stay inside the kai repo (your cwd is the repo root).
  - When you change code, our wrapper will run 'go build' against ./kai-cli/cmd/kai after you exit. You don't need to build manually.
  - Be concise. Long monologues don't help here; the user is trying to keep working.

`

// BuildErrorPrompt renders the prompt for mode 1: a
// classifier-routed error fired and we want claude to look at
// it. errorLogTail is the last N lines of .kai/errors.log so
// claude has historical context.
func BuildErrorPrompt(kind, headline, raw string, errorLogTail string) string {
	var b strings.Builder
	b.WriteString(errorPromptHeader)
	b.WriteString("Signal: a kai error just fired.\n\n")
	fmt.Fprintf(&b, "  KIND:      %s\n", kind)
	fmt.Fprintf(&b, "  HEADLINE:  %s\n", headline)
	fmt.Fprintf(&b, "  RAW:       %s\n", raw)
	if tail := strings.TrimSpace(errorLogTail); tail != "" {
		b.WriteString("\nRecent errors.log entries (newest last):\n")
		b.WriteString(tail)
		b.WriteString("\n")
	}
	b.WriteString("\nDiagnose, fix, exit.")
	return b.String()
}

// BuildFeedbackPrompt renders the prompt for mode 2: the user
// said "no sir i don't like it" so we forward the recent
// conversation along with their complaint. recentTurns is
// pre-formatted by the caller (REPL has the turn structures
// handy; this package just concatenates).
func BuildFeedbackPrompt(complaint string, recentTurns string) string {
	var b strings.Builder
	b.WriteString(errorPromptHeader)
	b.WriteString("Signal: the kai user just told kai-the-tool that the recent answer was bad.\n\n")
	fmt.Fprintf(&b, "  COMPLAINT: %q\n", complaint)
	if turns := strings.TrimSpace(recentTurns); turns != "" {
		b.WriteString("\nRecent conversation (chronological):\n")
		b.WriteString(turns)
		b.WriteString("\n")
	}
	b.WriteString("\nReminder: do NOT attempt the user's original task. " +
		"The user already has a way to do that — they're complaining because " +
		"kai-the-tool didn't help them well. Fix kai's source (planner prompts, " +
		"dispatch, agent prompts, mode selection, output rendering) so the next " +
		"time someone makes a request of this shape, kai handles it correctly. " +
		"If the recent conversation references files in some other repo, ignore them — " +
		"those aren't yours to touch.")
	return b.String()
}

// BuildReviewPrompt renders the prompt for mode 3: every turn
// completes, claude reviews kai's behavior. Pass the user's
// request + kai's reply + any tool calls. Claude either says
// "looks fine" (we tell the user) or fixes something.
func BuildReviewPrompt(userRequest, kaiReply, toolCallSummary string) string {
	var b strings.Builder
	b.WriteString(errorPromptHeader)
	b.WriteString("Signal: kai just finished a turn. Mode 3 fixxy: review whether kai-the-tool handled it well.\n\n")
	fmt.Fprintf(&b, "  USER ASKED:  %s\n", truncate(userRequest, 800))
	fmt.Fprintf(&b, "  KAI REPLIED: %s\n", truncate(kaiReply, 1500))
	if tc := strings.TrimSpace(toolCallSummary); tc != "" {
		b.WriteString("  TOOL CALLS:\n")
		for _, line := range strings.Split(tc, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	b.WriteString("\nReminder: do NOT attempt the user's original task. " +
		"You're judging whether kai-the-tool handled the request well. " +
		"If kai over-fetched, hallucinated, missed obvious context, picked the " +
		"wrong mode, or rendered a confusing reply — fix the underlying behavior in " +
		"kai source (planner / agentprompt / agent / orchestrator / tui). " +
		"If kai's response was reasonable for what was asked, just say " +
		"\"nothing to fix here\" in one line and exit. Don't fix problems that " +
		"aren't there; mode 3 fires on EVERY turn so false positives multiply.")
	return b.String()
}

// ReadErrorLogTail returns the last n bytes of .kai/errors.log
// so the error prompt can include recent context. Best-effort:
// missing file or read errors return "". Capped at n bytes
// from the end of the file to avoid shipping a huge log.
func ReadErrorLogTail(kaiDir string, n int) string {
	path := filepath.Join(kaiDir, "errors.log")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) <= n {
		return string(data)
	}
	tail := data[len(data)-n:]
	// Snap to the next newline so we don't mid-line truncate.
	if i := strings.IndexByte(string(tail), '\n'); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:]
	}
	return string(tail)
}
