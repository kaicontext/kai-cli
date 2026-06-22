// Package planner turns a natural-language request plus the semantic
// graph into a structured WorkPlan. Phase 3's REPL routes unrecognized
// input through Plan(); the orchestrator consumes the resulting plan.
//
// This file holds the public types so other packages (agentprompt,
// orchestrator) can depend on them without depending on the LLM call
// machinery in plan.go.
package planner

// WorkPlan is the structured output of a single planner call. The
// orchestrator iterates Agents in parallel; RiskNotes is rendered to
// the user before they confirm "go".
type WorkPlan struct {
	// Summary is one line describing what the whole plan accomplishes.
	Summary string `json:"summary"`

	// Diagnosis is the Sherlock-Holmes-style "what's the problem,
	// why is it happening, what evidence" narrative the planner
	// emits BEFORE proposing the fix. Two or three sentences.
	// The user reads this before confirming "go" — without it
	// the plan reads like an opaque commit message and the user
	// has to trust the planner blindly. Optional: empty when the
	// request is fully obvious (an explicit "rename X to Y").
	//
	// Format: a brief narrative, not a bulleted list. Bullets
	// for evidence belong in RiskNotes.
	Diagnosis string `json:"diagnosis,omitempty"`

	// Approach is the one-line "how this fix works" — the bridge
	// between Diagnosis ("here's the problem") and Agents
	// ("here's the work"). Renders right after Diagnosis so the
	// user sees the reasoning chain at a glance: problem → fix
	// strategy → concrete agents.
	Approach string `json:"approach,omitempty"`

	// Agents is the work split. Empty means the request was too vague
	// to plan — Plan() returns an error in that case so callers don't
	// silently execute nothing.
	Agents []AgentTask `json:"agents"`

	// RiskNotes are advisory bullets the LLM flagged (e.g. "router.go
	// is called by 4 services"). Surfaced verbatim in the REPL.
	RiskNotes []string `json:"risk_notes,omitempty"`

	// Trivial marks a plan the planner — having explored the code —
	// judged small, localized, and low-risk enough to run without the
	// go/cancel/feedback confirm step (one file, a handful of lines).
	// The TUI auto-runs a trivial plan. Triage can only route on the
	// request text; the planner sets this once it has actually seen
	// the change is a one-liner.
	Trivial bool `json:"trivial,omitempty"`
}

// AgentTask is one agent's scoped assignment. Files / DontTouch are
// not enforced by a sandbox in v1 — they go into the agent's prompt
// and the agent is expected to honor them. Phase 3.x adds post-hoc
// verification; v1 trusts the prompt.
type AgentTask struct {
	// Name is a short identifier ("backend-api", "tests"). Used as
	// the agent's --agent label and in the spawn directory name.
	Name string `json:"name"`

	// Prompt is the human-readable description of the task, written
	// by the planner LLM. agentprompt.Build wraps this with file
	// boundaries and graph context to produce the final prompt.
	Prompt string `json:"prompt"`

	// Files lists the paths this agent should focus on (planner's
	// best guess). May be empty for agents whose work touches new
	// files exclusively.
	Files []string `json:"files,omitempty"`

	// DontTouch lists paths this agent must avoid. Typically a subset
	// of the gate's protected globs plus other agents' Files lists.
	DontTouch []string `json:"dont_touch,omitempty"`

	// Mode is the orchestrator's explicit mode override for this
	// agent (planning / coding / review / debug / conversation).
	// Empty / "unknown" leaves the agent in normal auto-detection.
	// Stored as a string here so the planner package doesn't pull
	// in the agent package; the agent runner maps it to agent.Mode.
	Mode string `json:"mode,omitempty"`

	// AcceptanceCriteria are 2-4 concrete, checkable statements of
	// what "done" means for this agent — the INTENT behind the
	// request, not a restatement of the EDIT CHECKLIST steps. A
	// criterion is something a reviewer can verify the finished
	// change against ("adding a sixth role model needs no edit to
	// Load()"), so a change that is mechanically correct but misses
	// the point can be caught. Threaded into the agent prompt (so
	// the agent knows the goal) and into the gate review (so the
	// reviewer audits against intent, not vibes). Optional — empty
	// for a request whose wording IS the full intent.
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`

	// VerifyChecks are MACHINE-CHECKABLE acceptance criteria the harness
	// runs itself after the edits land — not prose a verify agent (or the
	// executor) narrates and can confabulate satisfying. Each is a shell
	// command plus the result it must produce. Use them when the change
	// depends on an external contract (a CLI command/flag, an endpoint,
	// an output format): "the command exits 0 with these flags" is a
	// fact the harness confirms by running it, so an agent claiming "I
	// verified it" can't override reality. Prevents the failure where an
	// agent runs the command its change depends on, sees it fail, then
	// fabricates a verification and ships a feature wired to a flag that
	// doesn't exist. Optional; most tasks need none.
	VerifyChecks []VerifyCheck `json:"verify_checks,omitempty"`

	// Evidence carries the planner's "this is what I verified, and
	// where" notes so the executor doesn't have to re-derive the
	// reasoning behind the proposed edit. Capped at ~3 entries per
	// agent (1500 chars total — see EvidenceBlockMaxBytes) so the
	// executor's prompt stays focused; the entries are intended as
	// PRIOR that nudges, not as a transcript replacement.
	//
	// Why this exists (2026-05-26 spec #1): the vite-plugin-svelte
	// dogfood pinned the failure shape — planner correctly identified
	// the hardcoded option at preprocess.js:43; plan said "patch-
	// package this file"; executor (operating from the bare task
	// prompt with no view of the planner's evidence) fell back to
	// the lower-friction interpretation ("this is a tsconfig option,
	// I'll edit svelte.config.js"), did the wrong thing confidently.
	// The executor wasn't wrong so much as deliberately stripped of
	// the context the planner had; absent the REASON node_modules
	// was the target, the model's training prior ("config goes in
	// user config files") wins.
	Evidence []EvidenceEntry `json:"evidence,omitempty"`
}

// VerifyCheck is a harness-run acceptance check: a shell command and the
// outcome it must produce. The harness executes it after the agent's
// edits are captured; a mismatch holds the gate. Because the harness
// runs it (not the agent), no narrative — honest or confabulated — can
// satisfy it. At least one of ExpectExit / ExpectStdoutContains should
// be set; an all-zero check just asserts the command runs at exit 0.
type VerifyCheck struct {
	// Run is the shell command to execute in the workspace root.
	Run string `json:"run"`
	// ExpectExit, when non-nil, is the exit code the command must
	// return. Defaults to 0 when nil (the common "it must succeed").
	ExpectExit *int `json:"expect_exit,omitempty"`
	// ExpectStdoutContains, when non-empty, is a substring that must
	// appear in the command's combined output.
	ExpectStdoutContains string `json:"expect_stdout_contains,omitempty"`
	// Why is a one-line note on what this check proves (surfaced in the
	// hold reason when it fails).
	Why string `json:"why,omitempty"`
}

// EvidenceEntry is a single planner-cited location plus the
// planner's one-sentence "and this means…" annotation. The
// annotation is the load-bearing field — raw quotes without
// framing get skimmed past by executors that already have their
// own training priors. Pair the quote with the planner's reasoning.
type EvidenceEntry struct {
	// File is the workspace-relative path the planner cited.
	File string `json:"file"`

	// LineStart, LineEnd are the 1-based inclusive line range the
	// excerpt was sliced from. Used for drift detection (orchestrator
	// re-stats the file at spawn time and degrades the entry if its
	// content changed) and so the executor can re-read precisely
	// without guessing.
	LineStart int `json:"line_start"`
	LineEnd   int `json:"line_end"`

	// Excerpt is the literal file content for the cited range. Capped
	// at ~200 chars per entry; longer excerpts get truncated with an
	// ellipsis. The point is a recognizable quote, not the full body.
	Excerpt string `json:"excerpt"`

	// Annotation is the planner's one-sentence reasoning: "this
	// line hardcodes importsNotUsedAsValues after the user-options
	// spread, so the user's tsconfig override is silently
	// overwritten." Without the annotation, the excerpt reads as
	// trivia; with it, the executor sees WHY the planner cited this
	// location.
	Annotation string `json:"annotation"`

	// ContentHash is the BLAKE3 hex digest of File's contents at
	// the moment the planner captured the excerpt. Used at executor-
	// spawn time to detect drift: if the file's current hash
	// matches, evidence passes through unchanged; if not, the
	// orchestrator emits a degraded form ("evidence stale — file
	// changed since planning; re-read X:Y±20 before acting").
	// Optional — empty when the planner couldn't compute it (e.g.
	// the cited file is outside the workspace). Drift detection
	// skips entries with no hash.
	ContentHash string `json:"content_hash,omitempty"`
}

// EvidenceBlockMaxBytes caps the rendered EVIDENCE FROM PLANNING
// block in each agent's prompt. ~1500 chars empirically: enough
// for 3 sensible cited ranges with annotations, small enough that
// the executor's working window stays dominated by its own
// exploration. Bumping past this risks the executor treating
// evidence as authoritative transcript instead of as a prior.
const EvidenceBlockMaxBytes = 1500

// EvidencePerEntryMaxBytes caps individual excerpts. The full
// citation (file + line range) is always preserved; the EXCERPT
// gets truncated.
const EvidencePerEntryMaxBytes = 200
