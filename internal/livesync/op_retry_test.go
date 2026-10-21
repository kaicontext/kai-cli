package livesync

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kaicontext/kai-core/crdt"
	"github.com/kaicontext/kai-engine/remote"
)

// A receiver that gets a peer's edits without the ops they're anchored to
// (the peer's genesis push was lost) must not write the incomplete
// materialized view: that replaced a 55-line kv.py with an empty file.
func TestApplyOpsWaitsForMissingAnchors(t *testing.T) {
	eA, dirA := newCRDTEngine(t, "A")
	eB, dirB := newCRDTEngine(t, "B")
	a, b := filepath.Join(dirA, "kv.py"), filepath.Join(dirB, "kv.py")

	writeFile(t, a, "def get():\n    pass\n")
	genesis := genOps(eA, "kv.py", a) // never reaches B at first
	writeFile(t, a, "def get():\n    pass\n\ndef put():\n    pass\n")
	edit := genOps(eA, "kv.py", a)

	writeFile(t, b, "local content that must survive\n")
	eB.applyOps("kv.py", b, edit, "A")
	if got, _ := os.ReadFile(b); string(got) != "local content that must survive\n" {
		t.Fatalf("receiver wrote an incomplete file: %q", got)
	}

	eB.applyOps("kv.py", b, genesis, "A") // the missing ops arrive
	want, _ := os.ReadFile(a)
	if got, _ := os.ReadFile(b); string(got) != string(want) {
		t.Fatalf("after the anchors arrived, B = %q, want %q", got, want)
	}
}

// A push that fails is retried with the next push instead of being lost
// (its ops are already in the local Doc, so the next diff no longer
// contains them). Queued ops survive a restart.
func TestFailedOpPushIsRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var delivered [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/sync/push") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			http.Error(w, "connection reset", http.StatusBadGateway)
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.StdEncoding.DecodeString(body.Content)
		delivered = append(delivered, raw)
	}))
	defer server.Close()

	eA, dirA := newCRDTEngine(t, "A")
	eA.client = remote.NewClient(server.URL, "t", "r")
	a := filepath.Join(dirA, "kv.py")

	writeFile(t, a, "def get():\n    pass\n")
	eA.pushOpsForChange("kv.py") // genesis push fails
	if got := len(eA.unsent["kv.py"]); got == 0 {
		t.Fatal("failed push was not queued")
	}
	if _, err := os.Stat(eA.unsentPath()); err != nil {
		t.Fatalf("queued ops not persisted: %v", err)
	}
	restarted := &Engine{kaiDir: dirA, workDir: dirA}
	restarted.loadUnsent()
	if len(restarted.unsent["kv.py"]) == 0 {
		t.Fatal("queued ops not restored after restart")
	}

	writeFile(t, a, "def get():\n    pass\n\ndef put():\n    pass\n")
	eA.pushOpsForChange("kv.py")
	if len(eA.unsent) != 0 {
		t.Fatal("queue not drained after a successful push")
	}
	if len(delivered) != 1 {
		t.Fatalf("delivered %d batches, want 1", len(delivered))
	}

	eB, dirB := newCRDTEngine(t, "B")
	b := filepath.Join(dirB, "kv.py")
	for _, batch := range delivered {
		eB.applyOps("kv.py", b, batch, "A")
	}
	want, _ := os.ReadFile(a)
	if got, _ := os.ReadFile(b); string(got) != string(want) {
		t.Fatalf("peer = %q, want %q (the lost genesis was never resent)", got, want)
	}
	_ = crdt.MarshalOps
}
