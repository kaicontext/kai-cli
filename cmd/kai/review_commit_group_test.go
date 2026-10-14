package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// kai-api#4's shape: three bugs, each confirmed twice, the pairs sharing no
// code names. The grouping folds each repeat into the first.
func TestGroupingFoldsRepeatsTheModelNames(t *testing.T) {
	raw := "prose\n" + rcReviewDataMarker + "\nMERGE_READY: 3\nISSUES:\n" +
		"- internal/store/billing.go:56 — the backoff is measured from created_at, so old rows retry on every tick\n" +
		"- internal/models/models.go:221 — `billingTransport` mints a new charge id per request, so a retry is charged twice\n" +
		"- internal/store/billing.go:57 — once a charge is an hour old the due condition is always true, so it re-reports every cycle\n" +
		"- internal/models/models.go:218 — `RoundTrip` assigns a new charge id to every HTTP request; a retried call gets a different id\n" +
		"- nit: internal/store/billing.go:3 — the comment names the wrong table\n" +
		"DECISIONS:\n- [money] tenants are charged gateway cost +10%\n"
	var asked string
	got, n := rcGroupDuplicateIssuesWith(raw, nil, func(user string) (string, error) {
		asked = user
		return "3 = 1\n4 = 2\n", nil
	})
	if n != 2 {
		t.Fatalf("folded %d, want 2:\n%s", n, got)
	}
	if strings.Contains(asked, "nit:") || !strings.HasPrefix(asked, "1. internal/store/billing.go:56") || strings.Count(asked, "\n") != 4 {
		t.Errorf("asked about the wrong findings:\n%s", asked)
	}
	_, risks, decisions, _, _, _ := rcParseReviewOutput(got)
	if len(risks) != 3 || !strings.Contains(risks[0], "(also: internal/store/billing.go:57)") || !strings.Contains(risks[1], "(also: internal/models/models.go:218)") || len(decisions) != 1 {
		t.Errorf("risks = %q, decisions = %q", risks, decisions)
	}
}

// Anything unusable leaves the review exactly as it was.
func TestGroupingLeavesTheReviewAloneOnABadAnswer(t *testing.T) {
	raw := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — one\n- a.go:9 — two\n"
	for name, reply := range map[string]string{"none": "NONE", "self": "1 = 1", "forward": "1 = 2", "out of range": "7 = 1", "prose": "Findings 2 and 1 look similar."} {
		if got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return reply, nil }); n != 0 || got != raw {
			t.Errorf("%s: folded %d", name, n)
		}
	}
	if got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return "", errors.New("timeout") }); n != 0 || got != raw {
		t.Error("a failed call changed the review")
	}
	one := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — one\n"
	called := false
	rcGroupDuplicateIssuesWith(one, nil, func(string) (string, error) { called = true; return "", nil })
	if called {
		t.Error("asked about a review with one finding")
	}
}

// A repeat of a repeat folds into the first.
func TestGroupingChainsFoldIntoTheFirst(t *testing.T) {
	raw := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — x is nil\n- a.go:2 — x can be nil here\n- a.go:3 — nil x is dereferenced\n"
	got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return "2 = 1\n3 = 2", nil })
	_, risks, _, _, _, _ := rcParseReviewOutput(got)
	if n != 2 || len(risks) != 1 || !strings.Contains(risks[0], "a.go:2") || !strings.Contains(risks[0], "a.go:3") {
		t.Errorf("folded %d, risks = %q", n, risks)
	}
}

// The also-list reads in finding order on every run, and a repeat on the
// kept finding's own line adds no location.
func TestGroupingIsStableAndSkipsItsOwnLine(t *testing.T) {
	raw := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — x is nil\n- a.go:5 — x can be nil\n- a.go:1 — nil x is dereferenced\n- a.go:9 — x is unchecked\n- a.go:7 — x may be nil\n"
	for i := 0; i < 20; i++ {
		got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return "5 = 1\n2 = 1\n3 = 1\n4 = 1", nil })
		_, risks, _, _, _, _ := rcParseReviewOutput(got)
		if n != 4 || len(risks) != 1 || !strings.Contains(risks[0], "(also: a.go:5, a.go:9, a.go:7)") {
			t.Fatalf("run %d: %q", i, risks)
		}
	}
}

// A folded repeat's earlier-ref moves to the finding kept (so kai-server
// does not post an already-raised point as new), with the higher severity.
func TestGroupingCarriesTheRepeatsTags(t *testing.T) {
	draft := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — x is nil (severity: low)\n- a.go:5 — x can be nil here (severity: high) (earlier: 0123456789ab)\n"
	raw, refs := rcTakeEarlierTags(draft)
	got, n := rcGroupDuplicateIssuesWith(raw, refs, func(string) (string, error) { return "2 = 1", nil })
	_, risks, _, _, _, _ := rcParseReviewOutput(rcPutEarlierTags(got, refs))
	if n != 1 || len(risks) != 1 || !strings.Contains(risks[0], "(earlier: 0123456789ab)") || !strings.Contains(risks[0], "(severity: high)") || strings.Contains(risks[0], "low") {
		t.Errorf("risks = %q, want the kept finding with the repeat's ref and the higher severity", risks)
	}

	// The kept finding's own ref stays.
	draft = rcReviewDataMarker + "\nISSUES:\n- a.go:1 — x is nil (earlier: aaaaaaaaaaaa)\n- a.go:5 — x can be nil here (earlier: 0123456789ab)\n"
	raw, refs = rcTakeEarlierTags(draft)
	got, _ = rcGroupDuplicateIssuesWith(raw, refs, func(string) (string, error) { return "2 = 1", nil })
	if _, risks, _, _, _, _ = rcParseReviewOutput(rcPutEarlierTags(got, refs)); len(risks) != 1 || !strings.Contains(risks[0], "aaaaaaaaaaaa") || strings.Contains(risks[0], "0123456789ab") {
		t.Errorf("risks = %q, want the kept finding's own ref", risks)
	}
}

// Also-lists merge: the repeat's own also-locations are kept, and one the
// kept finding already lists is not added twice.
func TestGroupingMergesAlsoLists(t *testing.T) {
	raw := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — x is nil (also: a.go:9).\n- a.go:5 — x can be nil here (also: b.go:2, a.go:9).\n"
	got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return "2 = 1", nil })
	_, risks, _, _, _, _ := rcParseReviewOutput(got)
	if n != 1 || len(risks) != 1 || !strings.Contains(risks[0], "(also: a.go:9, a.go:5, b.go:2)") || strings.Count(risks[0], "a.go:9") != 1 {
		t.Errorf("risks = %q", risks)
	}
}

// kai-server#467: six confirmed findings, two of them said twice. The answer
// folds them whether the model writes "5 = 2" or "- 5 = 2." as a list.
func TestGroupingOnTheTruncateSample(t *testing.T) {
	raw := rcReviewDataMarker + "\nISSUES:\n" +
		"- kailab-control/internal/textutil/truncate.go:21 — TruncateWords always backs up to the last space even when the cut lands on a word boundary\n" +
		"- kailab-control/internal/textutil/truncate.go:8 — Both functions use byte length and byte slicing while documenting \"characters\"\n" +
		"- kailab-control/internal/textutil/truncate.go:11 — max <= 0 with a non-empty string makes s[:max-1] panic (also: kailab-control/internal/textutil/truncate.go:20)\n" +
		"- kailab-control/internal/textutil/truncate.go:22 — runs of consecutive spaces leave trailing whitespace before the \"…\"\n" +
		"- kailab-control/internal/textutil/truncate.go:11 — `len` and `s[:max-1]` count bytes, not characters\n" +
		"- kailab-control/internal/textutil/truncate.go:11 — `max <= 0` with a non-empty `s` slices with `max-1 < 0` and panics\n"
	for _, reply := range []string{"5 = 2\n6 = 3", "- 5 = 2.\n- 6 = 3.", "#5 = #2\n#6 = #3\n"} {
		got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return reply, nil })
		_, risks, _, _, _, _ := rcParseReviewOutput(got)
		if n != 2 || len(risks) != 4 {
			t.Errorf("answer %q: folded %d, %d findings left", reply, n, len(risks))
			continue
		}
		if !strings.Contains(risks[1], "(also: kailab-control/internal/textutil/truncate.go:11)") || strings.Count(risks[2], "truncate.go:11") != 1 {
			t.Errorf("answer %q: %q", reply, risks)
		}
	}
}

// The call leaves room for the model to reason before it answers (on
// kai-server#467 a 400-token cap left none), and a reply with no text is a
// failed call, not "no repeats".
func TestGroupingCallLeavesRoomToReason(t *testing.T) {
	raw := rcReviewDataMarker + "\nISSUES:\n- a.go:1 — x is nil\n- a.go:2 — x can be nil here\n"
	var req provider.Request
	reply := ""
	prov := rcChallengeProvider{send: func(_ context.Context, r provider.Request) (provider.Response, error) {
		req = r
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: reply}}, OutputTokens: 400}, nil
	}}
	if got, n := rcGroupDuplicateIssues(context.Background(), prov, "z-ai/glm-5.2", raw, nil); n != 0 || got != raw {
		t.Errorf("an empty answer changed the review: %d", n)
	}
	if req.MaxTokens < 4096 || req.ReasoningEffort != "low" {
		t.Errorf("max tokens %d, effort %q: no room to reason and answer", req.MaxTokens, req.ReasoningEffort)
	}
	reply = "2 = 1"
	if _, n := rcGroupDuplicateIssues(context.Background(), prov, "z-ai/glm-5.2", raw, nil); n != 1 {
		t.Errorf("folded %d, want 1", n)
	}
}

// The read-through folds the same way: a repeat's "### " section goes, the
// finding it repeats carries the also-list, and the rest is untouched
// (kai-server#467 showed all eight write-ups under four findings).
func TestGroupingFoldsTheReadThrough(t *testing.T) {
	a := "a.go:8 — slices by bytes, not characters"
	b := "a.go:11 — max <= 0 panics"
	c := "a.go:10 — `Truncate` counts and slices bytes"
	d := "a.go:10 — `s[:max-1]` panics when max <= 0"
	raw := "## Scope\n- a.go\n\n## Findings\n" +
		"\n### " + a + "\nbyte prose\n\n**Remedy:** use runes\n" +
		"\n### " + b + "\npanic prose\n" +
		"\n### " + c + "\nrepeat of the byte prose\n#include is not a heading\n" +
		"\n### " + d + "\nrepeat of the panic prose\n" +
		"\n## Limitations\n- none\n\n" + rcReviewDataMarker + "\nISSUES:\n- " + a + "\n- " + b + "\n- " + c + "\n- " + d + "\n"
	got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return "3 = 1\n4 = 2", nil })
	prose := got[:strings.Index(got, rcReviewDataMarker)]
	if n != 2 || strings.Contains(prose, "repeat of") || strings.Contains(prose, "#include") || strings.Count(prose, "### ") != 2 {
		t.Fatalf("folded %d; prose:\n%s", n, prose)
	}
	for _, want := range []string{"### a.go:8 — slices by bytes, not characters (also: a.go:10).", "byte prose", "**Remedy:** use runes",
		"### a.go:11 — max <= 0 panics (also: a.go:10).", "panic prose", "## Limitations\n- none", "## Scope\n- a.go"} {
		if !strings.Contains(prose, want) {
			t.Errorf("prose lost %q:\n%s", want, prose)
		}
	}
}

// A repeat word for word the same as the finding it repeats: one section
// stays, and only one.
func TestGroupingFoldsAnIdenticalRepeatsProse(t *testing.T) {
	a := "a.go:8 — slices by bytes"
	raw := "## Findings\n\n### " + a + "\nfirst prose\n\n### " + a + "\nsecond prose\n\n## Limitations\n- none\n\n" +
		rcReviewDataMarker + "\nISSUES:\n- " + a + "\n- " + a + "\n- b.go:3 — another bug\n"
	got, n := rcGroupDuplicateIssuesWith(raw, nil, func(string) (string, error) { return "2 = 1", nil })
	prose := got[:strings.Index(got, rcReviewDataMarker)]
	if n != 1 || strings.Count(prose, "### "+a) != 1 || !strings.Contains(prose, "first prose") || strings.Contains(prose, "second prose") {
		t.Errorf("folded %d; prose:\n%s", n, prose)
	}
}
