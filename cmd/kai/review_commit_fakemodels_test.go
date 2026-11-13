package main

// A fake model server for whole grounded reviews (review-commit --deep): it
// speaks the OpenAI chat shape the review's provider uses, tells each review
// step apart by its request, answers each the way a healthy model would, and
// can make any step fail (an empty reply, or a 5xx) on any model. Every
// request it receives is recorded, so a test can assert on exactly which
// models a review asked for.
//
// The change under review seeds two bugs: Add subtracts, and Total skips the
// first element. A review that is correct finds both; one that does not must
// say it is partial or incomplete.
//
// This file uses only what main already has, so the same harness can record
// today's calls and bundle (testdata/review_parity_main.json) and the branch
// can be checked against them.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	rcFakeBugFile   = "calc/calc.go"
	rcFakeBugLine   = "return a - b"
	rcFakeBugIssue  = "calc/calc.go:5 — Add subtracts b instead of adding it, so every total built on it is wrong (severity: high)"
	rcFakeBug2Line  = "range xs[1:]"
	rcFakeBug2Issue = "calc/total.go:6 — Total ranges over xs[1:], so the first element is never added (severity: high)"
)

// rcFakeBugs maps each seeded bug's issue to the line that shows it.
var rcFakeBugs = map[string]string{rcFakeBugIssue: rcFakeBugLine, rcFakeBug2Issue: rcFakeBug2Line}

// Review steps as the fake server tells them apart.
const (
	rcFakeIntent      = "intent"
	rcFakeDescription = "description"
	rcFakeAgent       = "agent"
	rcFakeConclusion  = "conclusion"
	rcFakeSweep       = "sweep"
	rcFakeFactcheck   = "factcheck"
	rcFakeRank        = "rank"
	rcFakeGroup       = "group"
	rcFakeHumanize    = "humanize"
	rcFakeSummary     = "summary"
	rcFakeUnknown     = "unknown"
)

// Faults.
const (
	rcFaultNone  = ""
	rcFaultEmpty = "empty" // a 200 with no content
	rcFault5xx   = "5xx"   // a 503
)

// rcFakeCall is one request the server received.
type rcFakeCall struct {
	Step  string `json:"step"`
	Model string `json:"model"`
	// Tools and Messages describe the request's shape, for the parity check.
	Tools    int    `json:"tools"`
	Messages int    `json:"messages"`
	Fault    string `json:"fault,omitempty"`
	// Prompt is the request's last user text with the run's paths removed:
	// a change of wording shows up in the parity check.
	Prompt string `json:"prompt"`
}

// rcFakeScenario is how the models behave.
type rcFakeScenario struct {
	// Main is every finder agent: "review" reads both files and writes a
	// review naming the bugs; "loop" reads until its turn cap; "prose" reads
	// and answers with prose and no verdict block; "dry" answers nothing.
	Main string
	// AgentFor overrides Main for the finder on one model.
	AgentFor map[string]string
	// Conclusion answers a conclusion call: "review" (the default), "stray"
	// (tool-call syntax as text) or "empty".
	Conclusion string
	// Sweep answers a sweep chunk: "bug" (the default) or "none".
	Sweep string
	// Fault decides each request's fault; nil is none.
	Fault func(step, model string, rng *rand.Rand) string
	// Summarise reports nearly full context windows on agent turns, so the
	// engine summarises the conversation.
	Summarise bool
	// Seed seeds the fault draws.
	Seed int64
}

type rcFakeModelServer struct {
	t        *testing.T
	srv      *httptest.Server
	scenario rcFakeScenario
	// root is the reviewed repository; it is written <ROOT> in a recorded
	// prompt, in its own spelling and with symlinks resolved.
	root string

	mu    sync.Mutex
	calls []rcFakeCall
	rng   *rand.Rand
}

func newRcFakeModelServer(t *testing.T, sc rcFakeScenario) *rcFakeModelServer {
	t.Helper()
	f := &rcFakeModelServer{t: t, scenario: sc, rng: rand.New(rand.NewSource(sc.Seed))}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// Calls is every request received, in order.
func (f *rcFakeModelServer) Calls() []rcFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rcFakeCall(nil), f.calls...)
}

// Models is every distinct model asked for.
func (f *rcFakeModelServer) Models() map[string]bool {
	out := map[string]bool{}
	for _, c := range f.Calls() {
		out[c.Model] = true
	}
	return out
}

type rcFakeRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// rcFakeText is a message's text, whether content is a string or parts.
func rcFakeText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func rcFakePrefix(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func (f *rcFakeModelServer) classify(req *rcFakeRequest) string {
	system := ""
	for _, m := range req.Messages {
		if m.Role == "system" {
			system += rcFakeText(m.Content)
		}
	}
	for _, tl := range req.Tools {
		if tl.Function.Name == "submit_review" {
			return rcFakeFactcheck
		}
	}
	switch {
	case strings.Contains(system, "terse, factual summarizer"):
		return rcFakeSummary
	case strings.Contains(system, rcFakePrefix(rcInferIntentSystem)):
		return rcFakeIntent
	case strings.Contains(system, rcFakePrefix(rcDescriptionSystem)):
		return rcFakeDescription
	case strings.Contains(system, rcFakePrefix(rcSweepSystem)):
		return rcFakeSweep
	case strings.Contains(system, rcFakePrefix(rcRankSystem)):
		return rcFakeRank
	case strings.Contains(system, rcFakePrefix(rcGroupSystem)):
		return rcFakeGroup
	case strings.Contains(system, rcFakePrefix(rcHumanizeSystem)):
		return rcFakeHumanize
	case strings.Contains(system, rcFakePrefix(rcReviewSystem)) && len(req.Tools) > 0:
		return rcFakeAgent
	case strings.Contains(system, rcFakePrefix(rcReviewSystem)):
		return rcFakeConclusion
	}
	return rcFakeUnknown
}

func (f *rcFakeModelServer) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req rcFakeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	step := f.classify(&req)
	lastUser := ""
	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "user" {
			lastUser = rcFakeText(m.Content)
		}
		if m.Role == "tool" {
			toolResults++
		}
	}
	f.mu.Lock()
	fault := rcFaultNone
	if f.scenario.Fault != nil {
		fault = f.scenario.Fault(step, req.Model, f.rng)
	}
	prompt := lastUser
	if f.root != "" {
		if real, err := filepath.EvalSymlinks(f.root); err == nil {
			prompt = strings.ReplaceAll(prompt, real, "<ROOT>")
		}
		prompt = strings.ReplaceAll(prompt, f.root, "<ROOT>")
	}
	f.calls = append(f.calls, rcFakeCall{Step: step, Model: req.Model, Tools: len(req.Tools), Messages: len(req.Messages), Fault: fault, Prompt: prompt})
	f.mu.Unlock()

	if fault == rcFault5xx {
		http.Error(w, `{"error":{"message":"upstream overloaded"}}`, http.StatusServiceUnavailable)
		return
	}
	var text string
	var call *rcFakeToolCall
	usage := 10
	if fault != rcFaultEmpty {
		text, call = f.answer(step, &req, lastUser, toolResults)
		if step == rcFakeAgent && f.scenario.Summarise {
			usage = 950_000 // past the summarising threshold even on a 1M-token window
		}
	}
	f.write(w, req.Stream, text, call, usage)
}

type rcFakeToolCall struct {
	Name string
	Args string
}

func (f *rcFakeModelServer) answer(step string, req *rcFakeRequest, lastUser string, toolResults int) (string, *rcFakeToolCall) {
	switch step {
	case rcFakeIntent:
		return "Adds two numbers for the calculator's totals.", nil
	case rcFakeDescription:
		return "Changes how Add combines its two arguments.", nil
	case rcFakeSummary:
		return "The reviewer read calc/calc.go and calc/total.go.", nil
	case rcFakeSweep:
		if f.scenario.Sweep == "none" {
			return "ISSUES:\n- (none)\n", nil
		}
		return "ISSUES:\n- " + rcFakeBugIssue + "\n- " + rcFakeBug2Issue + "\n", nil
	case rcFakeConclusion:
		switch f.scenario.Conclusion {
		case "stray":
			return "<tool_call>kai_view calc/calc.go</arg_value></tool_call>", nil
		case "empty":
			return "", nil
		}
		return rcFakeReview(), nil
	case rcFakeFactcheck:
		return "", &rcFakeToolCall{Name: "submit_review", Args: rcFakeFactcheckAnswer(lastUser, req)}
	case rcFakeRank:
		ids := []int{}
		for _, m := range rcFakeRankID.FindAllStringSubmatch(lastUser, -1) {
			var id int
			fmt.Sscanf(m[1], "%d", &id)
			ids = append(ids, id)
		}
		b, _ := json.Marshal(map[string]any{"publish": ids, "minor": []int{}, "skip": []any{}})
		return string(b), nil
	case rcFakeGroup:
		return "none", nil
	case rcFakeHumanize:
		var found []map[string]any
		for _, m := range rcFakeHumanizeID.FindAllStringSubmatch(lastUser, -1) {
			var id int
			fmt.Sscanf(m[1], "%d", &id)
			found = append(found, map[string]any{"id": id, "statement": "The changed line breaks the arithmetic.",
				"title": "Arithmetic is wrong", "plain": "The result is wrong.", "when": "", "impact": "", "fix": ""})
		}
		b, _ := json.Marshal(map[string]any{"findings": found})
		return string(b), nil
	case rcFakeAgent:
		files := []string{rcFakeBugFile, "calc/total.go"}
		behaviour := f.scenario.Main
		if b, ok := f.scenario.AgentFor[req.Model]; ok {
			behaviour = b
		}
		switch behaviour {
		case "dry":
			return "", nil
		case "loop":
			return "", &rcFakeToolCall{Name: "kai_view", Args: fmt.Sprintf(`{"file_path":%q,"offset":%d}`, files[toolResults%2], toolResults+1)}
		}
		reads := len(files)
		if f.scenario.Summarise {
			reads = 10 // enough history for the engine to have something to summarise
		}
		if toolResults < len(files) {
			return "", &rcFakeToolCall{Name: "kai_view", Args: fmt.Sprintf(`{"file_path":%q}`, files[toolResults])}
		}
		if toolResults < reads {
			return "", &rcFakeToolCall{Name: "kai_view", Args: fmt.Sprintf(`{"file_path":%q,"offset":%d}`, files[toolResults%2], toolResults)}
		}
		if behaviour == "prose" {
			return "I read both files. The change looks reasonable.", nil
		}
		return rcFakeReview(), nil
	}
	return "ok", nil
}

// rcFakeReview is a healthy review of the seeded bug.
func rcFakeReview() string {
	return "Add now subtracts its second argument, so every total is wrong.\n\n" + rcReviewDataMarker +
		"\nINTENT_MATCH: partial\nMERGE_READY: 2\nSUMMARY: Add subtracts and Total skips an element.\nISSUES:\n- " + rcFakeBugIssue + "\n- " + rcFakeBug2Issue + "\nDECISIONS:\n"
}

var (
	rcFakeSourceHead = regexp.MustCompile(`^SOURCE (\d+) \(\d+ rows?; cite the ROW numbers`)
	rcFakeRow        = regexp.MustCompile(`^\s*(\d+)\| (.*)$`)
	rcFakeRankID     = regexp.MustCompile(`(?m)^\[(\d+)\] `)
	rcFakeHumanizeID = regexp.MustCompile(`(?m)^\[(\d+)\]$`)
)

// rcFakeFactcheckAnswer checks every issue the way a careful model would: the
// seeded bug is supported, citing the row that shows it, and anything else is
// refuted on the same row.
func rcFakeFactcheckAnswer(user string, req *rcFakeRequest) string {
	// The first user message carries the sources and the issues.
	for _, m := range req.Messages {
		if m.Role == "user" {
			user = rcFakeText(m.Content)
			break
		}
	}
	type at struct{ source, row int }
	where := map[string]at{} // bug line → first row showing it
	cur := 0
	for _, ln := range strings.Split(user, "\n") {
		if m := rcFakeSourceHead.FindStringSubmatch(ln); m != nil {
			fmt.Sscanf(m[1], "%d", &cur)
			continue
		}
		m := rcFakeRow.FindStringSubmatch(ln)
		if m == nil || cur == 0 {
			continue
		}
		for _, line := range rcFakeBugs {
			if _, seen := where[line]; !seen && strings.Contains(m[2], line) {
				var row int
				fmt.Sscanf(m[1], "%d", &row)
				where[line] = at{cur, row}
			}
		}
	}
	var issues []string
	if i := strings.Index(user, "ISSUES TO CHECK:\n"); i >= 0 {
		for _, ln := range strings.Split(user[i+len("ISSUES TO CHECK:\n"):], "\n") {
			if !strings.HasPrefix(ln, "- ") {
				break
			}
			issues = append(issues, strings.TrimPrefix(ln, "- "))
		}
	}
	type ev struct {
		Source    int `json:"source"`
		LineStart int `json:"line_start"`
		LineEnd   int `json:"line_end"`
	}
	type check struct {
		Issue      string `json:"issue"`
		Verdict    string `json:"verdict"`
		Reason     string `json:"reason"`
		Finding    string `json:"finding"`
		Remedy     string `json:"remedy"`
		Evidence   []ev   `json:"evidence"`
		Confidence int    `json:"confidence"`
		Impact     string `json:"impact"`
	}
	ans := struct {
		Scope       []string `json:"scope"`
		Limitations []string `json:"limitations"`
		IntentMatch string   `json:"intent_match"`
		MergeReady  int      `json:"merge_ready"`
		Checks      []check  `json:"checks"`
		Decisions   []any    `json:"decisions"`
	}{Scope: []string{"the supplied sources"}, Limitations: []string{}, IntentMatch: "partial", MergeReady: 2, Decisions: []any{}}
	// Refuted issues cite the earliest row found, so the answer is the same
	// on every run.
	any1 := at{}
	for _, a := range where {
		if any1.source == 0 || a.source < any1.source || (a.source == any1.source && a.row < any1.row) {
			any1 = a
		}
	}
	for _, is := range issues {
		c := check{Issue: is, Confidence: 9, Impact: "high"}
		bug := ""
		for issue, line := range rcFakeBugs {
			if strings.HasPrefix(is, issue[:strings.Index(issue, " — ")]) {
				bug = line
			}
		}
		if a, ok := where[bug]; ok && bug != "" {
			c.Verdict, c.Reason = "supported", "The changed line shows it."
			c.Finding, c.Remedy = "The changed line computes the wrong value.", "Restore the original line."
			c.Evidence = []ev{{Source: a.source, LineStart: a.row, LineEnd: a.row}}
		} else {
			c.Verdict, c.Reason = "refuted", "The cited line does not show this."
			c.Evidence = []ev{{Source: any1.source, LineStart: any1.row, LineEnd: any1.row}}
		}
		ans.Checks = append(ans.Checks, c)
	}
	b, _ := json.Marshal(ans)
	return string(b)
}

// write answers in the request's own shape: a JSON body, or an SSE stream.
func (f *rcFakeModelServer) write(w http.ResponseWriter, stream bool, text string, call *rcFakeToolCall, promptTokens int) {
	finish := "stop"
	var toolCalls []map[string]any
	if call != nil {
		finish = "tool_calls"
		toolCalls = []map[string]any{{"index": 0, "id": fmt.Sprintf("call_%d", len(f.Calls())), "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Args}}}
	}
	usage := map[string]any{"prompt_tokens": promptTokens, "completion_tokens": 5, "total_tokens": promptTokens + 5}
	if !stream {
		msg := map[string]any{"role": "assistant", "content": text}
		if toolCalls != nil {
			msg["tool_calls"] = toolCalls
		}
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": msg}},
			"usage":   usage,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	var b bytes.Buffer
	chunk := func(v any) {
		j, _ := json.Marshal(v)
		fmt.Fprintf(&b, "data: %s\n\n", j)
	}
	delta := map[string]any{"role": "assistant"}
	if text != "" {
		delta["content"] = text
	}
	if toolCalls != nil {
		delta["tool_calls"] = toolCalls
	}
	chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
	chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage})
	b.WriteString("data: [DONE]\n\n")
	_, _ = w.Write(b.Bytes())
}

// rcFakeRepo makes a captured repository whose last commit seeds the bug, and
// changes into it.
func rcFakeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(rcFakeBugFile, "package calc\n\n// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	write("calc/total.go", "package calc\n\n// Total adds up xs.\nfunc Total(xs []int) int {\n\tt := 0\n\tfor _, x := range xs {\n\t\tt = Add(t, x)\n\t}\n\treturn t\n}\n")
	initGitRepo(t, dir)
	write(rcFakeBugFile, "package calc\n\n// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	write("calc/total.go", "package calc\n\n// Total adds up xs.\nfunc Total(xs []int) int {\n\tt := 0\n\tfor _, x := range xs[1:] {\n\t\tt = Add(t, x)\n\t}\n\treturn t\n}\n")
	gitCommitAll(t, dir, "Simplify Add")
	t.Chdir(dir)
	t.Setenv("KAI_NO_AUTOSYNC", "1")
	if err := runCapture(nil, nil); err != nil {
		t.Fatalf("capture: %v", err)
	}
	return dir
}

// rcFakeReviewEnv points the review at f and clears every setting a test
// might inherit. profile, when not nil, sets each stage's model.
func rcFakeReviewEnv(t *testing.T, f *rcFakeModelServer, model string, profile map[string]string) {
	t.Helper()
	// Every KAI_, GITHUB_, CI_ and RUNNER_ setting the test process inherits
	// is cleared first: a CI runner sets GITHUB_REPOSITORY, GITHUB_HEAD_REF
	// and more, which the review reads (its REPOSITORY line, the branch on the
	// bundle), and the parity check compares exact prompts and bundles.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		for _, prefix := range []string{"KAI_", "GITHUB_", "CI_", "RUNNER_"} {
			if strings.HasPrefix(name, prefix) {
				t.Setenv(name, "")
			}
		}
	}
	// CI=true makes the run object's source "ci".
	t.Setenv("CI", "")
	// Each stage's seconds is wall-clock time, which no two runs share: a
	// stopped clock makes it 0, so the parity check compares the rest.
	oldNow := rcRunNow
	stopped := time.Unix(1_800_000_000, 0)
	rcRunNow = func() time.Time { return stopped }
	t.Cleanup(func() { rcRunNow = oldNow })
	t.Setenv("KAI_NO_AUTOSYNC", "1")
	t.Setenv("KAI_PROVIDER", "openai")
	t.Setenv("KAI_OPENAI_BASE_URL", f.srv.URL)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("KAI_OPENAI_MODEL", model)
	// Named too, so they are cleared even when the process never had them.
	for _, k := range []string{"KAI_REVIEW_TIER", "KAI_REVIEW_SET_VERSION", "KAI_REVIEW_PROFILE", "KAI_REVIEW_PROFILE_LOW_RISK",
		"GITHUB_REPOSITORY_FULLNAME", "GITHUB_REPOSITORY", "GITHUB_HEAD_REF", "GITHUB_REF_NAME", "GITHUB_EVENT_NAME",
		"GITHUB_EVENT_PATH", "CI_COMMIT_REF_NAME", "KAI_REVIEW_MODEL", "KAI_SWEEP_MODEL", "KAI_CHALLENGE_MODEL",
		"KAI_REVIEW_REASONING_EFFORT", "KAI_REVIEW_FALLBACK_MODELS", "KAI_REVIEW_SUMMARY_MODEL", "KAI_REVIEW_PARTIAL",
		"KAI_REVIEW_SWEEP", "KAI_REVIEW_SANDBOX_IMAGE", "KAI_REVIEW_ONE_CLAIM", "KAI_PR_TITLE", "KAI_PR_BODY",
		"KAI_PRIOR_REVIEW", "KAI_REVIEW_FETCH_PRIOR", "KAI_REVIEW_RESCOPE", "KAI_REVIEW_RULES"} {
		t.Setenv(k, "")
	}
	if profile != nil {
		stages := map[string]any{}
		for st, m := range profile {
			stages[st] = map[string]string{"model": m}
		}
		b, _ := json.Marshal(map[string]any{"stages": stages})
		p := filepath.Join(t.TempDir(), "profile.json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KAI_REVIEW_PROFILE", p)
	}
}

// rcFakeBundle is the parts of a bundle the tests read.
type rcFakeBundle struct {
	Incomplete     bool            `json:"incomplete"`
	Partial        *bool           `json:"partial"`
	PartialReasons *[]string       `json:"partialReasons"`
	Review         string          `json:"review"`
	Run            *rcRunReport    `json:"run"`
	Claims         json.RawMessage `json:"claims"`
	Intent         struct {
		Risks []string `json:"risks"`
	} `json:"intent"`
}

// FindsTheBugs reports whether the bundle publishes both seeded bugs.
func (b rcFakeBundle) FindsTheBugs() bool {
	found := 0
	for _, want := range []string{"calc.go:5", "total.go:6"} {
		for _, r := range b.Intent.Risks {
			if strings.Contains(r, want) {
				found++
				break
			}
		}
	}
	return found == 2
}

// rcFakeRunReview runs one grounded review, as CI does, and returns the raw
// bundle ("" when none was written) and the run's error.
func rcFakeRunReview(t *testing.T) (string, error) {
	t.Helper()
	// The review sets process-wide state (the active profile among it); put it
	// back so a later test does not resolve its models from this review's set.
	oldKaiDir, oldFormat, oldDeep, oldProfile := kaiDir, reviewCommitFormat, reviewCommitDeep, rcActiveProfile
	reviewCommitFormat, reviewCommitDeep = "json", true
	t.Cleanup(func() {
		kaiDir, reviewCommitFormat, reviewCommitDeep, rcActiveProfile = oldKaiDir, oldFormat, oldDeep, oldProfile
	})
	var stdout bytes.Buffer
	reviewCommitCmd.SetOut(&stdout)
	t.Cleanup(func() { reviewCommitCmd.SetOut(nil) })
	err := runReviewCommit(reviewCommitCmd, []string{"HEAD"})
	return stdout.String(), err
}
