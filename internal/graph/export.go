package graph

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ExportNode represents a node in the export output.
type ExportNode struct {
	ID      []byte          `json:"id"`
	Kind    NodeKind        `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// MarshalJSON encodes the ID field as hex.
func (n ExportNode) MarshalJSON() ([]byte, error) {
	type Alias ExportNode
	return json.Marshal(&struct {
		ID string `json:"id"`
		Alias
	}{
		ID:    hex.EncodeToString(n.ID),
		Alias: Alias(n),
	})
}

// ExportEdge represents an edge in the export output.
type ExportEdge struct {
	Src  []byte   `json:"src"`
	Dst  []byte   `json:"dst"`
	Type EdgeType `json:"type"`
	At   []byte   `json:"at"`
	ID   uint64   `json:"id"`
}

// MarshalJSON encodes the Src, Dst, At fields as hex and ID as hex.
func (e ExportEdge) MarshalJSON() ([]byte, error) {
	type Alias ExportEdge
	return json.Marshal(&struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
		At  string `json:"at"`
		ID  string `json:"id"`
		Alias
	}{
		Src:   hex.EncodeToString(e.Src),
		Dst:   hex.EncodeToString(e.Dst),
		At:    hex.EncodeToString(e.At),
		ID:    fmt.Sprintf("0x%x", e.ID),
		Alias: Alias(e),
	})
}

// ExportResult holds one page of exported graph data.
type ExportResult struct {
	Nodes       []ExportNode `json:"nodes"`
	Edges       []ExportEdge `json:"edges"`
	NodeCursor  int64        `json:"node_cursor"`
	EdgeCursor  int64        `json:"edge_cursor"`
	HasMoreNodes bool        `json:"has_more_nodes"`
	HasMoreEdges bool        `json:"has_more_edges"`
}

// ExportPage returns a page of nodes and edges using rowid-based cursor pagination.
// The limit is split evenly between nodes and edges.
func (db *DB) ExportPage(nodeCursor, edgeCursor int64, limit int) (*ExportResult, error) {
	if limit <= 0 {
		limit = 10000
	}
	half := limit / 2
	if half < 1 {
		half = 1
	}

	result := &ExportResult{
		NodeCursor: nodeCursor,
		EdgeCursor: edgeCursor,
	}

	// Query nodes with rowid cursor
	nodeRows, err := db.conn.Query(
		"SELECT rowid, id, kind, payload FROM nodes WHERE rowid > ? ORDER BY rowid LIMIT ?",
		nodeCursor, half,
	)
	if err != nil {
		return nil, fmt.Errorf("query nodes: %w", err)
	}
	defer nodeRows.Close()

	for nodeRows.Next() {
		var n ExportNode
		var rowid int64
		if err := nodeRows.Scan(&rowid, &n.ID, &n.Kind, &n.Payload); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		result.Nodes = append(result.Nodes, n)
		result.NodeCursor = rowid
	}
	if err := nodeRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate nodes: %w", err)
	}

	// Check if there are more nodes beyond this page
	var nodeCount int
	err = db.conn.QueryRow("SELECT COUNT(*) FROM nodes WHERE rowid > ?", result.NodeCursor).Scan(&nodeCount)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("count nodes: %w", err)
	}
	result.HasMoreNodes = nodeCount > 0

	// Query edges with rowid cursor
	edgeRows, err := db.conn.Query(
		"SELECT rowid, src, dst, type, at FROM edges WHERE rowid > ? ORDER BY rowid LIMIT ?",
		edgeCursor, half,
	)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer edgeRows.Close()

	for edgeRows.Next() {
		var e ExportEdge
		var rowid int64
		if err := edgeRows.Scan(&rowid, &e.Src, &e.Dst, &e.Type, &e.At); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		e.ID = uint64(rowid)
		result.Edges = append(result.Edges, e)
		result.EdgeCursor = rowid
	}
	if err := edgeRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate edges: %w", err)
	}

	// Check if there are more edges beyond this page
	var edgeCount int
	err = db.conn.QueryRow("SELECT COUNT(*) FROM edges WHERE rowid > ?", result.EdgeCursor).Scan(&edgeCount)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("count edges: %w", err)
	}
	result.HasMoreEdges = edgeCount > 0

	return result, nil
}