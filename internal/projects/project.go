// Package projects models a multi-root kai workspace: a set of one or
// more initialized kai projects discovered (or pinned) under a common
// discovery root. The TUI runs against a Set; tools route per-file by
// asking the Set which Project owns a given path.
//
// One Project ≈ one initialized kai project on disk. It owns a graph
// DB, a kai dir (`.kai/` or `.git/kai/` per kaipath conventions), and
// a safetygate config. Projects are independent — there is no merging
// of graphs or gate config across projects.
package projects

import (
	"path/filepath"

	"kai/internal/graph"
	"kai/internal/safetygate"
)

// Project is one initialized kai project. The fields are populated
// lazily by Set.Open: Path/Name/KaiDir on discovery; DB and GateConfig
// when the Set is opened for use.
type Project struct {
	// Path is the absolute project root (the directory the user would
	// `cd` into to work on this project).
	Path string

	// Name is the user-facing label (from package.json/go.mod/etc, or
	// the directory base). Used in the TUI header and in agent prompt
	// injection so the model can talk about projects by name.
	Name string

	// KaiDir is the resolved kai data directory for this project,
	// computed via kaipath.Resolve(Path). Cached on discovery so the
	// resolution rules (`.kai` vs `.git/kai` vs `$KAI_DIR`) don't run
	// on every access.
	KaiDir string

	// Pinned, when true, means the project is kept in
	// kai.projects.yaml across discoveries even if a future scan
	// wouldn't otherwise find it. Lets the user opt projects in
	// permanently (e.g. "this sibling repo is part of my workspace
	// even though it's not initialized yet").
	Pinned bool

	// Description, when non-empty, is a one-line "what's in this
	// project" annotation rendered verbatim in the agent's first-
	// turn workspace overview. Lets a planner answer "where does
	// X live" in zero searches instead of N grep round-trips
	// against the wrong project — the 2026-05-27 /exit dogfood
	// pinned this exactly: the planner spent 8 of 10 turns searching
	// kai-desktop for slash-command code that lives in kai/kai-cli/.
	// Optional, user-edited in kai.projects.yaml via:
	//
	//   - path: kai
	//     description: "Go monorepo — TUI, planner, CLI commands"
	//
	// kai never auto-generates; descriptions are the project owner's
	// summary of what an agent should look for here.
	Description string

	// DB is the project's graph DB. nil until Set.Open is called.
	// Closed by Set.Close.
	DB *graph.DB

	// GateConfig is the project's safetygate config. Loaded by
	// Set.Open; defaults to safetygate.DefaultConfig when no
	// gate.yaml is present.
	GateConfig safetygate.Config
}

// Owns reports whether absPath lives under this project's root.
// Used by Set.ProjectFor for the longest-prefix routing decision.
func (p *Project) Owns(absPath string) bool {
	if p == nil || p.Path == "" {
		return false
	}
	rel, err := filepath.Rel(p.Path, absPath)
	if err != nil {
		return false
	}
	// Rel returns "../..." when absPath is outside p.Path; reject any
	// path that starts with "..". The "." case (absPath == p.Path) is
	// fine — the root itself counts as owned.
	if rel == "." {
		return true
	}
	if len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return false
	}
	return true
}
