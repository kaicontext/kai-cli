package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"kai/internal/dirio"
	"kai/internal/graph"
	"kai/internal/module"
	"kai/internal/snapshot"
	"kai/internal/util"
)

// setupSelectionFixture creates two directory snapshots (base and head),
// builds a changeset, and returns the selected test targets.
func setupSelectionFixture(t *testing.T, baseLayout, headLayout map[string]string) []string {
	t.Helper()

	// Create base snapshot from baseLayout
	baseDir := t.TempDir()
	makeFixtureLayout(t, baseDir, baseLayout)

	// Create head snapshot from headLayout
	headDir := t.TempDir()
	makeFixtureLayout(t, headDir, headLayout)

	return runSelectionFromDirs(t, baseDir, headDir)
}

// runSelectionFromDirs creates snapshots from two directories, builds a changeset,
// and returns the selected test targets using the same logic as generateCIPlanFromGitRange.
func runSelectionFromDirs(t *testing.T, baseDir, headDir string) []string {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	objDir := filepath.Join(tmpDir, "objects")
	os.MkdirAll(objDir, 0755)

	db, err := graph.Open(dbPath, objDir)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()

	if err := applyDBSchema(db); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	matcher := module.NewMatcher(nil)
	creator := snapshot.NewCreator(db, matcher)
	progress := func(current, total int, filename string) {}

	// Create base snapshot
	baseSource, err := dirio.OpenDirectory(baseDir)
	if err != nil {
		t.Fatalf("opening base dir: %v", err)
	}
	baseSnap, err := creator.CreateSnapshot(baseSource)
	if err != nil {
		t.Fatalf("creating base snapshot: %v", err)
	}
	creator.AnalyzeSymbols(baseSnap, progress)
	creator.AnalyzeCalls(baseSnap, progress)

	// Create head snapshot
	headSource, err := dirio.OpenDirectory(headDir)
	if err != nil {
		t.Fatalf("opening head dir: %v", err)
	}
	headSnap, err := creator.CreateSnapshot(headSource)
	if err != nil {
		t.Fatalf("creating head snapshot: %v", err)
	}
	creator.AnalyzeSymbols(headSnap, progress)
	creator.AnalyzeCalls(headSnap, progress)

	// Create changeset
	_, err = createChangesetFromSnapshots(db, baseSnap, headSnap, "")
	if err != nil {
		t.Fatalf("creating changeset: %v", err)
	}

	// Get changed files
	changedFiles, err := getChangedFiles(db, creator, baseSnap, headSnap)
	if err != nil {
		t.Fatalf("getting changed files: %v", err)
	}

	// Get all files from head snapshot
	files, err := creator.GetSnapshotFiles(headSnap)
	if err != nil {
		t.Fatalf("getting snapshot files: %v", err)
	}

	// Build file path map
	filePathByID := make(map[string]string)
	for _, f := range files {
		path, _ := f.Payload["path"].(string)
		idHex := util.BytesToHex(f.ID)
		filePathByID[idHex] = path
	}

	// Use import graph to find affected tests (same logic as generateCIPlanFromGitRange)
	affectedTargets := make(map[string]bool)
	for _, changedPath := range changedFiles {
		testsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeTests)
		for _, e := range testsEdges {
			srcHex := util.BytesToHex(e.Src)
			if path, ok := filePathByID[srcHex]; ok {
				affectedTargets[path] = true
			}
		}
		importsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeImports)
		for _, e := range importsEdges {
			srcHex := util.BytesToHex(e.Src)
			if path, ok := filePathByID[srcHex]; ok {
				if isTestFilePath(path) {
					affectedTargets[path] = true
				}
			}
		}
	}

	var selected []string
	for t := range affectedTargets {
		selected = append(selected, t)
	}
	sort.Strings(selected)
	return selected
}

// runSelectionInDir creates snapshots from git HEAD~1 and HEAD using git rev-parse
// to resolve commit hashes. Requires a git repo with at least 2 commits.
func runSelectionInDir(t *testing.T, dir string) []string {
	t.Helper()

	// Resolve HEAD~1 and HEAD to actual commit hashes
	getHash := func(ref string) string {
		cmd := exec.Command("git", "rev-parse", ref)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git rev-parse %s: %v", ref, err)
		}
		return strings.TrimSpace(string(out))
	}

	baseHash := getHash("HEAD~1")
	headHash := getHash("HEAD")

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	objDir := filepath.Join(tmpDir, "objects")
	os.MkdirAll(objDir, 0755)

	db, err := graph.Open(dbPath, objDir)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()

	if err := applyDBSchema(db); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	matcher := module.NewMatcher(nil)
	creator := snapshot.NewCreator(db, matcher)
	progress := func(current, total int, filename string) {}

	baseSnap, err := createSnapshotFromGitRef(db, dir, baseHash)
	if err != nil {
		t.Fatalf("creating base snapshot: %v", err)
	}
	creator.AnalyzeSymbols(baseSnap, progress)
	creator.AnalyzeCalls(baseSnap, progress)

	headSnap, err := createSnapshotFromGitRef(db, dir, headHash)
	if err != nil {
		t.Fatalf("creating head snapshot: %v", err)
	}
	creator.AnalyzeSymbols(headSnap, progress)
	creator.AnalyzeCalls(headSnap, progress)

	_, err = createChangesetFromSnapshots(db, baseSnap, headSnap, "")
	if err != nil {
		t.Fatalf("creating changeset: %v", err)
	}

	changedFiles, err := getChangedFiles(db, creator, baseSnap, headSnap)
	if err != nil {
		t.Fatalf("getting changed files: %v", err)
	}

	files, err := creator.GetSnapshotFiles(headSnap)
	if err != nil {
		t.Fatalf("getting snapshot files: %v", err)
	}

	filePathByID := make(map[string]string)
	for _, f := range files {
		path, _ := f.Payload["path"].(string)
		idHex := util.BytesToHex(f.ID)
		filePathByID[idHex] = path
	}

	affectedTargets := make(map[string]bool)
	for _, changedPath := range changedFiles {
		testsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeTests)
		for _, e := range testsEdges {
			srcHex := util.BytesToHex(e.Src)
			if path, ok := filePathByID[srcHex]; ok {
				affectedTargets[path] = true
			}
		}
		importsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeImports)
		for _, e := range importsEdges {
			srcHex := util.BytesToHex(e.Src)
			if path, ok := filePathByID[srcHex]; ok {
				if isTestFilePath(path) {
					affectedTargets[path] = true
				}
			}
		}
	}

	var selected []string
	for t := range affectedTargets {
		selected = append(selected, t)
	}
	sort.Strings(selected)
	return selected
}

// isTestFilePath is a simple test file check for the selection tests.
func isTestFilePath(path string) bool {
	for _, pattern := range []string{".test.", ".spec.", "_test."} {
		if strings.Contains(path, pattern) {
			return true
		}
	}
	return false
}

func TestSelection_ImportsStrategy(t *testing.T) {
	baseLayout := map[string]string{
		"src/utils.ts":      `export function add(a: number, b: number) { return a + b; }`,
		"src/app.ts":        "import { add } from './utils';\nexport const result = add(1, 2);",
		"src/orphan.ts":     `export const orphan = "unused";`,
		"tests/app.test.ts": "import { result } from '../src/app';\ntest('works', () => expect(result).toBe(3));",
	}

	tests := []struct {
		name     string
		change   map[string]string
		wantAny  []string // at least these tests should be selected
		wantNone bool     // expect no tests selected
	}{
		{
			name:    "change source file that test imports",
			change:  map[string]string{"src/app.ts": "import { add } from './utils';\nexport const result = add(2, 3);"},
			wantAny: []string{"tests/app.test.ts"},
		},
		{
			name:     "change orphan file with no dependents",
			change:   map[string]string{"src/orphan.ts": `export const orphan = "changed";`},
			wantNone: true,
		},
		{
			// Note: changing only a test file that has no inbound IMPORTS/TESTS edges
			// won't be selected by the import graph alone. This tests current behavior.
			// The changed-file-is-test-file case should ideally be handled separately.
			name:    "change test file itself with source change",
			change:  map[string]string{"src/app.ts": "import { add } from './utils';\nexport const result = add(3, 4);", "tests/app.test.ts": "import { result } from '../src/app';\ntest('updated', () => expect(result).toBe(7));"},
			wantAny: []string{"tests/app.test.ts"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headLayout := make(map[string]string)
			for k, v := range baseLayout {
				headLayout[k] = v
			}
			for k, v := range tt.change {
				headLayout[k] = v
			}

			selected := setupSelectionFixture(t, baseLayout, headLayout)

			if tt.wantNone {
				if len(selected) != 0 {
					t.Errorf("expected no tests selected, got %v", selected)
				}
				return
			}

			for _, want := range tt.wantAny {
				found := false
				for _, s := range selected {
					if s == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %s to be selected, got %v", want, selected)
				}
			}
		})
	}
}

func TestSelection_TransitiveDeps(t *testing.T) {
	baseLayout := map[string]string{
		"src/c.ts":        `export const c = 1;`,
		"src/b.ts":        "import { c } from './c';\nexport const b = c + 1;",
		"src/a.ts":        "import { b } from './b';\nexport const a = b + 1;",
		"tests/a.test.ts": "import { a } from '../src/a';\ntest('a', () => expect(a).toBe(3));",
	}
	headLayout := make(map[string]string)
	for k, v := range baseLayout {
		headLayout[k] = v
	}
	headLayout["src/c.ts"] = `export const c = 100;`

	selected := setupSelectionFixture(t, baseLayout, headLayout)
	t.Logf("selected tests for transitive change: %v", selected)
}

func TestSelection_NoTestsAffected(t *testing.T) {
	baseLayout := map[string]string{
		"src/utils.ts":        `export function add(a: number, b: number) { return a + b; }`,
		"src/orphan.ts":       `export const orphan = "unused";`,
		"tests/utils.test.ts": "import { add } from '../src/utils';\ntest('add', () => expect(add(1,2)).toBe(3));",
	}
	headLayout := make(map[string]string)
	for k, v := range baseLayout {
		headLayout[k] = v
	}
	headLayout["src/orphan.ts"] = `export const orphan = "modified";`

	selected := setupSelectionFixture(t, baseLayout, headLayout)

	if len(selected) != 0 {
		t.Errorf("expected no tests for orphan change, got %v", selected)
	}
}
