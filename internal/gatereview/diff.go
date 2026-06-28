// Package gatereview drives the AI-assisted review of safety-gate-held
// integrations. It builds a unified diff between the held snapshot and
// its target, asks the LLM for a summary + audit + recommendation, and
// returns a structured Result the CLI / TUI can render.
//
// Kept separate from internal/safetygate (which is a pure read of the
// graph) so the review path is allowed to touch the LLM and the file
// store without polluting the gate's classifier.
package gatereview

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/snapshot"
	"github.com/kaicontext/kai-engine/util"
)

// HeldSnapshotDiff returns the unified line-level diff between a held
// snapshot and its `targetSnapshot` base. Both snapshots live in the
// graph DB; the patch is built path-by-path from their stored file
// contents using diffmatchpatch (the same primitive `kai diff -p` uses).
//
// Output is uncolored — model input shouldn't carry ANSI noise, and
// callers that want color can post-process.
func HeldSnapshotDiff(db *graph.DB, snap *graph.Node) (string, error) {
	targetHex, _ := snap.Payload["targetSnapshot"].(string)
	if targetHex == "" {
		return "", fmt.Errorf("snapshot %s has no targetSnapshot in payload (was it produced by `kai integrate`?)",
			util.BytesToHex(snap.ID)[:12])
	}
	targetID, err := util.HexToBytes(targetHex)
	if err != nil {
		return "", fmt.Errorf("decoding target hex: %w", err)
	}

	creator := snapshot.NewCreator(db, nil)

	collect := func(id []byte) (map[string]string, map[string][]byte, error) {
		files, err := creator.GetSnapshotFiles(id)
		if err != nil {
			return nil, nil, err
		}
		digests := make(map[string]string)
		contents := make(map[string][]byte)
		for _, f := range files {
			path, _ := f.Payload["path"].(string)
			digest, _ := f.Payload["digest"].(string)
			digests[path] = digest
			c, _ := creator.GetFileContent(digest)
			contents[path] = c
		}
		return digests, contents, nil
	}

	baseDigests, baseContent, err := collect(targetID)
	if err != nil {
		return "", fmt.Errorf("loading base snapshot: %w", err)
	}
	headDigests, headContent, err := collect(snap.ID)
	if err != nil {
		return "", fmt.Errorf("loading held snapshot: %w", err)
	}

	var added, modified, deleted []string
	for p, hd := range headDigests {
		if bd, ok := baseDigests[p]; !ok {
			added = append(added, p)
		} else if hd != bd {
			modified = append(modified, p)
		}
	}
	for p := range baseDigests {
		if _, ok := headDigests[p]; !ok {
			deleted = append(deleted, p)
		}
	}

	// Emit modified files first, then added, then deleted. Modified
	// source is the most review-relevant; ordering it ahead of large
	// added files means the 24KB review cap can't starve it (a build
	// artifact landing in `added` once truncated the real fix out).
	var sb strings.Builder
	for _, p := range modified {
		fmt.Fprintf(&sb, "diff --kai a/%s b/%s\n--- a/%s\n+++ b/%s\n", p, p, p, p)
		appendFileBody(&sb, baseContent[p], headContent[p])
		sb.WriteString("\n")
	}
	for _, p := range added {
		fmt.Fprintf(&sb, "diff --kai a/%s b/%s\n--- /dev/null\n+++ b/%s\n", p, p, p)
		appendFileBody(&sb, nil, headContent[p])
		sb.WriteString("\n")
	}
	for _, p := range deleted {
		fmt.Fprintf(&sb, "diff --kai a/%s b/%s\n--- a/%s\n+++ /dev/null\n", p, p, p)
		appendFileBody(&sb, baseContent[p], nil)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// appendFileBody renders a line diff for text files, or a one-line
// summary for binary files. Build artifacts and other binaries carry
// no reviewable content, and rendering an 8MB blob byte-by-byte would
// blow past the review's patch budget and truncate real source out.
func appendFileBody(sb *strings.Builder, before, after []byte) {
	if isBinary(before) || isBinary(after) {
		size := len(after)
		if size == 0 {
			size = len(before)
		}
		fmt.Fprintf(sb, "Binary file (%s) — content not shown\n", humanSize(size))
		return
	}
	appendLineDiff(sb, string(before), string(after))
}

// isBinary reports whether content looks like a binary blob, using the
// standard heuristic: a NUL byte within the first 8KB.
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// humanSize formats a byte count for the binary-file summary line.
func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// diffContextLines is how many unchanged lines of context a hunk keeps
// on each side of a change — the git default.
const diffContextLines = 3

// dline is one line of a line-level diff with its operation.
type dline struct {
	op   diffmatchpatch.Operation
	text string
}

// appendLineDiff writes a proper unified diff — `@@` hunks with bounded
// context — for the change from before to after.
//
// It used to emit the WHOLE file: every unchanged line written out as
// ` ` context. A 15-line change in a 1,300-line file then produced a
// ~40KB "diff" that was 99% noise, blew the reviewer's 24KB patch cap,
// and got truncated mid-file — so the gate reviewer literally could not
// see the change it was reviewing (observed on the DeepSeek bbolt t4
// run: "the diff is truncated so the freepages() change cannot be
// reviewed"). Hunking keeps the diff proportional to the change.
func appendLineDiff(sb *strings.Builder, before, after string) {
	dmp := diffmatchpatch.New()
	chars1, chars2, lineArray := dmp.DiffLinesToChars(before, after)
	diffs := dmp.DiffMain(chars1, chars2, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

	// Flatten the segment diff into one entry per line.
	var lines []dline
	for _, d := range diffs {
		text := strings.TrimSuffix(d.Text, "\n")
		if text == "" && d.Text == "" {
			continue
		}
		for _, ln := range strings.Split(text, "\n") {
			lines = append(lines, dline{d.Type, ln})
		}
	}
	if len(lines) == 0 {
		return
	}

	// Indices of changed (non-equal) lines.
	var changed []int
	for i, l := range lines {
		if l.op != diffmatchpatch.DiffEqual {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return // identical content — nothing to show
	}

	// Group changed lines into hunks: merge two changes whose gap of
	// equal lines is small enough that their context would touch.
	type span struct{ first, last int }
	var hunks []span
	hs, he := changed[0], changed[0]
	for _, idx := range changed[1:] {
		if idx-he <= 2*diffContextLines+1 {
			he = idx
		} else {
			hunks = append(hunks, span{hs, he})
			hs, he = idx, idx
		}
	}
	hunks = append(hunks, span{hs, he})

	// Prefix counts: oldNo[i] / newNo[i] = lines consumed before index i.
	oldNo := make([]int, len(lines)+1)
	newNo := make([]int, len(lines)+1)
	o, n := 0, 0
	for i, l := range lines {
		oldNo[i], newNo[i] = o, n
		switch l.op {
		case diffmatchpatch.DiffEqual:
			o, n = o+1, n+1
		case diffmatchpatch.DiffDelete:
			o++
		case diffmatchpatch.DiffInsert:
			n++
		}
	}
	oldNo[len(lines)], newNo[len(lines)] = o, n

	for _, h := range hunks {
		s := h.first - diffContextLines
		if s < 0 {
			s = 0
		}
		e := h.last + diffContextLines
		if e > len(lines)-1 {
			e = len(lines) - 1
		}
		oldLen, newLen := 0, 0
		for i := s; i <= e; i++ {
			switch lines[i].op {
			case diffmatchpatch.DiffEqual:
				oldLen, newLen = oldLen+1, newLen+1
			case diffmatchpatch.DiffDelete:
				oldLen++
			case diffmatchpatch.DiffInsert:
				newLen++
			}
		}
		fmt.Fprintf(sb, "@@ -%d,%d +%d,%d @@\n", oldNo[s]+1, oldLen, newNo[s]+1, newLen)
		for i := s; i <= e; i++ {
			switch lines[i].op {
			case diffmatchpatch.DiffDelete:
				sb.WriteString("-")
			case diffmatchpatch.DiffInsert:
				sb.WriteString("+")
			default:
				sb.WriteString(" ")
			}
			sb.WriteString(lines[i].text)
			sb.WriteString("\n")
		}
	}
}
