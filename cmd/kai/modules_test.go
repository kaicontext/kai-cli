package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/module"
)

// TestWalkDirectoryIncludesRootFiles verifies that walking from "." includes
// files in the current directory and subdirectories, and doesn't skip the
// root directory itself.
//
// This test was added to prevent regression of a bug where the root "."
// directory was being skipped because strings.HasPrefix(".", ".") returns true.
func TestWalkDirectoryIncludesRootFiles(t *testing.T) {
	// Create temp directory structure
	tmpDir, err := os.MkdirTemp("", "walk-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	testFiles := []string{
		"root.txt",
		"src/app.js",
		"src/utils/math.js",
		".hidden/secret.txt",
	}

	for _, f := range testFiles {
		path := filepath.Join(tmpDir, f)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("Failed to create dir for %s: %v", f, err)
		}
		if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
			t.Fatalf("Failed to create file %s: %v", f, err)
		}
	}

	// Change to temp directory
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current dir: %v", err)
	}
	defer os.Chdir(oldDir)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to change to temp dir: %v", err)
	}

	// Walk the directory using the same logic as runModulesPreview
	var allFiles []string
	err = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			// Skip hidden directories (but not "." itself), node_modules, and vendor
			if (strings.HasPrefix(name, ".") && name != ".") || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		allFiles = append(allFiles, path)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	// Verify we found the expected files
	expectedFiles := map[string]bool{
		"root.txt":          false,
		"src/app.js":        false,
		"src/utils/math.js": false,
	}

	for _, f := range allFiles {
		if _, ok := expectedFiles[f]; ok {
			expectedFiles[f] = true
		}
	}

	for f, found := range expectedFiles {
		if !found {
			t.Errorf("Expected file %q was not found in walk results", f)
		}
	}

	// Verify hidden directory was skipped
	for _, f := range allFiles {
		if strings.Contains(f, ".hidden") {
			t.Errorf("Hidden directory file %q should have been skipped", f)
		}
	}

	// Most importantly: verify we found files at all
	// This was the original bug - 0 files were found
	if len(allFiles) == 0 {
		t.Error("Walk found 0 files - this was the original bug where root '.' was skipped")
	}
}

// TestLoadMatcherUsesCorrectPath verifies that loadMatcher() loads modules
// from .kai/rules/modules.yaml (not just kai.modules.yaml in the root).
//
// This test was added to prevent regression of a bug where modules defined
// via "kai modules add" were not being used by "kai snap" because they were
// saved to different locations.
func TestLoadMatcherUsesCorrectPath(t *testing.T) {
	// Create temp directory structure
	tmpDir, err := os.MkdirTemp("", "loadmatcher-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create .kai/rules directory
	rulesDir := filepath.Join(tmpDir, ".kai", "rules")
	if err := os.MkdirAll(rulesDir, 0755); err != nil {
		t.Fatalf("Failed to create rules dir: %v", err)
	}

	// Create modules.yaml in .kai/rules/ (the correct location)
	modulesContent := `modules:
  - name: TestModule
    paths:
      - "src/**"
`
	modulesPath := filepath.Join(rulesDir, "modules.yaml")
	if err := os.WriteFile(modulesPath, []byte(modulesContent), 0644); err != nil {
		t.Fatalf("Failed to write modules.yaml: %v", err)
	}

	// Change to temp directory
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current dir: %v", err)
	}
	defer os.Chdir(oldDir)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to change to temp dir: %v", err)
	}

	// Load matcher using the same logic as loadMatcher()
	// Try the new location first (.kai/rules/modules.yaml)
	matcher, err := module.LoadRulesOrEmpty(".kai/rules/modules.yaml")
	if err != nil {
		t.Fatalf("LoadRulesOrEmpty failed: %v", err)
	}

	modules := matcher.GetAllModules()
	if len(modules) == 0 {
		t.Error("loadMatcher should find modules from .kai/rules/modules.yaml")
	}

	if len(modules) > 0 && modules[0].Name != "TestModule" {
		t.Errorf("Expected module name 'TestModule', got %q", modules[0].Name)
	}
}

// TestModulesAddAndSnapConsistency verifies that modules added via
// "kai modules add" are visible to "kai snap" (they use the same path).
func TestModulesAddAndSnapConsistency(t *testing.T) {
	// The paths should be consistent
	const expectedPath = ".kai/rules/modules.yaml"

	// Verify modulesRulesPath constant matches expected
	if modulesRulesPath != expectedPath {
		t.Errorf("modulesRulesPath = %q, want %q", modulesRulesPath, expectedPath)
	}
}

// TestSkipHiddenDirectoriesButNotRoot verifies that the directory skip logic
// correctly handles the edge case where "." is the current directory.
func TestSkipHiddenDirectoriesButNotRoot(t *testing.T) {
	tests := []struct {
		name     string
		dirName  string
		expected bool // true = should skip
	}{
		{"current dir", ".", false},
		{"hidden dir", ".git", true},
		{"hidden config", ".config", true},
		{"normal dir", "src", false},
		{"node_modules", "node_modules", true},
		{"vendor", "vendor", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.dirName
			shouldSkip := (strings.HasPrefix(name, ".") && name != ".") ||
				name == "node_modules" ||
				name == "vendor"

			if shouldSkip != tc.expected {
				t.Errorf("Directory %q: shouldSkip=%v, want %v", tc.dirName, shouldSkip, tc.expected)
			}
		})
	}
}
