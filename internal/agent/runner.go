package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"kai/internal/agent/provider"
	"kai/internal/agent/session"
	"kai/internal/agent/tools"
	"kai/internal/authorship"
	"kai/internal/memstat"
	"kai/internal/projects"
	"kai/internal/runlog"
	"kai/internal/safetygate"
	"kai/internal/tasksmd"
)

// sendOneTurn dispatches one provider call. When the provider
// implements Streamer, we consume the SSE channel and forward
// text-delta events to Hooks.OnAssistantDelta so the TUI can
// render assistant prose live. Tool-use events accumulate into
// the final Response just like the non-streaming path; we don't
// dispatch tools speculatively yet (deferred — needs careful
// thinking about cancellation and rollback if the model hadn't
// truly finished the call when we acted).
//
// Falls back to the synchronous Send path automatically when the
// provider isn't a Streamer or when SendStream returns an error
// before the first event (which means SSE setup failed; the caller
// can recover).
// responseIsEmpty reports whether a successful provider Response
// carries no usable content — no text, no tool calls. The "Model
// returned no text" symptom: text="", FinishReason often
// max_tokens or length (reasoning ate the budget) or sometimes
// end_turn with empty parts (provider-side empty-completion bug
// or SSE stream that closed with no chunks). Used by
// sendWithRecovery's empty-retry path; one retry with doubled
// MaxTokens covers reasoning-model cases generically.
func responseIsEmpty(resp provider.Response) bool {
	for _, p := range resp.Parts {
		switch v := p.(type) {
		case message.TextContent:
			if strings.TrimSpace(v.Text) != "" {
				return false
			}
		case message.ToolCall:
			return false
		}
	}
	return true
}

// isReasoningModel reports whether the model identifier likely
// designates a model that emits a silent <think> reasoning trace
// against its output budget before producing visible text. Used to
// pre-allocate a larger MaxTokens budget so reasoning + visible
// output can coexist without one starving the other.
//
// Heuristic substring match — fast, brittle to new families, but
// the cost of a miss is at most one TUI-side empty-response retry
// (planner_dispatch.go does that fallback). Adding a new family
// is one line here.
//
// Known reasoning families as of 2026-05:
//   - Qwen3 (any qwen3 variant; Qwen2.5 likewise has some reasoning SKUs)
//   - OpenAI o1 / o3 / o4 / gpt-5 (when reasoning mode is on)
//   - DeepSeek-R1 / R2 / V4 family (V4-Pro observed burning ~4m of
//     hidden reasoning for ~500 visible tokens in a 2026-05-24 dogfood)
//   - Any model whose name contains "reasoning" or "-r1"
// IsReasoningModel is the exported alias of isReasoningModel for
// callers outside the agent package (notably the TUI's chat
// wall-clock-budget scaler). Same heuristic.
func IsReasoningModel(model string) bool { return isReasoningModel(model) }

// isReasoningModel delegates to the canonical classifier in the provider
// package (provider.IsReasoningModel) so the agent and provider sides
// can't diverge. They diverged once (2026-05-29): the provider copy
// matched only Qwen3, leaving DeepSeek-V4-Pro unfloored and returning
// empty completions. One source of truth prevents the repeat.
func isReasoningModel(model string) bool {
	return provider.IsReasoningModel(model)
}

// fallbackModelChain is the ordered list of model ids the runner
// tries when a primary model returns empty completions twice
// (original send + MaxTokens-doubled retry). Same models the
// classifier's "Model returned no text" error already recommends
// to the user — the failover automates the manual /model swap.
//
// Order matters: GLM-5.1 first because the 2026-05-19 reliability
// runs showed near-zero empty-response rate; Kimi second as a
// distinct family in case the failure is a GLM-side hiccup too.
var fallbackModelChain = []string{
	"z-ai/glm-5.1",
	"moonshotai/kimi-k2.6",
}

// pickFallbackModel returns the first model in the chain that is
// NOT the current model (case-insensitive substring match — handles
// "zai-org/GLM-5.1" vs "GLM-5.1" tagging variants). Returns "" if
// the current model is unknown or already a fallback (don't retry
// the same model under a different cap).
func pickFallbackModel(current string) string {
	cur := strings.ToLower(current)
	for _, m := range fallbackModelChain {
		mLow := strings.ToLower(m)
		// Skip if current contains this fallback's name (e.g.
		// current="zai-org/GLM-5.1" matches fallback "zai-org/GLM-5.1").
		if strings.Contains(cur, mLow) || strings.Contains(mLow, cur) {
			continue
		}
		return m
	}
	return ""
}

func sendOneTurn(ctx context.Context, opts Options, req provider.Request) (provider.Response, error) {
	streamer, ok := opts.Provider.(provider.Streamer)
	if !ok {
		return opts.Provider.Send(ctx, req)
	}
	ch, err := streamer.SendStream(ctx, req)
	if err != nil {
		// SSE setup failed (network, auth, 4xx). Fall back to
		// non-streaming so a transient SSE issue doesn't kill the
		// whole run.
		return opts.Provider.Send(ctx, req)
	}
	var final *provider.Response
	for ev := range ch {
		switch ev.Kind {
		case "text_delta":
			if opts.Hooks.OnAssistantDelta != nil && ev.Text != "" {
				opts.Hooks.OnAssistantDelta(ev.Text)
			}
		case "tool_use":
			// Don't fire OnToolCall here — dispatchToolCalls
			// fires it once the runner actually starts the tool
			// after the stream completes. Firing twice would put
			// duplicate breadcrumbs in the TUI.
		case "done":
			final = ev.Final
		case "error":
			return provider.Response{}, ev.Err
		}
	}
	if final == nil {
		return provider.Response{}, fmt.Errorf("agent: stream ended without done event")
	}
	return *final, nil
}

// sendWithRecovery wraps sendOneTurn with two recovery paths from
// the never-crash spec:
//
//   - Transient upstream errors (429 rate-limit, 529 overloaded,
//     network resets, gateway hiccups) trigger exponential backoff:
//     1s, 2s, 4s, 8s, 16s, 32s, 60s cap, up to maxRetryAttempts
//     attempts. The OnRetryWait hook fires before each sleep so the
//     TUI can render "rate limited, retrying in 4s…" instead of
//     leaving the user staring at silence.
//
//   - Context-overflow errors (413, "prompt is too long",
//     "context_length") tell the caller to compact and retry. We
//     return the original error so the runLoop top can see it,
//     compact the conversation, and re-call sendWithRecovery.
//
// Non-transient, non-overflow errors (auth, billing, malformed
// request) bubble up immediately — retrying won't help and the user
// needs to fix the root cause.
//
// Cancellation: ctx.Err() always wins. A canceled context shortcuts
// the backoff sleep and returns whatever error caused the loop to
// pause.
func sendWithRecovery(ctx context.Context, opts Options, req provider.Request) (provider.Response, error) {
	const maxRetryAttempts = 5
	// Empty-response retry: at most ONE provider-level retry on an
	// empty completion (no text, no tool calls). Bumps MaxTokens on
	// the retry so a reasoning model that ate the budget gets room
	// to produce visible output. Applies to every caller of
	// sendWithRecovery — planner, chat agent, gate review,
	// kai_consult — so the fix from v0.31.25 (which only covered
	// the chat agent layer) now covers everything. Set on first
	// empty response; checked once per call so the retry can't loop.
	emptyRetried := false
	var lastErr error
	for attempt := 0; attempt < maxRetryAttempts; attempt++ {
		resp, err := sendOneTurn(ctx, opts, req)
		if err == nil {
			// Empty-content check: the provider returned a Response
			// with no error but no usable content. Symptoms include
			// Qwen3 / reasoning models burning the budget on a
			// silent <think> trace, OR an SSE stream that closed
			// cleanly with no chunks, OR a provider-side empty-
			// completion bug. Retry once with a doubled budget
			// before surfacing.
			if !emptyRetried && responseIsEmpty(resp) && req.MaxTokens > 0 {
				emptyRetried = true
				retryReq := req
				retryReq.MaxTokens = req.MaxTokens * 2
				if opts.Hooks.OnRetryWait != nil {
					opts.Hooks.OnRetryWait(attempt+1, 0, fmt.Errorf("empty completion; retrying with MaxTokens=%d", retryReq.MaxTokens))
				}
				retryResp, retryErr := sendOneTurn(ctx, opts, retryReq)
				if retryErr == nil && !responseIsEmpty(retryResp) {
					return retryResp, nil
				}
				// MaxTokens retry also empty. Failover to a known-
				// reliable fallback model (same list kai's user-
				// facing error message recommends). If a fallback
				// produces real output, return it WITH a marker that
				// downstream rendering can surface — the user should
				// know the original model was swapped out for this
				// turn so they can adjust /model if it recurs.
				// 2026-05-26 dogfood: a wrap-up turn died with
				// "Model returned no text"; the work had succeeded
				// but the run ended in a visible error. Auto-failover
				// trades a one-turn model switch for a clean finish.
				if fb := pickFallbackModel(req.Model); fb != "" {
					failReq := retryReq
					failReq.Model = fb
					if opts.Hooks.OnRetryWait != nil {
						opts.Hooks.OnRetryWait(attempt+1, 0, fmt.Errorf("empty completion; failing over to %s", fb))
					}
					failResp, failErr := sendOneTurn(ctx, opts, failReq)
					if failErr == nil && !responseIsEmpty(failResp) {
						return failResp, nil
					}
				}
				// All retries empty. Fall through to return the
				// ORIGINAL response so the caller's existing empty-
				// handling (the TUI's "Model returned no text"
				// surface) still fires — same shape as today.
			}
			return resp, nil
		}
		lastErr = err
		// Context overflow is the runLoop's job to fix — return
		// immediately so it can compact and retry.
		if provider.IsContextOverflow(err) {
			return provider.Response{}, err
		}
		// Anything non-transient (auth, billing, bad request) is a
		// hard failure. Surface it.
		if !provider.IsTransient(err) {
			return provider.Response{}, err
		}
		// Final attempt failed — surface rather than sleep again.
		if attempt == maxRetryAttempts-1 {
			break
		}
		delay := backoffDelay(attempt)
		if opts.Hooks.OnRetryWait != nil {
			opts.Hooks.OnRetryWait(attempt+1, delay, err)
		}
		select {
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return provider.Response{}, lastErr
}

// backoffDelay returns the wait time for the given retry attempt
// (0-indexed). Base sequence is 1, 2, 4, 8, 16, 32, capped at 60s
// per the spec — exponential keeps short hiccups fast while the cap
// prevents minute-plus pauses on a long outage that's unlikely to
// resolve in this run anyway.
func backoffDelay(attempt int) time.Duration {
	const cap = 60 * time.Second
	d := time.Second << attempt
	if d <= 0 || d > cap {
		return cap
	}
	return d
}

// gateVerdictBag accumulates safety-gate verdicts produced inside
// the most recent mutating tool call. Mutating tools run inline (one
// at a time) per dispatchToolCalls' contract, so a single un-locked
// slice is safe — read-only tools that run concurrently never write
// here. The dispatcher drains the bag after each mutating call and
// appends a one-line summary to the tool result, so the model sees
// the gate verdict on its NEXT turn and can react to held / blocked
// outcomes (revert, ask the user, escalate).
type gateVerdictBag struct {
	notes []string
}

func (b *gateVerdictBag) push(verdict string, paths []string, radius int, reasons []string) {
	if b == nil {
		return
	}
	b.notes = append(b.notes, formatVerdictNote(verdict, paths, radius, reasons))
}

// drain returns the accumulated notes and clears the bag for the
// next mutating call.
func (b *gateVerdictBag) drain() []string {
	if b == nil || len(b.notes) == 0 {
		return nil
	}
	out := b.notes
	b.notes = nil
	return out
}

// formatVerdictNote renders a single line the model will read on
// its next turn. Format chosen to be both human-readable (for when
// the transcript is replayed in /resume) and easy to parse in case
// future agents want to programmatically condition on it.
func formatVerdictNote(verdict string, paths []string, radius int, reasons []string) string {
	pathLabel := strings.Join(paths, ", ")
	if pathLabel == "" {
		pathLabel = "(no paths)"
	}
	switch verdict {
	case "auto":
		return fmt.Sprintf("[GATE: auto ✓] %s — %d downstream", pathLabel, radius)
	case "review":
		reason := fmt.Sprintf("%d downstream", radius)
		if len(reasons) > 0 {
			reason = reasons[0]
		}
		return fmt.Sprintf("[GATE: held ⚠] %s — %s", pathLabel, reason)
	case "block":
		reason := fmt.Sprintf("%d downstream", radius)
		if len(reasons) > 0 {
			reason = reasons[0]
		}
		return fmt.Sprintf("[GATE: blocked ✗] %s — %s. Stop and ask the developer; do not retry without confirmation.", pathLabel, reason)
	case "error":
		reason := "classification failed"
		if len(reasons) > 0 {
			reason = reasons[0]
		}
		return fmt.Sprintf("[GATE: error] %s — %s", pathLabel, reason)
	default:
		return fmt.Sprintf("[GATE: %s] %s", verdict, pathLabel)
	}
}

// classifyAndEmit runs the safety gate on a freshly-mutated set of
// paths and forwards the verdict to the TUI hook. Cheap no-op when
// the gate truly can't run (no Graph) so callers can invoke it
// unconditionally after each mutation.
//
// Gate-every-write semantics: as long as a graph is wired, EVERY
// file mutation gets classified. A zero-value GateConfig
// (BlockThreshold == 0) is treated as "no operator policy" and we
// fall back to safetygate.DefaultConfig — strict on the auto side
// (only zero-blast-radius edits auto-promote) but permissive on
// blocking (only protected paths block). This keeps the developer-
// visible verdict on for every edit instead of silently disabling
// when the operator hasn't dropped a .kai/gate.yaml yet.
//
// We don't revert on Block here — agent-side rollback would mean
// re-reading the file's prior content per mutation, which we don't
// keep around. The verdict is informational; the existing
// orchestrator+gate path is the chokepoint that actually holds
// changes back from publish. In chat mode the user sees the verdict
// inline and can decide to revert, run /gate, or continue.
func classifyAndEmit(opts Options, paths []string, bag *gateVerdictBag) {
	dec, ok := classifyForGate(opts, paths)
	if !ok {
		return
	}
	if opts.Hooks.OnGateVerdict != nil {
		opts.Hooks.OnGateVerdict(paths, string(dec.Verdict), dec.BlastRadius, dec.Reasons)
	}
	bag.push(string(dec.Verdict), paths, dec.BlastRadius, dec.Reasons)
}

// classifyForGate runs the gate and returns the decision. Pulled out
// of classifyAndEmit so other call sites (the runner's tool-result
// injection path) can share the classification without depending on
// the OnGateVerdict hook. Returns ok=false when classification truly
// can't run (no Graph, no paths) so callers don't have to guard.
func classifyForGate(opts Options, paths []string) (safetygate.Decision, bool) {
	if opts.Graph == nil || len(paths) == 0 {
		return safetygate.Decision{}, false
	}
	cfg := opts.GateConfig
	if cfg.BlockThreshold == 0 {
		// Zero-value config means the operator hasn't loaded one;
		// fall back to defaults so gate-every-write stays on.
		cfg = safetygate.DefaultConfig()
	}
	dec, err := safetygate.Classify(context.Background(), paths, opts.Graph, cfg)
	if err != nil {
		return safetygate.Decision{
			Verdict: "error",
			Reasons: []string{err.Error()},
		}, true
	}
	return dec, true
}

// readOnlyTools is the set of tools safe to dispatch concurrently —
// they don't mutate the workspace, don't depend on each other's
// output, and don't compete for shared resources. Adding bash here
// would be wrong even for "ls": users issue `bash` for arbitrary
// commands and we don't introspect the command. Adding new read-only
// tools is intentional, not automatic — verify before extending.
var readOnlyTools = map[string]bool{
	"view":           true,
	"kai_callers":    true,
	"kai_dependents": true,
	"kai_context":    true,
	// Added 2026-05-11: the remaining read-only kai tools were
	// excluded from the dedupe + concurrent-dispatch set, which
	// surfaced in a kai-code session as `kai_grep "Enter sends"`
	// running THREE times (and kai_tree twice) for an identical
	// query inside a single turn. All four below are pure
	// read-only graph/fs queries with no shared mutable state:
	// safe to dispatch concurrently AND safe to dedupe.
	"kai_grep":    true,
	"kai_symbols": true,
	"kai_files":   true,
	"kai_tree":    true,
}

// editTools counts toward the per-run edit tally surfaced to the model
// on every tool result. Bash, kai_checkpoint, kai_diff and friends are
// intentionally omitted: a file mutation is the unambiguous signal
// that the agent has stopped exploring and started implementing.
var editTools = map[string]bool{
	"write": true,
	"edit":  true,
}

// bashReadCommands are shell commands that only inspect files or print
// state — they never mutate the workspace and never build. A `bash`
// turn made up solely of these counts against the read-streak gate,
// exactly like a `view`. Without this, a model can dodge the streak
// gate entirely by reading files through `cat`/`sed`/`head` instead of
// `view` — observed in the K2.6 benchmark, where t4/t7 read-looped via
// bash (`cat`, `sed -n`, `head`, `python3 -c open()`, `perl -pe ''`)
// straight into a `context deadline exceeded` because every read was
// invisible to the streak counter. Build/test/git/rm commands are
// deliberately absent: those turns stay streak-neutral, as before.
var bashReadCommands = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
	"sed": true, "awk": true, "grep": true, "egrep": true, "fgrep": true,
	"rg": true, "ag": true, "wc": true, "nl": true, "ls": true, "find": true,
	"tree": true, "stat": true, "file": true, "od": true, "xxd": true,
	"hexdump": true, "cut": true, "sort": true, "uniq": true, "column": true,
	"echo": true, "printf": true, "pwd": true, "true": true, "which": true,
	"basename": true, "dirname": true, "realpath": true, "diff": true,
	"cmp": true, "comm": true, "python": true, "python3": true, "perl": true,
	"ruby": true, "node": true,
}

// bashCallReads reports whether a `bash` tool call is a pure read: every
// segment of the command (split on `&&`, `||`, `;`, `|`) is a known
// read-only command and the command performs no output redirection.
// Anything unrecognized — a build, a `git` call, an unknown binary —
// fails closed to false so the streak gate never blocks on a guess.
func bashCallReads(input string) bool {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		return false
	}
	cmd := strings.TrimSpace(p.Command)
	if cmd == "" {
		return false
	}
	// Strip benign stderr plumbing so it doesn't trip the `>` write
	// check below.
	for _, noise := range []string{"2>&1", "2>/dev/null", "&>/dev/null", ">/dev/null"} {
		cmd = strings.ReplaceAll(cmd, noise, "")
	}
	// Quote-aware scan: split into command segments on `;`, `|`, `&&`,
	// `||`, newline — but only outside single/double quotes, so a `;`
	// or `>` inside `python3 -c "…"` is left alone. Any `>` outside
	// quotes is a redirection — the turn produced a file, not a read.
	var segs []string
	var cur strings.Builder
	var inS, inD bool
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case inS:
			if c == '\'' {
				inS = false
			}
			cur.WriteRune(c)
			continue
		case inD:
			if c == '"' {
				inD = false
			}
			cur.WriteRune(c)
			continue
		case c == '\'':
			inS = true
			cur.WriteRune(c)
			continue
		case c == '"':
			inD = true
			cur.WriteRune(c)
			continue
		case c == '>':
			return false
		case c == ';' || c == '\n':
			segs = append(segs, cur.String())
			cur.Reset()
			continue
		case c == '|':
			if i+1 < len(rs) && rs[i+1] == '|' {
				i++
			}
			segs = append(segs, cur.String())
			cur.Reset()
			continue
		case c == '&':
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
			}
			segs = append(segs, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(c)
	}
	segs = append(segs, cur.String())
	saw := false
	for _, seg := range segs {
		fields := strings.Fields(seg)
		// Drop leading VAR=value environment assignments.
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		name = name[strings.LastIndexByte(name, '/')+1:]
		if !bashReadCommands[name] {
			return false
		}
		saw = true
	}
	return saw
}

// readStreak thresholds bound the "reconnaissance never ends" failure
// mode. The failing add-config-show-command run did 37 read-only
// turns before its first (broken) edit; nothing in the loop ever
// said "stop reading." Soft nudge fires while the streak is at or
// above the lower threshold; hard block fires at the upper one and
// rejects further read-only calls until any edit lands. Both
// counters reset on any edit. A build/test/git bash turn or a
// kai_checkpoint-only turn is neutral — it neither extends nor resets
// the streak — so a "run the build" turn between reads doesn't game
// the limit either way. A read-only bash turn (cat/sed/head/grep/…)
// DOES extend the streak: see bashCallReads.
const (
	readStreakSoftNudge = 5
	readStreakHardBlock = 10
)

// testFight* bound the "fighting the test framework" loop: consecutive
// turns that RUN a test command but change no production code. That's
// the signature of yak-shaving on test infra after the real change is
// done — observed 2026-05-31 on add-cli-handler, where the agent landed
// the handler in 2.5 min, then burned ~9 min and ~24k output tokens
// rewriting a vitest test that couldn't pass (vi.mock can't intercept
// require() in CJS) and shipped it broken. Soft nudge first; hard-block
// further test RUNS (edits still pass, so the agent can remove/skip the
// test and finalize).
const (
	testFightSoftNudge = 3
	testFightHardBlock = 5
)

// resolveReadStreakThresholds returns the effective soft/hard
// thresholds for this run. Zero values in opts fall back to the
// package defaults so existing callers keep their behavior; the
// orchestrator overrides these per-agent based on declared scope so
// single-file tasks can't burn 10 turns on recon before any edit.
func resolveReadStreakThresholds(opts Options) (soft, hard int) {
	soft, hard = readStreakSoftNudge, readStreakHardBlock
	if opts.ReadStreakSoft > 0 {
		soft = opts.ReadStreakSoft
	}
	if opts.ReadStreakHard > 0 {
		hard = opts.ReadStreakHard
	}
	// Caller-supplied thresholds may invert under bad config; enforce
	// soft < hard so the soft-nudge window never empties out into a
	// straight jump to hard-block.
	if soft >= hard {
		soft = hard - 1
		if soft < 1 {
			soft = 1
		}
	}
	return soft, hard
}

// classifyTurnReads tallies a turn's tool calls into edit and
// read counts for streak-gate accounting, and forwards the
// contiguous-paging tracker. Returns:
//
//	edits       — number of edit-tool calls (write/edit) this turn
//	streakReads — number of reads that COUNT against the streak
//	                (a `view` paging continuation does not count)
//	addedReads  — total read tool calls (incl. paging continuations);
//	                used to update the per-run read total which
//	                reflects token cost, not investigation breadth
//	newFile, newEnd — updated continuous-paging tracker state
//
// The split between streakReads and addedReads is the whole point:
// per-run accounting must reflect token cost (every view costs
// tokens, paging or not), but the streak gate's job is to detect
// "this run has stopped progressing, only investigating," which
// paging through one large file is not.
func classifyTurnReads(calls []message.ToolCall, lastFile string, lastEnd int) (edits, substantiveEdits, streakReads, addedReads int, newFile string, newEnd int) {
	return classifyTurnReadsWithDecisions(calls, nil, lastFile, lastEnd)
}

// classifyTurnReadsWithDecisions is classifyTurnReads with the turn's
// intercept decisions threaded in so intercepted calls (deduped views,
// loop-guard stubs, etc.) are excluded from the tally — they never ran
// and returned no content, so they're neither cost nor investigation.
// classifyTurnReads is the nil-decisions wrapper kept for tests.
func classifyTurnReadsWithDecisions(calls []message.ToolCall, decisions []interceptDecision, lastFile string, lastEnd int) (edits, substantiveEdits, streakReads, addedReads int, newFile string, newEnd int) {
	newFile, newEnd = lastFile, lastEnd
	for i, c := range calls {
		// Intercepted calls never ran — they returned a stub (dedup
		// nudge, loop guard, etc.), not file content. They cost almost
		// nothing and represent no investigation, so they must not count
		// toward the read-streak gate OR the per-run read total.
		// Otherwise a deduped re-read would push the agent toward the
		// hard block for content it never actually received.
		if i < len(decisions) && decisions[i].Intercept {
			continue
		}
		switch {
		case readOnlyTools[c.Name]:
			addedReads++
			if c.Name == "view" {
				if file, start, end, ok := parseViewRange(c.Input); ok {
					if file == newFile && start == newEnd && file != "" {
						// Contiguous paging — don't count against streak.
						newEnd = end
						continue
					}
					newFile = file
					newEnd = end
				}
			}
			streakReads++
		case editTools[c.Name]:
			edits++
			if !isCosmeticEdit(c) {
				substantiveEdits++
			}
		case c.Name == "bash" && bashCallReads(c.Input):
			// A bash call that only reads files (cat/sed/head/grep/…)
			// is reconnaissance just like `view` — count it against
			// the streak so a model can't loop forever by reading
			// through the shell. Build/test/git bash turns stay
			// neutral (bashCallReads is false for them).
			addedReads++
			streakReads++
		}
	}
	return edits, substantiveEdits, streakReads, addedReads, newFile, newEnd
}

// isCosmeticEdit reports whether an edit-tool call changes only
// comments and whitespace — no actual code. Such an edit must NOT
// reset the read-streak gate.
//
// The 2026-05-15 dogfood pinned this: an agent discovered that any
// edit zeroes consecutiveReadTurns, and toggled a `// marker` comment
// on and off (five round-trips) purely to keep its read budget
// refilled while it paged files. Only a substantive, code-changing
// edit represents the explore→implement transition the streak gate
// is meant to reward.
//
// `write` is always treated as substantive: creating or overwriting a
// whole file is real progress, and the observed gaming used `edit`.
// Unparseable input fails open (treated as substantive) — the gate
// errs toward trusting the agent, not toward false blocks.
func isCosmeticEdit(c message.ToolCall) bool {
	if c.Name != "edit" {
		return false
	}
	var p struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if json.Unmarshal([]byte(c.Input), &p) != nil {
		return false
	}
	return stripCommentsAndSpace(p.OldString) == stripCommentsAndSpace(p.NewString)
}

// stripCommentsAndSpace removes /* */ block comments, // and # line
// comments, and all whitespace, so two snippets that differ only in
// comments or formatting compare equal. Deliberately language-
// agnostic and crude — it does not respect string literals that
// contain "//" or "#". A real edit mis-scored as cosmetic only costs
// the agent an earlier streak nudge, never a blocked edit, so the
// conservative direction is safe.
func stripCommentsAndSpace(s string) string {
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			break
		}
		rest := s[i+2:]
		j := strings.Index(rest, "*/")
		if j < 0 {
			s = s[:i]
			break
		}
		s = s[:i] + rest[j+2:]
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, r := range line {
			switch r {
			case ' ', '\t', '\r', '\v', '\f':
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func readStreakSoftNudgeText(soft int) string {
	return fmt.Sprintf(`[runner] You have made %d+ consecutive turns of read-only tool calls without editing any file. Reconnaissance should be nearly complete. Either:
- Make your first edit now (write or edit a file), OR
- State in one sentence what specific information is still missing.

Do not re-read files you've already seen — their content is in your context.`, soft)
}

// searchNoBashThreshold is the turn count at which the
// "search-without-bash" nudge starts firing. Three is past the
// first plausible "I'm orienting" pair of turns but well before
// the read-streak hard limit, so the nudge has time to change
// behavior before the reads-only block kicks in.
const searchNoBashThreshold = 3

// classifyTurnSearchVsBash reports whether this turn's tool calls
// included a bash invocation and/or a source-search invocation
// (kai_grep / kai_search / kai_files). Used by the runner to
// detect the "still hunting for a CLI output field in source"
// pattern that the 2026-05-26 dogfood pinned: 11 turns of
// grep/search across kai for 'snapshot_count' / 'SnapshotCount'
// when one 'kai stats --json' would have answered the question.
// The classifier does NOT care whether the search returned hits;
// the failure shape is repeated searching, not the search outcome.
func classifyTurnSearchVsBash(calls []message.ToolCall) (usedBash, usedSearch bool) {
	for _, c := range calls {
		switch c.Name {
		case "bash":
			// Only verification-shaped bash counts. The 2026-05-26
			// edges dogfood gamed the naive "any bash resets" rule:
			// the model issued `which kai 2>/dev/null || echo "..."`
			// on turn 7 — pure shell hygiene, no contract verified —
			// which cleared the streak and suppressed the nudge for
			// the remaining turns. Hygiene commands (which / pwd /
			// ls / cat / echo / cd alone) don't reset the streak;
			// CLI-with-subcommand invocations and --json/--help
			// probes do. See bashIsVerification.
			if bashIsVerification(c.Input) {
				usedBash = true
			}
		case "kai_grep", "kai_search", "kai_files":
			usedSearch = true
		}
	}
	return
}

// bashCallRunsTests reports whether a bash tool call invokes a test
// runner — the signal the test-fight guard counts. Broad substring
// match across stacks; a false positive only costs a slightly early
// nudge.
func bashCallRunsTests(input string) bool {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		return false
	}
	cmd := strings.ToLower(p.Command)
	for _, pat := range []string{
		"vitest", "jest", "mocha", "pytest", "phpunit", "rspec",
		"go test", "cargo test", "npm test", "npm run test",
		"pnpm test", "yarn test", "npx vitest", "npx jest",
	} {
		if strings.Contains(cmd, pat) {
			return true
		}
	}
	return false
}

// isTestFilePath reports whether a path looks like a test file. Mirrors
// the tools-package helper; kept local to avoid a cross-package import.
func isTestFilePath(p string) bool {
	l := strings.ToLower(p)
	base := l
	if i := strings.LastIndex(l, "/"); i >= 0 {
		base = l[i+1:]
	}
	return strings.HasSuffix(l, "_test.go") ||
		strings.Contains(l, ".test.") ||
		strings.Contains(l, ".spec.") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(l, "/tests/") ||
		strings.Contains(l, "/test/") ||
		strings.HasPrefix(l, "tests/") ||
		strings.HasPrefix(l, "test/") ||
		strings.Contains(l, "__tests__/")
}

// classifyTurnTestFight reports whether a turn (a) ran a test command
// and (b) made a PRODUCTION (non-test) edit. The test-fight streak
// climbs on ran-tests-without-prod-edit turns and resets on any
// production edit — so legitimate TDD (which changes production code as
// it goes) never trips it, while "rewrite the test, re-run, repeat"
// after the real change is done does.
func classifyTurnTestFight(calls []message.ToolCall) (ranTests, prodEdit bool) {
	for _, c := range calls {
		switch c.Name {
		case "bash":
			if bashCallRunsTests(c.Input) {
				ranTests = true
			}
		case "edit", "write":
			var p struct {
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal([]byte(c.Input), &p) == nil && p.FilePath != "" && !isTestFilePath(p.FilePath) {
				prodEdit = true
			}
		}
	}
	return
}

// testFightNudgeText is the soft per-turn reminder once the streak hits
// the soft threshold. Positive-instruction: names the concrete way out.
func testFightNudgeText(streak int) string {
	return fmt.Sprintf("[runner] You've run the test suite %d turns in a row without changing production code. If your implementation is already done and the test is failing for a FRAMEWORK/TOOLING reason — a mock that won't intercept, module resolution, missing deps, env — rather than a real bug in your code, STOP re-fighting it. Instead: keep the implementation, then either (a) extract the unit under test into its own importable module so it can be tested without that mock, or (b) remove or skip the un-runnable test and leave a one-line TODO naming why. Re-rewriting the test is not progress.", streak)
}

// testFightBlockMessage hard-blocks a further test RUN once the streak
// hits the hard threshold. Edits still pass, so the agent can remove or
// skip the test and finalize.
func testFightBlockMessage() string {
	return "[runner] Test-tooling fight: you've re-run the test suite repeatedly without changing production code and without it going green. Further test RUNS are blocked this turn (edits still work). The implementation stands — finalize now: keep the production change, then either extract the unit for testability or remove/skip the un-runnable test with a one-line TODO. Do not run the test again."
}

// bashIsVerification reports whether a bash invocation is doing
// verification work (running a real CLI, checking external contracts)
// vs. shell hygiene that the model uses to look busy without actually
// verifying anything. The leading executable name after any "cd X &&"
// prefix-strip is matched against bashHygieneCommands; anything not
// on that list counts as verification. Empty / unparseable input
// returns false (caller doesn't want to credit unknown shapes).
func bashIsVerification(input string) bool {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(input), &p); err != nil || strings.TrimSpace(p.Command) == "" {
		return false
	}
	cmd := stripCDPrefix(p.Command)
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	// Take the leading binary basename (handles /usr/bin/which → which).
	head := strings.ToLower(filepath.Base(fields[0]))
	return !bashHygieneCommands[head]
}

// bashHygieneCommands lists shell-hygiene invocations that should NOT
// reset the search-without-bash streak. These are commands the model
// reaches for to "look like" verification without invoking any real
// CLI contract — binary discovery (which/where), cwd manipulation
// (pwd/cd), filesystem listing (ls), output noise (echo/printf), file
// reads that view already handles (cat/head/tail/less/more), and
// no-ops (true/false/:). Anything else — npm, kai, git, go, cargo,
// python, custom binaries — counts as verification.
var bashHygieneCommands = map[string]bool{
	"which": true, "where": true, "type": true, "command": true,
	"pwd": true, "cd": true,
	"ls": true, "echo": true, "printf": true,
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
	"true": true, "false": true, ":": true,
}

// stripCDPrefix returns cmd with any leading "cd <dir> &&" or
// "cd <dir>;" removed so the verification classifier evaluates the
// actual command rather than the cwd change. The 2026-05-26 dogfood
// often shows commands shaped like "cd /Users/.../kai-desktop && kai
// stats --json" — without strip, the leading exec is cd (hygiene)
// and the verification kai call is missed.
func stripCDPrefix(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "cd ") {
		return trimmed
	}
	// Find the first separator that ends the cd command.
	for _, sep := range []string{"&&", ";"} {
		if i := strings.Index(trimmed, sep); i > 0 {
			return strings.TrimSpace(trimmed[i+len(sep):])
		}
	}
	// cd alone with no follow-up command — hygiene.
	return trimmed
}

// promptHasExploreDirective returns true when the agent's initial
// prompt contains an EXPLORE: block that names a verification step.
// kai's planner emits this verbatim ("EXPLORE: max N turns — run
// <command>") for executor agents that need to verify an external
// contract before editing. The check is intentionally narrow — we
// match the literal "EXPLORE:" token rather than any reconnaissance
// keyword — so the gate fires only on prompts shaped like a kai
// plan output, not on arbitrary mention of "explore" in prose.
func promptHasExploreDirective(prompt string) bool {
	// Strip the planner's structured envelope so the match anchors
	// on a real EXPLORE block, not the word appearing in a system
	// preamble or a stack trace.
	if !strings.Contains(prompt, "EXPLORE:") {
		return false
	}
	// Require the gate ONLY if there's a clear directive to run
	// something — bash, a CLI invocation, or a JSON probe. Without
	// this, prompts that mention EXPLORE as a header without
	// actually asking for verification could be unfairly gated.
	for _, marker := range []string{"--json", "--help", "run ", "bash", "kai ", "invoke"} {
		if strings.Contains(prompt, marker) {
			return true
		}
	}
	return false
}

// exploreBeforeEditBlockMessage returns the text shown to the model
// when its first write/edit is blocked because the prompt's EXPLORE
// directive hasn't been honored. The 2026-05-26 snapshot-count
// executor pinned this exact failure shape: planner said "run kai
// snapshot list --json", executor skipped that step and shipped
// half a fix with the wrong field source. The message points back
// at the EXPLORE block and tells the model what to do (call bash).
func exploreBeforeEditBlockMessage() string {
	return `[runner] BLOCKED: this write/edit was rejected because your plan starts with an EXPLORE step and you have not run any bash command yet. The EXPLORE step is NOT optional — it is the verification phase that prevents the executor from making assumptions about file shape, JSON contracts, or command behavior that the planner could not check.

What to do RIGHT NOW:
1. Look at the EXPLORE: block in your prompt. Find the literal command it asks you to run (often a "kai <subcmd> --json" or "<cli> --help" invocation).
2. Call bash with that exact command. Read the output.
3. Then re-attempt your edit — the gate releases after the first bash call.

The 2026-05-26 snapshot-count executor shipped a half-fix because its plan said "run kai snapshot list --json to see the exact JSON shape if possible" and that step was treated as optional. It wasn't. The "if possible" hedge meant "only skip if the command literally does not exist on this system" — not "skip if you feel like guessing." Run the command first.`
}

// searchWithoutBashNudgeText is the runner note injected when the
// search-without-bash streak hits threshold. Concrete-first: the
// model needs to know WHICH command to run, not just "run something."
// The 2026-05-26 v0.32.48 dogfood showed that abstract "RUN THE
// COMMAND" nudge got the model to use bash, but it ran the wrong
// thing (grepped the built JS bundle instead of invoking the CLI).
// This revision gives the exact grammar: <CLI> --help to discover,
// <CLI> <subcmd> --json to see the field shape.
func searchWithoutBashNudgeText(streak int) string {
	return fmt.Sprintf(`[runner] You have made %d+ consecutive turns calling kai_grep / kai_search / kai_files without invoking any external command via bash. If your question is about what an external command emits — a JSON field shape, CLI output keys, structured data behind a tool — INVOKE THE COMMAND. Grepping source for the field name (or cat'ing a built bundle and grepping it) is slower and will miss real emitters (struct tags, fmt.Printf, templated strings).

What "invoke the command" means concretely:
  1. Discover the command shape:    bash {"command": "<cli-name> --help"}
  2. Discover the subcommand shape: bash {"command": "<cli-name> <subcmd> --help"}
  3. See the actual JSON output:    bash {"command": "<cli-name> <subcmd> --json"}

DO NOT use bash to cat/grep a built bundle (dist/*.js, build/*.css, *.min.js) — that is grep wearing a bash mask. The right call is INVOKE the binary.

Worked example from the 2026-05-26 dogfood: the question was "does kai expose a snapshot count?" The model burned 11 turns hunting "snapshot_count" / "SnapshotCount" / "total_snapshots" across kai's source. The right move was: bash {"command": "kai stats --json"}. That returns {total_lines, ai_lines, human_lines, ai_pct, by_agent} — no snapshot field. ONE bash call answered "that field does not exist" definitively. The next step from there is "find the right kai subcommand (kai snapshot list --json) or accept that this UI element needs a new field."

Concrete check: is the next thing you would type a snake_case or PascalCase identifier you expect to find as a JSON field? If yes, INVOKE THE COMMAND FIRST. If no and the symptom is purely source-side, ignore this nudge and continue.`, streak)
}

// bashErrSigRingSize caps the cross-turn error-signature ring.
// Sized to hold a few recent distinct error classes — enough to
// recognize "we've seen this one before" without unbounded memory.
const bashErrSigRingSize = 8

// bashErrSigRingContains reports whether sig already appears in
// the ring. Linear scan; the ring is tiny so big-O doesn't matter.
func bashErrSigRingContains(ring []string, sig string) bool {
	for _, s := range ring {
		if s == sig {
			return true
		}
	}
	return false
}

// pushBashErrSigRing appends sig to the ring. If sig is already
// present, it stays in its original position (no re-ordering — we
// don't need recency; we need set membership). If the ring is at
// capacity, the OLDEST entry is dropped to make room. Returns the
// new ring slice (may share backing array with the input).
func pushBashErrSigRing(ring []string, sig string) []string {
	if bashErrSigRingContains(ring, sig) {
		return ring
	}
	if len(ring) >= bashErrSigRingSize {
		// Drop the oldest. copy keeps the slice's backing array
		// and avoids allocating a new one.
		copy(ring, ring[1:])
		ring = ring[:len(ring)-1]
	}
	return append(ring, sig)
}

// normalizeBashErrSig extracts a stable signature from a bash tool
// result's content. The goal: detect when the BUILD/TEST error
// fundamentally changed shape between turns. Two errors with the
// same file:line:col or the same first error-keyword get the SAME
// signature; two errors with different shapes get DIFFERENT
// signatures. False positives (sig changed but problem hasn't) are
// preferable to false negatives (sig stable when problem changed) —
// the runner note this gates is a soft nudge, not a hard block.
//
// Algorithm: scan for the first line that contains one of:
//   - "error" / "Error" / "ERROR"
//   - "Exception" / "exception"
//   - "failed" / "FAILED"
//   - a file:line:col pattern (e.g. "foo.go:12:3")
//
// Take the first ~120 chars of that line as the raw signature. Then
// strip variable noise: absolute paths (replace with basename),
// process IDs, timestamps. Return the cleaned form.
//
// Empty when no recognizable error line is found — in that case the
// caller skips the change-detection check (no signal to compare).
func normalizeBashErrSig(content string) string {
	if content == "" {
		return ""
	}
	// Scan line-by-line for the first error-shaped line.
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		hasError := strings.Contains(lower, "error") ||
			strings.Contains(lower, "exception") ||
			strings.Contains(lower, "failed") ||
			fileLineColRE.MatchString(line)
		if !hasError {
			continue
		}
		// Strip absolute paths down to basename so the signature
		// doesn't change just because the run happened in a
		// different cwd (e.g. /tmp/kai-spawn-... vs the main repo).
		clean := absPathRE.ReplaceAllStringFunc(line, func(p string) string {
			return filepath.Base(p)
		})
		// Cap length so a single huge error line doesn't dominate
		// memory or comparison cost.
		if len(clean) > 200 {
			clean = clean[:200]
		}
		return clean
	}
	return ""
}

// fileLineColRE matches "path:line:col" or "path:line" patterns
// commonly emitted by compilers / linters / test runners.
var fileLineColRE = regexp.MustCompile(`[\w./-]+:\d+(:\d+)?`)

// absPathRE matches absolute Unix paths so we can normalize spawn-
// vs-main-repo path differences in error signatures.
var absPathRE = regexp.MustCompile(`/(?:private/)?(?:tmp|var|Users|home)/[\w./-]+`)

// truncSig clamps a signature to maxLen chars for inclusion in the
// runner note. Adds an ellipsis when truncated.
func truncSig(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// perTurnReadCapMessage tells the model it dispatched more reads in
// a single turn than the cap allows. Names the cap explicitly so the
// model can self-pace: the first N reads always run, only the
// excess get intercepted.
//
// Message style: prescribe ONE positive next action, not a menu of
// options. The earlier wording offered "issue write/edit, bash, or
// end the turn" — and models faced with a multi-choice prompt picked
// the most agentic-looking option (write/edit) and confabulated
// content they hadn't actually read. The 2026-05-24 kai-desktop
// dogfood produced exactly this pathology: agent hit cap at read 5,
// then created kai-desktop/src/kai-theme.css with invented CSS
// values instead of ending the turn.
//
// New rule: tell the model to end the turn with a structured
// finalize-or-block message. Calling write/edit is still mechanically
// allowed (the cap only blocks reads), but we don't suggest it as
// an option when the model may not yet have read what it'd edit.
func perTurnReadCapMessage(cap int) string {
	return fmt.Sprintf(`[runner] Per-turn read cap reached: you issued %d read-only tool calls this turn; further reads in this turn will be rejected. End the turn now with one of:
- FINALIZE: state the FILE / INSERTION POINT / SYMBOL you've identified from the reads you've already made, plus the one-sentence change you'd apply next turn. Next turn the cap resets and you can read more or make the edit.
- BLOCKED: "I'm blocked because <specific gap> — I need to read <X> next turn to proceed."
Do not call write/edit this turn unless you have already read the exact file content you would be editing. If you have not read it, FINALIZE or BLOCKED is the correct action.`, cap)
}

// readStreakBlockMessage fires when the agent has racked up several
// consecutive read-only turns without producing any edit. Same
// positive-instruction discipline as perTurnReadCapMessage: prescribe
// concrete next actions, none of which is "edit with invented data."
func readStreakBlockMessage(hard int) string {
	return fmt.Sprintf(`[runner] Read limit reached: %d consecutive read-only turns without any edit. Further read tools are rejected this turn. Take one of these specific actions:
- MAKE THE SMALLEST DEFENSIBLE EDIT: write/edit a single file with a change you can cite directly from a previous read. Quote the line you're changing or the file path you're creating, sourced from a read this run.
- ESCALATE: call kai_consult with goal (one sentence), tried (your last 3 reads in "tool args → result" form), and blocked_by (one sentence on what's stuck). The stronger model returns a diagnosis — not code.
- END THE TURN WITH "I'm blocked because <specific gap> — I need <Y> from the developer."`, hard)
}

// budgetFinalizePrompt is the single-shot recovery message injected
// when an agent run exhausts its token budget mid-exploration. The
// 2026-05-26 dogfood pinned the failure shape: the agent had already
// gathered enough evidence to make a wise guess, but kept reading
// more files until cap, then the run errored out and the user got
// nothing for the spend. New behavior: ONE more turn with this
// prompt, which forbids further exploration and forces a concrete
// commit based on what's already in scope. Verbosity is the failure
// mode here — short, decisive, exit.
func budgetFinalizePrompt(used, cap int) string {
	return fmt.Sprintf(`[runner] BUDGET EXHAUSTED — STOP EXPLORING, COMMIT NOW. Used %d / cap %d tokens. You have already gathered enough context to make a wise guess. Do NOT call any more read tools (view, kai_grep, kai_files, kai_context, kai_callers, kai_callees, kai_tree, kai_search, kai_consult). Do NOT re-investigate.

Based ONLY on the tool results and file content already in this conversation, commit to a single concrete next step in this turn:

(a) MAKE THE EDIT: call write or edit with the specific change you'd propose. Cite the file + line from a read above. If your confidence is below 70%%, still pick the most likely change — the user will tell you if it's wrong.

(b) STATE THE FIX IN PROSE: if no tool result you've already seen gives you a clear target, write 2-3 sentences naming (file path, line range, what to change, why). The user will apply it manually.

Pick (a) or (b). Be brief — extra words burn the few tokens we have left to finish this turn cleanly.`, used, cap)
}

// dispatchToolCalls runs the model's tool calls and returns one
// tool_result per call, preserving call order in the result slice
// (Anthropic matches by tool_use_id, but ordered results read better
// when persisted to the transcript and replayed). Read-only calls
// fan out into goroutines; mutating calls run inline. The two
// classes are interleaved correctly because we collect a slot per
// call and fill it in place — ordering is by call index, not finish
// time.
//
// gateBag, when non-nil, captures safety-gate verdicts produced
// inside mutating tools. After each mutating call returns, the
// dispatcher drains the bag and appends a one-line `[GATE: ...]`
// trailer to the tool result. The model reads this on its next turn
// and can react: it should stop and ask the developer when it sees
// `blocked ✗`, and proceed cautiously when it sees `held ⚠`.
func dispatchToolCalls(
	ctx context.Context,
	calls []message.ToolCall,
	registry map[string]tools.BaseTool,
	onCall func(name, inputJSON string),
	gateBag *gateVerdictBag,
	dedupeCache map[string]string,
	onCallDone func(name, inputJSON string, outputBytes int, durationMs int64, isError bool, errMsg string),
	bashFirst *bashFirstGate,
	projectSet *projects.Set, // used by canonicalToolInput to map project-prefixed paths to absolute form
) []message.ContentPart {
	results := make([]message.ContentPart, len(calls))
	var wg sync.WaitGroup
	// Read-only tools spawn one goroutine per call (see the loop
	// below). They all read+write dedupeCache, so the map needs a
	// mutex — Go maps panic via runtime.throw on concurrent
	// writes, which bypasses recover(). Without this lock the
	// runner crashes the process the first time the model issues
	// two read-only tools in a single turn.
	var dedupeMu sync.Mutex

	exec := func(idx int, call message.ToolCall) message.ToolResult {
		if onCall != nil {
			onCall(call.Name, call.Input)
		}
		// Per-call timer for the run-log recorder. Captured at
		// function entry so registry-miss / dedupe / error paths
		// all report a duration too — partial runs are the most
		// useful to debug.
		started := time.Now()
		report := func(content string, isError bool, errMsg string) {
			if onCallDone == nil {
				return
			}
			onCallDone(call.Name, call.Input, len(content), time.Since(started).Milliseconds(), isError, errMsg)
		}
		tool, ok := registry[call.Name]
		if !ok {
			// Helpful tool-not-found: when the model invokes a
			// tool that isn't in the current mode's allowlist
			// (the classic case: 'edit' in planning mode), it
			// previously got a bare "unknown tool: edit" and
			// fell back to invoking bash with a hallucinated
			// path. Now it gets the available-tools list so it
			// can pivot to the right call (view + describe, or
			// switch modes) on the next turn.
			content := unknownToolMessage(call.Name, registry)
			report(content, true, "unknown_tool")
			return message.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    content,
				IsError:    true,
			}
		}
		// First-call bash gate. When the user reported an error
		// without pasting it, force the agent to run the project
		// before reading source. We let bash through (and
		// kai_diagnose, in case the agent extracted a partial error
		// from the user's wording — it's a graph query, not a read);
		// everything else returns a tool error pointing at bash.
		// Cleared on the first bash call regardless of exit code.
		if bashFirst != nil && bashFirst.IsArmed() {
			switch call.Name {
			case "bash":
				bashFirst.Disarm()
			case "kai_diagnose":
				// pass-through; the model has an error string and
				// is doing graph triage, not speculative reading
			default:
				content := bashFirstRejectMessage(call.Name)
				report(content, true, "bash_first_gate")
				return message.ToolResult{
					ToolCallID: call.ID,
					Name:       call.Name,
					Content:    content,
					IsError:    true,
				}
			}
		}
		// Per-run dedupe of read-only tool calls. The planner
		// agent (and Sonnet 4.6 in general) tends to re-issue the
		// same view/grep with identical args several times in a
		// run as a verification habit. The result hasn't changed,
		// so we return the cached content and append a one-line
		// note so the model sees the duplicate flagged. Saves
		// real money on cache_read tokens (a re-view of a 1k-line
		// file pulls back ~20k tokens) and breaks "let me check
		// one more time" loops by making the loop visibly
		// non-productive.
		//
		// Mutating tools (write/edit/bash) skip the cache —
		// every invocation has side effects.
		var dedupeKey string
		if dedupeCache != nil && readOnlyTools[call.Name] {
			dedupeKey = call.Name + "\x00" + canonicalToolInputForCache(call.Input, projectSet)
			dedupeMu.Lock()
			cached, hit := dedupeCache[dedupeKey]
			dedupeMu.Unlock()
			if hit {
				content := cached + "\n\n[deduped: identical " + call.Name + " was called earlier this run; result is unchanged. If you need fresh data, change your arguments.]"
				report(content, false, "")
				return message.ToolResult{
					ToolCallID: call.ID,
					Name:       call.Name,
					Content:    content,
				}
			}
		}
		tr, err := tool.Run(ctx, tools.ToolCall{
			ID:    call.ID,
			Name:  call.Name,
			Input: call.Input,
		})
		if err != nil {
			content := fmt.Sprintf("tool error: %s", err.Error())
			report(content, true, err.Error())
			return message.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    content,
				IsError:    true,
			}
		}
		// Cache successful read-only results for the rest of the
		// run. Errors and tool-reported is_error responses are
		// NOT cached — the next call may legitimately succeed
		// (e.g. a missing file gets created by a sibling tool).
		if dedupeKey != "" && !tr.IsError {
			dedupeMu.Lock()
			dedupeCache[dedupeKey] = tr.Content
			dedupeMu.Unlock()
		}
		errMsg := ""
		if tr.IsError {
			errMsg = "tool_reported_error"
		}
		report(tr.Content, tr.IsError, errMsg)
		return message.ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    tr.Content,
			Metadata:   tr.Metadata,
			IsError:    tr.IsError,
		}
	}

	for i, call := range calls {
		// Per-call ctx check: if the user cancelled while a prior
		// tool was running (or between tools), short-circuit the
		// remaining dispatch instead of firing every queued call to
		// completion. Each tool.Run honors ctx internally too, but
		// re-checking here means cancellation drains the queue
		// promptly rather than after the last in-flight tool.
		// Round-21 dogfood: a model turn ended with multiple bash
		// + edit calls; user hit cancel; the runner kept dispatching
		// for ~30s while each call ran to completion.
		if err := ctx.Err(); err != nil {
			results[i] = message.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    "tool dispatch cancelled: " + err.Error(),
				IsError:    true,
			}
			continue
		}
		if readOnlyTools[call.Name] {
			wg.Add(1)
			go func(idx int, c message.ToolCall) {
				defer wg.Done()
				results[idx] = exec(idx, c)
			}(i, call)
			continue
		}
		// Mutating call: drain any in-flight reads so writes observe
		// a consistent state, then run inline. Subsequent reads in
		// the same batch will spawn fresh goroutines after the write
		// completes — happens-before is by-call-index, which is the
		// model's intent.
		wg.Wait()
		tr := exec(i, call)
		// Drain the gate bag and append any verdicts. Mutating tools
		// run inline so the bag is the verdicts produced *by this
		// call* — no contention with read-only goroutines (they
		// don't fire OnDiff/OnFilesChanged).
		if notes := gateBag.drain(); len(notes) > 0 {
			tr.Content = appendGateNotes(tr.Content, notes)
		}
		// Cache invalidation: when a mutating tool succeeds, evict
		// any cached read-only results that referenced the touched
		// path. Otherwise the agent edits a file, re-views it on the
		// next turn, and gets the pre-edit content from cache —
		// which sends it into a confused "my edit didn't apply" loop.
		if !tr.IsError && dedupeCache != nil {
			if paths := extractMutatedPaths(call.Name, call.Input); len(paths) > 0 {
				dedupeMu.Lock()
				for _, p := range paths {
					evictCacheForPath(dedupeCache, p)
				}
				dedupeMu.Unlock()
			}
		}
		results[i] = tr
	}
	wg.Wait()
	return results
}

// appendGateNotes adds gate-verdict trailers to a tool's result
// content. Separator picked so the model sees the verdicts as a
// distinct block at the end, not inline with the tool's own output.
// Empty notes → original content unchanged.
func appendGateNotes(content string, notes []string) string {
	if len(notes) == 0 {
		return content
	}
	if content == "" {
		return strings.Join(notes, "\n")
	}
	return content + "\n\n" + strings.Join(notes, "\n")
}

// runLoop is the in-process agent loop. It dispatches tool calls, feeds
// results back to the model, and stops when the model emits an
// end_turn (or hits a budget / cancellation).
//
// Slice 1 contract:
//   - Single Provider, single Hooks, fixed tool set.
//   - Tool registry is built from opts; runner doesn't know about
//     specific tools (file vs graph vs bash) — it just dispatches by
//     name against the registry.
//   - Conversation lives in memory; persistence lands in Slice 5.
//   - Per-run token cap enforced via opts.MaxTokens summed across
//     turns (see budget check below).
//
// The function is unexported; callers reach it via Run() in agent.go.
func runLoop(ctx context.Context, opts Options) (res *Result, err error) {
	// Per-run cost record (best-effort, async; kailab-routed runs only).
	runStart := time.Now()
	defer func() {
		if res == nil {
			return
		}
		k, ok := opts.Provider.(*provider.Kailab)
		if !ok || k.BaseURL == "" || k.AuthToken == "" {
			return
		}
		go uploadRunCost(&CostUploadConfig{BaseURL: k.BaseURL, AuthToken: k.AuthToken, TaskType: opts.TaskName}, res, opts.Model, time.Since(runStart))
	}()
	// Top-level panic recovery: an unexpected panic in any sub-call
	// (tool dispatch, provider parse, session write) would otherwise
	// take down the calling process. Convert it to a normal error
	// return so the REPL stays alive — the user can type again and
	// the session resumes from the durable write-ahead user message.
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			err = fmt.Errorf("agent: recovered from panic: %v\n%s", r, stack)
			if res == nil {
				res = &Result{}
			}
			res.FinishReason = message.FinishReasonError
		}
	}()

	if opts.Provider == nil {
		return nil, fmt.Errorf("agent: Provider required")
	}

	// Install the per-session routing tracer. Tools call
	// tools.TraceRouting() at every routing-sensitive dispatch point
	// (resolveInSet, kai_grep scope, graph-tool DB selection); the
	// runner forwards each line to opts.Hooks.OnRoutingTrace, which
	// the planner/chat wiring routes into <kaiDir>/planner-debug.log
	// or chat-debug.log. The deferred Clear prevents a tracer from a
	// finished session leaking into the next runLoop call.
	if opts.Hooks.OnRoutingTrace != nil {
		tools.SetRoutingTracer(opts.Hooks.OnRoutingTrace)
		defer tools.ClearRoutingTracer()
	}
	if opts.Workspace == "" {
		return nil, fmt.Errorf("agent: Workspace required")
	}
	if strings.TrimSpace(opts.Prompt) == "" {
		return nil, fmt.Errorf("agent: Prompt required")
	}

	// Per-run verdict bag. The OnDiff/OnFilesChanged hooks below
	// push gate verdicts into this; dispatchToolCalls drains it
	// after every mutating-tool call so the trailer reaches the
	// model on its next turn. Owned by runLoop so it stays per-run
	// (no leakage between concurrent agents that share Options).
	gateBag := &gateVerdictBag{}
	registry := buildToolRegistry(opts, gateBag)
	system, user := splitSystemAndUser(opts.Prompt)
	// Mode prompt is prepended to the system role so the per-mode
	// instructions sit ahead of the planner-built identity. Empty
	// when ModeUnknown resolves to Coding only by accident — Coding
	// has its own system prompt, so we always emit something.
	if modePrompt := opts.Mode.SystemPrompt(); modePrompt != "" {
		if system == "" {
			system = modePrompt
		} else {
			system = modePrompt + "\n\n" + system
		}
	}

	// First-call bash gate. When the user reports an error WITHOUT
	// pasting it ("getting an error on the homepage"), the agent's
	// first move should be running the project to surface the real
	// stack trace — not reading source files speculatively. We
	// detect that shape here and arm a flag the dispatcher reads
	// before each tool call. Cleared on the first bash call (any
	// outcome — exit code doesn't matter, the agent has a real
	// signal now). Not armed when bash isn't even in the tool set.
	bashFirst := &bashFirstGate{
		armed: opts.EnableBash && needsBashFirst(user),
	}
	model := opts.Model
	if model == "" {
		model = "deepseek/deepseek-v4-pro"
	}
	maxTokensPerTurn := opts.MaxTokens
	if maxTokensPerTurn <= 0 {
		// max_tokens is a per-response ceiling, not a budget — unused
		// headroom costs nothing (the per-run cap, budgetExceeded, is
		// keyed off actual usage) and a non-reasoning model stops at
		// end_turn well short of it. So size it generously.
		//
		// 4096 was the old non-debug default. It silently broke
		// reasoning models: Kimi K2.6 streams a long `reasoning`
		// trace that counts against max_tokens, so a 4096 cap was
		// exhausted by reasoning alone — the actual edit tool call
		// truncated mid-arguments and came back finish_reason=length
		// with no usable call ("worker produced no edits", observed
		// across the K2.6 bbolt benchmark t4/t5/t7/t8). A tool call
		// split by truncation can't be stitched back together by the
		// continuation hack either, so the only fix is not to
		// truncate: give the turn room for a reasoning trace PLUS a
		// full multi-file edit.
		//
		// Debug mode also reasons over tool output (typecheck dumps,
		// failing test traces) and gets extra room on top.
		if ResolveMode(opts.Mode) == ModeDebug {
			maxTokensPerTurn = 24576
		} else {
			maxTokensPerTurn = 16384
		}
		// Reasoning models (Qwen3 family, gpt-5*, o3*/o4*, deepseek-r1*)
		// emit a silent <think> trace that counts against max_tokens
		// before any visible output. At the non-reasoning default of
		// 16384 we've seen "model returned no text" because the whole
		// budget was consumed by reasoning — the model literally never
		// got to speak. Bump preemptively so the visible output has
		// room to coexist with the reasoning trace. The chat-side
		// empty-response retry (planner_dispatch.go) handles the cases
		// where this heuristic misses.
		if isReasoningModel(model) {
			if maxTokensPerTurn < 32768 {
				maxTokensPerTurn = 32768
			}
		}
	}

	// Resolve session: resume if id given, else start fresh, else
	// run without persistence. Errors during session setup are
	// surfaced as run errors rather than silently degrading — if
	// the caller asked for persistence, they expect it.
	sess, history, err := resolveSession(opts, model)
	if err != nil {
		return nil, err
	}
	// Per-turn run-log recorder. Records prompt-section sizes,
	// hashes, usage, and tool-call outcomes to <RunLogDir>/runs/
	// <sessionID>/<turn>.json. Disabled when RunLogDir is empty
	// or when there's no durable session id (in-memory runs skip
	// persistence by design — same convention as SessionStore).
	var rec *runlog.Recorder
	if opts.RunLogDir != "" && sess != nil {
		rec = runlog.New(opts.RunLogDir, sess.ID, opts.TaskName)
	}
	// Seed the new user turn. On a fresh session this is the only
	// message; on a resumed session we append after the prior turns
	// so the conversation ends with a user message (Anthropic rejects
	// requests that end on assistant — assistant-prefill is opt-in
	// and we don't use it here).
	//
	// Initialize Result early so the prompt-append block can record
	// injection metadata onto it (moved up from below the prompt
	// block specifically for the graph-powered context injection
	// signal — InjectedContextChars must be observable from the
	// final Result for the measurement hook).
	res = &Result{}

	// Write-ahead: the AppendMessage below happens BEFORE provider.Send
	// so a crash/SIGKILL mid-API-call leaves the user turn durable in
	// SQLite. Resume picks up exactly where we left off — at worst the
	// model re-answers the same question, never silently drops it.
	// Don't reorder this with the for-loop below.
	if strings.TrimSpace(user) != "" {
		newUser := message.Message{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: user}},
		}
		history = append(history, newUser)
		if sess != nil {
			if err := sess.AppendMessage(newUser, 0, 0); err != nil {
				return nil, err
			}
		}

		// Graph-powered context injection. When the planner (or
		// any caller) has pre-resolved entry points for this
		// request, splice in a synthetic context_lookup tool_use +
		// tool_result pair so the model sees the lookup as having
		// already happened. context_lookup is registered as a no-
		// op tool by buildToolRegistry — listed so the provider's
		// schema check passes, callable by the model but
		// uninformative on re-call.
		if strings.TrimSpace(opts.InjectedContext) != "" {
			injectContextLookup(opts.InjectedContext, &history, sess)
			res.InjectedContextChars = len(opts.InjectedContext)
		}
	} else if len(history) == 0 {
		// Caller passed an empty prompt and there's no prior history
		// to continue from — nothing to send.
		return nil, fmt.Errorf("agent: prompt empty and no session history to resume")
	}
	if sess != nil {
		res.SessionID = sess.ID
	}

	// Per-run dedupe cache for read-only tool calls. Lives for the
	// duration of this single agent run; identical (name, input)
	// pairs return the cached result with a "[deduped]" trailer so
	// the model can see the duplicate flagged. See dispatchToolCalls
	// for the matching read/write logic. Empty when an exploration
	// agent doesn't repeat itself.
	dedupeCache := map[string]string{}

	// maxTurns caps the loop so a confused model can't burn the
	// caller's wallet on coordination overhead. Higher than Claude
	// Code's 25 because kai's multi-agent paths legitimately spend
	// turns talking to themselves; lower would force noisy
	// continue-prompts on real work.
	//
	// Callers that know their workload converges sooner (e.g. the
	// planner agent) override via opts.MaxTurns to cap exploration
	// loops earlier — the model's "let me check one more thing"
	// tendency dominates after ~10 turns of pure exploration.
	maxTurns := 50
	if opts.MaxTurns > 0 {
		maxTurns = opts.MaxTurns
	}
	// convergeTurnsBefore: how many turns before the cap we start
	// injecting "wrap it up" reminders. The model gets one warning,
	// then a final-answer demand on the last turn.
	convergeTurnsBefore := 3
	if maxTurns < 10 {
		convergeTurnsBefore = 1
	}
	// Budget-exhaustion continuation: when the model truncates a
	// response because of MaxTokens (resp.FinishReason ==
	// FinishReasonMaxTokens) and emitted no tool calls, we inject a
	// "Continue from where you stopped. No recap." user message and
	// re-call so the model can finish its thought. Cap at 3
	// consecutive continuations — beyond that the response is
	// genuinely too long and the user should split the request.
	const maxContinuations = 3
	continuations := 0
	// dsmlRecoveries caps how many times we re-prompt after a tool-use stop
	// that carried no parsed tool call — the "DSML-delimiter leak" where a
	// model (notably the Claude family on this provider) emits its tool call
	// as text instead of a structured block, so nothing executes and the run
	// would otherwise die with zero edits and a "0/0/0" summary. 2 is enough
	// to clear a transient leak without looping a model that simply can't.
	const maxDSMLRecoveries = 2
	dsmlRecoveries := 0
	// finalizeAttempted guards the budget-exceeded recovery to a
	// single retry so a self-overshooting finalize turn can't loop.
	// Set in the budget-exceeded branch when we inject the
	// commit-now prompt; checked there too so the second hit
	// surfaces the original error.
	finalizeAttempted := false

	// absenceGuardFired is set the first (and only) time the run's
	// final-text guard triggers. Per-run scoping: once an agent has
	// been told to search more thoroughly, we accept whatever its
	// next conclusion is — the alternative (firing per-claim) lets
	// the model game the trigger by varying its phrasing.
	absenceGuardFired := false
	// Same fire-at-most-once contract as the absence guard. Real
	// failure cases tend to be one-shot fabrications rather than
	// repeated ones, and a second fire on the SAME turn would loop
	// indefinitely against a model that keeps inventing files.
	hallucinationGuardFired := false
	// buildSuccessGuardFired fires when the model narrates "build
	// succeeded" / "tests pass" / "compiles cleanly" while the most
	// recent bash command exited non-zero. Round-21 dogfood: worker
	// ran `go build ./cmd/kai/...` three times, all failed with
	// "directory prefix does not contain main module" (a cwd bug
	// since fixed), then said "The build/vet commands succeeded
	// with no errors (clean output)." The harness ran build_check
	// afterward and would have caught the real state — but the
	// agent's narration still polluted scrollback. This guard sends
	// the agent back to re-run and report the actual exit code
	// before declaring done. lastBashFailed below is the cross-
	// reference: set on a bash IsError, cleared on a successful
	// bash result.
	buildSuccessGuardFired := false
	lastBashFailed := false
	// conversationSearchGuardFired fires the first (and only) time a
	// chat-mode answer tries to finalize with zero codebase tool calls
	// behind it. Same fire-at-most-once contract as the guards above:
	// one forced "go search first" re-prompt is the enforcement; a
	// model that still answers from priors after that is allowed
	// through rather than looping a read-only Q&A indefinitely.
	conversationSearchGuardFired := false
	// truncatedAnswerGuardFired fires the first (and only) time an
	// interactive answer trails off mid-thought (ends on an ellipsis)
	// and the model ended its turn anyway — a reasoning-model
	// degenerate completion (2026-05-29 DeepSeek-V4-Pro). One re-prompt
	// to finish the answer; no hard-fail.
	truncatedAnswerGuardFired := false
	// Cross-turn error-class tracker. Holds a bounded ring of recent
	// bash-error signatures the model has seen. When a fresh bash
	// error has a signature NOT in the ring, we append a one-line
	// runner note to the result so the model notices a new error
	// class appeared since prior turns — its prior fix may have
	// worked and the new error is a NEW problem requiring fresh
	// diagnosis. Counters the tunnel-vision pattern from the
	// 2026-05-24 kai-desktop dogfood.
	//
	// v0.31.40 used a single-slot tracker that got wiped on EVERY
	// successful bash. The 2026-05-25 runlog inspection showed the
	// bug in action: turn 1 errored → turn 3 ran an unrelated
	// `node -e "console.log(...)"` and succeeded → the slot got
	// reset → turn 4 errored with a different signature, but the
	// detector had no prior to compare against, so no note fired.
	// A small ring fixes it: an intervening unrelated success
	// doesn't erase memory of the actual error class. Ring size is
	// kept small (8) because we only need the last few real errors
	// for the change-detection check, not unbounded history.
	bashErrSigRing := make([]string, 0, bashErrSigRingSize)
	// Fire up to dangleGuardMaxFires times. First fire nudges; if the
	// model dangles again after the nudge, we fire once more. A third
	// dangle is treated as terminal failure (return an error so the
	// orchestrator surfaces it instead of "no changes"). Gated on
	// coding mode; debug-mode's system prompt has its own variant.
	const dangleGuardMaxFires = 2
	dangleGuardFires := 0

	// Graph-context injector: before each provider.Send, scan the
	// latest turn's content for file paths and prepend their
	// depth-1 callers / dependents / protected status to the system
	// role. Stops the model from having to call kai_callers itself
	// — kai's graph signal arrives whether the model asks for it
	// or not.
	graphCtx := newGraphContextInjectorWithSet(opts.Graph, opts.Workspace, opts.Mode, opts.Projects)

	// lastInputTokens tracks the input-token count from the prior
	// provider.Send. Compaction triggers when this crosses the
	// threshold for the model's context window — checked at the top
	// of each iteration so the next request can ship a shorter
	// history. Zero on the first turn (no prior response yet) means
	// compaction never fires before the conversation has actually
	// grown.
	lastInputTokens := 0

	// Per-run tool-call counters surfaced to the model on every tool
	// result via the budget suffix. Cumulative across the run so the
	// model can see its own behavior (ratio of reads to edits)
	// reflected back each turn — the cheapest pacing signal we have.
	var readCount, editCount int

	// Read-streak tracker: number of consecutive turns whose only
	// tool calls were read-only. Reset on any edit, untouched by
	// neutral turns (bash, kai_checkpoint). Drives the soft nudge
	// and hard block via the scope-aware thresholds resolved once
	// per run so the orchestrator can tighten them for small tasks.
	var consecutiveReadTurns int
	// searchWithoutBashStreak tracks consecutive turns where the
	// model called kai_grep / kai_search / kai_files but never bash.
	// At threshold the runner injects a nudge to RUN THE UNDERLYING
	// COMMAND. See the per-turn classifier below.
	var searchWithoutBashStreak int
	// testFightStreak tracks consecutive turns that ran a test command
	// without changing production code — the test-tooling yak-shave.
	var testFightStreak int

	// runHasSeenBash tracks whether ANY bash call has occurred so far
	// in this run. Used by the explore-before-edit gate below to
	// require executor agents to verify the plan's EXPLORE directive
	// before applying their first modification. False until the
	// first bash call; never reset.
	var runHasSeenBash bool

	// verifiedOnPreviousTurn is true when the immediately preceding
	// turn made a verification-shaped bash call. Used to defer the
	// final-turn tool strip by one turn so the model can ACT on what
	// it just verified instead of being forced to commit immediately
	// after a discovery. The 2026-05-26 edges dogfood pinned this:
	// model finally reached for bash on turn 9 (with max=10), but
	// turn 9 already had tools=0 (convergence had stripped them),
	// so the bash was rejected and the model emitted a plan based
	// on what it could see WITHOUT the verification. One extension
	// turn lets the model land on a real answer.
	var verifiedOnPreviousTurn bool

	// promptRequiresExplore is true when the agent's initial prompt
	// (the planner's executor instructions, typically) contains an
	// "EXPLORE" directive. When set, the runner blocks the first
	// write/edit until at least one bash call has occurred — the
	// 2026-05-26 snapshot-count executor shipped a half-fix with a
	// wrong field source because the planner's EXPLORE step ("run
	// kai snapshot list --json to see the exact JSON shape if
	// possible") was treated as optional.
	promptRequiresExplore := promptHasExploreDirective(opts.Prompt)
	readSoftThreshold, readHardThreshold := resolveReadStreakThresholds(opts)

	// Continuous-paging tracker for the read-streak counter. When the
	// agent pages through one large file via sequential `view` calls
	// (offset N, then offset N+limit, then N+2*limit, ...), that's
	// not "new reconnaissance" — it's reading one file the model is
	// already interested in. Counting each page against the streak
	// pushed workers into `bash python3 -c "with open(...)"` to
	// bypass the gate (observed today, run-29 dogfood), which kept
	// the file-reading happening at the same token cost but defeated
	// the gate's intent. Tracking lastViewFile + lastViewEnd lets us
	// recognize contiguous paging and leave the streak counter alone
	// for those turns. The dedupe tracker (viewRangeTracker) is
	// orthogonal: it prevents re-reading the same range, while this
	// tracker handles forward paging of new ranges.
	var lastViewFile string
	var lastViewEnd int

	// View-range tracker: per-file [start, end) line windows the
	// agent has already viewed, tagged with the turn they were read
	// on. Dispatch consults this before each view call; matches
	// (>=80% overlap, within last 15 turns) get a stub instead of
	// re-running the view.
	viewTracker := newViewRangeTracker()
	loops := newLoopTracker()

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			res.FinishReason = message.FinishReasonCanceled
			if sess != nil {
				_ = sess.End(session.StatusErrored)
			}
			return res, err
		}

		// Tool-result trim: rewrite content of tool results older
		// than the recent window to a one-line summary. Tool
		// results live in history forever and re-cost tokens on
		// every subsequent provider call until full compaction
		// fires at 80%; trimming early bounds that growth without
		// losing the headline (count, file list, error flag) the
		// model needs to remember a result happened. Cheap and
		// idempotent — already-trimmed entries skip themselves.
		// Trim older tool results to one-line stubs unless the
		// caller opted out. Trimming saved tokens when conversation
		// history was re-sent fresh every turn; with prompt caching
		// it's no longer the right default for exploration agents
		// (they lose visibility of files they already viewed and
		// re-view them, burning the turn budget).
		if !opts.KeepToolResults {
			trimOldToolResults(history, toolResultRecentWindow)
		}

		// Compaction check: fire when EITHER the previous turn was
		// near the model's context window (per-turn check) OR the
		// cumulative spend is approaching MaxTotalTokens (budget
		// check). The two cover different failure modes — context
		// overflow vs. dollars-per-run cap blow — and either alone
		// has shipped a real bug in dogfood. Graph context is
		// intentionally NOT preserved by compaction — it's re-
		// injected fresh below.
		usedSoFar := res.TokensIn + res.TokensOut
		if shouldCompact(lastInputTokens, model) || shouldCompactByBudget(usedSoFar, opts.MaxTotalTokens) {
			compacted, cerr := compact(ctx, opts.Provider, "", history, defaultRecentWindow)
			if cerr == nil {
				history = compacted
				// Reset so we don't immediately re-trigger on the
				// next iteration if the response is delayed.
				lastInputTokens = 0
				// Compaction drops old tool results — including `view`
				// and `kai_grep` content — from the conversation. Both
				// read caches assume a recently-fetched result is
				// still in context; after a compaction that's false.
				// Clear them so the agent can re-fetch what it can no
				// longer see, instead of being served a content-free
				// view-range stub or a stale "[deduped]" note.
				viewTracker.Reset()
				dedupeCache = map[string]string{}
			}
			// On error: leave history alone and let the next send
			// either succeed or fail with the provider's own
			// context-overflow error. Compaction is best-effort, not
			// a hard prerequisite for forward progress.
		}

		// CRITICAL: keep `systemForTurn` byte-identical across all
		// turns of the run. Anthropic prompt caching matches on
		// prefix bytes; a single-byte change in the system slice
		// dies the cache breakpoint and forces a fresh
		// cache_creation write at 1.25× input cost on the entire
		// system+tools prefix. We previously appended graph context
		// AND the convergence nudge to systemForTurn here, which
		// invalidated the cache on every turn (graph block grows
		// as new files are referenced) and on the final 3 turns of
		// every run (convergence nudge fires). May 3 billing data
		// shows ~12× more cache_write than cache_read tokens —
		// confirmed via TestCacheInvalidation_* in the provider
		// package.
		//
		// Fix: move both per-turn dynamic injections into a
		// transient TextContent block appended to the LAST USER
		// message before send. The base system prompt stays
		// stable; the new content rides in the message slice's
		// growing tail (which the cache handles correctly via
		// prefix matching of the earlier blocks).
		systemForTurn := system

		turnsLeft := maxTurns - turn
		var perTurnHints []string
		if extra := graphCtx.buildBlock(history, opts.GateConfig.Protected); extra != "" {
			perTurnHints = append(perTurnHints, extra)
		}
		// TASKS.md ledger. Loaded fresh per turn (file is small, user
		// may have hand-edited it between turns). Silent on Load
		// error — a parse failure must never break a chat turn. See
		// docs/tasks-md-spec.md for the file contract.
		if tm, err := tasksmd.Load(opts.Workspace); err == nil {
			if extra := tm.FormatForPrompt(); extra != "" {
				perTurnHints = append(perTurnHints, extra)
			}
		}
		// Read-streak soft nudge fires every turn the streak is at or
		// above the soft threshold and below the hard one. Rendered
		// per turn (not fire-once) because the model genuinely may
		// need the reminder multiple times — the alternative is a
		// one-shot nudge that goes stale within two turns.
		if consecutiveReadTurns >= readSoftThreshold && consecutiveReadTurns < readHardThreshold {
			perTurnHints = append(perTurnHints, readStreakSoftNudgeText(readSoftThreshold))
		}
		// Search-without-bash nudge. Fires when the model has made
		// N consecutive turns of source-hunting (kai_grep/kai_search/
		// kai_files) without ever running an external command. The
		// prompt rule (VERIFY EXTERNAL CONTRACT, 0.32.46) and the
		// tool-description warnings (0.32.47) BOTH ship in the
		// per-turn context, but the planner kept ignoring them.
		// This injection fires from the same place the read-streak
		// soft nudge fires — empirically the model reads runner
		// notes here. Threshold 3 because that's already a turn
		// past where the answer would be visible in one bash call.
		if searchWithoutBashStreak >= searchNoBashThreshold {
			perTurnHints = append(perTurnHints, searchWithoutBashNudgeText(searchWithoutBashStreak))
		}
		// Test-fight soft nudge: fires while the streak sits between the
		// soft and hard thresholds. Past the hard threshold the decide
		// closure blocks further test runs outright, so the nudge stops.
		if !opts.ReadOnly && testFightStreak >= testFightSoftNudge && testFightStreak < testFightHardBlock {
			perTurnHints = append(perTurnHints, testFightNudgeText(testFightStreak))
		}
		// Wind-down hint: warns the agent it's nearly out of budget
		// and tells it how to leave the workspace coherent. Fires
		// strictly above the convergence window so the model gets one
		// "wrap up cleanly" turn before convergence demands the
		// deliverable. Both can co-fire on lower turns — they say
		// different things and reinforce each other.
		if turnsLeft <= windDownThreshold {
			perTurnHints = append(perTurnHints, windDownHint(turnsLeft))
		}

		// First-turn project overview is injected by graphCtx
		// (see internal/agent/graph_context.go buildOverview),
		// not here. graphCtx ships a richer block (manifest +
		// top-level tree + README excerpt) that's already
		// referenced by the chat-mode system prompt, and its
		// isFirstTurn heuristic correctly handles the planner-
		// then-chat handoff. Don't add a second injector here —
		// two overlapping overview blocks would just confuse the
		// model and burn tokens.
		if turnsLeft <= convergeTurnsBefore {
			// Mode-aware. Coding mode gets "make the edits via
			// edit/write" instead of "produce your final answer" —
			// the latter reads as "write prose," which silently
			// shipped a 0-edits run in round-15 dogfood. See
			// convergence.go for the per-mode strings.
			if h := convergenceHint(opts.Mode, turnsLeft); h != "" {
				perTurnHints = append(perTurnHints, h)
			}
		}

		// Per-turn message slice: history with the per-turn hints
		// appended as a TextContent block on the LAST user
		// message. We don't mutate `history` itself — that would
		// pollute the persisted session and re-inject hints
		// retroactively on resume. Instead we build a shallow
		// copy whose tail message is augmented just for this send.
		messagesForTurn := history
		if len(perTurnHints) > 0 {
			messagesForTurn = withPerTurnHints(history, strings.Join(perTurnHints, "\n\n"))
		}

		// On the FINAL turn, strip research/exploration tools. For
		// non-coding modes we strip everything (the planner needs to
		// emit JSON as text; tools would just invite another
		// "let me check one more thing"). For coding mode we keep
		// edit + write — the deliverable IS a tool call, so removing
		// the means to produce it leaves the model literally unable
		// to comply with the final-turn instruction (round-15
		// dogfood: "I'm blocked because I have no tools available
		// to make the actual edits" — true at the moment it ran).
		//
		// NOTE: this still kills the tools-array cache breakpoint
		// on the final turn. Acceptable: it's one turn out of
		// MaxTurns and the convergence hint already signals the
		// terminal behavior.
		toolList := toolInfos(registry)
		// Defer the final-turn tool strip when the previous turn
		// made a verification bash call. Lets the model ACT on what
		// it just discovered (adjust the plan, make a follow-up
		// call, finalize with the new info) instead of being forced
		// to commit without using the verification. Re-set below
		// from THIS turn's classification, so a model that keeps
		// verifying gets a rolling one-turn grace — but the total
		// turn budget never increases.
		if turnsLeft <= 1 && !verifiedOnPreviousTurn {
			toolList = finalTurnTools(opts.Mode, toolList)
		}

		// EphemeralTailMessages tells the provider to place its
		// cache_control breakpoint BEFORE any per-turn hint we just
		// appended (graph-context block, convergence nudge). Without
		// this the breakpoint lands on the hint's bytes and varies
		// turn-over-turn, killing reuse on the canonical history
		// prefix that's supposed to be cacheable.
		ephemeralTail := 0
		if len(messagesForTurn) > len(history) {
			ephemeralTail = len(messagesForTurn) - len(history)
		}
		req := provider.Request{
			Model:                 model,
			System:                systemForTurn,
			Messages:              messagesForTurn,
			Tools:                 toolList,
			MaxTokens:             maxTokensPerTurn,
			EphemeralTailMessages: ephemeralTail,
			OnState:               opts.Hooks.OnProviderState,
			OutputJSONSchema:      opts.OutputJSONSchema,
			// Forced tool use applies ONLY on turn 0. After the model
			// has emitted at least one tool call, the constraint comes
			// off so subsequent turns can produce the final response.
			// Without this gate, every turn would be forced into a
			// tool call and the agent could never emit its terminal
			// text/JSON.
			RequireToolUse: opts.RequireToolUseFirstTurn && turn == 0,
		}
		// Debug observability: snapshot the full request just
		// before send. Used by the chat-agent debug log to
		// answer "what did the model actually receive on this
		// turn" — including the per-turn injected project
		// overview, system prompt, and tools list. No-op when
		// no hook is wired.
		if opts.Hooks.OnRequest != nil {
			opts.Hooks.OnRequest(turn, req)
		}
		// runlog Begin captures prompt-section sizes/hashes BEFORE
		// the network round-trip so a hung Send still produces a
		// debuggable artifact when the user Ctrl+Cs out.
		rec.Begin(turn, req)
		resp, err := sendWithRecovery(ctx, opts, req)
		// Context-overflow recovery: compact aggressively and retry
		// once. The first compaction uses the default recent window
		// (6 messages); if that's still too large we shrink the
		// window to 3 and retry one more time. Beyond that the
		// failure is genuine — surface it.
		if err != nil && provider.IsContextOverflow(err) {
			recoveryWindows := []int{defaultRecentWindow, 3}
			for _, win := range recoveryWindows {
				compacted, cerr := compact(ctx, opts.Provider, "", history, win)
				if cerr != nil {
					break
				}
				history = compacted
				lastInputTokens = 0
				req.Messages = history
				resp, err = sendWithRecovery(ctx, opts, req)
				if err == nil || !provider.IsContextOverflow(err) {
					break
				}
			}
		}
		if err != nil {
			// Land the run-log artifact before bailing — failed
			// turns are the most useful to introspect, not least
			// because they often explain why the next-turn cache
			// missed (compaction, retry-with-shrunk-history, etc.).
			rec.End(nil, err)
			res.FinishReason = message.FinishReasonError
			if sess != nil {
				_ = sess.End(session.StatusErrored)
			}
			return res, err
		}
		// Post-provider ctx check: the provider can return successfully
		// at the exact moment the user hits cancel, in which case the
		// HTTP layer doesn't see the cancellation. Without this check
		// the runner spends another ~30s processing the response
		// through guards, dispatching tools, etc. — exactly the silent
		// stretch the user reads as "stuck" because no activity-emitting
		// step in that window touches the TUI's event channel. Round-21
		// dogfood saw this as 4m of silence after HTTP 200.
		if err := ctx.Err(); err != nil {
			rec.End(&resp, err)
			res.FinishReason = message.FinishReasonCanceled
			if sess != nil {
				_ = sess.End(session.StatusErrored)
			}
			return res, err
		}
		res.TokensIn += resp.InputTokens
		res.TokensOut += resp.OutputTokens
		res.TokensCached += resp.CachedInputTokens
		res.TokensCacheCreate += resp.CacheCreationTokens
		res.TokensCacheRead += resp.CacheReadTokens
		res.ProviderCostUSD += resp.EstimatedCostUSD
		res.RequestCount++
		// Compaction triggers off the actual prompt size, which
		// with caching = uncached input + cache reads + cache
		// creation. The billing-aligned InputTokens alone
		// undercounts the prompt, so without this addition
		// compaction would never fire on a cache-heavy turn.
		lastInputTokens = resp.InputTokens + resp.CachedInputTokens
		if opts.Hooks.OnTurnComplete != nil {
			opts.Hooks.OnTurnComplete(res.TokensIn, res.TokensOut, res.TokensCached)
		}

		// Surface assistant-visible text via the hook so the TUI can
		// render the agent narrating its work.
		for _, p := range resp.Parts {
			if t, ok := p.(message.TextContent); ok && opts.Hooks.OnAssistantText != nil {
				if s := strings.TrimSpace(t.Text); s != "" {
					opts.Hooks.OnAssistantText(s)
				}
			}
		}

		// Append the assistant turn to history.
		assistantMsg := message.Message{
			Role:     message.RoleAssistant,
			Parts:    resp.Parts,
			Finished: resp.FinishReason,
			Model:    model,
		}
		history = append(history, assistantMsg)
		if sess != nil {
			// Persist with this turn's token deltas (resp's counts,
			// not res's cumulative — session row aggregates separately).
			if err := sess.AppendMessage(assistantMsg, resp.InputTokens, resp.OutputTokens); err != nil {
				return res, err
			}
		}

		// If the model didn't ask for tools, we're either done or
		// just truncated. A clean end_turn / stop_sequence finishes
		// the run; a max_tokens stop with no tool calls is a
		// truncated reply we can resume by nudging the model to
		// continue. Tool-use stops always carry tool calls — handled
		// below.
		toolCalls := extractToolCalls(resp.Parts)
		if len(toolCalls) == 0 {
			// DSML-leak recovery: the model stopped on tool_use but no tool
			// call parsed — it emitted the call as text (delimiter leak)
			// instead of a structured block. Left alone this leads to a
			// zero-edit "0/0/0" death (see config.go defaultAgentModel). Nudge
			// it to re-emit the call properly, capped so a model that can't
			// won't loop. Placed first: this is a thwarted tool-use turn, not
			// a prose answer, so the answer-oriented guards below don't apply.
			if resp.FinishReason == message.FinishReasonToolUse && dsmlRecoveries < maxDSMLRecoveries {
				dsmlRecoveries++
				nudge := message.Message{
					Role: message.RoleUser,
					Parts: []message.ContentPart{
						message.TextContent{Text: "Your last turn ended on a tool call, but none was received — it looks like the tool call was written as plain text instead of an actual tool invocation. Re-issue it now as a real tool call. Do not narrate; just make the call."},
					},
				}
				history = append(history, nudge)
				if sess != nil {
					if err := sess.AppendMessage(nudge, 0, 0); err != nil {
						rec.End(&resp, err)
						return res, err
					}
				}
				rec.End(&resp, nil)
				continue
			}
			if resp.FinishReason == message.FinishReasonMaxTokens && continuations < maxContinuations {
				continuations++
				cont := message.Message{
					Role: message.RoleUser,
					Parts: []message.ContentPart{
						message.TextContent{Text: "Continue from where you stopped. No recap."},
					},
				}
				history = append(history, cont)
				if sess != nil {
					if err := sess.AppendMessage(cont, 0, 0); err != nil {
						rec.End(&resp, err)
						return res, err
					}
				}
				// Truncation continuation closes one turn and opens
				// another — record the closing turn now so the next
				// loop's Begin starts cleanly.
				rec.End(&resp, nil)
				continue
			}

			// Search guard: refuse to let a pure-prose answer finalize
			// with zero codebase tool calls behind it. The failure this
			// catches is answering a workspace question from training
			// priors instead of looking. Gated on GroundAnswers — the
			// interactive in-process answer path (runChatAgent) sets it;
			// background/spawned workers don't, so this never disturbs a
			// coding worker mid-task. Fires only on ANSWER turns: a
			// terminal reply with no edits AND no search behind it (edit
			// turns and turns that already searched are exempt).
			// Pleasantries ("hi", "thanks") exempt. Fires once per run.
			if opts.GroundAnswers && !conversationSearchGuardFired {
				if len(SearchCalls(history)) == 0 && EditToolCalls(history) == 0 && !isPleasantry(user) {
					conversationSearchGuardFired = true
					res.ConversationSearchGuardFired = true
					nudge := message.Message{
						Role: message.RoleUser,
						Parts: []message.ContentPart{
							message.TextContent{Text: conversationSearchGuardNudge},
						},
					}
					history = append(history, nudge)
					if sess != nil {
						if err := sess.AppendMessage(nudge, 0, 0); err != nil {
							rec.End(&resp, err)
							return res, err
						}
					}
					rec.End(&resp, nil)
					continue
				}
			}

			// Truncated-answer guard: a reasoning model can spend its
			// output on hidden reasoning and emit a visibly incomplete
			// answer that trails off on an ellipsis, then end the turn on
			// its own (finish=end_turn, well under any token cap). Surface
			// that fragment and the user sees a cut-off non-answer. Send
			// it back once to produce the complete reply. Gated on
			// GroundAnswers (interactive path) and fires at most once.
			if opts.GroundAnswers && !truncatedAnswerGuardFired {
				if looksTruncated(assistantFinalText(resp.Parts)) {
					truncatedAnswerGuardFired = true
					res.TruncatedAnswerGuardFired = true
					nudge := message.Message{
						Role: message.RoleUser,
						Parts: []message.ContentPart{
							message.TextContent{Text: truncatedAnswerNudge},
						},
					}
					history = append(history, nudge)
					if sess != nil {
						if err := sess.AppendMessage(nudge, 0, 0); err != nil {
							rec.End(&resp, err)
							return res, err
						}
					}
					rec.End(&resp, nil)
					continue
				}
			}

			// Absence guard: if the model's final answer reads as a
			// negative claim ("X doesn't exist") and the run has
			// fewer than 3 relevant searches behind it, send the
			// agent back to do more lookups. Fires at most once per
			// run — see absenceGuardFired declaration. Skipped when
			// the caller opted out via NoAbsenceGuard (e.g. inside
			// the guard's own retry pass, were one to exist later).
			if !opts.NoAbsenceGuard && !absenceGuardFired {
				finalText := assistantFinalText(resp.Parts)
				if IsNegativeClaim(finalText) {
					calls := SearchCalls(history)
					if RelevantSearches(finalText, calls) < 3 {
						absenceGuardFired = true
						res.AbsenceGuardFired = true
						nudge := message.Message{
							Role: message.RoleUser,
							Parts: []message.ContentPart{
								message.TextContent{Text: absenceGuardNudge},
							},
						}
						history = append(history, nudge)
						if sess != nil {
							if err := sess.AppendMessage(nudge, 0, 0); err != nil {
								rec.End(&resp, err)
								return res, err
							}
						}
						rec.End(&resp, nil)
						continue
					}
				}
			}

			// Hallucination guard: if the final answer names files
			// (with known source/config extensions) and any of those
			// names don't appear anywhere in the conversation
			// context — tool results, project overview, user
			// messages — send the agent back with a coaching nudge
			// to consult kai_tree / view / kai_files first. Same
			// fire-at-most-once pattern as the absence guard.
			//
			// Caught live 2026-05-12 when opus-4-6 confidently said
			// "you have an index.js and package.json" in the kai
			// monorepo (which has neither at the root). The
			// hallucination wasn't anchored to ANY tool result or
			// the auto-injected overview — pure improvisation.
			if !opts.NoHallucinationGuard && !hallucinationGuardFired {
				finalText := assistantFinalText(resp.Parts)
				mentions := ExtractFileMentions(finalText)
				if len(mentions) > 0 {
					fabricated := FabricatedFileMentions(mentions, history)
					if len(fabricated) > 0 {
						hallucinationGuardFired = true
						res.HallucinationGuardFired = true
						nudge := message.Message{
							Role: message.RoleUser,
							Parts: []message.ContentPart{
								message.TextContent{Text: formatHallucinationNudge(fabricated)},
							},
						}
						history = append(history, nudge)
						if sess != nil {
							if err := sess.AppendMessage(nudge, 0, 0); err != nil {
								rec.End(&resp, err)
								return res, err
							}
						}
						rec.End(&resp, nil)
						continue
					}
				}
			}

			// Build-success hallucination guard: the model narrated
			// "build succeeded" / "tests pass" while the most recent
			// bash command exited non-zero. Send it back to re-run
			// and report the actual exit code. Fires at most once
			// per run.
			if !buildSuccessGuardFired && lastBashFailed {
				finalText := assistantFinalText(resp.Parts)
				if ClaimsBuildSuccess(finalText) {
					buildSuccessGuardFired = true
					res.BuildSuccessGuardFired = true
					nudge := message.Message{
						Role: message.RoleUser,
						Parts: []message.ContentPart{
							message.TextContent{Text: buildSuccessGuardNudge},
						},
					}
					history = append(history, nudge)
					if sess != nil {
						if err := sess.AppendMessage(nudge, 0, 0); err != nil {
							rec.End(&resp, err)
							return res, err
						}
					}
					rec.End(&resp, nil)
					continue
				}
			}

			// Dangle guard: in coding mode, if the agent is about to
			// end its turn having made zero write/edit calls and its
			// final text reads as a change-description (rather than an
			// explicit block), nudge it to either make the edit or
			// state what's blocking it. Fires at most once per run.
			if !opts.NoDangleGuard && ResolveMode(opts.Mode) == ModeCoding {
				finalText := assistantFinalText(resp.Parts)
				dangling := IsChangeDescription(finalText) && !IsExplicitBlock(finalText) && EditToolCalls(history) == 0
				if dangling && dangleGuardFires >= dangleGuardMaxFires {
					// Hit the cap with no edits — treat as failure so
					// the orchestrator marks the run Failed (instead of
					// reporting "no changes" with zero diff).
					rec.End(&resp, fmt.Errorf("dangle guard: %d nudges, still no edits", dangleGuardFires))
					res.FinishReason = message.FinishReasonError
					res.Transcript = history
					if sess != nil {
						_ = sess.End(session.StatusErrored)
					}
					return res, fmt.Errorf("agent: described changes %d times without making any edits", dangleGuardFires+1)
				}
				if dangling {
					dangleGuardFires++
					res.DangleGuardFired = true
					nudge := message.Message{
						Role: message.RoleUser,
						Parts: []message.ContentPart{
							message.TextContent{Text: dangleGuardNudge},
						},
					}
					history = append(history, nudge)
					if sess != nil {
						if err := sess.AppendMessage(nudge, 0, 0); err != nil {
							rec.End(&resp, err)
							return res, err
						}
					}
					rec.End(&resp, nil)
					continue
				}
			}

			rec.End(&resp, nil)
			res.FinishReason = resp.FinishReason
			res.FinalText = assistantFinalText(resp.Parts)
			res.Transcript = history
			if sess != nil {
				_ = sess.End(session.StatusEnded)
			}
			return res, nil
		}
		// Model issued tool calls → it's making progress, reset the
		// continuation counter so any later truncation gets its own
		// fresh allotment of resumes.
		continuations = 0

		// Per-run token budget check. Enforce after a turn completes
		// so we always include the model's final output in the total.
		//
		// Forced-finalize recovery (2026-05-26 dogfood): when the
		// agent runs through the cap mid-exploration, the gathered
		// evidence is almost always enough for a wise guess. Erroring
		// out wastes the prior turns AND leaves the user with no
		// answer. One more turn — with a constrained prompt that
		// FORBIDS more exploration and DEMANDS a concrete commit
		// based on what's already seen — usually lands the fix.
		//
		// Single retry only (finalizeAttempted guard) so the loop
		// can't recurse if the finalize turn itself overshoots.
		// Skips the budget check on that one turn; if the model still
		// doesn't produce a usable answer, the second budget-exceeded
		// hit surfaces the error as before.
		if exceeded, cap := budgetExceeded(res, opts); exceeded {
			if !finalizeAttempted {
				finalizeAttempted = true
				finalizeMsg := message.Message{
					Role: message.RoleUser,
					Parts: []message.ContentPart{message.TextContent{
						Text: budgetFinalizePrompt(res.TokensIn+res.TokensOut, cap),
					}},
				}
				history = append(history, finalizeMsg)
				if sess != nil {
					_ = sess.AppendMessage(finalizeMsg, 0, 0)
				}
				if opts.Hooks.OnRetryWait != nil {
					opts.Hooks.OnRetryWait(turn+1, 0, fmt.Errorf("budget exceeded (used %d / cap %d) — forcing one final commit-now turn", res.TokensIn+res.TokensOut, cap))
				}
				continue
			}
			rec.End(&resp, fmt.Errorf("budget exceeded"))
			res.FinishReason = message.FinishReasonError
			res.Transcript = history
			if sess != nil {
				_ = sess.End(session.StatusErrored)
			}
			return res, fmt.Errorf("agent: token budget exceeded (used %d, cap %d)",
				res.TokensIn+res.TokensOut, cap)
		}

		// Dispatch tool calls. Read-only tools (view, kai_callers,
		// kai_dependents, kai_context) run concurrently — they don't
		// touch the workspace and don't depend on each other, so
		// blocking any one of them on the others is wasted wall-
		// clock. Mutating tools (write, edit, bash) run serially in
		// the order the model emitted them — concurrent writes risk
		// stale-read interactions and out-of-order edits to the same
		// file. The OnToolCall hook fires from the dispatching
		// goroutine; consumers must be safe for concurrent calls
		// (the TUI's chat-activity channel is non-blocking, so it
		// is).
		// onCallDone routes per-tool outcomes (duration, output bytes,
		// error class) into the run-log recorder when one is wired.
		// Nil disables — same convention as the existing OnToolCall.
		var onCallDone func(name, input string, outputBytes int, durationMs int64, isError bool, errMsg string)
		if rec != nil {
			onCallDone = func(name, input string, outputBytes int, durationMs int64, isError bool, errMsg string) {
				rec.AddToolCall(runlog.ToolCall{
					Name:        name,
					InputBytes:  len(input),
					OutputBytes: outputBytes,
					DurationMs:  durationMs,
					Error:       errMsg,
				})
				_ = isError // captured via Error already
			}
		}
		// Two pre-dispatch interception gates compose through
		// partitionCalls. Order matters:
		//   1. Read-streak hard block — once the streak is past the
		//      upper threshold, every read is refused regardless of
		//      target or recency.
		//   2. View-range dedupe — for reads that survive (1), refuse
		//      any that re-cover >=80% of a window read in the last
		//      15 turns (the "30–80 line slices at adjacent offsets"
		//      pattern that ate the failing run's budget).
		// Non-read calls (bash, write/edit, kai_checkpoint) bypass
		// both gates so the agent can always recover by editing.
		hardBlock := consecutiveReadTurns >= readHardThreshold
		currentDisplayTurn := turn + 1
		// Per-turn read counter for the in-turn cap (separate from
		// the per-run readCount). The cap blocks the (cap+1)th
		// onwards so the first `cap` reads still dispatch — the
		// agent gets a useful first round, just not an unbounded one.
		readsThisTurnSoFar := 0
		decide := func(c message.ToolCall) interceptDecision {
			// Loop detector fires first: a repeated identical failure
			// shouldn't even reach the read-streak / dedupe gates,
			// because the right answer is "stop trying this" regardless
			// of what the other gates would say.
			if loops.IsLoop(c.Name, c.Input) {
				return interceptDecision{Intercept: true, Content: loopInterceptMessage(c.Name)}
			}
			if hardBlock && readOnlyTools[c.Name] {
				return interceptDecision{Intercept: true, Content: readStreakBlockMessage(readHardThreshold)}
			}
			// Test-fight hard block: once the agent has re-run the test
			// suite past the threshold without changing production code,
			// refuse further test RUNS so it stops the rewrite-rerun loop
			// and finalizes. Only the test-run bash call is blocked —
			// edits still pass, so it can remove/skip the test or extract
			// the unit for testability.
			if !opts.ReadOnly && testFightStreak >= testFightHardBlock && c.Name == "bash" && bashCallRunsTests(c.Input) {
				return interceptDecision{Intercept: true, Content: testFightBlockMessage()}
			}
			// Explore-before-edit gate. Block the first write/edit
			// when the agent prompt contained an EXPLORE directive
			// and no bash call has happened yet. The 2026-05-26
			// snapshot-count executor pinned this: the plan said
			// "EXPLORE: max 3 turns — run kai snapshot list --json
			// to see the exact JSON shape if possible", and the
			// executor skipped straight to editing with an unverified
			// assumption (read snapshot_count from kai stats --json,
			// which doesn't expose that field). The "if possible"
			// hedge made verification feel optional — the gate
			// makes it not optional. Only the FIRST modification is
			// blocked; after one bash call the gate releases.
			if promptRequiresExplore && !runHasSeenBash && editTools[c.Name] {
				return interceptDecision{Intercept: true, Content: exploreBeforeEditBlockMessage()}
			}
			if readOnlyTools[c.Name] {
				readsThisTurnSoFar++
				if opts.MaxReadsPerTurn > 0 && readsThisTurnSoFar > opts.MaxReadsPerTurn {
					return interceptDecision{Intercept: true, Content: perTurnReadCapMessage(opts.MaxReadsPerTurn)}
				}
			}
			if c.Name == "view" {
				if file, start, end, ok := parseViewRange(c.Input); ok {
					if hit, prior := viewTracker.Overlap(file, start, end, currentDisplayTurn); hit {
						// Serve, don't refuse, when the agent re-requests a
						// range we already nudged it on. Refusing the same
						// read twice corners the model — it can't use the
						// buried content, can't re-read, and escapes to
						// `bash cat` (which is itself rejected), thrashing.
						// First overlap nudges; an insistent repeat serves.
						if !viewTracker.ShouldServeAfterDedup(file, start, end) {
							return interceptDecision{Intercept: true, Content: viewRangeStub(file, prior)}
						}
					}
				}
			}
			return interceptDecision{}
		}
		dispatchCalls, decisions := partitionCalls(toolCalls, decide)
		resultParts := dispatchToolCalls(ctx, dispatchCalls, registry, opts.Hooks.OnToolCall, gateBag, dedupeCache, onCallDone, bashFirst, opts.Projects)
		resultParts = spliceIntercepted(toolCalls, resultParts, decisions)
		// Record successful view reads into the range tracker so the
		// dedupe gate above can intercept overlapping repeats on
		// future turns. Errors are intentionally NOT recorded — a
		// failed view (missing file, bad offset) may legitimately be
		// retried by the agent on the next turn.
		for i, c := range toolCalls {
			if c.Name != "view" || decisions[i].Intercept {
				continue
			}
			tr, ok := findToolResult(resultParts, c.ID)
			if !ok || tr.IsError {
				continue
			}
			if file, start, end, ok := parseViewRange(c.Input); ok {
				// Record the ACTUAL number of lines returned, not the
				// requested window. A view of a 571-line file with the
				// default limit=2000 only delivers 571 lines, not 2000 —
				// recording [start, start+2000) over-claims and would
				// dedup re-reads of lines that were never in context.
				// Clamp to the requested end so we never claim MORE than
				// was asked for either.
				returned := start + strings.Count(tr.Content, "\n") + 1
				if returned < end {
					end = returned
				}
				viewTracker.Record(file, start, end, currentDisplayTurn)
			}
		}

		// Loop tracker: record every non-intercepted call's outcome so
		// the next decide() can detect 3-in-a-row identical failures.
		// Intercepted calls are skipped — they never reached the tool,
		// so they're not signal about the model's loop behavior.
		for i, c := range toolCalls {
			if decisions[i].Intercept {
				continue
			}
			tr, ok := findToolResult(resultParts, c.ID)
			if !ok {
				continue
			}
			loops.Record(c.Name, c.Input, tr.IsError)
		}

		// Track bash exit state for the build-success hallucination
		// guard further down. Reflects the LAST bash call in this
		// dispatch batch — i.e. the freshest signal the model has
		// when it composes the next turn. A successful later bash
		// supersedes an earlier failure (the model may have fixed
		// the issue and re-run); a failure supersedes an earlier
		// success the same way.
		//
		// Same pass also computes the error-class signature for the
		// tunnel-vision guard. When the new error's signature
		// differs from the prior turn's, we mutate the bash result's
		// Content to append a one-line `[runner] note:` so the
		// model's next turn sees that the error class CHANGED — its
		// prior fix may have worked, and this is a NEW problem.
		for i, p := range resultParts {
			tr, ok := p.(message.ToolResult)
			if !ok || tr.Name != "bash" {
				continue
			}
			lastBashFailed = tr.IsError
			if !tr.IsError {
				// Successful bash does NOT clear the ring. An
				// intervening unrelated success (a quick `node -e
				// "console.log(...)"` version check between two
				// real build failures) should not erase memory of
				// the prior error class — that was the v0.31.40
				// regression the runlog pinned.
				continue
			}
			curSig := normalizeBashErrSig(tr.Content)
			if curSig == "" {
				continue
			}
			// New error class? Compare against EVERY signature in
			// the ring, not just the most recent. A model that
			// alternates between two error classes (npm error /
			// build error / npm error / build error) shouldn't
			// trip the note on every alternation — only when a
			// genuinely new class first appears.
			if !bashErrSigRingContains(bashErrSigRing, curSig) && len(bashErrSigRing) > 0 {
				note := fmt.Sprintf(
					"\n\n[runner] New error class detected this turn: %q. You've seen %d different error class(es) in recent turns; this one is NEW. Your prior fix may have closed the earlier problem and exposed a different one — re-read THIS error fresh before applying a related fix. Concretely: if the error names a file:line:col, view that file at those lines BEFORE proposing a change. Recent error classes: %s.",
					truncSig(curSig, 80),
					len(bashErrSigRing),
					truncSig(strings.Join(bashErrSigRing, " | "), 200),
				)
				tr.Content += note
				resultParts[i] = tr
			}
			bashErrSigRing = pushBashErrSigRing(bashErrSigRing, curSig)
		}

		// Post-edit re-read injection. After every successful
		// write/edit, append the file's CURRENT content to the
		// tool result so the model's next turn sees the post-edit
		// state inline. Without this the model frequently re-edits
		// based on stale line numbers / surrounding context — the
		// classic "added the same block twice in different
		// places" failure mode. Bounded: we cap each appended
		// view at ~4KB so a 10k-line file doesn't blow up token
		// usage.
		appendPostEditViews(toolCalls, opts, &resultParts)

		// Build-after-edit: per-edit semantic correctness signal. The
		// orchestrator's runBuildCheck is the comprehensive gate at
		// integrate time; this in-loop variant catches compile
		// failures one turn after the edit lands, while the agent is
		// still in the same context as the change. Scoped to the
		// touched file's package for Go, project root for TS/Rust.
		runBuildAfterEdit(ctx, opts, toolCalls, resultParts)

		// Checkpoint verification: if the build trailer above
		// reported FAIL on this turn, any kai_checkpoint call the
		// model made in the same turn was premature — it's claiming
		// the work is done while the build is broken. Replace those
		// results with a rejection so the model sees the contradiction
		// on its next turn instead of marching toward integrate.
		rejectPrematureCheckpoints(resultParts)

		// Update per-run tool counters and append the budget suffix to
		// every ToolResult. The model reads the suffix on its next turn
		// and uses the three numbers (turn, edits, reads) to pace
		// itself — without this it has no visibility into how close it
		// is to the cap or how lopsided its read/edit ratio has grown.
		//
		// Per-turn classification feeds the read-streak tracker:
		//   any edit  → reset streak to 0 (the explore→implement
		//                 transition is the only valid streak-breaker)
		//   reads only → streak++ (extends the run of pure recon)
		//   neither   → streak untouched (build/test bash or
		//                 kai_checkpoint alone are neutral; they
		//                 neither extend nor reset the recon clock.
		//                 A read-only bash turn counts as a read.)
		// readsThisTurn drives the streak counter. readCount is the
		// per-run total and always increments — paging through one
		// file still costs tokens, so per-run accounting reflects
		// that. The split lets the streak gate ignore contiguous
		// paging without losing the cost signal in the budget suffix.
		editsThisTurn, substantiveEditsThisTurn, readsThisTurn, addedReads, newViewFile, newViewEnd := classifyTurnReadsWithDecisions(toolCalls, decisions, lastViewFile, lastViewEnd)
		readCount += addedReads
		editCount += editsThisTurn
		lastViewFile, lastViewEnd = newViewFile, newViewEnd
		// Only a substantive (code-changing) edit resets the streak.
		// A comment/whitespace-only edit is not the explore→implement
		// transition — see isCosmeticEdit for the gaming incident
		// this guards against.
		switch {
		case substantiveEditsThisTurn > 0:
			consecutiveReadTurns = 0
		case readsThisTurn > 0:
			consecutiveReadTurns++
		}
		// Track grep/search-only turns separately. A model hunting
		// for a field name in source for many turns without ever
		// running the underlying command (via bash) is the failure
		// shape the 2026-05-26 dogfood pinned: 11 turns burned
		// searching for 'snapshot_count' / 'SnapshotCount' /
		// 'total_snapshots' across kai's source when 'kai stats
		// --json' would have answered "field does not exist" in
		// one bash call. The tool-description warnings shipped in
		// 0.32.47 didn't move the behavior; this counter+nudge
		// fires from the same place existing runner interventions
		// fire (where the model actually reads them).
		usedBash, usedSearch := classifyTurnSearchVsBash(toolCalls)
		switch {
		case usedBash:
			searchWithoutBashStreak = 0
		case usedSearch:
			searchWithoutBashStreak++
		}
		// Test-fight streak: a production-code edit is real progress and
		// resets it; a turn that runs tests without one extends it.
		ranTests, prodEdit := classifyTurnTestFight(toolCalls)
		switch {
		case prodEdit:
			testFightStreak = 0
		case ranTests:
			testFightStreak++
		}
		// Latch the bash-seen flag for the explore-before-edit gate.
		// Once any bash call has run, the gate releases edits for the
		// rest of the run.
		if usedBash {
			runHasSeenBash = true
		}
		// Track whether this turn made a verification call so the
		// next iteration's tool-strip decision can defer convergence
		// by one turn. Transient (overwritten each turn) — same
		// classifier signal as the streak counter above, just used
		// for a different downstream decision.
		verifiedOnPreviousTurn = usedBash
		appendBudgetSuffix(resultParts, budgetSuffix(turn+1, maxTurns, editCount, readCount))

		// Auto-checkpoint. The model is supposed to call
		// kai_checkpoint after every edit, but in practice it
		// often skips on quick writes — leaving `kai blame` with
		// no authorship data. Record one ourselves per
		// write/edit so authorship lands consistently. The
		// model's manual kai_checkpoint calls still work and
		// stack on top.
		recordAutoCheckpoints(toolCalls, resultParts, opts)

		toolMsg := message.Message{
			Role:  message.RoleUser,
			Parts: resultParts,
		}
		history = append(history, toolMsg)
		if sess != nil {
			if err := sess.AppendMessage(toolMsg, 0, 0); err != nil {
				rec.End(&resp, err)
				return res, err
			}
		}
		// Close the run-log artifact for this turn now that the
		// tool batch has finished and AddToolCall has populated the
		// per-call entries. Next loop iteration's Begin starts a
		// fresh state.
		rec.End(&resp, nil)

		// Per-turn memory sample. Catches the agent-loop variant of
		// the OOM-during-idle problem: a long run that slowly grows
		// memory across turns is easy to lose track of in the
		// 60s-interval idle sampler. This logs one line per turn to
		// ~/.kai/memory-stats.log so a post-mortem on a SIGKILL'd
		// run can see exactly which turn pushed RSS over the line.
		memstat.Log("agent-turn")
	}

	// Turn cap reached without a clean stop. Treat this as a
	// checkpoint, not a crash: the caller can offer the user a
	// "continue?" prompt and call Run again with the same session
	// id. End the session as Ended (not Errored) so the DB
	// reflects "paused at cap" rather than "failed".
	res.FinishReason = message.FinishReasonTurnCap
	res.Transcript = history
	if sess != nil {
		_ = sess.End(session.StatusEnded)
	}
	return res, nil
}

// resolveSession centralizes the session-setup branching:
//   - SessionStore nil + SessionID empty → no persistence; runner
//     proceeds with empty history.
//   - SessionStore set + SessionID empty → start a fresh session.
//   - SessionStore set + SessionID populated → resume; load history.
//
// Returned history may be empty (fresh session); the runner seeds it
// with the prompt above.
func resolveSession(opts Options, model string) (*session.Session, []message.Message, error) {
	if opts.SessionStore == nil {
		return nil, nil, nil
	}
	if opts.SessionID != "" {
		s, err := session.Resume(opts.SessionStore, opts.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("agent: resuming session %s: %w", opts.SessionID, err)
		}
		// Conversation-shaped view when the caller opted in
		// (typically the chat agent — see opts.UserVisibleHistoryOnly).
		// 2026-05-26 spec #3: a chat agent resuming a planner's
		// session shouldn't see the planner's JSON tool dispatches
		// and fenced plan emits as raw history — those degrade
		// recall of the actual conversational thread. The filter
		// keeps user prompts + assistant prose + a one-line summary
		// per tool-call cluster, drops system messages entirely.
		// Other tasks (planner / executor / critic) keep the full
		// transcript via the default branch below.
		var hist []message.Message
		if opts.UserVisibleHistoryOnly {
			hist, err = s.UserVisibleHistory()
		} else {
			hist, err = s.History()
		}
		if err != nil {
			return nil, nil, fmt.Errorf("agent: loading history: %w", err)
		}
		return s, hist, nil
	}
	s, err := session.New(opts.SessionStore, opts.TaskName, opts.Workspace, model)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: creating session: %w", err)
	}
	return s, nil, nil
}

// buildToolRegistry registers the file tools (Slice 1) and any
// pre-built tools the caller passed. Future slices will register
// kai_* graph tools and bash here.
func buildToolRegistry(opts Options, gateBag *gateVerdictBag) map[string]tools.BaseTool {
	reg := map[string]tools.BaseTool{}

	// Synthesize a single-project Set when the caller only supplied
	// Workspace. Lets the tool layer always rely on opts.Projects
	// being non-nil without forcing every existing caller (tests,
	// orchestrator-spawned subagents) to know about projects.
	set := opts.Projects
	if set == nil && opts.Workspace != "" {
		set = projects.Single(opts.Workspace)
		// Inherit Graph and GateConfig onto the synthetic project so
		// per-path routing in the file tools doesn't strand callers
		// who set those fields directly.
		if p := set.Primary(); p != nil {
			if opts.Graph != nil {
				p.DB = opts.Graph
			}
			p.GateConfig = opts.GateConfig
		}
	}

	ft := &tools.FileTools{
		Set:         set,
		ReadOnly:    opts.ReadOnly,
		SharedPaths: opts.SharedPaths,
		OnChange: func(rel, op string) {
			if opts.Hooks.OnFileChange != nil {
				opts.Hooks.OnFileChange(rel, op)
			}
		},
		OnBroadcast: func(rel, digest, contentBase64 string) {
			if opts.Hooks.OnFileBroadcast != nil {
				opts.Hooks.OnFileBroadcast(rel, digest, contentBase64)
			}
		},
		OnDiff: func(rel, op, patch string, added, removed int) {
			if opts.Hooks.OnFileDiff != nil {
				opts.Hooks.OnFileDiff(rel, op, patch, added, removed)
			}
			classifyAndEmit(opts, []string{rel}, gateBag)
		},
	}
	// Per-write approval. Wired only when the caller set the hook;
	// headless CLI runs without a hook continue writing immediately.
	if opts.Hooks.OnFileConfirm != nil {
		confirm := opts.Hooks.OnFileConfirm
		ft.Approve = func(_ context.Context, op, path string, added, removed int, diff string) (bool, error) {
			return confirm(op, path, added, removed, diff), nil
		}
	}
	for _, t := range ft.All() {
		reg[t.Info().Name] = t
	}

	// Graph + filesystem tools. KaiTools.All() returns the right
	// subset for whatever's configured: graph-only tools when DB
	// is set, fs-walking tools (kai_files, kai_grep) when
	// Workspace is set, both when both are. Tests that don't need
	// either leave them empty.
	kt := &tools.KaiTools{
		Set:       set,
		Workspace: opts.Workspace,
		// Protected paths flow into kai_impact so its results
		// flag protected files in the same vocabulary the
		// safetygate uses on each mutation.
		Protected: opts.GateConfig.Protected,
		// kai_diff, kai_checkpoint, kai_live_sync need orchestrator-
		// supplied wiring (binary path, authorship writer, remote
		// client) that the runner doesn't have today. They register
		// only when the surrounding caller (cmd/kai/tui.go,
		// orchestrator) extends Options to thread these in. Until
		// then they're silently omitted, which is what the mode
		// system already tolerates.
		KaiBinary:        opts.KaiBinary,
		CheckpointWriter: opts.CheckpointWriter,
		LiveSyncClient:   opts.LiveSyncClient,
		ChannelID:        opts.SyncChannelID,
		AgentName:        opts.TaskName,
		AgentModel:       opts.Model,
		// kai_consult adapter: wrap the consult provider behind the
		// tools.Sender interface so the tools package doesn't have
		// to import internal/agent/provider (which would be a
		// cycle — provider imports tools for ToolInfo).
		ConsultProvider: newConsultSender(opts.ConsultProvider),
		ConsultModel:    opts.ConsultModel,
		ConsultMode:     ResolveMode(opts.Mode).String(),
		// kai_web_search routes through the kailab base URL + token
		// the caller has already resolved (from `kai auth login`
		// creds). Tool registers only when both are present; tests
		// and offline runs get it omitted.
		KailabBaseURL: opts.KailabBaseURL,
		KailabToken:   opts.KailabToken,
		// kai_logs registers when a managed-process logger is
		// configured by the caller (TUI chat agent path). Nil
		// silently omits the tool.
		ManagedProcLogger: opts.ManagedProcLogger,
	}
	// Only assign opts.Graph when it's a real, non-nil pointer.
	// KaiTools.DB is the KaiGrapher *interface*; assigning a typed-
	// nil *graph.DB would produce a non-nil interface that wraps
	// nil — KaiTools.All() would then skip the "fall back to
	// Set.Primary().DB" path and the symbol-aware tools would crash
	// on the first method call. Callers that pass a Set without an
	// explicit Graph (the planner agent is the canonical case) rely
	// on this filter.
	if opts.Graph != nil {
		kt.DB = opts.Graph
	}
	for _, t := range kt.All() {
		reg[t.Info().Name] = t
	}

	// Bash is opt-in: tests stay shell-free by default; the TUI
	// flips EnableBash=true so the agent can run npm test, etc.
	if opts.EnableBash {
		bt := &tools.BashTool{
			Workspace: opts.Workspace,
			Allow:     opts.BashAllow,
			OnOutput: func(line string) {
				if opts.Hooks.OnBashOutput != nil {
					opts.Hooks.OnBashOutput(line)
				}
			},
			OnFilesChanged: func(paths []string) {
				classifyAndEmit(opts, paths, gateBag)
			},
		}
		// Per-command approval. Wired only when a hook is set —
		// headless CLI runs with no hook fall back to the allowlist
		// alone (existing behavior). The hook signature is bool;
		// BashTool's signature returns (bool, error) so we adapt.
		if opts.Hooks.OnBashConfirm != nil {
			confirm := opts.Hooks.OnBashConfirm
			bt.Approve = func(_ context.Context, cmd, warning string) (bool, error) {
				return confirm(cmd, warning), nil
			}
		}
		reg[bt.Info().Name] = bt
	}

	for _, t := range opts.ExtraTools {
		reg[t.Info().Name] = t
	}

	// context_lookup is a synthetic dummy tool used by the
	// graph-powered context injection. Registered so the provider's
	// schema check passes when a tool_use for context_lookup appears
	// in history; the model is told not to call it (see
	// contextLookupTool.Info().Description) but if it does, the
	// response is harmless.
	if _, exists := reg[contextLookupToolName]; !exists {
		reg[contextLookupToolName] = contextLookupTool{}
	}

	// Mode-based whitelist. Drops tools the current mode forbids
	// (e.g. write/edit/bash for Planning, Review, Conversation) so
	// the model never sees them in its tool list and can't invoke
	// them even if it tries. ModeUnknown resolves to Coding which
	// allows the full set, so callers that don't set Mode keep the
	// pre-modes behavior. Tools listed in AllowedTools but not
	// registered (future kai_impact, kai_diff, etc.) are silently
	// dropped — the spec lists them ahead of implementation.
	allowed := make(map[string]bool, len(opts.Mode.AllowedTools()))
	for _, name := range opts.Mode.AllowedTools() {
		allowed[name] = true
	}
	for name := range reg {
		if !allowed[name] {
			delete(reg, name)
		}
	}
	return reg
}

func toolInfos(reg map[string]tools.BaseTool) []tools.ToolInfo {
	out := make([]tools.ToolInfo, 0, len(reg))
	for _, t := range reg {
		out = append(out, t.Info())
	}
	// Sort by name so the serialized tool list is byte-stable across
	// turns. Without this, Go's randomized map iteration produces a
	// different ordering every turn, which means the kailab/Anthropic
	// request bytes for the tools section differ each turn — and the
	// cache_control marker on the last tool lands on a different tool
	// name each turn. Result: cache_read=0 on every turn after the
	// first, which is exactly the symptom we observed on the moby
	// benchmark (May 2026: 4 executor turns, only the LAST one
	// reused cache; turns 1 and 2 paid full cache_write rates for
	// what should have been a stable prefix). The runlog's section
	// hash hid this for a while because runlog already calls
	// stableTools before hashing — but the wire payload doesn't.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// unknownToolMessage produces the body of a tool-not-found
// tool_result. Lists the currently-registered tools so the model
// can pivot on the next turn instead of falling into the failure
// shape we observed on 2026-05-11: model calls `edit` in planning
// mode → registry returns "unknown tool: edit" → model gives up on
// the kai tools and tries `bash: cd /home/user/repos && cat ...`
// (with a hallucinated path) → still fails → eventually pivots to
// "describe the fix" anyway, but after three wasted turns.
//
// The intent here is purely diagnostic — the runner still reports
// the call as an error and IsError=true, so the model's loop-state
// (turn counter, max-turns guard) is unchanged. We just trade an
// opaque error message for an actionable one.
func unknownToolMessage(name string, reg map[string]tools.BaseTool) string {
	available := make([]string, 0, len(reg))
	for n := range reg {
		// Don't advertise the synthetic context_lookup — the
		// description explicitly says "do not call this tool."
		// Listing it as available encourages the model to try.
		if n == "context_lookup" {
			continue
		}
		available = append(available, n)
	}
	sort.Strings(available)
	return fmt.Sprintf(
		"Tool %q isn't registered in this mode. Available tools: %s. "+
			"If you need to modify files but this mode doesn't allow it, describe the change in your final response — the user can switch modes or run a planning turn explicitly.",
		name, strings.Join(available, ", "))
}

// extractToolCalls returns the ToolCall parts from a content slice.
// Pure helper kept here (not on Message) because it's only used by the
// runner; promoting it to message.Message would muddy that package's
// scope.
func extractToolCalls(parts []message.ContentPart) []message.ToolCall {
	var out []message.ToolCall
	for _, p := range parts {
		if tc, ok := p.(message.ToolCall); ok {
			out = append(out, tc)
		}
	}
	return out
}

// withPerTurnHints returns a shallow copy of `history` whose last
// USER message has the given hints appended as a final TextContent
// block. Returns history unchanged when hints is empty or there is
// no user message to attach to.
//
// This is the cache-friendly home for per-turn injected content
// (graph context block, convergence nudge): the historical prefix
// stays byte-stable so Anthropic's cache reads, and the new bytes
// land on the message slice's growing tail where they don't break
// any prior breakpoint.
//
// Why the LAST USER message specifically: Anthropic's API requires
// strict user/assistant alternation, so we can't append a fresh
// user message after an assistant turn. The last user message is
// always either (a) the user's original prompt this run or (b) the
// freshly-built tool_results from the just-completed dispatch.
// Both are NEW bytes this turn — augmenting them adds zero cache
// invalidation since they weren't in any prior cached prefix.
//
// We never mutate the message in place; mutating would corrupt the
// persisted session. Instead we copy the tail message, copy its
// Parts slice, append the hint block, and substitute it back into
// a copied history slice.
func withPerTurnHints(history []message.Message, hints string) []message.Message {
	if hints == "" || len(history) == 0 {
		return history
	}
	// Append the hint as a brand-new ephemeral user message at the
	// tail rather than mutating the existing last user message.
	//
	// The previous implementation appended the hint as a new
	// TextContent part on the last user message in the slice. That
	// looked harmless because we always made a shallow copy first
	// — but it broke prompt caching: on turn N the hint landed on
	// whatever message was last (the seed user prompt, or a
	// tool_result), and on turn N+1 that same message was no
	// longer last (a fresh tool_result was) so it got sent without
	// the hint. The bytes of message[N] therefore differed
	// between the turn-N cached request and the turn-N+1 request,
	// invalidating the prefix cache at exactly that index — which
	// in practice is what you saw as "cache_read=0 every turn"
	// even though we'd already opted out of the trim. Confirmed
	// May 2026 via run-log per-message hash diff:
	//
	//   msg[0..25]: =  ...
	//   msg[26]:    !  9087cb65a0a6 → 22e07038e75d   (hint added/dropped)
	//
	// Appending instead of mutating makes the existing message
	// bytes turn-stable: all previously-sent messages reach the
	// next turn byte-identical, the cache prefix matches, and only
	// the trailing ephemeral hint is fresh write.
	hintMsg := message.Message{
		Role: message.RoleUser,
		Parts: []message.ContentPart{
			message.TextContent{Text: "[runner: " + hints + "]"},
		},
	}
	out := make([]message.Message, 0, len(history)+1)
	out = append(out, history...)
	out = append(out, hintMsg)
	return out
}

// appendPostEditViews refreshes the model's mental model after
// every successful write/edit by APPENDING the file's current
// content to the matching tool_result's Content field. The model
// sees the post-edit state on its next turn alongside the normal
// "wrote N bytes" line, so it doesn't re-edit using stale line
// numbers — the "added the same block twice in different places"
// pathology.
//
// Why we modify the existing ToolResult (not append a new one):
// Anthropic requires every tool_result.tool_use_id to match a
// tool_use.id from the prior assistant message. A synthetic
// "postedit_<id>" gets rejected with `unexpected tool_use_id
// found in tool_result blocks`. Mutating the existing result is
// invisible to that validation since we're just lengthening
// content for an already-valid pairing.
//
// Best-effort: file-read failures are silently skipped. Caps each
// appended view at viewByteCap to bound token cost on large files.
func appendPostEditViews(toolCalls []message.ToolCall, opts Options, parts *[]message.ContentPart) {
	const viewByteCap = 4096

	// Track files we've already injected this turn so two edits
	// to the same file don't double up.
	seen := map[string]bool{}
	for _, call := range toolCalls {
		if call.Name != "write" && call.Name != "edit" {
			continue
		}
		var args struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
			continue
		}
		if args.FilePath == "" || seen[args.FilePath] {
			continue
		}
		seen[args.FilePath] = true

		// Resolve absolute path via Projects (multi-root) or
		// Workspace, mirroring the file tools' resolution rules.
		abs := args.FilePath
		switch {
		case opts.Projects != nil && opts.Projects.Primary() != nil:
			if proj := opts.Projects.ProjectFor(abs); proj != nil {
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(opts.Projects.DiscoveryRoot, abs)
				}
			} else if !filepath.IsAbs(abs) {
				abs = filepath.Join(opts.Projects.Primary().Path, abs)
			}
		case opts.Workspace != "":
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(opts.Workspace, abs)
			}
		}

		body, err := readFileCapped(abs, viewByteCap)
		if err != nil {
			continue
		}
		trailer := fmt.Sprintf(
			"\n\n[runner: %s now contains the following — use these line numbers for any further edits to this file]\n%s",
			args.FilePath, body)

		// Find the matching ToolResult in parts and append the
		// trailer to its Content. The result's ToolCallID
		// matches the call's ID exactly (dispatchToolCalls
		// preserves it), so we look it up by that.
		for i, p := range *parts {
			tr, ok := p.(message.ToolResult)
			if !ok || tr.ToolCallID != call.ID {
				continue
			}
			tr.Content += trailer
			(*parts)[i] = tr
			break
		}
	}
}

// interceptDecision is the per-call verdict produced by partitionCalls.
// Intercept=true means the call must NOT be dispatched — instead the
// splice step will synthesize a ToolResult carrying Content (and the
// IsError flag, defaulted true since every intercept here is a
// refusal). Generalizes across the read-streak hard block and the
// view-range dedupe; future gates plug in by extending the decider.
type interceptDecision struct {
	Intercept bool
	Content   string
}

// loopTracker spots identical-failure loops: same tool, same input,
// erroring N times in a row. The 2026-05-24 install-kai dogfood had
// the agent retry `cd /Users/... && go build` 18+ times with the same
// "go: go.mod file not found" error and no orchestrator gate caught
// it. This catches the next one at iteration N+1.
//
// Detection is exact-match on (toolName, input). It does NOT diff the
// error content — different stderr per attempt (a timestamp, a PID)
// still counts as "the same failure" for our purposes; the model
// isn't learning new information from those bytes. False positives
// on legitimately-retried operations are tolerable because the
// intercept content tells the agent to try something different, not
// to never run the call again — a turn later the agent can call it
// once more if it actually needs to (the tracker resets on success
// or on any other tool call interleaving).
type loopTracker struct {
	recent []loopRecord // bounded to loopTrackerWindow
}

type loopRecord struct {
	name    string
	input   string
	isError bool
}

const (
	// loopTrackerWindow caps how much history we keep — strictly
	// greater than the trigger threshold so we can always see one
	// non-matching entry beyond a streak.
	loopTrackerWindow = 8
	// loopTrackerTrigger is "after this many consecutive identical
	// failures, intercept the next attempt." 3 is the smallest
	// number that's clearly intentional retry-not-coincidence (one
	// retry is normal, two might be "I'm trying a variation," three
	// is "the agent is stuck").
	loopTrackerTrigger = 3
)

func newLoopTracker() *loopTracker {
	return &loopTracker{recent: make([]loopRecord, 0, loopTrackerWindow)}
}

// Record appends a tool outcome. Truncates from the front when over
// window.
func (l *loopTracker) Record(name, input string, isError bool) {
	l.recent = append(l.recent, loopRecord{name: name, input: input, isError: isError})
	if len(l.recent) > loopTrackerWindow {
		l.recent = l.recent[len(l.recent)-loopTrackerWindow:]
	}
}

// IsLoop returns true when the proposed call matches the last
// loopTrackerTrigger entries AND all of them were errors. Caller
// should intercept if so.
func (l *loopTracker) IsLoop(name, input string) bool {
	if len(l.recent) < loopTrackerTrigger {
		return false
	}
	tail := l.recent[len(l.recent)-loopTrackerTrigger:]
	for _, r := range tail {
		if r.name != name || r.input != input || !r.isError {
			return false
		}
	}
	return true
}

// loopInterceptMessage is what the agent sees on the intercepted Nth
// attempt. Names the failure mode explicitly so the model has a
// chance to actually reformulate instead of trying yet another
// variation of the same broken call.
func loopInterceptMessage(name string) string {
	return fmt.Sprintf(
		"loop detected: you've called `%s` with identical input %d times in a row and it errored every time. "+
			"The %d+1th attempt is blocked. The next call you make MUST be different — different tool, "+
			"different input, or an explicit step to diagnose why the earlier attempts failed (e.g. `pwd` to "+
			"confirm working directory, `ls` to confirm a path exists, view-ing a file you assumed existed). "+
			"If you genuinely cannot make progress without re-running this exact call, stop and report what "+
			"you've tried and what you'd need to proceed.",
		name, loopTrackerTrigger, loopTrackerTrigger)
}

// partitionCalls splits toolCalls into a dispatch list and a parallel
// per-call decision slice. The decision slice always has len(calls)
// entries (one per original call); dispatch holds only the calls
// whose decision was Intercept=false. Order of dispatch matches the
// order of non-intercepted calls in the original list, which is what
// spliceIntercepted relies on to zip results back together.
func partitionCalls(calls []message.ToolCall, decide func(message.ToolCall) interceptDecision) (
	dispatch []message.ToolCall,
	decisions []interceptDecision,
) {
	decisions = make([]interceptDecision, len(calls))
	dispatch = make([]message.ToolCall, 0, len(calls))
	for i, c := range calls {
		d := decide(c)
		decisions[i] = d
		if !d.Intercept {
			dispatch = append(dispatch, c)
		}
	}
	return dispatch, decisions
}

// spliceIntercepted zips together synthetic intercept ToolResults and
// the real dispatch results, restoring one-for-one alignment with the
// original toolCalls slice. Required for Anthropic's tool_result/
// tool_use_id pairing — a missing slot rejects the entire next
// request.
func spliceIntercepted(calls []message.ToolCall, dispatched []message.ContentPart, decisions []interceptDecision) []message.ContentPart {
	anyIntercept := false
	for _, d := range decisions {
		if d.Intercept {
			anyIntercept = true
			break
		}
	}
	if !anyIntercept {
		return dispatched
	}
	merged := make([]message.ContentPart, 0, len(calls))
	di := 0
	for i, c := range calls {
		if decisions[i].Intercept {
			merged = append(merged, message.ToolResult{
				ToolCallID: c.ID,
				Name:       c.Name,
				Content:    decisions[i].Content,
				IsError:    true,
			})
			continue
		}
		if di < len(dispatched) {
			merged = append(merged, dispatched[di])
			di++
		}
	}
	return merged
}

// rejectPrematureCheckpoints scans parts for a build-after-edit FAIL
// trailer and, when one is present, rewrites every kai_checkpoint
// ToolResult in the same batch to a rejection error. The model reads
// the rejection on the next turn and corrects course before any
// downstream stage takes the checkpoint at face value.
//
// Why this lives in the runner rather than the kai_checkpoint tool:
// the build trailer is added AFTER the tool executes, and the tool
// has no view of sibling results. Centralizing here keeps the
// kai_checkpoint tool focused on authorship recording (single
// responsibility) while still tying the two signals together.
func rejectPrematureCheckpoints(parts []message.ContentPart) {
	buildFailed := false
	for _, p := range parts {
		tr, ok := p.(message.ToolResult)
		if !ok {
			continue
		}
		if strings.Contains(tr.Content, "[auto-build: FAIL") {
			buildFailed = true
			break
		}
	}
	if !buildFailed {
		return
	}
	for i, p := range parts {
		tr, ok := p.(message.ToolResult)
		if !ok || tr.Name != "kai_checkpoint" {
			continue
		}
		tr.Content = checkpointRejectMessage
		tr.IsError = true
		parts[i] = tr
	}
}

const checkpointRejectMessage = `[runner] Checkpoint rejected: build is currently broken (see the [auto-build: FAIL] trailer above). Calling kai_checkpoint while the build fails records authorship for non-compiling code and signals "step done" downstream. Fix the build error first, then re-checkpoint after the next successful build trailer.`

// findToolResult locates the ToolResult in parts whose ToolCallID
// matches id. Used by the post-dispatch range-record pass so we only
// memoize successful, real-dispatched view calls — synthetic
// intercept results and errors are excluded.
func findToolResult(parts []message.ContentPart, id string) (message.ToolResult, bool) {
	for _, p := range parts {
		tr, ok := p.(message.ToolResult)
		if !ok || tr.ToolCallID != id {
			continue
		}
		return tr, true
	}
	return message.ToolResult{}, false
}

// budgetSuffix formats the one-line status trailer that gets appended
// to every ToolResult before the model sees it. Three numbers — turn,
// edits, reads — let the model pace itself between exploration and
// implementation instead of burning the cap on re-reads. turn is
// 1-indexed in the formatted string; callers pass the 0-indexed loop
// counter incremented to match what the model already sees in narration.
func budgetSuffix(turn, maxTurns, edits, reads int) string {
	return fmt.Sprintf("\n\n[turn %d/%d · edits: %d · reads: %d]",
		turn, maxTurns, edits, reads)
}

// appendBudgetSuffix appends suffix to the Content of every ToolResult
// in parts. Mirrors appendPostEditViews' direct-mutation pattern: the
// ContentPart interface holds value-typed ToolResult, so we read, edit,
// and write back at the slice index.
func appendBudgetSuffix(parts []message.ContentPart, suffix string) {
	for i, p := range parts {
		tr, ok := p.(message.ToolResult)
		if !ok {
			continue
		}
		tr.Content += suffix
		parts[i] = tr
	}
}

// readFileCapped returns up to maxBytes of the file's content,
// prefixed with line numbers (1-indexed, matching the view tool's
// format) so the model can reason about offsets. When the file
// exceeds maxBytes we include the head, a truncation marker, and
// move on — the head is where edits usually go.
func readFileCapped(path string, maxBytes int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	truncated := false
	if len(data) > maxBytes {
		data = data[:maxBytes]
		truncated = true
	}
	lines := strings.Split(string(data), "\n")
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%d: %s\n", i+1, line)
	}
	if truncated {
		fmt.Fprintf(&b, "(truncated at %d bytes; view the file with offset+limit if you need the rest)\n", maxBytes)
	}
	return b.String(), nil
}

// recordAutoCheckpoints writes an authorship checkpoint per
// successful write/edit so `kai blame` finds attribution data even
// when the model didn't bother calling kai_checkpoint itself.
//
// The model IS supposed to call kai_checkpoint after each edit
// (per the Coding mode system prompt), but in practice it skips
// that step on quick scaffolds — leaving authorship empty. This
// auto-checkpoint runs unconditionally so blame data lands
// regardless of model compliance. Manual kai_checkpoint calls from
// the model still work; they stack on top of the auto-recorded
// ones.
//
// Best-effort: missing CheckpointWriter, missing TaskName, file-
// stat failures all silently skip. We never fail the run for
// authorship bookkeeping.
//
// For the line range: we use 1..lineCount on creates, and the diff's
// min/max changed line on edits (parsed from the diff text in the
// tool result). When parsing fails we fall back to 1..lineCount —
// imprecise but better than no record.
func recordAutoCheckpoints(toolCalls []message.ToolCall, results []message.ContentPart, opts Options) {
	if opts.CheckpointWriter == nil {
		return
	}
	for i, call := range toolCalls {
		if call.Name != "write" && call.Name != "edit" {
			continue
		}
		var args struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
			continue
		}
		if args.FilePath == "" {
			continue
		}
		// Skip if the matching tool result reports an error —
		// don't record authorship for failed writes.
		if i < len(results) {
			if tr, ok := results[i].(message.ToolResult); ok && tr.IsError {
				continue
			}
		}

		abs := args.FilePath
		switch {
		case opts.Projects != nil && opts.Projects.Primary() != nil:
			if !filepath.IsAbs(abs) {
				if proj := opts.Projects.ProjectFor(abs); proj != nil {
					abs = filepath.Join(opts.Projects.DiscoveryRoot, abs)
				} else {
					abs = filepath.Join(opts.Projects.Primary().Path, abs)
				}
			}
		case opts.Workspace != "":
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(opts.Workspace, abs)
			}
		}

		// Line count: read the file and count newlines. The
		// auto-checkpoint records the WHOLE file as authored on
		// every edit; per-line precision would need the diff
		// parsed, which we skip for now. Imprecise attribution
		// still gives `kai blame` something useful to show.
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		lineCount := strings.Count(string(data), "\n")
		if lineCount == 0 && len(data) > 0 {
			lineCount = 1
		}
		if lineCount < 1 {
			continue
		}
		action := "modified"
		if call.Name == "write" {
			// Couldn't check the pre-state from here; assume
			// "created" for write since the write tool is
			// usually used for new files. The Consolidate step
			// in `kai capture` resolves overlapping records
			// correctly regardless.
			action = "created"
		}
		_, _ = opts.CheckpointWriter.Write(authorship.CheckpointRecord{
			File:       args.FilePath,
			StartLine:  1,
			EndLine:    lineCount,
			Action:     action,
			AuthorType: "ai",
			Agent:      opts.TaskName,
			Model:      opts.Model,
			Timestamp:  time.Now().UnixMilli(),
		})
	}
}

// extractMutatedPaths returns the file paths a mutating tool just
// touched, so dispatchToolCalls can evict any cached read-only
// results referencing those paths. Returns nil for tools we don't
// recognize as path-mutating — bash is intentionally excluded
// because the command is opaque (best-effort would over-invalidate).
func extractMutatedPaths(toolName, inputJSON string) []string {
	switch toolName {
	case "write", "edit":
		// fall through
	default:
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &v); err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"path", "file_path", "file"} {
		if s, ok := v[k].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// evictCacheForPath removes any dedupeCache entries whose canonical
// JSON input contains the given path. Cheap substring scan — the
// cache holds only entries from the current run, so it stays small.
// Caller holds dedupeMu.
func evictCacheForPath(cache map[string]string, path string) {
	if path == "" {
		return
	}
	// Match the JSON-encoded form so we don't accidentally hit
	// arbitrary text in tool output (cache keys end with the JSON
	// input, not the result).
	needle := strconv.Quote(path)
	for k := range cache {
		// Cache key is `name\x00<canonical-json>`; check the json half.
		idx := strings.IndexByte(k, '\x00')
		if idx < 0 {
			continue
		}
		if strings.Contains(k[idx+1:], needle) || strings.Contains(k[idx+1:], path) {
			delete(cache, k)
		}
	}
}

// canonicalToolInput normalizes a JSON tool-input string so the
// dedupe cache treats `{"a":1,"b":2}` and `{"b":2,"a":1}` as
// equivalent. Falls back to the raw string when the input isn't
// valid JSON — better to occasionally miss a dedupe opportunity
// than to crash on a tool that doesn't take JSON input.
func canonicalToolInput(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
}

// canonicalToolInputForCache extends canonicalToolInput with
// project-aware path normalization. When a tool's input contains a
// path-shaped field (`path`, `file_path`, `file`) and the agent's
// projects.Set has multiple projects, the path is resolved through
// the set's ByName + ProjectFor logic so that two inputs naming
// the same file via different prefix conventions produce the same
// dedupe key.
//
// Concretely: in a multi-root workspace with a project named "Kai"
// at path "<spawn>/Kai/", both these inputs canonicalize to the
// same form:
//   - {"file_path": "Kai/kai-cli/foo.go"}
//   - {"file_path": "<spawn>/Kai/kai-cli/foo.go"}
//
// Without this, sonnet's path-prefix improvisation produced
// cache-missing duplicates that LOOKED identical in the TUI log
// (see the 2026-05-11 DX review).
//
// Falls back to canonicalToolInput when the set is single-root or
// nil. Best-effort: any resolution failure leaves the field
// unchanged rather than panicking.
func canonicalToolInputForCache(raw string, set *projects.Set) string {
	if set == nil || len(set.Projects()) <= 1 {
		return canonicalToolInput(raw)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	for _, key := range []string{"path", "file_path", "file"} {
		s, ok := v[key].(string)
		if !ok || s == "" {
			continue
		}
		if abs, ok := resolveProjectPath(set, s); ok {
			v[key] = abs
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
}

// resolveProjectPath maps a tool-input path string to its absolute
// form via the projects.Set. Mirrors the routing in tools/file.go
// (ByName head-match for project-prefixed paths, ProjectFor for
// already-absolute paths). Returns the canonical form + ok=true on
// success; ok=false leaves the caller free to fall back to the
// raw string.
func resolveProjectPath(set *projects.Set, p string) (string, bool) {
	if set == nil || p == "" {
		return "", false
	}
	// Project-name prefix: "Kai/kai-cli/foo.go" → set.ByName("Kai").
	// Tolerant: only fires when the head segment exactly matches a
	// project name and the rest stays inside the project's tree.
	if !strings.HasPrefix(p, "/") {
		head, rest, found := strings.Cut(p, "/")
		if found && head != "" {
			if proj := set.ByName(head); proj != nil {
				abs := strings.TrimRight(proj.Path, "/") + "/" + rest
				return abs, true
			}
		}
	}
	// Already absolute (or relative under the workspace) — resolve
	// against the set's ProjectFor mapping when available. Failure
	// here is non-fatal; the caller leaves the field unchanged.
	if strings.HasPrefix(p, "/") {
		if proj := set.ProjectFor(p); proj != nil {
			return p, true
		}
	}
	return "", false
}

// splitSystemAndUser pulls a "System: ..." prefix off the prompt if
// the agentprompt builder emitted one. Slice 1's agentprompt produces
// a single string; future agentprompt revisions can return roles
// directly and this helper goes away.
func splitSystemAndUser(prompt string) (system, user string) {
	const sysPrefix = "System:"
	if strings.HasPrefix(prompt, sysPrefix) {
		// Take everything up to the first blank line as system.
		rest := strings.TrimPrefix(prompt, sysPrefix)
		if i := strings.Index(rest, "\n\n"); i >= 0 {
			return strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i+2:])
		}
		return strings.TrimSpace(rest), ""
	}
	// No explicit system role — let the model treat the whole thing
	// as the user message and use its default system prompt. The
	// agent prompt builder already includes identity + boundaries.
	return "", prompt
}

// budgetExceeded checks the cumulative token usage against the
// per-run cap if one was set. Returns (exceeded, cap).
func budgetExceeded(res *Result, opts Options) (bool, int) {
	if opts.MaxTotalTokens <= 0 {
		return false, 0
	}
	used := res.TokensIn + res.TokensOut
	return used > opts.MaxTotalTokens, opts.MaxTotalTokens
}

// bashFirstGate is the runtime state of the "run-the-project before
// reading source" rule. Armed at run start when the user's request
// is error-shaped without a pasted trace; disarmed on the first bash
// call regardless of exit code (the agent has a real signal now).
//
// Concurrent-safe: dispatchToolCalls fans out read-only tools across
// goroutines and they all consult this gate; the mutex ensures the
// "is armed?" check and the "disarm on bash" flip happen atomically.
type bashFirstGate struct {
	mu    sync.Mutex
	armed bool
}

func (g *bashFirstGate) IsArmed() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.armed
}

func (g *bashFirstGate) Disarm() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = false
}

// needsBashFirst returns true when the user's prompt looks like
// an error report ("X is broken", "getting an error", "doesn't work")
// and does NOT contain a pasted error trace. Heuristic: reuse the
// existing error-signature checks for the "trace was pasted" half;
// add a separate set of natural-language patterns for the bug-report
// half.
//
// False negatives are fine — the gate is an optimization, not a
// safety boundary. False positives (gate fires when the user
// actually pasted a hard-to-recognize trace) just mean one extra
// bash call before the agent recovers; the cost is minor.
func needsBashFirst(prompt string) bool {
	if strings.TrimSpace(prompt) == "" {
		return false
	}
	lower := strings.ToLower(prompt)
	if hasErrorSignature(prompt, lower) {
		return false
	}
	for _, re := range bugReportRegexes {
		if re.MatchString(lower) {
			return true
		}
	}
	return false
}

// bashFirstRejectMessage is what the dispatcher returns for any
// non-bash tool call while the gate is armed. The message names bash
// explicitly and explains why — the model treats tool errors as loud
// signal where it ignores prompt-level guidance.
func bashFirstRejectMessage(toolName string) string {
	return fmt.Sprintf(
		"%s rejected: this turn requires running the project first. The user reported a problem without pasting an error message — reading source files now is speculation that wastes tokens chasing the wrong file. Read package.json's \"scripts\" section and run the dev/test/start command via bash. Once stderr lands you'll have the real error to anchor on, and reads (or kai_diagnose with the captured error) become productive. (kai_diagnose IS allowed if you can already pass an error string from the user's wording.)",
		toolName,
	)
}
