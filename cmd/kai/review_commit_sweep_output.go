package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

const rcSweepOutputInstruction = `
SWEEP OUTPUT CONTRACT: Return one JSON object with findings: an array of objects containing file (one of the supplied paths), line (positive integer), claim (concrete defect), and evidence (array of strings). Return {"findings":[]} when no defects are proposed. Do not supply intent, readiness, decisions, or a merge verdict: those belong to the main review and verifier. This replaces earlier output-format instructions. Findings are unverified proposals; preserve the evidence requirements.`

// Ignore review-level metadata: a sweep proposes defects, not an intent verdict.
// Reuse finding validation without letting unrelated fields discard proposals.
func rcDecodeSweep(raw string, changed map[string]bool) ([]string, error) {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```") {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = strings.TrimSpace(strings.TrimSuffix(text[i+1:], "```"))
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return nil, fmt.Errorf("invalid sweep JSON: %w", err)
	}
	findings, ok := fields["findings"]
	if !ok || string(findings) == "null" {
		return nil, fmt.Errorf("missing sweep findings array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(findings, &entries); err != nil {
		return nil, fmt.Errorf("sweep findings must be an array: %w", err)
	}
	var issues, rejected []string
	for i, entry := range entries {
		var location struct {
			File string `json:"file"`
		}
		if err := json.Unmarshal(entry, &location); err != nil || !changed[location.File] {
			rejected = append(rejected, fmt.Sprintf("finding %d has an invalid or out-of-chunk file", i+1))
			continue
		}
		normalized := map[string]any{"findings": []json.RawMessage{entry}, "intent_match": "partial", "merge_ready": 2, "summary": "Sweep proposals", "decisions": []string{}, "limitations": []string{}}
		data, _ := json.Marshal(normalized)
		decoded, err := rcDecodeReview(string(data))
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("finding %d: %v", i+1, err))
			continue
		}
		// Already structured and validated; do not re-parse prose or infer a
		// retraction with a phrase regex. The verifier judges these proposals.
		issues = append(issues, decoded.Findings...)
	}
	if len(rejected) > 0 {
		return issues, fmt.Errorf("%s", strings.Join(rejected, "; "))
	}
	return issues, nil
}

type rcSweepRun struct {
	Model    string            `json:"model"`
	Chunks   int               `json:"chunks"`
	Failed   int               `json:"failed"`
	Attempts []rcOutputAttempt `json:"attempts,omitempty"`
}

// One format repair, within the original sweep deadline. Provider failures and
// truncated responses remain failures; a repair must not certify lost content.
func rcSweepChunk(ctx context.Context, prov provider.Provider, req provider.Request, changed map[string]bool, chunk int) ([]string, []rcOutputAttempt, error) {
	var attempts []rcOutputAttempt
	var retained []string
	seen := map[string]bool{}
	for n := 0; n < 2; n++ {
		stage := "sweep"
		if n > 0 {
			stage = "sweep_repair"
		}
		resp, err := prov.Send(rcUsageStage(ctx, stage), req)
		raw := rcResponseText(resp)
		a := rcOutputAttempt{Stage: "sweep", Raw: raw, Chunk: chunk}
		if n > 0 {
			a.Stage = "sweep_repair"
		}
		if err != nil {
			a.Error = err.Error()
			attempts = append(attempts, a)
			return retained, attempts, err
		}
		if resp.FinishReason == message.FinishReasonMaxTokens {
			err = fmt.Errorf("sweep response truncated")
			a.Error = err.Error()
			attempts = append(attempts, a)
			return retained, attempts, err
		}
		issues, decodeErr := rcDecodeSweep(raw, changed)
		for _, issue := range issues {
			if !seen[issue] {
				retained = append(retained, issue)
				seen[issue] = true
			}
		}
		if decodeErr == nil {
			attempts = append(attempts, a)
			return retained, attempts, nil
		}
		a.Error = decodeErr.Error()
		attempts = append(attempts, a)
		if n == 1 || ctx.Err() != nil {
			return retained, attempts, decodeErr
		}
		req.Messages = append(append([]message.Message(nil), req.Messages...),
			message.Message{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: raw}}},
			message.Message{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "The sweep output could not be decoded: " + decodeErr.Error() + ". Reformat your proposals using the sweep output contract. Preserve the claims and evidence; do not invent defects. This is the only format repair."}}})
	}
	panic("unreachable")
}

func (i *rcIncomplete) recordSweep(sw rcSweepResult) {
	if i.Execution == nil {
		i.Execution = &rcExecution{Discovery: "incomplete", Verification: "not_started"}
	}
	i.Execution.Sweeps = append(i.Execution.Sweeps, sw.Runs...)
	if sw.Failed > 0 {
		i.Execution.Discovery = "incomplete"
	}
}
