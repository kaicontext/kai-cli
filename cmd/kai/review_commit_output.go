package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/finding"
)

// Discovery data is untrusted. Only the challenge may turn it into published
// findings. Legacy text is accepted at this boundary, never as a second gate.
type rcReviewOutput struct {
	IntentMatch string   `json:"intent_match"`
	MergeReady  int      `json:"merge_ready"`
	Summary     string   `json:"summary"`
	Findings    []string `json:"findings"`
	Decisions   []string `json:"decisions"`
	Limitations []string `json:"limitations"`
}

const rcOutputInstruction = `
FINAL OUTPUT CONTRACT: Return one JSON object with intent_match (verified, partial, or diverges), merge_ready (1-5), summary (string), findings (array of objects with file, line, claim, and evidence fields; evidence is an array of strings), decisions (array of strings), and limitations (array of strings). All fields are required; use empty arrays when there are no findings. Do not output Markdown headings or a REVIEW-DATA block. These are proposals, not verified findings. This final-output contract replaces earlier instructions about the response format; keep their evidence and review requirements.`

func rcOutputSchema() map[string]interface{} {
	str := map[string]interface{}{"type": "string"}
	list := map[string]interface{}{"type": "array", "items": str}
	return map[string]interface{}{"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"intent_match": map[string]interface{}{"type": "string", "enum": []string{"verified", "partial", "diverges"}},
			"merge_ready":  map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 5},
			"summary":      str, "findings": map[string]interface{}{"type": "array", "items": map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"file": str, "line": map[string]interface{}{"type": "integer", "minimum": 1}, "claim": str, "evidence": list},
				"required":   []string{"file", "line", "claim", "evidence"},
			}}, "decisions": list, "limitations": list,
		}, "required": []string{"intent_match", "merge_ready", "summary", "findings", "decisions", "limitations"}}
}

func rcDecodeReview(raw string) (rcReviewOutput, error) {
	var out rcReviewOutput
	t := strings.TrimSpace(raw)
	if strings.HasPrefix(t, "```") {
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = strings.TrimSpace(strings.TrimSuffix(t[i+1:], "```"))
		}
	}
	if strings.HasPrefix(t, "{") {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(t), &fields); err != nil {
			return out, fmt.Errorf("invalid discovery JSON: %w", err)
		}
		for _, k := range []string{"intent_match", "merge_ready", "summary", "findings", "decisions", "limitations"} {
			v, ok := fields[k]
			if !ok || string(v) == "null" {
				return out, fmt.Errorf("missing discovery field %s", k)
			}
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(fields["findings"], &entries); err != nil {
			return out, err
		}
		fields["findings"] = json.RawMessage(`[]`)
		rest, _ := json.Marshal(fields)
		if err := json.Unmarshal(rest, &out); err != nil {
			return out, err
		}
		for _, entry := range entries {
			var legacy string
			if json.Unmarshal(entry, &legacy) == nil {
				out.Findings = append(out.Findings, legacy)
				continue
			}
			var f struct {
				File     string   `json:"file"`
				Line     int      `json:"line"`
				Claim    string   `json:"claim"`
				Evidence []string `json:"evidence"`
			}
			if err := json.Unmarshal(entry, &f); err != nil {
				return out, err
			}
			if f.File == "" || path.IsAbs(f.File) || path.Clean(f.File) == ".." || strings.HasPrefix(path.Clean(f.File), "../") || strings.ContainsAny(f.File, "\r\n") || f.Line < 1 || strings.TrimSpace(f.Claim) == "" || f.Evidence == nil {
				return out, fmt.Errorf("invalid discovery finding location, claim, or evidence")
			}
			out.Findings = append(out.Findings, fmt.Sprintf("%s:%d — %s", f.File, f.Line, rcSingleLine(f.Claim)))
		}
	} else {
		_, issues, decisions, match, ready, note := rcParseReviewOutput(t)
		// An explicit list header is necessary: a cut-off answer with only a
		// verdict is not evidence that discovery found zero bugs.
		listSeen := false
		for _, line := range strings.Split(raw, "\n") {
			k, _, ok := rcMachineLine(strings.TrimSpace(line))
			if ok && (k == "issues" || k == "findings") {
				listSeen = true
			}
		}
		if !listSeen {
			return out, fmt.Errorf("discovery has no explicit findings list")
		}
		out = rcReviewOutput{IntentMatch: string(match), MergeReady: int(ready), Summary: note, Findings: issues, Decisions: decisions}
	}
	if _, ok := rcIntentVerdicts[strings.ToLower(strings.TrimSpace(out.IntentMatch))]; !ok {
		return out, fmt.Errorf("invalid discovery intent")
	}
	if !finding.Readiness(out.MergeReady).Valid() {
		return out, fmt.Errorf("invalid discovery readiness")
	}
	for _, v := range append(append([]string{}, out.Findings...), out.Decisions...) {
		if strings.TrimSpace(v) == "" {
			return out, fmt.Errorf("empty discovery allegation")
		}
	}
	return out, nil
}

// Canonical text is an adapter for the existing batching/selection helpers.
// It is written by code, never relied on as a model formatting contract.
func (o rcReviewOutput) draft() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nINTENT_MATCH: %s\nMERGE_READY: %d\nSUMMARY: %s\nISSUES:\n", rcReviewDataMarker, o.IntentMatch, o.MergeReady, rcSingleLine(o.Summary))
	if len(o.Findings) == 0 {
		b.WriteString("- (none)\n")
	}
	for _, v := range o.Findings {
		fmt.Fprintf(&b, "- %s\n", rcSingleLine(v))
	}
	b.WriteString("DECISIONS:\n")
	for _, v := range o.Decisions {
		fmt.Fprintf(&b, "- %s\n", rcSingleLine(v))
	}
	return b.String()
}
func rcSingleLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Diagnostics contain model output, not prompts/tool responses. They are part
// of the private finding artifact, not the rendered public review.
type rcOutputAttempt struct {
	Stage string `json:"stage"`
	Raw   string `json:"raw"`
	Error string `json:"error,omitempty"`
}
type rcExecution struct {
	Failure      string            `json:"failure,omitempty"`
	Discovery    string            `json:"discovery"`
	Verification string            `json:"verification"`
	Attempts     []rcOutputAttempt `json:"attempts,omitempty"`
}

func (i *rcIncomplete) recordOutput(stage, raw string) {
	if i.Execution == nil {
		i.Execution = &rcExecution{Discovery: "incomplete", Verification: "not_started"}
	}
	a := rcOutputAttempt{Stage: stage, Raw: raw}
	if _, err := rcDecodeReview(raw); err != nil {
		a.Error = err.Error()
	}
	i.Execution.Attempts = append(i.Execution.Attempts, a)
}
func rcExecutionOf(i *rcIncomplete) *rcExecution {
	if i == nil {
		return nil
	}
	if i.Execution != nil {
		i.Execution.Failure = i.ChallengeFailure
	}
	return i.Execution
}

// Final defense at the publication boundary. The review body is rebuilt from
// verification results so draft prose cannot smuggle in additional claims.
func rcPublicationReady(raw string, r *rcChallengeResult) bool {
	if _, err := rcDecodeReview(raw); err != nil {
		return false
	}
	if r == nil {
		return false
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(raw)
	allowed := map[string]bool{}
	for _, a := range r.Allegations {
		if a.Status == rcStatusSupported && !a.Unchecked && len(a.Evidence) > 0 && strings.TrimSpace(a.Finding) != "" {
			allowed[rcPublishedIssue(a)] = true
		}
	}
	for _, issue := range issues {
		if !allowed[issue] {
			return false
		}
	}
	return true
}

func rcFindingKey(issue string) string { return fmt.Sprintf("f-%x", sha256.Sum256([]byte(issue))) }

// Carry failed attempts to the emitted incomplete bundle as well as successful ones.
type rcDiscoveryError struct {
	Cause               error
	Attempts            []rcOutputAttempt
	VerificationStarted bool
}

func (e *rcDiscoveryError) Error() string { return e.Cause.Error() }
func (e *rcDiscoveryError) Unwrap() error { return e.Cause }

// Structured decoding belongs at the output boundary, not on every turn of
// a tool-using explorer. Run29 produced valid JSON but opened zero files.
func rcExplorationOptions(opts agent.Options) agent.Options {
	opts.OutputJSONSchema = nil
	opts.DisableTools = false
	return opts
}

func rcRequireExploration(inc *rcIncomplete, changed []string) error {
	if len(changed) > 0 && (inc == nil || len(inc.FilesRead) == 0) {
		return fmt.Errorf("grounded discovery opened no repository files; withholding the draft despite valid output")
	}
	return nil
}
