package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"kai/internal/graph"
	"kai/internal/projects"
)

// kaiSearchTool exposes kai's semantic-graph FTS5 index as a tool.
// The agent gets ripgrep-grade speed AND multi-root awareness AND
// snippet rendering — and because the index is pre-built, the cost
// is constant regardless of repo size.
//
// Workflow:
//  1. First call (or call after a known-stale index) triggers a
//     synchronous backfill from disk. Walks every project's files,
//     filters out binaries / ignored dirs, calls db.IndexFile.
//  2. Subsequent calls hit the FTS index directly — sub-10ms typical.
//
// Backfill is INSIDE the tool rather than at TUI startup so users
// with huge monorepos who never ask a text question never pay the
// indexing cost. Tradeoff: the first call feels slow; we surface
// "indexing N files…" in the response so the user knows why.
//
// This is Phase 1: no capture-pipeline integration, so edits after
// the backfill are stale until the next backfill. Phase 2 will hook
// into runCapture for incremental maintenance.

// fsTextExtensions is the inclusion list. Kept as a positive list
// (rather than "not in fsBinaryExtensions") so we never accidentally
// index a 4MB minified JS bundle just because it lacks a binary
// extension. Source files only — that's what the agent searches for.
var fsTextExtensions = map[string]bool{
	".go": true, ".py": true, ".rs": true, ".ts": true, ".tsx": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".java": true, ".kt": true, ".swift": true, ".c": true, ".cc": true,
	".cpp": true, ".cxx": true, ".h": true, ".hpp": true,
	".rb": true, ".php": true, ".cs": true, ".scala": true, ".ex": true,
	".exs": true, ".erl": true, ".clj": true, ".hs": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true,
	".yaml": true, ".yml": true, ".toml": true, ".json": true,
	".md": true, ".mdx": true, ".rst": true, ".txt": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".sass": true,
	".sql": true, ".graphql": true, ".proto": true,
	".dockerfile": true, ".tf": true, ".tfvars": true,
}

// maxIndexableSize is the per-file cap for the backfill. Files
// larger than this are skipped — typically generated code, minified
// bundles, lockfiles, or vendored binary blobs that slipped through
// the extension filter. The FTS5 token cost on these dwarfs their
// usefulness in search results.
const maxIndexableSize = 1 << 20 // 1 MiB

// maxSearchResultBytes caps the rendered kai_search result the model
// sees. kai_search already caps the hit COUNT (limit, default 20 / cap
// 200) but a 200-hit result — each carrying a path, enclosing symbol,
// and FTS5 snippet — can still be 10–15 KB of tool-result text. Every
// other tool path applies a byte/line cap before the result enters
// history; this brings kai_search in line. Hits past the cap are
// dropped with a "…N more hit(s)" suffix so the model knows to refine.
//
// maxSnippetBytes trims a single hit's snippet so one pathological
// match can't blow the budget on its own (the result cap always emits
// at least the first hit, so an uncapped snippet would otherwise
// defeat it).
const (
	maxSearchResultBytes = 4096
	maxSnippetBytes      = 240
)

// Searcher is the subset of *graph.DB kai_search needs. Defined as
// an interface so unit tests can substitute a tiny fake without
// spinning up SQLite — same pattern as KaiGrapher.
type Searcher interface {
	IndexFile(project, path, body string) error
	SearchText(query, project string, limit int) ([]graph.SearchHit, error)
	FileTextCount() int
	CountFileTextForProject(project string) int
	ClearFileTextForProject(project string) error
	RemoveFile(project, path string) error
}

type kaiSearchTool struct {
	workspace string
	set       *projects.Set
	db        Searcher

	// grapher is the symbol-graph reader used for hit enrichment.
	// When non-nil, every search result is joined back to the graph's
	// Symbol nodes by line range so the agent sees the enclosing
	// function/method/class for each match. The differentiator vs.
	// rg-shell-out: a hit becomes "in handleAuth() at line 42",
	// not "src/handler.go:42:match". May be nil — the tool degrades
	// to plain FTS results in that case.
	grapher KaiGrapher

	// backfilled memoizes which projects this tool instance has
	// confirmed fully indexed (or just backfilled), so completed
	// projects aren't disk-counted on every search. Per-instance (not
	// package-global) so it can't leak across tests/runs. Guarded by
	// backfillMu. Lazily initialized.
	backfilled map[string]bool
}

// backfillMu serializes concurrent backfills against the same
// Searcher so two concurrent searches against a freshly-added
// project don't race on ClearFileTextForProject + IndexFile.
// One global mutex is fine — backfills are infrequent (only when
// a project's row count is zero) and fast (single project walk).
var backfillMu sync.Mutex

// countIndexableFiles counts the files under dir that backfillRoots
// WOULD index — same noise-dir, extension, and size filter, but no
// reads. Used to detect a PARTIALLY indexed project (FTS rows < disk
// files): the old `count == 0` gate let a stale 1-file partial block
// the one-time backfill forever (kai-tui sat at 1 of ~490 files, so
// kai_search project=kai-tui returned nothing — 2026-05-29).
func countIndexableFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != dir && fsIgnoreDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !fsTextExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > maxIndexableSize {
			return nil
		}
		n++
		return nil
	})
	return n
}

type kaiSearchParams struct {
	Query   string `json:"query"`
	Project string `json:"project"`
	Limit   int    `json:"limit"`
}

func (t *kaiSearchTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_search",
		Description: "PRIMARY text-search tool for the workspace. Pre-built SQLite FTS5 index, " +
			"BM25-ranked, multi-root aware, snippet-highlighted with «match» markers. " +
			"Sub-10ms queries even on large monorepos. Empty projects contribute zero rows so they " +
			"can't produce false-hope results (unlike a filesystem walk). " +
			"USE THIS FIRST for any \"where is X used / mentioned / set / defined\" question, including " +
			"identifier lookups, config keys, strings, error messages, comments, and TODOs. " +
			"Query syntax: bare words match whole tokens (case-insensitive); quote phrases (\"reasoning format\"); " +
			"`*` for prefix (`config*`); AND/OR/NOT for boolean composition. " +
			"AVOID dots in queries — FTS5 reads `.` as a column operator and rejects `foo.Bar` with " +
			"`syntax error near \".\"`. Use spaces between identifier parts (`foo Bar` not `foo.Bar`), " +
			"or wrap the whole query in `\"...\"` to make the dot a literal in a phrase search. " +
			"Fall back to kai_grep only when you need regex matching FTS5 can't express, or you " +
			"suspect very recent uncommitted changes the index hasn't caught yet. " +
			"DO NOT use kai_search to verify whether a CLI command exposes a JSON field — run the command (via bash) and look at its actual output instead. The 2026-05-26 dogfood burned 6 turns hunting 'snapshot_count' / 'SnapshotCount' / 'total_snapshots' in source when 'kai stats --json' would have answered 'that field does not exist' in one bash call.",
		Parameters: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "FTS5 query. Bare words match tokens; phrases in double quotes; * for prefix.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Optional project name to scope to (e.g. \"kai-server\"). Empty = all projects.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results. Default 20, cap 200.",
				"default":     20,
			},
		},
		Required: []string{"query"},
	}
}

func (t *kaiSearchTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.db == nil {
		return NewTextErrorResponse("kai_search: not configured (no graph DB)"), nil
	}
	var p kaiSearchParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_search: invalid input json: " + err.Error()), nil
		}
	}
	query := strings.TrimSpace(p.Query)
	if query == "" {
		return NewTextErrorResponse("kai_search: query required"), nil
	}
	// Translate regex-style `a|b|c` alternation into FTS5's
	// `a OR b OR c`. FTS5's MATCH grammar uses keyword `OR`;
	// `|` is not a valid operator and produces a SQL syntax error
	// otherwise. Models writing kai_search queries default to the
	// regex idiom; this rewrite makes them work as intended. Skip
	// the rewrite if the query uses double quotes (FTS5 phrase
	// syntax) since pipes inside a phrase are literal characters.
	query = rewritePipeAlternation(query)
	query = rewriteIdentifierSeparators(query)

	// Lazy backfill. For each project in the workspace, ensure the
	// FTS index has rows for it; if not, walk that project and
	// index. This per-project check (rather than the legacy total-
	// count check) catches the case where an earlier single-root
	// session populated only the primary, then the workspace
	// expanded to multi-root and the previously-unindexed siblings
	// stayed permanently invisible to FTS until a manual `kai capture`
	// or similar. Observed in the 2026-05-20 dogfood: kai's
	// db.sqlite had 69 rows under project=kai and zero rows for
	// kai-server, even after the workspace was set up multi-root.
	//
	// Synchronous on purpose: a background goroutine would return
	// phantom-empty results on the first call, which is a much
	// worse UX than a one-time 1-2 second wait per missing project.
	var indexNote string
	if n, err := t.ensureProjectsBackfilled(ctx); err != nil {
		return NewTextErrorResponse("kai_search: backfill failed: " + err.Error()), nil
	} else if n > 0 {
		indexNote = fmt.Sprintf("(indexed %d files for projects missing from FTS; future searches will be instant)\n\n", n)
	}

	hits, err := t.db.SearchText(query, p.Project, p.Limit)
	if err != nil {
		// FTS5 surfaces parse errors here ("malformed MATCH expression").
		// Pass them through verbatim — they're actionable.
		return NewTextErrorResponse("kai_search: " + err.Error()), nil
	}
	// Drop hits that live under .claude/worktrees/ — those are git
	// worktree mirrors of the same source files, indexed once when
	// the walk happened before fsIgnoreDirs[".claude"] was added.
	// They produce N copies of the same hit and mislead the planner
	// into thinking phantom field names exist (2026-05-26 dogfood:
	// 20 fake "SnapshotCount" matches across 5 worktree copies).
	// This filter is belt-and-suspenders alongside the walk exclusion
	// in fsIgnoreDirs; remove once the index is fully rebuilt with
	// the walk skip in place.
	hits = dropWorktreeHits(hits)
	if len(hits) == 0 {
		return NewTextResponse(indexNote + fmt.Sprintf("kai_search: no matches for %q\n\n%s", query, zeroMatchHint(query, p.Project != ""))), nil
	}

	// Enrichment pass: attach the enclosing symbol (function /
	// method / class) and the line of the first match to each hit.
	// This is the differentiator vs. shelling out to rg — every
	// result carries semantic context, not just a path:line:match.
	// Best-effort: hits where enrichment fails (file unreadable,
	// no symbol covers the line) render with just path + snippet.
	if t.grapher != nil {
		queryToken := firstSearchToken(query)
		for i := range hits {
			t.enrich(&hits[i], queryToken)
		}
	}

	var b strings.Builder
	b.WriteString(indexNote)
	fmt.Fprintf(&b, "kai_search: %d match(es) for %q\n", len(hits), query)
	for i, h := range hits {
		// Render one hit into a scratch buffer first so the byte cap
		// can decide whether it fits BEFORE committing it — truncation
		// lands on a hit boundary, never mid-hit.
		var hb strings.Builder
		display := h.Path
		if h.Project != "" {
			display = h.Project + "/" + h.Path
		}
		if h.Line > 0 {
			display = fmt.Sprintf("%s:%d", display, h.Line)
		}
		fmt.Fprintf(&hb, "  %s\n", display)
		if h.Symbol != "" {
			fmt.Fprintf(&hb, "    in %s\n", h.Symbol)
		}
		fmt.Fprintf(&hb, "    %s\n", truncateSnippet(h.Snippet, maxSnippetBytes))

		// Always emit at least the first hit (i==0) so a single large
		// hit still shows; past that, stop once we'd blow the cap.
		if i > 0 && b.Len()+hb.Len() > maxSearchResultBytes {
			fmt.Fprintf(&b, "  …%d more hit(s) not shown (%d-byte cap) — refine the query or lower `limit`.\n",
				len(hits)-i, maxSearchResultBytes)
			break
		}
		b.WriteString(hb.String())
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// truncateSnippet trims an FTS5 snippet to at most max bytes, backing
// off to a rune boundary so a multi-byte character is never split, and
// appends an ellipsis when it cut. Returns s unchanged when it fits.
func truncateSnippet(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \t") + "…"
}

// enrich attaches Symbol + Line to a hit by:
//  1. Resolving the hit's project to its on-disk path.
//  2. Reading the file and finding the first line that contains
//     the query's first token. FTS5 doesn't expose match offsets
//     directly; a text scan over a few-KB file is cheaper than
//     piping snippet output through an offset parser.
//  3. Looking up Symbol nodes that DEFINES_IN this file and
//     picking the one whose [start, end] line range contains the
//     match line. Top-level matches (between symbols, e.g. in
//     package-level comments) leave Symbol empty.
//
// Errors are swallowed: this is purely additive. The base FTS
// answer is still correct without it.
func (t *kaiSearchTool) enrich(h *graph.SearchHit, queryToken string) {
	if h == nil || queryToken == "" {
		return
	}
	projectPath := t.resolveProjectPath(h.Project)
	if projectPath == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(projectPath, h.Path))
	if err != nil {
		return
	}
	h.Line = findFirstMatchLine(string(body), queryToken)
	if h.Line == 0 {
		return
	}

	fileNodes, err := t.grapher.FindNodesByPayloadPath(string(graph.KindFile), h.Path)
	if err != nil || len(fileNodes) == 0 {
		return
	}
	edges, err := t.grapher.GetEdgesByDst(graph.EdgeDefinesIn, fileNodes[0].ID)
	if err != nil {
		return
	}
	// Find the symbol whose line range covers the match. Tree-sitter
	// emits 0-based lines; our match scan is 1-based, so compare
	// against (start+1, end+1).
	var bestName string
	var bestSpan int = 1 << 30
	for _, e := range edges {
		n, err := t.grapher.GetNode(e.Src)
		if err != nil || n == nil {
			continue
		}
		start, end, ok := symbolLineRange(n.Payload)
		if !ok {
			continue
		}
		if h.Line < start+1 || h.Line > end+1 {
			continue
		}
		// When multiple symbols enclose the line (nested function /
		// method inside class), pick the tightest one — that's the
		// most specific owner of the match.
		span := end - start
		if span < bestSpan {
			bestSpan = span
			name, _ := n.Payload["fqName"].(string)
			bestName = name
		}
	}
	h.Symbol = bestName
}

// resolveProjectPath turns the project name on a hit (which is a
// directory basename, per the indexer's convention) into an
// absolute filesystem path. Falls back to the tool's workspace for
// single-root sessions where every hit carries the same project.
func (t *kaiSearchTool) resolveProjectPath(project string) string {
	if t.set != nil {
		if proj := t.set.ByName(project); proj != nil {
			return proj.Path
		}
	}
	if t.workspace != "" {
		return t.workspace
	}
	return ""
}

// firstSearchToken extracts a single identifier-shaped token from
// an FTS5 query so the text scan in enrich() has something concrete
// to find. Strips quotes, operators, and prefix wildcards. Returns
// empty if the query is all operators (unusual but possible).
func firstSearchToken(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	for _, sep := range []string{" AND ", " OR ", " NOT ", " NEAR ", "(", ")"} {
		q = strings.ReplaceAll(q, sep, " ")
	}
	for _, f := range strings.Fields(q) {
		f = strings.Trim(f, `"'*`)
		f = strings.TrimSpace(f)
		if f != "" && f != "AND" && f != "OR" && f != "NOT" {
			return f
		}
	}
	return ""
}

// findFirstMatchLine returns the 1-based line number of the first
// occurrence of needle in body. Case-insensitive to match FTS5's
// unicode61 tokenizer behavior. Returns 0 when no match — the
// caller treats this as "couldn't enrich; render the hit anyway."
func findFirstMatchLine(body, needle string) int {
	lower := strings.ToLower(body)
	lowerNeedle := strings.ToLower(needle)
	idx := strings.Index(lower, lowerNeedle)
	if idx < 0 {
		return 0
	}
	return strings.Count(body[:idx], "\n") + 1
}

// symbolLineRange extracts the [start, end] line range from a
// Symbol node's payload. The on-disk payload format is
// `range: {start: [line, col], end: [line, col]}` after JSON
// unmarshal, which gives us nested map[string]interface{} +
// []interface{} of float64. Returns ok=false for malformed payloads
// — never panics on a missing field.
func symbolLineRange(payload map[string]interface{}) (start, end int, ok bool) {
	rng, _ := payload["range"].(map[string]interface{})
	if rng == nil {
		return 0, 0, false
	}
	s, _ := rng["start"].([]interface{})
	e, _ := rng["end"].([]interface{})
	if len(s) < 1 || len(e) < 1 {
		return 0, 0, false
	}
	startF, sOk := s[0].(float64)
	endF, eOk := e[0].(float64)
	if !sOk || !eOk {
		return 0, 0, false
	}
	return int(startF), int(endF), true
}

// backfill walks every project (or just the workspace for single-
// root) and indexes every text file under it. Returns the number of
// files indexed. Skips ignored dirs and non-text extensions —
// indexing a 50MB minified bundle would explode the FTS tokenizer
// budget and produce search hits the agent can't act on.
// ensureProjectsBackfilled is the multi-root-aware lazy backfill.
// For each project in t.set (or just t.workspace in single-root
// mode), it checks the FTS index for that project's row count and
// triggers a backfill if zero. Returns the total number of newly
// indexed files; zero means everything was already covered.
//
// The legacy behavior (single total-count check, then once.Do for
// the whole workspace) couldn't recover when a workspace expanded
// after first index: the total stayed > 0 because the primary was
// indexed, but new sibling projects never got walked. This per-
// project check is the structural fix.
// searchBackfillRoot pairs a project name (used as the FTS row key)
// with the on-disk directory to walk. Package-level so backfillRoots
// and ensureProjectsBackfilled share the same type without an
// awkward struct conversion.
type searchBackfillRoot struct {
	project string
	dir     string
}

func (t *kaiSearchTool) ensureProjectsBackfilled(ctx context.Context) (int, error) {
	backfillMu.Lock()
	defer backfillMu.Unlock()
	var allRoots []searchBackfillRoot
	if t.set != nil && len(t.set.Projects()) > 1 {
		for _, proj := range t.set.Projects() {
			allRoots = append(allRoots, searchBackfillRoot{
				project: filepath.Base(proj.Path),
				dir:     proj.Path,
			})
		}
	} else if t.workspace != "" {
		allRoots = append(allRoots, searchBackfillRoot{project: filepath.Base(t.workspace), dir: t.workspace})
	} else {
		return 0, fmt.Errorf("no workspace or projects configured")
	}

	// Backfill projects that are missing OR only PARTIALLY indexed.
	// "Partial" = fewer FTS rows than indexable files on disk — the old
	// `count == 0` gate treated a stale 1-file partial as "done" and
	// never completed it (kai-tui stuck at 1/~490). The per-session memo
	// keeps the "walk once" optimization: a project confirmed complete
	// (or just backfilled) isn't disk-counted again this process.
	if t.backfilled == nil {
		t.backfilled = map[string]bool{}
	}
	var missing []searchBackfillRoot
	for _, r := range allRoots {
		if t.backfilled[r.project] {
			continue
		}
		if t.db.CountFileTextForProject(r.project) >= countIndexableFiles(r.dir) {
			t.backfilled[r.project] = true // already complete
			continue
		}
		missing = append(missing, r)
	}
	if len(missing) == 0 {
		return 0, nil
	}
	n, err := t.backfillRoots(ctx, missing)
	if err == nil {
		for _, r := range missing {
			t.backfilled[r.project] = true
		}
	}
	return n, err
}

// backfillRoots walks the given roots and indexes their files. Split
// out from ensureProjectsBackfilled so the project-list resolution
// stays a separate concern from the walk itself.
func (t *kaiSearchTool) backfillRoots(ctx context.Context, roots []searchBackfillRoot) (int, error) {
	var indexed int
	for _, r := range roots {
		// Wipe before re-indexing so a deleted file doesn't linger
		// as a ghost result. Cheap on first index (no-op delete) and
		// correct on re-index.
		_ = t.db.ClearFileTextForProject(r.project)
		err := filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if path != r.dir && fsIgnoreDirs[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if !fsTextExtensions[ext] {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() > maxIndexableSize {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return nil // best-effort
			}
			rel, _ := filepath.Rel(r.dir, path)
			rel = filepath.ToSlash(rel)
			if err := t.db.IndexFile(r.project, rel, string(body)); err != nil {
				return nil // skip on per-file failure rather than abort the whole backfill
			}
			indexed++
			return nil
		})
		if err != nil && err != fs.SkipAll {
			return indexed, err
		}
	}
	return indexed, nil
}

// dropWorktreeHits filters out search hits whose path lives under a
// git worktree directory (.claude/worktrees/<name>/...). Those hits
// come from a one-time index walk that happened before .claude was
// added to fsIgnoreDirs; the underlying files are mirrors of source
// already present in the workspace root and indexed under it, so
// surfacing them as separate hits triples or quintuples result
// counts without adding information. Filter at query time so
// stale index rows do not mislead the planner.
func dropWorktreeHits(hits []graph.SearchHit) []graph.SearchHit {
	out := hits[:0]
	for _, h := range hits {
		if strings.Contains(h.Path, ".claude/worktrees/") {
			continue
		}
		out = append(out, h)
	}
	return out
}

// zeroMatchHint returns a one-paragraph suggestion appended to the
// "no matches" response from kai_search / kai_grep / kai_files. The
// 2026-05-27 dogfoods (the /exit autocomplete + the early snapshot_
// count runs) pinned the failure shape this addresses: planner keeps
// rephrasing the same query against the same project instead of
// widening scope or switching approach. Three concrete actions, in
// the order they tend to pay off:
//
//   1. If scoped to a specific project, the answer may live in a
//      sibling project (the workspace overview names them — re-read
//      it). Drop the project filter to search all projects.
//   2. If the query looks like a field/identifier you EXPECT in CLI
//      output (snake_case or PascalCase), run the command via bash
//      first — the field may not exist in source even if the command
//      emits it (struct tag, fmt.Printf, template).
//   3. Open the likely file directly via view — for UI behavior or
//      tightly-bound features the answer often isn't grep-shaped.
//
// Single paragraph, no bullet list — the planner reads tool results
// densely and structured prose surfaces actionable hints better than
// numbered options that get skimmed.
func zeroMatchHint(query string, scopedToProject bool) string {
	looksLikeIdentifier := isLikelyIdentifierQuery(query)
	var parts []string
	if scopedToProject {
		parts = append(parts, "the answer may live in a sibling project (check the workspace overview at turn 0) — drop the project filter to search all projects")
	}
	if looksLikeIdentifier {
		parts = append(parts, "the query looks like an identifier you expect in CLI output — run the command via bash (e.g. `<cli> --help` then `<cli> <subcmd> --json`) before assuming source defines this name")
	}
	parts = append(parts, "open the likely file directly via view if you have a strong prior about where the feature lives")
	return "(zero matches — " + strings.Join(parts, "; ") + ")"
}

// isLikelyIdentifierQuery returns true when the query looks like a
// programming-language identifier the user expects in CLI output:
// snake_case, camelCase, PascalCase, or all-caps. No spaces, no
// quotes, no boolean operators. False for free-text queries.
func isLikelyIdentifierQuery(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" || strings.ContainsAny(q, " \t\"'") {
		return false
	}
	if strings.Contains(q, " OR ") || strings.Contains(q, " AND ") || strings.Contains(q, " NOT ") {
		return false
	}
	// Identifier-ish: only [A-Za-z0-9_]. Allow leading underscore
	// but not leading digit (that would be a number, not an ident).
	if q[0] >= '0' && q[0] <= '9' {
		return false
	}
	for _, r := range q {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}
// rewritePipeAlternation converts regex-style `a|b|c` queries into
// FTS5's `a OR b OR c`. FTS5's MATCH grammar uses the keyword `OR`
// for alternation; `|` is not a valid operator and produces a
// "syntax error near '|'" SQL error. Models writing kai_search
// queries default to the regex idiom, so we translate.
//
// Conservative: if the query contains a double-quoted phrase, we
// leave it alone — pipes inside a phrase are literal characters
// the caller wants matched. The simple unquoted case is what
// caused the dogfood breakage and is what we fix here.
func rewritePipeAlternation(q string) string {
	if !strings.Contains(q, "|") {
		return q
	}
	if strings.Contains(q, "\"") {
		return q
	}
	parts := strings.Split(q, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return q
	}
	return strings.Join(out, " OR ")
}

// rewriteIdentifierSeparators turns Go-/Python-/JS-flavored identifier
// punctuation into FTS5-friendly spaces. FTS5 reads `.` and `:` as
// query-grammar metacharacters: `foo.Bar` raises "syntax error near
// '.'", `coverage:tests` raises "no such column: coverage". Models
// naturally type those forms when grounding in source identifiers
// (set.Close, pkg.Func, currentDir:fallback, etc.). Replacing them
// with spaces lets FTS5 AND the resulting tokens, returning the same
// intent — same shape and same rationale as rewritePipeAlternation.
//
// Conservative: if the query contains a double-quoted phrase, leave
// it alone — the caller chose phrase mode and means the punctuation
// literally (e.g. searching for "kai stats --json" or "v0.32.69").
func rewriteIdentifierSeparators(q string) string {
	if strings.Contains(q, "\"") {
		return q
	}
	if !strings.ContainsAny(q, ".:") {
		return q
	}
	return strings.NewReplacer(".", " ", ":", " ").Replace(q)
}
