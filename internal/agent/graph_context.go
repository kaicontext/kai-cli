package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/projects"
)

// graphContextInjector tracks which files have already had their
// graph relationships sent to the model so we don't repeat the same
// context block on every turn. The state lives on the runner stack;
// it doesn't survive across runs (resumed sessions re-inject from
// scratch on first turn — small redundancy in exchange for
// stateless persistence).
type graphContextInjector struct {
	graph     *graph.DB
	workspace string
	injected  map[string]bool // workspace-relative paths
	set       *projects.Set   // when non-nil + multi-root, overview covers every root
	// overviewSent flips true after buildBlock emits the one-time
	// project overview prefix on the first call. We only need it
	// once per run — the model carries it forward in conversation
	// memory after that. Re-sending wastes tokens and risks the
	// overview drifting from the workspace state mid-session.
	overviewSent bool
	// mode shapes how each file gets summarized: Planning gets a
	// broad full-caller list, Debug gets a depth-2 hub-drop trace,
	// Coding/Conversation/Review get the default depth-1 summary.
	// ModeUnknown is treated as ModeCoding via Mode.GraphScope().
	mode Mode
}

func newGraphContextInjector(g *graph.DB, workspace string, mode Mode) *graphContextInjector {
	return newGraphContextInjectorWithSet(g, workspace, mode, nil)
}

// newGraphContextInjectorWithSet is the multi-root variant.
// When set has multiple roots, the auto-injected project overview
// rendered on turn 0 covers EVERY root with a labeled subtree
// instead of just the primary workspace. Without this, an agent
// in a kai+kai-server multi-root session sees only kai/'s tree on
// first turn and confidently asserts "X doesn't exist" when X
// lives in kai-server/.
func newGraphContextInjectorWithSet(g *graph.DB, workspace string, mode Mode, set *projects.Set) *graphContextInjector {
	if g == nil && workspace == "" {
		return nil
	}
	return &graphContextInjector{
		graph:     g,
		workspace: workspace,
		set:       set,
		injected:  make(map[string]bool),
		mode:      mode,
	}
}

// buildBlock looks at the new content added since the last
// injection (the just-appended user message + any tool results from
// the prior turn), pulls file paths out, queries the graph for
// each path's depth-1 callers + protected status, and returns a
// short text block ready to prepend to the system role. Returns ""
// when nothing new is in scope so the caller can skip the prefix.
//
// We deliberately stay file-level rather than symbol-level for now:
// graph traversal is cheap, the block stays compact (1 line per
// file), and a follow-up can promote to symbol granularity once we
// have a clear UX win to point at.
func (gc *graphContextInjector) buildBlock(history []message.Message, protected []string) string {
	if gc == nil {
		return ""
	}
	var sections []string

	// One-time project overview. Sent on the first call of the run
	// (when no assistant turn has happened yet) so the model can
	// answer "what does this project do" without burning tool
	// calls on find/cat/view. Skipped on resumed sessions where
	// the model already saw it in the prior run — we'd need
	// session-state plumbing to detect that perfectly, but the
	// current heuristic (overview only when history starts with a
	// single user turn) gets it right for the common case.
	if !gc.overviewSent && isFirstTurn(history) {
		if ov := gc.buildOverview(); ov != "" {
			sections = append(sections, ov)
		}
		gc.overviewSent = true
	}

	// Per-file callers/dependents block. Scans the latest user
	// message + any tool results since the prior assistant turn,
	// pulls referenced paths, and emits graph context shaped by
	// the current mode (depth, breadth, truncation).
	paths := extractFilePaths(latestSlice(history))
	// Filter out phantom paths — graph nodes that point to files no
	// longer on disk. Without this, an old capture's stale File
	// nodes (created when the project briefly had placeholder
	// package.json/index.js scaffolding, since deleted) keep
	// surfacing in "Files in scope" forever, and the agent
	// confidently reports them to the user as real. 2026-05-12
	// dogfood pinned this: opus said "you have an index.js and
	// package.json here" because the runner injected those phantoms
	// from the graph, even though no such files existed anywhere in
	// the workspace tree.
	//
	// Two layers of protection: this one stops the phantom from
	// REACHING the agent (immediate user-facing fix); graph-gc on
	// capture (separate change) stops the phantom from accumulating
	// in the first place.
	paths = filterExistingPaths(paths, gc.workspace, gc.set)
	if len(paths) > 0 {
		scope := gc.mode.GraphScope()
		var blocks []string
		for _, p := range paths {
			// Review intentionally bypasses the per-file inject
			// cache because the reviewer rereads the same files
			// across turns as the developer asks follow-ups, and
			// stale call-chain data hurts more than the duplicate
			// tokens. Other modes dedupe so chatty turns don't
			// repeat the same context block.
			if scope != ScopeReviewSnapshot && gc.injected[p] {
				continue
			}
			gc.injected[p] = true
			block := gc.summarizeForMode(p, protected, scope)
			if block != "" {
				blocks = append(blocks, block)
			}
		}
		if len(blocks) > 0 {
			header := "Files in scope (kai graph):"
			if scope == ScopeTrace {
				header = "Trace (depth 2, hubs dropped — kai graph):"
			} else if scope == ScopeBroad {
				header = "Files in scope, full caller chains (kai graph):"
			}
			sections = append(sections, header+"\n"+strings.Join(blocks, "\n"))
		}
	}

	return strings.Join(sections, "\n\n")
}

// summarizeForMode dispatches to the mode-specific renderer. Returns
// "" when the path isn't in the graph or has nothing to say.
func (gc *graphContextInjector) summarizeForMode(path string, protected []string, scope GraphScopeStrategy) string {
	switch scope {
	case ScopeTrace:
		return gc.summarizeTrace(path, protected)
	case ScopeBroad:
		return gc.summarizeBroad(path, protected)
	default:
		return gc.summarize(path, protected)
	}
}

// filterExistingPaths drops candidate paths that don't resolve to a
// real file on disk. The graph can carry stale File nodes long after
// the underlying file has been deleted; without this filter the
// "Files in scope" injector lists those phantoms and the agent
// happily reports them to the user as real.
//
// Resolution strategy: try workspace-relative first (single-root),
// then each project's root for multi-root. The first hit wins;
// anything that can't be resolved anywhere drops. os.Stat is the
// only call — cheap enough to run on every injection (the path list
// is tiny, typically <20 entries).
func filterExistingPaths(paths []string, workspace string, set *projects.Set) []string {
	if len(paths) == 0 {
		return paths
	}
	candidates := make([]string, 0, 4)
	addIfReal := func(root string) {
		if root == "" {
			return
		}
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			candidates = append(candidates, root)
		}
	}
	addIfReal(workspace)
	if set != nil {
		for _, p := range set.Projects() {
			if p == nil {
				continue
			}
			addIfReal(p.Path)
		}
	}
	// If we can't anchor any real root (test fixtures with stub
	// workspace paths, or workspace was unexpectedly removed), skip
	// the filter rather than dropping everything — better to surface
	// possibly-phantom paths than to silently strip all context.
	if len(candidates) == 0 {
		return paths
	}
	out := paths[:0]
	for _, p := range paths {
		if pathExistsUnderAny(p, candidates) {
			out = append(out, p)
		}
	}
	return out
}

// pathExistsUnderAny returns true if `rel` resolves to a real file
// when joined with any of the candidate roots. The path may also
// be project-prefixed (e.g. "kai-server/foo.go") — we try each
// candidate as the workspace base; the prefix-aware resolution that
// the file tools use isn't reproduced here because the graph stores
// project-relative paths in the node payload, not project-prefixed
// ones.
func pathExistsUnderAny(rel string, roots []string) bool {
	if rel == "" {
		return false
	}
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			return true
		}
	}
	return false
}

// isFirstTurn reports whether `history` looks like a fresh
// conversation that hasn't yet done any code exploration. Used
// to gate the project-overview prefix.
//
// Heuristic: no ToolResult parts anywhere in history. This is
// stricter than "no assistant turns" (the previous version) and
// covers the planner-then-chat handoff: planner runs first on a
// fresh session, declares ErrTooVague WITHOUT calling tools,
// then chat agent resumes the same session. Chat sees an
// assistant turn (planner's "too vague" message) but zero tool
// results — overview should still fire so chat agent isn't
// blind. Without this, "yo" responses pattern-match on the
// directory name instead of the actual file tree.
func isFirstTurn(history []message.Message) bool {
	for _, m := range history {
		for _, p := range m.Parts {
			if _, ok := p.(message.ToolResult); ok {
				return false
			}
		}
	}
	return true
}

// summarize renders one line for a single file:
//
//	"- relpath/file.go — called by: a.go, b.go [PROTECTED]"
//
// Empty when the path isn't in the graph (unindexed file, just
// created, etc.) — we only want to surface real signal.
func (gc *graphContextInjector) summarize(path string, protected []string) string {
	callers := gc.callersOf(path)
	importers := gc.importersOf(path)
	merged := mergeUnique(callers, importers)

	parts := []string{"- " + path}
	if len(merged) > 0 {
		const cap = 5
		display := merged
		if len(display) > cap {
			display = append(append([]string{}, merged[:cap]...),
				fmt.Sprintf("… +%d more", len(merged)-cap))
		}
		parts = append(parts, "called by: "+strings.Join(display, ", "))
	} else {
		// No callers in the graph — could be entry point, dead
		// code, or unindexed. Note "no inbound edges" so the
		// model knows changes here have low blast radius.
		parts = append(parts, "no inbound edges")
	}
	if isProtected(path, protected) {
		parts = append(parts, "[PROTECTED]")
	}
	return strings.Join(parts, " — ")
}

// summarizeBroad is the Planning-mode renderer. Same depth as
// summarize() but no caller-name cap — the planner needs to see the
// full blast radius before splitting work, so we surface every
// caller. Token cost is acceptable here: Planning runs are short and
// the model uses this to decide work splits, not to navigate code.
func (gc *graphContextInjector) summarizeBroad(path string, protected []string) string {
	callers := gc.callersOf(path)
	importers := gc.importersOf(path)
	merged := mergeUnique(callers, importers)

	parts := []string{"- " + path}
	if len(merged) > 0 {
		parts = append(parts,
			fmt.Sprintf("called by (%d): %s", len(merged), strings.Join(merged, ", ")))
	} else {
		parts = append(parts, "no inbound edges")
	}
	if isProtected(path, protected) {
		parts = append(parts, "[PROTECTED]")
	}
	return strings.Join(parts, " — ")
}

// debugTraceNode is one node in the Debug-mode trace. distance is the
// number of inbound edges from the seed (1 = direct caller, 2 =
// caller-of-caller). degree is the total inbound caller count for
// this node — high-degree nodes are hubs (a logger, a panic helper,
// an error formatter). For distance-2 nodes, via lists the
// intermediate distance-1 callers we reached this node through, so
// the model can read the chain "broken_fn ← a.go ← x.go" instead of
// a flat list.
type debugTraceNode struct {
	path     string
	distance int
	degree   int
	via      []string
}

// summarizeTrace renders the Debug-mode block: depth-2 BFS over
// inbound edges from `path`, capped at mode.GraphCap() nodes using
// the hub-drop heuristic from the prompt-modes spec.
//
// Heuristic: keep the nodes closest to the error origin; when two
// nodes are equidistant, keep the one with fewer connections (more
// specific, less likely to be a utility). Hubs (high inbound degree
// — log.Error, panic helpers, common formatters) get dropped first
// because they call everything and tell us nothing about the bug.
//
// Sort order: ascending by (distance, degree). Slice to the cap.
// Distance-1 nodes are kept ahead of distance-2 nodes because direct
// callers are always more relevant for trace.
func (gc *graphContextInjector) summarizeTrace(path string, protected []string) string {
	if gc.graph == nil {
		return ""
	}
	cap, capped := gc.mode.GraphCap()
	if !capped {
		// Defensive: trace mode without a cap would let a hub blow
		// up the prompt. Pick a sane fallback identical to the
		// spec'd Debug cap so downstream code still terminates.
		cap = 50
	}

	// Distance-1 callers from the seed.
	dist1 := gc.callersOf(path)
	seen := map[string]bool{path: true}
	nodes := make([]debugTraceNode, 0, len(dist1))
	for _, c := range dist1 {
		if seen[c] {
			continue
		}
		seen[c] = true
		nodes = append(nodes, debugTraceNode{
			path: c, distance: 1,
			degree: len(gc.callersOf(c)),
		})
	}

	// Distance-2: callers of each distance-1 node. Group by
	// destination so we can render the via-list compactly.
	via := make(map[string][]string)
	d2Degrees := make(map[string]int)
	for _, c := range dist1 {
		for _, c2 := range gc.callersOf(c) {
			if seen[c2] {
				// c2 is the seed or already a distance-1 node;
				// don't demote it to distance-2.
				continue
			}
			via[c2] = append(via[c2], c)
			if _, ok := d2Degrees[c2]; !ok {
				d2Degrees[c2] = len(gc.callersOf(c2))
			}
		}
	}
	for p, vs := range via {
		sort.Strings(vs)
		nodes = append(nodes, debugTraceNode{
			path: p, distance: 2,
			degree: d2Degrees[p],
			via:    vs,
		})
	}

	// Truncation: ascending by (distance, degree, path) so we keep
	// distance-1 ahead of distance-2 and low-degree ahead of hubs.
	// Adding `path` last keeps the order deterministic when
	// distance and degree tie.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].distance != nodes[j].distance {
			return nodes[i].distance < nodes[j].distance
		}
		if nodes[i].degree != nodes[j].degree {
			return nodes[i].degree < nodes[j].degree
		}
		return nodes[i].path < nodes[j].path
	})
	dropped := 0
	if len(nodes) > cap {
		dropped = len(nodes) - cap
		nodes = nodes[:cap]
	}

	if len(nodes) == 0 {
		// Only emit a block when there's something to trace —
		// otherwise summarize() falls back further up.
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "- %s", path)
	if isProtected(path, protected) {
		b.WriteString(" [PROTECTED]")
	}
	b.WriteString("\n")
	// Group rendering by distance for readability.
	d1Lines, d2Lines := []string{}, []string{}
	for _, n := range nodes {
		if n.distance == 1 {
			d1Lines = append(d1Lines, fmt.Sprintf("    - %s (deg %d)", n.path, n.degree))
		} else {
			d2Lines = append(d2Lines,
				fmt.Sprintf("    - %s (deg %d, via %s)",
					n.path, n.degree, strings.Join(n.via, ", ")))
		}
	}
	if len(d1Lines) > 0 {
		b.WriteString("  callers (depth 1):\n")
		b.WriteString(strings.Join(d1Lines, "\n"))
		b.WriteString("\n")
	}
	if len(d2Lines) > 0 {
		b.WriteString("  callers-of-callers (depth 2):\n")
		b.WriteString(strings.Join(d2Lines, "\n"))
		b.WriteString("\n")
	}
	if dropped > 0 {
		// Some of the dropped nodes are hubs (high inbound degree),
		// others are just farther from the seed. Both are noise for
		// trace purposes; we phrase the notice generically rather
		// than claiming they're all hubs.
		fmt.Fprintf(&b, "  [%d nodes dropped (hubs + far) to fit %d-node cap]\n", dropped, cap)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (gc *graphContextInjector) callersOf(path string) []string {
	// Tolerate a nil graph: tests and lightweight runs (e.g. the
	// planner agent driven by a fake provider) don't always supply
	// one. Without nil-safety here the runner panics inside
	// buildBlock the first time the model mentions a file.
	if gc == nil || gc.graph == nil {
		return nil
	}
	edges, err := gc.graph.GetEdgesToByPath(path, graph.EdgeCalls)
	if err != nil {
		return nil
	}
	return filterExistingPaths(resolveSrcPaths(gc.graph, edges, path), gc.workspace, gc.set)
}

func (gc *graphContextInjector) importersOf(path string) []string {
	if gc == nil || gc.graph == nil {
		return nil
	}
	edges, err := gc.graph.GetEdgesToByPath(path, graph.EdgeImports)
	if err != nil {
		return nil
	}
	return filterExistingPaths(resolveSrcPaths(gc.graph, edges, path), gc.workspace, gc.set)
}

func resolveSrcPaths(g *graph.DB, edges []*graph.Edge, exclude string) []string {
	if len(edges) == 0 {
		return nil
	}
	out := make([]string, 0, len(edges))
	seen := make(map[string]bool)
	for _, e := range edges {
		node, err := g.GetNode(e.Src)
		if err != nil || node == nil {
			continue
		}
		p, _ := node.Payload["path"].(string)
		if p == "" || p == exclude || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// isProtected matches a path against the gate's protected glob
// list. Mirrors safetygate's logic but kept local so this helper
// doesn't reach across packages just to surface an annotation.
func isProtected(path string, protected []string) bool {
	for _, pat := range protected {
		if ok, _ := filepath.Match(pat, path); ok {
			return true
		}
		// Approximate "**" recursive glob: stdlib doesn't grok it.
		if strings.HasSuffix(pat, "/**") && strings.HasPrefix(path, strings.TrimSuffix(pat, "/**")+"/") {
			return true
		}
	}
	return false
}

// pathPattern matches workspace-relative file paths embedded in
// prose: at least one path segment + a recognized extension. The
// list is intentionally narrow — picking up every "log.txt" string
// in tool output would inject noise, but missing the actual files
// the conversation is about is worse. Add to extensions on demand.
var pathPattern = regexp.MustCompile(
	`\b[\w./-]+?\.(?:go|js|jsx|ts|tsx|py|rb|java|c|h|cc|cpp|hpp|rs|md|yaml|yml|json|toml|sql|sh|html|css)\b`,
)

// extractFilePaths pulls likely workspace-relative file paths out
// of a slice of messages. Looks at text content and tool_result
// content; ignores tool_use input (already a structured arg slot).
// Filters absolute paths to bare filenames since the graph is
// keyed by workspace-relative form.
func extractFilePaths(msgs []message.Message) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range msgs {
		for _, p := range m.Parts {
			text := ""
			switch v := p.(type) {
			case message.TextContent:
				text = v.Text
			case message.ToolResult:
				text = v.Content
			}
			for _, hit := range pathPattern.FindAllString(text, -1) {
				clean := strings.TrimPrefix(hit, "/")
				if seen[clean] {
					continue
				}
				seen[clean] = true
				out = append(out, clean)
			}
		}
	}
	return out
}

// buildOverview produces a compact one-time summary of the
// workspace so the model can answer project-level questions
// ("what does this do") without bash exploration. Three pieces,
// each cheap to read:
//
//   - top-level tree: dirs + files at depth 1, with common noise
//     (node_modules, .git, build artifacts) filtered out
//   - manifest digest: package.json name+scripts+deps, or go.mod,
//     pyproject.toml, etc. — the first one we recognize wins
//   - README excerpt: first ~800 chars of README.md
//
// Empty workspace, missing files, or read errors → that piece is
// just omitted; we never block the run on overview generation.
// identityLine is the one-line "you are here" header that opens the
// turn-0 overview: which directory the agent is working in and, when
// resolvable, the human name of the project. Stated plainly up front so
// the model never has to infer (or hallucinate) the workspace identity
// — the 2026-05-27/28 dogfoods both turned on the model guessing "this
// is the kai repo" against a different workspace. Mode-agnostic: the
// overview is injected on the first turn of every mode, so this helps
// code/debug/review as much as conversation.
func identityLine(workspace string, set *projects.Set) string {
	if workspace == "" {
		return ""
	}
	dir := filepath.Base(workspace)
	if set != nil && len(set.Projects()) > 1 {
		return fmt.Sprintf("You are working in a multi-root workspace at %q (%d projects; see the per-project breakdown below).\n\n", dir, len(set.Projects()))
	}
	name := ""
	if set != nil {
		if ps := set.Projects(); len(ps) == 1 && ps[0] != nil {
			name = ps[0].Name
		}
	}
	line := fmt.Sprintf("You are working in the %q directory.", dir)
	if name != "" && name != dir {
		line += fmt.Sprintf(" This project is called %q.", name)
	}
	return line + "\n\n"
}

func (gc *graphContextInjector) buildOverview() string {
	if gc.workspace == "" {
		return ""
	}
	var sections []string
	// Multi-root: emit one tree per root with a labeled
	// header. Without this, an agent in a kai+kai-server multi-
	// root session sees only kai/'s tree on first turn and
	// confidently asserts "X doesn't exist" when X lives in
	// kai-server/.
	if gc.set != nil && len(gc.set.Projects()) > 1 {
		var b strings.Builder
		b.WriteString("Multi-root workspace (")
		fmt.Fprintf(&b, "%d projects). Use the directory name (lowercase, no spaces) as a path prefix when calling tools — e.g. \"kai-server/api/llm.go\", NOT \"Kai Server/api/llm.go\":\n", len(gc.set.Projects()))
		for _, proj := range gc.set.Projects() {
			// Directory basename is the canonical path prefix. See
			// agent_planner.go for the same reasoning — Name is for
			// humans (README H1, package.json), basename is for
			// tool calls.
			dir := filepath.Base(proj.Path)
			if proj.Name != "" && proj.Name != dir {
				fmt.Fprintf(&b, "\n── %s (also known as %q) at %s ──\n", dir, proj.Name, proj.Path)
			} else {
				fmt.Fprintf(&b, "\n── %s at %s ──\n", dir, proj.Path)
			}
			// Description, when present in kai.projects.yaml, collapses
			// search scope: the planner reads what each project owns
			// and routes queries to the right one in zero searches
			// instead of N greps against the wrong cwd. Optional;
			// missing description just omits this line (no auto-gen,
			// no inference). The 2026-05-27 /exit dogfood pinned the
			// failure shape this addresses: 8 of 10 turns spent
			// searching kai-desktop for slash-command code that lives
			// in kai/kai-cli/.
			if proj.Description != "" {
				fmt.Fprintf(&b, "  %s\n", proj.Description)
			}
			if t := overviewTree(proj.Path); t != "" {
				b.WriteString(t)
				if !strings.HasSuffix(t, "\n") {
					b.WriteByte('\n')
				}
			}
		}
		sections = append(sections, strings.TrimRight(b.String(), "\n"))
	} else if tree := overviewTree(gc.workspace); tree != "" {
		sections = append(sections, "Top-level tree:\n"+tree)
	}
	if manifest := overviewManifest(gc.workspace); manifest != "" {
		sections = append(sections, manifest)
	}
	// Structural facts: module roots + canonical build/test commands.
	// Workers used to discover these by trial-and-error — running
	// `go build ./...` from random subdirs and seeing "directory
	// prefix kailab/pack does not contain main module" 5+ times
	// before finding the right cwd. Surface them once at turn 0 and
	// the worker can pick the right command first try.
	//
	// Multi-root: emit per-project subsection. Single-root: one block
	// for the workspace root.
	if gc.set != nil && len(gc.set.Projects()) > 1 {
		var parts []string
		for _, proj := range gc.set.Projects() {
			if s := overviewStructure(proj.Path); s != "" {
				dir := filepath.Base(proj.Path)
				parts = append(parts, "── "+dir+" ──\n"+s)
			}
		}
		if len(parts) > 0 {
			sections = append(sections, "Structural facts (module roots + canonical build/test commands):\n"+strings.Join(parts, "\n\n"))
		}
	} else if s := overviewStructure(gc.workspace); s != "" {
		sections = append(sections, "Structural facts (module roots + canonical build/test commands):\n"+s)
	}
	if readme := overviewReadme(gc.workspace); readme != "" {
		sections = append(sections, "README excerpt:\n"+readme)
	}
	if len(sections) == 0 {
		return ""
	}
	// cwd preface: bash and file tools run with cwd == workspace.
	// The agent often sees absolute paths in the user prompt
	// (e.g. /Users/jacobschatz/...) and tries to cd into them; when
	// running inside a CoW spawn dir at /tmp/kai-<task>-<ts>/, those
	// real-fs paths don't exist. State the cwd up front and direct
	// the agent to use relative paths from it. The 2026-05-14
	// "quality-nits-fix" dogfood burned 25+ turns on this confusion
	// before the openai stream timed out.
	cwd := "Your cwd: " + gc.workspace + "\n" +
		"All paths in this overview are RELATIVE to that cwd. When the user mentions an absolute path or a workspace path like \"kai-server/foo/bar.go\", treat it as a path relative to your cwd. Bash and file tools both run with that cwd; do not cd into different absolute paths.\n\n"
	return identityLine(gc.workspace, gc.set) +
		"Project overview (auto-injected by kai from the workspace).\n" +
		"This is authoritative for high-level questions like \"what does this project do\", \"how is it structured\", \"how do I run it\". " +
		"Do NOT re-discover any of it with bash/find/view — answer directly from the block below. " +
		"Only reach for view/bash when the user asks for code-level detail this overview doesn't cover.\n\n" +
		cwd +
		strings.Join(sections, "\n\n")
}

// overviewIgnore is the noise filter for the top-level tree
// listing. Entries that aren't relevant to "what does this project
// do" — vendored deps, VCS metadata, build outputs, OS junk —
// would just dilute signal in the system prompt.
var overviewIgnore = map[string]bool{
	".git":         true,
	".kai":         true,
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"build":        true,
	"dist":         true,
	".next":        true,
	".nuxt":        true,
	".cache":       true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	".idea":        true,
	".vscode":      true,
	".DS_Store":    true,
}

func overviewTree(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var dirs, files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && overviewIgnore[name] {
			continue
		}
		if overviewIgnore[name] {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, name+"/")
		} else {
			files = append(files, name)
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	all := append(dirs, files...)
	if len(all) == 0 {
		return ""
	}
	const maxEntries = 30
	if len(all) > maxEntries {
		all = append(all[:maxEntries], fmt.Sprintf("… +%d more", len(all)-maxEntries))
	}
	return "  " + strings.Join(all, "\n  ")
}

// overviewManifest tries each known manifest in turn and returns
// a short digest of the first one it finds. The order matters:
// package.json before pyproject.toml in case a project ships both
// (e.g. a JS frontend with a Python tooling sidecar) — the
// dominant manifest is usually the first one a developer reaches
// for.
func overviewManifest(root string) string {
	if d := manifestPackageJSON(root); d != "" {
		return d
	}
	if d := manifestGoMod(root); d != "" {
		return d
	}
	if d := manifestPyproject(root); d != "" {
		return d
	}
	if d := manifestCargo(root); d != "" {
		return d
	}
	return ""
}

func manifestPackageJSON(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Name            string            `json:"name"`
		Description     string            `json:"description"`
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Manifest (package.json):")
	if m.Name != "" {
		fmt.Fprintf(&b, "\n  name: %s", m.Name)
	}
	if m.Description != "" {
		fmt.Fprintf(&b, "\n  description: %s", m.Description)
	}
	if len(m.Scripts) > 0 {
		fmt.Fprintf(&b, "\n  scripts: %s", strings.Join(sortedKeys(m.Scripts), ", "))
	}
	if len(m.Dependencies) > 0 {
		fmt.Fprintf(&b, "\n  dependencies (%d): %s", len(m.Dependencies),
			joinCapped(sortedKeys(m.Dependencies), 12))
	}
	if len(m.DevDependencies) > 0 {
		fmt.Fprintf(&b, "\n  devDependencies (%d): %s", len(m.DevDependencies),
			joinCapped(sortedKeys(m.DevDependencies), 8))
	}
	return b.String()
}

func manifestGoMod(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	var module, goVer string
	requires := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "module "):
			module = strings.TrimSpace(strings.TrimPrefix(l, "module "))
		case strings.HasPrefix(l, "go "):
			goVer = strings.TrimSpace(strings.TrimPrefix(l, "go "))
		case strings.HasPrefix(l, "require "), strings.HasPrefix(l, "\trequire "):
			requires++
		}
	}
	var b strings.Builder
	b.WriteString("Manifest (go.mod):")
	if module != "" {
		fmt.Fprintf(&b, "\n  module: %s", module)
	}
	if goVer != "" {
		fmt.Fprintf(&b, "\n  go: %s", goVer)
	}
	if requires > 0 {
		fmt.Fprintf(&b, "\n  require lines: %d", requires)
	}
	return b.String()
}

func manifestPyproject(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "pyproject.toml"))
	if err != nil {
		return ""
	}
	// Light excerpt — first 25 lines is enough to show name +
	// dependencies block. Avoiding a TOML parser dep here to keep
	// this helper trivial.
	return "Manifest (pyproject.toml, first 25 lines):\n" + truncateLines(string(data), 25)
}

func manifestCargo(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return ""
	}
	return "Manifest (Cargo.toml, first 20 lines):\n" + truncateLines(string(data), 20)
}

// overviewStructure walks the workspace looking for module roots
// (go.mod, package.json, pyproject.toml, Cargo.toml) up to depth 3,
// plus the workspace root's Makefile / Justfile if present. For
// each module root it emits the canonical cwd the worker must use
// to invoke its build tool, plus the most relevant commands.
//
// Design constraints:
//   - Bounded: max 20 module roots, no recursion past depth 3 — big
//     monorepos blow out the overview otherwise.
//   - overviewIgnore is reused so vendored deps and build outputs
//     don't pollute the listing.
//   - Cheap: stats files, parses only the small ones (package.json
//     for scripts, go.mod for module name). No TOML parser.
//   - Tolerant: a missing/corrupt file just skips that root; we
//     never fail the overview.
//
// Output shape: one line per module root, indented commands below
// when relevant. E.g.:
//
//	go modules:
//	  cd kai-server/kailab/ — module kailab — `go build ./...` / `go test ./...`
//	  cd kai-server/kai-core/ — module kai-core — `go build ./...` / `go test ./...`
//	node packages:
//	  cd ui/ — name ui — scripts: build, test, dev
//	root commands:
//	  Makefile targets: install, build, test, fmt
// goModuleEntry holds the bits of a discovered go.mod we want to
// surface to the model: where to cd, what module name to use in
// import paths, and (for cross-module dep mining) the file's own
// directory so we can re-parse its require block later.
type goModuleEntry struct {
	relDir string
	module string
}

func overviewStructure(root string) string {
	const maxDepth = 3
	const maxModules = 20

	type goMod = goModuleEntry
	type nodePkg struct {
		relDir  string
		name    string
		scripts []string
	}
	type pyProj struct {
		relDir string
	}
	type rustCrate struct {
		relDir string
	}

	var goMods []goMod
	var nodePkgs []nodePkg
	var pys []pyProj
	var rusts []rustCrate

	stop := false
	total := func() int { return len(goMods) + len(nodePkgs) + len(pys) + len(rusts) }

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if stop || depth > maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		// First pass: harvest any manifests at this depth before
		// descending. We want shallower modules listed first.
		for _, e := range entries {
			if stop {
				return
			}
			if e.IsDir() {
				continue
			}
			full := filepath.Join(dir, e.Name())
			rel, _ := filepath.Rel(root, dir)
			if rel == "" {
				rel = "."
			}
			switch e.Name() {
			case "go.mod":
				if total() >= maxModules {
					stop = true
					return
				}
				goMods = append(goMods, goMod{relDir: rel, module: goModName(full)})
			case "package.json":
				if total() >= maxModules {
					stop = true
					return
				}
				name, scripts := nodePackageInfo(full)
				nodePkgs = append(nodePkgs, nodePkg{relDir: rel, name: name, scripts: scripts})
			case "pyproject.toml":
				if total() >= maxModules {
					stop = true
					return
				}
				pys = append(pys, pyProj{relDir: rel})
			case "Cargo.toml":
				if total() >= maxModules {
					stop = true
					return
				}
				rusts = append(rusts, rustCrate{relDir: rel})
			}
		}
		// Second pass: descend (filtered).
		for _, e := range entries {
			if stop {
				return
			}
			if !e.IsDir() || overviewIgnore[e.Name()] || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	walk(root, 0)

	var b strings.Builder
	if len(goMods) > 0 {
		b.WriteString("go modules:\n")
		for _, g := range goMods {
			loc := g.relDir + "/"
			if g.relDir == "." {
				loc = "./"
			}
			if g.module != "" {
				// Import path is the *module name*, not the workspace
				// path. Without this, workers write
				// `import "kai-server/kailab/store"` (workspace path)
				// when the actual import is `"kailab/store"` (module
				// name). Explicit beats inferred.
				fmt.Fprintf(&b, "  cd %s — module %s — import path: %s/... — `go build ./...` / `go test ./...`\n", loc, g.module, g.module)
			} else {
				fmt.Fprintf(&b, "  cd %s — `go build ./...` / `go test ./...`\n", loc)
			}
		}
	}
	if len(nodePkgs) > 0 {
		b.WriteString("node packages:\n")
		for _, p := range nodePkgs {
			loc := p.relDir + "/"
			if p.relDir == "." {
				loc = "./"
			}
			line := "  cd " + loc
			if p.name != "" {
				// For node, the package name IS the import specifier
				// other packages use (`import x from "foo"` where
				// "foo" is package.json's name).
				line += " — name " + p.name + " — import as: " + p.name
			}
			if len(p.scripts) > 0 {
				line += " — scripts: " + joinCapped(p.scripts, 8)
			}
			b.WriteString(line + "\n")
		}
	}
	// Cross-module deps: surface local modules that depend on each
	// other so the planner sees "this is one repo with shared deps"
	// instead of "two repos that might have drifted." Mining target
	// is the require + replace blocks of each go.mod we found.
	if shared := buildSharedGoModules(root, goMods); shared != "" {
		b.WriteString(shared)
	}
	if len(pys) > 0 {
		b.WriteString("python projects:\n")
		for _, p := range pys {
			loc := p.relDir + "/"
			if p.relDir == "." {
				loc = "./"
			}
			fmt.Fprintf(&b, "  cd %s — `pytest` / `python -m build`\n", loc)
		}
	}
	if len(rusts) > 0 {
		b.WriteString("rust crates:\n")
		for _, r := range rusts {
			loc := r.relDir + "/"
			if r.relDir == "." {
				loc = "./"
			}
			fmt.Fprintf(&b, "  cd %s — `cargo build` / `cargo test`\n", loc)
		}
	}
	// Root commands: Makefile / Justfile targets at workspace root.
	// Only at root — nested makefiles are noise.
	if targets := makefileTargets(root); len(targets) > 0 {
		fmt.Fprintf(&b, "root commands:\n  Makefile targets: %s\n", joinCapped(targets, 12))
	}
	if targets := justfileTargets(root); len(targets) > 0 {
		fmt.Fprintf(&b, "root commands:\n  Justfile targets: %s\n", joinCapped(targets, 12))
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildSharedGoModules scans each discovered local go.mod for
// require + replace directives pointing at OTHER local modules,
// and emits a "shared modules" section listing each shared dep
// with its users. Surfaces the structural fact "this is one
// codebase with internal deps, not two unrelated repos" so the
// planner doesn't hallucinate drift between two on-disk copies of
// a shared module (the run-4 cas.go failure was exactly this — the
// planner asserted "kai-core in kai-server is 8 weeks behind"
// when the two files were byte-identical).
//
// Only deps that resolve to a module we ALSO found locally show
// up; external deps (github.com/foo/bar) aren't surfaced.
func buildSharedGoModules(root string, mods []goModuleEntry) string {
	if len(mods) < 2 {
		return ""
	}
	// Index by module name so we can look up "does this requirement
	// match one of our local modules?" without scanning each time.
	byName := make(map[string]*goModuleEntry, len(mods))
	for i := range mods {
		if mods[i].module == "" {
			continue
		}
		byName[mods[i].module] = &mods[i]
	}
	if len(byName) < 2 {
		return ""
	}
	type sharedDep struct {
		name  string
		users []string // module names that depend on it
	}
	depUsers := make(map[string]map[string]bool)
	for _, m := range mods {
		if m.module == "" {
			continue
		}
		full := filepath.Join(root, m.relDir, "go.mod")
		reqs := parseGoModRequires(full)
		for _, req := range reqs {
			if req == m.module {
				continue // self-reference, ignore
			}
			if _, isLocal := byName[req]; !isLocal {
				continue
			}
			if depUsers[req] == nil {
				depUsers[req] = make(map[string]bool)
			}
			depUsers[req][m.module] = true
		}
	}
	if len(depUsers) == 0 {
		return ""
	}
	var deps []sharedDep
	for name, users := range depUsers {
		userNames := make([]string, 0, len(users))
		for u := range users {
			userNames = append(userNames, u)
		}
		sort.Strings(userNames)
		deps = append(deps, sharedDep{name: name, users: userNames})
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].name < deps[j].name })

	var b strings.Builder
	b.WriteString("shared modules (one source of truth, used by multiple local modules — don't assume drift):\n")
	for _, d := range deps {
		entry := byName[d.name]
		loc := entry.relDir + "/"
		if entry.relDir == "." {
			loc = "./"
		}
		fmt.Fprintf(&b, "  %s at %s — used by: %s\n", d.name, loc, strings.Join(d.users, ", "))
	}
	return b.String()
}

// parseGoModRequires extracts module names from a go.mod's require
// directives. Handles both single-line `require foo v1` and the
// block form `require ( ... )`. Tolerant of comments and indirect
// markers. Returns an empty slice if the file is unreadable.
func parseGoModRequires(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		raw := line
		// Strip trailing comment.
		if i := strings.Index(raw, "//"); i >= 0 {
			raw = raw[:i]
		}
		l := strings.TrimSpace(raw)
		if l == "" {
			continue
		}
		switch {
		case strings.HasPrefix(l, "require ("):
			inBlock = true
			continue
		case l == ")":
			inBlock = false
			continue
		case strings.HasPrefix(l, "require "):
			// Single-line require: `require name v1.2.3`
			rest := strings.TrimSpace(strings.TrimPrefix(l, "require"))
			if name := firstToken(rest); name != "" {
				out = append(out, name)
			}
		case inBlock:
			// Inside `require (`: lines are `name v1.2.3` (with
			// optional `// indirect` already stripped above).
			if name := firstToken(l); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// goModName extracts the `module <name>` line from a go.mod. Returns
// "" when the file is missing or unparseable — we never fail
// overview generation on a malformed manifest.
func goModName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "module "))
		}
	}
	return ""
}

// nodePackageInfo extracts package.json name + script keys. Only
// the script *keys* — values are often shell snippets too long to
// quote inline; the worker can `view package.json` for details.
// Sorted output so the listing is deterministic across runs.
func nodePackageInfo(path string) (name string, scripts []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	var m struct {
		Name    string            `json:"name"`
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", nil
	}
	return m.Name, sortedKeys(m.Scripts)
}

// makefileTargets returns the names of phony targets declared in
// the workspace root Makefile (or the file pointed at by GNUmakefile
// / makefile). Heuristic only: matches lines that look like
// "target:" or "target :" at the start of a line, skips include
// directives and pattern rules. Bounded at 30 — bigger lists are
// rarely scannable anyway.
func makefileTargets(root string) []string {
	for _, name := range []string{"Makefile", "GNUmakefile", "makefile"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		return parseMakeTargets(string(data))
	}
	return nil
}

func parseMakeTargets(src string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(src, "\n") {
		// First non-whitespace token followed by ':' (not ':=', not '::').
		if line == "" || line[0] == '\t' || line[0] == '#' || line[0] == ' ' {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || strings.ContainsAny(name, " \t%$.") {
			continue
		}
		// Skip ':=' assignments and ':: rules' — neither are targets.
		rest := line[colon:]
		if strings.HasPrefix(rest, ":=") || strings.HasPrefix(rest, "::") {
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
			if len(out) >= 30 {
				break
			}
		}
	}
	return out
}

// justfileTargets does the same job for Justfile (https://just.systems).
// Just recipes look like `name:` or `name args:` at line start.
func justfileTargets(root string) []string {
	for _, name := range []string{"justfile", "Justfile", ".justfile"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		var out []string
		seen := make(map[string]bool)
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" || line[0] == '#' || line[0] == ' ' || line[0] == '\t' {
				continue
			}
			colon := strings.IndexByte(line, ':')
			if colon <= 0 {
				continue
			}
			// Recipe head is "name [args]:" — split first word.
			head := strings.TrimSpace(line[:colon])
			head = strings.SplitN(head, " ", 2)[0]
			if head == "" || strings.ContainsAny(head, "%$.") {
				continue
			}
			if !seen[head] {
				seen[head] = true
				out = append(out, head)
				if len(out) >= 30 {
					break
				}
			}
		}
		return out
	}
	return nil
}

func overviewReadme(root string) string {
	for _, name := range []string{"README.md", "readme.md", "Readme.md", "README"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		const maxBytes = 800
		if len(s) > maxBytes {
			s = s[:maxBytes] + "…"
		}
		return s
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinCapped(items []string, cap int) string {
	if len(items) <= cap {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:cap], ", ") + fmt.Sprintf(", … +%d more", len(items)-cap)
}

func truncateLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n…"
}

// latestSlice returns history from the most recent user turn back
// to (but excluding) the prior assistant turn. That window is what
// the agent is reasoning about right now — earlier turns have
// already had their graph context injected on previous calls.
func latestSlice(history []message.Message) []message.Message {
	if len(history) == 0 {
		return nil
	}
	end := len(history)
	for i := end - 1; i >= 0; i-- {
		if history[i].Role == message.RoleAssistant {
			return history[i+1 : end]
		}
	}
	return history
}
