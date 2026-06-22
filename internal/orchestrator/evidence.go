// Evidence preamble: render the planner's cited locations + annotations
// as an EVIDENCE FROM PLANNING block prepended to each spawned
// executor's prompt. Drift-checked against the spawn dir at render
// time — a cited file whose content has changed since the planner
// captured it gets a degraded-form note ("re-read X before acting")
// rather than a now-wrong line-numbered excerpt.
//
// 2026-05-26 spec #1: the planner spends 5-10 turns building a model
// of why a particular fix is the right one. Without forwarding that
// model, the executor falls back to training priors — picking the
// lower-friction interpretation (edit user config) over the right
// one (patch a library internal). The Evidence block is the bridge.
package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kaicontext/kai-core/cas"
	"kai/internal/planner"
)

// renderEvidencePreamble formats Evidence[] into the EVIDENCE FROM
// PLANNING block. Returns "" when there's nothing to render
// (no entries or all entries dropped after drift check). Caller
// prepends to the task prompt.
//
// spawnDir is the executor's working directory (a CoW clone of
// the integrate-time snapshot). Hash checks run against files in
// spawnDir because that's what the executor will see when it
// re-reads.
//
// Bounded at planner.EvidenceBlockMaxBytes total; entries past
// the cap are dropped with a "...N more entries omitted (cap)"
// trailer so the executor knows it's seeing a slice.
func renderEvidencePreamble(entries []planner.EvidenceEntry, spawnDir string) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("=== EVIDENCE FROM PLANNING ===\n")
	b.WriteString("The planner cited these locations as the basis for the task below. ")
	b.WriteString("Treat them as PRIOR (this is what the planner verified), not as a transcript replacement — do your own reads as needed, but don't re-derive the diagnosis when the planner already did it.\n\n")

	emitted := 0
	truncated := 0
	for i, e := range entries {
		entry := formatEvidenceEntry(e, spawnDir)
		// Stop if adding this entry would exceed the block cap.
		// Account for the closing separator line we always write.
		const closeOverhead = len("=== end evidence ===\n")
		if b.Len()+len(entry)+closeOverhead > planner.EvidenceBlockMaxBytes {
			truncated = len(entries) - i
			break
		}
		b.WriteString(entry)
		emitted++
	}
	if emitted == 0 {
		return ""
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "...%d more entr%s omitted (block cap reached)\n",
			truncated, plural(truncated, "y", "ies"))
	}
	b.WriteString("=== end evidence ===")
	return b.String()
}

// formatEvidenceEntry renders one entry as a 3-4 line stanza:
//
//	• <file>:<lineStart>-<lineEnd>
//	  <annotation>
//	  | <excerpt line 1>
//	  | <excerpt line 2>
//
// When drift is detected, the excerpt is replaced with a
// degraded-form note pointing the executor at the citation but
// telling them to re-read.
func formatEvidenceEntry(e planner.EvidenceEntry, spawnDir string) string {
	excerpt := strings.TrimSpace(e.Excerpt)
	if len(excerpt) > planner.EvidencePerEntryMaxBytes {
		excerpt = excerpt[:planner.EvidencePerEntryMaxBytes-1] + "…"
	}

	drifted := evidenceDrifted(e, spawnDir)
	var b strings.Builder
	if e.LineStart > 0 && e.LineEnd >= e.LineStart {
		fmt.Fprintf(&b, "• %s:%d-%d\n", e.File, e.LineStart, e.LineEnd)
	} else {
		fmt.Fprintf(&b, "• %s\n", e.File)
	}
	if ann := strings.TrimSpace(e.Annotation); ann != "" {
		fmt.Fprintf(&b, "  %s\n", ann)
	}
	if drifted {
		fmt.Fprintf(&b, "  [evidence stale — file changed since planning; re-read %s around the cited range before acting]\n", e.File)
	} else if excerpt != "" {
		for _, line := range strings.Split(excerpt, "\n") {
			fmt.Fprintf(&b, "  | %s\n", line)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// evidenceDrifted reports whether the cited file's current content
// hash differs from the planner's recorded hash. Returns false
// (treat as fresh) when:
//   - No recorded hash on the entry (planner couldn't compute it).
//   - The file can't be read (permissions, gone). Drift signal is
//     more honest than fabricating a "stale" warning for a missing
//     file — the executor will fail on its own re-read.
//   - The spawn dir is empty (test scaffolds with bare entries).
func evidenceDrifted(e planner.EvidenceEntry, spawnDir string) bool {
	if e.ContentHash == "" || spawnDir == "" {
		return false
	}
	path := e.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(spawnDir, e.File)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return cas.Blake3HashHex(content) != e.ContentHash
}

func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
