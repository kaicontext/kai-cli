package main

import (
	"os"
	"path/filepath"
	"testing"

	"kai/internal/dirio"
	"kai/internal/graph"
	"kai/internal/module"
	"kai/internal/ref"
	"kai/internal/snapshot"
)

// TestTypeScriptGraphEdges pins that a TypeScript fixture with relative imports
// produces IMPORTS and CALLS edges after Analyze. Regression test: the demo
// fixture returned empty results for `kai query callers/dependents/impact`
// because `kai init` imports git history without analysis, then `kai capture`
// hit the skipAnalysis branch (snapshot id unchanged from snap.latest) and
// never built the graph. This test exercises the full path end-to-end.
func TestTypeScriptGraphEdges(t *testing.T) {
	layout := map[string]string{
		"src/auth/validate.ts": `export function validateToken(token: string): boolean {
  return token.length > 10;
}
`,
		"src/db/users.ts": `export async function getUserByEmail(email: string) {
  return { id: "1", email };
}
`,
		"src/auth/login.ts": `import { validateToken } from "./validate";
import { getUserByEmail } from "../db/users";

export async function login(email: string, token: string) {
  if (!validateToken(token)) {
    throw new Error("invalid token");
  }
  const user = await getUserByEmail(email);
  return { userId: user.id, email };
}
`,
	}

	dir := t.TempDir()
	makeFixtureLayout(t, dir, layout)

	dbPath := filepath.Join(dir, ".kai", "db.sqlite")
	objDir := filepath.Join(dir, ".kai", "objects")
	if err := os.MkdirAll(objDir, 0755); err != nil {
		t.Fatal(err)
	}
	db, err := graph.Open(dbPath, objDir)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := applyDBSchema(db); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	creator := snapshot.NewCreator(db, module.NewMatcher(nil))
	source, err := dirio.OpenDirectory(dir)
	if err != nil {
		t.Fatalf("opening dir: %v", err)
	}

	// Step 1: import-style snapshot — no analysis. Matches what runImport does
	// for bulk git history. This leaves the snapshot with no DEFINES_IN edges.
	snapID, err := creator.CreateSnapshot(source)
	if err != nil {
		t.Fatalf("creating snapshot: %v", err)
	}

	refMgr := ref.NewRefManager(db)
	if err := refMgr.Set("snap.latest", snapID, ref.KindSnapshot); err != nil {
		t.Fatalf("setting snap.latest: %v", err)
	}

	definesBefore, _ := db.GetEdgesByContext(snapID, graph.EdgeDefinesIn)
	if len(definesBefore) != 0 {
		t.Fatalf("expected no DEFINES_IN edges before analysis, got %d", len(definesBefore))
	}

	// Step 2: analysis runs (the fix ensures runCapture no longer skips when
	// there are zero DEFINES_IN edges for the matching snap.latest).
	progress := func(current, total int, filename string) {}
	if err := creator.Analyze(snapID, progress); err != nil {
		t.Fatalf("analyzing: %v", err)
	}

	definesAfter, _ := db.GetEdgesByContext(snapID, graph.EdgeDefinesIn)
	if len(definesAfter) == 0 {
		t.Fatal("expected DEFINES_IN edges after analysis, got 0")
	}

	importsEdges, _ := db.GetEdgesByContext(snapID, graph.EdgeImports)
	if len(importsEdges) != 2 {
		t.Fatalf("expected 2 IMPORTS edges (login→validate, login→users), got %d", len(importsEdges))
	}

	// CALLS edges store the intermediate call-node id in `at`, not the
	// snapshot id, so query by type instead of by context.
	callsEdges, _ := db.GetEdgesOfType(graph.EdgeCalls)
	if len(callsEdges) != 2 {
		t.Fatalf("expected 2 CALLS edges (validateToken, getUserByEmail), got %d", len(callsEdges))
	}
}
