package main

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/kaicontext/kai-engine/ref"
)

// TestWorkspaceRefSet pins the ref contract that lets a workspace cross the
// remote: `kai push --ws` must emit a `ws.<name>` node ref — the exact name
// `kai fetch --ws` resolves — plus base/head snapshot refs and one ref per open
// changeset, each pointing at the right object. A regression here is precisely
// the F-12 failure mode: `fetch --ws` reporting "workspace not found on remote".
func TestWorkspaceRefSet(t *testing.T) {
	digest := func(b byte) []byte {
		d := make([]byte, 32)
		for i := range d {
			d[i] = b
		}
		return d
	}
	wsDigest := digest(0x11)
	base := digest(0x22)
	head := digest(0x33)
	cs1 := digest(0x44)
	cs2 := digest(0x55)

	refs := workspaceRefSet("feat", wsDigest, base, head, [][]byte{cs1, cs2})

	byName := map[string]*ref.Ref{}
	for _, r := range refs {
		byName[r.Name] = r
	}

	// The node ref fetch --ws resolves: must exist, be a Workspace, and point at
	// the workspace's content digest.
	node := byName["ws.feat"]
	if node == nil {
		t.Fatal("missing ws.feat node ref — fetch --ws would report 'not found on remote'")
	}
	if node.TargetKind != ref.KindWorkspace {
		t.Errorf("ws.feat kind = %v, want Workspace", node.TargetKind)
	}
	if !bytes.Equal(node.TargetID, wsDigest) {
		t.Errorf("ws.feat -> %x, want %x", node.TargetID, wsDigest)
	}

	// Snapshot bounds.
	if r := byName["ws.feat.base"]; r == nil || !bytes.Equal(r.TargetID, base) {
		t.Errorf("ws.feat.base wrong or missing: %+v", r)
	}
	if r := byName["ws.feat.head"]; r == nil || !bytes.Equal(r.TargetID, head) {
		t.Errorf("ws.feat.head wrong or missing: %+v", r)
	}

	// One ref per changeset, named by the 8-char hex prefix, pointing at the cs.
	for _, cs := range [][]byte{cs1, cs2} {
		name := "ws.feat.cs." + hex.EncodeToString(cs)[:8]
		if r := byName[name]; r == nil || !bytes.Equal(r.TargetID, cs) {
			t.Errorf("%s wrong or missing: %+v", name, r)
		}
	}

	if len(refs) != 5 {
		t.Errorf("got %d refs, want 5 (node + base + head + 2 changesets)", len(refs))
	}

	// A workspace with no open changesets still publishes the three core refs.
	if got := workspaceRefSet("solo", wsDigest, base, head, nil); len(got) != 3 {
		t.Errorf("no-changeset workspace produced %d refs, want 3 (node + base + head)", len(got))
	}
}
