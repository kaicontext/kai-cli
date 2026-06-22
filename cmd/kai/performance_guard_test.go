package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kai/internal/dirio"
	"kai/internal/graph"
	"kai/internal/module"
	"kai/internal/snapshot"
)

func TestPerf_GraphBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf test in short mode")
	}

	// Create a fixture with ~50 TS files
	layout := make(map[string]string)
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("src/module_%d.ts", i)
		content := fmt.Sprintf(`export function fn_%d() { return %d; }`, i, i)
		if i > 0 {
			// Import from previous module to create dependency chain
			content = fmt.Sprintf("import { fn_%d } from './module_%d';\n%s", i-1, i-1, content)
		}
		layout[name] = content
	}
	// Add some test files
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("tests/module_%d.test.ts", i*5)
		layout[name] = fmt.Sprintf("import { fn_%d } from '../src/module_%d';\ntest('fn_%d', () => expect(fn_%d()).toBe(%d));", i*5, i*5, i*5, i*5, i*5)
	}

	dir := t.TempDir()
	makeFixtureLayout(t, dir, layout)

	dbPath := filepath.Join(dir, ".kai", "db.sqlite")
	objDir := filepath.Join(dir, ".kai", "objects")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
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

	source, err := dirio.OpenDirectory(dir)
	if err != nil {
		t.Fatalf("opening dir: %v", err)
	}

	start := time.Now()
	snapID, err := creator.CreateSnapshot(source)
	if err != nil {
		t.Fatalf("creating snapshot: %v", err)
	}

	progress := func(current, total int, filename string) {}
	if err := creator.AnalyzeSymbols(snapID, progress); err != nil {
		t.Fatalf("analyzing symbols: %v", err)
	}
	if err := creator.AnalyzeCalls(snapID, progress); err != nil {
		t.Fatalf("analyzing calls: %v", err)
	}
	elapsed := time.Since(start)

	t.Logf("graph build for 60 files: %v", elapsed)
	if elapsed > 5*time.Second {
		t.Errorf("graph build took %v, expected < 5s", elapsed)
	}
}

func TestPerf_ImpactAnalysis(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf test in short mode")
	}

	// Create fixture with dependency chain
	layout := make(map[string]string)
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("src/mod_%d.ts", i)
		content := fmt.Sprintf(`export const val_%d = %d;`, i, i)
		if i > 0 {
			content = fmt.Sprintf("import { val_%d } from './mod_%d';\n%s", i-1, i-1, content)
		}
		layout[name] = content
	}
	for i := 0; i < 5; i++ {
		layout[fmt.Sprintf("tests/mod_%d.test.ts", i*6)] = fmt.Sprintf(
			"import { val_%d } from '../src/mod_%d';\ntest('val', () => expect(val_%d).toBe(%d));",
			i*6, i*6, i*6, i*6)
	}

	dir := t.TempDir()
	makeFixtureLayout(t, dir, layout)
	initGitRepo(t, dir)

	// Make a change
	layout["src/mod_0.ts"] = `export const val_0 = 999;`
	makeFixtureLayout(t, dir, map[string]string{"src/mod_0.ts": `export const val_0 = 999;`})
	gitCommitAll(t, dir, "change mod_0")

	start := time.Now()
	selected := runSelectionInDir(t, dir)
	elapsed := time.Since(start)

	t.Logf("impact analysis: %v, selected %d tests", elapsed, len(selected))
	if elapsed > 5*time.Second {
		t.Errorf("impact analysis took %v, expected < 5s", elapsed)
	}
}
