package main

// review_commit_status.go — what became of each point the previous review made.
//
// A re-review already starts from the previous one (KAI_PRIOR_REVIEW) and
// re-raises a point that still holds with "(earlier: <ref>)". A point it
// dropped was silent: fixed, wrong, or simply not recalled this time — and
// recall varies a lot between runs (the same diff gave 1 finding in one run
// and 3 in another). Nothing told the author which, and the old inline
// comments stayed open.
//
// So the reviewer accounts for every earlier ref under EARLIER, and the
// bundle carries one status per ref that kai-server acts on: "fixed" and
// "withdrawn" close the point's GitHub thread with a reply saying why, "open"
// keeps it, "unknown" leaves it alone. The status is the reviewer's claim
// checked against what the run actually published:
//
//   - re-raised in the published review (its earlier-tag survived): open,
//     whatever EARLIER says;
//   - re-raised and refuted by the fact-check: withdrawn;
//   - "fixed at path:line": fixed — unless that file has not changed since
//     the previous review, when the claim cannot be right: unknown;
//   - "wrong": withdrawn;
//   - "open" but not published (the fact-check could not settle it): open;
//   - not accounted for: unknown. Silence is never "fixed".

import (
	"path"
	"regexp"
	"strings"
)

// rcEarlierStatus is one earlier point's fate, as the bundle carries it.
type rcEarlierStatus struct {
	Ref    string `json:"ref"`
	Status string `json:"status"` // open | fixed | withdrawn | unknown
	Where  string `json:"where,omitempty"`
	Note   string `json:"note,omitempty"`
}

const (
	rcEarlierOpen      = "open"
	rcEarlierFixed     = "fixed"
	rcEarlierWithdrawn = "withdrawn"
	rcEarlierUnknown   = "unknown"
)

// rcEarlierClaim is what the reviewer wrote for one ref under EARLIER.
type rcEarlierClaim struct {
	status, where, note string
}

var (
	rcEarlierLineRe  = regexp.MustCompile(`^-\s*` + "`?" + `([0-9a-f]{12})` + "`?" + `\s*:\s*(.*)$`)
	rcEarlierWhereRe = regexp.MustCompile("(?i)^fixed\\s+at\\s+`?([^\\s`]+:\\d+)`?")
	rcEarlierNoteRe  = regexp.MustCompile(`^\s*(?:—|–|-|:)\s*`)
)

// rcParseEarlierSection reads the EARLIER list from a review's coda.
func rcParseEarlierSection(raw string) map[string]rcEarlierClaim {
	i := strings.Index(raw, rcReviewDataMarker)
	if i < 0 {
		return nil
	}
	out := map[string]rcEarlierClaim{}
	in := false
	for _, line := range strings.Split(raw[i+len(rcReviewDataMarker):], "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "-") {
			// Only a coda header moves in or out of the list; any other
			// line inside it (a wrapped note, "the guard: now …") is part of
			// it and must not end it (Kai's review of kai-cli#182).
			if key, ok := rcCodaHeader(t); ok {
				in = key == "earlier"
			}
			continue
		}
		ref, c, ok := rcParseEarlierLine(t)
		if !ok || (!in && !rcIsEarlierStatusLine(t)) {
			continue
		}
		if _, seen := out[ref]; !seen {
			out[ref] = c
		}
	}
	return out
}

// rcIsEarlierStatusLine reports a bullet that is an earlier point's status
// ("- <12-hex ref>: fixed|open|wrong …") wherever it was written. On
// kai-cli#182 (2026-10-14) a reviewer put one under ISSUES and it was posted
// to the PR as a new 🐞 point; one found outside EARLIER is read as a status
// and taken out of the draft like the list itself.
func rcIsEarlierStatusLine(t string) bool {
	m := rcEarlierLineRe.FindStringSubmatch(t)
	if m == nil {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(m[2]))
	return strings.HasPrefix(low, "fixed") || strings.HasPrefix(low, "open") || strings.HasPrefix(low, "wrong")
}

// rcParseEarlierLine reads one "- <ref>: open | fixed at path:line — … |
// wrong — …" bullet.
func rcParseEarlierLine(t string) (string, rcEarlierClaim, bool) {
	m := rcEarlierLineRe.FindStringSubmatch(t)
	if m == nil {
		return "", rcEarlierClaim{}, false
	}
	ref, rest := m[1], strings.TrimSpace(m[2])
	low := strings.ToLower(rest)
	var c rcEarlierClaim
	switch {
	case strings.HasPrefix(low, "fixed"):
		c.status = rcEarlierFixed
		if w := rcEarlierWhereRe.FindStringSubmatch(rest); w != nil {
			c.where = w[1]
			rest = rest[len(w[0]):]
		} else {
			rest = rest[len("fixed"):]
		}
	case strings.HasPrefix(low, "wrong"):
		c.status = rcEarlierWithdrawn
		rest = rest[len("wrong"):]
	case strings.HasPrefix(low, "open"):
		c.status = rcEarlierOpen
		rest = rest[len("open"):]
	default:
		return "", rcEarlierClaim{}, false
	}
	c.note = strings.TrimSpace(rcEarlierNoteRe.ReplaceAllString(rest, ""))
	return ref, c, true
}

// rcTakeEarlierSection reads the EARLIER list and removes it from the draft.
// Every later pass that walks the coda tracks its own sections by header —
// the stakeless-decision filter, the speculation filter, the sweep merge —
// and one that did not know EARLIER read its bullets as part of the list
// before it: after DECISIONS they were dropped as decisions with no stake,
// with a false "N decisions dropped" (Kai's review of kai-cli#182). Taking
// the section out right after reading it means none of them can see it.
func rcTakeEarlierSection(raw string) (string, map[string]rcEarlierClaim) {
	claims := rcParseEarlierSection(raw)
	i := strings.Index(raw, rcReviewDataMarker)
	if i < 0 {
		return raw, claims
	}
	head, coda := raw[:i+len(rcReviewDataMarker)], raw[i+len(rcReviewDataMarker):]
	lines := strings.Split(coda, "\n")
	out := lines[:0]
	in := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if key, ok := rcCodaHeader(t); ok {
			in = key == "earlier"
			if in {
				continue
			}
		}
		if in {
			continue // a bullet, a continuation or a blank inside EARLIER
		}
		if strings.HasPrefix(t, "-") && rcIsEarlierStatusLine(t) {
			continue // a status written under ISSUES or DECISIONS
		}
		out = append(out, ln)
	}
	return head + strings.Join(out, "\n"), claims
}

// rcCodaHeaders are the coda's own section headers — the ones
// rcParseReviewOutput switches on — plus EARLIER.
var rcCodaHeaders = map[string]bool{"issues": true, "findings": true, "decisions": true, "intent_match": true, "merge_ready": true, "summary": true, "note": true, "earlier": true}

// rcCodaHeader reports whether a line is one of the coda's headers, and which.
// A line with a colon in it is not a header unless its label is one of them.
//
// "Note" and "Summary" are also ordinary words a wrapped note can start with
// ("Note: the caller checks it"), so those two count as headers only in the
// capitals the coda template writes them in. The others are headers in any
// case: mistaking a real "issues:" header for a continuation would swallow
// findings into the EARLIER list.
func rcCodaHeader(t string) (string, bool) {
	if t == "" || strings.HasPrefix(t, "-") {
		return "", false
	}
	key, _, labelled := rcMachineLine(t)
	if !labelled || !rcCodaHeaders[key] {
		return "", false
	}
	if key == "note" || key == "summary" {
		label := strings.Trim(strings.TrimSpace(t[:strings.Index(t, ":")]), "#*` ")
		if label != strings.ToUpper(label) {
			return "", false
		}
	}
	return key, true
}

// rcEarlierStatuses decides each earlier ref's status from the reviewer's
// claims and what was published (see the file comment). final is the
// published review with its tags back on; tags maps a bullet to the tags it
// carried before the challenge (rcTakeEarlierTags); changed and known are the
// files changed since the previous review (rcChangedSince).
func rcEarlierStatuses(prior *rcPrior, claims map[string]rcEarlierClaim, final string, res *rcChallengeResult, tags map[string]string, changed []string, known bool) []rcEarlierStatus {
	if prior == nil {
		return nil
	}
	refuted := map[string]string{}
	if res != nil {
		for _, a := range res.Allegations {
			// A confirmed defect the selection step left unpublished is not a
			// withdrawal: the code still has it (Kai's review of kai-cli#188).
			if a.Status != rcStatusRefuted || strings.HasPrefix(a.Reason, rcSelectionSkipPrefix) {
				continue
			}
			key, _, ok := rcEarlierBulletKey("issues", "- "+a.Issue)
			if !ok {
				continue
			}
			for _, m := range rcEarlierRefRe.FindAllStringSubmatch(tags[key], -1) {
				refuted[m[1]] = a.Reason
			}
		}
	}
	reraised := map[string]bool{}
	for _, m := range rcEarlierRefRe.FindAllStringSubmatch(final, -1) {
		reraised[m[1]] = true
	}
	// The files changed since each point's own commit: the previous review's
	// (changed, known) for its points, worked out once per older commit for
	// the points an older review raised (rcPriorPoint.Since).
	type changedSince struct {
		set   map[string]bool
		known bool
	}
	asSet := func(files []string, known bool) changedSince {
		cs := changedSince{set: map[string]bool{}, known: known}
		for _, p := range files {
			cs.set[rcCleanRepoPath(p)] = true
		}
		return cs
	}
	bySince := map[string]changedSince{prior.Head: asSet(changed, known)}
	changedFor := func(pt rcPriorPoint) changedSince {
		since := rcPointSince(prior, pt)
		if cs, ok := bySince[since]; ok {
			return cs
		}
		cs := asSet(rcChangedSince(since))
		bySince[since] = cs
		return cs
	}
	var out []rcEarlierStatus
	seen := map[string]bool{}
	for _, pt := range prior.Points {
		ref := strings.TrimSpace(pt.Ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		c := claims[ref]
		st := rcEarlierStatus{Ref: ref}
		cs := changedFor(pt)
		switch {
		case reraised[ref]:
			st.Status = rcEarlierOpen
		case refuted[ref] != "":
			st.Status, st.Note = rcEarlierWithdrawn, "the fact-check found it no longer holds: "+rcClip(refuted[ref], 300)
		case c.status == rcEarlierFixed && c.where == "":
			st.Status, st.Note = rcEarlierUnknown, "said fixed without naming the line that fixes it"
		case c.status == rcEarlierFixed && !cs.known:
			// A fix that cannot be checked against the changed files does
			// not close the author's thread (Kai's review of kai-cli#182).
			st.Status, st.Note = rcEarlierUnknown, "said fixed at "+c.where+", but the commit the point was raised at is not in this clone, so the fix could not be checked"
		case c.status == rcEarlierFixed && !cs.set[rcCleanRepoPath(rcPathOf(c.where))]:
			st.Status, st.Note = rcEarlierUnknown, "said fixed at "+c.where+", but that file has not changed since the point was raised"
		case c.status == rcEarlierFixed:
			st.Status, st.Where, st.Note = rcEarlierFixed, c.where, c.note
		case c.status == rcEarlierWithdrawn:
			st.Status, st.Note = rcEarlierWithdrawn, c.note
		case c.status == rcEarlierOpen:
			st.Status, st.Note = rcEarlierOpen, c.note
		default:
			st.Status = rcEarlierUnknown
		}
		out = append(out, st)
	}
	return out
}

// rcEarlierRefRe reads the ref out of an earlier-tag.
var rcEarlierRefRe = regexp.MustCompile(`\(earlier:\s*([0-9a-f]{12})\)`)

// rcCleanRepoPath normalises a repo-relative path for comparison:
// "./a/b.go" and "a//b.go" are a/b.go.
func rcCleanRepoPath(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	return strings.TrimPrefix(path.Clean(p), "./")
}

// rcPathOf is the path of "path:line".
func rcPathOf(where string) string {
	if i := strings.LastIndex(where, ":"); i > 0 {
		return where[:i]
	}
	return where
}
