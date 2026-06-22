// Package projects is the TUI's re-export of kai-cli/internal/projects.
// Phase 1 of the TUI API extraction (see
// docs/architecture/tui-api-extraction.md).
package projects

import engine "kai/internal/projects"

type Set = engine.Set
type Project = engine.Project

// New constructs a multi-root Set. Mirrors engine.projects.New.
func New(workspace string, projects []*Project) *Set {
	return engine.New(workspace, projects)
}
