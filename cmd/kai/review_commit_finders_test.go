package main

import (
	"reflect"
	"strings"
	"testing"
)

const findersDraft = "Prose.\n\n" + rcReviewDataMarker + "\nINTENT_MATCH: verified\nMERGE_READY: 4\nSUMMARY: ok\nISSUES:\n- a.go:10 — the session user is dereferenced without a nil check after expiry\n"

// A repeat the second finder's merge drops credits the bullet it repeats;
// a new defect is recorded as its own.
func TestFinderLedgerCreditsADroppedRepeat(t *testing.T) {
	l := newRcFinderLedger()
	l.addAll(rcIssuesOf(findersDraft), rcFinderMain)
	extra := []string{
		"a.go:10 — dereferences the session user after expiry without a nil check (severity: high)",
		"b.go:3 — a new defect",
	}
	merged, matched := rcDraftWithSweepMatched(findersDraft, extra)
	if matched[0] == "" || matched[1] != "" {
		t.Fatalf("matched = %q", matched)
	}
	l.merged(extra, matched, nil, rcFinderSecond)
	issues := rcIssuesOf(merged)
	if len(issues) != 2 {
		t.Fatalf("issues = %q", issues)
	}
	if got := l.of(issues[0]); !reflect.DeepEqual(got, []string{rcFinderMain, rcFinderSecond}) {
		t.Errorf("kept bullet's finders = %q, want main and finder2", got)
	}
	if got := l.of(issues[1]); !reflect.DeepEqual(got, []string{rcFinderSecond}) {
		t.Errorf("added bullet's finders = %q, want finder2", got)
	}
}

// rcDraftWithSweep is unchanged for its other callers.
func TestDraftWithSweepMatchesItsMatchedVariant(t *testing.T) {
	extra := []string{"a.go:10 — dereferences the session user after expiry without a nil check", "c.go:1 — x"}
	got, _ := rcDraftWithSweepMatched(findersDraft, extra)
	if want := rcDraftWithSweep(findersDraft, extra); got != want {
		t.Errorf("variants differ:\n%s\n---\n%s", got, want)
	}
}

// Two sweeps proposing one issue give it both; the sweep merge credits them
// to the reviewer's bullet it repeats.
func TestSweepFindersSurviveTheMerges(t *testing.T) {
	a := rcSweepResult{Issues: []string{"a.go:10 — the session user is dereferenced without a nil check after expiry (severity: high)", "d.go:2 — only the first sweep"}}
	a.By = rcFindersOf(a.Issues, rcFinderSweep)
	b := rcSweepResult{Issues: []string{"a.go:10 — the session user is dereferenced without a nil check after expiry (severity: high)"}}
	b.By = rcFindersOf(b.Issues, rcFinderSweep2)
	sw := rcMergeSweeps(a, b)
	if len(sw.Issues) != 2 {
		t.Fatalf("issues = %q", sw.Issues)
	}
	if got := sw.By[rcFinderKeyOnly(sw.Issues[0])]; !reflect.DeepEqual(got, []string{rcFinderSweep, rcFinderSweep2}) {
		t.Errorf("repeated issue's sweeps = %q", got)
	}
	l := newRcFinderLedger()
	l.addAll(rcIssuesOf(findersDraft), rcFinderMain)
	// The tags come off before the merge, as in the review flow.
	for i, is := range sw.Issues {
		stripped, _ := rcSplitTags(is)
		sw.Issues[i] = strings.TrimSpace(stripped)
	}
	merged, matched := rcDraftWithSweepMatched(findersDraft, sw.Issues)
	l.merged(sw.Issues, matched, sw.By, rcFinderSweep)
	issues := rcIssuesOf(merged)
	if got := l.of(issues[0]); !reflect.DeepEqual(got, []string{rcFinderMain, rcFinderSweep, rcFinderSweep2}) {
		t.Errorf("reviewer's bullet finders = %q, want main, sweep, sweep2", got)
	}
	if got := l.of(issues[1]); !reflect.DeepEqual(got, []string{rcFinderSweep}) {
		t.Errorf("first sweep's own bullet finders = %q, want sweep", got)
	}
}

// The fold before the check drops a bullet and leaves its location in
// "(also: …)": the folded bullet's finders are found there.
func TestFinderLedgerFollowsAnAlsoFold(t *testing.T) {
	l := newRcFinderLedger()
	l.add("a.go:10 — `x` is never closed", rcFinderMain)
	l.add("a.go:50 — `x` is never closed", rcFinderSweep2)
	l.add("a.go:70 — one defect", rcFinderSweep)
	l.add("a.go:70 — another defect on the same line", rcFinderSecond)
	if got := l.of("a.go:10 — `x` is never closed (also: a.go:50)"); !reflect.DeepEqual(got, []string{rcFinderMain, rcFinderSweep2}) {
		t.Errorf("finders = %q, want main and sweep2", got)
	}
	// A location two bullets share is ambiguous and adds nobody.
	if got := l.of("a.go:10 — `x` is never closed (also: a.go:70)"); !reflect.DeepEqual(got, []string{rcFinderMain}) {
		t.Errorf("finders = %q, want main only", got)
	}
	var none *rcFinderLedger
	none.add("a.go:1 — x", rcFinderMain)
	if got := none.of("a.go:1 — x"); got != nil {
		t.Errorf("nil ledger answered %q", got)
	}
}

// Each allegation carries the finders of its whole vote group; the gate's
// decisions do not change.
func TestPublishGateWidensProposersAndDecidesAsBefore(t *testing.T) {
	plain := gateFixture()
	rcPublishGate(plain, gateRepo(), true)

	res := gateFixture()
	l := newRcFinderLedger()
	l.add("a.go:11 — raised three times", rcFinderMain)
	l.add("a.go:11 — the same, reworded", rcFinderSweep2)
	l.add("a.go:12 — raised once", rcFinderSweep)
	if known := rcRecordProposers(res, l); known != 3 {
		t.Errorf("known = %d, want 3", known)
	}
	rcPublishGate(res, gateRepo(), true)
	for i, a := range res.Allegations {
		if a.Status != plain.Allegations[i].Status || a.Reason != plain.Allegations[i].Reason || a.Votes != plain.Allegations[i].Votes {
			t.Errorf("allegation %d decided differently with finders recorded: %+v vs %+v", a.ID, a, plain.Allegations[i])
		}
		want := map[int][]string{1: {rcFinderMain, rcFinderSweep2}, 2: {rcFinderMain, rcFinderSweep2}, 3: {rcFinderMain, rcFinderSweep2}, 4: {rcFinderSweep}}[a.ID]
		if !reflect.DeepEqual(a.Proposers, want) {
			t.Errorf("allegation %d proposers = %q, want %q", a.ID, a.Proposers, want)
		}
	}
}
