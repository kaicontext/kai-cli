package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/kaicontext/kai-engine/finding"
	"strings"
	"testing"
)

// Exercise the actual output/exit boundary, not a copy of the JSON shape.
// #127/#128 published useful findings but one unresolved allegation made
// the command fail and the PR announce that no review had finished.
func TestReviewOutcomePublication(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		unresolved, interrupted bool
		want                    string
	}{
		{"complete", false, false, "completed"},
		{"missing dependency source", true, false, "completed_with_unresolved"},
		{"unproven race", true, false, "completed_with_unresolved"},
		{"deadline", false, true, "interrupted"},
		{"deadline beats unresolved", true, true, "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			if tc.unresolved {
				a.Checks[0].Verdict = "unverified"
				a.Checks[0].Reason = tc.name
				a.Checks[0].Evidence = nil
			}
			res := rcMustValidate(t, a, nil)
			prose := res.Review
			if tc.interrupted {
				prose = rcIncompleteProse(&rcIncomplete{ChallengeFailure: "context deadline exceeded"})
			}
			var out bytes.Buffer
			err := rcEmitReviewBundle(&out, finding.Finding{}, prose, "grounded", tc.interrupted, nil, res)
			if errors.Is(err, rcErrIncompleteReview) != tc.interrupted {
				t.Fatalf("exit error=%v, interrupted=%v", err, tc.interrupted)
			}
			var b struct {
				Outcome    string
				Incomplete bool
				Review     string
				Challenge  *rcChallengeResult
			}
			if err := json.Unmarshal(out.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
			if b.Outcome != tc.want || b.Incomplete != (tc.interrupted || tc.unresolved) {
				t.Fatalf("wrong outcome/compatibility flag: %+v", b)
			}
			if !tc.interrupted && (!strings.Contains(b.Review, rcEscapeFinding) || !strings.Contains(b.Review, rcEscapeRemedy)) {
				t.Fatal("confirmed finding or remedy lost")
			}
			if tc.unresolved && !tc.interrupted && (!strings.Contains(b.Review, tc.name) || strings.Contains(b.Review, "review is incomplete")) {
				t.Fatal("unresolved question hidden or misreported as execution failure")
			}
		})
	}
}
