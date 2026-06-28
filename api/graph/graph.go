package graph

// enginegraph.go: re-exports of the symbols kai-cli/internal/tui/ uses
// from kai-cli/internal/enginegraph. Phase 1 of the TUI API extraction
// — see docs/architecture/tui-api-extraction.md.
//
// Strategy: type aliases for value/handle types, function wrappers
// for constructors. Surprisingly small surface (4 symbols across 6
// TUI files): the TUI mostly threads *DB through to engine
// functions; it doesn't call many methods on it. That justified
// re-export over interface — designing an interface for 4-6
// methods (RemoveFile, IndexFile, GetNode, Close, plus test-only
// Exec / InsertNodeDirect) is over-engineering today.
//
// If/when the TUI ever needs to be testable with a fake graph
// backend, this file is where the interface would land — bounded
// to exactly what the TUI calls, not the full *enginegraph.DB surface.

import enginegraph "github.com/kaicontext/kai-engine/graph"

// DB is the kai semantic-graph database handle. The TUI receives
// one from the kai command's startup wiring (constructed via
// OpenDB below) and threads it through PlannerServices into the
// orchestrator. The TUI's own direct method calls are limited:
// RemoveFile / IndexFile for FTS maintenance after gate-rollback,
// GetNode for snapshot lookups in gate-review, Close in tests.
type DB = enginegraph.DB

// Node is a single semantic-graph node — a snapshot, a file, a
// symbol, etc. The TUI reads Payload (a map[string]any of
// classifier-decorated fields) when rendering gate-review entries.
type Node = enginegraph.Node

// NodeKind enumerates the kinds of graph nodes; the TUI uses only
// KindSnapshot today.
type NodeKind = enginegraph.NodeKind

// KindSnapshot is the NodeKind for a captured snapshot — used by
// the TUI to insert synthetic test snapshots in gate_test.go and
// by gate-review to walk snapshot.gateVerdict payloads.
const KindSnapshot = enginegraph.KindSnapshot

// OpenDB constructs a enginegraph.DB at the given SQLite path with
// object storage rooted at objPath. Used today only in TUI tests
// (gate_test.go etc.) — production wiring constructs the *DB in
// the kai command and threads it in via Options.
//
// Renamed from enginegraph.Open to avoid an ambiguous "api.Open" once
// more openers join api/ (sessions, projects).
func OpenDB(dbPath, objPath string) (*DB, error) {
	return enginegraph.Open(dbPath, objPath)
}
