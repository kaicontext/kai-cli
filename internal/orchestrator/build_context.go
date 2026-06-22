package orchestrator

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// buildMarker maps a build-system marker filename to the canonical
// build + test commands an agent should run from the directory that
// contains it.
type buildMarker struct {
	file  string
	stack string
	build string
	test  string
}

// buildMarkers is ordered most-specific first so a directory with
// several markers reports under the most meaningful one.
var buildMarkers = []buildMarker{
	{"go.mod", "Go", "go build ./...", "go test ./..."},
	{"Cargo.toml", "Rust", "cargo build", "cargo test"},
	{"package.json", "Node", "npm install && npm run build", "npm test"},
	{"pyproject.toml", "Python", "", "pytest"},
}

// buildContextDirSkip names directories the marker scan never
// descends into — heavyweight or generated trees where a marker file
// (e.g. a vendored package.json) would be noise, not the project's
// own build root.
var buildContextDirSkip = map[string]bool{
	".git":         true,
	".kai":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".svelte-kit":  true,
	"testdata":     true,
}

const buildContextMaxDepth = 6
const buildContextMaxHits = 24

// buildContextBlock scans spawnDir for build-system marker files and
// returns a "BUILD CONTEXT" block naming each module's root and the
// exact build/test commands to run from it.
//
// Why this exists: every 2026-05 dogfood trace shows the agent
// burning turns rediscovering where go.mod lives — `go build ./...`
// from the wrong directory, "directory prefix does not contain main
// module", `cat go.mod | head`. The orchestrator already knows the
// answer; handing it to the agent up front removes the single most
// repeated source of meandering. This is the agent-side analogue of
// the planner's project graph-facts injection.
//
// Returns "" when no markers are found — nothing useful to inject.
func buildContextBlock(spawnDir string) string {
	type hit struct {
		rel    string
		marker buildMarker
	}
	var hits []hit

	_ = filepath.WalkDir(spawnDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a transient FS error on one entry shouldn't abort the scan
		}
		rel, relErr := filepath.Rel(spawnDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && buildContextDirSkip[d.Name()] {
				return filepath.SkipDir
			}
			if rel != "." && strings.Count(rel, "/")+1 > buildContextMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		for _, m := range buildMarkers {
			if d.Name() == m.file {
				modDir := filepath.ToSlash(filepath.Dir(rel))
				if modDir == "." {
					modDir = ""
				}
				hits = append(hits, hit{rel: modDir, marker: m})
				break
			}
		}
		return nil
	})

	if len(hits) == 0 {
		return ""
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].rel < hits[j].rel })
	truncated := false
	if len(hits) > buildContextMaxHits {
		hits = hits[:buildContextMaxHits]
		truncated = true
	}

	var b strings.Builder
	b.WriteString("BUILD CONTEXT — verified module roots in this workspace. ")
	b.WriteString("Run build/test from the directory shown; do NOT rediscover go.mod / package.json locations or guess the module path:\n")
	for _, h := range hits {
		loc := h.rel
		if loc == "" {
			loc = "."
		}
		if h.marker.build != "" {
			fmt.Fprintf(&b, "  - %s (%s) — build: `cd %s && %s` — test: `%s`\n",
				loc, h.marker.stack, loc, h.marker.build, h.marker.test)
		} else {
			fmt.Fprintf(&b, "  - %s (%s) — test: `cd %s && %s`\n",
				loc, h.marker.stack, loc, h.marker.test)
		}
	}
	if truncated {
		fmt.Fprintf(&b, "  … (%d+ modules; only the first %d shown)\n", buildContextMaxHits, buildContextMaxHits)
	}
	return b.String()
}
