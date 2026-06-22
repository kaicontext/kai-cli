package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
)

// kaiDiffTool exposes Kai's semantic diff to the agent. Review and
// Debug modes both rely on this — Review needs to see what changed
// since the last snapshot; Debug uses recent diffs to correlate
// errors with edits.
//
// Implementation shells out to `kai diff -p [file]` rather than
// re-implementing the snapshot/changeset machinery. The CLI binary
// already handles ref resolution, snapshot lookup, and unified-diff
// formatting; wrapping it here keeps the tool small and avoids
// duplicating maintenance work. When the binary path isn't known
// the tool returns a clear error rather than guessing — the runner
// threads in the same `kai` binary path the REPL already shells to.
type kaiDiffTool struct {
	// kaiBinary is the absolute path to the kai executable. Empty
	// disables the tool (Run returns a clear error). The runner
	// wires this from cfg in cmd/kai/tui.go.
	kaiBinary string
	// workspace is the cwd to run `kai diff` in. The repo root.
	workspace string
}

type kaiDiffParams struct {
	File  string `json:"file"`
	Since string `json:"since"`
	Until string `json:"until"`
}

type kaiDiffFile struct {
	File             string   `json:"file"`
	FunctionsChanged []string `json:"functions_changed,omitempty"`
	Additions        int      `json:"additions"`
	Deletions        int      `json:"deletions"`
	Patch            string   `json:"patch,omitempty"`
}

type kaiDiffResult struct {
	// SemanticSummary is the structural change view from `kai diff`
	// (no -p): which functions/types/contracts were added, removed, or
	// changed, and the change category. This is the part git diff
	// can't give — read it FIRST to understand what changed before
	// diving into the line-level patch below.
	SemanticSummary string        `json:"semantic_summary,omitempty"`
	FilesChanged    int           `json:"files_changed"`
	Diffs           []kaiDiffFile `json:"diffs"`
}

func (t *kaiDiffTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_diff",
		Description: "Show the SEMANTIC diff for code changes — prefer this over `git diff`. " +
			"Returns a structural summary (which functions/types/contracts were added, " +
			"removed, or changed) PLUS the line-level patch and per-file add/delete counts. " +
			"The semantic summary is the part git diff can't give. Diffs the working tree " +
			"against the last snapshot by default; pass `since`+`until` to diff any two " +
			"snapshots (each kai snapshot is labeled with the run that produced it, so this " +
			"traces WHEN/by-which-run a change was introduced — something git diff can't do " +
			"for orchestrator-absorbed changes). Use it in Review mode to see what to review, " +
			"in Debug mode to correlate errors with recent edits, and to trace where a change " +
			"came from.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Optional file path to scope the diff to. Omit to diff every changed file.",
			},
			"since": map[string]any{
				"type":        "string",
				"description": "Baseline (older) ref, e.g. \"@snap:last\", \"@snap:prev\", or a snapshot id. Defaults to the last integrated snapshot (diffed against the working tree).",
			},
			"until": map[string]any{
				"type":        "string",
				"description": "Target (newer) ref to diff `since` against, e.g. another snapshot id. Omit to diff `since` against the current working tree. Use both to compare two snapshots.",
			},
		},
	}
}

func (t *kaiDiffTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.kaiBinary == "" || t.workspace == "" {
		return NewTextErrorResponse(
			"kai_diff: not configured (binary or workspace missing). " +
				"Restart kai or report this as a bug.",
		), nil
	}
	var p kaiDiffParams
	if len(call.Input) > 0 {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_diff: invalid input json: " + err.Error()), nil
		}
	}

	// Positional refs come first (since, then until), then the optional
	// file. `kai diff <since> <until> <file>` mirrors the CLI; an empty
	// ref is simply omitted. We don't validate ref strings — the CLI
	// errors out on a malformed ref and we surface that to the model.
	refArgs := []string{}
	if p.Since != "" {
		refArgs = append(refArgs, p.Since)
	}
	if p.Until != "" {
		refArgs = append(refArgs, p.Until)
	}
	// `kai diff` takes only ref positionals (0-2); it has no file-scope
	// flag, so we diff everything and filter to p.File in-process below.
	posArgs := append([]string{}, refArgs...)

	runKaiDiff := func(extra ...string) (string, error) {
		a := append([]string{"diff"}, extra...)
		a = append(a, posArgs...)
		c := exec.CommandContext(ctx, t.kaiBinary, a...)
		c.Dir = t.workspace
		c.Env = os.Environ()
		o, e := c.CombinedOutput()
		return string(o), e
	}

	// Semantic pass first (no -p): the structural change view that git
	// diff can't produce. Best-effort — if it fails we still return the
	// patch below rather than erroring the whole call.
	semantic, semErr := runKaiDiff()
	semanticSummary := ""
	if semErr == nil {
		semanticSummary = strings.TrimSpace(semantic)
		if p.File != "" {
			semanticSummary = semanticBlockForFile(semanticSummary, p.File)
		}
	}

	// Patch pass (-p): the line-level unified diff, parsed per file.
	out, err := runKaiDiff("-p")
	if err != nil {
		// Distinguish "no changes" (exit 0 with empty output) from
		// real failures. CombinedOutput on a non-zero exit returns
		// err non-nil; we surface stderr in the response so the
		// model can reason about it.
		return NewTextErrorResponse(
			"kai_diff: " + err.Error() + ": " + strings.TrimSpace(string(out)),
		), nil
	}

	patches := splitUnifiedPatches(string(out))
	if p.File != "" {
		// Scope to the requested file (kai diff has no file flag).
		for path := range patches {
			if path != p.File {
				delete(patches, path)
			}
		}
	}
	if len(patches) == 0 {
		// No `--- a/` markers means no line-level changes. Prefer the
		// semantic summary (it captures renames / metadata / structural
		// changes that don't show as +/- lines); fall back to the raw
		// patch output so the model still sees what kai diff said.
		if semanticSummary != "" {
			return NewTextResponse(semanticSummary), nil
		}
		return NewTextResponse(strings.TrimSpace(string(out))), nil
	}

	result := kaiDiffResult{
		SemanticSummary: semanticSummary,
		FilesChanged:    len(patches),
		Diffs:           make([]kaiDiffFile, 0, len(patches)),
	}
	for path, patch := range patches {
		add, del := countAddDel(patch)
		result.Diffs = append(result.Diffs, kaiDiffFile{
			File:             path,
			FunctionsChanged: extractFunctionsChanged(patch),
			Additions:        add,
			Deletions:        del,
			Patch:            patch,
		})
	}
	// Stable ordering by path so repeated calls produce identical
	// output (better for prompt caching).
	sortDiffsByFile(result.Diffs)

	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return NewTextErrorResponse("kai_diff: marshal: " + err.Error()), nil
	}
	return NewTextResponse(string(body)), nil
}

// splitUnifiedPatches breaks combined `kai diff -p` output into one
// patch per file, keyed by the +++ b/<path> header. Returns an empty
// map if no patches were found (e.g. the CLI emitted only a summary
// or "no changes").
func splitUnifiedPatches(out string) map[string]string {
	patches := map[string]string{}
	lines := strings.Split(out, "\n")
	var (
		current  strings.Builder
		curFile  string
	)
	flush := func() {
		if curFile != "" && current.Len() > 0 {
			patches[curFile] = strings.TrimRight(current.String(), "\n")
		}
		current.Reset()
		curFile = ""
	}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "--- "):
			// Start of a new patch. Flush the prior one (if any)
			// before resetting.
			flush()
			current.WriteString(line)
			current.WriteByte('\n')
		case strings.HasPrefix(line, "+++ "):
			// Capture the destination filename. Strip "+++ b/" or
			// "+++ " prefix so curFile is workspace-relative.
			f := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			f = strings.TrimPrefix(f, "b/")
			curFile = f
			current.WriteString(line)
			current.WriteByte('\n')
		default:
			if curFile != "" {
				current.WriteString(line)
				current.WriteByte('\n')
			}
		}
	}
	flush()
	return patches
}

// countAddDel counts lines beginning with '+' / '-' (excluding the
// '+++' / '---' headers). Mirrors what `git diff --stat` reports.
func countAddDel(patch string) (add, del int) {
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") {
			continue
		}
		if strings.HasPrefix(line, "+") {
			add++
		} else if strings.HasPrefix(line, "-") {
			del++
		}
	}
	return
}

// extractFunctionsChanged scans hunk headers (`@@ ... @@ <ctx>`) for
// function names. The unified-diff hunk-context line frequently
// names the enclosing function — `@@ -47,6 +47,17 @@ func login(...)`.
// Best-effort: returns an empty slice when the diff format doesn't
// carry hunk context.
func extractFunctionsChanged(patch string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		// Find the second "@@" and take whatever follows.
		i := strings.Index(line, "@@")
		j := strings.Index(line[i+2:], "@@")
		if j < 0 {
			continue
		}
		ctx := strings.TrimSpace(line[i+2+j+2:])
		if ctx == "" {
			continue
		}
		name := extractFunctionName(ctx)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// extractFunctionName pulls a function-ish identifier out of a hunk
// context line. Handles common shapes: "func login(...)" (Go),
// "def login(...)" (Python), "class X" (boundary), "function X(...)"
// (JS). Falls back to the first identifier if no keyword matches.
func extractFunctionName(ctx string) string {
	for _, kw := range []string{"func ", "def ", "function ", "class "} {
		if strings.HasPrefix(ctx, kw) {
			rest := strings.TrimPrefix(ctx, kw)
			return firstIdent(rest)
		}
		if i := strings.Index(ctx, " "+kw); i >= 0 {
			return firstIdent(ctx[i+len(kw)+1:])
		}
	}
	return firstIdent(ctx)
}

// firstIdent returns the leading identifier-shaped token: letters,
// digits, underscores, dots. Stops at the first paren / bracket /
// space. Used to extract a function name from a hunk-context line.
func firstIdent(s string) string {
	for i, r := range s {
		if !(r == '_' || r == '.' || r == '$' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			return s[:i]
		}
	}
	return s
}

// semanticBlockForFile extracts the change block for one file from
// `kai diff` (semantic) output. That output groups changes by file: a
// non-indented header line names the file (e.g. "~ path/to.go") and
// the indented lines under it list the structural changes ("  + func
// init()"). We keep the header + detail lines for the matching file
// and drop the rest. Returns "" when the file has no semantic block.
func semanticBlockForFile(summary, file string) string {
	if summary == "" {
		return ""
	}
	var out []string
	inBlock := false
	for _, ln := range strings.Split(summary, "\n") {
		indented := strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t")
		if !indented {
			// New file header (or a non-file line) — start a block only
			// when it names the requested file.
			inBlock = strings.Contains(ln, file)
		}
		if inBlock {
			out = append(out, ln)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func sortDiffsByFile(d []kaiDiffFile) {
	if len(d) < 2 {
		return
	}
	// Tiny insertion sort to avoid pulling sort into a single-call
	// helper; this slice is bounded by file count.
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j-1].File > d[j].File; j-- {
			d[j-1], d[j] = d[j], d[j-1]
		}
	}
}

