package graph

import (
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-core/cas"
)

// TestSnapshotMeta_StatusDoesNotCorruptDigest pins the 2026-06-14 fix for the
// root cause of snapshot corruption: mutable gate status was written into the
// content-addressed payload, breaking id == blake3(payload). UpdateNodePayload
// now splits that status into snapshot_meta and keeps the hashed content intact.
func TestSnapshotMeta_StatusDoesNotCorruptDigest(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "graph.db"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	mint := map[string]interface{}{
		"message":     "baseline",
		"fileCount":   3,
		"fileDigests": []interface{}{"aa", "bb", "cc"},
	}
	id, err := db.InsertNodeDirect(KindSnapshot, mint)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	want, _ := cas.NodeID(string(KindSnapshot), mint)
	if string(id) != string(want) {
		t.Fatalf("mint id != NodeID(mint payload)")
	}

	digestValid := func(ctx string) {
		t.Helper()
		kind, raw, err := db.GetNodeRawPayload(id)
		if err != nil {
			t.Fatalf("%s: raw payload: %v", ctx, err)
		}
		got := cas.Blake3Hash(append([]byte(string(kind)+"\n"), raw...))
		if string(got) != string(id) {
			t.Fatalf("%s: stored payload no longer hashes to id (corrupt)", ctx)
		}
	}
	digestValid("after mint")

	// Decorate exactly like the gate/integrate path: read, add status, write back.
	n, _ := db.GetNode(id)
	n.Payload["gateVerdict"] = "review"
	n.Payload["targetSnapshot"] = "deadbeef"
	n.Payload["dismissed"] = false
	if err := db.UpdateNodePayload(id, n.Payload); err != nil {
		t.Fatalf("decoration via split-router should succeed: %v", err)
	}
	digestValid("after gate decoration") // the regression: this used to corrupt

	// Status is surfaced to readers (overlaid from snapshot_meta) — single + bulk.
	n2, _ := db.GetNode(id)
	if v, _ := n2.Payload["gateVerdict"].(string); v != "review" {
		t.Fatalf("GetNode did not surface gateVerdict, got %q", v)
	}
	snaps, _ := db.GetNodesByKind(KindSnapshot)
	bulkOK := false
	for _, s := range snaps {
		if string(s.ID) == string(id) {
			if v, _ := s.Payload["gateVerdict"].(string); v == "review" {
				bulkOK = true
			}
		}
	}
	if !bulkOK {
		t.Fatalf("GetNodesByKind did not surface gateVerdict")
	}

	// MergeSnapshotMeta updates status without touching the digest.
	if err := db.MergeSnapshotMeta(id, map[string]interface{}{"dismissed": true}); err != nil {
		t.Fatalf("merge meta: %v", err)
	}
	digestValid("after MergeSnapshotMeta")
	n3, _ := db.GetNode(id)
	if d, _ := n3.Payload["dismissed"].(bool); !d {
		t.Fatalf("dismissed=true not surfaced after merge")
	}

	// A genuine change to HASHED content must be rejected loudly.
	bad, _ := db.GetNode(id)
	bad.Payload["fileCount"] = 99
	if err := db.UpdateNodePayload(id, bad.Payload); err == nil {
		t.Fatalf("content mutation (fileCount) should be rejected by the guard")
	}
	digestValid("after rejected content mutation")
}
