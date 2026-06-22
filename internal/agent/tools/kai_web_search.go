// kai_web_search: external web search via the kai-server Brave proxy.
//
// Why this exists: prior to v0.31.43 the agent had no way to look up
// "latest version of X" / "release date of Y" / "is library Z still
// maintained" — it had to fall back on training-data priors that go
// stale. The kai-server already deployed a /api/v1/search proxy
// fronting Brave Search; this tool wires the CLIENT side so agents
// can call it as a normal tool.
//
// Boundaries:
//   - Registers ONLY when both BaseURL and Token are configured
//     (i.e. the user is signed in to kailab). Tests and local-only
//     runs without `kai auth login` get the tool silently omitted.
//   - Returns top-N results as a compact text block (title + URL +
//     1-line snippet per hit). Capped to keep the tool result inside
//     the model's token budget — see maxResultsCap.
//   - Bounded HTTP timeout so a flaky upstream doesn't pin the
//     agent's wall-clock budget.
//   - All results land in a single tool_result; the agent can re-search
//     with a refined query if the first try misses.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// kaiWebSearchTool implements the BaseTool interface.
type kaiWebSearchTool struct {
	baseURL string // kai-server base, e.g. "https://kailab.kailayer.com"
	token   string // bearer token from kai auth login
	client  *http.Client
}

type kaiWebSearchParams struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// kaiWebSearchResult matches one row in the proxy's JSON response.
// Keep field names tolerant: Brave API and Google API name them
// differently; the server normalizes but we accept either shape.
type kaiWebSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Snippet     string `json:"snippet"`
}

type kaiWebSearchResponse struct {
	Results []kaiWebSearchResult `json:"results"`
	// Some servers wrap under "web.results" — accept both via custom
	// unmarshal below.
	Web struct {
		Results []kaiWebSearchResult `json:"results"`
	} `json:"web"`
	Error string `json:"error"`
}

const (
	webSearchDefaultLimit = 5
	webSearchMaxLimit     = 10
	webSearchHTTPTimeout  = 15 * time.Second
	// snippetCap bounds each result's description in the formatted
	// output so a single chatty Brave snippet can't dominate the
	// tool result's token budget. 240 chars ≈ ~60 tokens — enough
	// for the agent to tell whether the hit is relevant without
	// crowding out the 4 other results.
	snippetCap = 240
)

func (t *kaiWebSearchTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_web_search",
		Description: "Web search via the kai-server Brave proxy. Use this for facts the workspace doesn't contain — " +
			"library versions, release dates, deprecation notices, documentation snippets, package availability, " +
			"and general lookups about the OUTSIDE world. " +
			"Do NOT use for searching the user's codebase — that's kai_search (FTS) or kai_grep. " +
			"Returns up to 10 results as title + URL + snippet. Issue ONE query with the most specific terms; " +
			"refine with a second call if the first misses, do not spray 5 phrasings.",
		Parameters: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query. Plain text works best; include the most-specific named entities (library name, version number, year). Avoid quoting unless you need exact-phrase matching.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Max results, default %d, cap %d.", webSearchDefaultLimit, webSearchMaxLimit),
				"default":     webSearchDefaultLimit,
			},
		},
		Required: []string{"query"},
	}
}

func (t *kaiWebSearchTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.baseURL == "" || t.token == "" {
		return NewTextErrorResponse("kai_web_search: not configured (run `kai auth login`)"), nil
	}
	var p kaiWebSearchParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_web_search: invalid input json: " + err.Error()), nil
		}
	}
	query := strings.TrimSpace(p.Query)
	if query == "" {
		return NewTextErrorResponse("kai_web_search: query required"), nil
	}
	limit := p.Limit
	if limit <= 0 {
		limit = webSearchDefaultLimit
	}
	if limit > webSearchMaxLimit {
		limit = webSearchMaxLimit
	}

	results, err := t.callProxy(ctx, query, limit)
	if err != nil {
		return NewTextErrorResponse("kai_web_search: " + err.Error()), nil
	}
	if len(results) == 0 {
		return NewTextResponse(fmt.Sprintf("No results for query %q.", query)), nil
	}
	return NewTextResponse(formatWebResults(query, results)), nil
}

// callProxy fires the HTTP request and parses the response. Centralized
// so the formatter can be unit-tested separately from the network code.
func (t *kaiWebSearchTool) callProxy(ctx context.Context, query string, limit int) ([]kaiWebSearchResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, webSearchHTTPTimeout)
	defer cancel()

	endpoint := strings.TrimRight(t.baseURL, "/") + "/api/v1/search"
	body, err := json.Marshal(map[string]any{
		"query": query,
		"limit": limit,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncForErr(string(respBody), 200))
	}
	var out kaiWebSearchResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("upstream: %s", out.Error)
	}
	// Accept either top-level "results" or nested "web.results".
	if len(out.Results) > 0 {
		return out.Results, nil
	}
	return out.Web.Results, nil
}

// formatWebResults renders the result list as a compact text block.
// Numbered, with title on its own line, URL beneath, snippet indented.
// Keep snippets bounded so a single chatty result can't dominate.
func formatWebResults(query string, results []kaiWebSearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q (%d):\n\n", query, len(results))
	for i, r := range results {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = "(untitled)"
		}
		snippet := strings.TrimSpace(r.Snippet)
		if snippet == "" {
			snippet = strings.TrimSpace(r.Description)
		}
		if len(snippet) > snippetCap {
			snippet = snippet[:snippetCap-1] + "…"
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, title, r.URL)
		if snippet != "" {
			fmt.Fprintf(&b, "   %s\n", snippet)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncForErr clamps a string for inclusion in an error message.
// Different from the snippet cap above so we can tune the
// upstream-error display independently from the per-result preview.
func truncForErr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
