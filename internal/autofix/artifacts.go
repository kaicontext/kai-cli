package autofix

// The artifact guard moved to kai-engine/gitio so every consumer of
// DirtyPaths (autofix, kai ship, the TUI close-out offer) filters
// identically; these wrappers keep autofix's existing API. Provenance:
// this is the guard that stopped a zero-edit run from shipping a PR
// whose entire diff was `.codex/hooks.json`.

import "github.com/kaicontext/kai-engine/gitio"

// IsKaiArtifact reports whether p is something kai writes for its own
// operation rather than a change toward the fix.
func IsKaiArtifact(p string) bool { return gitio.IsKaiArtifact(p) }

// FilterArtifacts returns paths with kai's own artifacts removed,
// preserving order.
func FilterArtifacts(paths []string) []string { return gitio.FilterKaiArtifacts(paths) }
