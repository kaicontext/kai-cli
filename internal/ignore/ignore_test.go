package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBasicPatterns(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		isDir   bool
		want    bool
	}{
		// Simple file patterns
		{"*.log", "debug.log", false, true},
		{"*.log", "logs/debug.log", false, true},
		{"*.log", "debug.txt", false, false},

		// Directory patterns
		{"node_modules/", "node_modules", true, true},
		{"node_modules/", "node_modules/foo.js", false, true},
		{"node_modules/", "src/node_modules", true, true},

		// Anchored patterns
		{"/build", "build", true, true},
		{"/build", "src/build", true, false},

		// Double-star patterns
		{"**/test", "test", true, true},
		{"**/test", "src/test", true, true},
		{"**/test", "src/deep/test", true, true},

		// Specific paths
		{"src/*.js", "src/app.js", false, true},
		{"src/*.js", "src/sub/app.js", false, false},
		{"src/**/*.js", "src/sub/app.js", false, true},
	}

	for _, tt := range tests {
		m := NewMatcher("")
		m.AddPattern(tt.pattern)
		got := m.Match(tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("pattern %q, path %q (isDir=%v): got %v, want %v",
				tt.pattern, tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestNegation(t *testing.T) {
	m := NewMatcher("")
	m.AddPattern("*.log")
	m.AddPattern("!important.log")

	tests := []struct {
		path string
		want bool
	}{
		{"debug.log", true},
		{"important.log", false},
		{"other.log", true},
	}

	for _, tt := range tests {
		got := m.Match(tt.path, false)
		if got != tt.want {
			t.Errorf("path %q: got %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestCommentsAndBlanks(t *testing.T) {
	m := NewMatcher("")
	m.AddPattern("# This is a comment")
	m.AddPattern("")
	m.AddPattern("   ")
	m.AddPattern("*.log")

	if len(m.patterns) != 1 {
		t.Errorf("expected 1 pattern, got %d", len(m.patterns))
	}

	if !m.Match("test.log", false) {
		t.Error("expected test.log to match")
	}
}

func TestDirOnlyPatterns(t *testing.T) {
	m := NewMatcher("")
	m.AddPattern("build/")

	// Should match directories
	if !m.Match("build", true) {
		t.Error("expected build (dir) to match")
	}

	// Should not match files named "build"
	if m.Match("build", false) {
		t.Error("expected build (file) to not match")
	}

	// Should match files inside the directory
	if !m.Match("build/output.js", false) {
		t.Error("expected build/output.js to match")
	}
}

func TestDefaults(t *testing.T) {
	m := NewMatcher("")
	m.LoadDefaults()

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		// Structural exclusions (kai/git internals — kept regardless
		// of project preferences; see LoadDefaults comment).
		{".git", true, true},
		{".kai", true, true},
		// Everything else is now governed by .gitignore / .kaiignore,
		// not the defaults. node_modules / dist / build / .DS_Store /
		// vendor would all show up here in the old behavior; they
		// don't anymore — kai trusts the project's git configuration.
		{"node_modules", true, false},
		{"node_modules/lodash/index.js", false, false},
		{"vendor", true, false},
		{".DS_Store", false, false},
		{"dist", true, false},
		{"build", true, false},
		{"src/app.ts", false, false},
	}

	for _, tt := range tests {
		got := m.Match(tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("Match(%q, isDir=%v): got %v, want %v",
				tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestLoadFile(t *testing.T) {
	// Create temp directory with gitignore
	tmpDir, err := os.MkdirTemp("", "ignore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	gitignore := filepath.Join(tmpDir, ".gitignore")
	content := `# Build artifacts
dist/
*.min.js

# Dependencies
node_modules/

# But keep this one
!important.min.js
`
	if err := os.WriteFile(gitignore, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewMatcher(tmpDir)
	if err := m.LoadFile(gitignore); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"dist", true, true},
		{"dist/bundle.js", false, true},
		{"app.min.js", false, true},
		{"important.min.js", false, false},
		{"node_modules", true, true},
		{"src/app.ts", false, false},
	}

	for _, tt := range tests {
		got := m.Match(tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("path %q (isDir=%v): got %v, want %v",
				tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestLoadFromDir(t *testing.T) {
	// Create temp directory with both ignore files
	tmpDir, err := os.MkdirTemp("", "ignore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create .gitignore
	gitignore := `*.log
dist/
`
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(gitignore), 0644); err != nil {
		t.Fatal(err)
	}

	// Create .kaiignore with override
	kaiignore := `# Keep error logs
!error.log
# But ignore kai-specific stuff
.kai-cache/
`
	if err := os.WriteFile(filepath.Join(tmpDir, ".kaiignore"), []byte(kaiignore), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadFromDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		// Structural defaults (.git/, .kai/) still excluded.
		{".git", true, true},

		// From .gitignore (defaults no longer cover node_modules/dist —
		// they only show up here because the test fixture's .gitignore
		// would have to list them).
		{"debug.log", false, true},
		{"dist", true, true},

		// Overridden by .kaiignore
		{"error.log", false, false},

		// From .kaiignore
		{".kai-cache", true, true},
	}

	for _, tt := range tests {
		got := m.Match(tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("path %q (isDir=%v): got %v, want %v",
				tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestLoadNonexistentFile(t *testing.T) {
	m := NewMatcher("")
	err := m.LoadFile("/nonexistent/path/.gitignore")
	if err != nil {
		t.Errorf("expected nil error for nonexistent file, got %v", err)
	}
}
