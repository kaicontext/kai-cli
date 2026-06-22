package views

import (
	"strings"
	"testing"
	"time"

	"kai/api/planner"
)

// TestFormatCacheBand_AdaptiveBands pins the three-band display:
// healthy quiet headline, neutral quiet headline, broken-cache
// loud diagnostic. The May-3 bug pattern (7% reuse) must produce
// the warning band — that's the whole point of the redesign.
func TestFormatCacheBand_AdaptiveBands(t *testing.T) {
	cases := []struct {
		name              string
		fresh, create, read int
		wantContains      []string
		wantOmits         []string
	}{
		{
			name:   "healthy: 87% reused",
			fresh:  100, create: 1_000, read: 7_500,
			wantContains: []string{"cache:", "87% reused"},
			wantOmits:    []string{"⚠", "writing fresh", "above"},
		},
		{
			name:   "neutral: 50% reused",
			fresh:  500, create: 500, read: 1_000,
			wantContains: []string{"cache:", "50% reused"},
			wantOmits:    []string{"⚠", "writing fresh"},
		},
		{
			// May-3 pattern: cache_creation dominated reads; ~8-9%
			// reuse rate. Don't pin the exact percent — the test
			// is about the BAND firing correctly. Money was
			// removed from the warning per the May-5 cleanup;
			// token volume now conveys the same diagnostic.
			name:   "broken: <30% reused (May-3 pattern)",
			fresh:  14, create: 162_000, read: 16_000,
			wantContains: []string{"⚠", "reused", "writing fresh", "billed at write rate"},
			wantOmits:    []string{"$"}, // no dollars in the trailer
		},
		{
			name:   "idle: no input at all",
			fresh:  0, create: 0, read: 0,
			wantContains: []string{"idle"},
			wantOmits:    []string{"reused", "%"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCacheBand(tc.fresh, tc.create, tc.read)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in: %q", want, got)
				}
			}
			for _, omit := range tc.wantOmits {
				if strings.Contains(got, omit) {
					t.Errorf("unexpected %q in: %q", omit, got)
				}
			}
		})
	}
}

// TestFormatCacheBand_BrokenShowsTokenVolume pins the post-
// money-removal diagnostic: instead of "$0.56 above cached
// baseline" the warning now shows "162k tokens billed at
// write rate". Same diagnostic value (the user knows real
// volume is being burned), no dollar display.
func TestFormatCacheBand_BrokenShowsTokenVolume(t *testing.T) {
	got := formatCacheBand(14, 162_000, 16_000)
	if !strings.Contains(got, "162k") || !strings.Contains(got, "billed at write rate") {
		t.Errorf("expected token-volume diagnostic, got: %q", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("dollar amount leaked into cache band: %q", got)
	}
}

// TestFormatRunSummary_ClaudeCodeShape pins the wire format so
// the apples-to-apples comparison remains stable. Users moving
// between kai and Claude Code should read off the same
// "(elapsed · ↓ Xk tokens)" shape.
func TestFormatRunSummary_ClaudeCodeShape(t *testing.T) {
	start := time.Now().Add(-(14*time.Minute + 15*time.Second))
	got := formatRunSummary(start, time.Time{}, 46_000)
	for _, want := range []string{"14m", "15s", "↓", "46.0k", "tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in run summary, got: %q", want, got)
		}
	}
	if !strings.HasPrefix(got, "(") || !strings.HasSuffix(got, ")") {
		t.Errorf("expected parenthesized summary, got: %q", got)
	}
}

// TestFormatRunSummary_EmptyOnZeroOutput keeps the trailer
// quiet on tool-only turns where no output tokens have arrived
// yet. "(0s · ↓ 0 tokens)" would just be visual noise.
func TestFormatRunSummary_EmptyOnZeroOutput(t *testing.T) {
	if got := formatRunSummary(time.Now(), time.Time{}, 0); got != "" {
		t.Errorf("expected empty summary on 0 tokens, got: %q", got)
	}
}

// TestFormatRunSummary_FirstByteSegment is the surface test for
// the 2026-05-24 TTFR metric. When firstResponseAt is non-zero
// the trailer surfaces a third segment so a slow first-byte is
// visible without a profiler.
func TestFormatRunSummary_FirstByteSegment(t *testing.T) {
	start := time.Now().Add(-30 * time.Second)
	firstByte := start.Add(4 * time.Second)
	out := formatRunSummary(start, firstByte, 100)
	if !strings.Contains(out, "first-byte 4s") {
		t.Errorf("expected first-byte segment, got: %q", out)
	}
	// Zero firstResponseAt should omit the segment (e.g. host-task
	// turn that doesn't stream).
	out = formatRunSummary(start, time.Time{}, 100)
	if strings.Contains(out, "first-byte") {
		t.Errorf("expected NO first-byte segment with zero timestamp, got: %q", out)
	}
}

// TestFormatElapsed_DropsLeadingZeroUnits matches Claude Code's
// convention: a 9-second turn reads "9s", not "0h 0m 9s".
func TestFormatElapsed_DropsLeadingZeroUnits(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Millisecond:                "1s", // sub-second floored
		9 * time.Second:                       "9s",
		1*time.Minute + 5*time.Second:         "1m 5s",
		14*time.Minute + 15*time.Second:       "14m 15s",
		2*time.Hour + 3*time.Minute + 7*time.Second: "2h 3m 7s",
	}
	for d, want := range cases {
		if got := formatElapsed(d); got != want {
			t.Errorf("formatElapsed(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestHumanTokens_StaysClaudeCodeCompatible checks the "Xk"
// formatting matches Claude Code's "46.0k" (one decimal, always
// 'k' suffix at >=1000). This needs to be visually identical so
// side-by-side comparison reads cleanly.
func TestHumanTokens_StaysClaudeCodeCompatible(t *testing.T) {
	cases := map[int]string{
		1:        "1",
		999:      "999",
		1_000:    "1.0k",
		46_000:   "46.0k",
		460_500:  "460.5k",
		1_500_000: "1.5M",
	}
	for n, want := range cases {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestIsAlreadyDoneHeadline_NegationGuard pins the May-5
// regression where the planner returned "No prior spinner
// animation fix has been implemented" and the headline
// classifier rendered "✓ Already done — nothing to do" because
// the substring "implemented" matched. The model was saying
// the OPPOSITE: nothing has been done. Negation guard kicks in.
func TestIsAlreadyDoneHeadline_NegationGuard(t *testing.T) {
	cases := map[string]bool{
		// Genuine "already done" — fires the headline.
		"already implemented in main.go":             true,
		"the work is already done":                   true,
		"already in place — no changes needed":       true,
		"directory structure is already detailed in the project overview": true,

		// Negated forms that contain the trigger words —
		// must NOT fire the "already done" headline. These
		// were the leak. The model is reporting absence,
		// not presence.
		"No prior spinner animation fix has been implemented": false,
		"No fix done yet":                             false,
		"not yet implemented":                         false,
		"this isn't implemented":                      false,
		"hasn't been done":                            false,
		"never been implemented":                      false,
		"nothing has been done":                       false,
		"not done":                                    false,

		// No trigger words at all → false.
		"the project is a Go CLI": false,
		"":                        false,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := isAlreadyDoneHeadline(strings.ToLower(in)); got != want {
				t.Errorf("isAlreadyDoneHeadline(%q) = %v, want %v", in, got, want)
			}
		})
	}
}

// TestFormatPlan_DiagnosisAndApproachRender pins the May-5 fix:
// the planner now emits a Sherlock-style diagnosis + approach
// before the agent list, and formatPlan must surface them so
// the user reads the "what's wrong + how the fix works"
// narrative BEFORE deciding to confirm "go". Without these the
// plan reads like a commit message.
func TestFormatPlan_DiagnosisAndApproachRender(t *testing.T) {
	plan := &planner.WorkPlan{
		Summary:   "Randomize the spinner glyph each turn.",
		Diagnosis: "The spinner uses spinner.MiniDot exclusively (set in repl.go:621) and never varies. The bubbles library ships several other animation styles but the constructor never picks among them.",
		Approach:  "Add a curated pool of spinner.Spinner values and pick one at random per turn alongside the existing phrase pick.",
		Agents: []planner.AgentTask{
			{Name: "randomize-spinner", Prompt: "Add the pool", Files: []string{"x.go"}},
		},
	}
	out := formatPlan(plan)
	for _, want := range []string{
		"Diagnosis",
		"Approach",
		"spinner.MiniDot exclusively",
		"curated pool of spinner.Spinner",
		"randomize-spinner",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatPlan missing %q in:\n%s", want, out)
		}
	}
	// Diagnosis must appear BEFORE the agent name (narrative
	// flows: problem → fix → work).
	diagIdx := strings.Index(out, "Diagnosis")
	agentIdx := strings.Index(out, "randomize-spinner")
	if diagIdx < 0 || agentIdx < 0 || diagIdx > agentIdx {
		t.Errorf("Diagnosis must precede agent list, got diagIdx=%d agentIdx=%d", diagIdx, agentIdx)
	}
}

// TestFormatPlan_NoDiagnosisOK: plans without diagnosis (the
// "rename X to Y" obvious case where the planner skips it)
// still render cleanly with just summary + agents.
func TestFormatPlan_NoDiagnosisOK(t *testing.T) {
	plan := &planner.WorkPlan{
		Summary: "Rename X to Y.",
		Agents:  []planner.AgentTask{{Name: "rename", Prompt: "do it"}},
	}
	out := formatPlan(plan)
	if strings.Contains(out, "Diagnosis") {
		t.Errorf("empty diagnosis should not render header, got:\n%s", out)
	}
	if !strings.Contains(out, "rename") {
		t.Errorf("agent name missing: %s", out)
	}
}

// TestSummarizeToolCall_ShowsArgs pins the May-5 fix: the
// inline tool trace must show ENOUGH of the call to be
// useful. Before this, kai_grep / kai_tree / kai_files all
// rendered as bare names ("→ kai_grep") so a user watching
// the agent thrash through the same query four times had no
// idea what query was being repeated. Now the summary
// includes the primary arg + any path/glob/regex modifiers
// that change behavior.
func TestSummarizeToolCall_ShowsArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"kai_grep", `{"query":"spinner.New","glob":"*.go"}`, `→ kai_grep "spinner.New" (*.go)`},
		{"kai_grep", `{"query":"\\bspinner\\b","regex":true}`, `→ kai_grep "\bspinner\b" (regex)`},
		{"kai_grep", `{"query":"foo","path":"internal/tui"}`, `→ kai_grep "foo" in internal/tui`},
		{"kai_tree", `{}`, `→ kai_tree .`},
		{"kai_tree", `{"path":"internal/agent"}`, `→ kai_tree internal/agent`},
		{"kai_files", `{"pattern":"**/*.go"}`, `→ kai_files **/*.go`},
		{"kai_symbols", `{"file":"x.go"}`, `→ kai_symbols x.go`},
		{"kai_impact", `{"symbol":"foo"}`, `→ kai_impact foo`},

		// view ranges (added 2026-05-11). A view with offset/limit
		// renders the [start-end] slice so the user can tell a
		// paginated read apart from a cache-miss duplicate. Bare
		// view (no offset/limit) renders without the suffix.
		{"view", `{"file_path":"foo.go"}`, `→ view foo.go`},
		{"view", `{"file_path":"foo.go","offset":100,"limit":50}`, `→ view foo.go [101-150]`},
		{"view", `{"file_path":"foo.go","offset":0,"limit":50}`, `→ view foo.go [1-50]`},
		{"view", `{"file_path":"foo.go","offset":200}`, `→ view foo.go [201-]`},
	}
	for _, c := range cases {
		got := summarizeToolCall(c.name, c.in)
		if got != c.want {
			t.Errorf("summarizeToolCall(%q, %q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestRiskNotesHaveDoubt pins the doubt-phrase detector that
// downgrades "Already done" to "Possibly already done — verify
// before assuming" when the planner's own risk notes admit
// uncertainty about the verdict. Round-18 dogfood shape.
func TestRiskNotesHaveDoubt(t *testing.T) {
	cases := []struct {
		name  string
		notes []string
		want  bool
	}{
		{
			"round-18 exact phrase",
			[]string{"the bug may be in renderPlanMenu() not respecting the flag"},
			true,
		},
		{
			"would need investigation",
			[]string{"a follow-up investigation into renderPlanMenu() would be needed"},
			true,
		},
		{
			"not yet tested",
			[]string{"may have been added very recently and not yet tested"},
			true,
		},
		{
			"clean note",
			[]string{"Confirmed via kai_grep: the flag is read in View()."},
			false,
		},
		{
			"empty",
			nil,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := riskNotesHaveDoubt(tc.notes); got != tc.want {
				t.Errorf("riskNotesHaveDoubt(%v) = %v, want %v", tc.notes, got, tc.want)
			}
		})
	}
}

// TestFormatEmptyPlan_DoubtDowngradesHeadline confirms that the TUI
// renders "Possibly already done — verify before assuming" instead
// of "Already done — nothing to do" when the plan's risk_notes
// contain a doubt phrase. Round-18: the audit reprompt should catch
// this upstream, but if retries exhaust and the plan-with-doubts
// reaches the TUI, the headline must still warn rather than mislead.
func TestFormatEmptyPlan_DoubtDowngradesHeadline(t *testing.T) {
	p := &planner.WorkPlan{
		Summary: "Already implemented at repl.go:1047.",
		RiskNotes: []string{
			"If the user is observing that the second press 'does nothing', the bug may be in renderPlanMenu() not respecting the flag.",
		},
	}
	out := formatEmptyPlan(p)
	if !strings.Contains(out, "Possibly already done") {
		t.Errorf("expected 'Possibly already done' downgrade, got:\n%s", out)
	}
	if strings.Contains(out, "✓ Already done — nothing to do") {
		t.Errorf("doubt-laden plan should NOT render the confident headline:\n%s", out)
	}
}
