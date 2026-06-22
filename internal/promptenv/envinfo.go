// Package promptenv builds the environment-info block that gets
// appended to the model's system prompt: working directory, git
// status, platform, OS version, model identity, knowledge cutoff.
// Used by both the chat and planner system prompts so the model
// has a consistent picture of "where am I running".
package promptenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// ComputeEnvInfo returns the formatted <env> block ready to embed in
// a system prompt. modelID names the model the agent will call;
// additionalWorkingDirectories is rendered when non-empty (multi-root
// workspaces). The function never errors — it returns whatever it
// can gather and lets unknown fields fall through as empty strings.
func ComputeEnvInfo(modelID string, additionalWorkingDirectories []string) string {
	cwd, _ := os.Getwd()
	platform := runtime.GOOS
	osVersion := unameSR()
	gitStatus := "No"
	if isGitRepo(cwd) {
		gitStatus = "Yes"
	}
	shell := shellName()

	var extra string
	if len(additionalWorkingDirectories) > 0 {
		extra = fmt.Sprintf("Additional working directories: %s\n",
			strings.Join(additionalWorkingDirectories, ", "))
	}

	var model, cutoff string
	if modelID != "" {
		model = fmt.Sprintf("\nYou are powered by the model %s.", modelID)
		if c := KnowledgeCutoff(modelID); c != "" {
			cutoff = fmt.Sprintf("\nAssistant knowledge cutoff is %s.", c)
		}
	}

	return fmt.Sprintf(`Here is useful information about the environment you are running in:
<env>
Working directory: %s
Is directory a git repo: %s
%sPlatform: %s
Shell: %s
OS Version: %s
</env>%s%s`,
		cwd, gitStatus, extra, platform, shell, osVersion, model, cutoff)
}

// KnowledgeCutoff maps a model ID to the assistant's stated knowledge
// cutoff. Returns "" when the model isn't recognized — the caller
// renders no cutoff line in that case (better than guessing).
func KnowledgeCutoff(modelID string) string {
	switch {
	case strings.Contains(modelID, "claude-opus-4-7"):
		return "January 2026"
	case strings.Contains(modelID, "claude-sonnet-4-6"):
		return "August 2025"
	case strings.Contains(modelID, "claude-opus-4-6"):
		return "May 2025"
	case strings.Contains(modelID, "claude-haiku-4-5"):
		return "October 2025"
	case strings.Contains(modelID, "claude-haiku-4"):
		return "February 2025"
	}
	return ""
}

// isGitRepo checks for a .git directory or file (worktrees use a
// file). Walks up the directory tree from dir so a subdirectory
// inside a repo still reports true. Direct stat avoids spawning
// `git rev-parse` — same answer, no exec cost.
func isGitRepo(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// unameSR returns "sysname release" matching `uname -sr` output,
// e.g. "Darwin 24.3.0". Read directly from the kernel via the unix
// package — no `uname` exec. Empty string on platforms where the
// syscall fails or returns garbage.
func unameSR() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	sys := nullTrimmed(u.Sysname[:])
	rel := nullTrimmed(u.Release[:])
	if sys == "" || rel == "" {
		return strings.TrimSpace(sys + " " + rel)
	}
	return sys + " " + rel
}

// shellName extracts the basename of $SHELL — "/bin/zsh" → "zsh".
// Returns "unknown" when $SHELL is unset or empty so the env block
// still has a populated field.
func shellName() string {
	s := os.Getenv("SHELL")
	if s == "" {
		return "unknown"
	}
	return filepath.Base(s)
}

func nullTrimmed(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
