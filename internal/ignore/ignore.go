// Package ignore provides gitignore-style pattern matching for file filtering.
package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Pattern represents a single ignore pattern with its properties.
type Pattern struct {
	pattern      string
	negated      bool
	dirOnly      bool
	anchored     bool // Pattern starts with / (matches from root only)
	semanticOnly bool // @semantic-ignore: include in snapshot, skip in analysis
}

// Matcher holds compiled ignore patterns and provides matching functionality.
type Matcher struct {
	patterns []Pattern
	basePath string
}

// NewMatcher creates a new empty Matcher with the given base path.
func NewMatcher(basePath string) *Matcher {
	return &Matcher{
		patterns: []Pattern{},
		basePath: basePath,
	}
}

// AddPattern adds a single pattern string to the matcher.
func (m *Matcher) AddPattern(line string) {
	line = strings.TrimSpace(line)

	// Skip empty lines and comments
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}

	p := Pattern{}

	// Check for @semantic-ignore annotation
	if idx := strings.Index(line, "@semantic-ignore"); idx >= 0 {
		p.semanticOnly = true
		line = strings.TrimSpace(line[:idx])
		if line == "" {
			return
		}
	}

	// Check for negation
	if strings.HasPrefix(line, "!") {
		p.negated = true
		line = line[1:]
	}

	// Check for directory-only pattern
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	// Check for anchored pattern (starts with /)
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = line[1:]
	}

	// Handle patterns without slashes - they match at any level
	// Unless anchored, patterns without / match basename anywhere
	if !p.anchored && !strings.Contains(line, "/") {
		line = "**/" + line
	}

	p.pattern = line
	m.patterns = append(m.patterns, p)
}

// AddPatterns adds multiple pattern strings to the matcher.
func (m *Matcher) AddPatterns(lines []string) {
	for _, line := range lines {
		m.AddPattern(line)
	}
}

// LoadFile loads patterns from a gitignore-style file.
func (m *Matcher) LoadFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Ignore files that don't exist
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		m.AddPattern(scanner.Text())
	}

	return scanner.Err()
}

// Match checks if a path should be excluded from capture (not in snapshot at all).
// Semantic-only patterns are NOT matched here — those files are captured.
// The path should be relative to the matcher's base path.
// isDir indicates whether the path is a directory.
func (m *Matcher) Match(path string, isDir bool) bool {
	return m.match(path, isDir, false)
}

// MatchSemantic checks if a path should be excluded from semantic analysis.
// Returns true for both fully excluded files AND @semantic-ignore files.
// Use this in the symbol analyzer and call graph builder.
func (m *Matcher) MatchSemantic(path string, isDir bool) bool {
	return m.match(path, isDir, true)
}

func (m *Matcher) match(path string, isDir bool, includeSemantic bool) bool {
	// Normalize path separators
	path = filepath.ToSlash(path)

	// Remove leading ./
	path = strings.TrimPrefix(path, "./")

	ignored := false

	for _, p := range m.patterns {
		// Skip semantic-only patterns unless we're doing semantic matching
		if p.semanticOnly && !includeSemantic {
			continue
		}

		// For dirOnly patterns matching a file, we need to check if
		// the file is inside a matching directory
		if p.dirOnly && !isDir {
			// Check if any parent directory matches
			matched := m.matchDirPattern(p.pattern, path)
			if matched {
				ignored = !p.negated
			}
			continue
		}

		matched := m.matchPattern(p.pattern, path)

		if matched {
			ignored = !p.negated
		}
	}

	return ignored
}

// matchDirPattern checks if a path is inside a directory matching the pattern.
func (m *Matcher) matchDirPattern(pattern, path string) bool {
	// Split path into segments and check if any parent directory matches
	// We check prefixes up to but NOT including the full path (since the full path is a file)
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		if m.matchPattern(pattern, prefix) {
			return true
		}
	}
	return false
}

// matchPattern checks if a path matches a single pattern.
func (m *Matcher) matchPattern(pattern, path string) bool {
	// Try exact match first
	matched, _ := doublestar.Match(pattern, path)
	if matched {
		return true
	}

	// For directory patterns, also try matching with trailing content
	// e.g., "node_modules" should match "node_modules/foo/bar.js"
	if !strings.HasSuffix(pattern, "/**") {
		matched, _ = doublestar.Match(pattern+"/**", path)
		if matched {
			return true
		}
	}

	return false
}

// MatchPath is a convenience method that determines if a path is a directory
// by checking if it exists on the filesystem.
func (m *Matcher) MatchPath(path string) bool {
	fullPath := filepath.Join(m.basePath, path)
	info, err := os.Stat(fullPath)
	if err != nil {
		// If we can't stat, assume it's a file
		return m.Match(path, false)
	}
	return m.Match(path, info.IsDir())
}

// LoadDefaults loads only the structural exclusions kai requires for
// its own correctness. Everything else — secrets, dependency dirs,
// build outputs, OS junk, editor temp — is delegated to the project's
// .gitignore (and .kaiignore for kai-specific overrides).
//
// Why "no opinions" beats a curated list:
//
//   - The previous default included "vendor/" because Go projects
//     typically gitignore it. But moby (and others) commit vendor/
//     deliberately for reproducibility — kai's default silently
//     overrode that decision and absorb wiped 9k+ files thinking
//     they were agent-deletions. Symmetric problem for every default
//     that diverges from a given project's git decisions.
//   - Mental model: "if git tracks it, kai tracks it." One source of
//     truth, no surprises. Power-users can still write a .kaiignore
//     to exclude things git tracks but they don't want kai to see.
//   - Security caveat: secrets that get committed to git WILL now
//     get captured by kai (and pushed to kaicontext when the user
//     runs kai push). That's a deliberate choice — kai stops trying
//     to second-guess which committed files are private. Keep
//     secrets out of git in the first place; if they're already in,
//     `.kaiignore` provides an explicit exclude path.
//
// Structural exclusions kept here:
//
//   - .git/ — git's own internals; walking or writing here would
//     corrupt the repo.
//   - .kai/ — kai's own data dir; walking it would create infinite
//     "snapshots-of-snapshots" loops.
//   - .ivcs/, .svn/, .hg/ — other VCS metadata; kai doesn't speak
//     these but their internals shouldn't be tracked either.
//
// These are about tool correctness, not project preferences, so they
// stay regardless of what gitignore says. Same logic git itself uses
// for .git/.
func (m *Matcher) LoadDefaults() {
	excludes := []string{
		".git/",
		".kai/",
		".ivcs/",
		".svn/",
		".hg/",
		// Vite hot-reload artifacts. Vite writes
		// `vite.config.{js,ts,mjs,cjs}.timestamp-<n>-<hash>.{js,mjs}`
		// to disk during dev server startup and deletes them
		// within milliseconds. Capture races with the deletion:
		// when the walker catches one of these files mid-existence,
		// it ingests it into the snapshot, then the file vanishes
		// from disk before the next capture — leaving a permanent
		// dead reference to a blob whose source no longer exists.
		// Every `kai spawn` preflight then fails on the missing
		// blob until the file node is manually purged, and the
		// loop reappears the next time vite hot-reloads.
		//
		// Per-project .gitignore can't realistically fix this for
		// every vite consumer; the pattern is universal vite
		// behavior, not a project preference. Filtering here
		// closes the race entirely. 2026-05-25 dogfood pinned it:
		// one digest (85e7bce8…) accumulated 23+ dead refs over
		// a session, persisting through manual SQL deletes
		// because every dev server restart wrote fresh
		// timestamp-* files for the walker to catch.
		"vite.config.*.timestamp-*",
	}
	m.AddPatterns(excludes)
}

// LoadFromDir loads .gitignore and .kaiignore from a directory.
// Patterns are loaded in order: defaults, .gitignore, .kaiignore
// Later patterns can override earlier ones using negation.
func LoadFromDir(dir string) (*Matcher, error) {
	m := NewMatcher(dir)

	// Load default patterns
	m.LoadDefaults()

	// Load .gitignore if present
	gitignorePath := filepath.Join(dir, ".gitignore")
	if err := m.LoadFile(gitignorePath); err != nil {
		return nil, err
	}

	// Load .kaiignore if present (takes precedence)
	kaiignorePath := filepath.Join(dir, ".kaiignore")
	if err := m.LoadFile(kaiignorePath); err != nil {
		return nil, err
	}

	return m, nil
}

// Compile creates a matcher from a list of pattern strings.
func Compile(patterns []string) *Matcher {
	m := NewMatcher("")
	m.AddPatterns(patterns)
	return m
}
