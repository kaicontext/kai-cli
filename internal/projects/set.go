package projects

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Set is the multi-root container the TUI runs against. It owns one
// or more Projects under a common DiscoveryRoot, plus the
// kai.projects.yaml file that records membership across launches.
//
// Routing is by longest-prefix match: ProjectFor(path) returns the
// Project whose root is the deepest ancestor of path. Paths outside
// every project return nil — callers decide whether to fall back
// (e.g. /tmp writes during tests) or refuse.
//
// Set is not safe for concurrent mutation, but reads (ProjectFor,
// Projects, Primary) are safe once Open has returned.
type Set struct {
	// DiscoveryRoot is the *resolved* root after the walk-up to find
	// kai.projects.yaml — typically a parent of the user's cwd in a
	// multi-root workspace. Anchors the projects file and the
	// relative-path display in the TUI header.
	DiscoveryRoot string

	// InvokedFrom is the absolute path the user actually ran the
	// command from, BEFORE the walk-up to DiscoveryRoot. Lets Primary()
	// pick the project the user is sitting in instead of always
	// returning projects[0]. Empty when the Set was built without an
	// invocation context (Single(), New(), tests) — Primary then
	// falls back to projects[0].
	InvokedFrom string

	// projects is the ordered list of projects. Order is stable
	// (matches kai.projects.yaml on disk + discovery walk order) so
	// the TUI's "[1] foo  [2] bar" indicators don't shuffle between
	// launches.
	projects []*Project
}

// New constructs a Set with the given discovery root and projects.
// Caller is responsible for ordering and for ensuring no two
// projects share a Path. Used by Discover; tests can also build a
// Set directly without scanning the filesystem.
func New(discoveryRoot string, ps []*Project) *Set {
	return &Set{DiscoveryRoot: discoveryRoot, projects: ps}
}

// Single is a convenience for callers that only have a single
// workspace path (legacy single-root flows, tests). The returned Set
// has DiscoveryRoot == the project path and one Project with Name
// derived from SmartName. DB and GateConfig are left zero-valued —
// callers that need them populated should call Open or assign
// directly.
func Single(absPath string) *Set {
	return &Set{
		DiscoveryRoot: absPath,
		projects: []*Project{{
			Path: absPath,
			Name: SmartName(absPath),
		}},
	}
}

// Projects returns the project list. Callers must not mutate the
// slice or the returned pointers.
func (s *Set) Projects() []*Project {
	if s == nil {
		return nil
	}
	return s.projects
}

// SetProjectsForTest replaces the internal projects slice. Test-
// only escape hatch — production code constructs Sets via
// Discover/Single/file load. Tests need a way to fabricate a
// multi-root set with known names + paths without disk side
// effects.
func (s *Set) SetProjectsForTest(ps []*Project) { s.projects = ps }

// Primary returns the "default" project — the one to use when an
// operation has no path to route on (e.g. session keying, fallback
// for filesystem walkers without an explicit root).
//
// Selection rule:
//
//  1. If InvokedFrom is set and one of the projects owns it (by
//     longest-prefix match), return that project. This is the
//     "focused root" case: a user who ran `kai code` inside
//     ~/projects/kai/kai-server gets kai-server as primary even
//     though DiscoveryRoot walked up to ~/projects/kai.
//
//  2. Otherwise fall back to projects[0]. Covers single-root sets,
//     tests, and the case where the user ran from a dir that's
//     above all projects (rare — usually means the workspace yaml
//     was just discovered above cwd and no project contains cwd).
//
// Longest-prefix matters because nested project layouts are legal
// (a project at /repo and another at /repo/sub both own
// /repo/sub/foo). Mirrors ProjectFor's routing rule so Primary and
// ProjectFor agree on ownership.
func (s *Set) Primary() *Project {
	if s == nil || len(s.projects) == 0 {
		return nil
	}
	if s.InvokedFrom != "" {
		inv := filepath.Clean(s.InvokedFrom)
		var best *Project
		bestLen := -1
		for _, p := range s.projects {
			root := filepath.Clean(p.Path)
			if root == inv || hasPrefixDir(inv, root) {
				if len(root) > bestLen {
					best = p
					bestLen = len(root)
				}
			}
		}
		if best != nil {
			return best
		}
	}
	return s.projects[0]
}

// hasPrefixDir reports whether sub is inside parent — i.e. sub
// starts with parent followed by a path separator. Plain
// strings.HasPrefix would falsely match "/a/bc" against "/a/b".
func hasPrefixDir(sub, parent string) bool {
	if len(sub) <= len(parent) {
		return false
	}
	if sub[:len(parent)] != parent {
		return false
	}
	return sub[len(parent)] == filepath.Separator
}

// ByName returns the project with the given name (case-sensitive),
// or nil. Used by tools that accept an explicit `root:` parameter
// AND by multi-root path-prefix routing (scopeDirInSet, resolveInSet).
//
// Match order:
//  1. Project.Name (the human-friendly label — README H1, package.json
//     name, etc. via SmartName)
//  2. Directory basename of Project.Path
//
// The basename fallback exists because SmartName often produces names
// with spaces ("Kai Server" from a README H1), but the on-disk
// directory is "kai-server". The agent overwhelmingly reaches for
// the directory name when constructing paths — it's what `ls` and
// `cd` produce — so accepting both keeps `view kai-server/foo.go`
// working alongside `view "Kai Server"/foo.go`.
func (s *Set) ByName(name string) *Project {
	if s == nil || name == "" {
		return nil
	}
	for _, p := range s.projects {
		if p.Name == name {
			return p
		}
	}
	for _, p := range s.projects {
		if filepath.Base(p.Path) == name {
			return p
		}
	}
	return nil
}

// ProjectFor returns the project that owns the given path, by
// longest-prefix match. The path must be absolute; relative paths
// are first resolved against DiscoveryRoot. Returns nil when the
// path lives outside every project.
//
// Longest-prefix matters because nested layouts are legal: a
// project at /repo and another at /repo/sub both match
// /repo/sub/file.go; we want /repo/sub.
func (s *Set) ProjectFor(path string) *Project {
	if s == nil || path == "" {
		return nil
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.DiscoveryRoot, abs)
	}
	abs = filepath.Clean(abs)

	var best *Project
	bestLen := -1
	for _, p := range s.projects {
		if !p.Owns(abs) {
			continue
		}
		if len(p.Path) > bestLen {
			best = p
			bestLen = len(p.Path)
		}
	}
	return best
}

// Close releases every open project DB. Safe to call on a Set whose
// projects were never opened (Project.DB == nil entries are
// skipped).
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, p := range s.projects {
		if p.DB == nil {
			continue
		}
		if err := p.DB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing %s: %w", p.Name, err))
		}
		p.DB = nil
	}
	return errors.Join(errs...)
}
