package autofix

// artifacts.go identifies files kai writes for its own operation — the
// agent-session ingest hooks (.codex/hooks.json, .claude/settings.local.json)
// and the kai data dir (.kai/) — so the headless loop never mistakes them
// for the fix. This is the bug that once shipped a PR whose entire diff was
// `.codex/hooks.json`: the agent edited nothing, kai's hook installer wrote
// that file during the run, and `git add -A` swept it into the commit.

import "strings"

// kaiArtifactPrefixes are repo-relative path prefixes kai writes during a
// run. Directory prefixes (not exact files) so future additions under these
// dirs are covered too.
var kaiArtifactPrefixes = []string{".codex/", ".claude/", ".kai/"}

// IsKaiArtifact reports whether p is something kai writes for its own
// operation rather than a change toward the fix.
func IsKaiArtifact(p string) bool {
	p = strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
	for _, pre := range kaiArtifactPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// FilterArtifacts returns paths with kai's own artifacts removed, preserving
// order. Returns a fresh slice (never aliases the input).
func FilterArtifacts(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !IsKaiArtifact(p) {
			out = append(out, p)
		}
	}
	return out
}
