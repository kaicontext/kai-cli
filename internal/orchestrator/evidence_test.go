package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-core/cas"
	"kai/internal/planner"
)

// TestRenderEvidencePreamble_BasicShape pins the load-bearing format:
// header → per-entry stanza (location, annotation, quoted excerpt) →
// closer. Executors parse this visually; format stability matters
// more than the exact wording.
func TestRenderEvidencePreamble_BasicShape(t *testing.T) {
	dir := t.TempDir()
	writeEvidenceFile(t, dir, "foo.go", "line 1\nline 2\nline 3\n")
	hash := cas.Blake3HashHex([]byte("line 1\nline 2\nline 3\n"))

	out := renderEvidencePreamble([]planner.EvidenceEntry{
		{
			File:        "foo.go",
			LineStart:   2,
			LineEnd:     2,
			Excerpt:     "line 2",
			Annotation:  "this line is the actual bug location",
			ContentHash: hash,
		},
	}, dir)

	for _, want := range []string{
		"EVIDENCE FROM PLANNING",
		"foo.go:2-2",
		"this line is the actual bug location",
		"| line 2",
		"end evidence",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preamble missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRenderEvidencePreamble_DriftDegrades verifies that when the
// cited file has changed since the planner captured its hash, the
// excerpt is replaced with a "stale" notice pointing the executor
// at the file to re-read. Critical safety: a multi-agent plan where
// agent A edits a file agent B cited would pass wrong line numbers
// to B without this.
func TestRenderEvidencePreamble_DriftDegrades(t *testing.T) {
	dir := t.TempDir()
	originalContent := "import x\nx.foo()\n"
	writeEvidenceFile(t, dir, "drifted.go", originalContent)
	originalHash := cas.Blake3HashHex([]byte(originalContent))
	// Now mutate the file post-planning.
	writeEvidenceFile(t, dir, "drifted.go", "import x\nx.bar()\n")

	out := renderEvidencePreamble([]planner.EvidenceEntry{
		{
			File:        "drifted.go",
			LineStart:   2,
			LineEnd:     2,
			Excerpt:     "x.foo()",
			Annotation:  "wrong method being called here",
			ContentHash: originalHash,
		},
	}, dir)

	if !strings.Contains(out, "evidence stale") {
		t.Errorf("expected drift notice; got:\n%s", out)
	}
	if strings.Contains(out, "x.foo()") {
		t.Errorf("stale excerpt should be replaced with notice, not passed through; got:\n%s", out)
	}
	if !strings.Contains(out, "re-read drifted.go") {
		t.Errorf("expected re-read instruction; got:\n%s", out)
	}
}

// TestRenderEvidencePreamble_MissingHashPassesThrough — entries
// without a recorded hash (planner couldn't compute one) skip drift
// detection and pass through. Better to surface the excerpt than
// suppress evidence over a missing optional field.
func TestRenderEvidencePreamble_MissingHashPassesThrough(t *testing.T) {
	dir := t.TempDir()
	out := renderEvidencePreamble([]planner.EvidenceEntry{
		{
			File:       "missing-hash.go",
			LineStart:  1,
			LineEnd:    1,
			Excerpt:    "important content",
			Annotation: "the planner cited this",
			// ContentHash intentionally empty
		},
	}, dir)

	if !strings.Contains(out, "important content") {
		t.Errorf("expected excerpt to pass through, got:\n%s", out)
	}
	if strings.Contains(out, "stale") {
		t.Errorf("missing hash should not trigger drift notice, got:\n%s", out)
	}
}

// TestRenderEvidencePreamble_BlockCap pins the byte budget. The
// executor's prompt should not balloon when the planner over-emits;
// entries past the cap drop with a trailer noting the omission.
func TestRenderEvidencePreamble_BlockCap(t *testing.T) {
	dir := t.TempDir()
	bigExcerpt := strings.Repeat("a", planner.EvidencePerEntryMaxBytes)
	var entries []planner.EvidenceEntry
	for i := 0; i < 20; i++ {
		entries = append(entries, planner.EvidenceEntry{
			File:       "file.go",
			LineStart:  i + 1,
			LineEnd:    i + 1,
			Excerpt:    bigExcerpt,
			Annotation: "see this",
		})
	}
	out := renderEvidencePreamble(entries, dir)
	if len(out) > planner.EvidenceBlockMaxBytes+200 { // small slack for headers
		t.Errorf("preamble exceeds cap: %d bytes (cap %d)", len(out), planner.EvidenceBlockMaxBytes)
	}
	if !strings.Contains(out, "more entr") || !strings.Contains(out, "omitted") {
		t.Errorf("expected truncation notice when entries are dropped, got:\n%s", out)
	}
}

// TestRenderEvidencePreamble_EmptyReturnsEmpty — no entries → no
// preamble. Lets the orchestrator unconditionally prepend without
// emitting "EVIDENCE FROM PLANNING" + empty block on every spawn.
func TestRenderEvidencePreamble_EmptyReturnsEmpty(t *testing.T) {
	if got := renderEvidencePreamble(nil, t.TempDir()); got != "" {
		t.Errorf("nil entries should produce empty preamble, got: %q", got)
	}
	if got := renderEvidencePreamble([]planner.EvidenceEntry{}, t.TempDir()); got != "" {
		t.Errorf("empty entries should produce empty preamble, got: %q", got)
	}
}

func writeEvidenceFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeEvidenceFile: %v", err)
	}
}
