// Package orchestrator turns a planner.WorkPlan into running agents
// and integrated changes. It owns the agent subprocess lifecycle —
// spawn produces a workspace, but spawn does not start an agent;
// that's this package.
//
// Pipeline per agent:
//
//	1. shell out `kai spawn` to provision a CoW workspace
//	2. build the agent's prompt via internal/agentprompt
//	3. exec the configured agent command (e.g. claude -p {prompt})
//	4. wait for exit; capture stdout/stderr to <spawn>/.kai/agent.log
//	5. shell out `kai capture` to snapshot whatever the agent wrote
//	6. shell out `kai push origin` from the spawn dir
//	7. shell out `kai pull origin` in the main repo
//	8. in-process Manager.Integrate against the synced workspace
//	9. record verdict; optional `kai despawn`
//
// All v1 agents run in parallel — the planner deliberately avoids
// DependsOn for now (push/pull adds enough latency that ordering would
// feel slow, and live sync handles inter-agent visibility). Future
// phase can add ordering if real usage shows it's needed.
package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/session"
	"github.com/kaicontext/kai-engine/tools"
	"kai/internal/agentprompt"
	"github.com/kaicontext/kai-engine/authorship"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/kaipath"
	"kai/internal/planner"
	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/ref"
	"github.com/kaicontext/kai-engine/safetygate"
	"github.com/kaicontext/kai-engine/util"
	"kai/internal/workspace"
)

// AgentRun captures everything that happened to one agent: its task,
// where it ran, whether it exited cleanly, and what the gate said
// when its work was integrated.
type AgentRun struct {
	Task         planner.AgentTask
	SpawnDir     string                          // empty if spawn failed
	WorkspaceName string                         // "spawn-N", set by `kai spawn`
	ExitErr      error                           // nil = agent exited 0
	IntegrateErr error                           // nil = integrate ran (with any verdict)
	Verdict      *workspace.IntegrationDecision  // nil if integrate didn't run
	AdvancedRefs []string                        // populated when verdict == Auto
	// ChangedPaths is the set of files the agent actually modified
	// in the main repo (post-absorb). Used by the gate for blast-
	// radius classification and surfaced in the result message so
	// the user can see at a glance what landed.
	ChangedPaths []string

	// VerifyOutcome is the int form of verifyOutcome from verify.go,
	// populated by runVerifyPass when an auto-verify ran. Zero means
	// verify did not run for this agent (mode wasn't debug, no edits
	// applied, no bash issued, or the agent exited with an error).
	// Non-zero values: 1=passed, 2=blocked, 3=applied additional edits.
	VerifyOutcome int
	// VerifySummary is the one-line headline the TUI surfaces under
	// the verify block. Empty when verify didn't run.
	VerifySummary string
	// VerifyErr captures a verify-pass failure (provider error,
	// timeout) separately from the main run's ExitErr — a verify
	// crash should not invalidate an otherwise-good fix.
	VerifyErr error

	// PreexistingBuildBreak is set when the integrate-time build check
	// failed but every failing package was ALREADY failing in the
	// pre-run baseline — i.e. the change introduced no new breakage and
	// was let through. The surface uses this to tell the user their
	// tree still has pre-existing errors (not caused by this run)
	// instead of silently implying a clean build.
	PreexistingBuildBreak bool

	// TestSummary is the headline the TUI shows under the agent's
	// outcome line for the auto-test pass. Empty when no test pass
	// ran (mode wasn't coding, only test/doc files changed, or
	// no test convention was detected).
	TestSummary string
	// TestOutput is the head+tail-truncated stdout/stderr from the
	// harness's test command run. Surfaced in the run trailer so
	// the user can see actual failure messages without re-running
	// the suite themselves.
	TestOutput string
	// TestExitCode is the exit code of the harness's test command.
	// 0 = pass; non-zero = fail; -1 = command itself errored.
	TestExitCode int
	// TestErr captures a test-pass agent failure (provider error
	// during the agent run, etc.). Distinct from a "tests failed"
	// outcome — the latter is reported via TestExitCode != 0.
	TestErr error

	// TouchedPaths is the set of relpaths the agent invoked write/edit
	// against during the run, regardless of whether the final content
	// differs from main. Distinct from ChangedPaths (post-absorb diff):
	// when TouchedPaths is non-empty but ChangedPaths is empty, the
	// agent wrote and then reverted (often a cache-loop symptom).
	TouchedPaths []string
	// IntegrateNote is an advisory message surfaced after integrate
	// when something noteworthy happened that isn't an error. Used
	// to flag the "wrote then reverted" case.
	IntegrateNote string
}

// Result is the orchestrator's aggregate report. Fed back to the REPL
// so the user gets a one-line summary plus per-agent detail on demand.
type Result struct {
	Runs         []AgentRun
	AutoPromoted int
	Held         int
	Failed       int
}

// Config controls orchestrator behavior. Caller composes from
// internal/config (agent timeout + bash allowlist) plus a few
// orchestrator-specific knobs that don't fit in the user-facing
// config.
//
// As of Slice 6 the orchestrator only drives the in-process agent
// runner (`internal/agent`). The external-subprocess fields
// (AgentCommand, prompt-file plumbing, dual-path env-var dispatch)
// are gone.
type Config struct {
	// AgentTimeout is the outer wall-clock bound on a single agent
	// run. 0 means no outer cap (not recommended). With idle-timeout
	// active (AgentIdleTimeout > 0), this is now a safety net — it
	// only fires on a genuinely stuck loop that produces no deltas
	// or tool calls for the full window, which is essentially
	// impossible for a working agent. Wall-clock as a primary kill
	// switch killed productive runs (2026-05-24 dogfood: 7 files
	// written successfully, killed at 10min cap mid-write).
	AgentTimeout time.Duration

	// AgentIdleTimeout is the inactivity cap on a single agent run.
	// The watchdog cancels the run if no progress signal fires for
	// this long. Progress signals: (1) a successful tool result;
	// (2) a streaming text-delta from the model. A model deep in
	// hidden reasoning emits neither, so reasoning-model turns can
	// trip this if they're very long — but the empirical max
	// observed in dogfood is ~5min for DeepSeek-V4-Pro's longest
	// single turn, which fits within the 5-minute default.
	//
	// 0 disables idle-timeout; the run is bounded only by
	// AgentTimeout (the legacy behavior). Recommended: leave both
	// enabled — idle is the primary kill, AgentTimeout is the
	// outer safety net.
	AgentIdleTimeout time.Duration

	// ExecutorMaxTurns overrides the per-executor turn cap. 0 uses the
	// default (20), which is tuned for the interactive loop where a human
	// re-drives across messages. Headless callers (e.g. `kai autofix`) that
	// must finish a whole fix in one unattended spawn can raise it so a
	// slower model isn't cut off mid-fix.
	ExecutorMaxTurns int

	// PushRemote is the remote name agents push to. Default "origin".
	PushRemote string

	// KaiBinary overrides the path to the kai executable for shellouts
	// (spawn, capture, push, pull, despawn). Empty falls back to
	// os.Executable() — the natural choice when the orchestrator runs
	// inside the kai binary itself. Tests pass an explicit path.
	KaiBinary string

	// Despawn controls cleanup of /tmp/kai-* dirs after each agent
	// finishes. The orchestrator only despawns runs that succeeded
	// (no ExitErr, no IntegrateErr) regardless of this flag — failed
	// runs always stay so you can inspect agent.log post-mortem.
	//
	// Default true in the TUI: by the time we report the result,
	// the agent's edits are already in the user's working tree and
	// captured in kai's snap history; the spawn dir is redundant.
	// Set false if you want to keep all dirs (e.g. for offline review
	// of how an agent reached its answer).
	Despawn bool

	// SpawnPrefix sets the path prefix for spawn dirs (passed to
	// `kai spawn --prefix`). Default "/tmp/kai-".
	SpawnPrefix string

	// GateConfig is forwarded to every Manager.Integrate call.
	GateConfig safetygate.Config

	// PromptContext is the per-repo agentprompt.Context. The
	// orchestrator passes it to agentprompt.Build for each task.
	PromptContext agentprompt.Context

	// OnActivity, when set, is invoked from a per-spawn fsnotify
	// observer for every file change the agent makes. Lets the TUI
	// surface real-time agent edits in its sync pane without pulling
	// in the kai MCP. spawnName is the AgentTask.Name; relPath is
	// relative to the spawn dir; op is "created"/"modified"/"deleted".
	//
	// Callbacks fire from the observer goroutine — the receiver must
	// not block. A non-blocking channel send is the typical shape.
	OnActivity func(spawnName, relPath, op string)

	// OnFileDiff, when set, is invoked after each successful write
	// or edit by a spawned agent with a unified diff of the change
	// plus pre-counted +/- line counts. The TUI uses this to render
	// an inline "Update(file.go) +12 -3" block so the user sees
	// what the orchestrator's agents actually changed in real time
	// — not just "(tool) write" events with no content.
	//
	// Without this hook the chat agent's edits show diffs but the
	// orchestrator agent's edits don't, which feels like a
	// regression once a user clicks `go` on a plan. Same non-
	// blocking-receiver contract as OnActivity.
	OnFileDiff func(spawnName, relPath, op, unifiedDiff string, added, removed int)

	// OnAgentLifecycle, when set, fires once at the start and once
	// at the end of each spawned agent's run. event is "start" or
	// "end". Lets the TUI's status bar increment its "Agents: N"
	// live counter for orchestrator-spawned agents the same way
	// it does for the chat-fallback agent (which fires
	// agent_start / agent_end on its own activity channel).
	// Without this, the counter stays at 0 even while spawn
	// agents are clearly running — visible in the Sync line but
	// invisible in the bar.
	OnAgentLifecycle func(spawnName, event string)

	// OnAgentBashOutput, when set, fires once per line of bash
	// stdout/stderr while a spawned agent's command is running.
	// Lets the TUI mirror what the user would see if they were
	// running the command themselves — without this, the tool
	// captures the output but the user only sees the dispatch
	// indicator and never knows what `./hello_world` printed or
	// where `make` failed. The chat-fallback path already wires
	// this; the orchestrator path was missing it (May 2026 fix).
	OnAgentBashOutput func(spawnName, line string)

	// OnAgentProviderState, when set, forwards every HTTP/SSE
	// lifecycle transition of each spawned agent's underlying
	// provider call. Tagged with spawnName so the TUI can show
	// "ring-retry-on-error: streaming" instead of just "streaming"
	// when multiple agents run concurrently. Same non-blocking
	// receiver contract as OnActivity.
	OnAgentProviderState func(spawnName string, state provider.RequestState)

	// MaxAgentTokens caps token usage per agent run when the
	// in-process agent runner is enabled. 0 means "no cap" — the
	// kailab proxy may meter independently. Enforced by the runner
	// after each turn lands.
	MaxAgentTokens int

	// AgentProvider is the LLM provider the in-process runner uses.
	// nil produces a clear ExitErr from runOneAgent so users see why
	// (typically: not logged in to kailab via `kai auth login`).
	AgentProvider provider.Provider

	// AgentModel overrides the default model (deepseek-ai/DeepSeek-V4-Pro) the
	// in-process runner picks. Empty uses the default.
	AgentModel string

	// ConsultModel is the model id kai_consult invokes via
	// AgentProvider when an agent escalates a stuck exploration.
	// Empty disables kai_consult registration. Production wiring
	// (cmd/kai/tui.go) sets this to "claude-sonnet-4-6" by default;
	// a KAI_CONSULT_MODEL env var overrides at the cmd layer.
	ConsultModel string

	// KailabBaseURL + KailabToken authorize kai_web_search against
	// the kai-server Brave proxy at ${KailabBaseURL}/api/v1/search.
	// Threaded through from cmd/kai/{tui,headless}.go after
	// `kai auth login` resolves the creds. Either missing → tool
	// silently omits.
	KailabBaseURL string
	KailabToken   string

	// MainGraph is the main repo's graph DB. When non-nil, the
	// in-process runner registers kai_callers / kai_dependents /
	// kai_context tools the model can call mid-edit. nil disables
	// those tools (file ops still work).
	MainGraph *graph.DB

	// Projects is the multi-root workspace the user is operating in.
	// When non-nil and len(Projects()) > 1, the orchestrator
	// materializes ALL projects into the spawn dir (each project
	// gets a named subdirectory matching its yaml name), and the
	// agent's tool layer sees the full layout via its rewritten
	// projects.Set. This is what makes "user invoked from kai-server
	// but the bug is in kai-cli" work: the agent has read+edit
	// access to both projects in the same spawn dir.
	//
	// nil or single-project sets fall back to the legacy single-root
	// spawn flow — the existing finishRun path is unchanged for that
	// case.
	Projects *projects.Set

	// LiveSync, when set, broadcasts every agent file write to the
	// kailab live-sync channel. The TUI populates it after
	// subscribing a channel via remote.Client.SubscribeSync; nil
	// means live sync is disabled (run `kai live on` to enable).
	// Receiver must not block — it fires from the agent loop.
	LiveSync func(relPath, digest, contentBase64 string)

	// LiveSyncClient and LiveSyncChannelID enable the kai_live_sync
	// agent tool: the model can issue push/pull/status calls
	// directly against the remote sync channel. The existing
	// `LiveSync` callback covers the implicit push-on-edit path;
	// this pair covers the explicit model-driven path. Both must
	// be set for kai_live_sync to register; either one missing
	// silently omits the tool, which is what single-agent runs
	// want anyway. Wired from cmd/kai/tui.go after the same
	// SubscribeSync that produced the LiveSync callback.
	LiveSyncClient    tools.LiveSyncClient
	LiveSyncChannelID string

	// AgentBashEnabled turns on the in-process agent's bash tool.
	// AgentBashAllow optionally restricts it to a first-token
	// allowlist. Both are sourced from .kai/config.yaml's
	// agent.bash_allow.
	AgentBashEnabled bool
	AgentBashAllow   []string

	// OnAgentBashConfirm gates each non-allowlisted bash command
	// behind a user prompt. The TUI implementation blocks until
	// the user picks continue or cancel. nil disables — bash falls
	// back to allowlist-only gating, matching headless CLI usage.
	// spawnName identifies which agent is asking (for the prompt),
	// since multiple may be running in parallel.
	OnAgentBashConfirm func(spawnName, cmd, warning string) bool

	// OnAgentFileConfirm gates each write/edit behind a user prompt.
	// Same shape as OnAgentBashConfirm. op is "create" or "edit";
	// path is workspace-relative; added/removed are line-count
	// previews. nil disables — writes happen immediately.
	OnAgentFileConfirm func(spawnName, op, path string, added, removed int, diff string) bool

	// AutoTest enables the auto-test pass after the verify pass
	// when (a) the agent applied edits, (b) at least one changed
	// file is non-test, non-doc source, and (c) the workspace has
	// a detectable test convention. Default false — opt-in for now
	// so the cost is bounded; the TUI / config flag flips it on.
	AutoTest bool

	// AgentSessionStore, when set, persists each agent's
	// conversation to the kai DB (`<kaiDir>/db.sqlite`). The TUI
	// passes the main repo's graph.DB; tests pass a fake. nil
	// disables persistence — agents run with in-memory transcripts
	// only.
	AgentSessionStore session.Store

	// RunLogDir, when non-empty, redirects spawned executor agents'
	// per-turn runlog artifacts to <RunLogDir>/runs/ instead of the
	// per-spawn .kai/runs/. Without this, the executor's runlog dies
	// with the spawn dir on Despawn — making `kai run summary`
	// invisible to the (often dominant) executor cost. Headless mode
	// sets it to the main repo's kai dir so benchmarks see the full
	// cost surface.
	RunLogDir string
}

// kaiBinary returns the kai executable to use for shellouts. Order:
// explicit Config override → os.Executable() → "kai" on PATH.
func kaiBinary(cfg Config) string {
	if cfg.KaiBinary != "" {
		return cfg.KaiBinary
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "kai"
}

// Execute runs every agent in the plan, then integrates each one's
// work. Errors from individual agents do not abort the whole run —
// the Result will list each agent's outcome and the caller decides
// what to do.
//
// Parameters:
//   - ctx       — cancellation propagates to subprocesses via exec.CommandContext
//   - plan      — what to run
//   - cfg       — agent command + integration knobs
//   - db        — main repo's live DB (for in-process Integrate)
//   - mainRepo  — absolute path to the main repo (cwd of `kai pull` / Integrate)
//   - kaiDir    — main repo's kai data dir (for resolveSnapshotID-type calls)
func Execute(ctx context.Context, plan *planner.WorkPlan, cfg Config, db *graph.DB, mainRepo, kaiDir string) (*Result, error) {
	if plan == nil || len(plan.Agents) == 0 {
		return nil, fmt.Errorf("orchestrator: empty plan")
	}
	if db == nil {
		return nil, fmt.Errorf("orchestrator: nil db")
	}
	// Guard: mainRepo's expected .kai/ must point at the same on-disk
	// directory the db handle was opened from. In a multi-root
	// workspace the caller may set mainRepo = cwd (typically the
	// container dir) but pass primary.DB which corresponds to a
	// sub-project's .kai/. When they diverge, downstream operations
	// silently target different stores: `kai capture` shells out in
	// mainRepo and writes to mainRepo/.kai/db.sqlite, while
	// resolveLatestSnap reads from the db handle's underlying file.
	// The user sees "no such table: refs" or "snap.latest not found"
	// errors that are really "you're talking to two different DBs"
	// errors. Surface the mismatch up front with a fixable hint.
	if err := checkRepoDBAlignment(mainRepo, db); err != nil {
		return nil, err
	}
	if cfg.PushRemote == "" {
		cfg.PushRemote = "origin"
	}
	if cfg.SpawnPrefix == "" {
		cfg.SpawnPrefix = "/tmp/kai-"
	}

	runs := make([]AgentRun, len(plan.Agents))
	for i := range plan.Agents {
		runs[i].Task = plan.Agents[i]
	}

	// Prerequisites: cheap checks that catch the three most
	// common "preflight surprises" we've seen in the
	// errors.log — without each, the orchestrator blunders
	// into spawn and surfaces a cryptic shell-out error:
	//   1. "not in a kai repo: run `kai init` first" (no .kai/db.sqlite)
	//   2. "not found: @snap:last~0" (no snapshots in DB)
	//   3. "--sync full requires a remote" (no remote configured)
	// Catching them here gives a typed, actionable error
	// before we shell out.
	if err := precheckPrereqs(mainRepo, kaiDir, db); err != nil {
		return nil, err
	}

	// Working-tree drift guard. The absorb step is a pure filesystem
	// diff (spawn vs main); spawn itself is materialized from
	// snap.latest. So if the user has uncaptured edits in main when
	// the agent runs, those edits don't exist in spawn — and absorb
	// will treat them as "agent deleted these" or "agent rewrote
	// these" and overwrite the user's work with the stale snap
	// content. See the v1-caveat comment in absorb.go.
	//
	// The deferred fix from that comment ("snapshot main before
	// absorb so the user can recover") lands here: we just run
	// `kai capture` ourselves before spawning. Cheap when there's
	// no drift (capture is a no-op), and eliminates the entire
	// class of clobber-by-revert when there is.
	//
	// Best-effort: if the capture fails (no remote, gc mid-flight,
	// etc.) we surface a warning but proceed — preflightSpawn below
	// will catch the genuinely broken cases. Suppressing capture
	// failures here keeps the orchestrator runnable in tests and on
	// fresh repos without a remote.
	// Bound the pre-spawn capture with a timeout so a stalled
	// `kai capture` (e.g. SQLite writer-lock contention with the
	// parent TUI's open DB connection) can't wedge the orchestrator
	// indefinitely. 2026-05-13 dogfood: a single pre-spawn capture
	// span at 400-700% CPU for tens of minutes with the TUI alive,
	// blocking every plan dispatch. The fast-path (no-op capture)
	// finishes in ~1s, the analyze-heavy path in ~2s — 60s is well
	// above either with margin. If we hit the timeout, preflightSpawn
	// below catches genuine corruption cases; the most likely cause
	// of a slow capture (concurrency with the TUI) doesn't break
	// the spawn workspace, so proceeding-with-warning is correct.
	captureCtx, captureCancel := context.WithTimeout(ctx, 60*time.Second)
	// KAI_CAPTURE_SKIP_SUMMARY=1: the pre-spawn capture has no human
	// reader of the "X files, Y modified" summary, and computing it
	// runs classify.DetectChanges (tree-sitter parse + AST diff) on
	// every modified source file. On a working tree with edits to
	// big files (the 22k-line main.go is the dogfood example),
	// this single phase took 5+ minutes and was the dominant cost
	// of every pre-spawn capture. Skipping it leaves snap.latest,
	// the object store, and the analyze edges correct — only the
	// human-facing summary line is empty, which the orchestrator
	// doesn't display anyway.
	captureErr := runInWithEnv(captureCtx, mainRepo,
		[]string{"KAI_CAPTURE_SKIP_SUMMARY=1"},
		kaiBinary(cfg), "capture", "-m",
		"orchestrator: pre-spawn safety capture")
	captureCancel()
	if captureErr != nil {
		if captureCtx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "warning: pre-spawn capture exceeded 60s timeout (likely DB lock contention with TUI); proceeding\n")
		} else {
			fmt.Fprintf(os.Stderr, "warning: pre-spawn capture failed (%v); proceeding\n", captureErr)
		}
	}

	// Workspace build precondition. If the main repo doesn't compile
	// before agents fan out, every spawned worker inherits the broken
	// state and the run is doomed before it starts. The
	// add-config-show-command failure pinned this: cmd/kai/version_test.go
	// was committed without its matching impl, so every retest
	// workspace booted into a broken build state. Catching it here
	// gives a single clear error pointing at the diagnostic; without
	// this the failure surfaces N times in N spawned agents.
	//
	// Build-check baseline. We used to HARD-REFUSE to spawn whenever the
	// workspace didn't compile. That punished users for breakage they
	// didn't cause and that had nothing to do with their task — a stale
	// _test.go in some unrelated package would wall off the entire run
	// (2026-05-29: a `gate list --json` planning session died on a
	// pre-existing vet error in internal/agent/tools). Instead we
	// capture the baseline now and let the integrate gate block only on
	// failures the agent NEWLY introduces (see newFailures). A
	// pre-existing break is a warning, not a wall. KAI_SKIP_BUILD_CHECK
	// still short-circuits the whole thing (runBuildCheck returns
	// Ran=false), so an intentional broken-tree session is unaffected.
	baseline := runBuildCheck(ctx, mainRepo)

	// Preflight: do a single dry-run spawn to a throwaway dir before
	// fanning out N parallel spawns. If the object store is missing
	// blobs (a `kai gc` cleared something the snapshot still
	// references, or the repo was partially copied), all N spawns
	// would otherwise fail with the same opaque "no such file"
	// error. Catching it once here gives the user a single clear
	// "run kai capture to rebuild" hint instead of N identical
	// failures.
	if err := preflightSpawn(ctx, cfg, mainRepo); err != nil {
		return nil, err
	}

	// Phase A — spawn + run agents in parallel. Each agent owns its
	// slot in `runs` so we don't need a mutex for individual fields.
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runOneAgent(ctx, &runs[i], cfg, mainRepo)
		}(i)
	}
	wg.Wait()

	// Phase B — push, pull, integrate sequentially. Doing this in
	// parallel risks racing on the main repo's DB and on
	// snap.latest advancement. Sequential is simple and predictable;
	// optimize later if it's a bottleneck.
	for i := range runs {
		integrateOneAgent(ctx, &runs[i], cfg, db, mainRepo, baseline)
	}

	// Atomic integrate across the plan: a plan's agents are
	// supposed to land or hold as a unit. The failing dogfood that
	// motivated this gate left version_test.go committed without
	// its versionShort/versionJSON impl — exactly the "test agent
	// landed, impl agent didn't" half-merge pattern. Now: if ANY
	// agent in this plan run failed (ExitErr, IntegrateErr) or got
	// held, demote every Auto-promoted sibling to Review with a
	// reason naming the failing peer. The held snaps stay in the
	// DB so the user can review the whole plan's diff together
	// instead of discovering a half-landed change days later.
	demoteAutoPromotedSiblings(runs)

	// Cleanup. Despawn only successful runs — keep failed spawn dirs
	// around so the user can read agent.log and figure out what went
	// wrong. The opt-in flag toggles whether successful runs get
	// cleaned at all.
	if cfg.Despawn {
		for _, r := range runs {
			if r.SpawnDir == "" {
				continue
			}
			if r.ExitErr != nil || r.IntegrateErr != nil {
				continue // keep failures for diagnosis
			}
			// Kill any processes whose command line references
			// the spawn dir BEFORE removing the dir itself. The
			// 2026-05-25 dogfood pinned this: the agent's verify
			// pass ran `npm run dev &` then `kill $PID` — but
			// concurrently had already forked vite + electron +
			// wait-on as children. Killing the npm parent
			// orphaned the children to init/launchd, and they
			// kept running indefinitely. Result: every prior
			// orchestrator plan that touched a dev server left
			// a process zoo behind, plus port collisions on the
			// next run, plus user looking at stale Electron
			// windows from old spawn dirs.
			killProcessesUnder(r.SpawnDir)
			c := exec.CommandContext(ctx, kaiBinary(cfg), "despawn", r.SpawnDir, "--force")
			c.Dir = mainRepo
			_ = c.Run() // best-effort
		}
	}

	res := &Result{Runs: runs}
	for i := range runs {
		switch runOutcome(&runs[i]) {
		case outcomeFailed:
			res.Failed++
		case outcomeHeld:
			res.Held++
		case outcomeAuto:
			res.AutoPromoted++
		case outcomeNone:
			// No counter — render path labels it "no changes".
		}
	}
	return res, nil
}

// killProcessesUnder finds and kills any processes whose command
// line references the given spawn dir path. Best-effort cleanup
// for orphaned children left behind by backgrounded `cmd &`
// invocations inside the spawn — the parent npm/sh/wait-on dies
// when the agent's bash call returns but the children (vite,
// electron, etc.) get re-parented to init and keep running.
//
// Uses pgrep -f (matches by command line). Available on macOS,
// Linux, and most BSDs. Falls back to a no-op when pgrep isn't
// on PATH — better to leak processes than to block the despawn
// path on a portability issue.
//
// SIGTERM first with a 2-second grace period, then SIGKILL on
// anything still alive. Same shape as the v0.32.0 StopManagedProcess
// shutdown; reused mechanism, different trigger.
func killProcessesUnder(spawnDir string) {
	if strings.TrimSpace(spawnDir) == "" {
		return
	}
	// Resolve any symlinks (macOS often resolves /tmp -> /private/tmp;
	// processes started inside the spawn may show the resolved form
	// in their command line even when the dir was given as /tmp/*).
	// Use both forms in the pgrep -f search.
	resolved := spawnDir
	if rp, err := filepath.EvalSymlinks(spawnDir); err == nil {
		resolved = rp
	}
	pidSet := map[int]struct{}{}
	for _, dir := range distinct(spawnDir, resolved) {
		out, err := exec.Command("pgrep", "-f", dir).Output()
		if err != nil {
			// Exit status 1 = no matches, not an error. Other
			// statuses (pgrep missing, permission denied) we
			// silently ignore — leaking processes is preferable
			// to blocking despawn.
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || pid <= 0 {
				continue
			}
			pidSet[pid] = struct{}{}
		}
	}
	if len(pidSet) == 0 {
		return
	}
	// Don't kill ourselves. The orchestrator's own pid may match
	// the spawn dir string in some narrow cases (e.g. cwd
	// happens to contain a similar path); defensive guard.
	selfPid := os.Getpid()
	delete(pidSet, selfPid)

	// SIGTERM first.
	for pid := range pidSet {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	// 2-second grace, then SIGKILL any survivors.
	time.Sleep(2 * time.Second)
	for pid := range pidSet {
		// syscall.Kill(pid, 0) probes liveness; ESRCH = dead.
		if err := syscall.Kill(pid, 0); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// distinct returns the unique non-empty strings in the input.
// Tiny helper so killProcessesUnder doesn't fire two pgrep calls
// when symlink resolution returns the same path.
func distinct(ss ...string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range ss {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

type runOutcomeKind int

const (
	outcomeNone   runOutcomeKind = iota // ran clean, changed nothing observable
	outcomeFailed                       // errored with nothing to rescue, or integrate failed
	outcomeHeld                         // produced a change the gate held for review
	outcomeAuto                         // produced a change the gate auto-promoted
)

// runOutcome classifies a finished AgentRun for the result tally.
//
// The ExitErr-with-a-Verdict case is the load-bearing one: an agent
// can error (context deadline exceeded, budget) AFTER writing real
// edits, which the integrate phase now rescues and holds for review.
// That run is Held, NOT Failed — the user-facing outcome is "there is
// a change to review," and integrateOneAgent has forced it non-Auto.
// Only an error with no rescuable verdict is a hard failure.
func runOutcome(r *AgentRun) runOutcomeKind {
	switch {
	case r.ExitErr != nil && r.Verdict == nil:
		return outcomeFailed
	case r.IntegrateErr != nil:
		return outcomeFailed
	case r.Verdict == nil && len(r.ChangedPaths) == 0:
		return outcomeNone
	case r.Verdict == nil:
		// Edits landed on disk but integrate produced no verdict
		// (legacy path) — surface as Held so the user inspects.
		return outcomeHeld
	case r.Verdict.Verdict == string(safetygate.Auto) && !verifyGateAllowsAuto(r):
		// 2026-05-25: even when the safety gate's diff verdict is
		// Auto (low-risk file changes, no protected-path violations),
		// the run is held if the verify pass didn't return a clean
		// VERIFIED. The kai-desktop dogfood pinned this exact gap —
		// agent applied a "fix" that left the build failing, the diff
		// looked benign (a vite.config.mjs edit), and the orchestrator
		// auto-promoted before any verify could catch that the build
		// still didn't run. Verify-gating closes that door.
		return outcomeHeld
	case r.Verdict.Verdict == string(safetygate.Auto):
		return outcomeAuto
	default:
		return outcomeHeld
	}
}

// verifyGateAllowsAuto reports whether the run's verify-pass result
// permits auto-promotion. The rule: auto-promote ONLY when verify
// either didn't run at all OR verify ran and returned a clean pass.
// Any other outcome — applied additional edits, incomplete,
// blocked, or ran-without-clear-signal — is held for user review.
//
// "Did verify run?" is determined by VerifySummary being non-empty:
// runVerifyPass always sets it before returning, regardless of
// outcome. Distinguishes verify-didn't-run (legacy + modes that
// bail out of shouldVerify) from verify-ran-but-unclear (a real
// signal we shouldn't suppress).
//
// Pre-existing runs where verify never ran still auto-promote
// based on the gate verdict alone — no regression. The change is
// purely additive: when verify DOES run, its result matters.
func verifyGateAllowsAuto(r *AgentRun) bool {
	// Verify didn't run for this agent — fall through to the
	// gate-verdict-only path (legacy behavior).
	if strings.TrimSpace(r.VerifySummary) == "" {
		return true
	}
	// Verify ran. The int values track the private verifyOutcome
	// enum in verify.go: 1 = verifyPassed. Anything else
	// (unknown / blocked / applied / incomplete) means the loop
	// hadn't cleanly closed.
	return r.VerifyOutcome == 1
}

// logCIPlanPreview runs `kit ci plan --json` in the spawn dir and
// emits a one-line log of what tests it would run, for side-by-side
// comparison with the LLM verify that's about to fire. Phase 1 of
// wiring kit ci plan into verify (see runVerifyPass): observability
// only, no behavior change. Once the plan output is empirically
// trusted, Phase 2 swaps verify to deterministic test execution
// driven by the plan.
//
// Best-effort: any error (binary missing, plan failed, non-JSON output)
// is logged and the function returns; verify continues unchanged.
func logCIPlanPreview(ctx context.Context, spawnDir, taskName string, cfg Config) {
	if spawnDir == "" {
		return
	}
	// Look up the kit binary from the current process. Falls back to
	// "kit" on PATH if os.Executable fails (test envs, unusual installs).
	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin = "kit"
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, bin, "ci", "plan").Output()
	if err != nil {
		log.Printf("[ci-plan-preview] task=%s err=%v (skipped)", taskName, err)
		return
	}
	var plan struct {
		Mode       string  `json:"mode"`
		Risk       string  `json:"risk"`
		Confidence float64 `json:"confidence"`
		Targets    struct {
			Run      []string `json:"run"`
			Skip     []string `json:"skip"`
			Fallback bool     `json:"fallback"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(out, &plan); err != nil {
		log.Printf("[ci-plan-preview] task=%s parse-err=%v (skipped)", taskName, err)
		return
	}
	// Log the counts, not the raw slice — dumping the full
	// plan.Targets.Run ([]string of every affected test file) bloated
	// the diagnostic and, before the TUI redirected the logger, leaked
	// onto the screen.
	log.Printf("[ci-plan-preview] task=%s mode=%s risk=%s confidence=%.2f fallback=%v run=%d skip=%d",
		taskName, plan.Mode, plan.Risk, plan.Confidence, plan.Targets.Fallback,
		len(plan.Targets.Run), len(plan.Targets.Skip))
}

// runOneAgent: spawn a CoW workspace, then dispatch the in-process
// agent runner against it. Errors land on run.ExitErr; SpawnDir is
// empty if spawn itself failed.
//
// As of Slice 6 this only runs the in-process path. The external-
// subprocess (Claude Code, Cursor, etc.) flow is gone — kai owns the
// full agent loop. Spawn dirs remain because they still provide CoW
// isolation between parallel agents.
func runOneAgent(ctx context.Context, run *AgentRun, cfg Config, mainRepo string) {
	if cfg.AgentProvider == nil {
		run.ExitErr = fmt.Errorf("agent %s: AgentProvider is nil — run `kai auth login` and re-launch `kai code`", run.Task.Name)
		return
	}

	// Multi-root awareness: when the workspace has multiple
	// projects, materialize every project into the spawn dir so
	// the agent can read+edit files across projects. The single-
	// root spawnFor path handles the common case (one project) and
	// older flows where cfg.Projects isn't threaded yet.
	var (
		dir      string
		wsName   string
		spawnSet *projects.Set
	)
	if cfg.Projects != nil && len(cfg.Projects.Projects()) > 1 {
		var spawnErr error
		dir, wsName, spawnSet, spawnErr = spawnForMulti(ctx, run.Task.Name, cfg, cfg.Projects)
		if spawnErr != nil {
			run.ExitErr = fmt.Errorf("spawn (multi-root): %w", spawnErr)
			return
		}
	} else {
		var spawnErr error
		dir, wsName, spawnErr = spawnFor(ctx, run.Task.Name, cfg, mainRepo)
		if spawnErr != nil {
			run.ExitErr = fmt.Errorf("spawn: %w", spawnErr)
			return
		}
	}
	run.SpawnDir = dir
	run.WorkspaceName = wsName

	// Lifecycle: notify the TUI the agent has started so the
	// status bar's "Agents: N" counter increments. Mirrors the
	// runChatAgent path's emit("agent_start"/"agent_end") so the
	// counter behaves consistently across both code paths. Defer
	// the end-event so it fires regardless of how this function
	// returns (success, panic, ExitErr).
	if cfg.OnAgentLifecycle != nil {
		cfg.OnAgentLifecycle(run.Task.Name, "start")
		defer cfg.OnAgentLifecycle(run.Task.Name, "end")
	}

	// Pin the prompt's "Working directory:" line to the spawn dir, not
	// the main repo. Otherwise the agent prepends `cd <main-repo> &&
	// ...` to its bash commands (it sees mainRepo in the prompt and
	// "helpfully" goes there), which breaks spawn-dir isolation: file
	// tools edit spawn while bash mutates main. The bash-side cd
	// stripper in tools/bash.go is the second line of defense; this
	// override is the first. We deep-copy the context so we don't
	// mutate cfg's value across runs (orchestrator is invoked once
	// per Execute but agents share cfg by reference).
	promptCtx := cfg.PromptContext
	promptCtx.RepoRoot = dir
	prompt := agentprompt.Build(run.Task, promptCtx)
	// Evidence preamble (2026-05-26 spec #1). When the planner
	// attached cited locations + annotations to this task, render
	// them as an EVIDENCE FROM PLANNING block ahead of the task
	// prompt. Each entry gets drift-checked against the spawn dir
	// (which is a CoW clone of the integrate snapshot at this
	// moment): if the cited file's current BLAKE3 hash matches the
	// planner's recorded hash, pass the excerpt through; otherwise
	// emit a degraded form telling the executor the file has
	// changed and to re-read before acting. Without drift handling,
	// a multi-agent plan where agent A edits a file agent B cited
	// would pass stale line numbers to B.
	if evidence := renderEvidencePreamble(run.Task.Evidence, dir); evidence != "" {
		prompt = evidence + "\n\n" + prompt
	}

	runCtx := ctx
	if cfg.AgentTimeout > 0 {
		// Reasoning-model scaling — same 3x bump applied to
		// the TUI's chatWallClockBudget in v0.31.30. A 2026-05-24
		// follow-up run died at the 10-minute orchestrator cap
		// mid-productive-work (7 substantial files written
		// successfully before the kill) because chatWallClockBudget
		// got scaled but AgentTimeout didn't — different timer,
		// same model latency profile. Mirror the same rule here
		// so an orchestrator-spawned sub-agent on a reasoning
		// model gets the same headroom the chat path does.
		//
		// This is a stopgap. The deeper fix is replacing
		// wall-clock budgets with idle-timeout — see
		// [[idle-timeout-rework]] in the task queue. Wall-clock
		// caps kill actively-progressing agents; idle caps only
		// kill stuck ones.
		timeout := cfg.AgentTimeout
		if agent.IsReasoningModel(cfg.AgentModel) {
			timeout = timeout * 3
		}
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Idle-timeout watchdog. Wall-clock alone killed productive
	// agents (2026-05-24 dogfood). The idle watcher cancels the
	// run only when neither a tool call nor a streaming-text-delta
	// has fired for AgentIdleTimeout — i.e. the agent is silent,
	// not just slow. lastProgressNanos is updated atomically by
	// the OnAssistantDelta and OnToolCall hooks below. The
	// watchdog ticks every second; cheap (one Now() + one Load()
	// + one compare per tick) and bounded by runCtx.Done so it
	// exits as soon as the run finishes either way.
	var idleCancel context.CancelFunc
	var lastProgressNanos atomic.Int64
	if cfg.AgentIdleTimeout > 0 {
		lastProgressNanos.Store(time.Now().UnixNano())
		runCtx, idleCancel = context.WithCancel(runCtx)
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-tick.C:
					last := time.Unix(0, lastProgressNanos.Load())
					if time.Since(last) > cfg.AgentIdleTimeout {
						idleCancel()
						return
					}
				}
			}
		}()
		defer idleCancel()
	}
	bumpProgress := func() {
		if cfg.AgentIdleTimeout > 0 {
			lastProgressNanos.Store(time.Now().UnixNano())
		}
	}

	taskName := run.Task.Name

	// Verify-pass tracking. The orchestrator captures (a) whether
	// any file was written during the run and (b) the FIRST bash
	// command the agent issued. After the run finishes, if both
	// landed and the run was debug-mode, a follow-up verify pass
	// re-runs that bash command and confirms the fix actually
	// worked. Mutex-free because hooks fire serially within a single
	// agent run (the runner doesn't fan out hooks across goroutines).
	var firstBashCmd string
	var editsApplied bool
	// changedPaths accumulates every relpath the agent's edits hit
	// (deduped) — used by the test pass to know which files warrant
	// coverage. Populated from OnFileChange. Distinct from
	// run.ChangedPaths, which gets set by the integrate phase
	// downstream (post-absorb) and isn't available to us here.
	changedSet := map[string]bool{}

	// Per-bash-call line counter so OnBashOutput can throttle
	// flooding without dropping critical signal. Reset to 0 on each
	// new bash dispatch. Same shape the chat-fallback path uses.
	var bashLineCount int

	// Per-executor debug log. Captures the full per-turn trace
	// (model requests, tool inputs, assistant text, token totals,
	// retries) so executor failures are diagnosable after the spawn
	// terminates. Without this, the only persistence is the
	// run.json metadata files — byte counts but no message bodies
	// — which makes every executor failure a guess. Mirrors the
	// planner's planner-debug.log shape at
	// <spawnDir>/.kai/executor-debug.log so `tail -f` works
	// identically and grep idioms transfer. Best-effort: open
	// failure returns nil and the run continues without logging.
	//
	// MkdirAll the .kai/ subdir first: runOneAgent runs BEFORE the
	// orchestrator's spawn-setup phase has called `kai init` to
	// create that directory, so OpenDebugLogNamed would otherwise
	// fail silently on the missing parent. 2026-05-27 v0.32.67
	// shipped without the mkdir and produced zero logs; this
	// fixes that.
	//
	// The 2026-05-27 live-recent-snapshots executor pinned the
	// need: 47 turns across 3 retries, 16 bash errors, and zero
	// visibility into what the failing commands actually were.
	kaiSubdir := filepath.Join(dir, ".kai")
	_ = os.MkdirAll(kaiSubdir, 0o755)
	dbg, _ := planner.OpenDebugLogNamed(kaiSubdir, prompt, "executor-debug.log", "EXECUTOR")
	defer dbg.Close()

	hooks := agent.Hooks{
		OnFileChange: func(relPath, op string) {
			editsApplied = true
			if relPath != "" {
				changedSet[relPath] = true
			}
			if cfg.OnActivity != nil {
				cfg.OnActivity(taskName, relPath, op)
			}
		},
		OnAssistantText: func(text string) {
			dbg.Text(text)
			if cfg.OnActivity != nil {
				cfg.OnActivity(taskName, "(assistant)", text)
			}
		},
		OnRoutingTrace: func(msg string) {
			dbg.Routing(msg)
			if cfg.OnActivity != nil {
				cfg.OnActivity(taskName, "(route)", msg)
			}
		},
		OnToolCall: func(name, inputJSON string) {
			dbg.Tool(name, inputJSON)
			// Idle-timeout reset: a tool call is unambiguous
			// forward progress.
			bumpProgress()
			if name == "bash" && firstBashCmd == "" {
				firstBashCmd = extractBashCommand(inputJSON)
			}
			if name == "bash" {
				bashLineCount = 0
			}
			if cfg.OnActivity != nil {
				cfg.OnActivity(taskName, "(tool)", summarizeToolCall(name, inputJSON))
			}
		},
		OnRequest: func(turn int, req provider.Request) {
			dbg.Request(turn, req)
		},
		OnTurnComplete: func(tokensIn, tokensOut, tokensCached int) {
			dbg.Turn(tokensIn, tokensOut, tokensCached)
		},
		OnRetryWait: func(attempt int, delay time.Duration, err error) {
			dbg.Retry(attempt, delay, err)
		},
		OnAssistantDelta: func(delta string) {
			// Idle-timeout reset: streaming text deltas are
			// the second progress signal. A reasoning model
			// finishing its hidden <think> phase emits visible
			// text — bumping here keeps the watchdog from
			// killing a model that's actively writing its
			// answer. Empty deltas (heartbeat-only) don't
			// count; bytes do.
			if strings.TrimSpace(delta) != "" {
				bumpProgress()
			}
		},
		OnBashOutput: func(line string) {
			// Show the first 20 lines per bash call; after that
			// emit a single "remaining output suppressed" notice
			// and drop the rest. 20 fits typical "build & run"
			// output (compile + a few lines of program output)
			// without spamming the TUI on chatty commands like
			// `make` against a big project. The full output still
			// flows back to the agent in the tool result (where
			// our head+tail truncation applies).
			bashLineCount++
			if bashLineCount == 21 {
				if cfg.OnAgentBashOutput != nil {
					cfg.OnAgentBashOutput(taskName, "(remaining output suppressed — see tool result)")
				}
				return
			}
			if bashLineCount > 21 {
				return
			}
			if cfg.OnAgentBashOutput != nil {
				cfg.OnAgentBashOutput(taskName, line)
			}
		},
		OnFileBroadcast: func(relPath, digest, contentBase64 string) {
			if cfg.LiveSync != nil {
				cfg.LiveSync(relPath, digest, contentBase64)
			}
		},
		OnFileDiff: func(relPath, op, unifiedDiff string, added, removed int) {
			if cfg.OnFileDiff != nil {
				cfg.OnFileDiff(taskName, relPath, op, unifiedDiff, added, removed)
			}
		},
		OnProviderState: func(state provider.RequestState) {
			if cfg.OnAgentProviderState != nil {
				cfg.OnAgentProviderState(taskName, state)
			}
		},
		OnBashConfirm: func(cmd, warning string) bool {
			if cfg.OnAgentBashConfirm != nil {
				return cfg.OnAgentBashConfirm(taskName, cmd, warning)
			}
			return true
		},
		OnFileConfirm: func(op, path string, added, removed int, diff string) bool {
			if cfg.OnAgentFileConfirm != nil {
				return cfg.OnAgentFileConfirm(taskName, op, path, added, removed, diff)
			}
			// No hook configured → permit. Matches the headless
			// CLI default; the TUI sets this hook to enforce
			// per-write confirmation.
			return true
		},
	}

	// Per-agent checkpoint writer rooted at the spawn dir's kai
	// directory. We resolve via kaipath, NOT a hardcoded
	// `dir+"/.kai"`, because the spawn dir might be a git repo
	// (kai data lives at .git/kai) or not (.kai). Hardcoding .kai
	// silently writes checkpoints to a path nothing else reads —
	// `kai blame` then reports "no authorship data" even though
	// the agent recorded edits. The integrate phase later
	// consolidates these into the main repo via
	// authorship.Consolidate. Session ID matches the AgentTask
	// name so multi-turn checkpoints aggregate per agent
	// (collisions across simultaneous tasks are avoided since
	// each task gets its own spawn dir).
	ckpt := authorship.NewCheckpointWriter(kaipath.Resolve(dir), taskName)

	// Graph-powered context injection. Build the injection body
	// from the user's task prompt against the project's call graph
	// + cobra command index, then thread it through agent.Options
	// so the runner splices the synthetic context_lookup tool-use
	// pair into the transcript. Empty body (no resolvable entry
	// points) leaves InjectedContext unset and the agent runs
	// without injection. Best-effort: a missing graph just produces
	// an empty injection.
	var injectionBody string
	if cfg.MainGraph != nil {
		cmdIdx := planner.LoadCommandIndex(mainRepo)
		injectionBody = planner.BuildInjectedContext(prompt, cfg.MainGraph, cmdIdx)
	}
	// Prepend the BUILD CONTEXT block: verified module roots + the
	// exact build/test commands, scanned from the spawn dir. This
	// removes the single most repeated source of agent meandering —
	// rediscovering where go.mod lives turn after turn. Computed
	// independently of the graph so it lands even when MainGraph is
	// nil.
	if bc := buildContextBlock(dir); bc != "" {
		if injectionBody != "" {
			injectionBody = bc + "\n" + injectionBody
		} else {
			injectionBody = bc
		}
	}
	// TEST CONTEXT: the framework convention + example test files,
	// so an agent asked to add a test neither burns its read budget
	// rediscovering the convention nor invents a new one (the
	// 2026-05-16 gomega-in-a-Go-repo incident).
	if tc := testContextBlock(dir); tc != "" {
		if injectionBody != "" {
			injectionBody = tc + "\n" + injectionBody
		} else {
			injectionBody = tc
		}
	}

	soft, hard := scopeAwareReadStreak(len(run.Task.Files))
	mainOpts := agent.Options{
		Workspace:        dir,
		Projects:         spawnSet, // nil for single-root; multi-root passes the rewritten set
		Prompt:           prompt,
		InjectedContext:  injectionBody,
		Model:            cfg.AgentModel,
		MaxTotalTokens:   cfg.MaxAgentTokens,
		Provider:         cfg.AgentProvider,
		ConsultProvider:  cfg.AgentProvider, // same transport, different model
		ConsultModel:     cfg.ConsultModel,
		KailabBaseURL:    cfg.KailabBaseURL,
		KailabToken:      cfg.KailabToken,
		Graph:            cfg.MainGraph,
		EnableBash:       cfg.AgentBashEnabled,
		BashAllow:        cfg.AgentBashAllow,
		SessionStore:     cfg.AgentSessionStore,
		TaskName:         taskName,
		Hooks:            hooks,
		Mode:             agent.ParseMode(run.Task.Mode),
		KaiBinary:        kaiBinary(cfg),
		CheckpointWriter: ckpt,
		LiveSyncClient:   cfg.LiveSyncClient,
		SyncChannelID:    cfg.LiveSyncChannelID,
		ReadStreakSoft:   soft,
		ReadStreakHard:   hard,
		// Per-turn read cap of 5: even when the turn-grain streak
		// allows another read-only turn, no single turn may issue
		// more than 5 read-only calls. Stops the "ten views in one
		// turn" pattern that ate the failing run's call budget
		// inside the turn-grain limit. Chat/planner stay uncapped
		// (Options default 0).
		MaxReadsPerTurn: 5,
		// MaxTurns 20: the runner default is 50, but executor runs
		// without convergence pressure at that scale — the wind-down
		// hint fires at turnsLeft <= 3, so a 50-turn budget means
		// pressure only at turns 47-50, way past where a runaway
		// loop should have been forced to commit. The 2026-05-26
		// edges executor pinned this: 34 turns for a 4-step plan,
		// 11 of 16 bash calls erroring, 6 turns of zero tool calls
		// (model idle/empty completion), and even after a clean
		// edit landed on turn 21 the verify pass spun another 12
		// turns of failed bash before quitting. Capping at 20 (2x
		// the planner's 10) gives enough headroom for a real verify
		// pass — exploration, edits, build/test, checkpoint — while
		// firing the wind-down hint at turn 17 so a stuck executor
		// gets convergence pressure before token waste mounts.
		MaxTurns: func() int {
			if cfg.ExecutorMaxTurns > 0 {
				return cfg.ExecutorMaxTurns
			}
			return 20
		}(),
		RunLogDir: func() string {
			if cfg.RunLogDir != "" {
				return cfg.RunLogDir
			}
			return kaipath.Resolve(dir)
		}(),
	}
	mainRes, err := agent.Run(runCtx, mainOpts)
	// Snapshot the in-flight write/edit targets BEFORE the error
	// check: an agent can error (context deadline exceeded, budget,
	// provider drop) AFTER it has already written real edits to disk.
	// Recording TouchedPaths unconditionally lets the integrate phase
	// rescue that work — absorb it and hold it for review — instead of
	// discarding a finished change because the run didn't exit clean.
	run.TouchedPaths = mapKeysSorted(changedSet)
	if err != nil {
		run.ExitErr = fmt.Errorf("agent %s: %w", taskName, err)
		return
	}

	// Auto-verify pass. After a debug-mode agent applies edits, run
	// a second agent that re-runs the agent's first bash command
	// and confirms the fix actually works. Skipped when:
	//   - the agent failed (run.ExitErr set above; we returned early)
	//   - no edits were applied (nothing to verify)
	//   - no bash command was issued (no concrete check available)
	//   - mode wasn't debug (verify is meaningful for "is the bug
	//     gone" — coding/review/conversation don't have a natural
	//     "did the fix work" check)
	if shouldVerify(run.Task.Mode, editsApplied, firstBashCmd) {
		// Phase 1 observability: log what `kit ci plan` would do for
		// this run, side-by-side with the LLM verify that actually
		// fires. No behavior change — we run both, the plan output is
		// log-only, so we can validate plan accuracy before trusting
		// it as a verify substitute (Phase 2). The cost is one subprocess
		// per run; the call is fully best-effort and any error is
		// swallowed so a broken ci plan never blocks verify.
		logCIPlanPreview(runCtx, dir, run.Task.Name, cfg)
		runVerifyPass(runCtx, run, cfg, dir, ckpt, mainOpts, hooks, firstBashCmd, mapKeysSorted(changedSet), &editsApplied)
	}

	// Record the graph-context-injection metric. Best-effort: any
	// I/O failure inside is swallowed. Runs even when injection
	// didn't fire (InjectedChars=0) so the dashboard can see the
	// "context-less" baseline alongside the injected runs.
	if mainRes != nil {
		recordInjectionMetric(
			mainRepo,
			run.Task.Name,
			run.Task.Mode,
			mainOpts.InjectedContext,
			mainRes.Transcript,
			verifyOutcome(run.VerifyOutcome),
			mainRes,
		)
	}

	// Auto-test pass. Runs after verify when:
	//   - the agent applied edits to non-test, non-doc source
	//   - a test convention was detected in the workspace
	//   - cfg.AutoTest isn't explicitly disabled
	// The pipeline is: agent → verify → test agent → harness runs
	// the test command. The harness is the source of truth for
	// pass/fail counts, independent of what the test agent claims.
	if cfg.AutoTest && editsApplied {
		changedPaths := mapKeysSorted(changedSet)
		nonTestSource := nonTestSourceChanges(changedPaths)
		if len(nonTestSource) > 0 {
			conv, ok := detectTestConvention(dir)
			switch {
			case ok && shouldRunTests(changedPaths, conv):
				runTestPass(runCtx, run, cfg, dir, ckpt, mainOpts, hooks, conv, nonTestSource)
			case !ok:
				// Surface the skip so users know we considered it. A
				// silent skip looks like a bug ("did test pass run?
				// did it pass?"); the lifecycle event lets the TUI
				// emit a one-line note.
				run.TestSummary = "test pass skipped: no test convention detected (no go.mod, package.json scripts.test, Cargo.toml, pyproject test config, or Makefile test target)"
				if cfg.OnAgentLifecycle != nil {
					cfg.OnAgentLifecycle(run.Task.Name, "test_skipped")
				}
			}
		}
	}
}

// mapKeysSorted returns the keys of a string-set in deterministic
// order. Used so the test-pass changed-files list reads stably
// across runs.
func mapKeysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings is a small adapter so we don't import "sort" just for
// one slice ordering. Insertion sort — fine for the sub-1000-entry
// changed-files list.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// runTestPass executes the test-coverage follow-up. Mirrors
// runVerifyPass: same workspace, provider, model; different prompt;
// NoVerify=true AND NoTests=true to prevent recursion. The harness
// runs the test command itself after the agent finishes so pass/fail
// counts are authoritative regardless of what the agent claims.
func runTestPass(
	ctx context.Context,
	run *AgentRun,
	cfg Config,
	spawnDir string,
	ckpt *authorship.CheckpointWriter,
	mainOpts agent.Options,
	mainHooks agent.Hooks,
	conv testConvention,
	nonTestSource []string,
) {
	if cfg.OnAgentLifecycle != nil {
		cfg.OnAgentLifecycle(run.Task.Name, "test_start")
		defer cfg.OnAgentLifecycle(run.Task.Name, "test_end")
	}

	prompt := buildTestPrompt(nonTestSource, conv, run.Task.Prompt)
	opts := mainOpts
	opts.Prompt = prompt
	opts.Hooks = mainHooks
	opts.Mode = agent.ModeCoding // test pass writes/edits test files
	opts.NoVerify = true
	opts.NoTests = true
	opts.SessionID = ""

	if _, err := agent.Run(ctx, opts); err != nil {
		run.TestErr = err
		return
	}

	// Harness-run: regardless of what the agent claimed, run the
	// test command ourselves and capture the result. This is the
	// promised guarantee — "the harness never auto-fixes failing
	// tests, but it always runs them and tells the user the truth."
	runDir := spawnDir
	if conv.Dir != "" {
		runDir = filepath.Join(spawnDir, conv.Dir)
	}
	res := runTestCommand(ctx, runDir, conv.Run, 5*time.Minute)
	switch {
	case res.Errored:
		run.TestSummary = fmt.Sprintf("⚠ test command failed to start: %s", res.StartErr)
	case res.ExitCode == 0:
		run.TestSummary = fmt.Sprintf("✓ tests passed (%s, exit 0)", res.Duration.Round(time.Second))
	default:
		run.TestSummary = fmt.Sprintf("⚠ tests FAILED (%s, exit %d) — see test output above",
			res.Duration.Round(time.Second), res.ExitCode)
	}
	run.TestOutput = res.Output
	run.TestExitCode = res.ExitCode
}

// runVerifyPass executes the follow-up verify agent. It mirrors the
// main agent's options (same workspace, provider, model, tools)
// EXCEPT for the prompt and the NoVerify flag — both diverge by
// design. The prompt drives a verification-style investigation; the
// flag prevents the verify pass from triggering its own verify.
//
// The verify run reuses the spawn dir so any additional edits land
// alongside the main agent's, and the integrate phase picks up
// both. SessionID is intentionally unset — verify gets its own row
// in the persisted store so the transcript is replayable separately.
//
// The result is plumbed back into the AgentRun via VerifyOutcome /
// VerifySummary so the TUI can render a "─ verify ─" block. If
// verify itself fails, that's surfaced as run.VerifyErr — the main
// run is NOT marked failed (the original fix may still be valid).
func runVerifyPass(
	ctx context.Context,
	run *AgentRun,
	cfg Config,
	spawnDir string,
	ckpt *authorship.CheckpointWriter,
	mainOpts agent.Options,
	mainHooks agent.Hooks,
	firstBashCmd string,
	mainChangedPaths []string,
	mainEditsAppliedPtr *bool,
) {
	if cfg.OnAgentLifecycle != nil {
		cfg.OnAgentLifecycle(run.Task.Name, "verify_start")
		defer cfg.OnAgentLifecycle(run.Task.Name, "verify_end")
	}

	// Track verify-pass edits separately so the outcome classifier
	// can tell "verify passed cleanly" from "verify applied more
	// fixes." Hooks otherwise mirror the main run so the TUI's
	// existing tool/file/diff channels still light up.
	var verifyEditsApplied int
	verifyHooks := mainHooks
	verifyHooks.OnFileChange = func(relPath, op string) {
		verifyEditsApplied++
		if mainHooks.OnFileChange != nil {
			mainHooks.OnFileChange(relPath, op)
		}
	}

	verifyPrompt := buildVerifyPrompt(run.Task.Prompt, firstBashCmd, mainChangedPaths)
	verifyOpts := mainOpts
	verifyOpts.Prompt = verifyPrompt
	verifyOpts.Hooks = verifyHooks
	verifyOpts.Mode = agent.ModeDebug // verify is always debug-mode-shaped
	verifyOpts.NoVerify = true        // recursion guard
	verifyOpts.SessionID = ""         // fresh session row for this pass

	res, err := agent.Run(ctx, verifyOpts)
	if err != nil {
		run.VerifyErr = err
		return
	}

	// Pull the final assistant text out of the transcript so we can
	// detect the VERIFIED / BLOCKED sentinels.
	var finalText string
	if res != nil {
		for i := len(res.Transcript) - 1; i >= 0; i-- {
			m := res.Transcript[i]
			if m.Role != message.RoleAssistant {
				continue
			}
			for _, p := range m.Parts {
				if t, ok := p.(message.TextContent); ok {
					finalText = t.Text
					break
				}
			}
			if finalText != "" {
				break
			}
		}
	}

	outcome := classifyVerifyTranscript(finalText, verifyEditsApplied)
	run.VerifyOutcome = int(outcome)
	run.VerifySummary = verifySummary(outcome, verifyEditsApplied)

	// Roll the verify pass's edits into the main run's accounting so
	// the integrate phase processes them in the same absorb. Without
	// this the orchestrator's caller might re-classify "no edits"
	// even though the verify pass did write files.
	if verifyEditsApplied > 0 && mainEditsAppliedPtr != nil {
		*mainEditsAppliedPtr = true
	}
}

// integrateOneAgent absorbs a finished agent's edits into the main
// repo, then runs the safety gate against the result. Sequence:
//
//  1. Capture in the spawn dir (audit trail; spawn DB stays
//     self-consistent for debugging).
//  2. Diff spawn vs main and copy/delete the changed files into the
//     main repo's working tree.
//  3. Run `kai capture` in main to record the new state. snap.latest
//     auto-advances at this point.
//  4. Run safetygate.Classify on the changed paths. Tag the new snap
//     with the verdict so `kai gate list` surfaces held integrations.
//  5. If the verdict is non-Auto, roll snap.latest back to its
//     previous value — the new snap stays in the DB (with verdict
//     metadata) for review, but it's not team-visible. The
//     filesystem changes stay in main's working tree either way;
//     the user can `git diff` and decide what to do.
//
// Skips entirely if the agent failed in phase A, or if the agent
// produced no observable changes.
func integrateOneAgent(ctx context.Context, run *AgentRun, cfg Config, db *graph.DB, mainRepo string, baseline buildCheckResult) {
	if run.SpawnDir == "" {
		return
	}
	// A clean failure with no edits has nothing to integrate. But an
	// agent that errored AFTER writing edits (e.g. context deadline
	// exceeded during its verify step) still produced real work —
	// fall through so absorb + classify run and the gate can hold it
	// for review. The errored state forces a non-Auto verdict below.
	if run.ExitErr != nil && len(run.TouchedPaths) == 0 {
		return
	}

	// Diagnostic timing: round-21 retest (2026-05-13) saw the
	// integrate phase take ~4 minutes against accountable steps
	// totaling ~10 seconds when measured individually. Each step
	// below records start→end into ~/.kai/integrate-diag.log so
	// the next observed hang surfaces which step is the culprit.
	// Best-effort writes; remove this block once the slow step is
	// identified and addressed.
	diagStart := time.Now()
	defer func() { integrateDiag(run.Task.Name, "TOTAL", time.Since(diagStart)) }()
	timeStep := func(name string, fn func()) {
		t := time.Now()
		fn()
		integrateDiag(run.Task.Name, name, time.Since(t))
	}

	// Spawn-side capture: best-effort, non-fatal. Even if this
	// fails the absorb below works directly against the filesystem.
	//
	// KAI_CAPTURE_SKIP_SUMMARY: the change-summary phase parses every
	// modified file with tree-sitter and AST-diffs old vs new. On a
	// 22k-line file (round-22 dogfood: kai-cli/cmd/kai/main.go) it
	// dominated wall time at 3m35s. Pre-spawn capture already sets
	// this; the integrate-side captures didn't, so the user saw the
	// orchestrator "hang" after the LLM agents finished. Summary is
	// purely human-facing output and these captures never display it.
	timeStep("capture-spawn", func() {
		_ = runInWithEnv(ctx, run.SpawnDir,
			[]string{"KAI_CAPTURE_SKIP_SUMMARY=1"},
			kaiBinary(cfg), "capture", "-m",
			fmt.Sprintf("orchestrator: agent %s", run.Task.Name))
	})

	// Apply the agent's filesystem edits to main.
	//
	// A multi-root spawn dir contains every project as a named subdir
	// (<spawn>/kai/, <spawn>/kai-server/, ...). absorbSpawnIntoMain
	// compares one spawn tree against one real project tree by
	// relative path, so each project must be absorbed against its OWN
	// real directory — passing the spawn root would diff
	// `<spawn>/kai-server/api/foo.go` against `<mainRepo>/api/foo.go`,
	// a depth mismatch absorb reads as "everything moved."
	//
	// We absorb the primary project AND every sibling project the
	// agent actually wrote into. The 2026-05-15 dogfood pinned why
	// the old primary-only scope was wrong: the user launched `kai
	// code` from kai-server, but the task targeted the kai project —
	// absorb looked only at <spawn>/kai-server, found nothing, and
	// silently orphaned the worker's real edit in <spawn>/kai. A
	// run's deliverable lives wherever the plan scoped it, which is
	// frequently NOT the directory the user launched from.
	var targets []absorbTarget
	if cfg.Projects != nil && len(cfg.Projects.Projects()) > 1 {
		// Project basenames the agent's write/edit hooks fired on —
		// the first path segment of each touched relpath.
		touched := map[string]bool{}
		for _, tp := range run.TouchedPaths {
			if i := strings.IndexByte(tp, '/'); i > 0 {
				touched[tp[:i]] = true
			}
		}
		primaryName := ""
		if pr := cfg.Projects.Primary(); pr != nil {
			primaryName = projectDirBasename(pr)
		}
		for _, p := range cfg.Projects.Projects() {
			base := projectDirBasename(p)
			// Absorb the primary always (back-compatible) plus any
			// sibling the agent wrote into. Untouched siblings are a
			// faithful copy of their real dir — absorbing them is a
			// pure no-op, so skipping is just a cost/clarity win.
			if base != primaryName && !touched[base] {
				continue
			}
			spawnSub := filepath.Join(run.SpawnDir, base)
			// MANDATORY safety guard. A missing spawn subdir means the
			// project was never materialized (spawnForMulti drops a
			// project whose copy failed). absorbSpawnIntoMain on a
			// missing/empty spawn dir reads zero files and then
			// DELETES the entire real project as "removed by agent" —
			// the 9k-file moby-wipe class of bug. Never absorb a
			// project we cannot see, fully present, in the spawn.
			if st, e := os.Stat(spawnSub); e != nil || !st.IsDir() {
				absorbTrace(run.Task.Name, fmt.Sprintf(
					"project %q: no spawn subdir at %s — SKIPPED (any edit left in spawn for manual recovery)", base, spawnSub))
				continue
			}
			mainDir := p.Path
			if base == primaryName {
				mainDir = mainRepo
			}
			targets = append(targets, absorbTarget{name: base, spawnDir: spawnSub, mainDir: mainDir})
		}
	} else {
		targets = append(targets, absorbTarget{name: "", spawnDir: run.SpawnDir, mainDir: mainRepo})
	}

	// Per-project build baseline (Phase B). Capture each touched
	// project's build state BEFORE absorb, while its real dir is still
	// pristine (the agent worked in the spawn copy, not here). This is
	// the delta reference for the per-project build gate below — we
	// block only on packages a project NEWLY broke. The primary reuses
	// the baseline captured pre-spawn in Execute (its real dir is
	// likewise untouched by the spawned agents), so single-root runs
	// pay no extra build check.
	baselineByDir := map[string]buildCheckResult{mainRepo: baseline}
	timeStep("build-baseline", func() {
		for _, t := range targets {
			if _, ok := baselineByDir[t.mainDir]; ok {
				continue
			}
			baselineByDir[t.mainDir] = runBuildCheck(ctx, t.mainDir)
		}
	})

	var changed []string
	var err error
	timeStep("absorb", func() {
		for _, t := range targets {
			c, e := absorbSpawnIntoMain(t.spawnDir, t.mainDir)
			if e != nil {
				absorbTrace(run.Task.Name, fmt.Sprintf("project %q: ERROR %v", t.name, e))
				err = fmt.Errorf("absorb %s: %w", t.name, e)
				return
			}
			absorbTrace(run.Task.Name, fmt.Sprintf(
				"project %q: %d file(s) changed (spawn=%s main=%s)", t.name, len(c), t.spawnDir, t.mainDir))
			// Prefix multi-root results with the project basename so
			// ChangedPaths is unambiguous across projects and matches
			// the project-prefixed convention used elsewhere.
			for _, cp := range c {
				if t.name != "" {
					changed = append(changed, t.name+"/"+cp)
				} else {
					changed = append(changed, cp)
				}
			}
		}
	})
	if err != nil {
		run.IntegrateErr = fmt.Errorf("absorb: %w", err)
		return
	}
	run.ChangedPaths = changed
	if len(changed) == 0 {
		// No-op agent; nothing to gate or capture. If the agent did
		// fire write/edit hooks during the run, surface that — the
		// most common cause is the agent re-writing a file back to
		// its original contents (often after seeing stale cached
		// view results and "fixing" something that wasn't broken).
		// Cache eviction on write (runner.go) addresses the loop;
		// this note tells the user when it happened.
		if len(run.TouchedPaths) > 0 {
			// SPAWN-1 reconciliation (2026-05-26 master spec). The
			// agent's file-tool counter says writes happened, but
			// absorb's content-diff saw nothing to integrate. Three
			// classes — re-stat each TouchedPath against the spawn
			// to figure out which:
			//   (a) PRESENT IN SPAWN, IDENTICAL TO MAIN — agent
			//       rewrote a file back to its original contents
			//       (the historical default explanation). Note
			//       only; not an error.
			//   (b) PRESENT IN SPAWN, ABSENT FROM MAIN — absorb's
			//       path-scoping filter dropped the edit. REAL BUG:
			//       the spawn-side file is intact but the integrate
			//       step refused to copy it back. Surface as an
			//       integrate error so the user knows to recover
			//       the spawn-side edit manually before /tmp gets
			//       cleaned up.
			//   (c) ABSENT FROM SPAWN ENTIRELY — write claimed
			//       success but the file vanished between write and
			//       integrate. Pathological: filesystem race,
			//       symlink shenanigans, or some intermediate code
			//       path removed the file. Surface as an integrate
			//       error too — silent data loss is worse than a
			//       loud "we don't know what happened" report.
			var (
				identical, intactNew, missing []string
			)
			for _, rel := range run.TouchedPaths {
				spawnPath := filepath.Join(run.SpawnDir, rel)
				spawnContent, sErr := os.ReadFile(spawnPath)
				if sErr != nil {
					missing = append(missing, rel)
					continue
				}
				mainPath := filepath.Join(mainRepo, rel)
				mainContent, mErr := os.ReadFile(mainPath)
				if mErr != nil {
					// File present in spawn, absent from main —
					// absorb should have integrated this as a
					// new-file add.
					intactNew = append(intactNew, rel)
					continue
				}
				if bytes.Equal(spawnContent, mainContent) {
					identical = append(identical, rel)
				} else {
					// Present in both with different content but
					// absorb didn't see it — also a path-scoping
					// drop. Bucket with intactNew (same fix:
					// recover from spawn).
					intactNew = append(intactNew, rel)
				}
			}
			// Build the surfaced message. Loudest case wins for the
			// error vs note routing: any missing or intactNew → error.
			head := func(s []string) (string, string) {
				const cap = 6
				if len(s) <= cap {
					return strings.Join(s, ", "), ""
				}
				return strings.Join(s[:cap], ", "), fmt.Sprintf(" … and %d more", len(s)-cap)
			}
			switch {
			case len(missing) > 0:
				h, more := head(missing)
				run.IntegrateErr = fmt.Errorf(
					"agent reported writes that did not persist (%d file(s) vanished from spawn between write and integrate — likely a filesystem race or intermediate cleanup). Spawn dir kept at %s for inspection — find -type f -newer. Missing: %s%s",
					len(missing), run.SpawnDir, h, more,
				)
			case len(intactNew) > 0:
				h, more := head(intactNew)
				run.IntegrateErr = fmt.Errorf(
					"agent's edits exist in spawn but absorb did not integrate them (%d file(s) — likely a spawn-vs-main path-scoping bug). Spawn dir kept at %s for manual recovery. Affected: %s%s",
					len(intactNew), run.SpawnDir, h, more,
				)
			default:
				// Pure identical case: the historical "rewrote
				// to original" path. Note, not error.
				h, more := head(identical)
				run.IntegrateNote = fmt.Sprintf(
					"agent wrote to %d file(s) but the content matches mainRepo — the agent likely re-wrote a file back to its original contents (often from a stale cached view). No edits to integrate. Touched: %s%s",
					len(identical), h, more,
				)
			}
			return
		}
		// Zero-edits guard: the worker exited clean without writing to
		// any file. If the planner named ≥3 concrete signals (file
		// paths or specific identifiers), this is a research-only run
		// — the worker described changes without making them. Round-14
		// dogfood: opus emitted a detailed 5-step plan, worker ran 30+
		// view/grep calls, ended with "here are the exact edits needed"
		// and exited. Dangle guard's regex missed the adjective; this
		// is the integrate-side backstop.
		symbols, files := extractPlanSignals(run.Task.Prompt)
		if len(symbols)+len(files) >= 3 {
			missing := append([]string{}, files...)
			missing = append(missing, symbols...)
			const cap = 6
			if len(missing) > cap {
				missing = append(missing[:cap], "…")
			}
			run.IntegrateErr = fmt.Errorf(
				"worker produced no edits but plan named %d signals: %s",
				len(symbols)+len(files), strings.Join(missing, ", "),
			)
		}
		return
	}

	// Migrate authorship checkpoints from the spawn dir's kai
	// directory to main's. The spawned agent's kai_checkpoint
	// calls landed at <spawnDir>/.git/kai/checkpoints/ (or .kai/
	// depending on layout). The next `kai capture` consolidates
	// checkpoints from MAIN's kai dir into the snapshot's
	// authorship index — without this copy, those checkpoints are
	// stranded in the spawn dir and `kai blame` returns "no
	// authorship data" for files the orchestrator agents edited.
	timeStep("checkpoints", func() {
		if err := copyCheckpoints(run.SpawnDir, mainRepo); err != nil {
			// Non-fatal: blame data will be missing but the
			// integration itself should still complete. Surface
			// via debug, not IntegrateErr.
			_ = err
		}
	})

	// Build per-project state (one per touched project). Single-root
	// runs get one state pointing at the primary's plumbing — the
	// downstream loop is uniform regardless of root count. See
	// integrate_per_project.go for the multi-root rationale.
	states := buildProjectStates(cfg, db, mainRepo, targets, changed)
	// Attach each project's pre-absorb build baseline (captured above)
	// so the per-project build gate can compute its delta.
	for i := range states {
		states[i].baseline = baselineByDir[states[i].target.mainDir]
	}

	// Snapshot snap.latest BEFORE capture for each project so we can
	// (a) compute per-project diffs for classify, and (b) roll a
	// project's snap.latest back if its verdict comes back non-Auto.
	// Each state's prevLatest is consumed per-project by the build-fix
	// rollback (build-check loop below) and the gate's non-Auto
	// rollback fanout — no primary-only extraction is needed anymore.
	timeStep("resolve-prev-latest", func() {
		for i := range states {
			states[i].prevLatest, _ = resolveLatestSnap(states[i].db)
		}
	})

	var captureErr error
	timeStep("capture-main", func() {
		// Multi-root capture. Absorb (above) already fanned out and
		// wrote files into every project the agent touched. We mirror
		// that fanout here: run `kai capture` in each project's
		// mainDir so the agent's edits land in that project's own
		// snapshot graph. Before this fix, capture ran only in the
		// PRIMARY project's mainRepo — sibling projects got their
		// files updated on disk but no snapshot was ever recorded,
		// `kai log` in those projects didn't show the change, and the
		// orchestrator's HELD-for-review verdict pointed at a
		// snapshot that didn't exist. Observed in the 2026-05-20
		// fix-brave-search run where the agent wrote kai-server
		// files, the TUI reported "1 held", and `kai gate list`
		// returned nothing because no snapshot existed to hold.
		//
		// Per-project gate verdicts / classify / per-project rollback
		// are a SEPARATE follow-up — this commit just closes the
		// durability hole (work on disk + work in snapshot graph).
		// The classify+gate block below still runs against the
		// PRIMARY project only; non-primary captures land as
		// untagged snapshots that the user can `kai gate list` /
		// `kai log` from inside each project's directory. The
		// orchestrator's run.Verdict still reflects the primary's
		// outcome only.
		for i := range states {
			s := &states[i]
			t := s.target
			cerr := runInWithEnv(ctx, t.mainDir,
				[]string{"KAI_CAPTURE_SKIP_SUMMARY=1"},
				kaiBinary(cfg), "capture", "-m",
				fmt.Sprintf("orchestrator: %s", run.Task.Name))
			if cerr != nil && t.mainDir == mainRepo {
				// Primary-project capture failure is fatal — the rest
				// of the integrate path (resolve-new-latest, classify,
				// snapshot decoration) depends on it. Sibling failures
				// are flagged on the state and reported via aggregate
				// verdict; the agent's work is still on disk and the
				// user can manually capture later.
				captureErr = cerr
				s.captureFailed = true
				return
			}
			if cerr != nil {
				s.captureFailed = true
				absorbTrace(run.Task.Name, fmt.Sprintf(
					"project %q: capture failed (non-fatal, primary still proceeds): %v",
					t.name, cerr))
			}
		}
	})
	if captureErr != nil {
		run.IntegrateErr = fmt.Errorf("capture in main: %w", captureErr)
		return
	}
	// Resolve the new snap.latest per project. Each project's DB has
	// its own snap.latest pointer; capture above advanced it in each
	// initialized project that got changes. Capture-failed states
	// stay at newLatest=nil and skip downstream gate work.
	timeStep("resolve-new-latest", func() {
		for i := range states {
			s := &states[i]
			if s.captureFailed || s.gateSkipped {
				continue
			}
			s.newLatest, _ = resolveLatestSnap(s.db)
		}
	})

	// Keep `newLatest` as a name pointing at the PRIMARY's new snap
	// for the rest of this function — the build-fix loop, plan-
	// coverage, and the legacy single-project decoration block still
	// reference it under that name. (decorateProjectSnap on each
	// state below handles the per-project decoration.)
	var newLatest []byte
	for i := range states {
		if states[i].target.mainDir == mainRepo {
			newLatest = states[i].newLatest
			break
		}
	}
	if len(newLatest) == 0 {
		run.IntegrateErr = fmt.Errorf("resolve new snap.latest: primary capture produced no snapshot")
		return
	}

	// Build check: cheapest semantic-correctness gate. If the worker
	// introduced a compile error (typo, hallucinated method name —
	// round-17 dogfood: `r.renderPlanBanner()` against a struct that
	// only has `renderPlanMenu`), short-circuit before Classify and
	// mark Failed with the compiler output. Skipped silently when no
	// recognized manifest is detected or KAI_SKIP_BUILD_CHECK is set.
	//
	// Delta semantics: we block only on packages the change NEWLY broke
	// vs the pre-run baseline. A package that was already failing when
	// the user invoked kai is not the agent's fault and must not wall
	// off the run — that was the pre-2026-05-29 "workspace does not
	// compile" paper cut.
	// Build check, per project (Phase B). Each touched project is
	// checked against ITS OWN pre-absorb baseline (states[i].baseline)
	// and blocks only on packages it NEWLY broke — pre-existing breakage
	// in any project is tolerated. A project with new breakage gets its
	// own build-fix loop; if that exhausts, that project's working tree
	// is rolled back and the whole run fails (the aggregate gate below
	// won't run). Single-root collapses to one state == the primary, so
	// behavior there is identical to the pre-Phase-B path.
	//
	// Delta semantics close the pre-2026-05-29 "workspace does not
	// compile" paper cut; the per-project fanout closes the gap where a
	// sibling project the agent broke was never build-checked at all.
	timeStep("build-check", func() {
		for i := range states {
			s := &states[i]
			if s.captureFailed || s.gateSkipped {
				continue
			}
			bc := runBuildCheck(ctx, s.target.mainDir)
			newBreaks := newFailures(s.baseline, bc)
			if bc.Ran && bc.Err != nil && len(newBreaks) == 0 {
				// Tree doesn't fully compile, but every failure was
				// already there before this change — not the agent's
				// fault. Let it through; flag it for the surface.
				run.PreexistingBuildBreak = true
				continue
			}
			if !bc.Ran || len(newBreaks) == 0 {
				continue // clean, or no recognized manifest
			}
			// NEW breakage in this project — try to fix it in place.
			fixed, fbc := tryBuildFixLoop(ctx, s.db, cfg, run, s.target.mainDir, s.newLatest, s.baseline, bc)
			if fixed {
				// Re-resolve this project's snap.latest — the fix loop
				// captured a new snap on its last successful round.
				if nl, lerr := resolveLatestSnap(s.db); lerr == nil && len(nl) > 0 {
					s.newLatest = nl
					if s.target.mainDir == mainRepo {
						newLatest = nl
					}
				}
				continue
			}
			// Unfixable new breakage: roll THIS project's working tree
			// back to its pre-worker state so no broken edits remain on
			// disk. The held snapshot stays in the project's DB for
			// `kai gate diff`. Fail the whole run (all-or-nothing — the
			// aggregate gate/classify below is skipped via the
			// IntegrateErr check after this loop).
			rollbackErr := restoreWorkingTreeToSnapshot(s.db, s.prevLatest, s.target.mainDir)
			reason := formatBuildRegressionReason(fbc, s.baseline, rollbackErr)
			if s.target.name != "" {
				reason = "project " + s.target.name + ": " + reason
			}
			run.IntegrateErr = fmt.Errorf("%s", reason)
			return // exits the timeStep closure; checked below
		}
	})
	if run.IntegrateErr != nil {
		return
	}

	// Per-project classify. Each state's verdict is recorded on the
	// state; aggregate below is what bubbles to run.Verdict. Per-
	// project verdicts feed per-project snapshot decoration so each
	// project's `kai gate list` shows its own gate metadata. See
	// integrate_per_project.go.
	timeStep("classify", func() {
		classifyPerProject(ctx, states)
	})
	verdict := aggregateVerdicts(states)

	// Plan-coverage: catches "worker stopped after step 1" — when the
	// diff misses most of the symbols/files the planner named. Round-12
	// dogfood failure: planner named 6 signals, worker matched 1 (one
	// struct field), gate held only because blast > 0 with no reason
	// pointing at the gap. User saw "blast 4" and approved a no-op.
	//
	// Multi-root note: plan-coverage runs once against the primary
	// repo (the path-normalization fix for per-project coverage is a
	// separate follow-up). When it under-covers, we escalate every
	// per-project verdict + the aggregate so each project's snapshot
	// records the same coverage gap — it's a run-level signal, not a
	// per-project one.
	timeStep("plan-coverage", func() {
		cov := checkPlanCoverage(run.Task, changed)
		if !cov.UnderCovered() {
			return
		}
		reason := cov.Reason()
		for i := range states {
			s := &states[i]
			if s.captureFailed || s.gateSkipped || len(s.newLatest) == 0 {
				continue
			}
			if s.verdict.Verdict == safetygate.Auto {
				s.verdict.Verdict = safetygate.Review
			}
			s.verdict.Reasons = append(s.verdict.Reasons, reason)
		}
		if verdict.Verdict == safetygate.Auto {
			verdict.Verdict = safetygate.Review
		}
		verdict.Reasons = append(verdict.Reasons, reason)
	})

	// An agent that errored mid-run (timeout, budget, provider drop)
	// but still left edits on disk produced UNCERTAIN work — it may
	// have stopped halfway. Never auto-promote that: force review so
	// a human sees the partial change, and name the agent error as
	// the hold reason. Applied to every per-project verdict + the
	// aggregate; an ExitErr is a run-level event so every project
	// the run touched gets the same reason on its snapshot.
	if run.ExitErr != nil {
		reason := fmt.Sprintf("agent did not finish cleanly (%v) — edits may be partial; review before promoting", run.ExitErr)
		for i := range states {
			s := &states[i]
			if s.captureFailed || s.gateSkipped || len(s.newLatest) == 0 {
				continue
			}
			if s.verdict.Verdict == safetygate.Auto {
				s.verdict.Verdict = safetygate.Review
			}
			s.verdict.Reasons = append(s.verdict.Reasons, reason)
		}
		if verdict.Verdict == safetygate.Auto {
			verdict.Verdict = safetygate.Review
		}
		verdict.Reasons = append(verdict.Reasons, reason)
	}

	// Machine-checkable acceptance checks (run.Task.VerifyChecks). The
	// HARNESS runs each declared command in the integrated tree and
	// holds the gate when the real result doesn't match what was
	// declared — the structural answer to an agent that confabulates "I
	// verified it" (runs the command its change depends on, sees it
	// error, then claims it confirmed the command works, and ships a
	// feature wired to a flag that doesn't exist). A declared check
	// catches that deterministically — nobody asked the agent, the
	// command either passes or it doesn't. Run-level (the checks describe the whole
	// change), so it escalates every per-project verdict + the aggregate,
	// same shape as the gates above.
	if len(run.Task.VerifyChecks) > 0 {
		timeStep("verify-checks", func() {
			fails := runVerifyChecks(ctx, run.Task.VerifyChecks, mainRepo)
			if len(fails) == 0 {
				return
			}
			reason := "machine verify-check(s) failed: " + strings.Join(fails, " | ")
			for i := range states {
				s := &states[i]
				if s.captureFailed || s.gateSkipped || len(s.newLatest) == 0 {
					continue
				}
				if s.verdict.Verdict == safetygate.Auto {
					s.verdict.Verdict = safetygate.Review
				}
				s.verdict.Reasons = append(s.verdict.Reasons, reason)
			}
			if verdict.Verdict == safetygate.Auto {
				verdict.Verdict = safetygate.Review
			}
			verdict.Reasons = append(verdict.Reasons, reason)
		})
	}

	// The deterministic rename-completeness gate that used to live here
	// (RenameResiduals + the gateRenameResiduals payload) was removed.
	// It tried to catch incomplete renames by sweeping the codebase for
	// shape-matching tokens, but "did this rename complete?" is a
	// semantic, open-ended question — no finite regex covers it, and
	// the one shipped here false-positived on every Go selector
	// expression (b.WriteByte, t.Errorf, time.Now). The audit model in
	// gatereview/review.go now covers incomplete-rename detection by
	// reading the diff against the user's intent. See
	// project_rename_gate_deletion in user memory for the full rationale.

	run.Verdict = &workspace.IntegrationDecision{
		Verdict:     string(verdict.Verdict),
		BlastRadius: verdict.BlastRadius,
		Reasons:     verdict.Reasons,
		Touches:     verdict.Touches,
	}

	// Tag the new snap so `kai gate list` can find held integrations
	// later. We mirror the same payload keys integrateInternal writes
	// (gateVerdict, gateReasons, etc.) so the kai gate commands work
	// without code changes.
	// Per-project snapshot decoration. Each project's new snapshot
	// gets ITS OWN gate-verdict payload so that running `kai gate
	// list` from any project's directory shows that project's
	// verdict — not the primary's verdict copied across. See
	// decorateProjectSnap for the metadata schema (matches the
	// previous single-project shape exactly; just looped over
	// states).
	for i := range states {
		decorateProjectSnap(&states[i], run)
	}

	if verdict.Verdict == safetygate.Auto {
		// Auto-approve: each project's new snap.latest stays advanced.
		// Reporting just primary's ref is back-compat; sibling projects'
		// snap.latest were already advanced by their per-project capture
		// and stay there.
		run.AdvancedRefs = []string{"snap.latest"}
		return
	}

	// Held: aggregate verdict is non-Auto. Roll EVERY touched project's
	// snap.latest back to its prevLatest so the workspace state across
	// all projects matches the gate's decision atomically. The new
	// snapshots stay in each project's DB tagged for review; nothing
	// is lost — `kai gate diff <held-id>` from each project still
	// shows what the agent attempted.
	//
	// All-or-nothing semantics. If any project's verdict was non-Auto,
	// every project rolls back — matches how a PR-style workflow
	// behaves (the whole change lands or doesn't) and avoids the
	// surprising partial-apply state where a sibling's clean edits
	// stayed advanced while the primary's didn't. Confirmed in
	// v0.31.4's verdict-aggregation tests; this is just the
	// corresponding rollback fanout.
	//
	// Working tree files are NOT rolled back here. The legacy single-
	// project behavior leaves the user's working tree dirty even on a
	// Held verdict (per the merge-first architectural pattern); we
	// preserve that for now. The build-fix path in build_fix.go does
	// its own restoreWorkingTreeToSnapshot when a terminal build
	// failure exhausts the fix loop — that's the one place where
	// working-tree rollback DOES happen. Aligning the two paths is
	// queued under Tier 4 (architectural review-then-merge).
	rolled := []string{}
	for i := range states {
		s := &states[i]
		if s.captureFailed || s.gateSkipped {
			continue
		}
		if len(s.prevLatest) == 0 {
			// First snapshot in this project's DB; no prior pointer
			// to roll back to. Leave snap.latest at the new (held)
			// snap. Held snapshots are still discoverable via
			// `kai gate list`; this just can't undo the advance.
			continue
		}
		if err := ref.NewRefManager(s.db).Set("snap.latest", s.prevLatest, ref.KindSnapshot); err != nil {
			absorbTrace(run.Task.Name, fmt.Sprintf(
				"project %q: snap.latest rollback failed: %v (held snapshot remains tagged)", s.target.name, err))
			continue
		}
		rolled = append(rolled, s.target.name)
	}
	if len(rolled) > 0 {
		absorbTrace(run.Task.Name, fmt.Sprintf(
			"held: rolled back snap.latest in %d project(s): %s",
			len(rolled), strings.Join(rolled, ", ")))
	}
	// Avoid unused-import in builds where util is otherwise unused.
	_ = util.BytesToHex
}

// preflightSpawn does a single throw-away `kai spawn` against a
// temp dir, then immediately despawns. The point is to surface
// snapshot/object-store problems with one clear error instead of
// letting the parallel fan-out below fail N times with the same
// message — a stale or partial .kai/objects/ store typically takes
// down every agent identically and looks like four bugs when it's
// really one.
//
// Cost: a few hundred milliseconds in the happy path (one spawn +
// one despawn against a CoW workspace). We accept that overhead in
// exchange for a 4× clearer error message in the bad path.
//
// We don't use the first task's name because spawn's allocator
// might not collide with the real spawn for that task — pick a
// dedicated "preflight" name to keep the namespaces separate.
func preflightSpawn(ctx context.Context, cfg Config, mainRepo string) error {
	if mainRepo == "" {
		return nil // nothing to verify against
	}
	// Short-circuit: if mainRepo isn't a kai repo, every spawn will
	// fail with the same "not in a kai repo" error. Surface it once
	// here with the actionable hint instead of round-tripping through
	// a subprocess just to hear the same news.
	// Honor both placements kaipath understands: .kai/ (legacy) and
	// .git/kai/ (preferred for git repos — git auto-ignores it). The
	// previous check was .kai-only, which spuriously failed inside any
	// fresh git-repo init (moby, kubernetes, etc.).
	resolved := kaipath.Resolve(mainRepo)
	if _, err := os.Stat(resolved); os.IsNotExist(err) {
		return fmt.Errorf("orchestrator preflight: not in a kai repo (no %s) — run `kai init` first", resolved)
	}
	dir := fmt.Sprintf("%s%s-preflight-%d", cfg.SpawnPrefix, "kai", time.Now().UnixNano())
	// --sync none: orchestrator spawns are ephemeral local workspaces.
	// They don't need a remote round-trip and shouldn't fail when the
	// repo has no `kai remote` configured (the default for local-only
	// dogfooding setups).
	c := exec.CommandContext(ctx, kaiBinary(cfg), "spawn", dir, "--agent", "preflight", "--sync", "none")
	c.Dir = mainRepo
	out, err := c.CombinedOutput()
	if err != nil {
		// Best-effort cleanup of a partial spawn dir (the spawn
		// command may have created the dir before failing). Errors
		// here are swallowed — leaking a /tmp/kai-*-preflight-* dir
		// is annoying but not a failure mode worth surfacing.
		_ = os.RemoveAll(dir)
		hint := classifySpawnFailure(string(out))
		if hint != "" {
			// In-process self-heal: instead of bubbling a recoverable
			// failure up to the TUI's auto-repair (which surfaces
			// "Reindexing the workspace…" and forces the user to
			// re-issue their request), run `kai capture` here, retry
			// the spawn once, and only escalate if the retry still
			// fails. Two failure shapes qualify:
			//
			//   - "no snapshot to spawn from": fresh `.kai/` has no
			//     snapshots; the first-run path. capture creates the
			//     baseline snapshot.
			//
			//   - "missing blobs": the snapshot in DB references
			//     blobs that aren't in `.kai/objects/` (a partial
			//     pull, an interrupted gc, or — most commonly — a
			//     stat-cache "no change" path that skipped writing a
			//     blob whose file later got wiped). capture's
			//     working-tree-blob recovery (snapshot.go:106) re-
			//     materializes the blob from the file's content if
			//     the working-tree hash still matches.
			//
			// Verified May 2026: user reported "constantly needs
			// snapshot rebuilding" — preflight kept failing with
			// missing-blobs, auto-repair kept healing it, user kept
			// having to re-press "go". Doing the same heal here
			// closes the loop in one step. The dump-digests log
			// above gives us forensic visibility into WHICH blobs
			// were missing the first time the heal fires (so we can
			// fix the root cause separately).
			recoverable := strings.Contains(hint, "no snapshot to spawn from") ||
				strings.Contains(hint, "missing blobs")
			if recoverable {
				captureMsg := "auto: initial snapshot"
				if strings.Contains(hint, "missing blobs") {
					captureMsg = "auto: rebuild missing blobs"
					// Dump the spawn output to the local error log
					// so we can later identify which digest was
					// missing at the moment of the heal. Bounded
					// (truncate at 8KB) — full payload is in the
					// transient stderr already.
					logMissingBlobsContext(mainRepo, string(out))
				}
				cap := exec.CommandContext(ctx, kaiBinary(cfg), "capture", "-m", captureMsg)
				cap.Dir = mainRepo
				if capOut, capErr := cap.CombinedOutput(); capErr == nil {
					retryDir := fmt.Sprintf("%s%s-preflight-%d", cfg.SpawnPrefix, "kai", time.Now().UnixNano())
					retry := exec.CommandContext(ctx, kaiBinary(cfg), "spawn", retryDir, "--agent", "preflight", "--sync", "none")
					retry.Dir = mainRepo
					if _, retryErr := retry.CombinedOutput(); retryErr == nil {
						despawn := exec.CommandContext(ctx, kaiBinary(cfg), "despawn", retryDir, "--force")
						despawn.Dir = mainRepo
						_ = despawn.Run()
						return nil
					}
					_ = os.RemoveAll(retryDir)
				} else {
					_ = capOut
				}
			}
			// Hint explains the cause in one line; suppress the raw
			// spawn output (which contains the same error nested 2-3
			// times because of kai's own error-wrapping). The user
			// has the actionable fix; the noise just makes it harder
			// to read.
			return fmt.Errorf("orchestrator preflight: %s", hint)
		}
		// Unknown failure — surface the raw output so we don't hide
		// an unfamiliar error behind a generic message.
		return fmt.Errorf("orchestrator preflight: spawn check failed: %w\n\n%s",
			err, strings.TrimSpace(string(out)))
	}
	// Successful preflight — clean up the throwaway workspace
	// before the real fan-out starts to free disk + the spawn slot
	// in the snapshot graph.
	despawn := exec.CommandContext(ctx, kaiBinary(cfg), "despawn", dir, "--force")
	despawn.Dir = mainRepo
	_ = despawn.Run()
	return nil
}

// logMissingBlobsContext writes the missing-blob digest references
// from a failed spawn's stderr to <kaiDir>/missing-blobs.log so we
// can correlate root causes after-the-fact. Best-effort: silent on
// error (this is forensic noise, not a load-bearing path).
//
// The spawn command emits one or more lines of the form
// `open .kai/objects/<digest>: no such file or directory`. We
// extract the digests, deduplicate, and append a timestamped block
// per heal attempt so the log shows whether the same digest keeps
// reappearing (root cause is in the snapshot graph) vs. different
// digests on different heals (something is dropping new blobs).
func logMissingBlobsContext(mainRepo, spawnOutput string) {
	kaiDir := filepath.Join(mainRepo, ".kai")
	if _, err := os.Stat(kaiDir); err != nil {
		return
	}
	// Extract `<digest>` from `.kai/objects/<digest>:` substrings.
	// Digests are 64 hex chars; we keep this regex-free since the
	// path shape is fixed.
	const marker = ".kai/objects/"
	seen := map[string]bool{}
	var digests []string
	for i := 0; i < len(spawnOutput); {
		idx := strings.Index(spawnOutput[i:], marker)
		if idx < 0 {
			break
		}
		start := i + idx + len(marker)
		end := start
		for end < len(spawnOutput) && isHexDigit(spawnOutput[end]) {
			end++
		}
		if end-start == 64 {
			d := spawnOutput[start:end]
			if !seen[d] {
				seen[d] = true
				digests = append(digests, d)
			}
		}
		i = end
		if i <= start {
			i = start + 1
		}
	}
	if len(digests) == 0 {
		return
	}
	out := strings.Builder{}
	fmt.Fprintf(&out, "─── %s ─── %d missing digest(s) before heal:\n",
		time.Now().UTC().Format(time.RFC3339), len(digests))
	for _, d := range digests {
		fmt.Fprintf(&out, "  %s\n", d)
	}
	out.WriteString("\n")
	logPath := filepath.Join(kaiDir, "missing-blobs.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(out.String())
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// classifySpawnFailure inspects spawn's combined output for known
// failure modes and returns a one-line user-facing hint. Returns ""
// when the failure doesn't match a known pattern; the caller then
// surfaces the raw output verbatim so we never hide an unfamiliar
// error.
func classifySpawnFailure(output string) string {
	switch {
	case strings.Contains(output, "no such file or directory") &&
		strings.Contains(output, ".kai/objects/"):
		return "object store is missing blobs the snapshot references — run `kai capture` to rebuild, or `rm -rf .kai && kai init && kai capture` for a clean slate."
	case strings.Contains(output, "snap.latest"),
		strings.Contains(output, "not found: @snap:last"),
		strings.Contains(output, `resolving --from "@snap:last"`):
		return "no snapshot to spawn from — run `kai capture` first."
	case strings.Contains(output, "permission denied"):
		return "spawn dir is unwritable — check filesystem permissions on the spawn prefix."
	case strings.Contains(output, "not in a kai repo"):
		return "not in a kai repo — run `kai init` in this directory first."
	case strings.Contains(output, "--sync full requires a remote"):
		return "spawn was invoked with `--sync full` but the repo has no remote configured — orchestrator spawns should pass `--sync none` (this is a kai bug; please report)."
	}
	return ""
}

// spawnFor invokes `kai spawn` for one task. We parse the output via
// the --json flag rather than scraping human-readable text. Returns
// (spawnDir, workspaceName, error).
func spawnFor(ctx context.Context, taskName string, cfg Config, mainRepo string) (string, string, error) {
	// Use one path per task so the workspace name is predictable
	// (workspaceNameFor in cmd/kai/spawn.go always emits "spawn-N"
	// for slot N within a single `kai spawn` invocation; we always
	// invoke with count=1 so N=1 every time, but the directory name
	// keeps tasks distinct).
	dir := fmt.Sprintf("%s%s-%d", cfg.SpawnPrefix, taskName, time.Now().UnixNano())
	c := exec.CommandContext(ctx, kaiBinary(cfg), "spawn", dir, "--agent", taskName, "--sync", "none")
	c.Dir = mainRepo
	out, err := c.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("kai spawn: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Workspace name inside the spawn dir is always "spawn-1"
	// (workspaceNameFor in cmd/kai/spawn.go). Hardcoding here is
	// brittle; if that helper ever changes we'll learn quickly via
	// the integrate step failing to find the workspace.
	return dir, "spawn-1", nil
}

// runIn execs a child command in the given directory and discards
// its output. We don't need the output for the orchestrator's own
// flow — push/pull failures show up in the returned error.
func runIn(ctx context.Context, dir, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s in %s: %w: %s", name,
			strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runInWithEnv is runIn with additional env vars appended to the
// subprocess's environment. Used by paths that need to gate
// behavior in the child (e.g. the pre-spawn capture skipping the
// change-summary phase via KAI_CAPTURE_SKIP_SUMMARY).
func runInWithEnv(ctx context.Context, dir string, env []string, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s in %s: %w: %s", name,
			strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// checkRepoDBAlignment verifies that the on-disk kai directory
// resolved from mainRepo matches the directory the db handle was
// opened from. Returns a typed error with both paths and a fixable
// hint when they diverge — see Execute's call site for the full
// rationale.
//
// We compare the parents of (a) kaipath.Resolve(mainRepo) and (b)
// filepath.Dir(db.ObjectsDir()), each canonicalized via filepath.Abs
// + EvalSymlinks where available so the check tolerates symlinks
// without flagging them as a divergence. When EvalSymlinks fails for
// a non-existence reason we fall back to the unresolved path; we'd
// rather skip the guard than block on a transient FS hiccup.
func checkRepoDBAlignment(mainRepo string, db *graph.DB) error {
	if mainRepo == "" || db == nil {
		return nil
	}
	expectedKaiDir := kaipath.Resolve(mainRepo)
	dbObjectsDir := db.ObjectsDir()
	if dbObjectsDir == "" {
		return nil // can't verify; let downstream decide
	}
	dbKaiDir := filepath.Dir(dbObjectsDir)

	expC := canonicalPath(expectedKaiDir)
	dbC := canonicalPath(dbKaiDir)
	if expC == dbC {
		return nil
	}
	return fmt.Errorf(
		"orchestrator: working directory and graph DB don't agree on which kai project to use\n"+
			"  cwd resolves kai dir to: %s\n"+
			"  db handle is opened at:  %s\n\n"+
			"This usually means your multi-root config picked a different project as primary "+
			"than the directory you're running from. Two ways to fix:\n"+
			"  1. cd into the project that owns the populated DB (the one listed under \"db handle\" above), or\n"+
			"  2. mark the directory you want as primary by adding `pinned: true` to its entry in kai.projects.yaml.",
		expC, dbC)
}

// canonicalPath returns an absolute, symlink-resolved path. Falls
// back to the input on any error so the alignment check never
// crashes a run because of a stat hiccup.
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

// resolveLatestSnap reads snap.latest from the refs table. We don't
// import internal/ref to keep this package's dependency surface
// small — one short SQL query is cheaper than another import edge.
func resolveLatestSnap(db *graph.DB) ([]byte, error) {
	row := db.QueryRow(`SELECT target_id FROM refs WHERE name = 'snap.latest'`)
	var id []byte
	if err := row.Scan(&id); err != nil {
		return nil, fmt.Errorf("snap.latest not found: %w", err)
	}
	if len(id) == 0 {
		return nil, fmt.Errorf("snap.latest is empty")
	}
	return id, nil
}

// copyCheckpoints moves authorship checkpoints from spawnDir's kai
// directory to mainDir's, so that the next `kai capture` in main
// can consolidate them into the snapshot's authorship index.
// Without this, `kai blame` returns "no authorship data" for files
// the orchestrator's spawned agents edited — the checkpoints exist
// but live in /tmp/kai-N/.../checkpoints/ where main's capture
// never looks.
//
// Best-effort: if the source dir doesn't exist or any individual
// file fails, we skip rather than fail the whole integration.
// Each spawn's checkpoints land in a per-agent subdir keyed by the
// task name (set in the CheckpointWriter constructor) so multiple
// concurrent spawns don't collide.
func copyCheckpoints(spawnDir, mainDir string) error {
	srcRoot := filepath.Join(kaipath.Resolve(spawnDir), "checkpoints")
	dstRoot := filepath.Join(kaipath.Resolve(mainDir), "checkpoints")

	info, err := os.Stat(srcRoot)
	if err != nil || !info.IsDir() {
		return nil // no checkpoints to migrate, fine
	}

	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return fmt.Errorf("preparing dst checkpoints dir: %w", err)
	}

	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries instead of aborting
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return nil
		}
		dst := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			_ = os.MkdirAll(dst, 0o755)
			return nil
		}
		// Copy the checkpoint file. Skip on read failure — the
		// next agent's checkpoint will still land cleanly.
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		_ = os.WriteFile(dst, data, 0o644)
		return nil
	})
}

// precheckPrereqs verifies the preconditions an orchestrator
// run depends on before we shell out to `kai spawn`. Returns
// a typed, user-actionable error for each known-bad state so
// the classifier can render a clean message instead of the
// raw spawn shell-out failure.
//
// Three checks:
//
//   - mainRepo has a .kai/ directory with db.sqlite. Without
//     it, `kai spawn` errors with "not in a kai repo: run
//     `kai init` first" — opaque to the user who already ran
//     `kai code` from this dir, expecting the TUI to know.
//
//   - the snapshot log is non-empty. Without it, the resolver
//     for @snap:last falls through and `kai spawn` errors
//     with "not found: @snap:last~0" — confusing because the
//     ~0 is the resolver's canonical representation, not
//     anything the user typed.
//
//   - if cfg eventually requires a remote (sync=full path), it
//     exists in the project. We don't have a Config field for
//     sync mode here yet, so we skip this branch — `kai spawn`
//     itself surfaces the remote-missing error verbatim
//     (already in classifySpawnFailure as of May 2026), and a
//     full prereq check would require teaching the
//     orchestrator about sync mode. Future fix.
//
// The error strings deliberately echo phrases the classifier
// already matches, so a recurring failure produces a stable
// headline instead of "Something unexpected."
func precheckPrereqs(mainRepo, kaiDir string, db *graph.DB) error {
	if mainRepo == "" {
		return fmt.Errorf("orchestrator: empty mainRepo")
	}

	// 1. .kai/db.sqlite must exist.
	if kaiDir == "" {
		// Caller should always provide it, but be defensive.
		kaiDir = filepath.Join(mainRepo, ".kai")
	}
	if _, err := os.Stat(filepath.Join(kaiDir, "db.sqlite")); err != nil {
		return fmt.Errorf("orchestrator preflight: not in a kai repo: run `kai init` first (looked in %s)", kaiDir)
	}

	// 2. At least one snapshot must exist.
	if db != nil {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM nodes WHERE kind='Snapshot'`).Scan(&count); err == nil {
			if count == 0 {
				return fmt.Errorf("orchestrator preflight: no snapshots: run `kai capture` first to create one")
			}
		}
		// Query failure is non-fatal here — preflightSpawn
		// will still catch a broken DB.
	}

	return nil
}

// scopeAwareReadStreak returns the soft/hard read-streak thresholds
// the runner should use for an agent with the given file-count
// scope. Planner-declared scope drives this: a single-file task has
// no legitimate reason to do 10 turns of recon before its first
// edit, while a multi-file refactor needs room. fileCount==0 (the
// planner declined to scope) falls through to the runner default.
//
// Tuning rationale from the 2026-05-13 dogfood:
//   - 1 file:   nudge at 2, block at 5. The failing config-show run
//               touched exactly one file (main.go) yet spent 30+
//               turns exploring before its first (broken) edit.
//   - 2-4:     3 / 7. Modest tightening — most "add a flag" or
//               "register a subcommand" work falls in this band.
//   - 5+:      use default (5 / 10). Real refactors need recon.
func scopeAwareReadStreak(fileCount int) (soft, hard int) {
	switch {
	case fileCount == 1:
		return 2, 5
	case fileCount >= 2 && fileCount <= 4:
		return 3, 7
	default:
		return 0, 0 // 0 → runner falls back to its default thresholds
	}
}

// demoteAutoPromotedSiblings enforces plan-level atomic integrate.
// If any run in the slice ended in a non-Auto state — ExitErr,
// IntegrateErr, or a Held/Review verdict — every Auto-promoted run
// in the same plan is demoted to Review with a reason naming the
// failing peer(s). The DB snapshots stay tagged; the user reviews
// the whole plan's diff together instead of discovering a half-
// landed change later. No-op when every run is Auto (the plan
// landed cleanly) or when no run is Auto (nothing to demote).
//
// Why demote rather than block: the user may still want to inspect
// the working tree and selectively keep the auto-promoted file
// changes. Marking them Review surfaces the situation in `kai gate
// list` without discarding work; outright Block would force a
// re-run.
func demoteAutoPromotedSiblings(runs []AgentRun) {
	if len(runs) <= 1 {
		return
	}
	var failingPeers []string
	for _, r := range runs {
		switch {
		case r.ExitErr != nil:
			failingPeers = append(failingPeers, fmt.Sprintf("%s (agent failed)", r.Task.Name))
		case r.IntegrateErr != nil:
			failingPeers = append(failingPeers, fmt.Sprintf("%s (integrate failed)", r.Task.Name))
		case r.Verdict != nil && r.Verdict.Verdict != string(safetygate.Auto):
			failingPeers = append(failingPeers, fmt.Sprintf("%s (verdict=%s)", r.Task.Name, r.Verdict.Verdict))
		}
	}
	if len(failingPeers) == 0 {
		return
	}
	reason := fmt.Sprintf("atomic integrate: held for review because sibling agent(s) did not land cleanly: %s", strings.Join(failingPeers, "; "))
	for i := range runs {
		r := &runs[i]
		if r.Verdict == nil || r.Verdict.Verdict != string(safetygate.Auto) {
			continue
		}
		r.Verdict.Verdict = string(safetygate.Review)
		r.Verdict.Reasons = append(r.Verdict.Reasons, reason)
	}
}

// integrateDiag appends one line per integrate-phase step to
// ~/.kai/integrate-diag.log. Round-21 retest (2026-05-13) showed the
// post-LLM integrate phase taking ~4 minutes when accountable steps
// timed individually totaled ~10s — but the user had to force-quit
// before the orchestrator finished, so the slow step was never named.
// This logger surfaces start→end per step so the next observed hang
// can be pinned to its culprit. Best-effort writes; any I/O error is
// silently ignored. Remove this once the slow step is fixed.
func integrateDiag(agent, step string, took time.Duration) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".kai", "integrate-diag.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  agent=%-24s  step=%-22s  took=%s\n",
		time.Now().Format("2006-01-02T15:04:05.000"), agent, step, took)
}

// absorbTarget is one (spawn subdir → real project dir) pair the
// absorb step applies. A multi-root run produces one per project the
// agent may have edited.
type absorbTarget struct {
	name     string // project basename ("" for a single-root run)
	spawnDir string // the spawn-side tree to read edits from
	mainDir  string // the real project dir to apply them into
}

// absorbTrace appends a free-form line about the absorb step to
// ~/.kai/integrate-diag.log. The 2026-05-15 dogfood motivated this:
// absorb silently discarded a worker's real edit (it lived in a
// non-primary project subdir) and the only way to know was to dig
// through a not-yet-cleaned spawn dir. Every expensive agent run that
// fails this way costs real money; a per-project absorb log line
// makes the next failure diagnosable from disk, with no re-run.
func absorbTrace(agent, msg string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(home, ".kai", "integrate-diag.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  agent=%-24s  absorb: %s\n",
		time.Now().Format("2006-01-02T15:04:05.000"), agent, msg)
}
