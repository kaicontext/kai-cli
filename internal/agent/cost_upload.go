package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// CostUploadConfig, when set on Options, makes runLoop POST a per-run cost
// record to kailab-control at run end (best-effort, async). nil ⇒ no upload
// (sub-agents, tests, BYOM-direct runs that never touch our server). The
// caller supplies the attribution it knows; the server derives cost from the
// uploaded usage (cost-instrumentation brief, Step 5 — server owns prices).
type CostUploadConfig struct {
	BaseURL    string // kai-server base, e.g. https://kaicontext.com
	AuthToken  string // kailab bearer
	RepoID     string
	TaskType   string // code-review | plan | fix | chat | ...
	CLIVersion string
}

// uploadRunCost assembles and POSTs the per-run usage record. Best-effort:
// any failure is logged, never surfaced — a missing cost record must never
// affect the user's run. Runs in its own goroutine with a short timeout.
func uploadRunCost(cfg *CostUploadConfig, res *Result, model string, wall time.Duration) {
	if cfg == nil || res == nil || cfg.BaseURL == "" || cfg.AuthToken == "" {
		return
	}
	runID := res.SessionID
	if runID == "" {
		return // no stable id to key the record on
	}
	// Only usage that actually happened is worth a record.
	if res.TokensIn == 0 && res.TokensOut == 0 && res.RequestCount == 0 {
		return
	}

	type usageBlock struct {
		InputTokens     int     `json:"input_tokens"`
		OutputTokens    int     `json:"output_tokens"`
		CacheTokens     int     `json:"cache_tokens"`
		RequestCount    int     `json:"request_count"`
		Provider        string  `json:"provider"`
		Model           string  `json:"model"`
		BYOM            bool    `json:"byom"`
		ProviderCostUSD float64 `json:"provider_cost_usd"`
		WallTimeMs      int64   `json:"wall_time_ms"`
	}
	payload := struct {
		RunID      string     `json:"run_id"`
		RepoID     string     `json:"repo_id"`
		TaskType   string     `json:"task_type"`
		CLIVersion string     `json:"cli_version"`
		Timestamp  int64      `json:"timestamp"`
		Usage      usageBlock `json:"usage"`
	}{
		RunID: runID, RepoID: cfg.RepoID, TaskType: cfg.TaskType,
		CLIVersion: cfg.CLIVersion, Timestamp: time.Now().UnixMilli(),
		Usage: usageBlock{
			InputTokens:     res.TokensIn,
			OutputTokens:    res.TokensOut,
			CacheTokens:     res.TokensCacheRead,
			RequestCount:    res.RequestCount,
			Provider:        providerForModel(model),
			Model:           model,
			BYOM:            false, // only the kailab-routed path uploads
			ProviderCostUSD: res.ProviderCostUSD,
			WallTimeMs:      wall.Milliseconds(),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(cfg.BaseURL, "/")+"/api/v1/runs/cost", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("cost record upload failed (non-fatal): %v", err)
		return
	}
	resp.Body.Close()
}

// providerForModel maps a model id to its routing provider for the record.
func providerForModel(model string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude") {
		return "anthropic"
	}
	return "openrouter"
}
