package projects

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kaicontext/kai-engine/kaipath"
)

// CheckContainerInvariant verifies that root doesn't claim to be both
// a project (has its own .kai/) AND a container of projects (has
// kai.projects.yaml). The two are conceptually mutually exclusive: a
// kai.projects.yaml says "this directory holds projects below it",
// while a .kai/ says "this directory IS the project". Both in the
// same place produces silent cross-DB mismatches downstream — `kai
// capture` writes to one .kai/, the orchestrator reads from another,
// and the user sees opaque SQL errors at integrate time.
//
// Returns nil when the directory is unambiguous (one or neither, but
// not both), and a descriptive error when the conflict exists. The
// caller (typically the TUI launcher) renders the error and bails
// before any DB or spawn work begins.
//
// We don't auto-resolve: deleting either file silently could destroy
// the user's intended layout. The error names both paths and lists
// the two safe ways to fix it.
func CheckContainerInvariant(root string) error {
	if root == "" {
		return nil
	}
	root = filepath.Clean(root)
	yamlPath := filepath.Join(root, ProjectsFileName)
	kaiDirPath := filepath.Join(root, ".kai")

	yamlExists := false
	if _, err := os.Stat(yamlPath); err == nil {
		yamlExists = true
	}
	kaiExists := false
	if info, err := os.Stat(kaiDirPath); err == nil && info.IsDir() {
		kaiExists = true
	}
	if !yamlExists || !kaiExists {
		return nil
	}
	// Tolerate the "self-pointing" yaml: a kai.projects.yaml whose
	// only entry is `path: .` is functionally identical to no yaml
	// at all (it just names the current dir as a project). Some
	// kai init flows produce this — failing with a misconfig error
	// when the user did nothing wrong frustrates more than it helps.
	if isSelfPointingProjectsYAML(yamlPath) {
		return nil
	}
	return fmt.Errorf(
		"projects: this directory has both kai.projects.yaml AND .kai/, which is a misconfig\n"+
			"  yaml at:  %s\n"+
			"  .kai at:  %s\n\n"+
			"kai.projects.yaml means \"this dir holds projects below me\".\n"+
			".kai/ means \"I AM the project\". Both can't be true at once.\n\n"+
			"Two ways to fix:\n"+
			"  1. If this dir is a container (your repos live in subdirs), delete the .kai/ here.\n"+
			"  2. If this dir IS the project, delete kai.projects.yaml here.\n",
		yamlPath, kaiDirPath)
}

// isSelfPointingProjectsYAML returns true when the yaml has exactly
// one entry whose path resolves to "." (the directory containing the
// yaml itself). That's a degenerate but harmless config — equivalent
// to having no yaml at all. Returns false on parse error so the
// stricter "both yaml and .kai/" guard still fires for genuinely
// suspicious states.
func isSelfPointingProjectsYAML(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var f fileShape
	if err := yaml.Unmarshal(body, &f); err != nil {
		return false
	}
	if len(f.Projects) != 1 {
		return false
	}
	p := strings.TrimSpace(f.Projects[0].Path)
	return p == "" || p == "." || p == "./"
}

// Outcome reports what Discover found in a directory. Branches in the
// TUI off this — a Container shouldn't auto-init, an UninitProject
// should prompt to init, and so on.
type Outcome int

const (
	// OutcomeUnknown is the zero value; Discover never returns it.
	// Present so callers can spot uninitialized Outcomes via ==.
	OutcomeUnknown Outcome = iota

	// OutcomeRootsFound: at least one initialized kai project lives at
	// or beneath cwd. The returned Set contains them.
	OutcomeRootsFound

	// OutcomeContainer: cwd looks like a directory of projects rather
	// than a project itself (e.g. ~/projects). Don't auto-init here;
	// surface a "cd into one" message.
	OutcomeContainer

	// OutcomeUninitProject: cwd looks like a single uninitialized
	// project. Offer to run `kai init` for it.
	OutcomeUninitProject

	// OutcomeEmpty: cwd has no kai projects, no project markers, and
	// isn't an obvious container. Likely a brand-new dir or somewhere
	// the user wandered to by mistake. Print help and bail.
	OutcomeEmpty
)

func (o Outcome) String() string {
	switch o {
	case OutcomeRootsFound:
		return "roots-found"
	case OutcomeContainer:
		return "container"
	case OutcomeUninitProject:
		return "uninit-project"
	case OutcomeEmpty:
		return "empty"
	default:
		return "unknown"
	}
}

// Discover scans cwd to figure out what kind of directory we're in
// and returns a Set populated with whichever projects were found
// (possibly empty for non-RootsFound outcomes). The caller is
// responsible for deciding what to do with each outcome — Discover
// is pure: no prompts, no writes, no DB opens.
//
// The walk is deliberately shallow. We check cwd itself plus its
// direct children (depth 1). Going deeper would catch more cases but
// would also pick up vendored repos, build outputs (`node_modules`,
// `target/`, etc.), and other false positives. If a user's projects
// live deeper, they can pin them in kai.projects.yaml.
//
// Existing kai.projects.yaml is honored: pinned entries survive
// rediscovery; unpinned entries that no longer exist on disk are
// dropped.
func Discover(cwd string) (*Set, Outcome) {
	cwd = filepath.Clean(cwd)

	// Capture the original invocation directory BEFORE the walk-up
	// rewrites cwd. Primary() uses this to pick the project the user
	// is sitting in — without it, a user running `kai code` inside
	// ~/projects/kai/kai-server (a subdir of a multi-root workspace
	// rooted at ~/projects/kai) lands in projects[0] (the first
	// project listed in kai.projects.yaml) regardless of where they
	// invoked from. See Set.InvokedFrom.
	invokedFrom := cwd

	// Walk UP from cwd looking for kai.projects.yaml — like
	// git finds .git in an ancestor. Without this, a user
	// who configures a multi-root workspace at
	// ~/projects/foo/kai.projects.yaml gets a single-root
	// workspace whenever they run kai from a deeper subdir
	// (e.g. ~/projects/foo/cli/), because Discover only
	// looked at cwd. The agent then claims sibling roots
	// "don't exist" — they do, the workspace just didn't
	// know about them.
	//
	// Stops at the user's home directory (so we don't scan
	// the entire home dir as a workspace) and at the
	// filesystem root. Returns cwd unchanged if no ancestor
	// has a kai.projects.yaml.
	cwd = locateProjectsFileRoot(cwd)
	set := &Set{DiscoveryRoot: cwd, InvokedFrom: invokedFrom}

	// Step 1: load any existing projects file. Pinned entries are
	// authoritative — they stay regardless of what the scan finds.
	// Every entry in kai.projects.yaml is a configured root —
	// pinned just controls sort order at save time, not load
	// eligibility. Earlier behavior gated this with `if !p.Pinned`,
	// which silently dropped projects from a workspace that the
	// user had explicitly listed in the file (the agent then
	// claimed those projects "didn't exist"). If you wrote it in
	// the yaml, it counts.
	pinned := map[string]*Project{}
	if entries, err := LoadFile(cwd); err == nil {
		// Reject any entry whose path resolves to cwd itself
		// when there are sibling project entries below it.
		// Container-as-project is never useful: capture from
		// the container scoops up files from every nested
		// sibling repo (the May-2026 case: 6192 files
		// scanned, ~500MB of duplicate blobs in
		// `<container>/.kai/objects/`). Without this filter,
		// adding `path: .` to kai.projects.yaml — by accident,
		// by SaveFile round-trip after a future bug, or by a
		// user copy-pasting an example — silently re-creates
		// the same broken state.
		hasSibling := false
		for _, p := range entries {
			if filepath.Clean(p.Path) != cwd {
				hasSibling = true
				break
			}
		}
		for _, p := range entries {
			if hasSibling && filepath.Clean(p.Path) == cwd {
				continue
			}
			if _, err := os.Stat(p.Path); err != nil {
				// Path no longer on disk. Drop silently — the next
				// SaveFile cleans the yaml. Surfacing this via an
				// error would block the TUI launch.
				continue
			}
			if p.Name == "" {
				p.Name = SmartName(p.Path)
			}
			p.KaiDir = kaipath.Resolve(p.Path)
			pinned[p.Path] = p
		}
	}

	// Step 2: scan cwd-itself, then its direct children.
	found := map[string]*Project{}
	for k, v := range pinned {
		found[k] = v
	}

	if proj := projectAt(cwd); proj != nil {
		// Don't overwrite a pinned entry: it may have a hand-set name
		// the user prefers.
		if _, ok := found[proj.Path]; !ok {
			found[proj.Path] = proj
		}
	}

	childDirs, _ := readChildDirs(cwd)
	for _, child := range childDirs {
		if proj := projectAt(child); proj != nil {
			if _, ok := found[proj.Path]; !ok {
				found[proj.Path] = proj
			}
		}
	}

	// Step 3: classify. Container-by-name short-circuits even when
	// children contain kai data — running `kai code` in
	// ~/projects/ shouldn't silently open every initialized repo
	// inside it as one giant workspace. The user almost certainly
	// meant to cd into a single project first.
	if matchesContainerName(cwd) {
		// Drop discovered children; we still return the empty set
		// (with DiscoveryRoot set) so the caller can list them in
		// the "found these projects below, cd into one" message.
		set.projects = nil
		return set, OutcomeContainer
	}
	if len(found) > 0 {
		set.projects = sortProjects(found)
		return set, OutcomeRootsFound
	}
	if isContainer(cwd, childDirs) {
		return set, OutcomeContainer
	}
	if hasProjectMarkers(cwd) {
		// A single uninitialized project sitting at cwd. Populate the
		// set with a single uninitialized Project so the caller can
		// use its Name in the init prompt.
		set.projects = []*Project{{
			Path:   cwd,
			Name:   SmartName(cwd),
			KaiDir: kaipath.Resolve(cwd),
		}}
		return set, OutcomeUninitProject
	}
	return set, OutcomeEmpty
}

// projectAt returns a populated Project iff dir has an initialized
// kai project (the kaipath-resolved dir contains db.sqlite). Returns
// nil otherwise — including when dir doesn't exist.
func projectAt(dir string) *Project {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	kaiDir := kaipath.Resolve(dir)
	if _, err := os.Stat(filepath.Join(kaiDir, "db.sqlite")); err != nil {
		return nil
	}
	return &Project{
		Path:   filepath.Clean(dir),
		Name:   SmartName(dir),
		KaiDir: kaiDir,
	}
}

// readChildDirs returns the immediate subdirectories of dir, sorted
// for stable ordering. Hidden dirs (leading '.') and a small list of
// known-junk dirs are skipped — `.git`, `node_modules`, etc. would
// pollute the scan.
func readChildDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if isJunkDir(name) {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// isJunkDir filters out directory names that frequently appear under
// project trees but are never themselves a project root we want to
// surface. Conservative — we only block names where false positives
// (e.g. an actual project named "node_modules") are vanishingly
// unlikely.
func isJunkDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", "dist", "build", "out",
		"__pycache__", ".venv", "venv", ".tox", ".cache", ".idea",
		".vscode", "tmp", "temp":
		return true
	}
	return false
}

// containerPaths is the set of directory paths that are almost
// certainly project containers, not projects. Matched against the
// absolute path with $HOME-relative comparison so we catch
// /Users/foo/projects on macOS and /home/foo/projects on Linux
// uniformly.
var containerSuffixes = []string{
	"projects", "code", "src", "dev", "work", "repos", "Documents",
	"workspace", "workspaces", "Developer",
}

// matchesContainerName is the name-only branch of the container
// heuristic. Pulled out so the classifier can short-circuit even
// when initialized children exist — see the rationale at the call
// site.
func matchesContainerName(cwd string) bool {
	base := filepath.Base(cwd)
	for _, suffix := range containerSuffixes {
		if base == suffix {
			return true
		}
	}
	return false
}

// isContainer applies the heuristics described in the design:
//
//   - cwd's basename matches a known container name (~/projects, etc.)
//   - cwd has no project markers itself AND has multiple child dirs
//     that each look like a project
//
// Either is sufficient. We err on the side of "not a container" —
// false negatives just mean we offer init when we shouldn't, which
// the user can decline; false positives strand the user with a
// "found nothing" message in their actual project.
func isContainer(cwd string, childDirs []string) bool {
	base := filepath.Base(cwd)
	for _, suffix := range containerSuffixes {
		if base == suffix {
			return true
		}
	}
	// cwd is itself a project root → not a container.
	if hasProjectMarkers(cwd) {
		return false
	}
	// Strong-marker heuristic: ANY child with its own .git/ is an
	// unambiguous "I'm my own repo" signal. A directory holding even
	// one git sub-repo is almost never something we should
	// auto-init at the parent level — capturing here would slurp
	// the sub-repo's source into a parent snapshot, which produced
	// the May-2026 misconfig where ~/projects/kai/.kai indexed code
	// from kai/, kai-server/, kai-e2e/, and kai-tui/ as one giant
	// workspace. Threshold of 1 is intentional: even a parent dir
	// holding a single sibling repo wants the user to cd in, not
	// auto-init at the wrapper level.
	for _, child := range childDirs {
		if hasGitRepo(child) {
			return true
		}
	}
	// Weak-marker fallback: cwd has no project markers itself and
	// has 3+ children that have markers (package.json, go.mod, etc.
	// — but no .git, since that's already covered above). Keeps the
	// generic "directory of projects" detection working for mixed
	// non-git layouts (Python virtualenvs, monorepos, etc.).
	hits := 0
	for _, child := range childDirs {
		if hasProjectMarkers(child) {
			hits++
			if hits >= 3 {
				return true
			}
		}
	}
	return false
}

// hasGitRepo reports whether dir contains a .git directory (a real
// directory; the .git-as-file form used by worktrees / submodules
// is intentionally NOT treated as a repo here, because those forms
// resolve their object store back to a parent and don't represent
// independent state we'd want to refuse to capture across).
func hasGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}

// hasProjectMarkers reports whether dir contains any of the files
// that conventionally mark a project root. Used by both the container
// heuristic and the uninit-project classification.
func hasProjectMarkers(dir string) bool {
	for _, marker := range projectMarkers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

var projectMarkers = []string{
	".git",
	"package.json",
	"go.mod",
	"Cargo.toml",
	"pyproject.toml",
	"setup.py",
	"Gemfile",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"composer.json",
	"deno.json",
	"deno.jsonc",
}

// sortProjects orders by name (stable, deterministic). Pinned
// projects sort first within name order so a user scanning the TUI
// header sees their pinned projects on the left.
func sortProjects(m map[string]*Project) []*Project {
	out := make([]*Project, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// locateProjectsFileRoot walks UP from cwd looking for the
// nearest ancestor that contains kai.projects.yaml, like git
// finds .git. Returns that ancestor; if none, returns cwd
// unchanged.
//
// Bounded walk: stops at the user's home directory and at the
// filesystem root so a stray file at "/" or "$HOME" can't
// hijack discovery for an unrelated project. If the user
// genuinely wants a workspace rooted at home, they can run kai
// from there directly.
func locateProjectsFileRoot(cwd string) string {
	if cwd == "" {
		return cwd
	}
	home, _ := os.UserHomeDir()
	dir := cwd
	for i := 0; i < 32; i++ { // hard cap so a symlink loop can't spin
		if _, err := os.Stat(filepath.Join(dir, ProjectsFileName)); err == nil {
			return dir
		}
		// Stop walking past home or the filesystem root.
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		if home != "" && dir == home {
			break
		}
		dir = parent
	}
	return cwd
}
