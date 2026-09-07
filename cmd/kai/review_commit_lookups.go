package main

// review_commit_lookups.go — answering the reviewer's lookups before it asks.
//
// A review turn costs a full model round-trip. On kai-desktop#286 that was
// between forty seconds and five minutes each, and the reviewer spent eight of
// them on sixteen reads: eleven kai_greps and five kai_views, every one of them
// against a single 2,917-line file. It never reached a conclusion and the run
// failed on its time budget with nothing to show.
//
// None of those greps were wasted work and none were answerable from the graph.
// Five of the six names it hunted — lastReviewsJSON, lastFindingsJSON,
// lastModelJSON, dvClosed, diffDOM — are object properties and closure-scoped
// variables, which the indexer does not record; only setChat existed as a
// symbol. The model was not ignoring the instruction to use kai_callers. There
// was nothing there to call.
//
// So the fix is not to send it to the graph. It is that ripgrep answers those
// eleven questions in milliseconds, and a script can ask them before the first
// token is generated. rcChangedSymbols already does this for DECLARATIONS
// (func/type/class/function) after a run burned its whole budget looking for
// SendUsageWarning, a name sitting in the diff it had been handed. This is the
// same lesson one level down: an identifier the diff READS or ASSIGNS is just
// as much a lookup the reviewer should never spend a turn on.
//
// The bound matters more than the coverage. Seeding is only a win while it is
// smaller than what it replaces, so an identifier that appears everywhere is
// dropped rather than summarised — "err appears 1,400 times" tells a reviewer
// nothing it did not already assume.

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

const (
	// rcLookupMaxIdents bounds how many identifiers are SHOWN, applied after
	// ranking by specificity. Past this the block stops being a shortcut and
	// becomes something to read.
	rcLookupMaxIdents = 20
	// rcLookupMaxCandidates bounds how many go into the one search — enough
	// that specificity does the choosing, few enough to stay one fast call.
	rcLookupMaxCandidates = 150
	// rcLookupMaxHits is the most occurrences shown for one identifier. Two
	// or three sites is the shape of a real lookup; a longer list is a
	// reading exercise the reviewer can do itself if it cares.
	rcLookupMaxHits = 6
	// rcLookupTooCommon drops identifiers that are everywhere. These carry
	// no information and would crowd out the ones that do.
	rcLookupTooCommon = 25
	// rcLookupMaxBytes is the hard ceiling on the whole block.
	rcLookupMaxBytes = 4096
)

// rcIdentRe matches candidate identifiers: four characters or more, so `err`,
// `ctx`, `id` and their kind never enter the running.
var rcIdentRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)

// rcLookupStop are words that appear in changed lines constantly and resolve
// to nothing useful — language keywords, the types every file names, and the
// vocabulary of test scaffolding.
var rcLookupStop = map[string]bool{
	"func": true, "type": true, "struct": true, "interface": true, "import": true,
	"package": true, "return": true, "range": true, "const": true, "true": true,
	"false": true, "nil": true, "error": true, "string": true, "bool": true,
	"byte": true, "rune": true, "int32": true, "int64": true, "uint8": true,
	"float64": true, "make": true, "append": true, "len": true, "cap": true,
	"defer": true, "case": true, "switch": true, "default": true, "break": true,
	"continue": true, "else": true, "context": true, "Context": true,
	"function": true, "const_": true, "class": true, "extends": true,
	"export": true, "await": true, "async": true, "this": true, "null": true,
	"undefined": true, "document": true, "window": true, "console": true,
	"test": true, "Test": true, "testing": true, "assert": true, "expect": true,
	"JSON": true, "Math": true, "Object": true, "Array": true, "Promise": true,
	"value": true, "values": true, "result": true, "data": true, "item": true,
	"name": true, "path": true, "file": true, "line": true, "text": true,
}

// rcIdentifierLookups resolves the identifiers a diff reads or assigns to the
// places they live, so the reviewer starts holding the answers instead of
// spending a turn each on the questions.
//
// `skip` is the set already covered by CHANGED SYMBOLS: those are new in this
// change, the reviewer is told to open their files directly, and resolving
// them again would contradict that instruction with a list of one.
//
// Best effort throughout. No repo, no git, no matches — the review proceeds
// exactly as it does today, one turn poorer.
func rcIdentifierLookups(diff, root string, skip map[string]bool) string {
	cands := rcLookupCandidates(diff, skip)
	if len(cands) == 0 {
		return ""
	}
	hits := rcGrepIdentifiers(cands, root)
	if len(hits) == 0 {
		return ""
	}
	// Rank by SPECIFICITY, not by how often the diff says the name.
	//
	// Frequency-in-diff reads like a proxy for importance and is not one. On
	// kai-desktop#286 it spent the budget on `prior` (24 sites across the
	// repo) and `caret` (7) while dropping `diffDOM`, `lastReviewsJSON` and
	// `lastFindingsJSON` — the three the reviewer actually went looking for.
	// A name that resolves to two or three places is a lookup worth
	// answering; one that resolves to twenty is a word.
	ranked := make([]string, 0, len(cands))
	for _, id := range cands {
		if n := len(hits[id]); n > 0 && n <= rcLookupTooCommon {
			ranked = append(ranked, id)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if len(hits[ranked[i]]) != len(hits[ranked[j]]) {
			return len(hits[ranked[i]]) < len(hits[ranked[j]])
		}
		return ranked[i] < ranked[j]
	})
	if len(ranked) > rcLookupMaxIdents {
		ranked = ranked[:rcLookupMaxIdents]
	}
	var b strings.Builder
	for _, id := range ranked {
		locs := hits[id]
		shown := locs
		more := 0
		if len(shown) > rcLookupMaxHits {
			more = len(shown) - rcLookupMaxHits
			shown = shown[:rcLookupMaxHits]
		}
		line := fmt.Sprintf("- %s: %s", id, strings.Join(shown, ", "))
		if more > 0 {
			line += fmt.Sprintf(" (+%d more)", more)
		}
		if b.Len()+len(line)+1 > rcLookupMaxBytes {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// rcLookupCandidates pulls the identifiers worth resolving out of a diff's
// changed lines, most-mentioned first — a name the change touches repeatedly
// is likelier to be the one the reviewer needs to understand.
func rcLookupCandidates(diff string, skip map[string]bool) []string {
	freq := map[string]int{}
	for _, line := range strings.Split(diff, "\n") {
		if len(line) < 2 || (line[0] != '+' && line[0] != '-') {
			continue
		}
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		body := line[1:]
		for _, loc := range rcIdentRe.FindAllStringIndex(body, -1) {
			m := body[loc[0]:loc[1]]
			// A token preceded by a dot is a property access — `this.reviews`,
			// `state.pending` — and is code whatever its case. Without this the
			// shape filter drops exactly the state that the graph also cannot
			// see, which is the intersection this file exists to cover.
			prop := loc[0] > 0 && body[loc[0]-1] == '.'
			if rcLookupStop[m] || (skip != nil && skip[m]) {
				continue
			}
			if !prop && !rcLooksLikeCode(m) {
				continue
			}
			freq[m]++
		}
	}
	out := make([]string, 0, len(freq))
	for id := range freq {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if freq[out[i]] != freq[out[j]] {
			return freq[out[i]] > freq[out[j]]
		}
		return out[i] < out[j] // stable for a stable prompt, so caching holds
	})
	// Deliberately NOT capped here. The cut belongs after the search, when
	// occurrence counts are known — capping on diff frequency first is what
	// dropped the identifiers this whole file exists to resolve.
	if len(out) > rcLookupMaxCandidates {
		out = out[:rcLookupMaxCandidates]
	}
	return out
}

// rcGrepIdentifiers finds where each identifier occurs, in ONE git grep rather
// than one per name: twenty subprocesses is its own kind of slow, and the
// whole point of this file is to be cheaper than the turns it removes.
//
// git grep cannot say which -e matched a line, so attribution is a substring
// check afterwards — cheap, and exact for word-boundary matches.
func rcGrepIdentifiers(idents []string, root string) map[string][]string {
	if len(idents) == 0 {
		return nil
	}
	args := []string{"grep", "-n", "--fixed-strings", "--no-color"}
	for _, id := range idents {
		args = append(args, "-e", id)
	}
	cmd := exec.Command("git", args...)
	if root != "" {
		cmd.Dir = root
	}
	// Exit 1 simply means no matches; only stdout is read either way.
	out, _ := cmd.Output()
	if len(out) == 0 {
		return nil
	}
	hits := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		// path:line:content
		first := strings.Index(line, ":")
		if first < 0 {
			continue
		}
		second := strings.Index(line[first+1:], ":")
		if second < 0 {
			continue
		}
		path := line[:first]
		lno := line[first+1 : first+1+second]
		body := line[first+second+2:]
		for _, id := range idents {
			if strings.Contains(body, id) {
				hits[id] = append(hits[id], path+":"+lno)
			}
		}
	}
	return hits
}

// rcLooksLikeCode keeps camelCase, snake_case and CONSTANT_CASE and rejects a
// bare lowercase word.
//
// Diffs carry prose — comments, doc strings, commit text — and a single
// lowercase English word is far likelier to be prose than an identifier worth
// resolving. Ranking by rarity made this acute rather than incidental: rare is
// exactly what a word like "pathological" or "enormous" is, so the specificity
// sort promoted comment vocabulary to the top of the block and pushed the real
// identifiers out. The shape of the token settles it before rarity gets a vote.
func rcLooksLikeCode(id string) bool {
	if strings.Contains(id, "_") {
		return true
	}
	for _, c := range id[1:] {
		if c >= 'A' && c <= 'Z' {
			return true // camelCase or CONSTANT_CASE
		}
	}
	return false
}
