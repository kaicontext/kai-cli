package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// Declared citation coordinates. Every challenge source is cited in ONE
// coordinate system, stated in its header: a kai_view result by the file line
// numbers the tool printed, and only within the lines it actually returned;
// everything else by the row numbers the system prints. These tests cover the
// classification, the rendering, the extraction bounds and what an
// unresolvable citation does — not whether any citation is relevant.

func rcLoadJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// The parser against a REAL stored kai_view result (engine v0.6.73): the git
// header before the rows, the "N: text" rows including the phantom empty last
// row a trailing newline produces, and the harness footer after them. Only the
// rows are file coordinates.
func TestKaiViewRowsParsedFromRealResult(t *testing.T) {
	var fx struct{ Call, Content string }
	rcLoadJSON(t, "testdata/citation-coordinates/real-kai-view-result.json", &fx)
	name, input, _ := strings.Cut(fx.Call, " ")
	src := rcToolSource(name, input, fx.Content)
	if src.Coord != rcCoordFile || src.Path != "frontend/dist/app.js" || src.First != 1 || len(src.Rows) != 14 {
		t.Fatalf("classification: coord=%s path=%s first=%d rows=%d", src.Coord, src.Path, src.First, len(src.Rows))
	}
	if src.Rows[8] != "  const full = wsPath ? 'cd ' + JSON.stringify(wsPath) + ' && ' + command : command;" || src.Rows[13] != "" {
		t.Fatalf("rows: %q … %q", src.Rows[8], src.Rows[13])
	}
	// Header and footer are outside the mapping: not rows, not citable.
	if strings.Contains(strings.Join(src.Rows, "\n"), "[git:") || strings.Contains(strings.Join(src.Rows, "\n"), "[turn") || strings.Contains(strings.Join(src.Rows, "\n"), "[deduped") {
		t.Fatalf("header or footer leaked into the rows: %q", src.Rows)
	}
	sources := []rcSource{rcPromptSource("diff"), src}
	if got, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 2, LineStart: 9, LineEnd: 9}); !ok || !strings.HasPrefix(got, "  const full = wsPath") {
		t.Fatalf("file-line citation: %q %v", got, ok)
	}
	for _, bad := range []rcCheckEvidence{{Source: 2, LineStart: 0, LineEnd: 1}, {Source: 2, LineStart: 15, LineEnd: 15}, {Source: 2, LineStart: 9, LineEnd: 8}, {Source: 2, LineStart: 14, LineEnd: 20}} {
		if _, reason, ok := rcExtractCitation(sources, bad); ok || !strings.Contains(reason, "lines 1-14") {
			t.Fatalf("citation outside the returned lines accepted or misreported: %+v → %q %v", bad, reason, ok)
		}
	}
	// The rendering is the tool's own text with a header naming the returned range.
	rendered := rcRenderSource(2, src)
	if !strings.HasPrefix(rendered, "SOURCE 2 (kai_view frontend/dist/app.js — file lines 1-14 returned; cite FILE line numbers exactly as printed below):\n") || !strings.Contains(rendered, "\n9:   const full") || strings.Contains(rendered, "    9| ") {
		t.Fatalf("file source rendering:\n%s", rendered)
	}
}

// A slice: offset is zero-indexed, the first returned file line is offset+1,
// and a truncation trailer marks lines the tool did not return. Requested
// range ≠ returned range: a short file returns fewer rows than the limit, and
// only returned lines are citable.
func TestKaiViewSliceAndTruncationBounds(t *testing.T) {
	content := "[git: clean · last commit abc 1h ago]\n191: a\n192: b\n193: c\n(truncated; 400 more lines after line 193)\n\n\n[turn 3/9 · edits: 0 · reads: 3 · context: 10%]"
	src := rcToolSource("kai_view", `{"file_path":"x.go","offset":190,"limit":3}`, content)
	if src.Coord != rcCoordFile || src.First != 191 || len(src.Rows) != 3 {
		t.Fatalf("slice: %+v", src)
	}
	sources := []rcSource{rcPromptSource("p"), src}
	if got, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 2, LineStart: 191, LineEnd: 193}); !ok || got != "a\nb\nc" {
		t.Fatalf("returned range: %q %v", got, ok)
	}
	// Line 194 was requested by nobody and returned by nobody; the trailer
	// names it but it is not citable. Lines below the slice are not either.
	for _, bad := range []rcCheckEvidence{{Source: 2, LineStart: 194, LineEnd: 194}, {Source: 2, LineStart: 190, LineEnd: 191}, {Source: 2, LineStart: 1, LineEnd: 1}} {
		if _, reason, ok := rcExtractCitation(sources, bad); ok || !strings.Contains(reason, "x.go lines 191-193") {
			t.Fatalf("outside returned lines: %+v → %q %v", bad, reason, ok)
		}
	}
	// A short file returns fewer rows than the limit: the returned range wins.
	short := rcToolSource("kai_view", `{"file_path":"s.go","offset":"0","limit":50}`, "1: only\n2: two\n3: \n")
	if short.First != 1 || len(short.Rows) != 3 {
		t.Fatalf("short file: %+v", short)
	}
	// Results with no file rows have no file coordinates.
	for _, c := range []string{"(empty: offset 900 past end of 300-line file)", "(binary file: a.bin — 12 bytes, not displayed; first NUL byte at offset 0)", ""} {
		if s := rcToolSource("kai_view", `{"file_path":"a","offset":900}`, c); s.Coord != rcCoordRows {
			t.Fatalf("rows expected for %q, got %s", c, s.Coord)
		}
	}
	// Rows must start at offset+1: a result whose numbering does not match the
	// call's offset is not trusted as file coordinates.
	if s := rcToolSource("kai_view", `{"file_path":"a","offset":10}`, "1: x\n2: y\n"); s.Coord != rcCoordRows {
		t.Fatalf("mismatched offset accepted as file coordinates: %+v", s)
	}
}

// rcChallengeSources over a real-shaped multi-pass transcript: the first user
// message is source 1; each retained tool result is a source in order; an
// errored result and an empty result leave NO source (the count alone cannot
// tell the two apart); the coverage gate's second user message is not a
// source; kai_view results with rows are file-addressed, everything else
// row-addressed.
func TestChallengeSourcesFromMultiPassTranscript(t *testing.T) {
	view := func(id, path string, offset int, rows ...string) []message.ContentPart {
		var b strings.Builder
		b.WriteString("[git: clean · last commit abc 1h ago]\n")
		for i, r := range rows {
			b.WriteString(strconv.Itoa(offset+1+i) + ": " + r + "\n")
		}
		b.WriteString("\n\n[turn 1/9 · edits: 0 · reads: 1 · context: 3%]")
		return []message.ContentPart{message.ToolResult{ToolCallID: id, Content: b.String()}}
	}
	tr := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "AUTHOR CONTEXT…\nDIFF:\n+x"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "v1", Name: "kai_view", Input: `{"file_path":"a.go","offset":20,"limit":2}`},
			message.ToolCall{ID: "g1", Name: "kai_grep", Input: `{"query":"foo"}`},
			message.ToolCall{ID: "e1", Name: "kai_view", Input: `{"file_path":"missing.go"}`},
			message.ToolCall{ID: "z1", Name: "kai_files", Input: `{"pattern":"*.md"}`},
		}},
		{Role: message.RoleUser, Parts: append(append(append(view("v1", "a.go", 20, "alpha", "beta"),
			message.ToolResult{ToolCallID: "g1", Content: "a.go:21: alpha\nb.go:3: alpha"}),
			message.ToolResult{ToolCallID: "e1", IsError: true, Content: "no such file"}),
			message.ToolResult{ToolCallID: "z1", Content: ""})},
		// The coverage gate's second pass: a user-role nudge, then more reads.
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "coverage gate: 1 of 2 changed files never opened — open it"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.ToolCall{ID: "v2", Name: "kai_view", Input: `{"file_path":"b.go"}`}}},
		{Role: message.RoleUser, Parts: view("v2", "b.go", 0, "one", "two", "three")},
	}
	sources := rcChallengeSources(tr)
	if len(sources) != 4 {
		t.Fatalf("expected prompt + 3 retained results, got %d: %+v", len(sources), sources)
	}
	if sources[0].Coord != rcCoordRows || !strings.HasPrefix(sources[0].Text, "AUTHOR CONTEXT") {
		t.Fatalf("source 1 is not the prompt: %+v", sources[0])
	}
	if s := sources[1]; s.Coord != rcCoordFile || s.Path != "a.go" || s.First != 21 || len(s.Rows) != 2 || s.Rows[1] != "beta" {
		t.Fatalf("source 2 (kai_view slice): %+v", s)
	}
	if s := sources[2]; s.Coord != rcCoordRows || s.Tool != "kai_grep" || !strings.HasPrefix(s.Text, "kai_grep {") {
		t.Fatalf("source 3 (grep, rows): %+v", s)
	}
	if s := sources[3]; s.Coord != rcCoordFile || s.Path != "b.go" || s.First != 1 || len(s.Rows) != 3 {
		t.Fatalf("source 4 (second-pass view): %+v", s)
	}
	for i, s := range sources {
		if strings.Contains(s.Text, "coverage gate") {
			t.Fatalf("the second user message became source %d", i+1)
		}
	}
	// Citations resolve in each source's own coordinates and the ref says which.
	refs, problems := rcResolveEvidence(1, []rcCheckEvidence{{Source: 2, LineStart: 22, LineEnd: 22}, {Source: 3, LineStart: 2, LineEnd: 3}, {Source: 4, LineStart: 3, LineEnd: 3}}, sources)
	if len(problems) != 0 || len(refs) != 3 || refs[0].Coord != rcCoordFile || refs[0].Path != "a.go" || refs[1].Coord != rcCoordRows || refs[2].Coord != rcCoordFile {
		t.Fatalf("refs=%+v problems=%+v", refs, problems)
	}
	// Row numbers cited into a file source, and file lines cited into a row
	// source, are invalid: there is no fallback to the other system.
	_, problems = rcResolveEvidence(1, []rcCheckEvidence{{Source: 2, LineStart: 2, LineEnd: 2}, {Source: 3, LineStart: 21, LineEnd: 21}}, sources)
	if len(problems) != 2 {
		t.Fatalf("cross-coordinate citations accepted: %+v", problems)
	}
}

// The #119 failure, replayed on a SYNTHETIC RECONSTRUCTION (see the fixture and
// its PROVENANCE for what is exact, inferred and approximated). With declared
// file coordinates, both citations the production validator rejected — 201-205
// into a 25-row slice returning 194-218, and 411-419 into a 200-row slice
// returning 396-595 — resolve, because they name lines those sources actually
// contain. A citation past the returned range still does not, and it degrades
// its allegation instead of withholding the review.
func TestPR119CitationsResolveInFileCoordinates(t *testing.T) {
	var fx struct {
		Label     string                                  `json:"label"`
		Calls     map[string]struct{ Name, Input string } `json:"calls"`
		Results   map[string]string                       `json:"results"`
		Returned  map[string][2]int                       `json:"expectedReturned"`
		Citations struct {
			First           rcCheckEvidence `json:"first"`
			AfterCorrection rcCheckEvidence `json:"afterCorrection"`
		} `json:"citations"`
		LoggedLengths map[string]int `json:"loggedLengths"`
	}
	rcLoadJSON(t, "testdata/citation-coordinates/pr119-review-f4a52f23.json", &fx)
	if !strings.HasPrefix(fx.Label, "SYNTHETIC RECONSTRUCTION") {
		t.Fatal("the fixture must say what it is")
	}
	// Assemble 27 sources with the two reconstructed ones at their inferred
	// positions; the rest are placeholders (their content is unknown).
	sources := make([]rcSource, 27)
	for i := range sources {
		sources[i] = rcRowSource("(source not reconstructed)")
	}
	for _, n := range []string{"4", "25"} {
		c := fx.Calls[n]
		src := rcToolSource(c.Name, c.Input, fx.Results[n])
		idx := map[string]int{"4": 3, "25": 24}[n]
		sources[idx] = src
		// The stored length matches the job log exactly.
		if got := len(rcSourceLines(src.Text)); got != fx.LoggedLengths[n] {
			t.Fatalf("source %s reconstructs to %d lines, log said %d", n, got, fx.LoggedLengths[n])
		}
		if src.Coord != rcCoordFile || src.First != fx.Returned[n][0] || src.First+len(src.Rows)-1 != fx.Returned[n][1] {
			t.Fatalf("source %s returned range: first=%d rows=%d, want %v", n, src.First, len(src.Rows), fx.Returned[n])
		}
	}
	for _, ev := range []rcCheckEvidence{fx.Citations.First, fx.Citations.AfterCorrection} {
		if _, reason, ok := rcExtractCitation(sources, ev); !ok {
			t.Fatalf("citation %+v rejected in file coordinates: %s", ev, reason)
		}
	}
	// And the row-coordinate reading that production applied really does
	// reject both — this is the defect, stated as a test of the old reading.
	for _, ev := range []rcCheckEvidence{fx.Citations.First, fx.Citations.AfterCorrection} {
		rows := len(rcSourceLines(sources[ev.Source-1].Text))
		if ev.LineEnd <= rows {
			t.Fatalf("citation %+v would have been in row range (%d rows) — not the failure the log shows", ev, rows)
		}
	}
	// A citation past what the slice returned still fails, and degrades.
	issues := []string{"cmd/kai/review_commit.go:600 — something past the slice"}
	a := rcChallengeAnswer{IntentMatch: "partial", MergeReady: 4, Checks: []rcIssueCheck{{Issue: issues[0], Verdict: "supported", Reason: "r", Finding: "f", Remedy: "fix", Evidence: []rcCheckEvidence{{Source: 4, LineStart: 596, LineEnd: 600}}}}}
	res, problems, err := rcValidateChallenge(rcTestAnswer(t, a), issues, nil, sources)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0].Reason, "lines 396-595") {
		t.Fatalf("past-range citation: err=%v problems=%+v", err, problems)
	}
	if res.Allegations[0].Status != rcStatusUnresolved || res.Allegations[0].Remedy != "" || !res.Incomplete {
		t.Fatalf("not degraded: %+v", res.Allegations[0])
	}
}

// The single correction round reports EVERY invalid citation at once, across
// checks and decisions, since there is only one attempt.
func TestCitationCorrectionReportsEveryInvalidLocation(t *testing.T) {
	calls := 0
	var feedback string
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		a := rcCDChecks()
		a.Decisions = []rcDecisionCheck{{Decision: rcTestDecision, Verdict: "supported", Reason: "r", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}}}
		if calls == 1 {
			a.Checks[0].Evidence[0].LineEnd = 99
			a.Checks[1].Evidence[0].Source = 7
			a.Decisions[0].Evidence[0].LineStart = 0
		} else {
			feedback = req.Messages[2].Parts[0].(message.TextContent).Text
		}
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestAnswer(t, a)}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue)+"DECISIONS:\n- "+rcTestDecision+"\n", rcCDSources, nil)
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	for _, want := range []string{"check 1, citation 1, source 1", "check 2, citation 1, source 7", "check 3, citation 1, source 2", "source number is out of range", "row range is out of bounds"} {
		if !strings.Contains(feedback, want) {
			t.Fatalf("feedback lacks %q:\n%s", want, feedback)
		}
	}
	if res.Incomplete || res.Allegations[1].Status != rcStatusSupported || res.Decisions[0].Status != rcStatusSupported {
		t.Fatalf("corrected answer not published clean: %+v", res)
	}
}
