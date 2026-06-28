package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kaicontext/kai-engine/projects"
)

// kai_files and kai_grep are filesystem-backed tools that replace
// the agent's bash fallbacks for `find` and `grep -rn`. Both walk
// the workspace skipping a hardcoded noise filter (node_modules,
// .git, build outputs) so the model gets clean results without
// having to know what to ignore.

// fsIgnoreDirs is the set of directory names skipped wholesale
// during walks. Mirrors overviewIgnore in graph_context.go but
// lives here independently so this tool file has no cross-file
// coupling. Directories starting with "." that aren't in this
// list (e.g. ".github") are walked normally.
var fsIgnoreDirs = map[string]bool{
	".git":         true,
	".kai":         true,
	".claude":      true, // claude agent state + .claude/worktrees/ git-worktree mirrors (the latter pollute kai_search with 5x duplicates of the same source files — 2026-05-26 planner trace burned 6 turns chasing phantom matches in worktree copies)
	".idea":        true,
	".vscode":      true,
	".cache":       true,
	".next":        true,
	".nuxt":        true,
	".venv":        true,
	"venv":         true,
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"build":        true,
	"dist":         true,
	"__pycache__":  true,
}

// fsBinaryExtensions is a quick-skip list for kai_grep so we don't
// burn time scanning binaries (images, archives, executables).
// Cheap heuristic; not exhaustive.
var fsBinaryExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".ico": true, ".svg": false, // svg is text-XML, keep
	".pdf": true, ".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".o": true, ".a": true, ".lib": true,
	".class": true, ".jar": true,
	".mp3": true, ".mp4": true, ".mov": true, ".avi": true, ".wav": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".sqlite": true, ".db": true,
}

// Output budgets for kai_files / kai_grep. Tight on purpose: tool
// results live in the conversation history and get re-sent every
// turn until compaction kicks in, so each chunky result lands as
// recurring tax on the next several requests. The model can issue
// a follow-up call with a narrower query if the cap clips real
// signal — that costs one tool call, vs. several thousand
// repeated tokens.
const (
	fsMaxFiles      = 100 // path-list cap for kai_files
	fsMaxGrepHits   = 60  // hit cap for kai_grep
	fsMaxLineBytes  = 160 // truncated length of a single grep match line
	fsMaxFileBytes  = 1 << 20
)

// --- kai_files --------------------------------------------------------

type kaiFilesTool struct {
	workspace string
	// set is the multi-root project set (when configured). When
	// non-nil and the caller passes no `path` arg, the walk
	// traverses EVERY root in the set rather than only the
	// primary workspace. Without this, kai_files in a multi-root
	// session silently misses files in sibling roots — the
	// "where do the docs live" failure that took users multiple
	// turns to figure out.
	//
	// When `path` IS given, we route the walk through Set.ProjectFor
	// (via scopeDir on each root) so the user can scope to a single
	// project explicitly with e.g. path="kai-server/docs-site".
	set *projects.Set
}

type kaiFilesParams struct {
	// Pattern is a glob like "*.go" or "**/*.test.js" matched
	// against file paths relative to Path. Empty matches everything
	// under Path (still subject to the noise filter).
	Pattern string `json:"pattern"`
	// Path is an optional sub-directory to scope the search to.
	// Empty means the whole workspace.
	Path string `json:"path"`
}

func (t *kaiFilesTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_files",
		Description: "List workspace files matching a glob pattern. Replaces `find -name`. " +
			"Automatically skips node_modules, .git, build outputs, and other noise. " +
			"Use this — NOT bash find/ls — when you need to discover files in the workspace.",
		Parameters: map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Glob pattern (e.g. \"*.go\", \"**/*.test.js\", \"src/*.ts\"). Empty lists everything under path.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Optional subdirectory to scope to (workspace-relative). Empty = whole workspace.",
			},
		},
		Required: []string{},
	}
}

func (t *kaiFilesTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiFilesParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_files: invalid input json: " + err.Error()), nil
		}
	}
	pattern := strings.TrimSpace(p.Pattern)
	matchFn, err := compileGlob(pattern)
	if err != nil {
		return NewTextErrorResponse("kai_files: bad pattern: " + err.Error()), nil
	}

	// Multi-root walk: when no `path` arg is given AND the set
	// has multiple roots, search every root and prefix matches
	// with the project name. With one root (or a path arg), the
	// behavior matches the legacy single-root walk so existing
	// tests stay green.
	roots := []walkRoot{}
	if strings.TrimSpace(p.Path) == "" && t.set != nil && len(t.set.Projects()) > 1 {
		var names []string
		for _, proj := range t.set.Projects() {
			roots = append(roots, walkRoot{
				dir:        proj.Path,
				// Directory basename as the path prefix — matches the
			// indexer convention (kai_search uses filepath.Base too)
			// and avoids "Kai Server/..." outputs from README-derived
			// names with spaces. See agent_planner.go for full reasoning.
			prefix:     filepath.Base(proj.Path) + "/",
				rebaseFrom: proj.Path,
			})
			names = append(names, proj.Name)
		}
		TraceRouting("kai_files pattern=%q scope=* → walked=%s", pattern, strings.Join(names, ","))
	} else {
		root, err := scopeDirInSet(t.set, t.workspace, p.Path)
		if err != nil {
			TraceRouting("kai_files pattern=%q scope=%q → ERROR %v", pattern, p.Path, err)
			return NewTextErrorResponse("kai_files: " + err.Error()), nil
		}
		roots = []walkRoot{{dir: root, rebaseFrom: t.workspace}}
		TraceRouting("kai_files pattern=%q scope=%q → root=%s", pattern, p.Path, root)
	}

	var matches []string
	for _, r := range roots {
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
			name := d.Name()
			if d.IsDir() {
				if path != r.dir && fsIgnoreDirs[name] {
					return fs.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(r.rebaseFrom, path)
			rel = filepath.ToSlash(rel)
			if matchFn(rel, name) {
				matches = append(matches, r.prefix+rel)
				if len(matches) >= fsMaxFiles {
					return fs.SkipAll
				}
			}
			return nil
		})
		if err != nil && err != fs.SkipAll {
			return NewTextErrorResponse("kai_files: " + err.Error()), nil
		}
		if len(matches) >= fsMaxFiles {
			break
		}
	}
	if len(matches) == 0 {
		return NewTextResponse(fmt.Sprintf("kai_files: no matches for %q under %q\n\n%s", pattern, p.Path, zeroMatchHint(pattern, p.Path != ""))), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "kai_files: %d match(es)", len(matches))
	if len(matches) >= fsMaxFiles {
		b.WriteString(" (capped — refine pattern for more)")
	}
	if len(roots) > 1 {
		b.WriteString(" across " + fmt.Sprintf("%d roots", len(roots)))
	}
	b.WriteString("\n")
	for _, m := range matches {
		fmt.Fprintf(&b, "  %s\n", m)
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// walkRoot pairs a directory with its display-prefix and the
// path-base used to compute relative output. Used by the
// multi-root walks so each match line shows
// "<project>/<rel-path>" instead of an ambiguous
// "/absolute/system/path" or root-confusing relative form.
type walkRoot struct {
	dir        string // absolute dir to walk
	prefix     string // prepended to each match (e.g. "kai-server/"); empty for single-root
	rebaseFrom string // path used as filepath.Rel base
}

// --- kai_grep ---------------------------------------------------------

type kaiGrepTool struct {
	workspace  string
	symbolHook symbolGrepHook
	// set: same multi-root awareness as kaiFilesTool — when no
	// `path` arg is given, walks every project root in the set.
	// Critical for the canonical "where is X?" use case in a
	// multi-root workspace.
	set *projects.Set
}

type kaiGrepParams struct {
	// Query is the text or regex to search for. Treated as a
	// literal substring unless Regex is true.
	Query string `json:"query"`
	// Regex switches Query interpretation to a Go regexp.
	Regex bool `json:"regex"`
	// Path is an optional subdirectory to scope to.
	Path string `json:"path"`
	// Glob, when set, restricts which files get scanned (e.g.
	// "*.go" to grep only Go files).
	Glob string `json:"glob"`
	// CaseInsensitive flips the match to be case-insensitive.
	// For regex queries the (?i) prefix is also honored.
	CaseInsensitive bool `json:"case_insensitive"`
}

func (t *kaiGrepTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_grep",
		Description: "Regex/substring text search via filesystem walk. " +
			"PREFER kai_search FIRST for any free-text question — it's faster (FTS5-indexed, sub-10ms), " +
			"BM25-ranked, and skips empty projects automatically. Use kai_grep ONLY when you need " +
			"regex matching that FTS5 can't express, or when kai_search returned no hits and you " +
			"suspect uncommitted/freshly-edited files (the FTS index lags behind disk). " +
			"Skips node_modules, .git, build outputs, and binary files. " +
			"Output is grouped per file (path + hit count + 3 sample lines per file) — much fewer tokens than flat path:line:text. " +
			"When the query looks like a single identifier and is defined in kai's parsed graph, " +
			"the result short-circuits to a 'defined in X / called by Y' summary instead of a text scan. " +
			"DO NOT use kai_grep to verify whether a CLI command exposes a JSON field — run the command (via bash) and look at its actual output instead. " +
			"A JSON field can be emitted by a struct tag, a fmt.Printf, or a templated string, and grep across source for the field name will miss real emitters and surface unrelated mentions. The 2026-05-26 dogfood burned 6 turns grepping for 'snapshot_count' / 'SnapshotCount' / 'total_snapshots' in source when 'kai stats --json' would have answered 'that field does not exist' in one bash call.",
		Parameters: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Substring (literal) to search for, or a Go regexp when regex=true.",
			},
			"regex": map[string]any{
				"type":        "boolean",
				"description": "Treat query as a Go-syntax regexp instead of a literal substring.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Optional subdirectory to scope to (workspace-relative).",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": "Optional glob to restrict which files get scanned (e.g. \"*.go\", \"**/*.test.js\").",
			},
			"case_insensitive": map[string]any{
				"type":        "boolean",
				"description": "Match case-insensitively. For regex queries the (?i) prefix is also honored.",
			},
		},
		Required: []string{"query"},
	}
}

func (t *kaiGrepTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiGrepParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_grep: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.Query) == "" {
		return NewTextErrorResponse("kai_grep: query required"), nil
	}
	// Symbol-aware fast path: if the query is identifier-shaped
	// and the graph knows it as a defined symbol, skip the text
	// walk and return the structurally-richer "defined in / called
	// by" summary. The agent gets a more useful result for fewer
	// tokens; on a miss we silently fall through to the regular
	// walk. Skipped when the caller explicitly asked for regex
	// matching since that's unambiguously a text query.
	if !p.Regex && t.symbolHook != nil {
		if out, ok := t.symbolHook.tryGrepSymbol(p.Query); ok {
			return NewTextResponse(out), nil
		}
	}
	matchFn, err := compileGlob(p.Glob)
	if err != nil {
		return NewTextErrorResponse("kai_grep: bad glob: " + err.Error()), nil
	}

	matcher, err := buildMatcher(p)
	if err != nil {
		return NewTextErrorResponse("kai_grep: bad query: " + err.Error()), nil
	}

	// Multi-root walk same as kai_files: when no `path` arg AND
	// the set has multiple roots, scan EVERY root and prefix
	// results with the project name. This is the actual fix for
	// "agent says X doesn't exist when it's in a sibling root."
	roots := []walkRoot{}
	if strings.TrimSpace(p.Path) == "" && t.set != nil && len(t.set.Projects()) > 1 {
		var names []string
		for _, proj := range t.set.Projects() {
			roots = append(roots, walkRoot{
				dir:        proj.Path,
				// Directory basename as the path prefix — matches the
			// indexer convention (kai_search uses filepath.Base too)
			// and avoids "Kai Server/..." outputs from README-derived
			// names with spaces. See agent_planner.go for full reasoning.
			prefix:     filepath.Base(proj.Path) + "/",
				rebaseFrom: proj.Path,
			})
			names = append(names, proj.Name)
		}
		TraceRouting("kai_grep query=%q scope=* → walked=%s", p.Query, strings.Join(names, ","))
	} else {
		root, err := scopeDirInSet(t.set, t.workspace, p.Path)
		if err != nil {
			TraceRouting("kai_grep query=%q scope=%q → ERROR %v", p.Query, p.Path, err)
			return NewTextErrorResponse("kai_grep: " + err.Error()), nil
		}
		roots = []walkRoot{{dir: root, rebaseFrom: t.workspace}}
		TraceRouting("kai_grep query=%q scope=%q → root=%s", p.Query, p.Path, root)
	}

	var hits []grepHit
	stopped := false
	for _, r := range roots {
		err = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, walkErr error) error {
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
		rel, _ := filepath.Rel(r.rebaseFrom, path)
		rel = filepath.ToSlash(rel)
		displayPath := r.prefix + rel
		if !matchFn(rel, d.Name()) {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if fsBinaryExtensions[ext] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.Size() > fsMaxFileBytes {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			return nil
		}
		// Cheap NUL-byte sniff catches binaries that slipped the
		// extension filter. If the first 8KB has a NUL we skip.
		head := data
		if len(head) > 8192 {
			head = head[:8192]
		}
		for _, b := range head {
			if b == 0 {
				return nil
			}
		}
		// Per-file scan cap: stop after enough samples so a hot
		// file (e.g. a vendored bundle that slipped the noise
		// filter, or a docs file that name-drops the query 50
		// times) can't monopolize the global hit budget.
		const perFileScan = 8
		fileHits := 0
		for i, line := range strings.Split(string(data), "\n") {
			if matcher(line) {
				hits = append(hits, grepHit{path: displayPath, line: i + 1, text: strings.TrimRight(line, "\r")})
				fileHits++
				if len(hits) >= fsMaxGrepHits {
					stopped = true
					return fs.SkipAll
				}
				if fileHits >= perFileScan {
					return nil
				}
			}
		}
		return nil
	})
	if err != nil && err != fs.SkipAll {
		return NewTextErrorResponse("kai_grep: " + err.Error()), nil
	}
	if stopped {
		break
	}
	}
	if len(hits) == 0 {
		// Auto-promote to regex on a footgun-prone zero-hit case: the
		// literal search found nothing AND the query contains regex
		// metacharacters AND the regex would compile. This catches the
		// case where the agent typed `foo|BAR` expecting alternation —
		// the literal pipe was searched for and obviously not found,
		// but the user clearly meant a regex. Every other code-search
		// tool (rg, ag, grep -E) takes alternation by default; kai_grep
		// historically required explicit p.Regex=true and silently
		// returned "no matches" otherwise, which is the kind of bug
		// that wastes 12 tool calls and gets the agent to conclude
		// "the feature doesn't exist" when it clearly does.
		//
		// Only fires when p.Regex was false (don't double-process
		// explicit regex queries) and the query has the obvious
		// regex shape. Failure to compile = stay with the literal
		// "no matches" — we never silently change semantics on a
		// query the agent might have meant literally.
		if !p.Regex && looksLikeRegex(p.Query) {
			retried, ok := retryAsRegex(ctx, p, roots, stopped)
			if ok && len(retried) > 0 {
				return NewTextResponse(formatGrepHits(retried, false) +
					"\n\n(note: query was auto-promoted to regex — saw a regex metacharacter and the literal search returned no matches. To control this explicitly, pass regex=true.)"), nil
			}
		}
		return NewTextResponse(fmt.Sprintf("kai_grep: no matches for %q\n\n%s", p.Query, zeroMatchHint(p.Query, p.Path != ""))), nil
	}
	return NewTextResponse(formatGrepHits(hits, stopped)), nil
}

// looksLikeRegex reports whether q contains a character that's
// likely-but-not-certain regex syntax. The conservative set: pipe,
// parens, square brackets, plus, question mark, caret, dollar,
// backslash, asterisk. A bare ``.`` is too common in filenames /
// version strings to count.
func looksLikeRegex(q string) bool {
	return strings.ContainsAny(q, `|()[]+?^$\*`)
}

// retryAsRegex re-runs the per-root walk with the query interpreted
// as a regex. Returns hits + true on success; nil + false when the
// regex fails to compile (caller falls back to the literal "no
// matches" response). Reuses the same roots set so we don't repeat
// the scope resolution.
func retryAsRegex(ctx context.Context, p kaiGrepParams, roots []walkRoot, _ bool) ([]grepHit, bool) {
	retryP := p
	retryP.Regex = true
	matcher, err := buildMatcher(retryP)
	if err != nil {
		return nil, false // not a valid regex; agent meant it literally
	}
	matchFn, err := compileGlob(p.Glob)
	if err != nil {
		return nil, false
	}
	var hits []grepHit
	for _, r := range roots {
		_ = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, walkErr error) error {
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
			rel, _ := filepath.Rel(r.rebaseFrom, path)
			rel = filepath.ToSlash(rel)
			displayPath := r.prefix + rel
			if !matchFn(rel, d.Name()) {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if fsBinaryExtensions[ext] {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() == 0 || info.Size() > fsMaxFileBytes {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			if len(data) > 8192 {
				if bytes.IndexByte(data[:8192], 0) >= 0 {
					return nil
				}
			} else if bytes.IndexByte(data, 0) >= 0 {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if matcher(line) {
					hits = append(hits, grepHit{path: displayPath, line: i + 1, text: line})
					if len(hits) >= fsMaxGrepHits {
						return fs.SkipAll
					}
				}
			}
			return nil
		})
	}
	return hits, true
}

// formatGrepHits renders the per-file-grouped output kai_grep
// returns. Two-line summary block: a header showing total counts,
// then one block per file with the first few samples + a "+N
// more" pointer when there are extras.
//
// Why grouped instead of flat: the flat path:line:text form repeats
// the path on every hit (60 hits in 6 files = 60 path repetitions),
// and every match line carries the full source context the model
// usually doesn't need. Grouping deduplicates the path, surfaces
// the per-file count up front (the signal the agent actually uses
// to decide where to look), and shows just enough sample lines for
// the agent to recognize the pattern. Empirically this is ~5×
// fewer tokens than the flat form for the same query.
// grepHit is one matched line. Lifted to package scope so the
// renderer (formatGrepHits) can take it as input — the previous
// anonymous-struct form lived inside Run() and couldn't escape.
type grepHit struct {
	path string
	line int
	text string
}

func formatGrepHits(hits []grepHit, capped bool) string {
	type fileGroup struct {
		path string
		hits []grepHit
	}
	order := []string{}
	groups := map[string]*fileGroup{}
	for _, h := range hits {
		g, ok := groups[h.path]
		if !ok {
			g = &fileGroup{path: h.path}
			groups[h.path] = g
			order = append(order, h.path)
		}
		g.hits = append(g.hits, h)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "kai_grep: %d hit(s) across %d file(s)", len(hits), len(order))
	if capped {
		b.WriteString(" (capped — refine query/path/glob for more)")
	}
	b.WriteByte('\n')

	const samplesPerFile = 3
	for _, path := range order {
		g := groups[path]
		fmt.Fprintf(&b, "  %s [%d]\n", g.path, len(g.hits))
		shown := g.hits
		if len(shown) > samplesPerFile {
			shown = shown[:samplesPerFile]
		}
		for _, h := range shown {
			text := strings.TrimSpace(h.text)
			if len(text) > fsMaxLineBytes {
				text = text[:fsMaxLineBytes] + "…"
			}
			fmt.Fprintf(&b, "    L%d: %s\n", h.line, text)
		}
		if extra := len(g.hits) - len(shown); extra > 0 {
			fmt.Fprintf(&b, "    … +%d more in this file\n", extra)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- kai_tree ---------------------------------------------------------

type kaiTreeTool struct {
	workspace string
	// set: when no `path` arg is given AND set has multiple
	// roots, kai_tree renders one subtree per root with a
	// "── Project: <name> ──" separator so the agent sees the
	// full multi-root layout instead of just the primary.
	set *projects.Set
}

type kaiTreeParams struct {
	// Path is the workspace-relative directory to inspect. Empty
	// means the workspace root.
	Path string `json:"path"`
	// Depth is how many levels deep to descend. 1 (default) shows
	// just immediate children — the natural replacement for `ls`.
	// 2 or 3 helps the model orient inside a folder without
	// follow-up calls. Capped at 4 to stop a runaway descent on
	// monorepos.
	Depth int `json:"depth"`
}

func (t *kaiTreeTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_tree",
		Description: "Show a directory tree with child counts on subdirectories. Replaces `ls` and `tree`. " +
			"Default depth=2 lets one call cover a folder + its children — usually enough to plan " +
			"where to look next without follow-up calls. " +
			"Subdirectory entries at the leaf level include `[N children]` counts so you can decide " +
			"where to drill in without a probing call. Skips node_modules, .git, build outputs.",
		Parameters: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Workspace-relative directory. Empty = root.",
			},
			"depth": map[string]any{
				"type":        "integer",
				"description": "Levels deep (1-4). Default 2 covers a dir + its immediate subdirs in one call.",
				"default":     2,
			},
		},
		Required: []string{},
	}
}

func (t *kaiTreeTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiTreeParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_tree: invalid input json: " + err.Error()), nil
		}
	}
	// Multi-root: when no `path` arg AND set has multiple roots,
	// concatenate one tree per root with a "── Project: <name>
	// ──" header. Renders the actual workspace layout the agent
	// is operating in instead of just the primary root.
	if strings.TrimSpace(p.Path) == "" && t.set != nil && len(t.set.Projects()) > 1 {
		var names []string
		for _, proj := range t.set.Projects() {
			names = append(names, proj.Name)
		}
		TraceRouting("kai_tree scope=* → walked=%s", strings.Join(names, ","))
		return t.runMultiRoot(ctx, p)
	}
	root, err := scopeDirInSet(t.set, t.workspace, p.Path)
	if err != nil {
		TraceRouting("kai_tree scope=%q → ERROR %v", p.Path, err)
		return NewTextErrorResponse("kai_tree: " + err.Error()), nil
	}
	TraceRouting("kai_tree scope=%q → root=%s", p.Path, root)
	depth := p.Depth
	if depth < 1 {
		depth = 2 // default: parent + children in one shot
	}
	if depth > 4 {
		depth = 4
	}

	const (
		perDirCap   = 40  // per-directory cap; truncate with "+N more"
		totalCap    = 250 // global entry cap; stop walking past this
	)

	var b strings.Builder
	displayRoot := p.Path
	if displayRoot == "" {
		displayRoot = "."
	}
	fmt.Fprintf(&b, "kai_tree: %s (depth %d)\n", displayRoot, depth)

	totalEmitted := 0
	walkAborted := false
	var walk func(dir string, level int) error
	walk = func(dir string, level int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if totalEmitted >= totalCap {
			walkAborted = true
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var dirs, files []string
		for _, e := range entries {
			name := e.Name()
			if fsIgnoreDirs[name] {
				continue
			}
			if e.IsDir() {
				dirs = append(dirs, name)
			} else {
				files = append(files, name)
			}
		}
		sort.Strings(dirs)
		sort.Strings(files)
		all := append(dirs, files...)
		extra := 0
		if len(all) > perDirCap {
			extra = len(all) - perDirCap
			all = all[:perDirCap]
			dirs = nil
			files = nil
			for _, n := range all {
				full := filepath.Join(dir, n)
				if info, err := os.Stat(full); err == nil && info.IsDir() {
					dirs = append(dirs, n)
				} else {
					files = append(files, n)
				}
			}
		}
		indent := strings.Repeat("  ", level)
		for _, name := range dirs {
			willRecurse := level+1 < depth
			if willRecurse {
				fmt.Fprintf(&b, "%s%s/\n", indent, name)
			} else {
				// Leaf-level dir: annotate with child count so the
				// model can decide whether to drill in without a
				// probing kai_tree call. Cheap (one extra ReadDir
				// per dir at the leaf depth).
				count := countDirEntries(filepath.Join(dir, name))
				fmt.Fprintf(&b, "%s%s/  [%d]\n", indent, name, count)
			}
			totalEmitted++
			if willRecurse {
				if err := walk(filepath.Join(dir, name), level+1); err != nil {
					return err
				}
				if walkAborted {
					return nil
				}
			}
		}
		for _, name := range files {
			fmt.Fprintf(&b, "%s%s\n", indent, name)
			totalEmitted++
			if totalEmitted >= totalCap {
				walkAborted = true
				return nil
			}
		}
		if extra > 0 {
			fmt.Fprintf(&b, "%s… +%d more\n", indent, extra)
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return NewTextErrorResponse("kai_tree: " + err.Error()), nil
	}
	if walkAborted {
		b.WriteString("(output capped at ~250 entries — narrow `path` for more)")
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// runMultiRoot is the multi-root variant of kai_tree. Renders one
// header-prefixed tree per project root in the set, with depth
// halved (default 1 instead of 2) so the combined output stays
// scannable on workspaces with 5+ roots. Drilling deeper is one
// path-arg call away.
func (t *kaiTreeTool) runMultiRoot(ctx context.Context, p kaiTreeParams) (ToolResponse, error) {
	depth := p.Depth
	if depth < 1 {
		// Multi-root default 1 vs single-root default 2: a depth-2
		// scan over 5 roots blows the per-call entry budget. The
		// agent gets the high-level "what projects exist + their
		// top-level dirs" picture and can drill into any one with
		// kai_tree path="<project>" depth=2.
		depth = 1
	}
	if depth > 4 {
		depth = 4
	}
	const (
		perDirCap = 40
		totalCap  = 250
	)
	var b strings.Builder
	fmt.Fprintf(&b, "kai_tree: multi-root workspace (%d projects, depth %d per project)\n",
		len(t.set.Projects()), depth)
	totalEmitted := 0
	walkAborted := false
	for _, proj := range t.set.Projects() {
		if walkAborted {
			break
		}
		// Same dir-basename convention as the path-prefix output below;
		// keeps the "── Project: kai-server ──" header consistent with
		// hits that render as "kai-server/...".
		fmt.Fprintf(&b, "\n── Project: %s (%s) ──\n", filepath.Base(proj.Path), proj.Path)
		var walk func(dir string, level int) error
		walk = func(dir string, level int) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if totalEmitted >= totalCap {
				walkAborted = true
				return nil
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil
			}
			var dirs, files []string
			for _, e := range entries {
				name := e.Name()
				if fsIgnoreDirs[name] {
					continue
				}
				if e.IsDir() {
					dirs = append(dirs, name)
				} else {
					files = append(files, name)
				}
			}
			sort.Strings(dirs)
			sort.Strings(files)
			all := append(dirs, files...)
			extra := 0
			if len(all) > perDirCap {
				extra = len(all) - perDirCap
				all = all[:perDirCap]
				dirs = nil
				files = nil
				for _, n := range all {
					full := filepath.Join(dir, n)
					if info, err := os.Stat(full); err == nil && info.IsDir() {
						dirs = append(dirs, n)
					} else {
						files = append(files, n)
					}
				}
			}
			indent := strings.Repeat("  ", level)
			for _, name := range dirs {
				willRecurse := level+1 < depth
				if willRecurse {
					fmt.Fprintf(&b, "%s%s/\n", indent, name)
				} else {
					count := countDirEntries(filepath.Join(dir, name))
					fmt.Fprintf(&b, "%s%s/  [%d]\n", indent, name, count)
				}
				totalEmitted++
				if willRecurse {
					if err := walk(filepath.Join(dir, name), level+1); err != nil {
						return err
					}
					if walkAborted {
						return nil
					}
				}
			}
			for _, name := range files {
				fmt.Fprintf(&b, "%s%s\n", indent, name)
				totalEmitted++
				if totalEmitted >= totalCap {
					walkAborted = true
					return nil
				}
			}
			if extra > 0 {
				fmt.Fprintf(&b, "%s… +%d more\n", indent, extra)
			}
			return nil
		}
		if err := walk(proj.Path, 0); err != nil {
			return NewTextErrorResponse("kai_tree: " + err.Error()), nil
		}
	}
	if walkAborted {
		b.WriteString("\n(output capped at ~250 entries — call kai_tree with path=\"<project>\" for one root in detail)")
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// countDirEntries returns the number of non-noise entries in dir.
// Used by kai_tree to annotate leaf-level subdirs with a child
// count so the agent can pick where to drill without a follow-up
// call. Errors collapse to 0 — the count is informational only.
func countDirEntries(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if fsIgnoreDirs[e.Name()] {
			continue
		}
		n++
	}
	return n
}

// walkIndexableFiles returns workspace-relative paths under
// scopeSub (empty = whole workspace) that are likely to be in the
// graph: source-code-ish text files, with the noise filter
// applied. Used by kai_symbols to enumerate files without
// requiring a graph index of "all paths under prefix" — we walk
// the fs cheaply and let the per-file graph query short-circuit
// when a file isn't indexed.
func walkIndexableFiles(workspace, scopeSub string) ([]string, error) {
	root, err := scopeDir(workspace, scopeSub)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && fsIgnoreDirs[name] {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if fsBinaryExtensions[ext] {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- helpers ----------------------------------------------------------

// scopeDir resolves the search root by joining the workspace with
// an optional sub-path. It rejects sub-paths that escape the
// workspace ("..", absolute paths) so a malicious or confused
// agent can't grep /etc/passwd via this tool.
//
// Forwards to scopeDirInSet with set=nil so existing callers
// (single-root tests, paths that don't need project-prefix
// awareness) keep working unchanged.
func scopeDir(workspace, sub string) (string, error) {
	return scopeDirInSet(nil, workspace, sub)
}

// scopeDirInSet is the multi-root-aware variant of scopeDir. When
// `set` has more than one project and `sub` begins with a known
// project name (e.g. "Kai/kai-cli/..."), it routes through that
// project's actual filesystem path. On unresolvable paths, the
// error includes a helpful hint listing project names so the
// agent can pivot on the first error instead of guessing
// 5+ path forms in a row (the 2026-05-11 opus dogfood failure
// where kai_grep with paths "kai-cli/...", "kai/packages/...", etc.
// all errored opaquely).
func scopeDirInSet(set *projects.Set, workspace, sub string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("workspace not configured")
	}
	if strings.TrimSpace(sub) == "" {
		return workspace, nil
	}
	if filepath.IsAbs(sub) {
		return "", fmt.Errorf("path must be relative to workspace, got %q", sub)
	}

	// Multi-root project-name resolution. Two shapes are accepted:
	//
	//   1. "kai-server"            — a bare project name, scopes to
	//                                that project's whole root. This
	//                                is the form an agent reaches for
	//                                naturally ("kai_grep X in
	//                                kai-server") and the form we want
	//                                to "just work."
	//   2. "kai-server/foo/bar"    — project name + path within. Used
	//                                when the planner has already
	//                                located a subdirectory.
	//
	// Both routes match the resolveInSet logic in file.go so
	// kai_grep / kai_files / kai_tree accept the same project-
	// prefixed paths the planner emits in a multi-root workspace.
	if set != nil && len(set.Projects()) > 1 {
		// Bare project name — the case that used to fall through to
		// filesystem lookup and fail with a misleading error.
		if !strings.Contains(sub, "/") {
			if proj := set.ByName(sub); proj != nil {
				return proj.Path, nil
			}
		}
		head, rest, found := strings.Cut(sub, "/")
		if found && head != "" {
			if proj := set.ByName(head); proj != nil {
				resolved := filepath.Clean(filepath.Join(proj.Path, rest))
				if rel, relErr := filepath.Rel(proj.Path, resolved); relErr == nil &&
					!strings.HasPrefix(rel, "..") {
					if _, err := os.Stat(resolved); err == nil {
						return resolved, nil
					}
				}
			}
		}
	}

	full := filepath.Join(workspace, sub)
	rel, err := filepath.Rel(workspace, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes workspace: %q", sub)
	}
	if _, err := os.Stat(full); err != nil {
		// Helpful error: in a multi-root workspace, list project
		// names + suggest a project-prefixed alternative + suggest
		// kai_files as the "find the right path" tool. Same shape
		// as resolveInSet's hint in file.go so the agent sees
		// consistent guidance across read tools.
		if set != nil && len(set.Projects()) > 1 {
			names := make([]string, 0, len(set.Projects()))
			for _, pr := range set.Projects() {
				names = append(names, pr.Name)
			}
			// Build the example carefully. If the user's input is
			// itself a known project name (sub matched ByName), the
			// "prefix with names[0]" advice produces nonsense like
			// "kai/kai-server" — and the agent will then try
			// "kai/kai/kai-server" on the next round. Suggest the
			// bare project name instead. Otherwise, names[0] + sub
			// is a sensible "you probably meant this project."
			var example string
			if proj := set.ByName(sub); proj != nil {
				example = proj.Name
			} else {
				example = filepath.Join(names[0], sub)
			}
			return "", fmt.Errorf(
				"path %q not found. Available projects: %s. "+
					"In a multi-root workspace, scope to a project by passing its name (e.g. %q), "+
					"or prefix a path within a project (e.g. %q). "+
					"If you don't know which project owns it, run kai_files with a glob like {\"glob\":\"**/%s\"} to list matches.",
				sub, strings.Join(names, ", "),
				example,
				filepath.Join(names[0], sub),
				filepath.Base(sub))
		}
		return "", fmt.Errorf("path %q: %w", sub, err)
	}
	return full, nil
}

// compileGlob builds a (relPath, baseName) → bool matcher. We
// accept either a `**/...` recursive glob or a plain pattern; an
// empty pattern matches everything (the noise filter still
// applies).
//
// The matcher tries the pattern against the basename first (so
// "*.go" works without forcing the user to write "**/*.go"), then
// against the full relative path. Callers can write either form
// and get the right behavior.
func compileGlob(pattern string) (func(relPath, base string) bool, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return func(_, _ string) bool { return true }, nil
	}
	// Validate by running a no-op match — filepath.Match returns
	// the same syntax error we'd hit at call time, so failing
	// fast here surfaces a clear "bad pattern" error.
	if _, err := filepath.Match(pattern, "x"); err != nil {
		return nil, err
	}
	return func(relPath, base string) bool {
		if ok, _ := filepath.Match(pattern, base); ok {
			return true
		}
		if ok, _ := filepath.Match(pattern, relPath); ok {
			return true
		}
		// Approximate "**/" recursive match — stdlib doesn't grok it.
		// "**/<rest>" matches relPath if relPath ends with the rest
		// after a path separator (or is exactly the rest at root).
		if strings.HasPrefix(pattern, "**/") {
			rest := strings.TrimPrefix(pattern, "**/")
			if ok, _ := filepath.Match(rest, base); ok {
				return true
			}
			if ok, _ := filepath.Match(rest, relPath); ok {
				return true
			}
		}
		return false
	}, nil
}

// buildMatcher returns a per-line predicate honoring the regex /
// case-insensitivity flags. Compiled once outside the walk so each
// file scan gets the prepared matcher rather than recompiling.
func buildMatcher(p kaiGrepParams) (func(string) bool, error) {
	q := p.Query
	if p.Regex {
		expr := q
		if p.CaseInsensitive && !strings.HasPrefix(expr, "(?i)") {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}
	if p.CaseInsensitive {
		ql := strings.ToLower(q)
		return func(line string) bool { return strings.Contains(strings.ToLower(line), ql) }, nil
	}
	return func(line string) bool { return strings.Contains(line, q) }, nil
}
