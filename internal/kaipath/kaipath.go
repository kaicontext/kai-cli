// Package kaipath resolves the on-disk location of a project's kai data
// directory. New projects in a git repo land in `.git/kai/` so git auto-
// ignores the directory; standalone projects (or pre-existing setups)
// keep `.kai/` for backward compatibility.
package kaipath

import (
	"os"
	"path/filepath"
)

// Resolve returns the kai data directory for the given project root.
// Priority:
//
//  1. $KAI_DIR — explicit override (used verbatim, no Join)
//  2. <root>/.kai — backward compat: if it already exists, keep using it
//  3. <root>/.git/kai — preferred default for fresh inits in a git repo
//     (git auto-ignores everything under .git/, so no .gitignore noise)
//  4. <root>/.kai — final fallback for non-git projects
//
// `.git` is only treated as a git repo when it's a real directory; the
// gitfile form used by worktrees and submodules falls through to .kai
// because we don't yet resolve the common-dir.
func Resolve(root string) string {
	if v := os.Getenv("KAI_DIR"); v != "" {
		return v
	}
	if info, err := os.Stat(filepath.Join(root, ".kai")); err == nil && info.IsDir() {
		return filepath.Join(root, ".kai")
	}
	if info, err := os.Stat(filepath.Join(root, ".git")); err == nil && info.IsDir() {
		return filepath.Join(root, ".git", "kai")
	}
	return filepath.Join(root, ".kai")
}

// NeedsGitignore reports whether the resolved kai directory should be
// added to .gitignore. Paths under .git/ are auto-ignored by git so we
// skip the gitignore step there; everywhere else (a literal .kai, or a
// custom KAI_DIR inside the worktree) needs an explicit entry.
func NeedsGitignore(resolved string) bool {
	return filepath.Base(filepath.Dir(resolved)) != ".git"
}
