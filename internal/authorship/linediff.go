package authorship

import "bytes"

// LineRange represents a contiguous range of lines (1-indexed, inclusive).
type LineRange struct {
	Start int
	End   int
}

// DiffLineRanges returns the line ranges in `new` that differ from `old`.
// If old is nil (first time seeing the file), returns the entire file as one range.
// Returns 1-indexed, inclusive line numbers.
func DiffLineRanges(old, new []byte) []LineRange {
	if len(new) == 0 {
		return nil
	}

	newLines := bytes.Split(new, []byte("\n"))
	totalNew := len(newLines)

	// First time seeing the file — attribute the whole thing
	if old == nil {
		return []LineRange{{Start: 1, End: totalNew}}
	}

	oldLines := bytes.Split(old, []byte("\n"))
	totalOld := len(oldLines)

	// Identical — no changes
	if bytes.Equal(old, new) {
		return nil
	}

	// Find common prefix (matching lines from start)
	minLen := totalOld
	if totalNew < minLen {
		minLen = totalNew
	}

	prefix := 0
	for prefix < minLen && bytes.Equal(oldLines[prefix], newLines[prefix]) {
		prefix++
	}

	// Find common suffix (matching lines from end), but don't overlap with prefix
	suffix := 0
	for suffix < minLen-prefix &&
		bytes.Equal(oldLines[totalOld-1-suffix], newLines[totalNew-1-suffix]) {
		suffix++
	}

	// The changed region in the new file
	changeStart := prefix + 1         // 1-indexed
	changeEnd := totalNew - suffix     // 1-indexed, inclusive

	if changeStart > changeEnd {
		// Edge case: lines were deleted but nothing changed in new
		// (old was longer). No new lines to attribute.
		return nil
	}

	return []LineRange{{Start: changeStart, End: changeEnd}}
}
