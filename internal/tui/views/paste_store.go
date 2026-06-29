package views

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Large-paste handling.
//
// Pasting a big block of text into the input (a stack trace, a file, a
// spec) buries the rest of the draft and — past the textarea's
// CharLimit — gets silently truncated. Instead, a paste over the
// thresholds below is diverted into the pasteStore and replaced in the
// input by a compact, human-readable placeholder:
//
//	[Pasted text #1 +234 lines]
//
// The placeholder is what the user sees and edits, what gets echoed to
// scrollback, and what lands in prompt history. The full content is
// re-expanded only when the turn is dispatched (see pasteStore.expand,
// called from submitLine), so the agent receives exactly what was
// pasted while the conversation stays readable.
//
// The store is session-scoped and in-memory: a paste only needs to
// survive until the turn that references it runs (and a little longer,
// so up-arrow recall within the session can re-expand it). Content is
// keyed by a monotonic id rather than a content hash — dedup isn't
// worth the bookkeeping for a single interactive input, and stable
// small ids keep the placeholder tokens short and legible.
// pasteCharThreshold diverts any paste of at least this many characters
// into the store, replacing it with a placeholder. A paste shorter than
// this (a URL, a path, a short snippet) stays inline so the user sees
// exactly what they pasted. 1000 mirrors roughly where Claude Code
// itself starts collapsing pasted text.
const pasteCharThreshold = 1000

// pastePlaceholderRe matches the tokens produced by placeholder(). The
// id capture is what expand() looks up; the "+N lines" part is display
// only. Anchored to the exact format we emit so stray bracketed text in
// a user's prompt won't be mistaken for a placeholder.
var pastePlaceholderRe = regexp.MustCompile(`\[Pasted text #(\d+) \+\d+ lines\]`)

// pasteEntry is one stored paste.
type pasteEntry struct {
	id      int
	content string
	lines   int
}

// pasteStore holds large pastes diverted out of the input. The zero
// value is not usable for storing (the map is nil); call newPasteStore
// or rely on lazy init in store(). Read paths (expand) tolerate a nil
// map and simply pass text through unchanged.
type pasteStore struct {
	entries map[int]pasteEntry
	nextID  int
}

func newPasteStore() pasteStore {
	return pasteStore{entries: map[int]pasteEntry{}}
}

// shouldStore reports whether a pasted chunk is large enough to divert.
// Measured in characters (runes), not bytes, so multibyte text isn't
// diverted early just for being non-ASCII.
func (ps *pasteStore) shouldStore(content string) bool {
	return utf8.RuneCountInString(content) >= pasteCharThreshold
}

// store records content under a fresh id and returns the entry. The
// caller turns the entry into a placeholder via placeholder().
func (ps *pasteStore) store(content string) pasteEntry {
	if ps.entries == nil {
		ps.entries = map[int]pasteEntry{}
	}
	ps.nextID++
	e := pasteEntry{id: ps.nextID, content: content, lines: countLines(content)}
	ps.entries[e.id] = e
	return e
}

// placeholder renders the compact token shown in place of the paste.
func (ps *pasteStore) placeholder(e pasteEntry) string {
	return fmt.Sprintf("[Pasted text #%d +%d lines]", e.id, e.lines)
}

// expand replaces every known placeholder token in s with its stored
// content. Unknown ids (e.g. a placeholder recalled from a prior
// session whose content this process never stored) are left verbatim
// rather than dropped, so nothing silently vanishes from the prompt.
func (ps *pasteStore) expand(s string) string {
	if ps == nil || len(ps.entries) == 0 || !strings.Contains(s, "[Pasted text #") {
		return s
	}
	return pastePlaceholderRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := pastePlaceholderRe.FindStringSubmatch(match)
		id, err := strconv.Atoi(sub[1])
		if err != nil {
			return match
		}
		if e, ok := ps.entries[id]; ok {
			return e.content
		}
		return match
	})
}

// countLines returns the number of visible lines in s, ignoring a
// single trailing newline so a paste of N lines reports N (not N+1).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}
