package main

import (
	"strings"
	"testing"
)

const rcStatusDraft = "Re-review.\n" + rcReviewDataMarker + `
INTENT_MATCH: verified
MERGE_READY: 3
SUMMARY: One point still holds.
ISSUES:
- src/a.go:10 — still panics on a nil map (severity: medium)
DECISIONS:
- [limit] raises the cap to 100
EARLIER:
- aaaaaaaaaaaa: fixed at ` + "`src/b.go:22`" + ` — the guard now returns an error
- bbbbbbbbbbbb: wrong — the caller already validates the length
- cccccccccccc: open
- ` + "`dddddddddddd`" + `: fixed at src/c.go:5 - clamps the value
- not-a-ref: fixed at src/x.go:1
- eeeeeeeeeeee: maybe
`

func TestParseEarlierSection(t *testing.T) {
	got := rcParseEarlierSection(rcStatusDraft)
	want := map[string]rcEarlierClaim{
		"aaaaaaaaaaaa": {rcEarlierFixed, "src/b.go:22", "the guard now returns an error"},
		"bbbbbbbbbbbb": {rcEarlierWithdrawn, "", "the caller already validates the length"},
		"cccccccccccc": {rcEarlierOpen, "", ""},
		"dddddddddddd": {rcEarlierFixed, "src/c.go:5", "clamps the value"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d claims, want %d: %+v", len(got), len(want), got)
	}
	for ref, w := range want {
		if got[ref] != w {
			t.Errorf("%s = %+v, want %+v", ref, got[ref], w)
		}
	}
}

// EARLIER bullets come after DECISIONS; they must never be filed as findings.
func TestEarlierBulletsAreNotFindings(t *testing.T) {
	_, issues, decisions, _, _, _ := rcParseReviewOutput(rcStatusDraft)
	if len(issues) != 1 || len(decisions) != 1 {
		t.Fatalf("EARLIER bullets leaked into findings: issues=%q decisions=%q", issues, decisions)
	}
}

// Kai's review of kai-cli#182: with EARLIER after DECISIONS, the
// stakeless-decision filter read its bullets as decisions and dropped them
// with a false count. Taken out right after it is read, no later coda pass
// sees it, and the rest of the draft is unchanged.
func TestTakeEarlierSectionHidesItFromLaterPasses(t *testing.T) {
	stripped, claims := rcTakeEarlierSection(rcStatusDraft)
	if len(claims) != 4 {
		t.Fatalf("parsed %d claims, want 4", len(claims))
	}
	if strings.Contains(stripped, "EARLIER:") || strings.Contains(stripped, "aaaaaaaaaaaa") {
		t.Fatalf("EARLIER still in the draft:\n%s", stripped)
	}
	if _, n := rcDropStakelessDecisions(stripped); n != 0 {
		t.Errorf("the stakeless filter dropped %d decisions; only the [limit] one is left and it has a stake", n)
	}
	if _, n := rcDropStakelessDecisions(rcStatusDraft); n == 0 {
		t.Error("precondition: on the unstripped draft the filter mistakes EARLIER bullets for decisions")
	}
	_, issues, decisions, _, _, _ := rcParseReviewOutput(stripped)
	if len(issues) != 1 || len(decisions) != 1 || !strings.Contains(stripped, "MERGE_READY: 3") {
		t.Errorf("the rest of the draft changed: issues=%q decisions=%q", issues, decisions)
	}
}

// A wrapped note inside EARLIER ("the guard: …" on its own line) is part of
// the list: it must not end it, and it must leave the draft with it.
func TestEarlierContinuationLines(t *testing.T) {
	draft := "x\n" + rcReviewDataMarker + `
INTENT_MATCH: verified
MERGE_READY: 4
SUMMARY: fine
ISSUES:
- src/a.go:1 — still wrong
EARLIER:
- aaaaaaaaaaaa: fixed at src/b.go:22 — the guard
  now returns an error. Note: the caller checks it.
- bbbbbbbbbbbb: wrong — the length is validated first
`
	stripped, claims := rcTakeEarlierSection(draft)
	if claims["aaaaaaaaaaaa"].status != rcEarlierFixed || claims["bbbbbbbbbbbb"].status != rcEarlierWithdrawn {
		t.Fatalf("a continuation line ended the list: %+v", claims)
	}
	if strings.Contains(stripped, "caller checks it") || strings.Contains(stripped, "bbbbbbbbbbbb") {
		t.Fatalf("EARLIER content left in the draft:\n%s", stripped)
	}
	if _, _, _, _, _, note := rcParseReviewOutput(stripped); note != "fine" {
		t.Errorf("summary note = %q, want fine", note)
	}
}

// kai-cli#182's own review: the reviewer wrote a status under ISSUES and it
// was posted as a new 🐞. A status line anywhere is a status, never a finding.
func TestEarlierStatusUnderIssuesIsNotAFinding(t *testing.T) {
	draft := "x\n" + rcReviewDataMarker + `
INTENT_MATCH: verified
MERGE_READY: 4
SUMMARY: fine
ISSUES:
- ed7b1d9c88d9: fixed at cmd/kai/review_commit.go:1104 — the section is stripped first
- src/a.go:1 — a real defect
DECISIONS:
- cccccccccccc: open
`
	stripped, claims := rcTakeEarlierSection(draft)
	if claims["ed7b1d9c88d9"].status != rcEarlierFixed || claims["ed7b1d9c88d9"].where != "cmd/kai/review_commit.go:1104" || claims["cccccccccccc"].status != rcEarlierOpen {
		t.Fatalf("misplaced statuses not read: %+v", claims)
	}
	_, issues, decisions, _, _, _ := rcParseReviewOutput(stripped)
	if len(issues) != 1 || issues[0] != "src/a.go:1 — a real defect" || len(decisions) != 0 {
		t.Fatalf("a status survived as a finding: issues=%q decisions=%q", issues, decisions)
	}
}

// Kai's review of #182: a wrapped "Note: …" / "Summary: …" line inside
// EARLIER is a continuation, not a header; the capitalised header still is.
func TestEarlierNoteContinuationIsNotAHeader(t *testing.T) {
	draft := "x\n" + rcReviewDataMarker + `
INTENT_MATCH: verified
MERGE_READY: 4
EARLIER:
- aaaaaaaaaaaa: fixed at src/b.go:22 — the guard returns an error.
  Note: the caller checks it.
  Summary: covered by TestGuard.
- bbbbbbbbbbbb: wrong — validated first
SUMMARY: fine
`
	stripped, claims := rcTakeEarlierSection(draft)
	if claims["bbbbbbbbbbbb"].status != rcEarlierWithdrawn {
		t.Fatalf("a Note:/Summary: continuation ended EARLIER: %+v", claims)
	}
	if strings.Contains(stripped, "caller checks") || strings.Contains(stripped, "TestGuard") {
		t.Fatalf("continuation left in the draft:\n%s", stripped)
	}
	if _, _, _, _, _, note := rcParseReviewOutput(stripped); note != "fine" {
		t.Errorf("summary = %q, want fine", note)
	}
}

// Fix locations with no extension or with brackets, and "./" paths.
func TestEarlierFixLocations(t *testing.T) {
	draft := "x\n" + rcReviewDataMarker + "\nEARLIER:\n- aaaaaaaaaaaa: fixed at Makefile:3 — x\n- bbbbbbbbbbbb: fixed at `app/[id]/page.tsx:12` — y\n- cccccccccccc: fixed at ./src/c.go:5 — z\n"
	claims := rcParseEarlierSection(draft)
	for ref, want := range map[string]string{"aaaaaaaaaaaa": "Makefile:3", "bbbbbbbbbbbb": "app/[id]/page.tsx:12", "cccccccccccc": "./src/c.go:5"} {
		if claims[ref].where != want {
			t.Errorf("%s where = %q, want %q", ref, claims[ref].where, want)
		}
	}
	prior := &rcPrior{Points: []rcPriorPoint{{Ref: "aaaaaaaaaaaa"}, {Ref: "bbbbbbbbbbbb"}, {Ref: "cccccccccccc"}}}
	for _, s := range rcEarlierStatuses(prior, claims, "", nil, nil, []string{"Makefile", "app/[id]/page.tsx", "src/c.go"}, true) {
		if s.Status != rcEarlierFixed {
			t.Errorf("%s: %s (%s), want fixed", s.Ref, s.Status, s.Note)
		}
	}
}

func TestEarlierStatuses(t *testing.T) {
	prior := &rcPrior{Head: "abc", Points: []rcPriorPoint{
		{Kind: "defect", Ref: "111111111111"}, // re-raised → open, even though EARLIER says fixed
		{Kind: "defect", Ref: "222222222222"}, // re-raised but refuted → withdrawn
		{Kind: "defect", Ref: "333333333333"}, // fixed in a changed file
		{Kind: "defect", Ref: "444444444444"}, // "fixed" in a file that did not change
		{Kind: "defect", Ref: "555555555555"}, // "fixed" with no location
		{Kind: "defect", Ref: "666666666666"}, // wrong
		{Kind: "defect", Ref: "777777777777"}, // open, withheld by the fact-check
		{Kind: "defect", Ref: "888888888888"}, // not accounted for
		{Kind: "nit", Text: "a nit has no ref"},
	}}
	claims := map[string]rcEarlierClaim{
		"111111111111": {rcEarlierFixed, "src/a.go:1", ""},
		"333333333333": {rcEarlierFixed, "src/b.go:22", "guarded"},
		"444444444444": {rcEarlierFixed, "src/old.go:9", ""},
		"555555555555": {rcEarlierFixed, "", ""},
		"666666666666": {rcEarlierWithdrawn, "", "the caller validates"},
		"777777777777": {rcEarlierOpen, "", ""},
	}
	final := "x\n" + rcReviewDataMarker + "\nISSUES:\n- src/a.go:10 — still panics (earlier:111111111111)\n"
	refutedIssue := "src/d.go:4 — reads past the end"
	tags := map[string]string{}
	rcRememberTags(tags, "issues", "- "+refutedIssue, "(earlier: 222222222222)")
	res := &rcChallengeResult{Allegations: []rcAllegationResult{{Issue: refutedIssue, Status: rcStatusRefuted, Reason: "the loop bound is len-1"}}}

	got := map[string]rcEarlierStatus{}
	for _, s := range rcEarlierStatuses(prior, claims, final, res, tags, []string{"src/b.go"}, true) {
		got[s.Ref] = s
	}
	want := map[string]string{
		"111111111111": rcEarlierOpen,
		"222222222222": rcEarlierWithdrawn,
		"333333333333": rcEarlierFixed,
		"444444444444": rcEarlierUnknown,
		"555555555555": rcEarlierUnknown,
		"666666666666": rcEarlierWithdrawn,
		"777777777777": rcEarlierOpen,
		"888888888888": rcEarlierUnknown,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d statuses, want %d: %+v", len(got), len(want), got)
	}
	for ref, st := range want {
		if got[ref].Status != st {
			t.Errorf("%s: %s (%s), want %s", ref, got[ref].Status, got[ref].Note, st)
		}
	}
	if got["333333333333"].Where != "src/b.go:22" || got["333333333333"].Note != "guarded" {
		t.Errorf("fixed status lost its location or note: %+v", got["333333333333"])
	}
	if !strings.Contains(got["222222222222"].Note, "loop bound") {
		t.Errorf("withdrawn-by-fact-check lost its reason: %+v", got["222222222222"])
	}

	// When the previous head is not in this clone, a "fixed" cannot be
	// checked against the changed files, so it does not close the thread.
	for _, s := range rcEarlierStatuses(prior, claims, final, res, tags, nil, false) {
		if (s.Ref == "333333333333" || s.Ref == "444444444444") && s.Status != rcEarlierUnknown {
			t.Errorf("unknown change set: %s is %s, want unknown", s.Ref, s.Status)
		}
	}
	if rcEarlierStatuses(nil, claims, final, res, tags, nil, true) != nil {
		t.Error("statuses with no previous review")
	}
}

func TestReviewPromptAsksForEarlier(t *testing.T) {
	for _, want := range []string{"Account for EVERY earlier ref under EARLIER", "EARLIER: (only when YOUR PREVIOUS REVIEW is present", "a wrong \"fixed\" hides a live bug"} {
		if !strings.Contains(rcReviewSystem, want) {
			t.Errorf("review prompt lost %q", want)
		}
	}
}
