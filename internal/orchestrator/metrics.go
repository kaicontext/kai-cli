package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kai/internal/agent"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/kaipath"
)

// Graph-context-injection measurement. One JSON line per agent run
// gets appended to .kai/metrics.jsonl. Three signals from the spec:
//
//   - locality: did the agent's first tool call name a file present
//     in the injected chain? Higher = injection points the agent
//     at the right area.
//   - correctness: did the verify pass return VERIFIED on the first
//     try without INCOMPLETE? Higher = agent is finishing the job.
//   - over_claiming: did the absence guard fire? Should go DOWN
//     when injection is working — the agent has the context.
//
// jsonl chosen so the file is grep-able and trivially aggregable
// (jq, awk). Append-only; no read path on the writer side.

// injectionMetric is the schema of one .kai/metrics.jsonl line.
// All fields are best-effort: missing signals (e.g. verify didn't
// run) get zero values rather than absent JSON keys, so downstream
// dashboards can rely on a stable shape.
type injectionMetric struct {
	Timestamp           string `json:"ts"`
	TaskName            string `json:"task,omitempty"`
	Mode                string `json:"mode,omitempty"`
	InjectedChars       int    `json:"injected_chars"`
	FirstToolName       string `json:"first_tool,omitempty"`
	FirstToolFile       string `json:"first_tool_file,omitempty"`
	FirstToolInChain    bool   `json:"first_tool_in_chain"`
	AbsenceGuardFired   bool   `json:"absence_guard_fired"`
	VerifyOutcomeName   string `json:"verify_outcome,omitempty"`
	CompletedFirstPass  bool   `json:"completed_first_pass"`
}

// recordInjectionMetric writes one JSON line summarising the run's
// graph-context-injection signals. Best-effort: any failure here is
// swallowed because metrics shouldn't block a real user run. Called
// from the orchestrator after both main and verify (if any) finish.
//
// injectionBody is the original opts.InjectedContext text — we walk
// it to extract the file list the injection pointed the agent at.
// transcript is the main agent's transcript (verify's is separate).
// verifyOutcome and absenceGuardFired are passed in directly rather
// than recomputed.
func recordInjectionMetric(
	workspaceRoot string,
	taskName, mode string,
	injectionBody string,
	transcript []message.Message,
	verifyOutcome verifyOutcome,
	res *agent.Result,
) {
	if workspaceRoot == "" {
		return
	}
	if res == nil {
		return
	}
	// Resolve the workspace's kai data directory through kaipath so
	// repos that use .git/kai/ get their metrics in the right place
	// instead of having a sibling .kai/ scattered next to .git/. And
	// CRITICALLY: only write if a kai dir ALREADY exists. Eager
	// MkdirAll here is exactly the pattern that scattered rogue
	// .kai/ directories at every workspace path the planner touched
	// — fixed in LogLocal on 2026-05-11, then snuck back in here.
	// Metrics are best-effort; if no kai dir exists at the workspace,
	// skip rather than fabricate one. The user-facing run is
	// unaffected either way.
	metricsDir := kaipath.Resolve(workspaceRoot)
	if metricsDir == "" {
		return
	}
	if info, err := os.Stat(metricsDir); err != nil || !info.IsDir() {
		return
	}
	path := filepath.Join(metricsDir, "metrics.jsonl")

	firstName, firstFile := firstAgentToolCall(transcript)
	chainFiles := injectionFiles(injectionBody)

	m := injectionMetric{
		Timestamp:          time.Now().UTC().Format(time.RFC3339Nano),
		TaskName:           taskName,
		Mode:               mode,
		InjectedChars:      res.InjectedContextChars,
		FirstToolName:      firstName,
		FirstToolFile:      firstFile,
		FirstToolInChain:   firstFile != "" && chainFiles[firstFile],
		AbsenceGuardFired:  res.AbsenceGuardFired,
		VerifyOutcomeName:  verifyOutcomeString(verifyOutcome),
		CompletedFirstPass: verifyOutcome == verifyPassed,
	}
	line, err := json.Marshal(m)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// firstAgentToolCall scans the transcript for the first tool_use the
// agent issued AFTER any injected context_lookup pair. Returns the
// tool name and any path-shaped input field. Empty when the agent
// never made a tool call.
func firstAgentToolCall(msgs []message.Message) (name, file string) {
	for _, m := range msgs {
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			tc, ok := p.(message.ToolCall)
			if !ok {
				continue
			}
			// Skip the synthetic context_lookup — we want the first
			// REAL call the agent made.
			if tc.Name == "context_lookup" {
				continue
			}
			name = tc.Name
			file = extractPathFromInput(tc.Input)
			return
		}
	}
	return "", ""
}

// extractPathFromInput pulls a path-shaped field out of the tool's
// JSON input. Tries common field names; first hit wins. Returns ""
// when no path-shaped field is present (tools like kai_grep with
// just a query string, for example — locality is unknowable in
// that case and the metric records an empty file).
func extractPathFromInput(inputJSON string) string {
	if inputJSON == "" {
		return ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &raw); err != nil {
		return ""
	}
	for _, k := range []string{"file_path", "path", "file"} {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// injectionFiles parses the formatted context-injection body and
// returns the set of file paths it referenced. The formatter writes
// paths as " (path/to/file.go)" — the regex catches those. Set is
// used by FirstToolInChain to credit locality when the agent opened
// any file mentioned in the chain.
func injectionFiles(body string) map[string]bool {
	out := map[string]bool{}
	if body == "" {
		return out
	}
	// Cheap line walk — looking for the "(path)" segments that
	// FormatCallChains emits next to each node. Could use a regex
	// but the format is stable and this avoids the regex cost on
	// every run.
	for _, line := range strings.Split(body, "\n") {
		open := strings.Index(line, "(")
		for open >= 0 {
			close := strings.Index(line[open+1:], ")")
			if close < 0 {
				break
			}
			path := strings.TrimSpace(line[open+1 : open+1+close])
			if isLikelyPath(path) {
				out[path] = true
			}
			line = line[open+1+close+1:]
			open = strings.Index(line, "(")
		}
	}
	return out
}

// isLikelyPath excludes parenthesized fragments that aren't paths
// ("(via command index)", "(stdlib, not expanded)") so the
// locality set stays clean.
func isLikelyPath(s string) bool {
	if s == "" {
		return false
	}
	// Must contain a `/` or a `.<ext>` shape.
	if strings.ContainsRune(s, '/') {
		return true
	}
	if i := strings.LastIndex(s, "."); i >= 0 && i < len(s)-1 {
		ext := s[i+1:]
		if len(ext) <= 6 && allLowerAlphaNum(ext) {
			return true
		}
	}
	return false
}

func allLowerAlphaNum(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// verifyOutcomeString maps the integer outcome to a stable name for
// the metric. Used by dashboards that group by outcome.
func verifyOutcomeString(o verifyOutcome) string {
	switch o {
	case verifyPassed:
		return "passed"
	case verifyApplied:
		return "applied"
	case verifyIncomplete:
		return "incomplete"
	case verifyBlocked:
		return "blocked"
	case verifyUnknown:
		return "unknown"
	}
	return ""
}
