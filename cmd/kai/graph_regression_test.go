package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"kai/internal/dirio"
	"kai/internal/graph"
	"kai/internal/module"
	"kai/internal/snapshot"
)

// setupGraphFixture creates a temp dir with a synthetic TS project, initializes
// the kai database, creates a snapshot, and analyzes symbols. Returns the db and snapshot ID.
func setupGraphFixture(t *testing.T, layout map[string]string) (*graph.DB, []byte, string) {
	t.Helper()
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
	t.Cleanup(func() { db.Close() })

	if err := applyDBSchema(db); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	matcher := module.NewMatcher(nil)
	creator := snapshot.NewCreator(db, matcher)

	source, err := dirio.OpenDirectory(dir)
	if err != nil {
		t.Fatalf("opening dir source: %v", err)
	}

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

	return db, snapID, dir
}

// graphSnapshot queries all nodes and edges from the database and returns sorted JSON.
func graphSnapshot(t *testing.T, db *graph.DB) []byte {
	t.Helper()
	type nodeRecord struct {
		Kind    string                 `json:"kind"`
		Payload map[string]interface{} `json:"payload"`
	}
	type edgeRecord struct {
		SrcKind string `json:"srcKind"`
		Type    string `json:"type"`
		DstKind string `json:"dstKind"`
	}
	type graphDump struct {
		Nodes []nodeRecord `json:"nodes"`
		Edges []edgeRecord `json:"edges"`
	}

	rows, err := db.Query("SELECT kind, payload FROM nodes ORDER BY kind, payload")
	if err != nil {
		t.Fatalf("querying nodes: %v", err)
	}
	defer rows.Close()

	var dump graphDump
	for rows.Next() {
		var kind, payloadStr string
		if err := rows.Scan(&kind, &payloadStr); err != nil {
			t.Fatalf("scanning node: %v", err)
		}
		var payload map[string]interface{}
		json.Unmarshal([]byte(payloadStr), &payload)
		// Remove non-deterministic fields
		delete(payload, "digest")
		delete(payload, "createdAt")
		delete(payload, "sourceRef")
		delete(payload, "fileDigests")
		dump.Nodes = append(dump.Nodes, nodeRecord{Kind: kind, Payload: payload})
	}

	edgeRows, err := db.Query(`
		SELECT n1.kind, e.type, n2.kind
		FROM edges e
		JOIN nodes n1 ON e.src = n1.id
		JOIN nodes n2 ON e.dst = n2.id
		ORDER BY n1.kind, e.type, n2.kind`)
	if err != nil {
		t.Fatalf("querying edges: %v", err)
	}
	defer edgeRows.Close()

	for edgeRows.Next() {
		var srcKind, edgeType, dstKind string
		if err := edgeRows.Scan(&srcKind, &edgeType, &dstKind); err != nil {
			t.Fatalf("scanning edge: %v", err)
		}
		dump.Edges = append(dump.Edges, edgeRecord{SrcKind: srcKind, Type: edgeType, DstKind: dstKind})
	}

	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		t.Fatalf("marshaling graph dump: %v", err)
	}
	return data
}

var tsLayout = map[string]string{
	"src/utils.ts":      `export function add(a: number, b: number) { return a + b; }`,
	"src/app.ts":        "import { add } from './utils';\nexport const result = add(1, 2);",
	"tests/app.test.ts": "import { result } from '../src/app';\ntest('works', () => expect(result).toBe(3));",
}

func TestGraph_DeterministicSnapshot(t *testing.T) {
	snap1 := func() []byte {
		db, _, _ := setupGraphFixture(t, tsLayout)
		return graphSnapshot(t, db)
	}()
	snap2 := func() []byte {
		db, _, _ := setupGraphFixture(t, tsLayout)
		return graphSnapshot(t, db)
	}()

	if string(snap1) != string(snap2) {
		t.Error("graph snapshots differ between identical runs — nondeterminism detected")
		t.Logf("Run 1:\n%s", snap1)
		t.Logf("Run 2:\n%s", snap2)
	}
}

func TestGraph_EdgeCreation(t *testing.T) {
	db, _, _ := setupGraphFixture(t, tsLayout)

	// Check that IMPORTS edges exist
	importEdges, err := db.GetEdgesOfType(graph.EdgeImports)
	if err != nil {
		t.Fatalf("getting IMPORTS edges: %v", err)
	}
	if len(importEdges) == 0 {
		t.Error("expected IMPORTS edges, got 0")
	}

	// Check that we have File nodes for all 3 fixture files
	fileNodes, err := db.GetNodesByKind("File")
	if err != nil {
		t.Fatalf("getting File nodes: %v", err)
	}
	if len(fileNodes) < 3 {
		t.Errorf("expected at least 3 File nodes, got %d", len(fileNodes))
	}
}

func TestGraph_RerunIdempotent(t *testing.T) {
	dir := t.TempDir()
	makeFixtureLayout(t, dir, tsLayout)

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
	progress := func(current, total int, filename string) {}

	source1, _ := dirio.OpenDirectory(dir)
	snap1, err := creator.CreateSnapshot(source1)
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	creator.AnalyzeSymbols(snap1, progress)
	creator.AnalyzeCalls(snap1, progress)

	// Get distinct node kinds after first pass
	kinds1 := getNodeKinds(t, db)

	// Create second snapshot of same content
	source2, _ := dirio.OpenDirectory(dir)
	snap2, err := creator.CreateSnapshot(source2)
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	creator.AnalyzeSymbols(snap2, progress)
	creator.AnalyzeCalls(snap2, progress)

	kinds2 := getNodeKinds(t, db)

	if len(kinds1) != len(kinds2) {
		t.Errorf("node kinds differ after rerun: %v vs %v", kinds1, kinds2)
	}
	for i := range kinds1 {
		if i < len(kinds2) && kinds1[i] != kinds2[i] {
			t.Errorf("kind %d: %s vs %s", i, kinds1[i], kinds2[i])
		}
	}
}

func getNodeKinds(t *testing.T, db *graph.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT DISTINCT kind FROM nodes ORDER BY kind")
	if err != nil {
		t.Fatalf("querying kinds: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

func TestGraph_ContentAddressedStability(t *testing.T) {
	db1, _, _ := setupGraphFixture(t, tsLayout)
	db2, _, _ := setupGraphFixture(t, tsLayout)

	getDigests := func(db *graph.DB) []string {
		rows, err := db.Query("SELECT payload FROM nodes WHERE kind = 'File' ORDER BY payload")
		if err != nil {
			t.Fatalf("querying: %v", err)
		}
		defer rows.Close()
		var digests []string
		for rows.Next() {
			var p string
			rows.Scan(&p)
			var m map[string]interface{}
			json.Unmarshal([]byte(p), &m)
			if d, ok := m["digest"].(string); ok {
				digests = append(digests, d)
			}
		}
		sort.Strings(digests)
		return digests
	}

	d1 := getDigests(db1)
	d2 := getDigests(db2)

	if len(d1) != len(d2) {
		t.Fatalf("different digest counts: %d vs %d", len(d1), len(d2))
	}
	for i := range d1 {
		if d1[i] != d2[i] {
			t.Errorf("digest mismatch at %d: %s vs %s", i, d1[i], d2[i])
		}
	}
}
