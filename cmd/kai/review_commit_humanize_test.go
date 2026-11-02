package main

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

func TestHumanizeAppliesOnlyCompleteRewordings(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Status: rcStatusSupported, Finding: "f1"},
		{ID: 2, Status: rcStatusSupported, Finding: "f2"},
		{ID: 3, Status: rcStatusSupported, Finding: "f3"},
		{ID: 4, Status: rcStatusRefuted, Finding: "f4"},
	}}
	got, err := rcParseHumanize(`Sure: {"findings": [
		{"id": 1, "title": "Relayed events lose UnitsChanged.", "plain": "Plain.", "when": "When.", "impact": "Impact.", "fix": "Fix."},
		{"id": 2, "title": "", "plain": "No title."},
		{"id": 3, "title": "A title far too long to be a title because it keeps going and going well past ninety characters", "plain": "x"},
		{"id": 4, "title": "Refuted", "plain": "x"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if n := rcApplyHumanize(res, []int{0, 1, 2}, got); n != 1 {
		t.Fatalf("applied %d, want 1", n)
	}
	a := res.Allegations[0]
	if a.Title != "Relayed events lose UnitsChanged" || a.Plain != "Plain." || a.When != "When." || a.Impact != "Impact." || a.Fix != "Fix." {
		t.Errorf("rewording not applied as written: %+v", a)
	}
	for _, i := range []int{1, 2, 3} {
		if b := res.Allegations[i]; b.Title != "" || b.Plain != "" {
			t.Errorf("allegation %d got a rewording it should not have: %+v", b.ID, b)
		}
	}
	for i, a := range res.Allegations {
		if want := []string{rcStatusSupported, rcStatusSupported, rcStatusSupported, rcStatusRefuted}[i]; a.Status != want {
			t.Errorf("humanize changed a status: %+v", a)
		}
	}
}

// After the selection reorders, ids run out of order; answers are numbered by
// position, so each title lands on the finding it was written for.
func TestHumanizeMapsAnswersByPosition(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Status: rcStatusSupported, Finding: "f1"},
		{ID: 3, Status: rcStatusSupported, Finding: "f3"},
		{ID: 2, Status: rcStatusSupported, Finding: "f2"},
	}}
	got := map[int]rcHumanized{1: {Title: "First", Plain: "p"}, 2: {Title: "Second", Plain: "p"}, 3: {Title: "Third", Plain: "p"}}
	rcApplyHumanize(res, []int{0, 1, 2}, got)
	for i, want := range []string{"First", "Second", "Third"} {
		if res.Allegations[i].Title != want {
			t.Errorf("position %d (id %d) titled %q, want %q", i, res.Allegations[i].ID, res.Allegations[i].Title, want)
		}
	}
}

func TestHumanizeRefusesAnAnswerWithoutJSON(t *testing.T) {
	if _, err := rcParseHumanize("I could not do that."); err == nil {
		t.Error("an answer without JSON was accepted")
	}
}

func TestHumanizeKeepsTheStatementApart(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{{ID: 1, Status: rcStatusSupported, Finding: "f"}}}
	got, err := rcParseHumanize(`{"findings": [{"id": 1, "statement": " ` + "`load`" + ` skips the fallback locale, so a missing translation is cached. ", "title": "T", "plain": "P"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	rcApplyHumanize(res, []int{0}, got)
	if want := "`load` skips the fallback locale, so a missing translation is cached."; res.Allegations[0].statement != want {
		t.Errorf("statement = %q, want %q", res.Allegations[0].statement, want)
	}
}

// The sentence is replaced; the location, the nit marker and the "(also: …)"
// list are what everything downstream parses, and stay exactly.
func TestOneClaimIssueKeepsLocationNitAndAlso(t *testing.T) {
	issue := "nit: lib/cache.rb:67 — The key is `k + locale`. The key also ignores the default. (also: lib/cache.rb:90, lib/other.rb:3)"
	got, ok := rcOneClaimIssue(issue, "The cache key `k + locale` omits the fallback chain, so a stale miss is served.")
	want := "nit: lib/cache.rb:67 — The cache key `k + locale` omits the fallback chain, so a stale miss is served. (also: lib/cache.rb:90, lib/other.rb:3)"
	if !ok || got != want {
		t.Fatalf("got %q (%v), want %q", got, ok, want)
	}
	if p, l, ok := rcIssueLocation(got); !ok || p != "lib/cache.rb" || l != 67 {
		t.Errorf("location no longer reads: %s:%d %v", p, l, ok)
	}
	if _, nit := rcSplitNit(got); !nit {
		t.Error("nit marker lost")
	}
}

func TestOneClaimIssueTrimsWhatTheStatementRepeats(t *testing.T) {
	issue := "a.go:10 — old sentence"
	for statement, want := range map[string]string{
		"a.go:10 — `x` is nil after expiry":           "a.go:10 — `x` is nil after expiry",
		"nit: `x` is nil after expiry":                "a.go:10 — `x` is nil after expiry",
		"`x` is nil (also: a.go:12) (severity: high)": "a.go:10 — `x` is nil",
		"`x` is nil after expiry — so `y` panics":     "a.go:10 — `x` is nil after expiry, so `y` panics",
		"  `x` is nil after expiry, so `y` panics.  ": "a.go:10 — `x` is nil after expiry, so `y` panics.",
	} {
		if got, ok := rcOneClaimIssue(issue, statement); !ok || got != want {
			t.Errorf("statement %q: got %q (%v), want %q", statement, got, ok, want)
		}
	}
}

func TestOneClaimIssueRefusesUnusableStatements(t *testing.T) {
	for _, c := range []struct{ issue, statement string }{
		{"a.go:10 — s", ""},
		{"a.go:10 — s", "two\nlines"},
		{"a.go:10 — s", strings.Repeat("x", rcOneClaimMaxLen+1)},
		{"no location here", "a fine sentence"},
		{"a.go:10 without the separator", "a fine sentence"},
	} {
		if got, ok := rcOneClaimIssue(c.issue, c.statement); ok {
			t.Errorf("issue %q, statement %q: accepted as %q", c.issue, c.statement, got)
		}
	}
}

func oneClaimFixture() *rcChallengeResult {
	return &rcChallengeResult{match: finding.MatchPartial, proposed: finding.ReadinessSmallFixes, Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:10 — first problem. It also breaks something else", Status: rcStatusSupported, Finding: "check's explanation", Remedy: "check's fix",
			statement: "`load` caches a miss before the fallback loads", Plain: "Plain explanation.", Fix: "Plain fix."},
		{ID: 2, Issue: "b.go:3 — kept as the check wrote it", Status: rcStatusSupported, Finding: "f2"},
		{ID: 3, Issue: "c.go:1 — refuted", Status: rcStatusRefuted, statement: "never posted"},
	}}
}

func TestApplyOneClaimRewritesWhatIsPosted(t *testing.T) {
	res := oneClaimFixture()
	oldKey, _, _ := rcEarlierBulletKey("issues", "- "+res.Allegations[0].Issue)
	earlier := map[string]string{oldKey: "(earlier: 0123456789ab) (severity: high)"}
	rewrote, kept := rcApplyOneClaim(res, earlier)
	if rewrote != 1 || kept != 1 {
		t.Fatalf("rewrote %d, kept %d; want 1 and 1", rewrote, kept)
	}
	a := res.Allegations[0]
	if a.Issue != "a.go:10 — `load` caches a miss before the fallback loads" || a.Finding != "Plain explanation." || a.Remedy != "Plain fix." {
		t.Errorf("not rewritten as expected: %+v", a)
	}
	if a.Original == nil || a.Original.Issue != "a.go:10 — first problem. It also breaks something else" || a.Original.Finding != "check's explanation" || a.Original.Remedy != "check's fix" {
		t.Errorf("original wording not kept: %+v", a.Original)
	}
	if b := res.Allegations[1]; b.Issue != "b.go:3 — kept as the check wrote it" || b.Original != nil {
		t.Errorf("an allegation without a statement changed: %+v", b)
	}
	if c := res.Allegations[2]; c.Issue != "c.go:1 — refuted" || c.Status != rcStatusRefuted {
		t.Errorf("a refuted allegation changed: %+v", c)
	}
	// The review is rebuilt from the rewritten allegations: the posted
	// sentence is the statement, and the rider is gone.
	if !strings.Contains(res.Review, "- a.go:10 — `load` caches a miss before the fallback loads\n") || strings.Contains(res.Review, "also breaks something else") {
		t.Errorf("review not rebuilt from the rewrite:\n%s", res.Review)
	}
	// The earlier-point and severity tags follow the new wording.
	newKey, _, _ := rcEarlierBulletKey("issues", "- "+a.Issue)
	if earlier[newKey] != "(earlier: 0123456789ab) (severity: high)" {
		t.Errorf("tags not carried to the new wording: %q", earlier)
	}
	if final := rcPutEarlierTags(res.Review, earlier); !strings.Contains(final, "`load` caches a miss before the fallback loads (earlier: 0123456789ab)") {
		t.Errorf("tags not put back on the rewritten bullet:\n%s", final)
	}
}

func TestApplyOneClaimNeedsAnAssembledReview(t *testing.T) {
	res := oneClaimFixture()
	res.match = ""
	if rewrote, kept := rcApplyOneClaim(res, nil); rewrote != 0 || kept != 0 || res.Allegations[0].Original != nil {
		t.Errorf("rewrote %d, kept %d without an intent verdict: %+v", rewrote, kept, res.Allegations[0])
	}
}

func TestOneClaimOffSwitch(t *testing.T) {
	if !rcOneClaimOn() {
		t.Error("one claim is off by default")
	}
	t.Setenv("KAI_REVIEW_ONE_CLAIM", "0")
	if rcOneClaimOn() {
		t.Error("KAI_REVIEW_ONE_CLAIM=0 did not turn it off")
	}
}
