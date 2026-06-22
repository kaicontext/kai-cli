# Changelog

All notable changes to Kai are documented here.

## [0.32.11] — 2026-05-25

### Fixed
- **Auto-followup now pastes the full vite/plugin error context block, not just one line.** 2026-05-25 dogfood pinned a structural misdiagnosis the recognize-first preamble (v0.32.8/9) couldn't fix on its own: a Svelte template error reported by vite as `AgentsView.svelte:181:63 Unexpected token`. The actual buggy code (a raw `{` in template text) was on source line **281** — not 181. Difference: exactly 100 lines, the size of the file's `<script lang="ts">` block. `svelte-preprocess` strips that script during preprocessing, so vite's diagnostics reference POST-preprocessing line numbers; the agent's `view` tool reads SOURCE. The numbers don't reconcile, and the model had no way to know the offset existed.
- Why the standalone model got this right while kai's agent didn't: the user's playground paste included vite's CONTEXT BLOCK — the numbered source-line excerpt with the carat (`181 |    + if showFlag {` / `                            ^`). That excerpt carries the actual offending CONTENT, which is invariant under line-number shifts. Kai's `detectHostCommandError` was capturing only the first error-keyword line and dropping the rest of the block before the followup got built.
- Fix: `detectHostCommandError` now detects vite/plugin-style errors (markers: `[vite]`, `[plugin:`, `vite-plugin-`, `Pre-transform error`, `Internal server error`) and returns the full multi-line context block — error header + Plugin/File lines + numbered source excerpt + carat — up to 15 lines / 1200 bytes. The auto-followup prompt in both paths (capture-window `HostCommandDoneMsg` and managed-process scanner `error_detected` event) pastes the block verbatim, so the model sees the buggy content directly and never has to trust the (preprocessor-shifted) line number.
- Display: when a detected error contains newlines, the TUI renders a one-line header followed by indented dim block lines so scrollback stays scannable.
- Plain non-vite errors (no plugin markers nearby) still capture a single line — the block path only triggers when vite/plugin signals are present within 4 lines of the error keyword.

## [0.32.10] — 2026-05-25

### Fixed
- **Auto-followup from host-process errors now forces coding mode.** 2026-05-25 dogfood pinned a hard-to-debug interaction: user typed `/code` twice expecting the next "kai detected an error" auto-followup to run in coding mode (with write+edit tools), but the model still reported "no edit tool registered here — view only." Asking the agent directly returned "I am in chat mode." The root cause: when `/chat` was used earlier in the session, the agent_sessions table's `prev_mode` column was set to "Conversation". When kai's managed-process scanner later detected an error and auto-dispatched a followup via `r.dispatch()`, the dispatch consulted `r.forcedMode` (which had been consumed by an earlier turn and reset to `ModeUnknown`) plus the sticky check (which found prev_mode=Conversation and locked routing to chat). The agent's report was technically correct — it WAS in chat mode — but it surprised the user who had explicitly `/code`'d.
- Fix: both auto-followup paths (the v0.31.45 `HostCommandDoneMsg` capture-window detection + the v0.32.0 `HostProcEventMsg.error_detected` managed-process scanner) now set `r.forcedMode = agent.ModeCoding` before calling `r.dispatch(followup)`. The "kai detected an error" follow-up is implicitly a fix request; it requires write/edit tools. The override means the followup ignores any prior sticky-chat state.
- Side benefit: when the user has been in `/chat` reviewing an error and kai catches the error themselves, the followup automatically escalates to coding mode without forcing the user to type `/code` first.

## [0.32.9] — 2026-05-25

### Changed
- **Recognize-first preamble extended to planning mode.** v0.32.8 added it to chat + debug prompts but the planner uses `planningSystemPrompt` — which is what fires for fix-style requests routed through `/code` or via implicit triage. The planning prompt's "emit a structured JSON plan, name concrete files, skip exploration on trivial" bias REWARDS fast commitment. That's a feature for clear scoping; it amplifies wrong priors when the model misreads an error.
- 2026-05-25 dogfood: model saw a Svelte "Unexpected token" error, scanned the file, found `${agent.cost.toFixed(2)}`, and emitted a confident plan to "remove the stray dollar-sign." Wrong on two axes: (a) dollar-then-brace-expression in Svelte template TEXT is valid (literal `$` + Svelte expression) — the dollar was an intentional currency prefix; (b) the actual error was on a DIFFERENT line — a raw `{` in text content, the same bug shape the user had been hitting all day. Planner committed to the visually-suspicious pattern instead of recognizing the real error class.
- New paragraph in `planningSystemPrompt`: when error-shape signals appear (stack trace, file:line:col, error keywords), do NOT pattern-match to the nearest syntactic suspect. First ask "what do I already know about this error class in this language/framework?" If the prior names the cause, build the plan around it. If uncertain, scope the plan around investigation steps. Calls out the dollar-sign pattern explicitly so the model doesn't repeat the misdiagnosis.
- Includes the same recognition-shortcut table for common errors as the chat/debug preamble: Svelte brace, block closing tag, "Cannot find module", "Hydration mismatch", "import.meta.env undefined", "ReferenceError". Closes the gap where the planner path was the ONE path through error-debugging that didn't have these anchors loaded.

### Investigating (not yet shipped)
- **`preflight.missing_blobs` auto-repair keeps firing.** User reports 2m46s reindex on every plan attempt. `errors.log` shows `auto_repaired:false` on every fire. `preflightSpawn` has an in-process self-heal (runs `kai capture` then retries) that should normalize this in one shot, but the error keeps recurring — the heal is either failing silently or not declaring success to the outer auto-repair layer. Needs more diagnostic data from the user's workspace (which project's blobs are missing each time, whether the same digest recurs, whether `kai-e2e` with 1118 modified files is driving churn between captures).

## [0.32.8] — 2026-05-25

### Changed
- **"Recognize before investigate" preamble added to chat + debug prompts.** 2026-05-25 dogfood pinned a costly disconnect: a developer pasted a Svelte "Unexpected token" error pointing at a raw `{` in template text. The model spent 18 minutes investigating — chasing dollar-brace interpolation, preprocessor versions, svelte 5 runes. The same model, asked "how does Svelte templating work" in isolation, explained the curly-brace syntax (dynamic content, #if, #each, @html, etc.) immediately and correctly. **The knowledge was always there. The debug framing suppressed it.** The investigation-first prompt structure was making the model treat every error as a novel puzzle instead of pattern-matching it against its own training priors.
- New paragraph in `conversationSystemPrompt` and the `debugSystemPrompt`'s step-zero block. Tells the model: when a message contains error-shape signals (stack trace, file:line:col, "Unexpected token", "Cannot find module", "ReferenceError", "Hydration mismatch", "Module not found") — first ask "what do I already know about this exact error class in this exact language/framework from training?" If the prior gives a direct answer, propose the fix BEFORE opening any tool. Only investigate if recognition fails.
- Includes a recognition-shortcut table for common errors: Svelte "Unexpected token" at a brace → raw-brace escape. "Unexpected block closing tag" → over-escape or block mismatch. "Cannot find module" → dep / path / extension. "Hydration mismatch" → SSR/CSR difference. "import.meta.env undefined" → missing VITE_ prefix. "ReferenceError" → import / typo. The list is illustrative not exhaustive — the model can recognize other shapes the same way once the recognize-first habit is loaded.

## [0.32.7] — 2026-05-25

### Fixed
- **Despawn kills orphaned processes from the spawn dir.** 2026-05-25 dogfood pinned the cost: the agent's verify pass ran `npm run dev &` then `kill $PID`, but `concurrently` had already forked vite + electron + wait-on as children. Killing the npm parent orphaned the children to init/launchd, and they kept running indefinitely. Every prior orchestrator plan that touched a dev server left a process zoo behind — 9 stale processes from a single prior run in the user's `ps` output. Side effects: port 5173 collisions on next `npm run dev`, user looking at stale Electron windows from old spawn dirs displaying old code, kai's managed-process scanner watching the NEW process but the user's browser connected to the OLD one.
- New `killProcessesUnder(spawnDir)` runs before `kai despawn`: `pgrep -f <spawn-dir>` (and the symlink-resolved variant — macOS often resolves `/tmp` → `/private/tmp`) finds any process whose command line references the spawn dir, SIGTERM with 2-second grace, then SIGKILL on survivors. Same shape as the v0.32.0 managed-process shutdown. Self-pid guard so the orchestrator can't accidentally kill itself.
- Best-effort: when `pgrep` isn't on PATH or returns no matches, the cleanup silently no-ops. Better to leak processes than to block the despawn path on a portability issue. macOS, Linux, and BSDs all have pgrep; the rare system without it is the only regression surface.

## [0.32.6] — 2026-05-25

### Fixed
- **Capture-lock creation now creates the `.kai/` directory if missing.** 2026-05-25 dogfood pinned this: in a multi-root workspace, a sibling project without a `.kai/` directory (e.g. a `design` project never `kai init`'d) caused `acquireCaptureLock` to fail with `open .kai/capture.lock: no such file or directory`. The error then cascaded: `preflight.missing_blobs` auto-repair failed for the WHOLE workspace, blocking plan execution for unrelated kai-aware projects. User's planner had correctly diagnosed an AgentsView.svelte fix and the planner's EDIT CHECKLIST was right — but the orchestrator's snapshot rebuild couldn't proceed because of the sibling project, so the fix never got applied. `os.MkdirAll(kaiDir, 0755)` before `OpenFile` is idempotent (no-op when the dir exists), and resolves the cross-project blockage.

## [0.32.5] — 2026-05-25

### Fixed
- **View tool prepends an unambiguity notice when the file contains literal Svelte brace-escape sequences.** The 2026-05-25 chat-debug log pinned a major kai-side bug: the model viewed `AgentsView.svelte`, saw `{"{"}` byte sequences in the output, and explicitly said *"the view tool is showing escape sequences that make it hard to see..."* — it couldn't tell whether those characters were the file's actual content or display escapes added by view. The dogfood standalone-DeepSeek user pointed out: same model, given the same content directly, fixes this in 30 seconds. Kai's view-tool ambiguity was preventing the model from operating. New `braceEscapeNotice` detects `{"{"}` / `{"}"}` literal patterns and prepends a paragraph: *"the byte sequences `{"{"}` and `{"}"}` you see below ARE the file's actual content (5 literal characters) — the view tool does NOT apply escaping. Common bug shapes: opening tags over-escaped to `{"{"}#each ...{"}"}` paired with a real `{/each}` → Unexpected block closing tag. Raw `{` in text that should have been escaped → Unexpected token. No re-verification with shell tools needed."* Empty notice for clean / non-Svelte content — only fires when the ambiguous pattern is actually present.

- **Bash chain walker pardons shadowed tools after a byte-inspection command.** Same 2026-05-25 log: model wanted to verify byte-level content with `xxd /path/to/file | grep pattern` and `hexdump | head -30` — every attempt rejected because the chain walker caught the shadowed downstream tool (`grep`, `head`, etc.). The anti-pattern rule was about *don't read files this way when view exists* — but kai has NO byte-level inspection tool. Banning the downstream filters in that workflow blocked legitimate byte verification with no offered alternative. New `byteInspectionTools` set (`xxd`, `hexdump`, `od`, `file`) — segments downstream of any of these are pardoned. So `xxd file | grep pattern`, `hexdump -C file | tail -20`, `od -c file | grep each` all pass. `cat file | grep pattern` still flagged (no byte tool in chain). `head -100 file | xxd` still flags `head` (byte tool comes too late to pardon).

- 14 new test cases (`brace_escape_test.go` + `byte_inspection_test.go`) pin both fixes.

### The bigger picture
The chat-debug log analysis revealed something important: when the user said *"standalone DeepSeek-V4-Pro solves this in 30 seconds, but kai-DeepSeek can't"*, the answer was kai-the-tool, not the model. Same capability, different scaffolding. The combination of (a) ambiguous view output, (b) blocked verification tools, and (c) turn-budget pressure forced the agent to commit to "I can't tell" before it could reach the obvious diagnosis. These two fixes close the verification-blocked-by-ambiguity loop directly. Future debugging of "kai struggles where the model standalone doesn't" should look at the chat-debug log first — what did the model actually try, and which kai layer turned it away.

## [0.32.4] — 2026-05-25

### Changed
- **Managed-process output is quiet by default; `/verbose` opts in.** v0.32.1's stream-everything-to-scrollback eat vertical space. vite + electron + concurrently produce dozens of lines per startup (IMKClient init, port-in-use retries, deprecation warnings) plus continuous re-compile noise, so an open dev server filled the scrollback fast. The 2026-05-25 dogfood pinned it: "the logs are showing and scrolling the view which is taking up space."
- Now: quiet-by-default, errors only. Same `/verbose` toggle that controls tool-call rendering also controls managed-process output. Off (default) → output lines dropped on the floor; the error scanner still fires `⚠ kai detected an error` events for real problems; `kai_logs` lets the agent read recent output on demand without anything reaching scrollback. On (`/verbose`) → each output batch streams to scrollback dimmed, same as v0.32.1.
- One toggle for "show me the noisy stuff" across tools + managed-process output keeps the UX cohesive.

## [0.32.3] — 2026-05-25

### Fixed
- **Auto-escalation handles `kai_consult` being unavailable.** v0.32.2's escalation prompt unconditionally told the agent to "Call kai_consult IMMEDIATELY as your first tool" — but the tool only registers when `ConsultModel` is configured. The 2026-05-25 dogfood pinned the failure: the escalated agent said "I don't have a kai_consult tool available, so let me explore the project to understand what 'run it' means" and started a fresh investigation, LOSING ALL PRIOR CONTEXT. Now `maybeAutoEscalate` checks `OrchestratorCfg.ConsultModel` and branches:
  - **Consult configured** → original escalation prompt (call kai_consult immediately).
  - **Consult NOT configured** → forced-finalize prompt: "STOP investigating. Based ONLY on tool results and content you've already seen, commit to a single concrete next step: either (a) an exact edit (file + line + replacement) you propose, or (b) one clear sentence stating what specific piece of information you genuinely need from the user that you cannot infer. Do NOT call more exploratory tools. Pick (a) or (b) and answer briefly — verbosity is the failure mode here."
  - The forced-finalize variant counters the dogfood pathology directly: the stuck DeepSeek turn had the full file content + error message in its context, but kept reaching for new exploration paths instead of acting on what was already visible. The prompt shuts that loop down.
- Banner text differs too: `⚠ agent idle for Xm — auto-escalating via kai_consult` when consult is available, `⚠ agent idle for Xm — forcing finalize (kai_consult not configured)` when not. Lets you see which path fired.

## [0.32.2] — 2026-05-25

### Added
- **Auto-escalation via kai_consult when the chat agent stalls mid-turn.** The 2026-05-25 dogfood showed DeepSeek-V4-Pro grinding for 11+ minutes on an over-escaped Svelte template, reaching for encoding / preprocessor / package.json explanations instead of recognizing the simple template-brace over-escape. A stronger or different-family model usually breaks that loop in one shot. The TUI now watches the chat agent's idle time during a run and, when no streaming activity has fired for **4 minutes**, automatically:
  - Cancels the in-flight chat turn (same path as user-pressed-Esc).
  - Renders `⚠ agent idle for Xm — auto-escalating via kai_consult`.
  - Dispatches a new chat turn with the original request + an explicit instruction: "Call kai_consult IMMEDIATELY as your first tool — do not investigate further on your own. Pass the original request as goal, list any tool calls you'd already made as tried, and describe what's stuck in blocked_by. Act on the consult's diagnosis."
  - One-shot per turn (`autoEscalatedTurn` flag): if the agent stalls AGAIN after the consult, the second stall is the user's signal to /stop and reframe; we don't infinite-loop the escalation.
  - Counter resets on any user-typed prompt — a new turn is a new attempt.
- 4 minutes is calibrated: past one full DeepSeek-V4-Pro reasoning cycle (~5min observed max) but well short of the 15-minute `chatWallClockBudgetReasoning` outer cap, so the escalation has plenty of room to run.
- The auto-escalation hooks into the existing spinner.TickMsg loop — no new tick infrastructure. The `lastActivity` tracker that powers the "no activity for Xm" stuck-hint also drives this guard, so the two surface in sync (the warning text appears at 2min, the auto-escalation fires at 4min).

## [0.32.1] — 2026-05-25

### Added
- **`kai_logs` tool — agents can read managed-process output on demand.** v0.32.0's managed-process model surfaced errors via background-scanner auto-followup, but the chat agent had no way to ANSWER explicit questions like "do you see the error?" — it would honestly say "I don't see one" even when an error was sitting in the buffer. The 2026-05-25 dogfood pinned the gap. New tool:
  - Returns recent stdout/stderr from the managed dev-server process kai is watching.
  - Parameters: `lines` (default 80, max 500, 0 = whatever fits in ~20KB). Total response capped at 20KB; truncates the FRONT (keeps newest) with `[... earlier output truncated ...]` marker.
  - Registers only when the TUI configures a `ManagedProcLogger` — orchestrator-spawned agents and tests get the tool silently omitted (they don't have a managed-process concept).
  - New `tools.ManagedProcLogger` interface in the tools package keeps the package cycle-free; the TUI implements it via `host_proc.go`'s `managedProcLogger` adapter that reads from `PlannerServices.ManagedProc()`'s ring buffer.
  - Threaded through `agent.Options.ManagedProcLogger` → `tools.KaiTools.ManagedProcLogger` → registration check.

### Changed
- **Managed-process output streams to scrollback (throttled).** v0.32.0 only surfaced error events; the user couldn't see the dev server starting up at all. The pre-0.32.0 capture-then-detach path streamed vite's "ready in 270ms" lines into scrollback as the user watched — that UX is back, now sustained for the process's lifetime.
  - Scanner now emits `HostProcEvent{Kind: "output"}` batches alongside the existing `error_detected` events. Each batch holds new-since-last-tick lines, capped at `managedOutputBatchMax` (8 lines per 2-second tick = ~4 lines/sec sustained — matches reading speed). Bursts past the cap render with `[... N earlier lines elided ...]`.
  - Delta computation: if the prior snapshot is a prefix of the current, only the suffix is emitted. If the ring buffer shifted (oldest evicted), fall back to the most-recent-N-lines as "new."
  - REPL handler appends each output line dimmed to scrollback with a 2-space indent.
- 7-case `kai_logs` test suite + `tailLines` covers the edge cases (no process, params, truncation, invalid JSON, n=0 unbounded).

## [0.32.0] — 2026-05-25

### Added — Managed Processes

The 12s/35s capture-then-detach approach for host commands (v0.31.45–v0.31.47) was always a stopgap: we were guessing how long to look at a process with no natural completion time. For dev-server-shape commands (`npm run dev`, `vite`, `webpack serve`, `next dev`, `electron`, `nodemon`, etc.) kai now spawns them as **managed processes** and watches them for their full lifetime.

- **New `host_proc.go`** — `ManagedProcess` type holds the spawned `*exec.Cmd`, a 64KB bounded ring buffer of recent output, a temp logfile for full history, and an error-class dedupe ring. Single slot on `PlannerServices`: a new dev-server command kills + replaces the prior. Process group setup (`Setpgid`) so concurrently-spawned children die alongside the parent on stop.
- **Background scanner goroutine** polls the ring buffer every 2 seconds, runs `detectHostCommandError` on the content, and emits `HostProcEvent{Kind: "error_detected"}` when a new error class appears. Dedupes against the 8-entry error-sig ring so a sustained error doesn't fire 30 events. Path-normalized signatures (absolute paths → basenames) so dogfood-vs-CI path differences don't generate spurious "new" errors.
- **Event channel** `HostProcEventCh` on PlannerServices. App pump (`PumpHostProcEvents`) re-arms after each delivery, same pattern as `ChatActivityCh`. Three event kinds: `started`, `error_detected`, `exited`.
- **REPL handler** renders error events as dim scrollback lines (`⚠ kai detected an error in \`...\`: ...`) and auto-dispatches a follow-up chat turn pasting the command + error if the user hasn't moved on (same user-priority guard as the v0.31.45 host-command path).
- **`/stop` slash command** kills the managed process via SIGTERM with 2s grace, then SIGKILL on the process group. No-op when nothing is running.
- **Exit cleanup**: `/exit` and ctrl+c double-quit both call `StopManagedProcess` before returning `tea.Quit` so dev servers don't orphan.
- **Routing**: y-approval handler checks `IsDevServerCommand(cmd)`; matches → managed-process path, others → legacy 12s capture path. The 35s extended-capture window from v0.31.47 is now unused for managed runs (kept for the rare fast-but-dev-shape miss case).
- 9 unit tests pin the ring buffer, sig normalization, dev-server matcher, managed-error ring, and a lifecycle end-to-end smoke (echo command → started + exited events).

### Behavioral changes vs v0.31.x

- `can you run it` → trivial-action fast path → [y] approval → kai spawns `npm run dev` as a managed process. Banner reads `↗ launched as managed process — kai is watching for errors. /stop to kill it.` No capture window, no detach — kai watches until the process exits, the user types `/stop`, or kai shuts down.
- Errors that fire 1s, 30s, 5min, or 30min after launch all flow back the same way. The "kai stopped watching at 35s" failure mode is gone for dev-server commands.
- Non-dev commands (npm install, make build, etc.) keep the 12s capture path unchanged — no behavioral change there.

## [0.31.48] — 2026-05-25

### Fixed
- **`/model` now swaps chat alongside worker.** Pre-fix `/model X` updated only `OrchestratorCfg.AgentModel` (the worker model for orchestrator-spawned agents). The chat-fallback path uses `s.ChatModel` and stayed on the startup default — so a user typing `/model claude-sonnet-4-6` to escape a slow reasoning model on chat turns saw worker switch, chat stay slow. The 2026-05-25 dogfood pinned this exact surprise. Both swapModel paths (same-provider model swap + cross-provider rebuild) now set `s.ChatModel = model` alongside `OrchestratorCfg.AgentModel = model`. Banner updated: `model → kailab / X   (worker + chat; planner stays on Y)`.
- Planner model still doesn't swap — the planner has structured-output requirements that some models (notably opus-4-6 on kai's planner prompt) don't handle reliably, and the failure mode is opaque (garbled JSON → empty-plan fallback). Planner override remains a startup-time concern via `--planner-model`. The comment in `swapModel` references `/planner-model` as a slash command but no such command exists yet — leaving that as a future addition rather than ripping the comment out, since the slash-command shape is the right direction once safety-checked.

## [0.31.47] — 2026-05-25

### Changed
- **Host-command capture window scales with command shape.** Fast commands (npm install, make install, file ops) still get the 12-second default. Dev-server-shape commands (npm run dev, npm start, yarn dev, pnpm dev, vite, webpack serve, next dev, electron, watch, nodemon, hugo server, jekyll serve, go run, python -m http) now get 35 seconds. Calibrated against the 2026-05-25 dogfood: vite reports "ready in 270ms" then takes another ~25s for the Electron renderer's first page load to fire pre-transform errors — the 12s window was missing those entirely. The wider window catches them. Window-picker uses plain string token matching against the command text (no regex). False positives are harmless (slow indicator on a quick command); false negatives miss errors (the failure mode we're fixing). Transient progress indicator now quotes the actual window: `↺ watching output for errors (~35s)…`.
- **Renamed the detach status banner.** Previous wording `↗ still running (detached) — npm run dev` read as "kai is still watching" to users. Now: `↗ launched — process keeps running in your shell, kai is no longer watching` (clean detach) or `⚠ kai detected an error during the capture window: …` + `  process keeps running in your shell — kai is no longer watching` (error detach). Removes the "still running" framing that implied continued attention.

## [0.31.46] — 2026-05-25

### Fixed
- **Critic no longer fires on trivial-action / host-command turns.** Root cause of the recurring critic false-positive "agent only proposed, didn't run" verdicts on every `can you run it`. The trivial-action proposal text ("Trivial action recognized (package.json: dev) — proposing host command without triage round-trip.") trips the critic's ≥80-char ChatReply threshold and looks like a substantive agent reply. The critic, seeing only the proposal text, judges it as "agent didn't execute" — which is wrong: the user approves with [y] and the command runs on host via the v0.31.45 capture-with-tail flow. The critic on top of that flow is pure noise. Gated: `msg.HostCommand == ""` now joins the existing `msg.Err == nil && len(ChatReply) >= 80` conditions before firing the critic.

### Changed
- **Progress indicator during the host-command capture window.** v0.31.45 holds the prompt for ~12s while capturing stdout/stderr. Previously the user saw `running on host: npm run dev` followed by 12 seconds of silence before the captured output rendered — looked like kai was hung. New transient line `↺ watching output for errors (~12s)…` shows from the moment the user approves until `HostCommandDoneMsg` arrives. The handler clears it before rendering captured output so the indicator doesn't visually layer with the result.

## [0.31.45] — 2026-05-25

### Added
- **Host commands tail-after-launch with auto-error feedback.** Previously the trivial-action fast path fired `npm run dev` (or whatever) via `exec.CombinedOutput()` and the user's terminal saw the output, but kai never did — fire-and-forget. The 2026-05-25 dogfood pinned the cost: vite printed `Unexpected block closing tag at AgentsView.svelte:25:12` in the user's terminal, kai had zero visibility, and the user had to copy/paste the error before kai could react.
  - Rewrote `runHostCommand` to use `exec.Cmd.Start()` + a waiter goroutine + a 12-second capture window. Stdout + stderr stream into a thread-safe buffer (a `safeBuf` wrapping `strings.Builder` with a mutex — bare `bytes.Buffer` races between the writer goroutine and the deadline reader).
  - **Long-running processes are detached, not killed.** When the capture window expires and the process is still running, `c.Process.Release()` hands control off — the user keeps their dev server, kai surfaces whatever it captured so far. Previously `CombinedOutput()` would SIGKILL the process at context expiry, taking the dev server down with it.
  - New `detectHostCommandError` scans the captured output for error-shaped lines. Two modes: process exited non-zero (use last non-empty line as summary), or detached/zero-exit (scan for `error` / `exception` / `failed` / `unexpected` keywords). Lines capped at 240 chars to bound the token cost downstream.
  - New `HostCommandDoneMsg.DetectedError` + `Detached` fields. The REPL handler renders four-way status: `↗ still running (detached)`, `⚠ still running (detached) — kai detected an error: <line>`, `✗ exited with error`, or `✓ done`.
  - **Auto-followup**: when a detected error is non-empty AND the user hasn't moved on (`!r.IsBusy() && r.input.Value() == ""`), the REPL auto-dispatches a chat turn pasting the command + detected error in. Agent reads the error and reacts; user doesn't have to copy/paste. Same user-priority guard as the v0.31.42 critic-retry — typed user input always wins.
  - 7-case test suite covers vite detached errors, npm ERESOLVE non-zero exits, no-error detached output, clean exits, empty output, truncation cap, Python-style exceptions.

## [0.31.44] — 2026-05-25

### Changed
- **Trivial-action fast path strips conversational openers before the verb-match.** "can you run the app?" / "could you start the dev server" / "please run it" / "would you build it" / "will you test it" / "pls run" / "please can you run it" now hit the host-command fast path. Previously the 4-word cap rejected anything with a leading nicety, and the request fell through to triage + a chat agent — which in the 2026-05-25 dogfood then fabricated a fake launch ("The Electron window should have popped up on your screen"). New `stripLeadingNicety` helper does plain string-prefix matching (no regex) against `can you`, `could you`, `would you`, `will you`, `please`, `pls`, with up to 3 chained-strip passes for "please can you …" shapes. Verb-allowlist + 4-word cap apply to the stripped form. Long requests after stripping still fail the cap intentionally (e.g. "can you run this for me please" → "run this for me please" → 5 words → no match).
- 7 new positive cases + 1 negative-after-strip case added to `TestMatchActionVerb` to pin the behavior.

## [0.31.43] — 2026-05-25

### Added
- **`kai_web_search` tool — agents can now look up facts the workspace doesn't contain.** Calls the kai-server Brave proxy at `${KailabBaseURL}/api/v1/search` with a Bearer token from `kai auth login`. Use case the user flagged: "what is the latest version of svelte?" — prior to this tool the agent had no way to answer, falling back on training-data priors that go stale. Now it can do a real lookup. Tool definition:
  - Parameters: `query` (required, plain text) + `limit` (optional, default 5, cap 10).
  - Returns top-N results as numbered title + URL + snippet, snippets capped at 240 chars each so a chatty result can't dominate the token budget.
  - 15-second HTTP timeout so a flaky upstream doesn't pin the agent's wall-clock.
  - 1 MiB body cap on the upstream response so a runaway proxy can't blow memory.
  - Accepts either `{results: [...]}` or `{web: {results: [...]}}` shapes from upstream (Brave's native shape varies).
- **Registration**: tool registers only when both `KailabBaseURL` AND `KailabToken` are configured (i.e. the user has run `kai auth login`). Anonymous / offline runs get the tool silently omitted — no broken-state UX.
- **Plumbing**: new fields `KailabBaseURL` + `KailabToken` on `orchestrator.Config`, threaded from `cmd/kai/{tui,headless}.go` where the auth creds already live. Also threaded through `agent.Options` and into `tools.KaiTools`. Chat path (`runChatAgent`) wired so chat-mode agents see the tool first — that's the primary use case.
- **Description**: tool description explicitly tells the model to use `kai_search` (FTS) / `kai_grep` for codebase content and reserves `kai_web_search` for external lookups (library versions, release dates, deprecation notices, documentation snippets, package availability).

## [0.31.42] — 2026-05-25

### Fixed
- **Critic auto-retry no longer kills the user's in-flight prompt.** Race condition pinned by the 2026-05-25 transcript: while the critic was running in the background, the user typed a follow-up prompt that started its own chat run. When the critic returned FAIL milliseconds later, the auto-retry's `r.dispatch(retryPrompt)` called `runPlan` → `armCancel`, which trips the previous run's cancel func via `prev()`. The previous run was the user's actual typed question. It died with `context.Canceled`, surfacing in scrollback as `· Cancelled` — looking like a user cancellation when in fact the auto-retry had silently murdered the user's input.
- **Fix**: gate the auto-retry on `!r.IsBusy() && r.input.Value() == ""`. When the user has already moved on — either a new run is in flight OR text is waiting in the input buffer — surface the critique but skip the dispatch. New trailer line: `skipping auto-retry — you've already moved on. The critique above stands as-is.` User input always wins.
- Counter resets in both branches (skip + cap) so the next user turn starts fresh.

## [0.31.41] — 2026-05-25

### Fixed
- **Error-class change detector now uses a bounded ring instead of a single slot that wiped on every successful bash.** v0.31.40's `lastBashErrSig` got wiped on every successful bash result — so an intervening unrelated success (e.g. a quick `node -e "console.log(require(...).version)"` version check) erased memory of the prior real error. The 2026-05-25 runlog inspection (session `e31c2173-01f3-42a4-a72b-e022cc8cbf86`) showed the bug in action: turn 1 errored, turn 3 ran a successful version check, turn 4 errored with a NEW signature — but the detector had no prior to compare against and the "new error class" note never fired. That was exactly the kai-desktop "Cannot find module" → "Unexpected token" transition we built the detector to catch.
  - New `bashErrSigRing` ring (capacity 8) replaces the single slot. Holds the last few distinct error signatures the model has seen.
  - On a fresh bash error: if the signature is NOT in the ring AND the ring is non-empty, emit a "New error class detected this turn" note with the recent classes listed for context. Then push the new sig (deduped).
  - **Successful bash does NOT clear the ring.** This was the v0.31.40 regression. An unrelated success between two failures should not erase memory of the failure class.
  - Note text rewritten to say "New error class detected" rather than "Error class CHANGED" — more precise now that the comparison is "not in recent set" rather than "differs from last."
  - 4 new ring-semantics unit cases (contains scan, push dedup, append, capacity eviction) plus the existing signature-extraction tests carry forward unchanged.

## [0.31.40] — 2026-05-25

### Added
- **Error-class change detection in the runner.** Counters the tunnel-vision pattern from the 2026-05-24 kai-desktop dogfood: an executor agent kept reasoning inside the "preprocessor config" frame even after the build error fundamentally shifted from `Cannot find module .../globalStyle` to `Unexpected token in AgentsView.svelte:182:63`. The error class CHANGED but the agent didn't notice — it kept re-running npm install, re-reading vite.config, re-checking svelte-preprocess version. New runtime guard:
  - After every bash tool result that exited non-zero, the runner extracts a normalized error signature: the first line containing `error` / `Error` / `Exception` / `failed` / a `file:line:col` pattern, with absolute paths stripped to basenames so spawn-vs-main-repo cwd differences don't generate spurious change signals.
  - The signature is tracked turn-to-turn on a `lastBashErrSig` variable scoped to the agent run.
  - When the new error's signature differs from the prior turn's, the runner mutates the bash tool result's Content to append a one-line note: `[runner] Error class CHANGED since last turn (was: "X" · now: "Y"). Your prior fix may have worked — the build is failing on a NEW problem. Re-read THIS error fresh before applying a related fix. Concretely: if the error names a file:line:col, view that file at those lines BEFORE proposing a change.`
  - Successful bash clears the tracker — a build that worked supersedes any prior failure; the next error starts fresh and won't falsely classify as a "changed" error vs. one from before the success.
  - 9 unit cases pin the signature extraction (vite errors, svelte syntax errors, Python traces, empty content, no-error output, absolute-path stripping) plus 1 change-detection invariant test (same first-error-line → same sig; different first-error-line → different sig).

## [0.31.39] — 2026-05-25

### Changed
- **File-read anti-pattern moved from bash tool description into the mode prompt.** "Reading files (use view, NOT cat/head/tail)" lived in the bash tool's *description* — easy for the model to skip past or weight lightly relative to system-prompt rules. The 2026-05-24 kai-desktop dogfood showed the cost: an executor agent that needed to inspect `AgentsView.svelte:182` reached for `sed -n '180,184p'` / `awk NR==182` / `python3 -c "open(...)"` via bash instead of `view`. The sed call likely silently failed (empty stdout + non-zero exit hidden in the wrap-around), the agent confabulated past the gap, and the real bug — Go syntax leaking into a Svelte `{...}` template expression — went undiagnosed. Promoted the guidance into `mode.go` next to the existing exploration-budget rules where it's loaded first and re-attended each turn. New text names `sed -n` / `awk NR==` / `python -c "open(...)"` explicitly as anti-patterns and explains the WHY: failures of those commands hide as empty stdout, which the model misreads as "I checked, nothing there." `view` returns content reliably or surfaces a real error. Companion rule: when a build/test error names file:line:col, the next step is ALWAYS `view <file>` against that line range.

## [0.31.38] — 2026-05-25

### Fixed
- **Verify-pass gates auto-promote — the orchestrator no longer rubber-stamps runs whose build is still failing.** Auto-promote (`outcomeAuto`) previously fired whenever the safety gate's verdict on the diff was Auto, regardless of whether the agent's verify command actually succeeded. The 2026-05-24 kai-desktop dogfood pinned the gap: a coding agent applied a config edit that left `npx vite build` failing, the diff looked benign (one-line change to `vite.config.mjs`), and the orchestrator auto-promoted before anything caught that the build still didn't run. Two changes close the door:
  - **`shouldVerify` extended to ModeCoding.** Previously verify only fired in ModeDebug. Coding agents that applied edits AND issued a bash command now trigger the same verify pass — the bash command they ran (typically a build/test/dev) IS their natural "did the fix work" check. Cost: one extra LLM call per coding run that issued bash; worth it.
  - **`runOutcome` consults the verify result before allowing auto-promote.** New `verifyGateAllowsAuto` helper: when verify ran (`VerifySummary` non-empty) and didn't return a clean `verifyPassed`, the run is forced to `outcomeHeld` regardless of the gate's diff verdict. Verify outcomes that block auto-promote: blocked, applied-more-edits, incomplete, and unclear/unknown signal. Purely additive: runs where verify didn't run (legacy modes, runs without bash) still auto-promote based on the gate verdict alone — no regression for pre-extension paths.
- 13 unit cases pin the classification matrix (auto + verify passed → auto; auto + verify {blocked, applied, incomplete, unknown} → held; auto + verify didn't run → auto). Plus 7-case `shouldVerify` test pinning the coding/debug/planning/review/conversation mode behavior.

## [0.31.37] — 2026-05-25

### Changed
- **Critic FAIL display in dim, auto-retry without prompt.** Two UX changes to the satisfaction-gate critic from v0.31.33:
  - FAIL rendered in `styleDim` (light gray) instead of `styleError` (red). The critic is a soft feedback signal, not a failure of kai. A red `× critic:` line read as "something went wrong with kai" when it was actually "kai's work didn't quite hit the ask." Same dim weight as the PASS line keeps both verdicts in the same visual register.
  - Auto-retry fires immediately on FAIL. No more `press r to retry with this critique` prompt — the retry just happens, with a dim `↻ auto-retrying (1/2) with critique appended` line so the user knows the chain is rolling. Two retries cap the chain (`criticMaxRetries=2`) so an unfulfillable request can't burn LLM calls forever. After the cap, the final critique surfaces with `retry cap (2) reached — stopping. Refine the request and try again.` Counter resets on PASS or on any user-typed prompt (the user is steering a new direction).

## [0.31.36] — 2026-05-24

### Changed
- **Idle-timeout watchdog replaces wall-clock as the primary kill switch for orchestrator agent runs.** v0.31.35 was a stopgap: it raised the wall-clock cap for reasoning models from 10min → 30min, but it didn't fix the underlying mechanism — a flat wall-clock cap kills actively-progressing agents just like it kills stuck ones. The 2026-05-24 dogfood pinned this twice: an agent wrote 7 substantial Svelte files successfully and was killed at the wall-clock boundary mid-write of the 8th.
  - New `cfg.AgentIdleTimeout` (config field `idle_timeout`, default **5 minutes**) is the primary kill. A watchdog goroutine in `runOneAgent` ticks every second; if no progress signal has fired for the full window, it cancels the run context. Progress signals: (1) a successful tool call (`OnToolCall` hook); (2) a non-empty streaming text-delta from the model (`OnAssistantDelta` hook). An agent actively writing files OR streaming visible text resets the timer indefinitely.
  - `cfg.AgentTimeout` (config field `timeout`, default raised to **30 minutes**, was 10) becomes the outer safety net. With idle-timeout active it only fires on a genuinely stuck loop that produces no deltas or tool calls for the full window — essentially impossible for a working agent. Reasoning-model 3x scaling from v0.31.35 stays in place, so a reasoning-model run gets up to 90 minutes of wall clock as a hard ceiling.
  - Reasoning models that spend minutes inside a single `<think>` phase still produce streaming text-deltas the moment the hidden phase ends, which resets the idle clock. The 5-minute default is calibrated against the 4m52s longest single turn observed in the dogfood — fits comfortably.
  - Atomic int64 progress timestamp; lock-free hot path so the per-delta overhead is one Store. Watchdog goroutine exits as soon as `runCtx.Done()` fires either way.
  - `IdleTimeoutSeconds: 0` disables the watchdog and reverts to legacy wall-clock-only behavior. Not recommended.

## [0.31.35] — 2026-05-24

### Fixed
- **Stopgap: AgentTimeout scales for reasoning models too.** v0.31.30 scaled `chatWallClockBudget` (the TUI chat path) but missed `AgentTimeout` (the orchestrator's per-sub-agent cap, 600s default). A 2026-05-24 follow-up run died at the 10-minute orchestrator cap mid-productive-work — the sub-agent had written 7 substantial Svelte files successfully and was on the 8th when killed. Same model-latency profile, different timer. Now `runOneAgent` applies the same 3x scaling when `agent.IsReasoningModel(cfg.AgentModel)` is true — reasoning sub-agents get a 30-minute outer cap to match the TUI's 15-minute chat budget. This is a stopgap; the real fix is replacing wall-clock budgets with idle-timeout (kill stuck agents, not productive ones) — tracked separately and coming next.

## [0.31.34] — 2026-05-24

### Changed
- **Critic speaks TO the user, not ABOUT them.** Initial v0.31.33 critic output read like an AI narrating to itself: "The user asked... the agent did...". The user is the one reading the output — the third-person framing made it feel like documentation about the conversation rather than a review of their work. Rewrote `criticSystemPrompt` with explicit voice rules: address the user as "you", refer to the agent as "kai" (lowercase), no third-person narration. Includes two concrete examples in the prompt — right voice vs. wrong voice — so the model has a clear pattern to match. No mechanical change; same PASS/FAIL/RETRY_HINT structure.

## [0.31.33] — 2026-05-24

### Added
- **Satisfaction-gate critic — automatic post-run task-fit check.** Every substantive chat reply (≥80 chars) now triggers a one-shot critic call after the trailer prints. The critic re-reads the original user request and the agent's reply, then emits PASS / FAIL with a specific critique. The 2026-05-24 kai-desktop dogfood pinned the need: the same model that confabulated CSS values was sharp and accurate when the user manually asked it to grade itself — `kai-the-grader` did the work `kai-the-doer` should have done before declaring done. Surface:
  - **PASS** → one dim trailer line: `✓ critic: looks good` (truncated to 100 chars). Quality was checked, no noise added.
  - **FAIL** → visible `× critic: <critique>` block, optional `hint: <retry hint>` line, then `press r to retry with this critique`. The REPL stashes a pendingCriticRetry; a single `r` keypress dispatches a new run with the prompt assembled as: original request + `[Prior attempt critique]` + critique + `[Concrete next step]` + retry hint. The model gets the diagnosis baked in instead of just "try again."
  - Transport errors on the critic call drop silently — a flaky critic shouldn't surface noise on top of the agent's already-rendered reply.
  - Malformed critic output (no `VERDICT:` line) defaults to pass — better to miss a real FAIL than flag a false one and erode user trust in the signal.
  - Skipped for short replies under 80 chars (vocab questions, yes-no, one-liners). Burning a critic call to grade "yes that's correct" is waste.
  - The critic runs as `agent.ModeConversation` with `MaxTurns:1` and a 90s wall-clock cap. No tools — pure prose grading. Uses the configured ChatModel.
- v1 covers the ChatReply path (where the response IS the deliverable). Code-execution path (ExecuteDoneMsg → orchestrator-applied edits) deferred to a follow-up that will grade against the actual diff.

## [0.31.32] — 2026-05-24

### Fixed
- **Planner requires both-sides exploration on SOURCE → TARGET requests.** Added failure mode #5 to `buildPlannerPrompt`: when the request points at a SOURCE and a TARGET ("apply X from A to B", "port the design of A onto B", "make B look like A"), the planner MUST kai_files the top level of BOTH and kai_grep for the entry point of each before emitting a plan. The 2026-05-24 kai-desktop dogfood pinned this — the user asked to apply a React reference app's design to a Svelte target, planner read only the source's theme.css, scoped a single CSS-token-copy agent, and missed that the deliverable required structural integration (component layout, routing, view shells, import wiring). The new rule names the failure shape explicitly so the model can recognize the request type and budget for it. No regex / package.json sniffing — this is a prompt-only fix that prescribes the discovery a structural port actually needs. A plan whose risk_notes only mentions the source side is now presumptively under-scoped.

## [0.31.31] — 2026-05-24

### Fixed
- **Tool calls auto-promote to scrollback when a run errors.** Quiet mode (default) hides per-tool-event lines from scrollback so an exploration phase doesn't flood the screen — but when a run fails (context deadline exceeded, classifier-flagged error, upstream timeout), the buffered tool-event trace is exactly the debug artifact the user needs. Burying it behind a /verbose toggle they could only flip BEFORE the failure wasted their time is hostile. The REPL now buffers the formatted tool-event lines (bounded ring, capped at 200 to survive runaway loops) alongside the existing count, and dumps the buffer to scrollback ahead of the friendly error message on the error branch of `PlanReadyMsg`. On a successful run the buffer is discarded and the existing "N tool calls hidden — /verbose to show next turn" trailer fires unchanged. Reset point at input dispatch matches the existing `suppressedToolEvents` reset.

## [0.31.30] — 2026-05-24

### Fixed
- **Read-cap rejection no longer offers "write/edit" as a fallback option.** 2026-05-24 kai-desktop dogfood pinned this pathology: an orchestrator-spawned agent (capped at `MaxReadsPerTurn=5`) hit the cap, got the old rejection message that listed "Issue write/edit, bash, or end the turn" as equal options, and chose write/edit — creating `kai-desktop/src/kai-theme.css` with invented CSS values it had never read from the source. The model picked the most agentic-looking option when given a menu. New messages prescribe ONE concrete next action: FINALIZE (cite reads, state intended change, end turn), BLOCKED (`"I'm blocked because <X> — I need <Y>"`), or ESCALATE (kai_consult with goal/tried/blocked_by). No menu, no "edit" as an offered option when the cap fired before reads finished.
- **Wall-clock budget scales for reasoning models.** Reasoning families (Qwen3, o1/o3/o4, gpt-5, DeepSeek-R*/V4) burn minutes per turn on hidden `<think>` tokens. The 5-minute `chatWallClockBudget` assumed turns are fast; a single DeepSeek-V4-Pro turn was observed at 4m52s for 472 visible output tokens, leaving no headroom for the next turn before `context deadline exceeded` fired and held the agent's mid-flight edits in the gate. New `chatWallClockBudgetFor(model)`: 15min for reasoning families, 5min baseline. `IsReasoningModel` exported for cross-package use; `isReasoningModel` extended to recognize `deepseek-v4`. Test reclassifies V4-Pro from non-reasoning to reasoning based on the dogfood evidence.
- **Cache-trailer warning suppressed when the provider doesn't report cache.** The "⚠ cache: only 19% reused — mostly writing fresh" warning was firing on runs where the underlying provider (DeepSeek via passthrough) doesn't emit `cache_read_input_tokens` on cold-start turns. The warning was technically accurate but misdirected the user toward looking for a prompt-mutation bug that didn't exist — the prompt prefix was stable (runlog hashes confirmed). When `create==0 && read==0`, trailer now reads `cache: not reported by provider` instead of the loud diagnostic. Real cache regressions (low pct with non-zero create) still emit the warning.

## [0.31.29] — 2026-05-24

### Fixed
- **Provider-level empty-response retry — covers every caller, not just chat agent.** v0.31.25 added an empty-response retry inside `runChatAgent` (the TUI's chat-fallback path). Real bug: that retry didn't cover the planner, gate review, kai_consult, or any other code path that calls `sendWithRecovery` directly. When a Qwen3 or other reasoning model burned its budget on a silent `<think>` block, the empty response surfaced as "Model returned no text" without any retry — exactly the symptom the user hit in the dogfood.
  - Retry now lives in `sendWithRecovery` itself. After every successful `Send` it checks `responseIsEmpty(resp)` — true iff Parts contains no text content AND no tool calls. On empty, exactly ONE retry fires with `MaxTokens` doubled. If the retry produces real content, the caller gets it transparently. If it's also empty, the original empty response surfaces unchanged so existing caller-side error handling (the TUI's "Model returned no text" prompt) still fires.
  - Every layer benefits: planner agent, chat agent, gate review, kai_consult, the triage LLM call, the file/edit reviewer — all of these go through `sendWithRecovery` and now get the empty-retry shield for free.
  - The retry is gated to one attempt per call (couldn't loop infinitely), guarded by `MaxTokens > 0` (no doubling-from-zero), and fires only on truly empty responses (whitespace-only text counts as empty; a tool call counts as non-empty).
  - `responseIsEmpty` covered by `response_empty_test.go`: nil/empty Parts, whitespace-only text, text-with-content, tool-call-only, and mixed-content cases.
  - The v0.31.25 chat-agent-layer retry stays in place as a second line of defense for the specific "agent run produced no result" semantics. Both fire on different signals.

## [0.31.28] — 2026-05-24

### Added
- **`/share <path>` slash command for read-only access to paths outside the workspace.** Solves the 2026-05-24 dogfood case where the user said "look at the designs in ~/Downloads/desktop-design" and kai's file tools refused because the path was outside the workspace. The TUI now exposes:
  ```
  /share ~/Downloads/desktop-design
  → shared (read-only, this session): /Users/jacobschatz/Downloads/desktop-design
  
  /share                       # list current session shares
  ```
  After `/share`, the agent's `view` tool resolves paths under the shared root and reads them like any in-workspace file. Persists for the session only — TUI exit clears the list.
  
  Implementation:
  - `agent.Options.SharedPaths []string` field, plumbed through to `tools.FileTools.SharedPaths`.
  - New `resolveInSetOrShared` wrapper: tries workspace resolution first, then falls back to the shared allowlist for absolute paths under any shared root. Returns `(nil project, abs, nil)` for shared hits — read-only tools accept the nil project; write/edit reject it.
  - `viewTool` calls the new resolver. Tilde-expansion (`~/...`) handled at both `/share` time and resolve time so users can pass either form.
  - The boundary stays read-only: write and edit refuse shared paths regardless. Spawn-isolation preserved.

### Limitations of this MVP
- Only `view` consults shared paths today. `kai_grep`, `kai_files`, `kai_tree` still scope to the workspace — they walk `projects.Set` directly rather than going through `resolveInSet`. Following the same pattern in those tools is the v0.31.29 follow-up.
- No auto-detection: the user must type `/share <path>` explicitly. The "mention a path in your prompt and kai asks for permission" UX from the design discussion is also v0.31.29 — it needs an in-prompt path-extractor + a new approval event flow (similar shape to `bash_confirm`).

## [0.31.27] — 2026-05-24

### Added
- **Trivial-action fast path: "run it" / "build it" / "test it" now skip the triage LLM entirely** (~3-5s saved on every match). Before triage fires, kai checks whether the prompt is a bare action verb (`run`, `start`, `build`, `test`, `launch`, `dev`, with optional `it`/`this`/`the app`/`the dev server`/etc.) and the workspace has a manifest the verb maps to. On a hit, kai synthesizes the host command locally and goes straight to the approval prompt — sub-100ms to first user-visible output, no LLM round-trip.
  - Anti-pattern guard: any prompt containing code-edit signals ("write a", "add a", "fix the", "refactor", "implement", "handler", "function", "endpoint", "test for") falls through to triage. Won't fire on "fix the build handler", "test for the run command", etc.
  - Manifest precedence: `package.json` scripts win when a script name matches the verb (`dev`/`start`/`build`/`test`); otherwise Cargo.toml → `cargo run|build|test`; go.mod → `go run|build|test ./...`; Makefile if a target matches the verb exactly.
  - Bound: prompt must be ≤4 words. Anything longer routes through triage so the LLM can disambiguate.
  - Includes "fire it up" / "kick it off" as informal run aliases.
  - Tests in `trivial_action_test.go`: 24 positive cases across package.json/Cargo.toml/go.mod, 5 negative anti-pattern cases, 5 verb-match scenarios.
  
  Net: the kai-desktop "run it" case the user hit at the 4-minute mark now resolves in ≈100ms with `npm run dev` proposed for approval. The triage path is still there for everything more complex; this just removes the LLM tax from the obvious cases.

## [0.31.26] — 2026-05-24

### Added
- **Project-aware triage hints — "run it" / "build it" / "test it" now route to a concrete command without exploration.** When the user types a generic action verb ("run it", "start it", "fire it up", "build it", "test it") with no explicit target, kai now passes one-line manifest summaries to the triage LLM so it can map the verb to the right command:
  - `package.json` with a `dev` script → `npm run dev` (the Electron / Vite case)
  - `package.json` with only `start` → `npm start`
  - `Cargo.toml` + run/test verb → `cargo run` / `cargo test`
  - `go.mod` + run/build/test verb → `go run` / `go build` / `go test`
  - `Makefile` with matching target → `make <target>`
  - `pyproject.toml` present → noted (Python)
  
  Implementation: `collectProjectHints(workspace)` reads `package.json`'s `scripts` keys, scans Makefile target names from `^name:` lines, and stat-checks the other manifests. Capped at 8 keys / targets each so the prompt stays lean. Triage `Request` grows a `ProjectHints []string` field; `buildTriageUserText` renders them as a `PROJECT HINTS:` section the LLM is taught to use in the system prompt.
  
  Bug it fixes: a dogfood run in `~/projects/kai/kai-desktop` (Electron + Vite + Svelte) where "run it" kicked off a full planner exploration — the agent ended up reading all the files and asking what the user wanted. With project hints, the same prompt routes to host-task with `npm run dev` proposed, one-keystroke approval, done.

## [0.31.25] — 2026-05-24

### Fixed
- **"Model returned no text" now auto-recovers instead of bailing.** When the chat-agent run produces an empty FinalText AND issued zero tool calls (i.e. nothing useful happened — the model spent the whole budget on a silent `<think>` trace and never got to speak), kai retries the run once with a doubled MaxTokens budget and a nudge in the prompt ("respond with visible text; do not spend your entire output budget on internal reasoning"). If the retry succeeds, the user sees the answer and a cumulative token count across both attempts. If it also fails, the original "assistant returned no text" error surfaces — same as today's behavior, no regression on truly broken responses.
  - Retry is gated on `shouldRetryEmpty`: zero ToolCall parts in the transcript means the turn produced no value, so retrying is cheap. A turn with tool calls already did real work and we don't waste tokens re-running.
  - Retry budget: 32768 on the default empty-completion path, 49152 when the first run's `finish_reason` was `max_tokens` or `length` (i.e. the model truly hit the ceiling).
  - The chat-activity channel emits an `info` event when the retry fires so the user sees recovery happening inline ("model returned no text — retrying with higher token budget").

### Added
- **Pre-emptive higher MaxTokens for known reasoning models.** New `isReasoningModel` heuristic in `runner.go` detects model names matching Qwen3, Qwen2.5, GPT-5, o1/o3/o4, DeepSeek-R1, and any "-r1"/"-r2"/"reasoning" substring. Those start at MaxTokens=32768 instead of the standard 16384 chat default — gives the silent `<think>` trace and visible output room to coexist on the first attempt, preventing most retries.
  - Substring-match heuristic, mechanical to extend (one line per new family). Test in `reasoning_model_test.go` pins 16 positive cases + 7 negatives (Claude Sonnet/Opus, DeepSeek V4 Pro, Kimi K2.6, GPT-4 family — all unaffected).

### Known issue still open
- **Auto-abort on prolonged streaming silence (no bytes for N seconds → cancel + synthesize error)** is deferred to v0.31.26. That one touches the SSE reader loops in the provider implementations — more invasive than the per-turn retry, deserves its own commit with its own care.

## [0.31.24] — 2026-05-24

### Changed
- **Tool-call lines are quiet by default — no more flooding scrollback with 15-30 `→ kai_grep …` lines per exploration.** Default behavior is now: tool dispatches update the spinner thinking line ("view kai-cli/internal/tui/views/repl.go") and increment a per-run counter, but don't pin a scrollback line. The screen stays clean during exploration phases; the user sees one live status line that updates, not a flood.
  - `/verbose` toggles per-tool-call scrollback writes ON for the rest of the session (pre-v0.31.24 behavior). `/verbose on` / `/verbose off` for explicit. `/quiet` is the inverse alias.
  - After each run, when quiet mode hid events, the trailer prints a single dim footer: `  · 12 tool calls hidden — /verbose to show next turn`. So users always know it happened.
  - Diff events, gate verdicts, bash-confirm prompts, file-confirm prompts are unaffected — those carry real content the user needs to see.
  - The harness gains `WithVerboseTools()` for tests that specifically pin tool-event rendering (the multi-root path-prefix tests added in v0.31.10).

### Known issue (worth a separate v0.31.25)
- **Empty model completions from Qwen3 reasoning models surface as "Model returned no text" and bail.** The reasoning step (silent `<think>…</think>`) sometimes consumes the entire completion budget, leaving zero output tokens. Today's error message tells the user to `/model` to a different default. Better recovery: detect the empty-completion + stop_reason="max_tokens" combo and auto-retry with a higher budget; fall back to the current message only if the second attempt also produces nothing. Deferred — that's a provider-layer change, not a TUI render fix.

## [0.31.23] — 2026-05-24

### Added
- **Time-to-first-byte surfaced in the run trailer.** Every assistant-reply trailer now reads `(<elapsed> · ↓ <tokens> · first-byte <ttfb>)`. TTFB is the wall-clock from user-submit to the moment the first provider streaming-phase event arrives — i.e. how long the spinner sat there before bytes started appearing. The number that maps to user-perceived latency, not LLM-side generation time.
  - Implementation: repl gains `turnFirstResponseAt time.Time`. The `provider_state` handler captures it on the first event whose summary contains "streaming" (excluding "stream idle"). Reset on every fresh `startRun()`. The formatRunSummary helper grew a third positional argument for the timestamp and renders the segment only when non-zero, so host-task turns (no streaming) stay clean.
  - Tests in `format_test.go` pin the new segment shape and confirm the trailer omits it for zero-timestamp turns.
  - This is intentionally diagnostic, not a perf fix — gives you a per-turn number so future optimization work (parallel triage, smaller triage model, build-check trimming) can be measured against a real metric instead of "feels slow."

## [0.31.22] — 2026-05-24

### Changed
- **Phase 1 of TUI/engine boundary: COMPLETE. `make check-tui-imports` now reports PASS.** The TUI no longer directly imports anything from `kai/internal/*` (except its own `internal/tui/*` subpackages); all 21 engine packages it used are now re-exported through `kai/api/*` sub-packages.
  - **Restructured `kai/api` into 21 sub-packages** (one per engine source package): `kai/api/provider`, `kai/api/graph`, `kai/api/projects`, `kai/api/planner`, `kai/api/agent`, `kai/api/session`, `kai/api/message`, `kai/api/safetygate`, `kai/api/util`, `kai/api/telemetry`, `kai/api/orchestrator`, `kai/api/triage`, `kai/api/memstat`, `kai/api/kaipath`, `kai/api/workspace`, `kai/api/agentprompt`, `kai/api/authorship`, `kai/api/clipboard`, `kai/api/gatereview`, `kai/api/promptenv`, `kai/api/watcher`. The flat `kai/api` package shipped in v0.31.20 / v0.31.21 had unavoidable name collisions (`Config` in provider/planner/safetygate/orchestrator, `Result` in agent/gatereview/orchestrator/triage, `Request` in provider/triage) — sub-packages preserve the original type names.
  - **Pattern**: each `api/<pkg>/<pkg>.go` aliases value types (`type X = engine.X`), re-exports constants (`const X = engine.X`), and re-exports functions as value bindings (`var Fn = engine.Fn`) so signatures stay in sync with the engine automatically.
  - **Gate flipped to strict.** `scripts/check-tui-imports.sh` now defaults to STRICT mode (`exit 1` on any direct import). Pre-merge CI can wire it as a blocking check. `--report` flag preserves the informational mode for ad-hoc audits.
  - **18 engine packages migrated in this commit** (the prior two — `agent/provider` and `graph` — were v0.31.20 / v0.31.21). 40+ TUI files updated.

### Next
- Phase 2 (no code change needed): the api package is now ready to be hoisted into a separate Go module (`kai-tui/` with its own `go.mod`) if/when separate versioning becomes valuable. The api sub-packages would move under `kai-cli/pkg/api/` or a new module path; TUI's import paths change once. Architecturally we're at the boundary that supports it.

## [0.31.21] — 2026-05-24

### Changed
- **Phase 1 cont'd: `internal/graph` migrated to `kai/api`.** Surprisingly small surface — the TUI only uses 4 symbols (`DB`, `Node`, `NodeKind`/`KindSnapshot`, `Open`) across 6 files. Re-export with type aliases (vs. interface) is the right call here: the TUI mostly threads `*DB` through to engine functions; it doesn't call many methods on it from its own logic. Designing an interface for 4-6 methods (`RemoveFile`, `IndexFile`, `GetNode`, `Close`, plus test-only `Exec` / `InsertNodeDirect`) is over-engineering today. The interface escape hatch is preserved — if/when the TUI ever needs fake-backend testing, `api/graph.go` is where the interface would land.
  - Renamed `graph.Open` → `api.OpenDB` to avoid future `api.Open` ambiguity (sessions, projects, etc.).
  - `make check-tui-imports` count: 20 → **19**. Next-up: `projects`, `planner`, `agent`, `agent/session` (5 files each).

## [0.31.20] — 2026-05-24

### Changed
- **Phase 1 of TUI/engine boundary: `agent/provider` migrated to `kai/api`.** First concrete chunk of the api extraction. The TUI now imports `kai/api` for provider types instead of `kai/internal/agent/provider`. 11 TUI files updated. New `kai-cli/api/provider.go` re-exports the symbols the TUI uses:
  - Types: `Provider`, `Request`, `Response`, `RequestState`, `RequestPhase`, `Config`, `Kind`, `CapExceededError`
  - Constants: `PhaseSent`, `PhaseConnected`, `PhaseStreaming`, `PhaseStreamIdle`, `PhaseDone`, `PhaseError`, `PhaseUpstreamSent`, `PhaseUpstreamConnected`, `PhaseUpstreamError`, `KindKailab`, `KindAnthropic`, `KindOpenAI`
  - Helpers: `NewProvider` (re-exports `provider.New`; renamed to avoid `api.New` ambiguity), `AsCapExceeded`, `IsContextOverflow`, `IsTransient`, `DailyUsage`
  
  `make check-tui-imports` baseline: 21 → **20** distinct engine packages still directly imported. Next-up by fanout: `graph` (6 files), `projects`/`planner`/`agent`/`agent/session` (5 each), `safetygate`/`agent/message` (4).

## [0.31.19] — 2026-05-24

### Added
- **TUI / engine boundary tracking infrastructure** (Phase 0 of the kai-tui extraction plan). Establishes the destination, the gate, and the documented migration plan; nothing migrates yet.
  - `kai-cli/api/` — empty public-surface package with `doc.go`. Long-term destination for everything the TUI imports from the engine. Phase 1+ will populate it incrementally.
  - `scripts/check-tui-imports.sh` — audit script that lists every engine package imported from inside `kai-cli/internal/tui/`, sorted by file count. Today's baseline: 21 distinct engine packages, 107 distinct type/func references. Top-fanout: `agent/provider` (11 files), `graph` (6), `projects` / `planner` / `agent/session` / `agent` (5 each). Currently reports-only; flips to `--strict` once migration completes.
  - `make check-tui-imports` (in `kai-cli/Makefile`) — runs the script from the standard build target context.
  - `docs/architecture/tui-api-extraction.md` — full four-phase plan with audit baseline, suggested migration order, re-export vs interface trade-off, and anti-goals (e.g. "don't restructure the engine to match the boundary").

### Why phased
The TUI imports 107 distinct symbols from 21 engine packages. Doing the whole `api/` extraction in one commit risks half-finished state (worse than untouched). Phased: each subsequent commit migrates one engine package (top-fanout first), updates the affected TUI files, and watches the gate's count drop. When the count reaches zero, the gate flips to blocking and Phase 1 is done.

## [0.31.18] — 2026-05-24

### Changed
- **Skip the LLM audit on Auto-tier gate reviews — saves 5-15s per turn.** Before: every `/gate review` of a held snapshot fired a full LLM audit, even when the deterministic gate had already classified the change as Auto (blast radius under threshold, plan-coverage 100%, no protected paths touched) and the build+tests passed green. The audit's conclusion in that case is reliably "looks good, ship it" — pure overhead.
  
  Now: when ALL of these hold, `runGateReviewItem` synthesises an APPROVE result without an LLM call —
  - snap.Payload `gateVerdict` == "Auto"
  - `gateReasons` empty (no held-reasons)
  - `vr.Ran` AND `vr.OK` (build verification ran AND passed)
  
  ANY OTHER case (Review tier, Block tier, Auto-with-reasons, missing or red build) still routes through the LLM audit. Specifically:
  - Audit fires if plan-coverage <100% even on low blast radius (catches "agent skipped a planner-named file")
  - Audit fires if blast radius is high enough to be in the Review tier
  - Audit fires if VerifyWorkspace couldn't run (no test convention detected) — "we didn't run a build" is not a green light
  
  The synthesised result includes the same fields the LLM audit would (verdict, blast radius, touches, summary, AuditClean=true, Recommendation=APPROVE) so all downstream rendering / approval flows work unchanged. Telemetry records `audit_skipped_auto=1` so we can track how often this fires in practice.

### Deferred to next release
- **B (parallel triage)**: race the triage LLM call against the chat/planner flow so its 3-5s isn't sequential before everything else. Requires extracting the chat/planner branch into its own function and racing two goroutines with cancellation. Clean change but big enough to deserve its own commit.

## [0.31.17] — 2026-05-24

### Fixed
- **Esc-cancel now actually escalates instead of spamming the same misleading message.** The 2026-05-24 dogfood pressed esc 22+ times while `kai_consult` hung "consulting the oracle… (3m51s)" — every press wrote an identical "cancelling… will return shortly" line because the cooperative-cancel path had no idempotency guard and no escape hatch.
  - **1st esc**: cooperative cancel (same as before), but the message now hints at the escalation: "press esc again to force-disconnect".
  - **2nd esc within 2s**: force-disconnect — trips cancel again (no-op if already cancelled), drops the TUI's belief that anything is running (planning/executing/gateReviewing flags cleared, spinner stopped, transient cleared), so the user gets their prompt back. The HTTP call may still leak in a background goroutine if the provider is ignoring ctx, but the TUI is usable.
  - **3rd esc within 2s**: prints the SIGTERM escape hatch — Ctrl-\\ or `kill <pid>` from another shell — for the case where the user genuinely wants the kai process gone.
  - **Subsequent presses**: silence. No more spam.
  - The escalation counter resets when a fresh run starts, so a user who cancelled a previous run with 3 presses doesn't see the SIGTERM hint on their first press next time.

Open follow-up: the deeper bug is that the cooperative cancel SHOULD have killed the in-flight call. `kai_consult` correctly passes ctx through to `provider.Send` — so the streaming HTTP transport at the provider layer is ignoring ctx.Done() between chunks. That's a separate fix (provider-side, not TUI-side).

## [0.31.16] — 2026-05-24

### Removed
- **Deleted the regex host-task classifier; routing now flows through the triage LLM.** v0.31.14 / v0.31.15 used a scored-regex table in `triage/host_task.go` to detect host-shell intent before any LLM call. That was brittle in the same way the rename gate (deleted 0.30.27) was: every phrasing not in the pattern table fell through, and growing the table only added more brittle patterns. "Set up the kai install" / "make it visible to my shell" / "get this binary onto my computer" all slipped past it.

  `runPlan` now calls `triage.Classify` directly — one cheap classification LLM call routes the request into one of five tracks (answer / quick / plan / clarify / host). The triage system prompt teaches the LLM the kai-specific recipes ("install" → `cd kai-cli && make install`, "register MCP" → `bash scripts/bootstrap-mcp.sh`, "kai login" → `kai auth login`) and tells it to populate `host_command` directly. For TrackAnswer (pure questions), the LLM's reply is surfaced inline — saves the extra chat-agent round-trip the planner-fallback path used to take. /plan forces TrackPlan inside `Classify` without any LLM call burned.

  Transport errors fall back to TrackPlan, the safe default that still goes through the planner confirm step. A broken triage doesn't break code-edit routing.

  Net: deletes `host_task.go` (~190 lines) and `host_task_test.go` (~180 lines). Adds ~40 lines of triage prompt + ~30 lines of `runPlan` rewiring. Cost is one cheap LLM classification call per non-/plan request; saves the chat-agent round-trip when triage hands back the answer directly.

## [0.31.15] — 2026-05-24

### Changed
- **Host-task fast path now RUNS the command after approval, instead of just refusing.** v0.31.14 detected install / PATH / MCP-register / login intents and showed an inline "please run this in your own terminal" explanation. That was the wrong design — kai-the-process already runs as the user, with the user's permissions, so it can execute `cd kai-cli && make install` and write to `~/go/bin/kai` exactly the way the user typing the command themselves would. Refusing was paternalism wearing safety drag.

  Triage's `Result.HostCommand` carries the literal command for the kai-specific recipes (install → `cd kai-cli && make install`, MCP register → `bash scripts/bootstrap-mcp.sh`, login → `kai auth login`). The TUI renders the explanation, then a boxed command preview, then `[y]es run on host / [n]o dismiss`. On `y`, kai runs the command via `os/exec` in its own shell context (NOT inside a CoW spawn workspace) and surfaces stdout+stderr inline plus a ✓/✗ status banner. On `n`, the user can run it themselves; the prompt clears cleanly.

  No sandbox bypass — the host-task fast path explicitly opts out of the CoW spawn machinery. The user approves each command individually; there is no "[a]llow all" affordance because host-task recipes come from triage's fixed table, not from agent emission. Each one is a deliberate one-off ask, not part of an agent loop.

  Limitations: interactive commands (e.g. `kai auth login` which expects a magic-link token on stdin) won't work cleanly with captured stdio — output renders, but the user can't type back. The dismiss path is always available so they can run interactive recipes themselves. A future revision can hand the subprocess an inherited TTY.

## [0.31.14] — 2026-05-24

### Added
- **Host-task fast path: install / PATH / sudo / MCP-register / login no longer spawn an agent.** New `TrackHost` triage track + a pre-LLM heuristic classifier (`triage.LooksLikeHostTask`) catch the obvious cases before any worker is spawned. Triage emits the routing decision with an actionable explanation (sometimes including the literal command for kai-specific cases like `cd kai-cli && make install` or `bash scripts/bootstrap-mcp.sh`), and the TUI renders it as inline assistant prose via the existing ChatReply path. No CoW workspace, no exploration, no chance of confabulation. The 2026-05-24 install-kai dogfood (30+ turns, installed a stale binary) is impossible from this single change.

  Heuristic uses a scored signal table (`host_task.go`):
  - **Strong positives** (single hit clears): `install <target>` verb, `run the install` noun, `put/add on PATH`, package-manager install commands, `sudo`, `chmod /abs`, MCP-server registration, shell rc files (`.bashrc`/`.zshrc`/`.profile`), system paths (`/etc`, `/usr/local/bin`, `/opt`), environment setup, login/auth, `kubectl apply`, "in my terminal" phrasing.
  - **Anti-patterns** (deduct strongly): test requests, requests that name code structure (function/handler/command/endpoint/method), references to code identifiers (struct/const/var/type), references to source files (`in the install script`, `in the login handler`).

  Forced `/plan` overrides the host-task gate so users can always insist on the planner. The triage LLM prompt also teaches the model about TrackHost for cases the heuristic misses. Tests in `host_task_test.go` cover 30+ real-prompt cases including the 2026-05-24 verbatim ask, code-edit anti-patterns, and the strong-anti-pattern-beats-weak-positive case.

  Closes the four-item runner-reliability arc that v0.31.13 partially shipped: planner-level routing was always the structural fix; #1–#3 contain the failure mode but #4 prevents it.

## [0.31.13] — 2026-05-24

### Fixed
- **Bash tool no longer silently drops `cd <outside-workspace> && ...`.** The pre-existing `stripLeadingCdToWorkspace` defended against absorb-side path confusion by stripping any leading `cd` to a path outside the agent's CoW workspace and running the rest of the command in the spawn dir. That was silent: the `cd` returned exit 0, no message, and the next command ran in a directory the model didn't expect. The 2026-05-24 install-kai dogfood spent ~18 turns running `cd /Users/jacobschatz/... && go build` against a workspace with no go.mod, all silently, because every retry looked superficially valid. The bash tool now refuses such commands at the entry point with a verbose error explaining the sandbox layout and what kinds of tasks (install/PATH/sudo) cannot be done from inside a spawn workspace at all. Workspace-internal absolute cd (the legitimate "enter a subdir" idiom) is unchanged. Returns `(cmd, escapeReason)` from `stripLeadingCdToWorkspace`; existing call site refuses on non-empty reason. New + updated tests in `bash_test.go`.

### Added
- **Loop detector in the agent runner.** A new `loopTracker` records each tool call's outcome (name + input + isError) and intercepts the next attempt when the last 3 calls have been identical and all errored. Intercept message is actionable — names the loop, demands a different tool/input/diagnose step, gives "stop and report" as the explicit escape hatch. Catches confusion spirals where the model retries the same broken call indefinitely without re-examining its assumptions; the install-kai dogfood would have been caught at turn 4. Tracker resets on any success or interleaving call, so legitimate retry-after-fix is unaffected. Tests in `loop_tracker_test.go`.

### Changed
- **Spawn-agent system prompt now explicitly discloses the sandbox.** A new "SANDBOX" / "HOST-ONLY TASKS" block in `agentprompt.Build` tells worker agents (a) they're running inside an isolated CoW workspace, not the user's host filesystem; (b) writes outside the workspace are rejected by the bash tool and silently ignored by file tools; (c) host-only operations (installing a binary, modifying `~/`, `sudo`, `brew install`, registering an MCP server) cannot be done from inside the workspace and they should stop and report rather than loop trying to escape. Without this, the model has no mental model of why a `cd /Users/...` is failing; with it, the model can correctly classify the task as out-of-sandbox on the first attempt.

## [0.31.12] — 2026-05-23

### Added
- **`scripts/bootstrap-mcp.sh`** — one-shot setup script that registers `kai` as an MCP server for both Claude Code (`claude mcp add kai -- kai mcp serve`) and Codex (direct write to `~/.codex/config.toml` `[mcp_servers.kai]`). Idempotent and verifying — re-runs are safe, and it confirms the registration is visible to each client. The Codex path bypasses `codex mcp add` because that subcommand isn't present on all Codex builds; the previous behavior in `kai init` silently emitted a warning when it failed, leaving users with no MCP namespace and no obvious hint why. Run once per machine; restart your Claude / Codex session afterward.

### Changed
- **`ensureAIContextFiles` now detects and replaces stale `## Code Analysis` sections** in CLAUDE.md / AGENTS.md / CODEX.md / `.cursorrules` / `.github/copilot-instructions.md` on every `kai mcp serve` startup. Previously it only noticed older sections that matched two specific hardcoded phrases — a hand-written "MANDATORY" section naming `kai_grep` and `kai_diff` (tools never exposed over MCP) slipped past the check and persisted across kai upgrades. Detection is now (a) marker-based via `<!-- kai-mcp-section: v2 -->` for forward compat and (b) signature-based via `staleSectionSignals` (mentions of `kai_grep`, `kai_diff`, or other dead phrases) for legacy CLAUDE.md files. Hand-written sections without stale signals are left alone.
- **`kaiMCPSection` body refreshed** — names only tools that the MCP server actually registers (the 18-tool set), and explicitly tells the agent that there is no MCP grep / diff / search tool so it doesn't refuse to use native grep for plain text search.

### Fixed
- The kai repo's own `CLAUDE.md` was the same kind of stale section the bug above let through — refreshed to the current 18-tool set and dropped instructions referencing `kai_grep` / `kai_diff` over MCP.

## [0.31.10] — 2026-05-21

### Added
- **End-to-end TUI tests for the v0.31.3–v0.31.9 multi-root rendering pipeline**, using the existing `tuiHarness` (teatest-backed) in `internal/tui/`. Tests boot the real model in a simulated terminal, inject `ChatActivityEvent` values mirroring what the orchestrator emits for cross-project runs, and assert that multi-root-prefixed paths + project-tagged gate reasons land in the rendered scrollback intact. Six tests in `multiroot_tui_test.go`:
  - `TestTUI_ToolEventWithProjectPrefixRenders` — `view kai-server/...` tool dispatch surfaces with the project prefix
  - `TestTUI_DiffEventWithProjectPrefixRendersPath` — diff events with multi-root paths render the prefix
  - `TestTUI_GateVerdictRendersProjectTaggedReasons` — `[kai-server] …` and `[kai] …` reason tags from v0.31.4's aggregate verdict survive the render
  - `TestTUI_CrossProjectAgentRunSequence` — realistic event sequence (tool → diff → gate) for a cross-project run; verifies every multi-root path and tag lands
  - `TestTUI_GateAutoVerdictRendersAcrossProjects` — clean aggregate verdict with two project paths
  - `TestTUI_BashConfirmAcrossProjectsRendersCorrectPath` — destructive bash gate (from v0.31.0) shows the full sibling-project path so the user can review the actual target before approving

These don't replace the per-package unit tests for the orchestrator and tools that produce these events — they prove the **rendering surface** doesn't mangle multi-root content. Run via `go test ./internal/tui/ -run TUI_`.

## [0.31.9] — 2026-05-21

### Added
- **`kai gate list` now aggregates held snapshots across every project in a multi-root workspace.** Pre-this-release, `kai gate list` only queried the cwd's DB — so a cross-project agent run that held a snapshot in kai-server didn't show up when you ran `kai gate list` from kai. Now: discovery resolves the workspace set, opens every initialized project's DB, calls `safetygate.ListHeld` per project, and prints a unified listing with a `[project-name]` tag prefixing each row.
- Single-root and uninitialized workspaces use the legacy single-DB path unchanged (no project tag, identical output to before). Open failures on one project's DB are logged to stderr and don't abort the listing for the rest.

### Tier 2 multi-root work — complete
All four Tier 2 items in `project_multi_root_todo` are now shipped:
- v0.31.7 — per-project `snap.latest` rollback
- v0.31.8 — plan-coverage path normalization + exemplar-vs-target
- v0.31.9 — `kai gate list` cross-project aggregation (this release)

Tier 3 (polish: discovery exclude-mechanism, multi-root `kai status`, cross-project `kai gate diff`) and Tier 4 (the merge-first → review-first architectural inversion) remain queued.

## [0.31.8] — 2026-05-21

### Fixed
- **`checkPlanCoverage` now reads the orchestrator's actual changed-paths slice and the planner's authoritative target file list — no more `git diff` against a single project, no more regex-extracting exemplar mentions from prompt prose.** Two structural bugs that produced false-positive Held verdicts on legitimate cross-project work:
  - **Path normalization.** Old code ran `git diff -C mainRepo HEAD` and substring-searched the diff for planner-named files. The planner uses multi-root prefixes (`kai-server/foo.go`); `git diff` against primary's repo only sees primary's paths (`kailab-control/foo.go`). 0/N false-positive holds resulted whenever the agent's actual edits were in a sibling project. Fix: compare `task.Files` (already multi-root-prefixed) directly against the orchestrator's `changed` slice (also multi-root-prefixed). No git invocation needed.
  - **Exemplar vs target conflation.** Old code extracted every file mention from the prompt prose via regex. Planner prompts often mention files as exemplars ("matches the pattern in `repos.go` and `webhooks.go`") — those got pulled into the must-edit signal list. A legitimate single-target edit would under-cover because the prose mentioned three exemplar files. Fix: only `task.Files` (the planner's authoritative target array) counts as a signal; prose stays prose.
- Defensive `pathMatchesChanged` with three matchers (exact, project-prefix-stripped, suffix) absorbs the few legitimate format variants observed in dogfood. False-positive matches in coverage are MUCH cheaper than false-negative holds (the original bug); ambiguous cases lean toward "matched."
- Symbol coverage was dropped from `checkPlanCoverage` — the legacy regex had the same exemplar-conflation problem as files. Could be reintroduced later by walking the diff per project from each project's DB (we have prevLatest/newLatest now); queued as a follow-up if signal needs return.

### Pinned tests
- `TestCheckPlanCoverage_MultiRootPathsMatch` — direct match against multi-root prefixed paths
- `TestCheckPlanCoverage_NormalizationStripsPrefix` — absorbs prefixed-vs-unprefixed mismatches both directions
- `TestCheckPlanCoverage_EmptyTaskFilesNoUnderCover` — vague plans (no named files) don't trigger under-coverage
- `TestCheckPlanCoverage_FullMiss` — the round-12 dogfood shape this guard exists to catch still fires correctly
- `TestCheckPlanCoverage_ExemplarFilesNotCounted` — exemplar mentions in prompt prose no longer contribute false signals

### Tier 2 progress
- ~~#5 Plan-coverage path normalization~~ ✅
- ~~#6 Plan-coverage exemplar vs target~~ ✅
- ~~#7 Per-project rollback~~ ✅ (v0.31.7)
- #8 `kai gate list` aggregation — open

## [0.31.7] — 2026-05-21

### Fixed
- **Per-project `snap.latest` rollback on Held aggregate verdicts.** v0.31.4 made gate verdicts per-project but left the rollback path single-root: a Held aggregate would move primary's `snap.latest` back to its prevLatest, sibling projects' pointers stayed forward. Net result: in a workspace where kai's verdict was Held but kai-server's was Auto, kai-server's `snap.latest` advanced into a state the aggregate verdict said shouldn't ship. Now: when the aggregate verdict is non-Auto, every touched project's `snap.latest` rolls back to its own prevLatest. All-or-nothing semantics matching PR-style workflows — the whole change lands or doesn't.
- Held snapshots stay in each project's DB tagged with their per-project gate metadata; `kai gate diff <held-id>` from each project still shows what the agent attempted in that project.
- Rollback failures are non-fatal per-project — logged via `absorbTrace`, the held snapshot stays discoverable, the other projects' rollbacks still proceed. Projects with no prevLatest (first-ever snapshot in that project's DB) are skipped (nothing to roll back to).

### Caveats
- **Working tree files are NOT rolled back here.** The legacy single-project behavior leaves the user's working tree dirty even on a Held verdict (consequence of the merge-first architectural pattern); we preserve that for now. The build-fix loop in `build_fix.go` still does its own `restoreWorkingTreeToSnapshot` when a terminal build failure exhausts the fix loop — that's the one path where working-tree rollback DOES happen. Aligning these is queued under Tier 4 (architectural review-then-merge).
- Surfaced as Tier 2 #7 in `project_multi_root_todo`; completes the per-project gate trifecta (capture v0.31.3, verdicts v0.31.4, rollback v0.31.7).

## [0.31.6] — 2026-05-21

### Fixed
- **`kai_search` FTS index now stays in sync as multi-root workspaces grow.** Before this release, the lazy backfill used a single `once.Do` guarded by `FileTextCount() > 0` — once any project had rows in the FTS index, every subsequent search short-circuited the backfill, even when a NEW project was added to the workspace and had zero rows. Confirmed in the 2026-05-20/21 dogfood: the kai workspace's `db.sqlite` had 69 rows under `project=kai` and zero under `project=kai-server`, even though `t.set` was multi-root; agents searching for kai-server content got "no matches" forever, no matter how many phrasings they tried.
- New per-project lazy backfill: every search call checks `CountFileTextForProject` for each project in the set. Projects with zero rows get backfilled in-place (the `IndexFile` path handles upsert; `ClearFileTextForProject` runs first so deleted files don't ghost). Projects already covered are left alone — re-walking on every search would be wasteful.
- Added `graph.DB.CountFileTextForProject(project)` (one-line `SELECT COUNT(*) FROM file_text WHERE project = ?`); added to the `Searcher` interface so the lazy-backfill path can probe without re-running a search.
- Removed the now-unused `kaiSearchToolState` + `backfillState` map machinery (legacy `sync.Once` per-DB cache); replaced with a single `backfillMu` that serializes concurrent backfills against the same Searcher.

### Pinned tests
- `TestKaiSearch_MultiRootLazyBackfillsNewProjects` — primary already indexed, secondary newly added, search for secondary content. Asserts the lazy backfill ran and produced hits.
- `TestKaiSearch_MultiRootSkipsAlreadyIndexedProjects` — both projects covered. Asserts no backfill triggered (no "indexed N files" note).

### Tier 1 multi-root work — complete
All four Tier 1 multi-root items in `project_multi_root_todo` are now shipped:
- v0.31.3 — multi-root capture
- v0.31.4 — per-project gate verdicts
- v0.31.5 — graph tools route per-project
- v0.31.6 — `kai_search` FTS multi-root lazy backfill (this release)

Tier 2 (false-positive fixes), Tier 3 (polish), and Tier 4 (the merge-first → review-first architectural inversion) are queued for follow-up releases.

## [0.31.5] — 2026-05-21

### Fixed
- **Graph tools (`kai_callers`, `kai_dependents`, `kai_context`, `kai_symbols`, `kai_impact`) now route to the right project's DB in multi-root workspaces.** Before this change, all five tools queried `Set.Primary().DB` unconditionally — an agent asking "who calls X in kai-server" got zero results because the primary's DB never indexed kai-server's symbols. Now: when the input path or target carries a project-name prefix (e.g. `kai-server/kailab-control/foo.go`), the tool looks up that project in the workspace `Set`, queries that project's own DB, and uses the project-relative path tail for the lookup. Single-root callers and prefix-less paths pass through to primary unchanged.
- New helper `routeGraphForPath(set, primary, inputPath) → (db, relPath, projectName)` in `kai.go` does the resolution; each graph tool struct now carries a `set *projects.Set` field populated by `KaiTools.All()` from `k.Set`.
- `kai_dependents` and `kai_context` re-prefix their result paths with the routed project name so the agent receives multi-root-prefixed output that round-trips cleanly into `view` / `edit` calls.
- ROUTE log entries now record the routed project (`→ db=kai-server rel=kailab-control/foo.go`) instead of the misleading `→ db=primary` that appeared regardless of where the query actually went.

### Caveats / scope notes
- `kai_callers` with no `file` argument can't disambiguate the symbol's project from input alone — stays on primary in that case. Cross-project symbol search (walking every project's DB and merging) is a known follow-up.
- `kai_symbols` with no `file` / no `path` (whole-workspace listing) stays primary-only for the same reason.
- The routing relies on `projects.Project.DB` being non-nil. `Set.Open` (called by the TUI / headless / gate entry points) populates it for initialized projects. Pinned-but-uninitialized projects fall back to primary.

## [0.31.4] — 2026-05-21

### Added
- **Per-project gate verdicts in multi-root orchestrator runs.** `integrateOneAgent` now classifies each touched project's diff against that project's own graph DB and safety-gate config (loaded by `Set.Open` at TUI startup), decorates each project's new snapshot with its own `gateVerdict / gateBlastRadius / gateReasons / gateTouches / targetSnapshot` payload, and aggregates a worst-of-N verdict for the run summary. Before this change, `safetygate.Classify` ran only against the primary's diff with the primary's gate config; secondary-project snapshots (post-v0.31.3) were untagged and invisible to `kai gate list`. Now each project's `kai gate list` shows its own held snapshots with verdicts reflecting that project's actual risk.
  - Aggregate verdict tiering: any `Block` → aggregate `Block`; any `Review` → aggregate `Review`; all `Auto` → aggregate `Auto`.
  - `captureFailed` for a sibling project (worker wrote files but `kai capture` errored) forces at least `Review` on the aggregate with a clear reason naming the project — partial work never silently auto-promotes.
  - Run-level signals (plan-coverage under-coverage, agent ExitErr) escalate every per-project verdict in addition to the aggregate, so each project's snapshot carries the same run-level concern in its `gateReasons`.
  - Reasons in the aggregate are prefixed `[<project-name>]` so the user can tell which project flagged what.
  - Touches in the aggregate are re-prefixed with the project name so cross-project touch lists are unambiguous.
- New helper file `internal/orchestrator/integrate_per_project.go` containing `projectState`, `buildProjectStates`, `classifyPerProject`, `aggregateVerdicts`, `decorateProjectSnap`, plus tests in `integrate_per_project_test.go` covering verdict aggregation tiers, reason prefixing, touch prefixing, captureFailed escalation, and skipped-state handling.

### Still primary-only after this release (queued follow-ups)
- `snap.latest` rollback on non-Auto verdict (primary's pointer moves back; sibling pointers stay forward). Per-project rollback changes user-facing semantics significantly and is a separate piece.
- Build check + build-fix loop (runs against primary's `mainRepo` only).
- Plan-coverage path normalization (multi-root prefixed paths vs project-relative diff paths cause false-positive coverage gaps).
- Graph tools `kai_callers / kai_dependents / kai_context / kai_symbols / kai_impact` still query the primary's DB regardless of which project a queried file lives in.
- `kai_search` FTS index is per-primary; secondary-project content isn't indexed.

## [0.31.3] — 2026-05-21

### Fixed
- **Multi-root capture: orchestrator now captures snapshots in every project the agent edited, not just the primary.** `integrateOneAgent`'s `absorb` step has been multi-root aware for a while (fans out file writes to every project's `mainDir`), but the `capture-main` step that follows ran only in the primary project's `mainRepo`. Net result: a cross-project agent run wrote files to a secondary project's working tree but never captured them into that project's snapshot graph — `kai log` from the secondary project showed nothing, the orchestrator's HELD-for-review verdict pointed at a snapshot that didn't exist, and `kai gate list` correctly returned nothing because no snapshot was created. Observed in a 2026-05-20 cross-project run where the worker successfully edited 4 files in kai-server, the TUI summary said "1 held", and `kai gate list` returned empty. After this fix, `capture-main` iterates the same `targets` slice that absorb used; sibling-project capture failures are non-fatal (logged but don't abort the integrate), and only the primary's capture failure short-circuits the run.

Per-project safety-gate verdicts / classify / per-project rollback are still PRIMARY-ONLY in this release. The classify+gate block continues to run against the primary project's diff. Non-primary captures land as untagged snapshots that the user can `kai gate list` / `kai log` from inside each project's directory. The orchestrator's `run.Verdict` still reflects the primary's outcome only. Per-project gating is the natural follow-up but is a larger surgery (per-project DB resolution, verdict aggregation, rollback fanout) deferred to a separate release.

## [0.31.2] — 2026-05-20

### Fixed
- **`kai_grep` auto-promotes alternation-shaped queries to regex on zero hits.** Previously, a query like `kai_grep "brave|BRAVE"` without `regex: true` did a literal-substring search for the string `"brave|BRAVE"` (pipe included), which exists nowhere — so the tool returned "no matches" and agents concluded the searched-for thing didn't exist. Now, when the literal search returns zero hits AND the query contains a regex metacharacter (`|`, `(`, `[`, `+`, `?`, `^`, `$`, `\`, `*`) AND the regex compiles, the tool transparently re-runs as regex and annotates the response: `(note: query was auto-promoted to regex — saw a regex metacharacter and the literal search returned no matches.)`. Explicit `regex: true` callers and queries without metacharacters are unchanged. Caught in a `/review` run where the agent issued five scoped greps with `|` alternations, got zero hits from each, and incorrectly concluded "no Brave API search feature exists anywhere in the codebase" — the feature was sitting in the working tree with 17 literal "brave" matches.

## [0.31.1] — 2026-05-20

### Added
- **Chat-mode exploration discipline rules.** The TUI's chat-mode system prompt now carries the same EXEMPLAR-FIRST / NEVER-view-same-file / NEVER-re-search-same-term / path-prefix-asymmetry guidance that landed in the planner and worker prompts in 0.31.0. A 200k-token budget crash mid-session was the trigger: the chat agent burned six `kai_search` calls retrying with different phrasings of the same query and viewed the same file three times before crashing. The new rules cover every chat turn (not just the first-turn overview path) and tell the model to escalate kai_search → kai_grep once, accept empty results as ground truth, and stop searching past 10 calls.
- **Anti-confabulation rule in chat-mode prompt: "Tool errors are facts, not gaps to fill."** When a view returns "file not found," a kai_grep returns zero hits, or a kai_callers returns an empty list, the model must report that honestly instead of inventing plausible-sounding content (file paths, symbol names, caller counts) to fill the gap. Caught in a review that fabricated client-side integration details for a file that had been deleted in 0.31.0.

## [0.31.0] — 2026-05-20

### Removed
- **Deterministic rename-completeness gate** (`internal/renamescan` + `gatereview.RenameResiduals`). The regex-based approach matched any Go selector expression (`b.WriteByte`, `t.Errorf`, `time.Now`) as a "renamed value" and could produce 2000+ false-positive findings on a routine refactor, blocking legitimate changes. "Did this rename complete?" is a semantic question; the audit model in the gate review now handles it. The `kai-rename-keep` annotation is no longer enforced — surviving instances are harmless comments.

### Security
- **Fail-closed defense for destructive bash commands.** A `BashTool` wired without an `Approve` hook (e.g. chat-mode TUI sessions where `agent.Hooks.OnBashConfirm` was unset) would silently execute `rm -rf <path>` because the destructive carve-out only fired on auto-allowlist matches and the empty-allowlist branch made it a no-op. Any `rm` or `git rm` now hits a fail-closed guard that hard-errors with a clear bug-report message when no approval channel is wired. Interactive approval flow for chat-mode TUI is restored in a follow-up release.

### Added
- **Build-fix loop with rollback safety net.** When `go build ./...` (or the ecosystem equivalent) fails after a worker run, up to 3 rounds of "agent reads the compile error, edits the tree, we re-check" attempt to recover automatically. If all 3 rounds exhaust, the working tree is restored to its pre-worker state via snapshot checkout, and the first 50 lines of build output are surfaced inline in the gate review. The user's working tree never carries broken edits after a failed run.
- **Per-tool routing tracer.** New `ROUTE` log entries in `planner-debug.log` and `chat-debug.log` record where every file tool, scoped grep, and graph query landed. Format: `ROUTE file in="kai-server/foo.go" → project=kai-server abs=... match=name-prefix`. Closes the diagnostic gap for multi-root routing failures. Wired via `tools.SetRoutingTracer` API and the new `Hooks.OnRoutingTrace` callback.
- **DSML tool-call leak filter.** DeepSeek-V4-Pro occasionally emits its native `<｜DSML｜tool_calls｜>...</｜DSML｜tool_calls｜>` delimiters in the content channel instead of the OpenAI `tool_calls` JSON array. A new streaming filter strips them at the provider boundary, including across SSE chunk boundaries; final accumulated text is also scrubbed so conversation history matches what the user saw.
- **Exploration-discipline rules in planner + worker prompts.** EXEMPLAR-FIRST ("view one exemplar in full + grep its registration site, then stop"), "NEVER view the same file twice," and "NEVER re-search a term you already searched" — targeting the "agent reads runner.go in four 200-line slices" failure mode.
- **SECURITY CHECKLIST in planner + EDIT-time variant in worker prompts.** Required `risk_notes` entries for new secrets, outbound network calls, user input flowing into shell/SQL/FS paths, untrusted-format parsing, and configurable base URLs.
- **Path-prefix asymmetry guidance in both prompts.** Explicit distinction between project names (valid path prefixes) and subdirectory names that LOOK like projects (e.g. `kai-cli` is a subdir inside the `kai` project, not a project prefix). Includes ✓/✗ examples drawn from a multi-root workspace.

### Fixed
- **Multi-root scoped grep accepts bare project names.** `kai_grep "X" in kai-server` now resolves to that project's root instead of erroring with "path 'kai-server' not found." Previously required a `/` in the input, sending the agent down a wrong-prefix advice loop (`in kai-server` → ERROR "try kai/kai-server" → ERROR "try kai/kai/kai-server" → …). Same fix lands for `kai_files` and `kai_tree`.
- **Error hint for unresolved paths suggests the bare project name** when the input matches a project, instead of constructing recursive nonsense like `names[0] + "/" + sub` (which produced "kai/kai-server" from the user's "kai-server").

### Earlier in the 0.31.0 cycle (2026-05-15 batch)

The following work landed in dogfood builds during the 0.30.12 – 0.30.34 internal version range and is included in this release.

### Added
- **Project overview now surfaces structural facts at turn 0.** Go module roots (with import paths), Node packages (with import-as names), Makefile/Justfile targets, and shared modules (cross-module deps) appear in the project-overview block injected on the first agent turn. Workers no longer thrash on `go build` paths or hallucinate cross-repo drift on shared deps.
- **`/chat` mode is now sticky.** Typing `/chat` keeps subsequent turns on the chat-agent path until you explicitly switch with another mode slash. Previously `/chat` was one-shot — the next message fell through to the planner.
- **Planner fast-path for trivially scoped work.** Single-file deliverables (write a doc, update a version, rename within one file) emit one agent with `EXPLORE: 0-2 turns`. Documentation and one-file edits explicitly cannot become multi-agent plans.
- **Meta-discussion questions auto-route to chat.** Long prompts ending with `?` that contain markers like "the agent", "the planner", "is there a way" now route to the chat agent instead of the planner, even when they contain action verbs as content.

### Changed
- **Reasoning-model `max_tokens` floor is now request-shape-aware.** Schema-constrained planner requests keep the 4096 floor (output bounded by JSON schema); free-form chat requests use a 16384 floor (reasoning step + prose answer both need room). Fixes "Model returned no text" on Qwen3 chat.
- **Bash tool preserves `cd /workspace/subdir`.** Previously stripped as an "escape attempt" even when the path was a legitimate workspace subdir. Worker can now `cd /spawn/.../kai-core && go test ./cas/` without the cd being silently dropped.
- **Project overview tells the agent its cwd.** Adds a `Your cwd: <workspace>` preamble so workers stop trying to cd into the user's real-fs path when running inside a CoW spawn dir.
- **Coding mode prompts the agent to reuse existing patterns.** New section directs the worker to scan for existing helpers, test frameworks, error-wrapping styles, and naming conventions before introducing siblings. Stops the "imported testify into a `t.Errorf` file" failure mode.

### Fixed
- **`session.New` and all session writes retry on SQLITE_BUSY.** When N agents spawn in parallel, the first one wins the `agent_sessions` write lock; the rest used to die instantly with "database is locked (5)." Same backoff schedule `AppendMessage` uses (~10s total resilience).
- **Stream-read failures classified as transient.** Mid-stream TLS resets, gateway hiccups, watchdog-fired timeouts on openai/kailab providers now retry via `sendWithRecovery` (5 attempts, exponential backoff). Previously the first hiccup killed the run.
- **Read-streak gate ignores contiguous `view` paging.** Sequential views on the same file at adjacent offsets count as one investigation, not N. Removes the "switch to bash python3 to read past the limit" workaround.
- **`kai push` skips locally corrupt blobs.** Pre-flight digest check verifies every PackObject's content matches its declared digest before sending; mismatches are skipped client-side with a `kai push -v` debug line naming the offender, rather than failing the whole pack at the server.
- **Multi-root absorb scoping.** Orchestrator absorb scopes to `<spawn>/<primary-basename>/` so multi-root spawns don't trip the case-collision guard.

## [0.20.1] — 2026-05-07

### Skip symlinks during capture (no more silent symlink → file conversion)
- **`internal/dirio/dirio.go:walkDir` now skips ALL symlinks** (file links and directory links). Previous behavior resolved them via `os.Stat` and captured the target's content as if it were a regular file — kai checkout would then materialize the link as a regular-file copy of the target, and absorb's filesystem diff would copy that copy back over the original symlink, permanently losing the link-ness.
- **Found in moby's `integration-cli/fixtures/https/`** during the v0.20.0 confirmation: 5 symlinks (`ca.pem`, `client-cert.pem`, `client-key.pem`, `server-cert.pem`, `server-key.pem`) silently became independent file copies after one agent run. Under previous defaults `*.pem` was excluded so the bug was masked; v0.20.0's "trust gitignore" exposed it.
- **Trade-off**: kai now pretends symlinks don't exist. A `kai checkout` to a fresh dir won't recreate them. Real symlink-ness is the user's git's job — kai snapshots everything else around them. Better than the previous silent corruption.
- New regression test in `internal/dirio/symlink_test.go`: walker yields the regular files but neither the file-link nor the directory-link.

## [0.20.0] — 2026-05-07

### Trust gitignore — drop kai's opinionated default ignore list
- **Behavior change**: kai's default ignore list now contains only the structural exclusions kai needs for its own correctness (`.git/`, `.kai/`, `.ivcs/`, `.svn/`, `.hg/`). Everything else — `vendor/`, `node_modules/`, build outputs, secrets, OS junk, editor temp — is delegated to the project's `.gitignore` (and `.kaiignore` for kai-specific overrides).
- **Why**: previous defaults silently overrode user git decisions. moby commits its `vendor/` deliberately for build reproducibility, but kai's default skipped it; absorb then treated the 9,283 vendor files as "agent-deleted" and wiped them. The class of bug repeats anywhere kai's curated list disagrees with the project's git decisions.
- **Mental model**: "if git tracks it, kai tracks it." One source of truth.
- **Security caveat**: secrets that get committed to git WILL now be captured by kai (and pushed to kaicontext on `kai push`). That's a deliberate trade-off — kai stops second-guessing which committed files are private. Keep secrets out of git in the first place; if they're already in, add a `.kaiignore` entry to exclude.
- **Migration**: most projects need no change because their `.gitignore` already covers `node_modules/` / `vendor/` / build outputs. Projects relying on kai's default to skip a path their `.gitignore` doesn't list will now track that path — fix by adding it to `.gitignore` or `.kaiignore`.
- Tests in `internal/ignore/` and `internal/orchestrator/` updated to reflect the new minimal defaults; new fixtures use a synthetic `.gitignore` to model real projects.

### Bullet-prepend regression in stream rendering
- `repl.go:924` had reverted to `r.streamBuf += "● " + ev.Display` which prepended a bullet to every stream chunk, producing output like `● Hello● world● !` when the model streamed in multiple parts. Bullet decoration is applied at render time by `bulletParagraphs`; the chunk-level prepend is a regression. Restored the bullet-free append + the test that pins it (`TestStreamingDelta_NoCopyPanic`).

## [0.19.2] — 2026-05-07

### Stop wiping working trees on absorb (moby fix)
- **`absorbSpawnIntoMain` now respects the same ignore matcher as `kai capture`** — defaults + `.gitignore` + `.kaiignore`. Previously absorb's walker had a stricter rule than capture (only excluded `.kai/`, `.git/`, `node_modules/`), so files capture intentionally skipped (`vendor/`, `*.pem`, build outputs, etc.) appeared in main but not in the spawn snapshot — and absorb treated their absence in spawn as "agent deleted these" and rm'd them from the working tree.
- Caught wiping 9,322 files (7,881 of them Go) from a moby checkout in May 2026 testing. The `.git/` itself stayed intact, but the working tree was almost entirely emptied. `git restore .` recovered everything since git's object store had the canonical copies.
- Added regression tests in `internal/orchestrator/absorb_test.go`: vendor/ files survive an absorb where the spawn doesn't have them, AND actual agent deletions still propagate (the over-correction case).
- The pre-spawn capture in v0.18.0 didn't help here — that fix solved drift between working tree and `snap.latest`. This is a different bug: divergence between the absorb walker's ignore set and capture's.

## [0.19.1] — 2026-05-07

### Build fix
- v0.19.0 shipped with a stale reference to `cfg.Agent.MaxTokens` in `cmd/kai/gate.go` that survived an earlier config refactor. The kai-cli `go build` from the root failed; the v0.19.0 binary on `main` is broken. This release removes the line. Users who already pulled v0.19.0 should `git pull` past `9a401ac`.

## [0.19.0] — 2026-05-07

### Multi-provider through kailab (no per-user API keys)
- Kailab now proxies **both** Anthropic and OpenAI models. Pick a model and the kailab provider routes to the right kailab endpoint without needing a user-side API key. Server-side `OPENAI_API_KEY` (kai-server v0.5.0+) holds the credential; per-user daily cost cap covers combined Anthropic + OpenAI usage on one cap.
- **Model-based routing** in `kai-cli/internal/agent/provider/kailab.go` — `Kailab.Send` / `SendStream` detect OpenAI-family ids (`gpt-*`, `o1*`, `o3*`, `o4*`) via `isOpenAIModel` and dispatch through the new `sendOpenAI` / `sendStreamOpenAI` helpers in `kailab_openai.go`. The OpenAI helpers reuse the existing `buildOpenAIRequest` / `parseOpenAIResponse` / `runOpenAISSE` marshaling from `openai.go` — no translation, native wire shape end-to-end.
- **No client config change needed.** Existing `KAI_PROVIDER=kailab` users just specify an OpenAI model id and routing is transparent. `/model openai gpt-4o-mini` (from v0.18.2) still works for the direct-OpenAI path; `/model kailab gpt-4o-mini` is the new through-kailab path.
- **Cost trailer note**: OpenAI models report `(no cache support)` in the trailer — they don't benefit from Anthropic's prompt caching, so per-turn cost runs ~5–10× a cached-Claude turn. The 80% daily-cap warning gives time to adjust.

## [0.18.2] — 2026-05-07

### `/model` slash command
- **`/model` in the TUI** lists providers + their default model, marking the active one. `/model <provider>` lists known model ids for that kind. `/model <provider> <model>` swaps the live provider+model for the next turn — no TUI restart needed; subsequent planner and orchestrator runs pick up the new provider via the same `OrchestratorCfg.AgentProvider` field they already read each turn.
- Same-kind swaps (e.g. `/model kailab claude-haiku-4-5-20251001` while already on kailab) update only the model id; cross-kind swaps rebuild the provider via `provider.New` and require credentials in env (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`). Switching TO kailab mid-session still requires a TUI restart since kailab credentials live in the on-disk credentials store, not env — error message points at the workaround.
- Useful for cost-routing experiments: pop into `/model anthropic claude-haiku-4-5-20251001` for a cheap turn, back to sonnet for the next.

## [0.18.1] — 2026-05-07

### Gate-review diff restored
- **`orchestrator.integrateOneAgent` now writes `targetSnapshot` on the held snap's payload** alongside `gateVerdict`. Without it, `kai gate diff <held-id>` errored with "snapshot X has no targetSnapshot in payload" — the gate-review and `gatereview.diff` paths both read this key to know what to diff against. Lost in a multi-revert during dogfood; restored with a comment explaining why it's load-bearing so it doesn't disappear again. Verified end-to-end on a held moby snap (`kai gate list / show / diff / review` all work).

## [0.18.0] — 2026-05-07

### Cache reuse fix (~3× cost reduction on real codebases)
- **`runner.go:toolInfos` sorts tools by name** before returning. Go's randomized map iteration was producing a different tool order every turn; the kailab provider lands `cache_control` on the last tool, so the marker fell on a different tool name each time and Anthropic's prefix cache lookup missed. The runlog hid the bug because `measureSections` already sorted before hashing — but the wire payload didn't. Now byte-stable.
- **Real-world impact**: moby benchmark cache reuse 17% → 58%, per-task cost $0.092 → $0.067 (-28%), executor wallclock 3m54s → 1m59s (-49%). Same fix applies to every cached-provider session.

### Headless mode + benchmarking surface
- **`kai code -p "task"`** runs one planner+execute cycle without launching the TUI. Designed for cost-validation scripts and CI regression testing — pair with `kai run summary [--all]` for a per-turn dollar / cache-reuse readout.
- **`kai run summary`** computes per-turn cost at Sonnet 4.6 list rates (input $3 / cache-write $3.75 / cache-read $0.30 / output $15 per Mtok) and prints a benchmark-friendly validation row: `| total | mean | max | reuse% | agents | tools | max-tool-bytes |`. `--all` aggregates across every session under `<KaiDir>/runs/` (needed because planner and executor write separate session dirs).

### Working-tree drift guard (eliminates "agent reverted my edits" class of bug)
- **`orchestrator.Execute` runs `kai capture` before any spawn** so `snap.latest` aligns with the working tree. `absorbSpawnIntoMain` is a pure filesystem diff between spawn and main; spawn is materialized from `snap.latest`. With uncaptured edits in main, absorb treated user files as "agent deletions" and overwrote them with the spawn's stale baseline. The v1-caveat in `absorb.go` predicted exactly this; resolved here. Cheap when there's no drift (capture is a no-op).
- **`orchestrator.Config.RunLogDir`** lets executor agents' runlog land in the main repo's kai dir instead of dying with the despawned spawn workspace. Without this, `kai run summary` only saw planner cost — half the picture.
- **`PlannerAgent.RunLogDir`** field for the same on the planner side.

### Operational fixes
- **`.git/kai/` placement honored** in `orchestrator.preflightSpawn` and `kai run`'s `resolveKaiDir` (was `.kai`-only, broke on every fresh git-repo init — moby, kubernetes, etc.).
- **Container/project invariant guards** in `projects.Discover` and `cmd/kai/{init,code,mcp}` paths refuse to launch when a directory has both `kai.projects.yaml` and `.kai/` — a misconfig that produces opaque cross-DB errors at integrate time.
- **Per-spawn `CheckpointWriter`/`SyncLogWriter`** refuse to auto-create a kai dir; stops the "kai mcp serve in container dir keeps regenerating .kai/" loop.

### Cost-reduction Phase 1
- **Default `Planner.MaxAgents` lowered from 5 to 1**. Single-agent runs are the right default for most tasks; the planner recommends more for genuinely-parallel cross-package work via the updated system prompt's "Agent count rules". Previous 5-default produced parallel agents on tasks that didn't benefit.

## [0.17.0] — 2026-05-04

### Bring Your Own Model
- **New `provider.New(cfg)` factory** — kai TUI now selects its LLM provider via `KAI_PROVIDER` env (`kailab` default, `anthropic`, `openai`). Existing kailab users see no change.
- **Anthropic-direct provider** — `KAI_PROVIDER=anthropic` + `ANTHROPIC_API_KEY` talks to api.anthropic.com directly. Same prompt-caching behavior as kailab; per-model pricing table for honest cost estimates.
- **OpenAI-compatible provider** — `KAI_PROVIDER=openai` + `OPENAI_API_KEY` (+ optional `KAI_OPENAI_BASE_URL`) works against OpenAI, Together, Groq, Ollama, vLLM, LM Studio, and any chat.completions endpoint. No prompt caching on the wire — trailer surfaces "(no cache support)" so the zero cache_read isn't read as a miss.
- **Tool-call text fallback for local models** — `KAI_OPENAI_TOOL_FORMAT=raw|hermes|llama3` extracts inline tool calls when the model emits them in `content` text instead of structured `tool_calls`. Whitelist always derived from `req.Tools`; one-shot stderr warning per process.
- **`SupportsCache()` interface** on Provider — kailab/anthropic return true, openai false. Used by the planner to scale per-turn budget down (200k → 50k tokens; 12 → 8 turns) and by the trailer to annotate honestly.

### Cost guardrails
- **Session running total in the trailer** — `· session: ~$X.XX (N turns)` accumulates across every chat + planner turn within the TUI invocation.
- **`KAI_MAX_SESSION_COST_USD` cap** — when running total exceeds the cap, the next agent run is gated on a `[y/N]` modal in the REPL (event-driven, mirrors the plan-confirmation pattern). Reset on user-confirmed continue.

### Prompt-caching fix (~75% of May-3 cache_write waste eliminated)
- **`runner.go`: graph context + convergence nudge moved out of the system prompt** into a per-turn `withPerTurnHints` block appended to the last user message. The base system prompt is now byte-stable across the entire run, so Anthropic's prompt cache actually hits on the system slice from turn 2 onward.
- Test guard `TestCacheStability_SystemPromptIsByteStable` (provider package) regression-checks future changes.
- Sister tests cover what's NOT a cache problem (post-edit re-reads, growing history) so future debugging starts from facts, not guesses.

### Auth + onboarding
- **`kai auth status` shows two sections** — LLM provider (kind, key, model, cache support) and Kailab services (sync, gates, account). Honest about BYOM users who never logged into kailab.
- **`kai init` prints the active LLM provider** when `KAI_PROVIDER` is set; warns if `ANTHROPIC_API_KEY` is missing or kailab sync is unconnected.

### Docs
- README "Bring Your Own Model" section: provider quickstarts, tradeoffs table, full env-var reference.
- `kai auth --help` and `kai init --help` updated with BYOM env vars.

## [0.16.0] — 2026-04-30

### Safety gate
- **New `safetygate` package** classifies every workspace integration into Auto / Review / Block based on depth-1 blast radius (callers + importers, the same primitive `kai impact` uses) plus per-repo protected-path globs in `.kai/gate.yaml`.
- **The gate is hooked at `Manager.Integrate` — the single chokepoint where private workspace work becomes team-visible.** Verdict is persisted on the resulting snapshot's payload and `IntegrateResult.Decision`.
- **Refuse-to-promote, never roll back.** A non-Auto verdict leaves the merged snapshot in the DB and the agent's CoW workspace untouched; only the `refMgr.Set` step on the team-visible ref is skipped. Approval re-runs the publish with `SkipGate=true`.
- **New `Manager.{PublishToRef, PublishAtTarget}`** — single helpers that wrap the ref-advance patterns previously inlined in `kai integrate` and `kai resolve`. Both consult the verdict so callers can't accidentally bypass the gate.
- **New `kai gate list / show / approve / reject`** — the human-facing surface for the hold queue.
- License switched to BSL (matches the kai-server repo).

### TUI — new `kai code` launcher
- **`kai code`** opens a Bubble Tea three-pane interface: REPL (top of input + scrollback output), Sync (live file-watcher activity), Gate (held integrations with `a`/`r` approve/reject hotkeys). Bare `kai` continues to print help.
- REPL shells out to existing kai subcommands by default; with `ANTHROPIC_API_KEY` set it routes unrecognized input to the planner instead.
- TTY detection — piped or redirected contexts get help instead of a doomed TUI.

### Planner + agent orchestrator
- **New `internal/planner`** — one-shot LLM call (Claude Sonnet) that turns a natural-language request into a structured `WorkPlan`. Resolves files via substring match against the graph, surfaces depth-1 callers and protected globs to the model, refuses vague requests with `ErrTooVague`.
- **New `internal/orchestrator`** — owns agent subprocess lifecycle. Phase A (parallel): `kai spawn` per task → write prompt file → exec configured agent command. Phase B (sequential): `kai capture` / `push` / `pull` / in-process `Manager.Integrate` per agent.
- **New `internal/agentprompt`** — pure prompt builder; identity, allowed/forbidden files (DontTouch + Protected merged), graph context, coordination notes.
- **Configurable agent command** via `.kai/config.yaml` (`agent.command: ["claude", "-p", "{prompt}"]` by default; pluggable for any runner).
- All-parallel agent execution; no DependsOn ordering in v1.

### Internals
- New `internal/config` for `.kai/config.yaml` (planner + agent runner sections).
- New `internal/tui` and `internal/tui/views` packages.
- `IntegrateOptions{Resolutions, SkipGate, GateConfig}` and `IntegrateWithOptions` entry point on `workspace.Manager`.

## [0.15.0] — 2026-04-29

### CLI — `.git/kai/` is the new default
- **`kai init` in a git repo now puts the kai data directory at `.git/kai/` instead of `.kai/`.** Two practical wins: git auto-ignores everything under `.git/`, so we no longer maintain a `.kai` entry in `.gitignore` for new repos, and `git clean -fdx` doesn't nuke kai state.
- Already-initialized projects keep their existing `.kai/` (backward compat — no migration required).
- `$KAI_DIR` overrides everything for explicit cases.
- Worktrees and submodules (where `.git` is a file, not a dir) currently fall through to `.kai/`. Resolving via `git rev-parse --git-common-dir` is a follow-up.
- New `kai/internal/kaipath` package centralizes the resolution; the previous `kaiDir` constant is gone in favour of a runtime-resolved package var.
- Bash hooks (pre-commit, pre-push, post-commit, post-merge, post-checkout) updated to short-circuit on either layout: `[ ! -d .git/kai ] && [ ! -d .kai ]`.

## [0.14.0] — 2026-04-28

### CLI — new commands
- **`kai live on / kai live off`** — CLI surface for the live-sync toggle that was previously only exposed as the `kai_live_sync` MCP tool. Writes/removes `.kai/sync-state.json` which the MCP server reads on startup. Useful for scripting and for getting `kai spawn --sync full` to take effect end-to-end.

### CLI — bug fixes
- **`kai integrate --into <ref>` now advances `<ref>`.** Previously the operation created a new merged snapshot but never moved the named target ref, so the second integrate from a parallel workspace fast-forwarded past the first one's result. As a consequence the conflict-detection branch was skipped entirely. Two parallel JS edits to the same function body would both report "Integration successful" with no conflict surfaced.
- **`kai resolve <ws> --continue` now advances the target ref** as well. Same root cause as above; the resolve path had its own copy of the ref-update gap. Skips `ws.*` auto-refs to avoid leaking into other workspaces' state.
- **`kai resolve --help` example fixed** — used to show `kai integrate myws --target snap.main`, now correctly shows `kai integrate --ws myws --into snap.main` (the actual flags).

### CLI — quality of life
- **`kai spawn list` auto-cleans stale entries** from `~/.kai/spawned.json` under an exclusive flock. The file no longer accumulates dead paths.

### Packages — public API surface
- **Lifted `internal/spawn` and `internal/synclog` to `pkg/spawn` and `pkg/synclog`.** Same code, importable by other modules (e.g. the new `kai-desktop`). `RewriteClonedWorkspace` (which depends on the graph DB internals) stays in `internal/spawnclone`.

### Desktop — new `Kai.app`
- **First-class macOS app** (`kai-desktop/`) — Wails-wrapped local dashboard. Same data model as `kai ui` (reads spawn registry + sync logs + checkpoints) but ships as a 7.7 MB `.app` with a real dock icon, traffic lights, and Cmd-Q. Builds cross-platform via `wails build` once you set up the runtime; for now Mac is the validated target.

### Cleanup
- **Removed `scripts/changelog-update.js`.** The 1medium-scheduled job hadn't run since 2026-03-06; manual changelog entries written as part of the release ritual produce better output anyway.

## [0.13.3] — 2026-04-27

### CLI
- **`kai ci rerun` now accepts run numbers** (e.g. `kai ci rerun 291`), not just internal UUIDs. `runCIRerun` was the only CI subcommand missing the `resolveRunID` lookup that `run` / `logs` / `trace` / `cancel` already had — single-line fix.
- **`docs/demo-livesync.md`** — escape angle-bracket placeholders so vitepress's vue-template parser doesn't read `<last line>` and `<n>` as unclosed HTML elements (broke kai-server's docs build, since it clones `kai/main` for its docs source).

## [0.13.2] — 2026-04-27

### CLI — `kai ui` multi-repo display
- **Per-card source repo** — each agent card now shows its source repo (e.g. `claude-1 · kaicontext/kai`) so a dashboard with agents spawned from multiple repos is legible at a glance.
- **Smart header** — replaced the cwd-derived `repo · branch` label with a registry-summary: `N agents · <repo>` (single repo), `N agents across M repos` (multiple), or `no spawned agents` (empty). The dashboard is global across the machine; the cwd was misleading when agents came from different sources.
- **`/api/header`** schema change: returns `{agent_count, repo_count, repos, sole_repo}` instead of `{repo, branch}`. `/api/agents` adds a `source_repo` field per entry.
- **Surface checkpoints in the activity sparkline + event feed** (was 0.13.1 follow-up): the dashboard reflects local AI authorship even when no peer sync events have fired, fixing the "everything stays empty" UX in single-agent or sync-off demos.

## [0.13.1] — 2026-04-26

### CLI — `kai ui`
- **`kai ui`** opens a local dashboard in your default browser. Localhost-only HTTP server (`127.0.0.1`, random free port unless `--port` is set). Shows live status of every spawned workspace (agent name, last-checkpointed file, checkpoint count, uptime, 5-minute sync-event sparkline) and a real-time strip of recent sync events across all workspaces. Single-page vanilla-JS UI embedded in the binary; no Wails, no Electron, no extra install. Exit with Ctrl+C.

## [0.13.0] — 2026-04-26

### CLI — `kai spawn` / `kai despawn` / `kai spawn list`
- **`kai spawn`** stands up N disposable, sync-connected workspaces from a snapshot. Workspace 1 is materialized via `kai checkout` from the object store; workspaces 2..N are CoW-cloned (APFS clone on macOS, reflink on btrfs/xfs, fallback to `cp -R`) from workspace 1, with their cloned graph DBs rewritten in place to give each a fresh workspace ID, name, and agent name.
- **`kai despawn`** refuses workspaces with unpushed checkpoints unless `--force`; pushes first if a remote is configured. Removes the dir, drops the registry entry, optionally runs `kai prune`.
- **`kai spawn list`** reads the spawn registry at `~/.kai/spawned.json` (each spawned dir is its own independently-init'd repo, so there's no central `.kai/` to query).
- **`kai push` writes a `.kai/last-push` marker** the despawn safety gate reads to decide whether the workspace has unpushed snapshots.
- **Workspace metadata gains an `AgentName` field.** `kai checkpoint` falls back to it when neither `--agent` flag nor `KAI_CHECKPOINT_AGENT` env is set, so agents pre-registered at spawn time get correct attribution without threading `--agent` through every hook.
- spawn/despawn/spawn list CLI (`8c38f4e`)
- 0.13.0 release (`133989f`)

## [0.12.5] — 2026-04-21

### CLI
- **MCP single-instance guard** prevents two `kai mcp serve` processes from racing for the same `.kai/`.
- **`kai org list`** and **`kai org delete`** subcommands for managing organizations on the remote server (`bca2d59`).

### Docs
- **`docs/layout-livesync.sh`** plus a TL;DR run order for the 4-agent live-sync demo (`f58b7a6`).
- 0.12.5 release (`bdadb79`)

## [0.12.4] — 2026-04-21

### CLI
- **Fix `kai clone <org>/<repo>` shorthand** — parser now correctly resolves the shorthand against the default kaicontext server (`6bac4f7`, `45348b6`).

### Docs
- **`docs/setup-livesync.sh`** — extracted setup script that doesn't kill your terminal (`6183434`, `2c5c4fa`).
- **4-agent live-sync demo script** with visible-change staging (`3ba26bd`).
- **Sync-feed command fix** — replace nonexistent `kai activity --follow` with the actual JSONL tail (`c94ce6b`).
- **90-second demo script** committed to unbreak the docs-site build (`1c0f771`).
- **`push` usage messaging reframed** from "commits" to "agent sync events" (`2e2e6d2`).

### CI
- **Removed `.kailab/workflows/ci.yml`** — GitHub Actions runs the real CI (`8559a9d`).

## [0.12.3] — 2026-04-20

### CLI — Semantic diff polish
- **Signature and value changes render as red/green pairs** in `kai diff`, matching the voiceover 30-second demo (`820348c`).
- **`docs/demo-30s.md`** adds `kai intent` as the third beat of the demo (`ff86358`).

## [0.12.2] — 2026-04-20

### CLI
- **Colorized `kai diff` output** in a TTY (`9eb3c80`, `0cf2406`).

### Docs
- **Demo script setup fix** — Scene 5 output now matches what actually runs; setup block lets Bob actually receive Alice's initial commit (`a81669f`, `b601b11`).

## [0.12.1] — 2026-04-20

### CLI — Telemetry overhaul
- **Telemetry default-on (opt-out)** with a one-time first-run notice (`d4b04e7`).
- **Events ship to PostHog** instead of a self-hosted endpoint (`a13f669`).
- **Bridge import on `git pull` / `git checkout`**, not just direct commits (`30ca8ec`).
- 0.12.1 release (`e4edf6c`)

## [0.12.0] — 2026-04-20

### CLI — kai↔git bridge end-to-end
- **`kai init --git-bridge`** installs a `post-commit` hook so git commits authored outside kai are imported as kai snapshots via `kai bridge import` (`e3665da`).
- **Milestone checkpoints become git commits** with `Kai-*` trailers via `kai bridge milestone` (`8822e46`).
- **End-to-end bridge wiring** (`12881a7`).
- **Smoke-init hook version assertions updated to v3** to keep CI green (`2126e41`).

## [0.11.7] — 2026-04-19

### CLI
- **`kai clone --kai-only`** clones from kai only, skipping git; materializes files from the latest snapshot on the remote.
- **`kai doctor --fix`** now installs missing kai-managed hooks in addition to upgrading stale ones.
- **Live-sync line-merge fallback** for `json` / `yaml` / `md` (file types where AST merge isn't available) (`4bbc329`).

## [0.11.6] — 2026-04-19

### CLI
- **`kai telemetry flush`** force-uploads spooled telemetry events, bypassing the 24-hour rate limit.
- **Fix `bufio.Scanner` aliasing bug** in the telemetry spooler (`5cbf90c`).

## [0.11.5] — 2026-04-18

### CLI
- **Fully exclude framework-generated code** from snapshots (`.svelte-kit/generated`, `.next/cache`, etc.) so Next/Sveltekit/etc. projects don't capture build artifacts (`56afa9b`).

## [0.11.4] — 2026-04-18

### CLI
- **`kai push` sends ref metadata** (git info, file counts) to the server so the kaicontext history page can render it (`b2d912b`).

## [0.11.3] — 2026-04-18

### CLI — Cross-project authorship continued
- **PostToolUse hook now writes checkpoints into foreign `.kai/` projects** when an agent edits a file outside its session's project root (`edc1686`).

## [0.11.2] — 2026-04-17

### CLI — Cross-project authorship
- **Checkpoints route to a foreign `.kai/`** when the edited file is in another kai-init'd project, so AI authorship is captured even on cross-repo edits (`9427ac0`).

## [0.11.1] — 2026-04-17

### CI
- **Smoke-test contract fix** — `kai init` now prints `Created repo:` so the smoke assertion passes (`ada9bfd`).

## [0.11.0] — 2026-04-17

### Spec — v3
- **v3 spec:** session base, trust assertions, CI evidence, quiet init (`7c5942c`). Foundation for the upcoming trust-level model (`unverified` / `agent-claimed` / `CI-verified`).

## [0.10.5] — 2026-04-16

### CLI
- **`kai purge <path-or-glob> --yes`** — escape hatch from immutability: remove a file from every snapshot in history. Supports glob patterns (`**/*.pem`, etc.). Snapshot nodes remain valid for navigation; purged file content is gone (`909bd30`).

## [0.10.4] — 2026-04-15

### CLI
- **Semantic diff reports const value changes** in addition to symbol structural changes (`db88883`).

## [0.10.3] — 2026-04-15

### CLI — `kai resolve`
- **Workspace conflict resolution flow** (`e7b4ee2`) — when `kai integrate` produces conflicts, `kai resolve <ws>` materializes them into editable `.HEAD` / `.TARGET` / `.BASE` files; `kai resolve --continue` re-runs the integration with your resolutions.
- **Fall back to working tree + marker on missing blob** in resolve (`ca0104e`).
- **Snapshot stores blob content for all file types**, not just text-recognized ones (`fc5969c`).
- **Analyze on first capture after import** so freshly-imported git history gets symbols and call edges immediately (`9d873a4`).

### CI
- **Smoke test for `kai init` against staging kai-server** (`b632a64`).
- **Smoke self-heal test** uses `kai doctor` instead of `kai --version` (`c3a9406`).
- **Release-kai-cli gated on smoke** (`a2eba48`).

## [0.10.2] — 2026-04-14

### CLI — Git hooks can no longer block git
- **Hooks are now best-effort and never block git.** The previous `pre-commit` / `pre-push` scripts ended with `kai capture` / `kai push` as the last command, so any failure (missing kai binary, deleted `.kai` directory, capture error) propagated as the hook's exit code and could block `git commit` / `git push`. The new `v2` hook scripts check for kai-on-PATH and `.kai/`, run capture/push silently, swallow any failure with `|| true`, and unconditionally `exit 0`. There is no execution path that returns nonzero.
- **Self-heal on every kai invocation.** `PersistentPreRun` now calls `selfHealHooks()` which silently rewrites any kai-managed (`# kai-managed-hook`) hook that isn't at the current `v2` version. Users with the old dangerous hook get healed the moment they run any `kai` command — no manual upgrade step required.
- **`kai hook install` always upgrades kai-managed hooks in place.** Previously bailed with "already installed". Foreign (non-kai) hooks are still left untouched, but with a warning instead of a hard error — init no longer aborts in repos with husky/lefthook setups.

### CLI — New `kai doctor` command
- **`kai doctor`** audits local Kai state: kai binary on PATH, `.kai/` present, git hooks (kai-managed vs foreign, current vs stale), kaicontext.com auth, configured remote.
- **`kai doctor --fix`** applies automatic repairs — currently upgrades any stale kai-managed git hook to the current safe version.

## [0.10.1] — 2026-04-14

### CLI — `kai init` is now one-shot and low-friction
- **Git history import is automatic** — previously gated behind a `[y/N]` prompt and a `≤1000` commit limit. Now runs unconditionally; `runGitImport` already caps at `importMaxCommits` (default 50), so large repos get their most recent 50 commits silently.
- **Git hooks install without prompting** — post-commit + pre-push hooks are set up automatically in any git repo.
- **MCP server install is auto-detected** — if Claude Code or Codex is on `PATH`, init runs `<tool> mcp list`; if `kai` isn't already registered, it's installed automatically. If it is, init says so and moves on. No prompt either way.
- **kaicontext.com signup is the default path** — the "Would you like to set that up?" gate is gone. Init proceeds straight to asking for an email, sends the magic link, exchanges the token, and signs you in.
- **Already-logged-in users skip signup entirely** — `GetValidAccessToken` is checked first; if valid, the signup copy and email prompt are suppressed.
- **Personal org + repo + first push are fully automatic** — after login, init uses the server-auto-created personal org (or derives a slug from the email local-part as a safety net), calls `DetectProjectName()` for the repo name, creates the repo, wires up `origin`, and pushes. The previous `Repository name [...]:` and `Push your semantic graph now? [Y/n]:` prompts are removed.
- **`kai bench` offer removed from init** — run `kai bench` manually anytime.

### Docs
- **README** — `Quick Start` section rewritten to describe the new one-shot init flow (graph → history → hooks → MCP → account + org + repo + push).

## [0.9.11] — 2026-03-18

### CLI
- **`kai capture -m`** — attach a message to a snapshot, shown as the CI run headline on push
- **`kai fetch --review`** — syncs review comments from the server to the local CLI
- **Push sends git commit message** — `kai push` includes the latest git HEAD message via `X-Kailab-Message` header, used as CI trigger message. Falls back to changeset intent.
- **Review fetch handles duplicates** — re-fetching an existing review syncs comments without erroring

### Reviews (kailayer.com)
- **Comments fixed** — review comments now work end-to-end (SQLite→Postgres migration: repo_id scoping, placeholder syntax, NOT NULL edge constraint)
- **Review page UX** — relative timestamps ("2h ago"), singular/plural grammar fix ("1 file changed"), Merge/Abandon buttons separated with confirmation dialogs, clearer Semantic/Lines toggle active state
- **GetObject API fix** — returns raw content with `X-Kailab-Kind` header for CLI compatibility

### CI
- **Commit messages as run headlines** — CI runs show the git commit message or `kai capture -m` message instead of generic "CI"
- **30-minute default timeouts** — job and step timeouts reduced from 6 hours to 30 minutes (overridable via `timeout-minutes` in workflow YAML)
- **Checkout reliability** — HTTP status checks, 3x retry with backoff, concurrency reduced from 20 to 10 parallel downloads
- **SSE fixes** — fixed `/events` 500 (Flusher passthrough on response wrapper), EventSource cleanup on tab navigation
- **Auto-scroll logs** — log viewer scrolls to bottom on new output

### File View (kailayer.com)
- **File search** — fuzzy filter above the tree with auto-expand on matching directories
- **Type-specific icons** — Go, Markdown, YAML/JSON, Shell files get distinct icons
- **IDE layout** — fixed-height container with independent panel scrolling (tree + content)
- **Better indentation** — 20px per nesting level
- **Loading fix** — no more flash of "No files in this snapshot" while loading

### Header (kailayer.com)
- **Logo mark** — favicon icon next to "Kai" wordmark
- **Refined spacing** — smaller wordmark (18px), consistent 24px nav gaps, `text-sm` nav items
- **Soft shadow** — `box-shadow` instead of hard 1px border
- **Desaturated avatar** — muted gray tint instead of saturated blue

### Infrastructure
- **GCS blob storage** — segments stored inline in Postgres + GCS with range reads for fast file access. Always stores inline as safety net; GCS write is best-effort.
- **Postgres upgraded** — `db-custom-1-3840` (1 vCPU, 3.75GB RAM), max connections raised to 200
- **Connection pool fix** — `SetMaxOpenConns(10)` on both data plane and control plane to prevent pool exhaustion

### Other
- **README links** — SPA navigation for internal links in rendered markdown
- **`kai push --force`** — skips negotiate for data recovery (re-sends all objects)

## [0.9.10] — 2026-03-16

### CLI
- **`kai query` command group** — query the semantic graph directly from the terminal:
  - `kai query callers <symbol>` — find all call sites with file:line locations
  - `kai query dependents <file>` — find all files that import a given file
  - `kai query impact <file>` — transitive downstream impact analysis with hop distance, separating source files from tests
- **`kai analyze` summary output** — `kai analyze symbols` and `kai analyze calls` now print what they found (e.g., "Found 61 symbols across 11 files", "Found 36 imports, 50 calls, 16 test links")

## [0.9.9] — 2026-03-14

### MCP
- **`kai_files` MCP tool** — list files in a repo with language, module, and glob pattern filters
- **MCP call logging** — JSONL logging for measuring tool usage, gated on `KAI_MCP_LOG=1`. Captures tool name, params, duration, extracted file/symbol references per session
- **SER analysis script** — `scripts/analyze-mcp-log.py` computes Structured Exploration Ratio with A/B comparison mode

### Review System
- **`kai review edit`** — update title, description, and assignees after creation
- **`kai review comment`** — add comments with `--file` and `--line` anchoring
- **`kai review comments`** — list all comments on a review
- **Review model alignment** — CLI and server now share the same data model: assignees, comment threading (parentId), changesRequestedSummary/By, targetBranch
- **Review state validation** — state machine enforcement on both CLI and server (draft→open→approved/changes_requested→merged/abandoned)
- **Review summary persistence** — `kai review summary` stores structured summary in the review payload, accessible via web UI
- **Language-aware API surface detection** — Go (uppercase), Python (no `_` prefix), Ruby (all public), Rust (uppercase types), JS/TS (top-level functions/classes)
- **Module-based file categorization** — review summaries load modules from `.kai/rules/modules.yaml` for meaningful grouping
- **Unified diff in reviews** — `kai review view` shows proper unified diffs

### Capture & Push
- **Quiet output** — one-line summary by default (`Captured abc123 (191 files, 20 modified)`), inline progress counters, full detail with `-v`
- **Snapshot history** — each capture preserves the previous snapshot as `snap.YYYYMMDDTHHMMSS.mmm`, browsable in the web UI and CLI
- **`kai snapshot list`** — now shows ref names alongside IDs

### Snapshots & Refs
- **`@snap:` ref resolution** — `@snap:snap.20260314T090755.729` and `@snap:20260314T090755.729` both work
- **`kai diff` with historical snapshots** — `kai diff snap.20260314T085932 snap.latest --semantic`

### kailayer.com
- **Web review creation** — "New Review" button on Reviews tab with changeset selector, title, and description fields
- **Raw endpoint fix** — serves `text/plain` with `nosniff` header so HTML source is displayed, not rendered
- **Skeleton loaders** — all loading states show animated skeleton placeholders matching the content shape
- **File-first loading** — file content renders immediately while the file tree loads in the background
- **Consistent page padding** — all repo pages now use matching `px-5 py-8`
- **kai-core auto-sync** — CI pulls latest kai-core from OSS repo before every build, no more drift
- **State transition validation** — server enforces same state machine as CLI

### Other
- Removed dead kailab/kailab-control build jobs from OSS CI
- MCP registry token files gitignored
- Updated README and site for MCP registry launch

## [0.9.6] — 2026-03-09

### Features
- Add `mcpName` field (`io.github.kailayerhq/kai`) for MCP registry discovery (`3b0a92a`)

### Fixes
- Skip flaky `TestRunCompletion` in CI — was timing out after 10m (`a27caca`)
- Remove kailab/kailab-control test jobs from OSS CI (server code moved to private repo) (`428837a`)

## [0.9.5] — 2026-03-08

### Features
- **MCP registry readiness** — npm package (`kai-mcp`), postinstall binary download, `server.json` schema, CI publish-on-tag pipeline (`33ac01c`)
- **Per-project remote config** — remote URLs stored per `.kai/` directory to prevent cross-repo pushes (`882ff7b`)
- **`kai_status` and `kai_refresh` MCP tools** — check graph freshness (via git, not file hashing) and re-capture from within an AI assistant (`c84c01c`)
- **Lazy MCP initialization** — semantic graph only built on first tool call, not on server startup (`cf47473`)
- **Token-efficient MCP responses** — optimized output format across all tools to reduce context window usage (`684156e`)
- **Go and Python import resolution** — dependency graph edges now resolve actual imports, not just file co-occurrence (`7c0f1b4`)
- **`kai pull`** — fetch snapshots and content from a remote Kailab server (`7cf314b`)
- **MCP server** — expose Kai's semantic graph (symbols, callers, callees, dependencies, tests, impact, diff, context) to AI coding assistants via Model Context Protocol (`e46fa1c`)

### Fixes
- Fix Go `CALLS` edges and same-package `TESTS` edge resolution in MCP callers query (`73a20e5`)

### Other
- Rewrite README with infrastructure-first framing and add install script (`d5ae0cc`)
- Update GitLab CI example and changelog script (`f1fcc88`)

## [0.4.0] — 2026-03-06

### Features
- **Open-core split** — server code (`kailab/`, `kailab-control/`, `deploy/`) moved to private `kai-server` repo. This repo is now pure OSS (Apache 2.0): `kai-core/` + `kai-cli/` + `bench/` + `docs/` (`b3fd983`)
- **Open-core architecture** — licensing, benchmarks, CI, telemetry, and regression test infrastructure (`8d38b45`)
- **Diff-first CI fast path** — skip full snapshot when coverage map exists, use native git diff (`bff10ae`, `4edf5fc`)
- **Ruby and Python change detection** — detect layer now covers Ruby and Python in addition to Go, JS/TS, and Rust (`497605a`)
- **VitePress docs site** with automated changelog pipeline (`e693fc9`)

### Other
- Contribution review policy with scope, determinism, and boundary rules (`d5aa775`)
- Move CLI reference to `docs/cli-reference.md` (`82143be`)
- Simplify README to focus on what Kai does (`f5a8fe0`)

## [0.3.0] — 2026-02-11

### Features
- **CI system** — GitHub Actions-compatible workflow engine with matrix expansion, job dependencies, schedule triggers, and reusable workflows (`4deb404`, `9c97e0f`)
- **Workflow discovery** — automatic detection of workflow files in snapshots (`9919d44`)
- **Light/dark mode** — system preference detection with manual toggle (`ad669e3`)
- **Markdown code copy** — copy button on code blocks in README rendering (`ce1f8bc`)

### Fixes
- Fix CI push notification: map `snap.latest` → `refs/heads/main` so workflows actually trigger (`4d6475f`)
- Fix matrix include-only expansion and runner job matching (`b695ba3`)
- Fix job dependency resolution: map `needs` keys to display names (`6940b0f`)
- Fix `StringOrSlice` JSON serialization to always use arrays (`9f2defa`)
- Fix job label matching and resolve matrix expressions in job names (`8d5df20`)
- Fix nil pointer in workflow sync when workflow doesn't exist in DB (`08e9cc3`)
- Fix workflow sync to decode base64 content from data plane API (`d90befb`)
- Fix workflow discovery: use file object digest and add `snap.latest` fallback (`9919d44`)
- Fix git source to capture all file types including images (`b5f31ce`)
- Fix UTF-8 encoding in file content and add raw content endpoint for images (`d2d7c09`)
- Fix code viewer horizontal overflow on long lines (`dc68d11`)
- Fix repo page showing content for non-existent repos instead of error (`151a226`)
- Fix idempotent migration for job outputs columns on PostgreSQL (`618c718`)
- Rewrite `actionCheckout` to use Kai API instead of git clone (`9078e36`)
