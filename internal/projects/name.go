package projects

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SmartName picks a human-friendly project name for the directory at
// root. The lookup order is intentional: package metadata beats README
// heading beats directory name, because the metadata files are what
// the project's own ecosystem treats as canonical.
//
//  1. package.json `name`               (Node)
//  2. go.mod module last segment        (Go)
//  3. Cargo.toml [package].name         (Rust)
//  4. pyproject.toml [project].name     (Python)
//  5. First H1 of README* (any case)
//  6. Directory base name, de-kebabed (last resort)
//
// Returns the de-kebabed directory name on any error or when no source
// yields a usable string. SmartName never returns an empty string for
// a non-empty root.
func SmartName(root string) string {
	if root == "" {
		return ""
	}
	root = filepath.Clean(root)

	if n := nameFromPackageJSON(root); n != "" {
		return n
	}
	if n := nameFromGoMod(root); n != "" {
		return n
	}
	if n := nameFromCargoToml(root); n != "" {
		return n
	}
	if n := nameFromPyproject(root); n != "" {
		return n
	}
	if n := nameFromReadme(root); n != "" {
		return n
	}
	return prettifyDirName(filepath.Base(root))
}

// nameFromPackageJSON pulls the top-level `name` field. We intentionally
// don't recurse into workspaces — for a monorepo, the root package.json
// is the right answer; sub-packages are detected as their own projects.
func nameFromPackageJSON(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Name)
}

var goModRE = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// nameFromGoMod returns the last path segment of the module declaration.
// e.g. `module github.com/foo/bar` → "bar". Matches how Go developers
// usually refer to their own modules in conversation.
func nameFromGoMod(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	m := goModRE.FindSubmatch(data)
	if len(m) < 2 {
		return ""
	}
	mod := strings.TrimSpace(string(m[1]))
	if mod == "" {
		return ""
	}
	if i := strings.LastIndex(mod, "/"); i >= 0 {
		return mod[i+1:]
	}
	return mod
}

// nameFromCargoToml uses a tiny TOML scrape rather than pulling in a
// full toml dependency — Cargo's [package].name is one regex away and
// adding a parser for one field isn't worth it.
var cargoNameRE = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)

func nameFromCargoToml(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return ""
	}
	// Only consider the [package] section. If the file starts with a
	// different table (rare for a real crate root), skip — better to
	// fall through than misidentify a dependency name.
	s := string(data)
	pkgIdx := strings.Index(s, "[package]")
	if pkgIdx < 0 {
		return ""
	}
	rest := s[pkgIdx:]
	if next := strings.Index(rest[len("[package]"):], "\n["); next >= 0 {
		rest = rest[:len("[package]")+next]
	}
	m := cargoNameRE.FindStringSubmatch(rest)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// nameFromPyproject pulls [project].name out of pyproject.toml. Same
// minimal-parser strategy as Cargo.
var pyNameRE = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)

func nameFromPyproject(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "pyproject.toml"))
	if err != nil {
		return ""
	}
	s := string(data)
	idx := strings.Index(s, "[project]")
	if idx < 0 {
		return ""
	}
	rest := s[idx:]
	if next := strings.Index(rest[len("[project]"):], "\n["); next >= 0 {
		rest = rest[:len("[project]")+next]
	}
	m := pyNameRE.FindStringSubmatch(rest)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// nameFromReadme scans the first ~50 lines of any README* file for an
// H1 heading. Markdown `# Title` and the underlined `Title\n====`
// forms both count. We cap the read so a giant README doesn't slow
// down discovery.
func nameFromReadme(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), "readme") {
			continue
		}
		f, err := os.Open(filepath.Join(root, name))
		if err != nil {
			continue
		}
		title := scanReadmeTitle(f)
		_ = f.Close()
		if title != "" {
			return title
		}
	}
	return ""
}

func scanReadmeTitle(r io.Reader) string {
	scanner := bufio.NewScanner(r)
	var prev string
	lines := 0
	for scanner.Scan() && lines < 50 {
		line := strings.TrimSpace(scanner.Text())
		lines++
		// Markdown H1: leading # followed by space+content.
		if strings.HasPrefix(line, "# ") {
			t := strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if t != "" {
				return t
			}
		}
		// Underlined H1: previous non-empty line followed by ===.
		if prev != "" && len(line) >= 3 && strings.Trim(line, "=") == "" {
			return prev
		}
		if line != "" {
			prev = line
		}
	}
	return ""
}

// prettifyDirName converts a directory base into a friendlier label
// for the prompt: "kai-server" → "kai server", "myProject" stays as-is.
// Kept minimal — over-prettifying makes the wrong call in too many
// cases (e.g. "rust-analyzer" → "Rust Analyzer" reads well, but
// "go-redis" → "Go Redis" is wrong; we skip title-casing entirely).
func prettifyDirName(s string) string {
	if s == "" {
		return ""
	}
	// Collapse hyphens/underscores to spaces. Don't title-case — that
	// loses information and gets project names wrong more often than
	// it gets them right.
	s = strings.ReplaceAll(s, "_", "-")
	return s
}
