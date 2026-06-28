package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/message"
)

// build_after_edit.go: per-edit semantic correctness check. After
// every successful write/edit tool call we run a lightweight build
// command scoped to the touched file's package (for Go) or the
// project root (for TS/Rust) and append the verdict to the matching
// ToolResult before the model sees it.
//
// Why this runs in-loop rather than at integrate: the failing
// add-config-show-command run added an orphan `"kai/internal/config"`
// import on turn 38 and never noticed it didn't compile. Integrate
// ran build_check at the end and rejected — but the agent had 12
// turns of fixable budget remaining and never got the signal. Tight
// build feedback in the loop catches "imported and not used" /
// "undefined: foo" / "missing return" at the moment the model is
// best-positioned to fix them: same context as the edit itself.
//
// Performance: scoped Go builds (go build ./pkg/) are typically
// 50-300ms on this repo's packages. TS and Rust are slower; the
// caller can disable per-edit checks for those ecosystems via
// Options.NoBuildAfterEdit when wall-clock matters. Skipped entirely
// when KAI_SKIP_BUILD_AFTER_EDIT is set in the environment.

const buildAfterEditTimeout = 60 * time.Second

// runBuildAfterEdit inspects toolCalls + their results, identifies
// the set of touched files from successful write/edit calls, picks
// the smallest reasonable build scope, and appends the verdict to
// each edit's ToolResult. Returns true when at least one build was
// actually run (used by tests).
func runBuildAfterEdit(ctx context.Context, opts Options, toolCalls []message.ToolCall, parts []message.ContentPart) bool {
	if opts.Workspace == "" || os.Getenv("KAI_SKIP_BUILD_AFTER_EDIT") != "" || opts.NoBuildAfterEdit {
		return false
	}
	// Bucket edited absolute paths by the call.ID so we can route
	// the verdict back to the right ToolResult on splice.
	type edit struct {
		callID string
		abs    string
	}
	var edits []edit
	for _, c := range toolCalls {
		if !editTools[c.Name] {
			continue
		}
		var args struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal([]byte(c.Input), &args); err != nil || args.FilePath == "" {
			continue
		}
		// Skip edits whose ToolResult is missing or errored — no
		// point compiling against a write that never landed.
		tr, ok := findToolResult(parts, c.ID)
		if !ok || tr.IsError {
			continue
		}
		abs := args.FilePath
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(opts.Workspace, abs)
		}
		edits = append(edits, edit{callID: c.ID, abs: abs})
	}
	if len(edits) == 0 {
		return false
	}

	// Group edits by build scope so we run one check per scope
	// instead of one per file. For Go this is the file's package
	// directory; for other ecosystems it's the workspace root.
	type scope struct {
		ecosystem string
		cwd       string
		args      []string
	}
	byScope := map[string][]string{} // scopeKey -> []callID
	scopes := map[string]scope{}
	for _, e := range edits {
		s := pickBuildScope(opts.Workspace, e.abs)
		if s.ecosystem == "" {
			continue
		}
		key := s.ecosystem + "\x00" + s.cwd
		byScope[key] = append(byScope[key], e.callID)
		scopes[key] = s
	}
	if len(byScope) == 0 {
		return false
	}

	for key, callIDs := range byScope {
		s := scopes[key]
		ok, output, took := runOneBuild(ctx, s)
		trailer := formatBuildTrailer(s.ecosystem, ok, output, took)
		for i, p := range parts {
			tr, isTR := p.(message.ToolResult)
			if !isTR {
				continue
			}
			match := false
			for _, id := range callIDs {
				if tr.ToolCallID == id {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			tr.Content += trailer
			parts[i] = tr
		}
	}
	return true
}

// pickBuildScope returns the smallest reasonable build invocation for
// the edited file. For Go, that's `go build ./` from the file's
// package directory; for TS/Rust, the project root.
func pickBuildScope(workspace, editedAbs string) struct {
	ecosystem string
	cwd       string
	args      []string
} {
	type result = struct {
		ecosystem string
		cwd       string
		args      []string
	}
	dir := filepath.Dir(editedAbs)
	switch strings.ToLower(filepath.Ext(editedAbs)) {
	case ".go":
		if findUpwards(dir, "go.mod") != "" {
			// -o /dev/null because `go build .` against a main
			// package writes the executable to cwd, which left a
			// 56MB binary under kai-cli/cmd/kai/ every time the
			// agent edited that package. Captures swept it into
			// snapshots; gates flagged the binary diff. /dev/null
			// keeps the compile check semantically identical
			// (any build error still surfaces in stderr) while
			// dropping the artifact.
			return result{ecosystem: "go", cwd: dir, args: []string{"go", "build", "-o", "/dev/null", "."}}
		}
	case ".ts", ".tsx":
		if root := findUpwards(dir, "tsconfig.json"); root != "" {
			return result{ecosystem: "ts", cwd: root, args: []string{"npx", "--no-install", "tsc", "--noEmit"}}
		}
	case ".rs":
		if root := findUpwards(dir, "Cargo.toml"); root != "" {
			return result{ecosystem: "rust", cwd: root, args: []string{"cargo", "check", "--quiet"}}
		}
	}
	return result{}
}

// findUpwards walks parents looking for the named manifest. Returns
// the directory containing the manifest, or "" if not found before
// hitting filesystem root.
func findUpwards(start, manifest string) string {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, manifest)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// runOneBuild executes the scope's command with the build-after-edit
// timeout. Returns (ok, combined-output, wall-time). A nil err from
// exec is treated as success; any non-zero exit or context timeout is
// a failure carrying the output for the model to read.
func runOneBuild(ctx context.Context, s struct {
	ecosystem string
	cwd       string
	args      []string
}) (bool, string, time.Duration) {
	cctx, cancel := context.WithTimeout(ctx, buildAfterEditTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.args[0], s.args[1:]...)
	cmd.Dir = s.cwd
	started := time.Now()
	out, err := cmd.CombinedOutput()
	took := time.Since(started)
	if cctx.Err() != nil {
		return false, fmt.Sprintf("build timed out after %s\n%s", buildAfterEditTimeout, out), took
	}
	if err != nil {
		return false, string(out), took
	}
	return true, string(out), took
}

// formatBuildTrailer renders the line(s) appended to a ToolResult's
// Content. Short on success, verbose on failure so the model sees the
// compiler diagnostic in the same turn it sees its own edit.
func formatBuildTrailer(ecosystem string, ok bool, output string, took time.Duration) string {
	const outputCap = 1800
	tt := took.Round(time.Millisecond)
	if ok {
		return fmt.Sprintf("\n\n[auto-build: OK (%s, %s)]", ecosystem, tt)
	}
	trimmed := strings.TrimSpace(output)
	if len(trimmed) > outputCap {
		trimmed = trimmed[:outputCap] + "\n…(truncated)"
	}
	return fmt.Sprintf("\n\n[auto-build: FAIL (%s, %s)]\n%s", ecosystem, tt, trimmed)
}
