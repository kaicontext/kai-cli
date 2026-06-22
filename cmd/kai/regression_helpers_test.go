package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// makeFixtureLayout creates files from a path→content map in dir.
func makeFixtureLayout(t *testing.T, dir string, layout map[string]string) {
	t.Helper()
	for rel, content := range layout {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", abs, err)
		}
	}
}

// initGitRepo does git init + add + commit in dir.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "add", "."},
		{"git", "commit", "-m", "initial"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE=2000-01-01T00:00:00+00:00")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %s\n%s", args, err, out)
		}
	}
}

// gitCommitAll stages and commits all changes.
func gitCommitAll(t *testing.T, dir string, msg string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", msg},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-02T00:00:00+00:00", "GIT_COMMITTER_DATE=2000-01-02T00:00:00+00:00")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %s\n%s", args, err, out)
		}
	}
}

// compareGolden compares got bytes against testdata/expected/<name>.
// If KAI_UPDATE_GOLDEN=1, writes got to golden file instead.
func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	goldenPath := filepath.Join("testdata", "expected", name)

	if os.Getenv("KAI_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0755); err != nil {
			t.Fatalf("mkdir for golden: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		t.Logf("updated golden file: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden file %s: %v (run with KAI_UPDATE_GOLDEN=1 to create)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("output differs from golden file %s.\nGot:\n%s\nWant:\n%s", goldenPath, got, want)
	}
}

var (
	reUUID      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reHexID     = regexp.MustCompile(`[0-9a-f]{64}`)
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[^\s"]*`)
	reAbsPath   = regexp.MustCompile(`/[^\s"]*/(kai-cli|\.kai)/`)
)

// normalizeJSON replaces timestamps, UUIDs, hex IDs, absolute paths for deterministic comparison.
func normalizeJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	s := string(data)
	s = reUUID.ReplaceAllString(s, "<UUID>")
	s = reHexID.ReplaceAllString(s, "<HEX64>")
	s = reTimestamp.ReplaceAllString(s, "<TIMESTAMP>")
	s = reAbsPath.ReplaceAllString(s, "<ABS>/")
	return []byte(s)
}

// sortJSONArrays recursively sorts arrays in parsed JSON by a stable key.
func sortJSONArrays(v interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		for _, child := range val {
			sortJSONArrays(child)
		}
	case []interface{}:
		for _, child := range val {
			sortJSONArrays(child)
		}
		sort.Slice(val, func(i, j int) bool {
			bi, _ := json.Marshal(val[i])
			bj, _ := json.Marshal(val[j])
			return string(bi) < string(bj)
		})
	}
}

// assertNoFalseNegatives checks that every ground-truth test is in the selected set.
func assertNoFalseNegatives(t *testing.T, selected, groundTruth []string) {
	t.Helper()
	set := make(map[string]bool)
	for _, s := range selected {
		set[s] = true
	}
	for _, gt := range groundTruth {
		if !set[gt] {
			t.Errorf("FALSE NEGATIVE: %s not selected", gt)
		}
	}
}
