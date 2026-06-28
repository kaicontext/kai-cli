package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/kaicontext/kai-engine/projects"
)

// diffContext is the number of unchanged lines emitted around each
// changed region, mirroring `diff -u`'s default. Big enough to
// orient the reader; small enough that a one-line edit doesn't
// flood the REPL with the whole file.
const diffContext = 3

// diffLine is one record in the flattened diff: marker is '+', '-',
// or ' '; text is the original line content (line-terminated).
// oldNum / newNum are 1-indexed positions in the source files (0
// means "doesn't exist on that side").
type diffLine struct {
	marker  byte
	text    string
	oldNum  int
	newNum  int
}

// unifiedDiff renders a hunked unified diff between old and new
// content. Only the changed lines plus `diffContext` lines of
// surrounding context appear; large unchanged regions between
// hunks are skipped and rendered as a "@@" separator the TUI shows
// as a visual break.
//
// File creation (oldContent == "") still emits every new line as
// "+", since by definition every line is new context.
func unifiedDiff(relPath, oldContent, newContent string) (patch string, added, removed int) {
	if oldContent == newContent {
		return "", 0, 0
	}
	dmp := diffmatchpatch.New()
	chrA, chrB, lines := dmp.DiffLinesToChars(oldContent, newContent)
	diffs := dmp.DiffMain(chrA, chrB, false)
	diffs = dmp.DiffCharsToLines(diffs, lines)

	all := make([]diffLine, 0, len(diffs))
	oldNum, newNum := 1, 1
	for _, d := range diffs {
		var marker byte
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			marker = '+'
		case diffmatchpatch.DiffDelete:
			marker = '-'
		default:
			marker = ' '
		}
		for _, line := range splitKeepNewlines(d.Text) {
			rec := diffLine{marker: marker, text: line}
			switch marker {
			case '+':
				rec.newNum = newNum
				newNum++
				added++
			case '-':
				rec.oldNum = oldNum
				oldNum++
				removed++
			default:
				rec.oldNum = oldNum
				rec.newNum = newNum
				oldNum++
				newNum++
			}
			all = append(all, rec)
		}
	}

	// Build hunk ranges: indices [start,end] inclusive that should
	// appear in the patch. Walk all lines; for each change, mark a
	// window of [change-context, change+context]. Merge overlapping
	// windows so adjacent edits don't duplicate context.
	type rng struct{ start, end int }
	var ranges []rng
	for i, ln := range all {
		if ln.marker == ' ' {
			continue
		}
		s := i - diffContext
		if s < 0 {
			s = 0
		}
		e := i + diffContext
		if e >= len(all) {
			e = len(all) - 1
		}
		if len(ranges) > 0 && s <= ranges[len(ranges)-1].end+1 {
			ranges[len(ranges)-1].end = e
		} else {
			ranges = append(ranges, rng{start: s, end: e})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", relPath, relPath)
	for hi, r := range ranges {
		if hi > 0 {
			b.WriteString("@@\n")
		}
		for i := r.start; i <= r.end; i++ {
			// Format: "<lineNum>\x1f<marker><text>" — 0x1f
			// (Information Separator One) splits the metadata from
			// the content unambiguously, regardless of leading
			// whitespace in source lines. The renderer splits on
			// the first 0x1f. For deletes we show the old-file
			// line number (since the line is gone from the new
			// file); for adds and context, the new-file number.
			ln := all[i].newNum
			if all[i].marker == '-' {
				ln = all[i].oldNum
			}
			fmt.Fprintf(&b, "%d\x1f%c%s", ln, all[i].marker, all[i].text)
			if !strings.HasSuffix(all[i].text, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), added, removed
}

// splitKeepNewlines splits on "\n" but keeps the trailing newline on
// each segment so the unified-diff renderer doesn't double up.
func splitKeepNewlines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	// SplitAfter leaves an empty trailing element when s ends with
	// "\n"; strip it so we don't emit a phantom blank line.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// contentDigest returns the hex-encoded sha256 of the given content.
// Used by the live-sync broadcast hook so the receiver can dedupe
// quickly without rehashing.
func contentDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// encodeBase64 wraps content for the live-sync wire format.
// kailab's SyncPushFile expects standard base64 (not URL-safe).
func encodeBase64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// approxLineDiff returns line counts for an approval prompt preview.
// Conservative: computes (lines in new) - (lines in old) splits, then
// derives "added" and "removed" as a quick heuristic. Not a true diff
// (no LCS) — the bash-approval prompt just needs a "+12 -3" headline,
// not a hunk-by-hunk count. The post-write OnDiff hook produces the
// real numbers.
//
// Returns (-1, -1) if either input is empty AND the other isn't —
// a brand-new file or a full delete — so the prompt can render
// "create X (47 lines)" instead of "+47 -0".
func approxLineDiff(oldContent, newContent string) (added, removed int) {
	if oldContent == "" && newContent == "" {
		return 0, 0
	}
	if oldContent == "" {
		return countLines(newContent), 0
	}
	if newContent == "" {
		return 0, countLines(oldContent)
	}
	oldLines := countLines(oldContent)
	newLines := countLines(newContent)
	delta := newLines - oldLines
	if delta >= 0 {
		return delta, 0
	}
	return 0, -delta
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := 1
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}

// previewUnifiedDiff returns a plain-text unified diff between
// oldContent and newContent with ~5 lines of context (the file's
// diffContext default), capped at maxLines total. When the diff is
// longer, trailing lines are truncated with a "… N more lines"
// marker.
//
// This is intended for human display above the file-write approval
// prompt — distinct from unifiedDiff (which encodes a 0x1f-separated
// machine format with per-line numbers for the post-confirm run log).
func previewUnifiedDiff(relPath, oldContent, newContent string, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 40
	}
	patch, _, _ := unifiedDiff(relPath, oldContent, newContent)
	if patch == "" {
		return ""
	}
	// Strip the per-line "<num>\x1f" metadata prefix so the result
	// reads as a normal unified diff. The "@@" hunk separators and
	// "--- a/" / "+++ b/" header lines pass through untouched.
	var b strings.Builder
	for _, raw := range strings.Split(patch, "\n") {
		if raw == "" {
			continue
		}
		if idx := strings.IndexByte(raw, '\x1f'); idx >= 0 {
			b.WriteString(raw[idx+1:])
		} else {
			b.WriteString(raw)
		}
		b.WriteByte('\n')
	}
	flat := strings.TrimRight(b.String(), "\n")
	if flat == "" {
		return ""
	}
	lines := strings.Split(flat, "\n")
	if len(lines) <= maxLines {
		return flat
	}
	kept := lines[:maxLines]
	more := len(lines) - maxLines
	kept = append(kept, fmt.Sprintf("… %d more lines", more))
	return strings.Join(kept, "\n")
}

// FileTools constructs the view/write/edit tools scoped to a single
// workspace directory. Two hooks fire after each successful write or
// edit:
//
//   - OnChange:    relPath + op ("created" / "modified" / "deleted")
//                  for visibility (TUI sync pane).
//   - OnBroadcast: relPath + digest + base64-content for live-sync
//                  forwarding to kailab. Optional — leave nil to skip.
//
// Both hooks are best-effort and fire synchronously; receivers must
// not block the agent's loop.
//
// Set is a *projects.Set that owns one or more project roots. The
// model-supplied path is resolved against the set's discovery root
// and routed to the owning project. For single-root workspaces
// (callers can wrap their existing absolute path with
// projects.Single) this collapses to the previous behavior.
type FileTools struct {
	Set         *projects.Set
	OnChange    func(relPath, op string)
	OnBroadcast func(relPath, digest, contentBase64 string)
	// OnDiff fires after each successful write or edit with a unified
	// diff of the change. The TUI uses it to render an inline
	// "Update(path) — Added N lines" entry like Claude Code's. addedLines
	// / removedLines are pre-computed so consumers don't reparse.
	// Optional — leave nil to skip.
	OnDiff func(relPath, op, unifiedDiff string, addedLines, removedLines int)
	// Approve, when set, gates each write/edit behind a user prompt.
	// Same shape as BashTool.Approve. nil disables the gate (matches
	// the default headless CLI behavior); the TUI sets it to surface
	// every mutation as a user-confirmable event. Returning false
	// aborts the write with a non-error response so the agent can
	// adjust rather than treat cancellation as a tool fault.
	//
	// op is "create" (writeTool) or "edit" (editTool). path is the
	// workspace-relative target. addedLines/removedLines are
	// pre-computed previews — useful for the TUI to show
	// "writing to X (12 added, 3 removed)" without rendering the
	// full diff. -1 in either field means "unknown" (e.g. write to
	// a brand-new file before the diff is computed).
	Approve func(ctx context.Context, op, path string, addedLines, removedLines int, diff string) (bool, error)
	// ReadOnly omits write and edit from the tool set. Used by the
	// chat fallback in the REPL so a quick "what files are here?"
	// query can call view and bash without risking accidental
	// modifications to the user's actual repo.
	ReadOnly bool

	// SharedPaths is the session-scoped allowlist of paths OUTSIDE
	// the workspace that the user has explicitly shared via the
	// /share TUI command. Read-only access only — write and edit
	// tools refuse them regardless. Used for the "look at the
	// design folder in ~/Downloads" workflow without copying files
	// into the workspace. Each entry is an absolute, cleaned path;
	// a path is allowed if it equals an entry or is under one as
	// a subdirectory. Empty by default.
	SharedPaths []string

	// viewLog tracks every successful view call by path → file mtime +
	// the list of (offset, limit) ranges already shown. viewTool
	// consults this on each call: if the agent re-requests a view
	// whose range is fully contained in a prior view AND the file's
	// mtime hasn't changed, the tool returns a short "already viewed;
	// content unchanged" notice instead of re-rendering the body.
	//
	// Two cache widths share this map:
	//   1. Exact re-issue (round-22 dogfood: opus chained 4-8
	//      identical view calls on the same region in a single run).
	//   2. Subset re-issue (TOK P1-3 from the 2026-05-26 master
	//      hardening spec): a new view of lines 50-60 after a prior
	//      view of lines 1-100 is a re-read of content the agent
	//      already has — cost is the round-trip + the model re-
	//      attending to text already in its working window.
	//
	// Mtime-based gating ensures we never serve stale "you already
	// viewed this" when the file actually changed between calls.
	viewLog   map[string]*viewLogEntry
	viewLogMu sync.Mutex
}

// viewLogEntry holds the cache state for one path: the file's mtime
// at last read, and the list of (offset, limit) ranges already shown
// to the agent. Membership test is "any prior range fully contains
// the new (offset, limit)" — see viewLogEntry.contains.
type viewLogEntry struct {
	Mtime  time.Time
	Ranges []viewRange
}

// viewRange is a (offset, limit) tuple. Limit of 0 here means "the
// caller asked for default" which the view tool resolves to its
// own default (2000 lines) — we store the post-resolution limit so
// containment checks compare apples to apples.
type viewRange struct {
	Offset int
	Limit  int
}

// contains reports whether `prior` fully covers the byte range that
// `next` requested. Used by the repeat-view gate: a new view nested
// inside (or identical to) a prior view returns a cached notice.
//
// Both ranges are interpreted as line ranges [Offset, Offset+Limit).
// A `next` whose limit extends past `prior` is NOT contained, even
// if the prior also paged into truncation — the agent might be
// asking for the deeper page deliberately.
func (prior viewRange) contains(next viewRange) bool {
	return prior.Offset <= next.Offset && prior.Offset+prior.Limit >= next.Offset+next.Limit
}

// View returns the read-only file viewer.
func (f *FileTools) View() BaseTool { return &viewTool{ft: f} }

// Write returns the file-create / overwrite tool.
func (f *FileTools) Write() BaseTool {
	return &writeTool{set: f.Set, onChange: f.OnChange, onBroadcast: f.OnBroadcast, onDiff: f.OnDiff, approve: f.Approve}
}

// Edit returns the patch-style editor.
func (f *FileTools) Edit() BaseTool {
	return &editTool{set: f.Set, onChange: f.OnChange, onBroadcast: f.OnBroadcast, onDiff: f.OnDiff, approve: f.Approve}
}

// All returns the available file tools. ReadOnly mode returns only
// the view tool — write and edit are intentionally absent so an
// agent in chat-fallback mode can't mutate files even if asked.
func (f *FileTools) All() []BaseTool {
	if f.ReadOnly {
		return []BaseTool{f.View()}
	}
	return []BaseTool{f.View(), f.Write(), f.Edit()}
}

// --- view ------------------------------------------------------------

type viewTool struct {
	ft *FileTools
}

type viewParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

func (v *viewTool) Info() ToolInfo {
	return ToolInfo{
		Name: "view",
		Description: "Read the contents of a file in the workspace. " +
			"Use offset/limit to page through large files. " +
			"Returns the file with line numbers prefixed (1: first line, 2: second, …).",
		Parameters: map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "Path relative to the workspace root, or an absolute path inside the workspace.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Zero-indexed line offset to start from. Default 0.",
				"default":     0,
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max lines to return. Default 2000; cap 20000.",
				"default":     2000,
			},
		},
		Required: []string{"file_path"},
	}
}

func (v *viewTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p viewParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("view: invalid input json: " + err.Error()), nil
	}
	_, abs, err := resolveInSetOrShared(v.ft.Set, v.ft.SharedPaths, p.FilePath)
	if err != nil {
		return NewTextErrorResponse("view: " + err.Error()), nil
	}
	if p.Limit <= 0 {
		p.Limit = 2000
	}
	if p.Limit > 20000 {
		p.Limit = 20000
	}

	// Repeat-view gate (TOK P1-3 widened, 2026-05-26 spec): if the
	// agent already viewed a range that contains this request AND
	// the file's mtime hasn't moved since, return a short notice
	// instead of re-rendering. The content is still available via
	// the provider's prompt cache, so the agent loses nothing
	// semantically; it loses the excuse to burn a turn on content
	// it has already seen. Two cases collapse into the same gate:
	// (1) identical re-issues (round-22 opus pattern: 4-8 same
	// view calls), and (2) nested re-issues (planner views 1-200
	// then later asks for 50-60 — the inner request is fully
	// contained in the outer view and re-emitting it is pure waste).
	curMtime := time.Time{}
	if st, err := os.Stat(abs); err == nil {
		curMtime = st.ModTime()
	}
	if !curMtime.IsZero() {
		v.ft.viewLogMu.Lock()
		entry, seen := v.ft.viewLog[abs]
		v.ft.viewLogMu.Unlock()
		if seen && entry.Mtime.Equal(curMtime) {
			req := viewRange{Offset: p.Offset, Limit: p.Limit}
			for _, prior := range entry.Ranges {
				if prior.contains(req) {
					// Tailor the notice based on whether this is an
					// exact re-issue or a contained subset — the
					// nudge differs slightly. Exact: "don't repeat."
					// Subset: "you already had this lines-N-to-M;
					// no need to re-view the slice." Both end with
					// the same "do something different" prod.
					// Mark as error so the model treats the cache hit
					// as a failed call (the prior NewTextResponse path
					// rendered it as success → model kept re-viewing
					// anyway, observed in the 2026-05-26 dogfood where
					// the agent viewed App.svelte 5x and Sidebar.svelte
					// 4x before tripping the budget cap). With IsError
					// the model adjusts strategy instead of looping.
					if prior.Offset == req.Offset && prior.Limit == req.Limit {
						return NewTextErrorResponse(fmt.Sprintf(
							"already viewed: %s [offset=%d limit=%d] — file is unchanged since your last view. "+
								"Your prior result is still valid; do not re-issue identical view calls. "+
								"Proceed with editing, or query a DIFFERENT region/file/symbol.",
							p.FilePath, p.Offset, p.Limit)), nil
					}
					return NewTextErrorResponse(fmt.Sprintf(
						"already viewed: %s — your prior view of [offset=%d limit=%d] fully contains this request [offset=%d limit=%d], and the file is unchanged since. "+
							"The lines you're asking for are still in your transcript above; do not re-view a subset of what you've already seen. "+
							"Proceed with editing, or query a DIFFERENT file/region/symbol.",
						p.FilePath, prior.Offset, prior.Limit, req.Offset, req.Limit)), nil
				}
			}
		}
	}

	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			// Path RESOLVED to a project but the file isn't on
			// disk. Common when the agent guessed a path that
			// happened to project-route correctly but pointed
			// at a non-existent file. The hint nudges toward
			// kai_files (the right tool for "where does X
			// live") instead of more view-and-guess attempts.
			base := filepath.Base(p.FilePath)
			return NewTextErrorResponse(fmt.Sprintf(
				"view: file not found: %s. The path resolved to %s but no file is there. "+
					"To find the right path, try kai_files with a glob (e.g. {\"glob\":\"**/%s\"}) — it'll list every file matching that basename across all projects.",
				p.FilePath, abs, base)), nil
		}
		return NewTextErrorResponse("view: open: " + err.Error()), nil
	}
	defer f.Close()

	// Bounded read: stop after the requested limit. Reading via a
	// scanner would be cleaner but stdlib's default token size caps at
	// 64KB per line, which trips on minified js. Read whole file then
	// slice — capped at a few MB which is fine for any source file.
	const maxBytes = 8 << 20 // 8 MiB
	body, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return NewTextErrorResponse("view: read: " + err.Error()), nil
	}
	if len(body) > maxBytes {
		return NewTextErrorResponse("view: file too large (>8MB)"), nil
	}

	lines := strings.Split(string(body), "\n")
	total := len(lines)
	start := p.Offset
	if start < 0 {
		start = 0
	}
	if start >= total {
		return NewTextResponse(fmt.Sprintf("(empty: offset %d past end of %d-line file)", start, total)), nil
	}
	end := start + p.Limit
	if end > total {
		end = total
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
	}
	if end < total {
		fmt.Fprintf(&b, "(truncated; %d more lines after line %d)\n", total-end, end)
	}
	// TOK-instr (2026-05-26 spec). Append a CSV row per view to
	// <KaiDir>/read-log.csv so the large-read guard and cap-sizing
	// decisions have actual per-read data instead of retrofit
	// grep-over-debug-log analysis. Best-effort — log failures
	// don't affect the read.
	if set := v.ft.Set; set != nil {
		if prim := set.Primary(); prim != nil {
			linesRead := end - start
			whole := start == 0 && end == total
			logRead(prim.KaiDir, p.FilePath, p.Offset, p.Limit, total, linesRead, b.Len(), whole)
		}
	}
	// Record the view so a follow-up call with a contained range
	// returns the short repeat-view notice instead of re-rendering
	// (TOK P1-3). Append to the path's range list; on mtime change
	// we reset the list since prior excerpts are no longer valid.
	if !curMtime.IsZero() {
		v.ft.viewLogMu.Lock()
		if v.ft.viewLog == nil {
			v.ft.viewLog = make(map[string]*viewLogEntry)
		}
		entry, ok := v.ft.viewLog[abs]
		if !ok || !entry.Mtime.Equal(curMtime) {
			entry = &viewLogEntry{Mtime: curMtime}
			v.ft.viewLog[abs] = entry
		}
		entry.Ranges = append(entry.Ranges, viewRange{Offset: p.Offset, Limit: p.Limit})
		v.ft.viewLogMu.Unlock()
	}

	// Prepend a one-line git-state header so the agent sees the
	// provenance of what it just read. Without this it can't tell
	// "this is the committed code" from "this is your in-flight
	// working tree" — and downstream it claims fixes are "in place"
	// when they're sitting uncommitted. Best-effort: if the file
	// isn't in a git repo, or git isn't installed, we just omit
	// the header. No error — the read itself is what matters.
	header := gitStateHeader(ctx, abs)
	contentStr := b.String()
	// Template-engine brace-escape notice. When the file contains
	// literal `{"{"}` or `{"}"}` byte sequences (Svelte's way to
	// render a literal `{` in text), the view output is ambiguous
	// to the model — it can't tell from looking whether the chars
	// are in the file or are display escapes added by view. The
	// 2026-05-25 chat-debug log pinned this exactly: the model
	// said "the view tool is showing escape sequences that make
	// it hard to see..." and tried sed/awk/xxd/python3/head to
	// verify — all blocked. The prefix below tells the model
	// explicitly: these ARE the file's bytes, no escaping is
	// applied by the tool.
	if notice := braceEscapeNotice(contentStr); notice != "" {
		if header == "" {
			header = notice
		} else {
			header = header + "\n" + notice
		}
	}
	if header != "" {
		return NewTextResponse(header + "\n" + contentStr), nil
	}
	return NewTextResponse(contentStr), nil
}

// braceEscapeNotice returns a one-paragraph note when the file
// contains literal Svelte brace-escape sequences. Empty otherwise.
// The note tells the model the byte sequence is the file's actual
// content, not a display artifact from the view tool. Counters the
// 2026-05-25 dogfood pathology where the model wasted 5+ turns
// trying to verify bytes via sed/awk/xxd/python3 (all blocked) and
// timed out before committing to the obvious diagnosis.
func braceEscapeNotice(content string) string {
	if !strings.Contains(content, `{"{"}`) && !strings.Contains(content, `{"}"}`) {
		return ""
	}
	return "[view] This file contains literal Svelte brace-escape sequences. " +
		"The byte sequences `{\"{\"}` and `{\"}\"}` you see below ARE the file's actual content " +
		"(5 literal characters: `{`, `\"`, `{`, `\"`, `}` and `{`, `\"`, `}`, `\"`, `}` respectively) " +
		"— the view tool does NOT apply escaping. " +
		"`{\"{\"}` is the standard Svelte idiom to render a literal `{` in template TEXT (vs. a control-flow expression). " +
		"If you ALSO see unescaped `{#each}` / `{/each}` / `{#if}` / `{:else}` / `{/if}` in this same file, those are REAL Svelte block tags. " +
		"A common bug shape: opening tags over-escaped to `{\"{\"}#each ...{\"}\"}` (treated as literal text) paired with a closing `{/each}` (real block) → Svelte sees a block-close with no matching open and throws \"Unexpected block closing tag\". " +
		"Another shape: a raw `{` in text that should have been escaped → Svelte parses it as expression-open and throws \"Unexpected token\". " +
		"When debugging an error in this file, the byte-level content of any cited line is the SAME bytes you see in this view output — no re-verification with shell tools needed."
}

// gitStateHeader returns a single-line summary of the file's git
// state, e.g. "[git: clean · last commit abc123 3h ago]" or
// "[git: MODIFIED (uncommitted) · last commit def456 2d ago]".
// Empty string when the file isn't under version control or git
// isn't available — the absence of the header is itself a signal
// ("not in a repo"), not an error.
//
// Two short shell-outs per view call. That's not free, but every
// kai_* graph tool already does at least one shell-out and the
// answer here is load-bearing for honesty about deployment state.
func gitStateHeader(ctx context.Context, absPath string) string {
	dir := filepath.Dir(absPath)
	// Anchor to the repo root so a relative pathspec works. If we're
	// not in a repo, --show-toplevel exits non-zero and we bail.
	topCmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	topOut, err := topCmd.Output()
	if err != nil {
		return ""
	}
	repoRoot := strings.TrimSpace(string(topOut))
	if repoRoot == "" {
		return ""
	}
	// macOS /var → /private/var symlinks (and similar on Linux) make
	// filepath.Rel produce a `..`-rooted relative path even when the
	// file genuinely lives inside the repo. Canonicalize both sides
	// before computing rel so the comparison is between equivalent
	// realpaths. EvalSymlinks may fail on race-y temp paths; treat
	// that as "skip the footer" rather than misroute the call.
	realAbs, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realAbs = absPath
	}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		realRoot = repoRoot
	}
	rel, err := filepath.Rel(realRoot, realAbs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	repoRoot = realRoot

	statusCmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "status", "--porcelain=v1", "--", rel)
	statusOut, _ := statusCmd.Output()
	dirty := strings.TrimSpace(string(statusOut)) != ""

	logCmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "-1", "--format=%h %ar", "--", rel)
	logOut, _ := logCmd.Output()
	logLine := strings.TrimSpace(string(logOut))

	var state string
	if dirty {
		state = "MODIFIED (uncommitted)"
	} else {
		state = "clean"
	}
	if logLine != "" {
		return fmt.Sprintf("[git: %s · last commit %s]", state, logLine)
	}
	return fmt.Sprintf("[git: %s · no commit history]", state)
}

// --- write -----------------------------------------------------------

type writeTool struct {
	set         *projects.Set
	onChange    func(relPath, op string)
	onBroadcast func(relPath, digest, contentBase64 string)
	onDiff      func(relPath, op, unifiedDiff string, addedLines, removedLines int)
	approve     func(ctx context.Context, op, path string, addedLines, removedLines int, diff string) (bool, error)
}

type writeParams struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func (w *writeTool) Info() ToolInfo {
	return ToolInfo{
		Name: "write",
		Description: "Create a new file or overwrite an existing one with the given content. " +
			"Parent directories are created as needed. " +
			"Use `edit` instead when you only need to change part of an existing file.",
		Parameters: map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "Path relative to the workspace root.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full contents of the file.",
			},
		},
		Required: []string{"file_path", "content"},
	}
}

func (w *writeTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p writeParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("write: invalid input json: " + err.Error()), nil
	}
	_, abs, err := resolveInSet(w.set, p.FilePath)
	if err != nil {
		return NewTextErrorResponse("write: " + err.Error()), nil
	}

	op := "modified"
	var prior string
	if existing, err := os.ReadFile(abs); err == nil {
		prior = string(existing)
	} else if os.IsNotExist(err) {
		op = "created"
	}

	// Approval gate. Pre-compute added/removed line counts so the
	// prompt can show "create X (47 lines)" or "modify Y (+12 -3)"
	// without rendering the full diff. Cancellation returns a
	// non-error response so the agent can adjust.
	if w.approve != nil {
		added, removed := approxLineDiff(prior, p.Content)
		approveOp := "create"
		if op == "modified" {
			approveOp = "edit"
		}
		diff := previewUnifiedDiff(p.FilePath, prior, p.Content, 40)
		ok, err := w.approve(ctx, approveOp, p.FilePath, added, removed, diff)
		if err != nil {
			return NewTextErrorResponse("write: approval failed: " + err.Error()), nil
		}
		if !ok {
			return NewTextResponse(fmt.Sprintf("write %s %s — cancelled by user", op, p.FilePath)), nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return NewTextErrorResponse("write: mkdir: " + err.Error()), nil
	}
	if err := os.WriteFile(abs, []byte(p.Content), 0o644); err != nil {
		return NewTextErrorResponse("write: " + err.Error()), nil
	}
	relForward := filepath.ToSlash(p.FilePath)
	if w.onChange != nil {
		w.onChange(relForward, op)
	}
	if w.onBroadcast != nil {
		w.onBroadcast(relForward, contentDigest(p.Content), encodeBase64(p.Content))
	}
	if w.onDiff != nil {
		patch, added, removed := unifiedDiff(relForward, prior, p.Content)
		w.onDiff(relForward, op, patch, added, removed)
	}
	// Strong-confirmation result format. Without this, the model
	// would issue a redundant `view` after every successful write
	// to "verify it took" — see May 2026 dogfood logs where simple
	// one-line edits ballooned to 7-turn sequences. The "✓" prefix +
	// "verified on disk" phrase + content digest give the model
	// machine-checkable proof, so the system prompt's anti-redundancy
	// guidance has something concrete to point at.
	return NewTextResponse(fmt.Sprintf(
		"✓ %s %s (%d bytes, sha256=%s) — verified on disk; do not re-view unless the user asks",
		op, p.FilePath, len(p.Content), contentDigest(p.Content)[:12])), nil
}

// --- edit ------------------------------------------------------------

type editTool struct {
	set         *projects.Set
	onChange    func(relPath, op string)
	onBroadcast func(relPath, digest, contentBase64 string)
	onDiff      func(relPath, op, unifiedDiff string, addedLines, removedLines int)
	approve     func(ctx context.Context, op, path string, addedLines, removedLines int, diff string) (bool, error)
}

type editParams struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (e *editTool) Info() ToolInfo {
	return ToolInfo{
		Name: "edit",
		Description: "Replace one occurrence (or all, with replace_all=true) of `old_string` " +
			"with `new_string` in the named file. The match must be exact, including " +
			"whitespace and line endings — read the file first with `view` to copy the " +
			"exact text. To create a brand-new file, use `write` instead.",
		Parameters: map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "Path relative to the workspace root.",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "Exact substring to find. Must match exactly once unless replace_all is true.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "Replacement text. Empty string deletes the match.",
			},
			"replace_all": map[string]any{
				"type":        "boolean",
				"description": "If true, replace every occurrence. Default false (require unique match).",
				"default":     false,
			},
		},
		Required: []string{"file_path", "old_string", "new_string"},
	}
}

func (e *editTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p editParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("edit: invalid input json: " + err.Error()), nil
	}
	_, abs, err := resolveInSet(e.set, p.FilePath)
	if err != nil {
		return NewTextErrorResponse("edit: " + err.Error()), nil
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return NewTextErrorResponse("edit: file not found: " + p.FilePath + " (use `write` to create new files)"), nil
		}
		return NewTextErrorResponse("edit: read: " + err.Error()), nil
	}
	src := string(body)

	var updated string
	fuzzy := false
	switch {
	case strings.Contains(src, p.OldString):
		if p.ReplaceAll {
			updated = strings.ReplaceAll(src, p.OldString, p.NewString)
		} else {
			// Enforce uniqueness for non-replace-all to catch ambiguous
			// matches early. The model can re-issue with replace_all=true
			// or a more specific old_string.
			count := strings.Count(src, p.OldString)
			if count > 1 {
				return NewTextErrorResponse(fmt.Sprintf(
					"edit: old_string appears %d times in %s; pass replace_all=true or expand the match to be unique",
					count, p.FilePath)), nil
			}
			updated = strings.Replace(src, p.OldString, p.NewString, 1)
		}
	default:
		// Exact match failed. Cheap models routinely reproduce leading
		// indentation or trailing whitespace slightly wrong, which used
		// to send them into a multi-minute retry loop. Fall back to a
		// whitespace-tolerant, line-based match: compare old_string and
		// the file line-by-line with each line trimmed. The replacement
		// is applied to the file's ACTUAL text for the matched span, so
		// surrounding whitespace is preserved. Requires a unique match —
		// ambiguity still errors.
		start, end, count := fuzzyLineMatch(src, p.OldString)
		switch {
		case count == 1:
			updated = src[:start] + p.NewString + src[end:]
			fuzzy = true
		case count > 1:
			return NewTextErrorResponse(fmt.Sprintf(
				"edit: old_string matched %d blocks in %s (whitespace-insensitive); expand the match to be unique",
				count, p.FilePath)), nil
		default:
			return NewTextErrorResponse("edit: old_string not found in " + p.FilePath), nil
		}
	}

	// Approval gate, matching writeTool. Pre-compute line-count
	// delta from the already-prepared src/updated strings.
	if e.approve != nil {
		added, removed := approxLineDiff(src, updated)
		diff := previewUnifiedDiff(p.FilePath, src, updated, 40)
		ok, err := e.approve(ctx, "edit", p.FilePath, added, removed, diff)
		if err != nil {
			return NewTextErrorResponse("edit: approval failed: " + err.Error()), nil
		}
		if !ok {
			return NewTextResponse(fmt.Sprintf("edit %s — cancelled by user", p.FilePath)), nil
		}
	}

	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return NewTextErrorResponse("edit: write: " + err.Error()), nil
	}
	relForward := filepath.ToSlash(p.FilePath)
	if e.onChange != nil {
		e.onChange(relForward, "modified")
	}
	if e.onBroadcast != nil {
		e.onBroadcast(relForward, contentDigest(updated), encodeBase64(updated))
	}
	if e.onDiff != nil {
		patch, added, removed := unifiedDiff(relForward, src, updated)
		e.onDiff(relForward, "modified", patch, added, removed)
	}
	delta := len(updated) - len(src)
	sign := "+"
	if delta < 0 {
		sign = "-"
		delta = -delta
	}
	// Same strong-confirmation pattern as write — see comment there.
	// The post-edit digest is the load-bearing signal: it lets the
	// model verify "yes, the file now contains the new_string I
	// asked for" without issuing a separate `view` to confirm.
	note := ""
	if fuzzy {
		note = " (matched whitespace-insensitively — old_string differed from the file only in indentation/whitespace)"
	}
	return NewTextResponse(fmt.Sprintf(
		"✓ edited %s (%s%d bytes, sha256=%s)%s — verified on disk; do not re-view unless the user asks",
		p.FilePath, sign, delta, contentDigest(updated)[:12], note)), nil
}

// fuzzyLineMatch finds the span of src whose lines, each trimmed of
// leading and trailing whitespace, equal the trimmed lines of old. It
// returns the byte offsets [start,end) of that span and how many
// spans matched; the caller applies an edit only when count == 1.
//
// This is the whitespace-tolerant fallback for edit: it rescues the
// common cheap-model failure where old_string is correct except for
// indentation, without loosening the exact-match path.
func fuzzyLineMatch(src, old string) (start, end, count int) {
	oldLines := splitTrimmedLines(old)
	if len(oldLines) == 0 {
		return 0, 0, 0
	}
	type lineSpan struct {
		start, end int
		trimmed    string
	}
	var spans []lineSpan
	off := 0
	for {
		nl := strings.IndexByte(src[off:], '\n')
		lineEnd := len(src)
		if nl >= 0 {
			lineEnd = off + nl
		}
		spans = append(spans, lineSpan{
			start:   off,
			end:     lineEnd,
			trimmed: strings.TrimSpace(src[off:lineEnd]),
		})
		if nl < 0 {
			break
		}
		off = lineEnd + 1
	}
	for i := 0; i+len(oldLines) <= len(spans); i++ {
		hit := true
		for j := range oldLines {
			if spans[i+j].trimmed != oldLines[j] {
				hit = false
				break
			}
		}
		if hit {
			count++
			start = spans[i].start
			end = spans[i+len(oldLines)-1].end
		}
	}
	return start, end, count
}

// splitTrimmedLines splits s on newlines and trims each line, dropping
// a single trailing empty line so a trailing newline on old_string
// doesn't add a phantom blank line to the match window.
func splitTrimmedLines(s string) []string {
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimSpace(l)
	}
	return out
}

// --- shared ----------------------------------------------------------

// resolveInSet turns the model-supplied path into an absolute path
// owned by one of the projects in set. Resolution order:
//
//  1. If the path is absolute, find the owning project by
//     longest-prefix match.
//  2. Otherwise, join with the set's discovery root and try again
//     (covers "kai-server/api/llm.go" from a multi-root cwd).
//  3. If still no owner, but the set has a single project, fall back
//     to that project's root (preserves single-root semantics where
//     "api/llm.go" means "<project>/api/llm.go").
//
// Returns the owning project plus the absolute path. Errors when the
// path can't be attributed to any project — the message lists the
// available project names so the model can retry with the right
// prefix.
// resolveInSetOrShared is the workspace-aware resolver with an
// additional shared-paths fallback. If the standard resolveInSet
// fails because the path "escapes workspace" but the path matches
// a session-shared external path (set via the TUI's /share
// command), return (nil, abs, nil) — the synthetic external
// project. Callers must accept a nil project value for the
// shared case; only read-only tools (view, kai_grep, kai_files,
// kai_tree) consult this — write/edit refuse shared paths
// unconditionally.
func resolveInSetOrShared(set *projects.Set, sharedPaths []string, p string) (*projects.Project, string, error) {
	proj, abs, err := resolveInSet(set, p)
	if err == nil {
		return proj, abs, nil
	}
	if len(sharedPaths) == 0 {
		return nil, "", err
	}
	// Expand ~ and clean.
	expanded := p
	if strings.HasPrefix(expanded, "~/") {
		if home, herr := os.UserHomeDir(); herr == nil {
			expanded = filepath.Join(home, expanded[2:])
		}
	}
	if !filepath.IsAbs(expanded) {
		// Relative path that didn't resolve under any project AND
		// isn't an absolute shared-path candidate. Bail.
		return nil, "", err
	}
	candidate := filepath.Clean(expanded)
	for _, sp := range sharedPaths {
		sp = filepath.Clean(sp)
		if rel, rerr := filepath.Rel(sp, candidate); rerr == nil && !strings.HasPrefix(rel, "..") {
			TraceRouting("file in=%q → SHARED root=%s abs=%s", p, sp, candidate)
			return nil, candidate, nil
		}
	}
	return nil, "", err
}

func resolveInSet(set *projects.Set, p string) (*projects.Project, string, error) {
	if set == nil || len(set.Projects()) == 0 {
		return nil, "", fmt.Errorf("workspace not set")
	}
	if p == "" {
		return nil, "", fmt.Errorf("file_path is required")
	}
	root := set.DiscoveryRoot

	// Project-name prefix: kai_grep / kai_tree / kai_files
	// emit results in a multi-root workspace prefixed with
	// the project name + "/" (see kai_fs.go's walkRoot
	// prefix wiring). When the agent feeds those paths back
	// into view / edit / write, we have to recognize the
	// prefix and resolve against the matching project's
	// filesystem path — otherwise filepath.Join(discoveryRoot,
	// "Kai/kai-cli/...") produces "<discoveryRoot>/Kai/..."
	// which doesn't exist, and every read fails with "file
	// not found." That's the May-2026 banner-edit failure:
	// the agent located banner.go via kai_tree, fed the
	// prefixed path back into view, and got a
	// "file not found" loop until it gave up.
	//
	// Match is case-sensitive on the first segment so we
	// don't accidentally absorb a real directory named
	// like a project. Project names are typically PascalCase
	// or snake_case — distinct from typical lowercase
	// directory names, but a real directory could collide.
	// On collision we prefer the project-prefix
	// interpretation since it's what the multi-root tools
	// produce; users can pass an absolute path to disambiguate.
	if !filepath.IsAbs(p) && len(set.Projects()) > 1 {
		head, rest, found := strings.Cut(p, "/")
		if found && head != "" {
			if proj := set.ByName(head); proj != nil {
				abs := filepath.Clean(filepath.Join(proj.Path, rest))
				if rel, err := filepath.Rel(proj.Path, abs); err == nil &&
					!strings.HasPrefix(rel, "..") {
					TraceRouting("file in=%q → project=%s abs=%s match=name-prefix", p, proj.Name, abs)
					return proj, abs, nil
				}
			}
		}
	}

	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)

	if proj := set.ProjectFor(abs); proj != nil {
		// Defense-in-depth: even though ProjectFor matched, verify the
		// resolved path is actually under that project (no traversal
		// escape via `..` segments that happened to canonicalize back).
		if rel, err := filepath.Rel(proj.Path, abs); err == nil &&
			!strings.HasPrefix(rel, "..") {
			TraceRouting("file in=%q → project=%s abs=%s match=longest-prefix", p, proj.Name, abs)
			return proj, abs, nil
		}
	}

	// Single-project fallback: model sent a path that didn't route,
	// but with one project it's unambiguous — try resolving against
	// that project's root.
	if len(set.Projects()) == 1 {
		proj := set.Projects()[0]
		if !filepath.IsAbs(p) {
			abs = filepath.Clean(filepath.Join(proj.Path, p))
		}
		if rel, err := filepath.Rel(proj.Path, abs); err == nil &&
			!strings.HasPrefix(rel, "..") {
			TraceRouting("file in=%q → project=%s abs=%s match=single-fallback", p, proj.Name, abs)
			return proj, abs, nil
		}
		TraceRouting("file in=%q → ERROR escapes workspace (single-project fallback)", p)
		return nil, "", fmt.Errorf("path escapes workspace: %s", p)
	}

	names := make([]string, 0, len(set.Projects()))
	for _, pr := range set.Projects() {
		names = append(names, pr.Name)
	}
	// Build a hint that preserves the FULL input path, not just its
	// basename. Earlier version of this used filepath.Base which
	// produced examples like 'Kai/banner.go' even when the input
	// was 'kai-cli/internal/tui/views/banner.go' — the model then
	// tried 'view Kai/banner.go' literally and got "file not
	// found" (it's at Kai/kai-cli/internal/tui/views/banner.go, NOT
	// at Kai/banner.go). Quoted 2026-05-11 dogfood DX review.
	example := filepath.Join(names[0], p)
	TraceRouting("file in=%q → ERROR no project matched (available: %s)", p, strings.Join(names, ","))
	return nil, "", fmt.Errorf(
		"path %q is not inside any project. Available projects: %s. "+
			"In a multi-root workspace, prefix the path with the project name (e.g. %q). "+
			"If you don't know which project owns the file, use kai_files with a glob (e.g. {\"glob\":\"**/%s\"}) to list matches.",
		p, strings.Join(names, ", "), example, filepath.Base(p))
}
