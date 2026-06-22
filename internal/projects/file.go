package projects

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// ProjectsFileName is the on-disk record of a Set: which projects
// were discovered (or pinned) under the discovery root. Lives at
// <discoveryRoot>/kai.projects.yaml. The file is optional —
// rediscovery from disk produces the same Set as long as nothing has
// moved.
const ProjectsFileName = "kai.projects.yaml"

// fileShape is the on-disk yaml schema. Paths are stored relative to
// the discovery root so the file is portable across machines (clone
// the repo, rediscovery still works).
type fileShape struct {
	Projects []fileEntry `yaml:"projects"`
}

type fileEntry struct {
	// Path is relative to the directory that contains the file.
	Path string `yaml:"path"`
	Name string `yaml:"name,omitempty"`
	// Pinned: see Project.Pinned.
	Pinned bool `yaml:"pinned,omitempty"`
	// Description: see Project.Description. Optional one-line
	// summary of what an agent should look for in this project,
	// rendered verbatim in the first-turn workspace overview.
	Description string `yaml:"description,omitempty"`
}

// LoadFile reads kai.projects.yaml from discoveryRoot. Returns an
// empty file (no error) when the file doesn't exist — discovery is
// expected to fall through to a fresh scan in that case.
func LoadFile(discoveryRoot string) ([]*Project, error) {
	path := filepath.Join(discoveryRoot, ProjectsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var f fileShape
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	out := make([]*Project, 0, len(f.Projects))
	for _, e := range f.Projects {
		if e.Path == "" {
			continue
		}
		abs := e.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(discoveryRoot, abs)
		}
		abs = filepath.Clean(abs)
		out = append(out, &Project{
			Path:        abs,
			Name:        e.Name,
			Pinned:      e.Pinned,
			Description: e.Description,
		})
	}
	return out, nil
}

// SaveFile writes kai.projects.yaml back to disk. Idempotent: writes
// only when the rendered yaml differs from the file on disk, so we
// don't churn mtime on every TUI launch. Order is stabilized by name
// within pinned/unpinned groups so diffs stay clean.
func SaveFile(discoveryRoot string, projects []*Project) error {
	if discoveryRoot == "" {
		return errors.New("projects: SaveFile requires a discovery root")
	}
	entries := make([]fileEntry, 0, len(projects))
	for _, p := range projects {
		if p == nil || p.Path == "" {
			continue
		}
		rel, err := filepath.Rel(discoveryRoot, p.Path)
		if err != nil || rel == "" {
			rel = p.Path
		}
		entries = append(entries, fileEntry{
			Path:        filepath.ToSlash(rel),
			Name:        p.Name,
			Pinned:      p.Pinned,
			Description: p.Description,
		})
	}
	// Pinned first (so a teammate skimming the file sees the
	// "permanent" members at the top), then alphabetical by name.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Pinned != entries[j].Pinned {
			return entries[i].Pinned
		}
		return entries[i].Name < entries[j].Name
	})

	out, err := yaml.Marshal(fileShape{Projects: entries})
	if err != nil {
		return fmt.Errorf("rendering projects file: %w", err)
	}
	path := filepath.Join(discoveryRoot, ProjectsFileName)
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(out) {
		return nil
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
