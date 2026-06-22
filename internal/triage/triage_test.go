package triage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubSender returns a canned response (or error) and records whether
// it was called — used to prove the forced-/plan short-circuit skips
// the LLM entirely.
type stubSender struct {
	resp   string
	err    error
	called bool
}

func (s *stubSender) Send(ctx context.Context, req SenderRequest) (SenderResponse, error) {
	s.called = true
	if s.err != nil {
		return SenderResponse{}, s.err
	}
	return SenderResponse{Text: s.resp}, nil
}

func TestClassify_EachTrack(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want Track
	}{
		{"answer", `{"track":"answer","reason":"a question","answer":"because X"}`, TrackAnswer},
		{"quick", `{"track":"quick","reason":"one-line typo fix"}`, TrackQuick},
		{"plan", `{"track":"plan","reason":"multi-file feature"}`, TrackPlan},
		{"clarify", `{"track":"clarify","reason":"ambiguous","question":"which file?"}`, TrackClarify},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &stubSender{resp: c.resp}
			got, err := Classify(context.Background(), s, "m", Request{UserRequest: "do something"})
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got.Track != c.want {
				t.Errorf("track = %q, want %q", got.Track, c.want)
			}
		})
	}
}

func TestClassify_ForcedPlanSkipsCall(t *testing.T) {
	s := &stubSender{resp: `{"track":"answer"}`}
	got, err := Classify(context.Background(), s, "m", Request{UserRequest: "x", ForcedMode: "plan"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Track != TrackPlan {
		t.Errorf("forced /plan: track = %q, want plan", got.Track)
	}
	if s.called {
		t.Error("forced /plan must not call the LLM")
	}
}

func TestClassify_MalformedResponseDefaultsToPlan(t *testing.T) {
	s := &stubSender{resp: "I think this should be a plan, sorry — no JSON here"}
	got, err := Classify(context.Background(), s, "m", Request{UserRequest: "x"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Track != TrackPlan {
		t.Errorf("malformed response: track = %q, want plan", got.Track)
	}
}

func TestClassify_UnknownTrackDefaultsToPlan(t *testing.T) {
	s := &stubSender{resp: `{"track":"banana","reason":"?"}`}
	got, _ := Classify(context.Background(), s, "m", Request{UserRequest: "x"})
	if got.Track != TrackPlan {
		t.Errorf("unknown track: got %q, want plan", got.Track)
	}
}

func TestClassify_SendErrorDefaultsToPlanAndReturnsErr(t *testing.T) {
	s := &stubSender{err: errors.New("boom")}
	got, err := Classify(context.Background(), s, "m", Request{UserRequest: "x"})
	if err == nil {
		t.Error("expected the transport error to be surfaced")
	}
	if got.Track != TrackPlan {
		t.Errorf("send error: track = %q, want plan", got.Track)
	}
}

func TestParseResult_HandlesFencedJSON(t *testing.T) {
	text := "Here is my decision:\n```json\n{\"track\":\"quick\",\"reason\":\"typo\"}\n```\n"
	r, ok := parseResult(text)
	if !ok || r.Track != TrackQuick {
		t.Errorf("fenced JSON: ok=%v track=%q", ok, r.Track)
	}
}

func TestBuildTriageUserText_IncludesContext(t *testing.T) {
	got := buildTriageUserText(Request{
		UserRequest: "add a flag",
		ForcedMode:  "debug",
		Projects:    []string{"kai", "kai-server"},
		RecentTurns: []string{"earlier ask"},
	})
	for _, want := range []string{"add a flag", "/debug", "kai-server", "earlier ask"} {
		if !strings.Contains(got, want) {
			t.Errorf("user text missing %q, got:\n%s", want, got)
		}
	}
}
