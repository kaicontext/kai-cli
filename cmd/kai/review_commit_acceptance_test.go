package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
	"os"
	"strings"
	"testing"
)

func TestAcceptanceRejectsCapturedGenericPRTitle(t *testing.T) {
	data, err := os.ReadFile("testdata/acceptance-keycloak-32918.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Author   string           `json:"author"`
		Decision rcDecisionResult `json:"decision"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"checks":[{"id":1,"accepted":false,"reason":"The title only requests caching, not a login-display change."}]}`, `{"checks":[]}`, `{"checks":[{"id":1,"reason":"missing decision"}]}`} {
		p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
			if req.System != rcAcceptanceSystem || len(req.Tools) != 0 {
				t.Fatal("acceptance is not independent")
			}
			text := req.Messages[0].Parts[0].(message.TextContent).Text
			if strings.Contains(text, "inferredIntent") {
				t.Fatal("inferred intent used as evidence")
			}
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: raw}}}, nil
		}}
		r := &rcChallengeResult{Decisions: []rcDecisionResult{fixture.Decision}, match: finding.MatchPartial, proposed: finding.Readiness(4)}
		rcAuditAcceptance(rcWithAuthorText(context.Background(), fixture.Author), p, "test", r)
		if r.Decisions[0].Status != rcStatusUnresolved || strings.Contains(r.Review, "## Decisions") {
			t.Fatalf("unsupported tradeoff survived: %+v", r)
		}
		if r.AcceptanceAudit == nil || r.AcceptanceAudit.Raw != raw {
			t.Fatal("missing audit")
		}
	}
}
func TestAcceptanceExplicitContractAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
			if fail {
				return provider.Response{}, errors.New("timeout")
			}
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: `{"checks":[{"id":1,"accepted":true,"reason":"The author explicitly requires missing users to raise TypeError."}]}`}}}, nil
		}}
		r := &rcChallengeResult{Allegations: []rcAllegationResult{{Issue: "Missing users now raise TypeError", Status: rcStatusRefuted, RefutationBasis: "author_acceptance", AcceptanceQuote: "Missing users must raise TypeError."}}, match: finding.MatchVerified, proposed: finding.Readiness(4)}
		rcAuditAcceptance(rcWithAuthorText(context.Background(), "Missing users must raise TypeError."), p, "test", r)
		if fail {
			if r.Allegations[0].Status != rcStatusUnresolved || !r.VerificationIncomplete {
				t.Fatal("failed acceptance cleared issue")
			}
		} else if r.Allegations[0].Status != rcStatusRefuted {
			t.Fatal("explicit contract lost")
		}
	}
}
