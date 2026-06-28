package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
	"github.com/kaicontext/kai-engine/remote"
)

// newRemoteSyncTestDB opens a throwaway graph DB under t.TempDir().
func newRemoteSyncTestDB(t *testing.T) *graph.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := graph.Open(filepath.Join(dir, "db.sqlite"), filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatalf("graph.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeRemote serves the handful of endpoints pullTagsAndReviews touches: list
// refs, get a ref, get an object (Snapshot/Review), and review comments. It
// models a remote that has one release tag and one open review (exactly the
// F-15 scenario) so we can verify pull brings both across — without a network.
func fakeRemote(t *testing.T, tenant, repo string, tagTarget, reviewTarget []byte, reviewIDHex string) *httptest.Server {
	t.Helper()
	base := "/" + tenant + "/" + repo
	snapshotBody := "Snapshot\n" + `{"sourceType":"directory","fileDigests":[]}`
	reviewBody := "Review\n" + `{"_uuid":"` + reviewIDHex + `","title":"demo review","status":"open"}`

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == base+"/v1/refs":
			_ = json.NewEncoder(w).Encode(remote.RefsListResponse{Refs: []*remote.RefEntry{
				{Name: "snap.latest", Target: tagTarget},
				{Name: "tag.v1.0", Target: tagTarget},
				{Name: "review." + reviewIDHex, Target: reviewTarget},
				// A companion ref the push side emits alongside a real review: it
				// points at the review's target changeset, NOT a Review node. Pull
				// must skip it (only `review.<hex>` are real reviews). Note the
				// `/v1/refs/review.` handler below would resolve THIS back to the
				// real review, so an absent guard double-counts — which makes the
				// `want (1, 1)` assertions a regression test for the filter.
				{Name: "review." + reviewIDHex + ".target", Target: tagTarget},
			}})
		case strings.HasPrefix(p, base+"/v1/refs/review."):
			_ = json.NewEncoder(w).Encode(remote.RefEntry{Name: "review." + reviewIDHex, Target: reviewTarget})
		case strings.HasPrefix(p, base+"/v1/objects/"):
			id := p[strings.LastIndex(p, "/")+1:]
			switch id {
			case hex.EncodeToString(tagTarget):
				w.Header().Set("X-Kailab-Kind", "Snapshot")
				_, _ = w.Write([]byte(snapshotBody))
			case hex.EncodeToString(reviewTarget):
				w.Header().Set("X-Kailab-Kind", "Review")
				_, _ = w.Write([]byte(reviewBody))
			default:
				http.NotFound(w, r)
			}
		case strings.HasSuffix(p, "/comments"):
			_, _ = w.Write([]byte(`{"comments":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestPullTagsAndReviews_BringsUsableTagAndReview is the F-15 regression: pull
// must bring a release tag across as a USABLE local tag (`tag.v1.0`, what
// `kai tag list` reads — not merely a `remote/origin/tag.*` tracking ref) and
// reconstruct the Review node. The pre-fix behavior left tags as tracking refs
// only and never touched reviews on pull.
func TestPullTagsAndReviews_BringsUsableTagAndReview(t *testing.T) {
	tenant, repo := "t", "r"
	tagTarget := make([]byte, 32) // snapshot digest the tag points to
	for i := range tagTarget {
		tagTarget[i] = byte(i + 1)
	}
	reviewTarget := make([]byte, 16) // review UUID
	for i := range reviewTarget {
		reviewTarget[i] = byte(0xA0 + i)
	}
	reviewIDHex := hex.EncodeToString(reviewTarget)

	srv := fakeRemote(t, tenant, repo, tagTarget, reviewTarget, reviewIDHex)
	defer srv.Close()

	db := newRemoteSyncTestDB(t)
	client := remote.NewClient(srv.URL, tenant, repo)
	refMgr := ref.NewRefManager(db)

	tags, reviews := pullTagsAndReviews(db, client, refMgr, "origin")
	if tags != 1 || reviews != 1 {
		t.Fatalf("first pull = (%d tags, %d reviews), want (1, 1)", tags, reviews)
	}

	// The `review.<hex>.target` companion ref must have been skipped, not synced
	// as a second review — only `review.<hex>` names a Review node.
	if r, _ := refMgr.Get("review." + reviewIDHex + ".target"); r != nil {
		t.Error("companion ref review.<id>.target was synced as a review — the .target guard regressed")
	}

	// The tag must be a usable LOCAL ref pointing at the tagged snapshot — this
	// is the whole defect (it used to land only as remote/origin/tag.v1.0).
	localTag, _ := refMgr.Get("tag.v1.0")
	if localTag == nil {
		t.Fatal("local tag.v1.0 ref missing — `kai tag list` would still show nothing")
	}
	if hex.EncodeToString(localTag.TargetID) != hex.EncodeToString(tagTarget) {
		t.Errorf("tag.v1.0 -> %x, want %x", localTag.TargetID, tagTarget)
	}

	// The review node must have been reconstructed locally.
	if ok, _ := db.HasNode(reviewTarget); !ok {
		t.Error("review node was not reconstructed on pull")
	}
	if r, _ := refMgr.Get("review." + reviewIDHex); r == nil {
		t.Error("local review ref missing after pull")
	}
}

// TestPullTagsAndReviews_Idempotent guards the other half of F-15: re-pulling
// must not re-create tags or duplicate Review nodes. A second pull over the same
// remote state should report nothing new.
func TestPullTagsAndReviews_Idempotent(t *testing.T) {
	tenant, repo := "t", "r"
	tagTarget := make([]byte, 32)
	for i := range tagTarget {
		tagTarget[i] = byte(i + 5)
	}
	reviewTarget := make([]byte, 16)
	for i := range reviewTarget {
		reviewTarget[i] = byte(0xB0 + i)
	}
	reviewIDHex := hex.EncodeToString(reviewTarget)

	srv := fakeRemote(t, tenant, repo, tagTarget, reviewTarget, reviewIDHex)
	defer srv.Close()

	db := newRemoteSyncTestDB(t)
	client := remote.NewClient(srv.URL, tenant, repo)
	refMgr := ref.NewRefManager(db)

	if tags, reviews := pullTagsAndReviews(db, client, refMgr, "origin"); tags != 1 || reviews != 1 {
		t.Fatalf("first pull = (%d, %d), want (1, 1)", tags, reviews)
	}
	if tags, reviews := pullTagsAndReviews(db, client, refMgr, "origin"); tags != 0 || reviews != 0 {
		t.Fatalf("second pull = (%d, %d), want (0, 0) — re-pull duplicated a tag or review", tags, reviews)
	}
}
