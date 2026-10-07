package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAcceptanceRequiresOriginalAuthorEvidence(t *testing.T) {
	author := "Simplify display name lookup"
	accepted := "Missing users must raise TypeError instead of returning Unknown."
	decision := "The author accepts missing-user crashes."
	for _, tc := range []struct {
		name, author, quote string
		want                string
	}{
		{"invented", author, "An error for a missing user is acceptable.", rcStatusUnresolved},
		{"absent", author, "", rcStatusUnresolved},
		{"explicit", accepted, "Missing users must raise TypeError instead of returning Unknown.", rcStatusSupported},
		{"line wrapping", accepted, "Missing users must raise\n TypeError instead of returning Unknown.", rcStatusSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := []rcSource{rcPromptSource("INFERRED INTENT: An error for a missing user is acceptable."), {Text: tc.author, Coord: rcCoordRows, AuthorText: &tc.author}}
			a := rcChallengeAnswer{IntentMatch: "verified", MergeReady: 4, Decisions: []rcDecisionCheck{{Decision: decision, Verdict: "supported", Reason: "accepted", AcceptanceQuote: tc.quote, Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}}}}
			raw, _ := json.Marshal(a)
			r, _, err := rcValidateChallenge(string(raw), nil, []string{decision}, sources)
			if err != nil {
				t.Fatal(err)
			}
			if r.Decisions[0].Status != tc.want {
				t.Fatalf("%+v", r.Decisions[0])
			}
		})
	}
}

func TestRefutationMustDistinguishCodeFromAcceptance(t *testing.T) {
	author := "Simplify lookup"
	sources := append(append([]rcSource{}, rcCDSources...), rcSource{Text: author, Coord: rcCoordRows, AuthorText: &author})
	for _, basis := range []string{"", "author_acceptance", "technical"} {
		a := rcCDChecks()
		a.Checks[0].RefutationBasis = basis
		a.Checks[0].AcceptanceQuote = "The author accepts this failure"
		raw, _ := json.Marshal(a)
		r, _, err := rcValidateChallenge(string(raw), rcCDIssues, nil, sources)
		if err != nil {
			t.Fatal(err)
		}
		want := rcStatusUnresolved
		if basis == "technical" {
			want = rcStatusRefuted
		}
		if r.Allegations[0].Status != want {
			t.Fatalf("basis %q: %+v", basis, r.Allegations[0])
		}
	}
}

func TestAuthorEvidenceCannotComeFromModelOrCode(t *testing.T) {
	quote := "Errors are acceptable"
	sources := []rcSource{rcPromptSource(quote), rcRowSource(quote), {Text: quote, Tool: "kai_view", Coord: rcCoordFile}}
	if rcValidAcceptanceQuote(quote, sources) {
		t.Fatal("model/code text established author acceptance")
	}
	if !strings.Contains(rcChallengeSystemPrompt(false), rcAuthorPolicy) {
		t.Fatal("verifier lacks independent acceptance policy")
	}
}
