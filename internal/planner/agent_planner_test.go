package planner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/safetygate"
)

// fakeProvider mirrors the runner-side fake: returns canned Responses
// in queue order and records the last request so tests can assert on
// what the model was shown. Keeps tests provider-only — no SQLite, no
// HTTP, no real LLM call.
type fakeProvider struct {
	queue []provider.Response
	calls int32
	last  provider.Request
}

func (f *fakeProvider) Send(_ context.Context, req provider.Request) (provider.Response, error) {
	atomic.AddInt32(&f.calls, 1)
	f.last = req
	if len(f.queue) == 0 {
		return provider.Response{}, errors.New("fakeProvider: queue empty")
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}

// TestPlannerAgent_ExploresThenPlans drives the canonical scenario:
// the model first calls a read-only tool to look at the codebase, then
// emits a JSON plan as its final assistant message. Verifies the plan
// is parsed and returned, and that the agent loop actually dispatched
// the tool call (not just took the model's word for what files exist).
func TestPlannerAgent_ExploresThenPlans(t *testing.T) {
	ws := t.TempDir()
	// Touch a file the model will pretend to look at via kai_files.
	if err := os.WriteFile(filepath.Join(ws, "real.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{queue: []provider.Response{
		{
			// Turn 1: model calls kai_files to verify what's in the workspace.
			Parts: []message.ContentPart{
				message.ToolCall{
					ID: "call-1", Name: "kai_files", Input: `{}`, Type: "tool_use",
				},
			},
			FinishReason: message.FinishReasonToolUse,
		},
		{
			// Turn 2: model emits the JSON plan as its final response.
			// The plan references the real file we touched above — that's
			// the whole point of the dogfooding: paths come from the
			// model's own exploration, not from hallucination.
			Parts: []message.ContentPart{
				message.TextContent{Text: "Here's the plan:\n\n```json\n" +
					`{"summary":"add a comment to real.go","agents":[` +
					`{"name":"commenter","prompt":"add a header comment","files":["real.go"]}` +
					`]}` + "\n```"},
			},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}

	pa := &PlannerAgent{
		Provider:   p,
		Set:        projects.Single(ws),
		GateConfig: safetygate.DefaultConfig(),
		Cfg:        Config{MaxAgents: 5},
	}

	res, err := pa.Run(context.Background(), "add a comment to real.go", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Plan == nil {
		t.Fatal("res.Plan = nil")
	}
	if res.Plan.Summary != "add a comment to real.go" {
		t.Errorf("Summary = %q", res.Plan.Summary)
	}
	if len(res.Plan.Agents) != 1 || res.Plan.Agents[0].Name != "commenter" {
		t.Errorf("Agents = %+v", res.Plan.Agents)
	}
	if res.Plan.Agents[0].Files[0] != "real.go" {
		t.Errorf("expected real.go in files, got %v", res.Plan.Agents[0].Files)
	}
	// Two provider calls means the loop actually dispatched the tool —
	// if the planner had skipped the tool and gone straight to JSON,
	// we'd see a single call. The whole point of moving to an agent
	// loop is to make this round-trip happen.
	if p.calls != 2 {
		t.Errorf("expected 2 provider calls (explore + plan), got %d", p.calls)
	}
}

// TestPlannerAgent_TooVagueReturnsErr covers the "model decided this
// isn't actionable AND has nothing useful to say" path: the JSON
// plan has no agents AND no useful content (empty summary, no
// risk_notes). Only THIS shape routes through ErrTooVague to the
// chat fallback. A plan with content but no agents is a real
// answer (see TestPlannerAgent_EmptyAgentsWithContentReturnsPlan).
func TestPlannerAgent_TooVagueReturnsErr(t *testing.T) {
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				// Empty summary, no risk_notes, no agents — model
				// produced literally nothing useful. ErrTooVague
				// is the right call: the chat fallback at least
				// lets the user TRY rephrasing.
				message.TextContent{Text: "```json\n" +
					`{"summary":"","agents":[],"risk_notes":[]}` + "\n```"},
			},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}

	pa := &PlannerAgent{
		Provider:   p,
		Set:        projects.Single(ws),
		GateConfig: safetygate.DefaultConfig(),
		Cfg:        Config{MaxAgents: 5},
	}

	res, err := pa.Run(context.Background(), "do the thing", "")
	if !errors.Is(err, ErrTooVague) {
		t.Fatalf("err = %v, want ErrTooVague", err)
	}
	if res == nil {
		t.Fatal("res should be non-nil even on ErrTooVague (carries token usage)")
	}
}

// TestPlannerAgent_VagueSummaryRoutesToErrTooVague pins the
// May-5 follow-up: even when the model produces non-empty
// summary + risk_notes, if the summary itself signals "I can't
// plan this — too vague" the result must route to ErrTooVague.
// Otherwise greetings like "hi" render as a "Couldn't plan
// this" headline instead of getting a friendly chat reply.
//
// The risk_notes in this case ("No concrete file...") are just
// the model elaborating WHY it can't plan, not a real answer —
// they shouldn't promote the result to the Plan path.
func TestPlannerAgent_VagueSummaryRoutesToErrTooVague(t *testing.T) {
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "```json\n" +
					`{"summary":"Request is too vague to plan","agents":[],"risk_notes":["No concrete file, feature, or behavior specified.","Unable to plan any work."]}` +
					"\n```"},
			},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}

	pa := &PlannerAgent{
		Provider:   p,
		Set:        projects.Single(ws),
		GateConfig: safetygate.DefaultConfig(),
		Cfg:        Config{MaxAgents: 5},
	}

	_, err := pa.Run(context.Background(), "hi", "")
	if !errors.Is(err, ErrTooVague) {
		t.Fatalf("err = %v, want ErrTooVague (vague summary should bypass the Plan path even with non-empty risk_notes)", err)
	}
}

// TestIsVagueRefusal_Markers pins the marker list directly so a
// future "let me also accept X" change can't silently weaken the
// classifier and let actual answers (with summary like "the
// project is unclear about routing logic") get rerouted to chat.
func TestIsVagueRefusal_Markers(t *testing.T) {
	cases := map[string]bool{
		"Request is too vague to plan":                  true,
		"I can't plan this without more info":           true,
		"Cannot plan — no concrete files specified":     true,
		"Unable to plan any work":                       true,
		"Insufficient context to plan":                  true,
		"Unclear what changes are needed":               true,
		"":                                              false,
		"Add a /health endpoint to server.go":           false,
		"Already implemented in main.go":                false,
		"Directory structure is in the project overview": false,
	}
	for in, want := range cases {
		if got := isVagueRefusal(in); got != want {
			t.Errorf("isVagueRefusal(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestPlannerAgent_EmptyAgentsWithContentReturnsPlan pins the
// May-5 BYOM-OpenAI fix: when the model returns a WorkPlan with
// no agents BUT non-empty summary or risk_notes, route as a Plan
// (so the REPL renders summary + notes via formatEmptyPlan)
// instead of routing to chat fallback.
//
// gpt-4o specifically tends to answer questions like "what's
// here?" with summaries like "Directory structure is already
// detailed in the project overview" + a list of useful
// risk_notes. Routing those through ErrTooVague on the old
// isAlreadyDoneSummary marker check produced a chat-fallback
// reply that pattern-matched the JSON shape from prior turns
// and re-emitted JSON to the user. Plan path renders cleanly.
func TestPlannerAgent_EmptyAgentsWithContentReturnsPlan(t *testing.T) {
	ws := t.TempDir()
	p := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.TextContent{Text: "```json\n" +
					`{"summary":"Directory structure is already detailed in the project overview.","agents":[],"risk_notes":["Top-level dirs: kai-cli, kai-core","README focuses on semantic infrastructure"]}` +
					"\n```"},
			},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}

	pa := &PlannerAgent{
		Provider:   p,
		Set:        projects.Single(ws),
		GateConfig: safetygate.DefaultConfig(),
		Cfg:        Config{MaxAgents: 5},
	}

	res, err := pa.Run(context.Background(), "what's here?", "")
	if err != nil {
		t.Fatalf("err = %v, want nil (empty agents with content is a real answer)", err)
	}
	if res == nil || res.Plan == nil {
		t.Fatal("expected Plan to be set so REPL renders via formatEmptyPlan")
	}
	if len(res.Plan.RiskNotes) != 2 {
		t.Errorf("expected risk_notes preserved on Plan, got %d", len(res.Plan.RiskNotes))
	}
	if res.Plan.Summary == "" {
		t.Error("expected summary preserved on Plan")
	}
}

// TestExtractFencedJSON_Variants covers the parser's tolerance for
// the model's output shape: ideally the model emits a ```json fence,
// but real LLMs wander between fences, no fences, and trailing prose.
// We accept all three rather than failing the user's plan because of
// minor format drift.
func TestExtractFencedJSON_Variants(t *testing.T) {
	cases := map[string]string{
		"json fence":  "intro prose\n```json\n{\"a\":1}\n```\nouttro",
		"plain fence": "```\n{\"a\":1}\n```",
		"bare object": "Here's the plan: {\"a\":1} done.",
	}
	for name, in := range cases {
		got, err := extractFencedJSON(in)
		if err != nil {
			t.Errorf("%s: err = %v", name, err)
			continue
		}
		if got != `{"a":1}` {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

// TestExtractFencedJSON_NoJSON returns an error when the model
// emits prose with no JSON at all — surfaced to the REPL so the
// user sees the raw text rather than a silent failure.
func TestExtractFencedJSON_NoJSON(t *testing.T) {
	if _, err := extractFencedJSON("just some prose, no json here"); err == nil {
		t.Error("expected error for no-json input")
	}
}

// TestExtractFencedJSON_BracesInsideStrings pins that braces inside
// JSON string values do NOT throw off the brace counter. The
// previous implementation counted ALL '{'/'}' characters and would
// truncate this output mid-object — failing json.Unmarshal and
// surfacing as raw text in the REPL. May 2026 regression.
func TestExtractFencedJSON_BracesInsideStrings(t *testing.T) {
	in := `prose before
{"summary": "fix struct {Foo string} usage", "agents": [{"name": "x", "prompt": "edit { line 12 }"}]}
prose after`
	got, err := extractFencedJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `{"summary": "fix struct {Foo string} usage", "agents": [{"name": "x", "prompt": "edit { line 12 }"}]}`
	if got != want {
		t.Errorf("brace-in-string broke balance:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestExtractFencedJSON_FencesInsideStrings pins the 2026-05-12
// regression: when the model emits raw JSON (no outer fence) whose
// string values CONTAIN markdown code-fences (e.g. an agent prompt
// quoting Go source with ```go ... ```), the generic-fence branch
// would eagerly match the first inner ``` and return a non-JSON
// fragment. The user saw raw JSON dumped to the REPL because the
// plan failed to parse downstream. Generic fence now validates the
// body as JSON before returning, so non-JSON fences fall through to
// the bare-object scan.
func TestExtractFencedJSON_FencesInsideStrings(t *testing.T) {
	in := "{\"summary\":\"fix\",\"agents\":[{\"name\":\"x\",\"prompt\":\"replace:\\n\\n```go\\nold()\\n```\\n\\nwith:\\n\\n```go\\nnew()\\n```\"}]}"
	got, err := extractFencedJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != in {
		t.Errorf("inner ```go fence broke extraction:\n  got:  %q\n  want: %q", got, in)
	}
}

// TestExtractFencedJSON_OuterJSONFenceWithInnerGoFence reproduces the
// May-2026 bbolt stall: the model wraps its plan in a ```json fence
// AND the agent prompt embeds a ```go code example. The ```json path
// must not match the inner ```go as its closing fence (which truncates
// the JSON and demotes the whole plan to raw REPL text). It must
// validate the candidate and fall through to the brace-scanner.
func TestExtractFencedJSON_OuterJSONFenceWithInnerGoFence(t *testing.T) {
	plan := "{\"summary\":\"fix\",\"agents\":[{\"name\":\"x\",\"prompt\":\"do this:\\n```go\\nlock()\\n```\\nthen build\"}]}"
	in := "[finalize] ```json\n" + plan + "\n```"
	got, err := extractFencedJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("extracted candidate is not valid JSON:\n%s", got)
	}
	if got != plan {
		t.Errorf("inner ```go truncated the ```json extraction:\n  got:  %q\n  want: %q", got, plan)
	}
}

// TestExtractFencedJSON_PicksLastObject pins that when the model
// emits illustrative JSON before its final plan, we return the
// LAST balanced object. The earlier implementation took the first
// match and lost the actual plan to an example block above it.
func TestExtractFencedJSON_PicksLastObject(t *testing.T) {
	in := `Here's an example of the format:
{"example": true}

Now here's the actual plan:
{"summary": "do thing", "agents": []}`
	got, err := extractFencedJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `{"summary": "do thing", "agents": []}`
	if got != want {
		t.Errorf("expected last object, got first:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestExtractFencedJSON_EscapedQuotesInsideStrings pins that an
// escaped quote inside a string doesn't terminate the string and
// expose the next character to brace-counting.
func TestExtractFencedJSON_EscapedQuotesInsideStrings(t *testing.T) {
	in := `{"a": "she said \"hello {world}\" loudly", "b": 1}`
	got, err := extractFencedJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != in {
		t.Errorf("escaped quote handling broken:\n  got:  %q\n  want: %q", got, in)
	}
}

// TestExtractFencedJSON_FencePreferredOverBareObject pins that the
// ```json fence still wins over a bare {...} elsewhere in the
// response. This matters because the prompt asks for the fence and
// the bare-object scan is a tolerance net for misbehaving models.
func TestExtractFencedJSON_FencePreferredOverBareObject(t *testing.T) {
	in := "here's some text mentioning {wrong: \"object\"}\n\n```json\n{\"right\":1}\n```"
	got, err := extractFencedJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != `{"right":1}` {
		t.Errorf("fence not preferred: got %q", got)
	}
}

// TestModelSupportsStructuredOutputs pins the gate for which models
// get Anthropic's `output_config` attached. Only Claude 4.x family
// is documented as GA (2026-05). Together / OpenAI ids must return
// false so we don't ship an Anthropic-only field down a wire that
// rejects it. claude-3-* is left out because the docs don't list it
// — be conservative.
func TestModelSupportsStructuredOutputs(t *testing.T) {
	cases := map[string]bool{
		"claude-opus-4-7":            true,
		"claude-opus-4-6":            true,
		"claude-sonnet-4-6":          true,
		"claude-haiku-4-5-20251001":  true,
		"claude-3-5-sonnet":          false, // pre-4 family — not documented
		"gpt-4o":                     false,
		"Qwen/Qwen3-Coder-Next-FP8":  false,
		"moonshotai/Kimi-K2.6":       false,
		"deepseek-ai/DeepSeek-V4-Pro": false,
		"":                           false,
	}
	for model, want := range cases {
		got := modelSupportsStructuredOutputs(model)
		if got != want {
			t.Errorf("model %q: got %v, want %v", model, got, want)
		}
	}
}

// TestWorkPlanJSONSchema_AnthropicGrammarCompliance pins the schema-
// shape rules Anthropic enforces: every object declares
// additionalProperties: false, and every property must appear in
// required. A future edit that adds an optional field would silently
// regress this — Anthropic's grammar compiler would 400 the request
// at the first call. Cheap check, big payoff.
func TestWorkPlanJSONSchema_AnthropicGrammarCompliance(t *testing.T) {
	s := workPlanJSONSchema()
	checkObject(t, "WorkPlan", s)
}

func checkObject(t *testing.T, label string, obj map[string]interface{}) {
	t.Helper()
	if obj["type"] != "object" {
		return // not an object node — nothing to check at this level
	}
	if obj["additionalProperties"] != false {
		t.Errorf("%s: missing additionalProperties:false", label)
	}
	props, _ := obj["properties"].(map[string]interface{})
	required, _ := obj["required"].([]string)
	requiredSet := make(map[string]bool, len(required))
	for _, r := range required {
		requiredSet[r] = true
	}
	for name, child := range props {
		if !requiredSet[name] {
			t.Errorf("%s.%s: property not in required list (Anthropic structured outputs forbid optionals)", label, name)
		}
		childMap, ok := child.(map[string]interface{})
		if !ok {
			continue
		}
		checkObject(t, label+"."+name, childMap)
		// Recurse into array items.
		if items, ok := childMap["items"].(map[string]interface{}); ok {
			checkObject(t, label+"."+name+"[]", items)
		}
	}
}

// --- already-done audit tests (round-18 motivation) -----------------

// TestAuditAlreadyDone_RoundEighteenShape pins the exact failure mode
// the audit exists to catch: planner returns "already implemented"
// while ALSO emitting a risk_note saying "the bug may be in
// renderPlanMenu" — without ever having queried renderPlanMenu.
func TestAuditAlreadyDone_RoundEighteenShape(t *testing.T) {
	plan := &WorkPlan{
		Summary: "Already implemented: the `?` handler at repl.go:1047 already toggles `planDetailsExpanded` and re-renders via `renderPlanMenu()`.",
		Diagnosis: "The handler reads `r.planDetailsExpanded = !r.planDetailsExpanded` (line 1047), then calls `r.renderPlanMenu()` (line 1048) to re-render.",
		RiskNotes: []string{
			"If the user is observing that the second press 'does nothing', the bug may be in `renderPlanMenu()` not respecting the `planDetailsExpanded` flag when rendering.",
		},
	}
	// Transcript queries planDetailsExpanded (via kai_grep) and
	// formatPlanDetails — but NOT renderPlanMenu. Mimics round-18.
	transcript := []message.Message{
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{
				message.ToolCall{Name: "kai_grep", Input: `{"query":"planDetailsExpanded"}`, Type: "tool_use"},
				message.ToolCall{Name: "kai_grep", Input: `{"query":"formatPlanDetails"}`, Type: "tool_use"},
			},
		},
	}
	unverified, doubt := auditAlreadyDone(plan, transcript)
	if !doubt {
		t.Errorf("doubt=false; the risk note 'the bug may be in renderPlanMenu' must trip the doubt detector")
	}
	gotRender := false
	for _, s := range unverified {
		if s == "renderPlanMenu" {
			gotRender = true
		}
	}
	if !gotRender {
		t.Errorf("unverified missing renderPlanMenu: got %v", unverified)
	}
}

func TestAuditAlreadyDone_ClearPasses(t *testing.T) {
	plan := &WorkPlan{
		Summary: "Already implemented: handler is wired and renderer respects the flag.",
		Diagnosis: "The `?` handler at line 1047 toggles `planDetailsExpanded`; the View() function reads `planDetailsExpanded` to render details.",
		RiskNotes: []string{
			"Confirmed via kai_grep on both symbols.",
		},
	}
	transcript := []message.Message{
		{
			Role: message.RoleAssistant,
			Parts: []message.ContentPart{
				message.ToolCall{Name: "kai_grep", Input: `{"query":"planDetailsExpanded"}`, Type: "tool_use"},
			},
		},
	}
	unverified, doubt := auditAlreadyDone(plan, transcript)
	if len(unverified) > 0 {
		t.Errorf("expected no unverified symbols, got %v", unverified)
	}
	if doubt {
		t.Errorf("clean risk_notes should not trip doubt, got true")
	}
}

func TestAuditAlreadyDone_DoubtAloneIsEnough(t *testing.T) {
	// No unverified symbols, but the risk note itself signals doubt.
	// That's a sufficient signal on its own — the planner is telling
	// us the verdict is incomplete.
	plan := &WorkPlan{
		Summary: "Already implemented.",
		Diagnosis: "",
		RiskNotes: []string{
			"Not yet tested visually.",
		},
	}
	unverified, doubt := auditAlreadyDone(plan, nil)
	if !doubt {
		t.Errorf("'Not yet tested' should trip the doubt detector")
	}
	if len(unverified) != 0 {
		t.Errorf("no symbols cited, unverified should be empty: %v", unverified)
	}
}

func TestExtractCitedSymbols_BackticksOnly(t *testing.T) {
	plan := &WorkPlan{
		Diagnosis: "The function `renderPlanMenu` does X, also `formatPlanDetails`.",
		RiskNotes: []string{"`planDetailsExpanded` is the flag."},
	}
	got := extractCitedSymbols(plan)
	want := map[string]bool{
		"renderPlanMenu":      true,
		"formatPlanDetails":   true,
		"planDetailsExpanded": true,
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected symbol %q in %v", s, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d symbols (%v), want %d", len(got), got, len(want))
	}
}

func TestExtractCitedSymbols_IgnoresPlainProse(t *testing.T) {
	// English words inside backticks (no camelCase, no underscore)
	// must NOT be extracted as code symbols.
	plan := &WorkPlan{
		Diagnosis: "The `details` should `appear` and `hide`.",
	}
	if got := extractCitedSymbols(plan); len(got) > 0 {
		t.Errorf("plain English in backticks should yield no symbols, got %v", got)
	}
}

func TestValidatePlan_AlreadyDoneWithUnverifiedTriggersReprompt(t *testing.T) {
	// End-to-end through validatePlan: an "already done" verdict
	// with an unverified cited symbol must downgrade to the verify
	// reprompt rather than passing through to planAcceptAsEmpty.
	plan := &WorkPlan{
		Summary:   "Already implemented at repl.go:1047.",
		Diagnosis: "Handler calls `renderPlanMenu` after toggling.",
	}
	transcript := []message.Message{
		// No kai_grep / view for renderPlanMenu.
		{Role: message.RoleAssistant, Parts: []message.ContentPart{
			message.ToolCall{Name: "kai_grep", Input: `{"query":"unrelated"}`, Type: "tool_use"},
		}},
	}
	v := validatePlan(plan, transcript, "toggle the ? key")
	if v.Action != planReprompt {
		t.Fatalf("expected planReprompt, got Action=%v", v.Action)
	}
	if v.Retry != repromptAlreadyDoneVerify {
		t.Errorf("expected repromptAlreadyDoneVerify, got %v", v.Retry)
	}
	if len(v.UnverifiedSymbols) == 0 {
		t.Errorf("expected unverified symbols to be passed forward, got empty")
	}
}

// TestPlannerFinalizeConstrainsOutput pins which models get a
// schema-constrained finalization turn. Non-Anthropic models (Together
// — DeepSeek, Kimi) and Anthropic 4.x get the schema; legacy claude-3-*
// does not. Kimi-K2.6 must be in the constrained set — without it the
// planner's finalize turn returns prose and hands-off dead-ends.
func TestPlannerFinalizeConstrainsOutput(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"moonshotai/Kimi-K2.6", true},
		{"zai-org/GLM-5.1", true},  // kai-rename-keep — GLM-5.1 still supported, tests structured-output path
		{"deepseek-ai/DeepSeek-V4-Pro", true},
		{"claude-opus-4-6", true},
		{"claude-sonnet-4-6", true},
		{"claude-3-5-sonnet", false},
	}
	for _, c := range cases {
		if got := plannerFinalizeConstrainsOutput(c.model); got != c.want {
			t.Errorf("plannerFinalizeConstrainsOutput(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}
