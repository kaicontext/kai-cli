package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kaicontext/kai-engine/projects"
)

// kaiGitStateTool answers "is the file I just read what's actually
// running?" — the gap that produces the dogfood failure pattern where
// the agent reads a fix in the working tree, claims "this is in
// place," and the user has to point out the change is uncommitted /
// unpushed / undeployed.
//
// Crucially this works ACROSS repos. The agent often cites files in
// sibling repos (kai-server, kai-playground, etc.). `git` operates on
// whatever directory it's invoked from, so as long as we resolve the
// path under the multi-root set and shell out from that path, the
// answer reflects the right repo's state — not the kai-cli repo the
// TUI was launched in.
type kaiGitStateTool struct {
	workspace string
	set       *projects.Set
}

type kaiGitStateParams struct {
	Path string `json:"path"`
}

func (t *kaiGitStateTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_git_state",
		Description: "Report git state for a path (file or directory): uncommitted/untracked changes, " +
			"last commit (sha + subject + relative date), and ahead/behind vs. upstream. " +
			"Use this BEFORE claiming a fix or feature is \"in place\" or \"deployed\" — reading " +
			"a file shows what's in the working tree, not what's committed or running in production. " +
			"Works across repos (multi-root): pass a project-prefixed path like \"kai-server/foo/bar.go\".",
		Parameters: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory path. Defaults to the workspace root.",
			},
		},
	}
}

func (t *kaiGitStateTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiGitStateParams
	if len(call.Input) > 0 {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_git_state: invalid input json: " + err.Error()), nil
		}
	}
	resolved, err := scopeDirInSet(t.set, t.workspace, p.Path)
	if err != nil {
		return NewTextErrorResponse("kai_git_state: " + err.Error()), nil
	}

	// Anchor every git invocation to the directory we resolved.
	// `git status path` from the wrong repo would silently return
	// "fatal: not a git repository" or — worse — answer from the
	// surrounding repo. Setting cmd.Dir on every shell-out keeps the
	// answer scoped to the repo that owns the path.
	gitDir := resolved
	// If resolved is a file, git's working-tree commands need the
	// containing directory (or accept a pathspec, but `git rev-parse
	// --show-toplevel` only makes sense from a directory).
	if info, statErr := os.Stat(resolved); statErr == nil && !info.IsDir() {
		gitDir = filepath.Dir(resolved)
	}

	top, err := runGit(ctx, gitDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return NewTextErrorResponse(
			"kai_git_state: not in a git repository: " + strings.TrimSpace(top) +
				" (path resolved to " + resolved + ")"), nil
	}
	repoRoot := strings.TrimSpace(top)

	// Branch + ahead/behind. `git status -sb` gives us a compact
	// header line we can parse, but we want structured output, so
	// we ask explicitly. Branch may be empty for detached HEAD.
	branch, _ := runGit(ctx, repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	branch = strings.TrimSpace(branch)

	// Porcelain status, scoped to the path so a busy workspace
	// doesn't bury the relevant file in 200 unrelated entries.
	porcelainArgs := []string{"status", "--porcelain=v1"}
	if p.Path != "" {
		porcelainArgs = append(porcelainArgs, "--", resolved)
	}
	statusOut, statusErr := runGit(ctx, repoRoot, porcelainArgs...)
	if statusErr != nil {
		return NewTextErrorResponse("kai_git_state: git status failed: " + statusErr.Error()), nil
	}
	statusLines := splitNonEmpty(statusOut)

	// Last commit touching the path. When no path was given we get
	// HEAD; with a path we get the most recent commit that mentions
	// it — answers "when did this last change?"
	logArgs := []string{"log", "-1", "--format=%h|%s|%ar"}
	if p.Path != "" {
		logArgs = append(logArgs, "--", resolved)
	}
	logOut, _ := runGit(ctx, repoRoot, logArgs...)
	logLine := strings.TrimSpace(logOut)

	// Ahead/behind vs upstream. Fails silently when there's no
	// upstream configured (common on local feature branches) — we
	// just omit the line rather than confuse the agent with an
	// error it can't act on.
	aheadBehind := ""
	if upstream, upErr := runGit(ctx, repoRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); upErr == nil {
		up := strings.TrimSpace(upstream)
		counts, cErr := runGit(ctx, repoRoot, "rev-list", "--left-right", "--count", "HEAD..."+up)
		if cErr == nil {
			parts := strings.Fields(strings.TrimSpace(counts))
			if len(parts) == 2 {
				aheadBehind = fmt.Sprintf("ahead %s, behind %s of %s", parts[0], parts[1], up)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "repo: %s\n", repoRoot)
	if branch != "" {
		fmt.Fprintf(&b, "branch: %s", branch)
		if aheadBehind != "" {
			fmt.Fprintf(&b, " (%s)", aheadBehind)
		}
		b.WriteString("\n")
	}
	if logLine != "" {
		fmt.Fprintf(&b, "last commit: %s\n", logLine)
	}
	if len(statusLines) == 0 {
		if p.Path != "" {
			fmt.Fprintf(&b, "working tree: clean for %s\n", p.Path)
		} else {
			b.WriteString("working tree: clean\n")
		}
	} else {
		fmt.Fprintf(&b, "working tree: %d changed entr%s\n",
			len(statusLines), plural(len(statusLines), "y", "ies"))
		// Cap at 20 entries so a giant uncommitted refactor doesn't
		// dwarf the rest of the answer; the count above is honest.
		max := 20
		for i, line := range statusLines {
			if i == max {
				fmt.Fprintf(&b, "  … (%d more)\n", len(statusLines)-max)
				break
			}
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func plural(n int, sing, pl string) string {
	if n == 1 {
		return sing
	}
	return pl
}
