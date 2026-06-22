// Package agentprompt builds the final prompt string handed to an
// agent process (Claude Code, Cursor, etc.) when the orchestrator
// launches it inside a spawned workspace.
//
// The contract: planner.AgentTask says WHAT the agent should do.
// agentprompt.Build says HOW to tell the agent. Splitting these
// concerns means we can iterate on prompt phrasing without touching
// the planner's LLM call, and vice versa.
//
// Pure function, no side effects, no I/O. Easy to unit-test against
// golden files.
package agentprompt

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kai/internal/planner"
)

// RootInfo describes one project root in a multi-root workspace.
// Surfaced in the prompt so the agent can refer to projects by name
// and route tool calls to the right one.
type RootInfo struct {
	// Name is the user-facing label (e.g. "kai-cli", "kai-server").
	// Stable across launches; safe for the agent to embed in
	// reasoning ("the change in kai-server requires...").
	Name string

	// Path is the absolute project root. The agent uses this to
	// disambiguate when the same relative path exists in multiple
	// projects.
	Path string
}

// Context is the per-repo information that's the same across all
// agents in a single plan. The orchestrator builds it once and passes
// it to Build for each AgentTask.
type Context struct {
	// RepoRoot is an absolute path; surfaced in the prompt so the
	// agent has an anchor when reading or writing files. In a
	// multi-root workspace this is the discovery root (the directory
	// the user invoked `kai code` in); per-project roots are listed
	// in Roots.
	RepoRoot string

	// Roots lists every active project in a multi-root workspace.
	// Empty or single-entry → behave as before. Two or more → Build
	// renders a "Project layout" block so the agent knows what
	// projects exist and where they live.
	Roots []RootInfo

	// Language is a short identifier ("go", "python", "ts") used to
	// hint the agent at its environment. Best-effort; safe to leave
	// empty if unknown.
	Language string

	// GraphContext is a pre-rendered string the orchestrator builds
	// from the semantic graph: a few lines per file in the agent's
	// allowed set listing callers and dependents at depth 1. Goes
	// in verbatim — agentprompt doesn't query the graph itself.
	GraphContext string

	// Protected globs from the safety gate config. Surfaced in
	// every prompt so the agent knows the gate's rules and doesn't
	// try to "fix" something it shouldn't.
	Protected []string

	// ModuleRoots lists directories containing language manifests
	// (go.mod, Cargo.toml, package.json, pyproject.toml) discovered
	// under RepoRoot. Each Dir is relative to RepoRoot ("" means
	// the manifest sits at the root). Build composes a hint telling
	// the agent to `cd` into the manifest dir before running build
	// or test commands — without this, agents run `go build ./...`
	// from the repo root in monorepos where go.mod lives a level
	// down (kai-cli/go.mod) and Go errors out with "directory
	// prefix … does not contain main module."
	//
	// Empty slice → no hint rendered (assume root-level project).
	// Populated by the caller (TUI/headless) using DetectModuleRoots.
	ModuleRoots []ModuleRoot
}

// ModuleRoot describes one language manifest discovered in the repo.
// Dir is relative to Context.RepoRoot; Manifest is the file name that
// triggered detection (e.g. "go.mod"). One ModuleRoot per manifest.
type ModuleRoot struct {
	Manifest string
	Dir      string
}

// Build composes the final prompt. Sections are clearly delimited so
// agents (which are LLMs and parse plain prose, not structured input)
// can navigate the prompt without confusion.
func Build(task planner.AgentTask, ctx Context) string {
	var b strings.Builder

	// Identity: tell the agent who it is and what it's doing. Short
	// and concrete is better than long and inspirational.
	fmt.Fprintf(&b, "You are agent %q.\n\n", task.Name)
	fmt.Fprintf(&b, "Task: %s\n\n", strings.TrimSpace(task.Prompt))

	// Acceptance criteria: the INTENT the change must satisfy, not the
	// steps. The EDIT CHECKLIST in the task tells you what to type;
	// these tell you what "done" actually means. A change that ticks
	// every checklist box but fails a criterion is NOT done — the
	// criterion is the real goal. Run the change against each one
	// before you finish.
	if len(task.AcceptanceCriteria) > 0 {
		b.WriteString("Done means ALL of these hold (the intent — verify your change against each before finishing):\n")
		for _, c := range task.AcceptanceCriteria {
			fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(c))
		}
		b.WriteByte('\n')
	}

	// EDIT BUDGET. Mirror of the planner's EXPLORATION BUDGET, framed
	// for the worker: the worker's deliverable is tool calls (edit /
	// write), not certainty. Without this, opus-4-6 burns 30-40 turns
	// re-reading the same regions to "confirm" before editing, then
	// either hits the convergence wall or exits after a single edit
	// thinking it's done. The worker has caching too, so re-reading
	// is essentially free — the model's instinct to verify is fine,
	// but it should verify cheaply and commit early.
	b.WriteString(`EDIT BUDGET: You have ~10 read-tool calls (view / kai_grep / kai_context / kai_symbols / kai_callers / kai_dependents) before you should start editing. Spend them on UNIQUE questions — re-reading the same file/region is free (cached) and provides no new signal, so don't burn a turn on it. After ~10 reads, switch to edit/write and converge. The deliverable is tool calls that change the file, not prose certainty.

EXEMPLAR-FIRST for "add a thing like an existing thing" work (new tool, new view, new endpoint, new handler, new command): find the closest existing exemplar with ONE kai_grep, view it ONCE in full, then kai_grep for its registration site. That is the entire exploration. Do NOT read the surrounding package top-to-bottom — the exemplar tells you the contract, the registration grep tells you where the new one slots in.

NEVER view the same file more than once. If you need more of a file than your first view returned, your first view should have been wider. Sequential slices of the same file ("view L1-100", "view L100-300", "view L300-500") are a single wasted decision against the budget: pick a slice that covers what you need, view the whole file, or commit to the edit with what you've seen. The cache makes repeat-reads cheap in TOKENS but they still cost a TURN against the budget and they don't tell you anything new.

NEVER re-search a term you already searched, even with a different alternation. If a kai_grep returned hits, refine by narrowing to a path (kai_grep "X" in pkg/) instead of re-spelling the term globally. If it returned nothing, the answer is "this term doesn't exist"; don't re-spell it hoping for a different result.

SECURITY CHECKLIST when adding or modifying functionality that touches the outside world. Before you finish, verify and address each that applies:
  - NEW SECRET / CREDENTIAL — confirm it's read from env or config (not hard-coded), redacted in logs, and rejected with a clear error when empty (do not let a missing key fail later as a confusing upstream 4xx).
  - OUTBOUND NETWORK CALL — explicit timeout (not "the default"), explicit response-body size cap (io.LimitReader or equivalent), and 4xx/5xx handling that surfaces a clean error instead of retry-storming.
  - USER INPUT INTO SHELL / SQL / FS PATHS — validate or quote; never assemble a shell line with raw user input.
  - PARSE UNTRUSTED FORMAT (network XML/HTML/zip/yaml) — use a parser with size/entity limits; never feed unbounded network bytes into a permissive parser.
  - CONFIGURABLE BASE URL OR ENDPOINT — generally a smell. The provider has one endpoint; making it configurable invites phishing-proxy / staging-leak misuse. Default to a constant in code; add config only when a concrete need is named.
These are EDIT-TIME concerns, not "someone reviewing this will catch it." If the planner's task names new functionality and your implementation omits one of the above, you are NOT done.

Plan completion: if the task names multiple steps (add field, modify handler, add reset, update comment), complete ALL of them in this run. A single edit that addresses step 1 and exits is NOT done — it ships a broken intermediate state. If you genuinely cannot complete a step (need information you don't have), say "I'm blocked because <X>" and ask the specific question; do not silently exit with the work half-finished.

Renames and replacements: when you change a name or value (a model id, symbol, constant, import path, flag), replacing the exact string you searched for is NOT enough. The same thing is referred to in shorter, informal forms — an abbreviation, the name without its namespace prefix, a mention in a comment or doc. After the substitution, search again for the DISTINCTIVE FRAGMENTS of the old value — e.g. for a removed id "acme-co/Widget-9.9" also search "Widget-9.9" and "Widget" — not just the full string, and update every hit. The integration gate's audit step reads your diff against the user's stated intent and will flag surviving references; if it catches one, that's a real miss, not a false positive — finish the rename before promoting.

`)

	if ctx.RepoRoot != "" {
		fmt.Fprintf(&b, "Working directory: %s\n", ctx.RepoRoot)
	}
	if ctx.Language != "" {
		fmt.Fprintf(&b, "Primary language: %s\n", ctx.Language)
	}
	if ctx.RepoRoot != "" || ctx.Language != "" {
		b.WriteByte('\n')
	}

	// Sandbox capability disclosure. Worker agents always run inside a
	// CoW spawn workspace (see internal/orchestrator). The bash tool
	// REFUSES any `cd /absolute/path && ...` where the path is outside
	// the workspace; the file tools (write, edit) silently ignore paths
	// outside it. Without this paragraph in the prompt, agents that
	// hit either restriction loop forever trying to re-run a host-shell
	// task in a CoW sandbox that can't perform it (2026-05-24 install-
	// kai dogfood: 18+ identical `cd /Users/... && go build` retries).
	b.WriteString(`SANDBOX: You are running inside an isolated workspace at the Working directory above — a copy-on-write checkout, NOT the user's host filesystem. The repo's mirror is the workspace root; everything you read and write happens there. Writes outside the workspace are rejected (the bash tool will refuse ` + "`cd /absolute/outside/path && ...`" + ` loudly; file tools silently ignore outside paths). When you need to run a build/test/install command, use a relative path from the workspace root (e.g. ` + "`cd kai-cli && make install`" + `), or an absolute path that stays inside the workspace — never the user's host path you see in error messages.
HOST-ONLY TASKS: Installing a binary onto the user's PATH, modifying ` + "`~/`" + ` config, registering an MCP server, ` + "`sudo`" + ` actions, ` + "`brew install`" + `, anything that mutates the host system — these CANNOT be done from inside this workspace by design. If your task requires one of these, STOP and report exactly what command needs to run on the host. Do not loop trying to escape; the sandbox will keep refusing.

`)

	// Module roots: tell the agent where each language's manifest
	// lives so it doesn't run `go build ./...` from the repo root
	// when go.mod is at kai-cli/go.mod. Without this hint, agents
	// reliably try the root and Go reports "directory prefix …
	// does not contain main module," which the agent then tries to
	// work around instead of cd'ing one level down.
	if nested := nestedModuleRoots(ctx.ModuleRoots); len(nested) > 0 {
		b.WriteString("Build/test command roots (cd into these subdirectories before running `go build`, `go test`, `cargo`, `npm`, etc.):\n")
		for _, m := range nested {
			fmt.Fprintf(&b, "  - %s  (%s)\n", m.Dir, m.Manifest)
		}
		b.WriteByte('\n')
	}

	// Multi-root layout. Only render when there's actually more than
	// one project — for single-root workspaces the Working directory
	// line above is sufficient and adding a "Project layout" block
	// would be noise.
	if len(ctx.Roots) > 1 {
		b.WriteString("Project layout (multi-root workspace):\n")
		for _, r := range ctx.Roots {
			fmt.Fprintf(&b, "  - %s at %s\n", r.Name, r.Path)
		}
		b.WriteString(`
File-path convention: the prefix is the PROJECT NAME (the first identifier on each line above), NOT a subdirectory inside a project. A project may contain a subdirectory whose name LOOKS like another project (e.g. the "kai" project contains a "kai-cli" subdirectory) — the subdirectory name is NEVER a valid prefix. Always use the project name from the layout block.

Examples:
  ✓ view kai/kai-cli/internal/foo.go         — file in the kai project, kai-cli subdirectory
  ✓ view kai-server/kailab/api/routes.go     — file in the kai-server project, kailab subdirectory
  ✗ view kai-cli/internal/foo.go              — WRONG: "kai-cli" is a subdirectory of kai, not a project
  ✗ view /Users/.../kai-cli/foo.go            — WRONG: never use an absolute filesystem path

Scoping kai_grep / kai_files / kai_tree: pass the project name as path (path="kai-server"), or path="kai-server/<subdir>" to narrow within a project. Bare subdirectory names that aren't projects ("kai-cli", "kailab") will NOT scope correctly.

`)
	}

	// File boundaries: the planner's intent for what this agent
	// should and shouldn't touch. v1 has no sandbox enforcement;
	// the agent is expected to honor these via the prompt.
	if len(task.Files) > 0 {
		b.WriteString("Files you should focus on:\n")
		for _, p := range sortedCopy(task.Files) {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteByte('\n')
	}

	// DontTouch + Protected merged so the agent sees one forbidden
	// list rather than two slightly different ones. Dedup so a path
	// that's both DontTouch and Protected appears once.
	forbidden := mergeUnique(task.DontTouch, ctx.Protected)
	if len(forbidden) > 0 {
		b.WriteString("Files you must NOT modify:\n")
		for _, p := range forbidden {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteString("\nIf changing one of these is genuinely necessary, stop and explain why instead of editing it.\n\n")
	}

	if s := strings.TrimSpace(ctx.GraphContext); s != "" {
		b.WriteString("Graph context for the files in scope:\n")
		b.WriteString(s)
		if !strings.HasSuffix(s, "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	// Coordination + checkpointing hint. Agents that don't have the
	// kai MCP installed will skip the checkpoint instruction; that's
	// fine — the watcher catches file changes regardless.
	b.WriteString(`Coordination notes:
  - Other agents may be working in sibling workspaces; live sync keeps the graph current.
  - If the kai_checkpoint tool is available, call it whenever you finish a logical unit of work.
  - When your task is done, exit cleanly. The orchestrator will integrate your changes through the safety gate.

Exploration playbook — read this BEFORE you start poking around:

  Folder names lie. A feature called "the TUI" might live in cli/, frontend/,
  internal/views/, or somewhere named nothing like "tui". A directory called
  "auth" might be empty scaffolding while the real auth code is in middleware/.
  In a multi-root workspace this is even worse — the same concept might span
  multiple projects. NEVER conclude "X doesn't exist" from an empty kai_tree
  on a name-guess. The semantic graph is the authoritative answer; use it.

  When the user asks "where is X?" or "does Y exist?", explore by CONCEPT, not
  by directory name:

    1. kai_grep for symbols, imports, or distinctive strings related to X
       across ALL roots. Examples for "where is the TUI?":
         kai_grep "bubbletea"           — common TUI framework import
         kai_grep "tea.NewProgram"      — Bubble Tea entry point
         kai_grep "spinner.New"         — UI element you're asking about
       Examples for "where is auth?":
         kai_grep "Authorization"
         kai_grep "Bearer "
         kai_grep "func.*Authenticate"

    2. Once you have hits, kai_context(file) on the most promising one to
       see its top-level symbols + dependents. This tells you the shape of
       the area without reading the whole file.

    3. kai_callers(symbol) and kai_dependents(file) to walk outward from
       there — who uses this, who depends on this. THIS is the part filesystem
       tools cannot do; this is why the graph exists.

    4. kai_tree / kai_files only AFTER you have an anchor. Trees are useful
       for "what's around this file"; useless for "does this concept exist?"

  Anti-loop rule: if a tool call returns nothing useful, do not call the same
  tool with the same arguments again. Try a different tool, or a different
  query. Repeating an empty result is wasted tokens and gives the user the
  illusion of effort while making no progress. If after 2-3 distinct queries
  you still cannot find what was asked about, SAY SO honestly: "I searched
  for X, Y, and Z across all roots and found nothing matching" — do not
  fabricate a conclusion or assume the thing doesn't exist.

  Verify-vs-build: when the user says "confirm X works", "check that Y is
  wired up", "verify Z", they want investigation, NOT scaffolding. Do not
  offer to build something the user asked you to confirm — find it first.

Graph tools (in-process kai agent only — silently absent for external runners):
  - kai_grep(pattern, path?) — search file contents across all roots. Your
    primary "where is X?" tool. Cheap; use it freely. Walks every project
    in a multi-root workspace by default.
  - kai_context(file) — top-level symbols + depth-1 dependents. Cheap
    orientation step before view on large files.
  - kai_symbols(file) — top-level symbol list for a file.
  - kai_callers(symbol, file?) — who calls this function. Use BEFORE editing
    a function to see who depends on it.
  - kai_dependents(file) — files that import this file (depth 1). Use
    BEFORE editing a heavily-imported file.
  - kai_impact(target, function?, depth?) — blast-radius summary: combined
    callers + dependents with a risk classification (none/low/medium/high).
    One call answers "is this safe to change?" — strictly more useful than
    kai_callers + kai_dependents when you just need the verdict. depth=2
    pulls callers-of-callers for shared infrastructure.
  - kai_files(pattern) — list files by glob. Use AFTER you have a directional
    answer from grep, not as the first move.
  - kai_tree(path?) — directory tree. Useful for "what's around here" once
    you've localized; never useful as a first guess at where something lives.

Diff and repo state:
  - kai_diff(path?, base?) — semantic diff of working-tree changes against
    the last snapshot. PREFER this over bash git diff for inspecting what
    you or a prior agent changed — it's typed output, cheaper, and doesn't
    require shelling out. bash git diff is acceptable only when you need
    a comparison kai_diff doesn't support (e.g. diff against a specific
    commit SHA the graph doesn't carry).
  - kai_git_state() — branch, dirty flag, ahead/behind. PREFER over
    bash git status. Use this before you change anything to know what's
    already pending in the working tree.

Shell:
  - bash(command, timeout?) — run a shell command in the workspace. Output is capped at ~30 KB; redirect long output to a file and view it. Use this to run tests, lint, build commands while you work.

Prefer kai_ tools over git/bash equivalents whenever both exist (kai_diff
over git diff, kai_git_state over git status, kai_grep over bash grep,
kai_files over bash find). The kai_ tools return structured output, cost
fewer tokens to render, and are graph-aware. Round-22 dogfood: worker ran
"cd kai-cli && git diff HEAD -- cmd/kai/main.go | head -200" to inspect its
own changes when one kai_diff call would have answered the same question.
`)

	return b.String()
}

// mergeUnique returns the sorted union of two string slices.
func mergeUnique(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	for _, s := range b {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// sortedCopy returns a sorted copy so prompt output is deterministic.
// Determinism matters for golden-file tests and for cache-key stability
// if we ever start caching prompts.
func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// moduleManifests lists the file names we treat as a module root for
// the purposes of the "cd here before running build/test" hint.
// Order doesn't matter — every match becomes its own ModuleRoot.
var moduleManifests = []string{"go.mod", "Cargo.toml", "package.json", "pyproject.toml"}

// DetectModuleRoots walks repoRoot for known language manifests and
// returns one ModuleRoot per match. Prunes the same noise dirs as the
// orchestrator's findManifest helper so polyglot trees don't take
// seconds to scan.
//
// Manifests at repoRoot itself are returned with Dir="" — Build's
// renderer filters those out (no hint needed when the manifest is at
// the cwd the agent already has). Nested manifests get rel paths
// like "kai-cli" so the agent knows where to cd.
//
// Safe to call on an empty repoRoot — returns nil.
func DetectModuleRoots(repoRoot string) []ModuleRoot {
	if repoRoot == "" {
		return nil
	}
	manifestSet := make(map[string]struct{}, len(moduleManifests))
	for _, m := range moduleManifests {
		manifestSet[m] = struct{}{}
	}
	var out []ModuleRoot
	_ = filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dn := d.Name()
			// fixtures: vendored test corpora (kai-e2e/fixtures/...).
			// Their manifests aren't real project modules — listing
			// them in the prompt would mislead the agent into thinking
			// every fixture is a build target.
			if dn == ".git" || dn == "node_modules" || dn == "vendor" ||
				dn == "target" || dn == ".kai" || dn == "dist" || dn == "build" ||
				dn == "fixtures" || dn == "wailsjs" || dn == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := manifestSet[d.Name()]; !ok {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, filepath.Dir(path))
		if err != nil {
			return nil
		}
		if rel == "." {
			rel = ""
		}
		out = append(out, ModuleRoot{Manifest: d.Name(), Dir: rel})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir < out[j].Dir
		}
		return out[i].Manifest < out[j].Manifest
	})
	return dedupNestedManifests(out)
}

// dedupNestedManifests drops manifests that are nested under another
// manifest of the same kind. Example: keep kai-cli/go.mod; drop
// kai-cli/internal/foo/go.mod (Go workspace member that's already
// covered by the parent). Same-manifest only — a frontend's
// package.json nested under a Go module's go.mod stays, because they
// describe different toolchains.
//
// Assumes input is sorted by Dir (which means parents come before
// children lexicographically, since "a" < "a/b").
func dedupNestedManifests(in []ModuleRoot) []ModuleRoot {
	if len(in) <= 1 {
		return in
	}
	// Track the shortest prefix per manifest type seen so far.
	prefixes := make(map[string][]string)
	var out []ModuleRoot
	for _, m := range in {
		covered := false
		for _, p := range prefixes[m.Manifest] {
			if m.Dir == p {
				continue
			}
			if p == "" || strings.HasPrefix(m.Dir, p+"/") {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		prefixes[m.Manifest] = append(prefixes[m.Manifest], m.Dir)
		out = append(out, m)
	}
	return out
}

// nestedModuleRoots filters DetectModuleRoots output down to the ones
// that actually warrant a hint — i.e. manifests in a subdirectory.
// Root-level manifests (Dir=="") don't need a "cd here" instruction
// because the agent's working directory is already the repo root.
func nestedModuleRoots(roots []ModuleRoot) []ModuleRoot {
	var out []ModuleRoot
	for _, r := range roots {
		if r.Dir != "" {
			out = append(out, r)
		}
	}
	return out
}
