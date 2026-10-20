package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// The kaicontext/kai-vcs#1 pair, as the gate saw it on 2026-10-14.
const (
	rcTruncIssue = "tools/reference-reader/src/bundle.rs:89 — `uint(&e[1])? as u16` truncates silently, so an inventory type such as 65546 is accepted as 10 (also: tools/reference-reader/src/bundle.rs:106)"
	rcShortIssue = "tools/reference-reader/src/bundle.rs:88 — Inventory entries are indexed as `e[0]`…`e[3]` with no length check, so a short entry panics"
	rcShortWhy   = "Bundle::read calls decode, which runs package::validate before this loop; it checks e.len() != 4 and returns Err(\"invalid inventory tuple\"), so a short entry is rejected before it reaches the indexing."
	rcBundleSrc  = "let e = array(entry)?;\nlet id = digest(&e[0])?;\nlet ty = uint(&e[1])? as u16;\n"
)

func rcKaiVcsPair() []rcAllegationResult {
	return []rcAllegationResult{
		{ID: 1, Issue: rcTruncIssue, Status: rcStatusSupported, Finding: "type truncated", Remedy: "use u16::try_from"},
		{ID: 2, Issue: rcShortIssue, Status: rcStatusRefuted, Reason: rcShortWhy},
	}
}

func TestGuardSiblings(t *testing.T) {
	if got := rcGuardSiblings(rcKaiVcsPair()); len(got[0]) != 1 || got[0][0] != 1 {
		t.Fatalf("the kai-vcs#1 pair was not matched: %v", got)
	}

	for _, tc := range []struct {
		name   string
		mutate func([]rcAllegationResult)
	}{
		{"different file", func(a []rcAllegationResult) {
			a[1].Issue = strings.Replace(a[1].Issue, "bundle.rs", "package.rs", 1)
		}},
		{"far apart", func(a []rcAllegationResult) {
			a[1].Issue = strings.Replace(a[1].Issue, ":88", ":400", 1)
		}},
		{"duplicate refutation", func(a []rcAllegationResult) {
			a[1].Reason = "Duplicate of issue 1: the same cast."
		}},
		{"refuted for another reason", func(a []rcAllegationResult) {
			a[1].Reason = "This is generic advice to add a test, not a defect."
		}},
		{"both supported", func(a []rcAllegationResult) {
			a[1].Status = rcStatusSupported
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcKaiVcsPair()
			tc.mutate(a)
			if got := rcGuardSiblings(a); len(got) != 0 {
				t.Fatalf("paired when it should not: %v", got)
			}
		})
	}

	// An "(also: …)" location counts: the refutation sits next to line 106.
	a := rcKaiVcsPair()
	a[1].Issue = strings.Replace(a[1].Issue, ":88", ":110", 1)
	a[0].Issue = "tools/reference-reader/src/bundle.rs:20 — truncates (also: tools/reference-reader/src/bundle.rs:106)"
	if got := rcGuardSiblings(a); len(got[0]) != 1 {
		t.Fatalf("an (also:) location was ignored: %v", got)
	}
}

// The second check sees the sibling's refutation and its verdict replaces the
// first one.
func TestReconcileSiblings_RechecksWithTheRefutation(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		text := req.Messages[0].Parts[0].(message.TextContent).Text
		if !strings.Contains(text, "A SEPARATE CHECK OF NEARBY CODE") || !strings.Contains(text, "invalid inventory tuple") {
			t.Errorf("the re-check did not carry the sibling refutation:\n%s", text)
		}
		if strings.Count(text, "\n- tools/") > 2 {
			t.Errorf("the re-check should carry one allegation to judge")
		}
		ans := rcChallengeAnswer{
			Scope: []string{"bundle.rs"}, IntentMatch: "verified", MergeReady: 5,
			Checks: []rcIssueCheck{{Issue: rcTruncIssue, Verdict: "refuted",
				Reason:   "package::validate rejects inventory types outside 1..65535 before the cast, so 65546 never reaches it.",
				Evidence: []rcCheckEvidence{{Source: 1, LineStart: 3, LineEnd: 3}}}},
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "c", Name: "submit_review", Input: rcTestAnswer(t, ans)}}}, nil
	}}
	res := &rcChallengeResult{Allegations: rcKaiVcsPair()}
	n := rcReconcileSiblings(context.Background(), p, "test", "Review prose.", res, []rcSource{rcRowSource(rcBundleSrc)}, nil, nil)
	if n != 1 || calls != 1 {
		t.Fatalf("re-checked %d allegations with %d calls, want 1 and 1", n, calls)
	}
	got := res.Allegations[0]
	if got.Status != rcStatusRefuted || got.ID != 1 || got.Issue != rcTruncIssue {
		t.Fatalf("the re-check verdict did not replace the first: %+v", got)
	}
	if res.Allegations[1].Status != rcStatusRefuted {
		t.Fatal("the sibling changed")
	}
}

// A re-check that cannot finish withholds the contested allegation rather
// than publishing it against the sibling's finding.
func TestReconcileSiblings_IncompleteRecheckWithholds(t *testing.T) {
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		return provider.Response{}, errors.New("provider down")
	}}
	res := &rcChallengeResult{Allegations: rcKaiVcsPair()}
	rcReconcileSiblings(context.Background(), p, "test", "Review prose.", res, []rcSource{rcRowSource(rcBundleSrc)}, nil, nil)
	got := res.Allegations[0]
	if got.Status != rcStatusUnresolved || got.Remedy != "" || got.WithheldRemedy != "use u16::try_from" {
		t.Fatalf("contested allegation not withheld: %+v", got)
	}
	if !strings.Contains(got.Reason, "nearby allegation on the same code was refuted") {
		t.Errorf("reason does not say why: %s", got.Reason)
	}
}

// No contradiction, no extra call.
func TestReconcileSiblings_NoSiblingNoCall(t *testing.T) {
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		t.Fatal("unexpected re-check")
		return provider.Response{}, nil
	}}
	a := rcKaiVcsPair()
	a[1].Reason = "Generic advice."
	res := &rcChallengeResult{Allegations: a}
	if n := rcReconcileSiblings(context.Background(), p, "test", "", res, nil, nil, nil); n != 0 {
		t.Fatalf("re-checked %d", n)
	}
}

func TestChallengePromptRequiresOpeningUpstreamValidation(t *testing.T) {
	for _, want := range []string{
		"AN INPUT THE CODE ALREADY REJECTS IS NOT AN INPUT IT ALLOWS",
		"If you have not opened it, the verdict is unverified, never supported",
		"once the checks the code applies before it have run",
	} {
		if !strings.Contains(rcChallengeSystemHead, want) {
			t.Errorf("challenge prompt lost: %q", want)
		}
	}
}
