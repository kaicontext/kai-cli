package safetygate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/util"
)

// fakeGraph is an in-memory Grapher: a path → callers map for inbound
// IMPORTS/CALLS edges, plus a node store keyed by hex-encoded path bytes.
type fakeGraph struct {
	// callers["foo.go"] = ["bar.go", "baz.go"] means bar.go and baz.go
	// have an inbound edge into foo.go (i.e. they call/import it).
	callers map[string][]string
}

func newFakeGraph(callers map[string][]string) *fakeGraph {
	return &fakeGraph{callers: callers}
}

func (f *fakeGraph) GetEdgesToByPath(p string, _ graph.EdgeType) ([]*graph.Edge, error) {
	srcs := f.callers[p]
	out := make([]*graph.Edge, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, &graph.Edge{Src: []byte(s)})
	}
	return out, nil
}

func (f *fakeGraph) GetNode(id []byte) (*graph.Node, error) {
	return &graph.Node{
		ID:      id,
		Kind:    graph.KindFile,
		Payload: map[string]interface{}{"path": string(id)},
	}, nil
}

func TestClassify_NoChanges(t *testing.T) {
	d, err := Classify(context.Background(), nil, newFakeGraph(nil), DefaultConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Verdict != Auto || d.BlastRadius != 0 {
		t.Fatalf("expected Auto/0, got %+v", d)
	}
}

func TestClassify_ZeroBlastAuto(t *testing.T) {
	// foo.go has no callers — blast radius 0.
	g := newFakeGraph(map[string][]string{})
	d, err := Classify(context.Background(), []string{"foo.go"}, g, DefaultConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Verdict != Auto {
		t.Fatalf("expected Auto for zero blast, got %s (reasons=%v)", d.Verdict, d.Reasons)
	}
	if d.BlastRadius != 0 {
		t.Fatalf("expected radius 0, got %d", d.BlastRadius)
	}
}

func TestClassify_NonZeroBlastReview(t *testing.T) {
	// foo.go has 3 callers; with default cfg (auto=0, block=huge) → Review.
	g := newFakeGraph(map[string][]string{
		"foo.go": {"bar.go", "baz.go", "qux.go"},
	})
	d, err := Classify(context.Background(), []string{"foo.go"}, g, DefaultConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Verdict != Review {
		t.Fatalf("expected Review, got %s", d.Verdict)
	}
	if d.BlastRadius != 3 {
		t.Fatalf("expected radius 3, got %d", d.BlastRadius)
	}
	if !reflect.DeepEqual(d.Touches, []string{"bar.go", "baz.go", "qux.go"}) {
		t.Fatalf("unexpected touches: %v", d.Touches)
	}
}

func TestClassify_AutoThresholdBoundary(t *testing.T) {
	g := newFakeGraph(map[string][]string{
		"foo.go": {"a.go", "b.go", "c.go"},
	})
	cfg := Config{AutoThreshold: 3, BlockThreshold: 100}
	// radius (3) <= auto (3) → Auto. Boundary inclusive on the auto side.
	d, _ := Classify(context.Background(), []string{"foo.go"}, g, cfg)
	if d.Verdict != Auto {
		t.Fatalf("expected Auto at boundary, got %s", d.Verdict)
	}
	cfg.AutoThreshold = 2
	d, _ = Classify(context.Background(), []string{"foo.go"}, g, cfg)
	if d.Verdict != Review {
		t.Fatalf("expected Review just above auto, got %s", d.Verdict)
	}
}

func TestClassify_BlockThresholdBoundary(t *testing.T) {
	callers := map[string][]string{"foo.go": {}}
	for i := 0; i < 10; i++ {
		callers["foo.go"] = append(callers["foo.go"], string(rune('a'+i))+".go")
	}
	g := newFakeGraph(callers)
	cfg := Config{AutoThreshold: 0, BlockThreshold: 10}
	// radius (10) >= block (10) → Block. Boundary inclusive on the block side.
	d, _ := Classify(context.Background(), []string{"foo.go"}, g, cfg)
	if d.Verdict != Block {
		t.Fatalf("expected Block at boundary, got %s (radius=%d)", d.Verdict, d.BlastRadius)
	}
	cfg.BlockThreshold = 11
	d, _ = Classify(context.Background(), []string{"foo.go"}, g, cfg)
	if d.Verdict != Review {
		t.Fatalf("expected Review just below block, got %s", d.Verdict)
	}
}

func TestClassify_ProtectedPathBlocks(t *testing.T) {
	g := newFakeGraph(nil) // zero blast — protection must override
	cfg := Config{
		AutoThreshold:  100,
		BlockThreshold: 1000,
		Protected:      []string{"pkg/auth/**", "**/billing.go"},
	}

	cases := []struct {
		name string
		path string
		want Verdict
	}{
		{"under protected dir", "pkg/auth/login.go", Block},
		{"deep under protected dir", "pkg/auth/oauth/google.go", Block},
		{"matches double-star suffix", "internal/billing.go", Block},
		{"unrelated path", "pkg/api/handler.go", Auto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Classify(context.Background(), []string{tc.path}, g, cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Verdict != tc.want {
				t.Fatalf("path %q: want %s, got %s (reasons=%v)", tc.path, tc.want, d.Verdict, d.Reasons)
			}
			if tc.want == Block && len(d.Reasons) == 0 {
				t.Fatalf("Block verdict missing reasons")
			}
		})
	}
}

func TestClassify_ProtectedBeatsRadius(t *testing.T) {
	// Even when radius is well within auto, a protected hit still blocks.
	g := newFakeGraph(map[string][]string{})
	cfg := Config{
		AutoThreshold:  100,
		BlockThreshold: 1000,
		Protected:      []string{"pkg/auth/**"},
	}
	d, _ := Classify(context.Background(), []string{"pkg/auth/x.go"}, g, cfg)
	if d.Verdict != Block {
		t.Fatalf("expected protected path to block regardless of radius, got %s", d.Verdict)
	}
}

func TestClassify_MultiplePathsUniqueRadius(t *testing.T) {
	// Two changed files with overlapping callers → unique-counted.
	g := newFakeGraph(map[string][]string{
		"foo.go": {"x.go", "y.go"},
		"bar.go": {"y.go", "z.go"},
	})
	d, _ := Classify(context.Background(), []string{"foo.go", "bar.go"}, g, DefaultConfig())
	if d.BlastRadius != 3 {
		t.Fatalf("expected dedup radius 3, got %d (touches=%v)", d.BlastRadius, d.Touches)
	}
}

func TestClassify_ChangedPathExcludedFromRadius(t *testing.T) {
	// If foo.go changed AND bar.go changed, and bar.go calls foo.go,
	// bar.go is part of the change set itself — it shouldn't count
	// toward the blast radius of foo.go.
	g := newFakeGraph(map[string][]string{
		"foo.go": {"bar.go"},
	})
	d, _ := Classify(context.Background(), []string{"foo.go", "bar.go"}, g, DefaultConfig())
	if d.BlastRadius != 0 {
		t.Fatalf("expected radius 0 (caller is in change set), got %d (touches=%v)", d.BlastRadius, d.Touches)
	}
}

func TestLoadConfig_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("expected defaults, got %+v", cfg)
	}
}

func TestLoadConfig_ValidYAML(t *testing.T) {
	kaiDir := t.TempDir()
	yaml := []byte("auto_threshold: 5\nblock_threshold: 50\nprotected:\n  - pkg/auth/**\n")
	if err := os.WriteFile(filepath.Join(kaiDir, "gate.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(kaiDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoThreshold != 5 || cfg.BlockThreshold != 50 {
		t.Fatalf("unexpected thresholds: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Protected, []string{"pkg/auth/**"}) {
		t.Fatalf("unexpected protected: %v", cfg.Protected)
	}
}

func TestLoadConfig_MalformedYAMLErrors(t *testing.T) {
	kaiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(kaiDir, "gate.yaml"), []byte("not: : valid:"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(kaiDir); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestIsHeld_RejectsSelfTargetedSnapshot is the regression test for
// the multi-root empty-diff bug: a held snapshot whose targetSnapshot
// is its own ID is degenerate — diffed against itself it yields an
// empty diff, can never be approved, and otherwise sits in the gate
// forever. IsHeld must not surface it.
func TestIsHeld_RejectsSelfTargetedSnapshot(t *testing.T) {
	id := []byte{0xa0, 0x7c, 0xe7, 0xc9, 0x48, 0x3a}
	selfHex := util.BytesToHex(id)

	mk := func(target string) *graph.Node {
		return &graph.Node{
			ID:   id,
			Kind: graph.KindSnapshot,
			Payload: map[string]interface{}{
				"gateVerdict":    string(Review),
				"targetSnapshot": target,
			},
		}
	}

	if IsHeld(mk(selfHex)) {
		t.Error("a snapshot whose targetSnapshot equals its own ID must NOT be held")
	}
	// Case-insensitive: stored hex may differ in case.
	if IsHeld(mk(strings.ToUpper(selfHex))) {
		t.Error("self-target check must be case-insensitive")
	}
	// A proper distinct target IS held.
	if !IsHeld(mk("deadbeefdeadbeef")) {
		t.Error("a review snapshot with a distinct target should be held")
	}
	// No target at all → still held (older snapshots predate the field).
	if !IsHeld(mk("")) {
		t.Error("a review snapshot with no targetSnapshot should be held")
	}
}
