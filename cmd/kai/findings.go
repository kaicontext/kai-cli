package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/remote"
	"github.com/spf13/cobra"
)

// Flags for `kai findings …`. Shared across the list/get subcommands.
var (
	findingsRepo  string
	findingsJSON  bool
	findingsLimit int
)

// Flags for `kai findings run`.
var (
	findingsRunPR            string
	findingsRunEvent         string
	findingsRunAllowFallback bool
	findingsRunWait          bool
)

// resolveFindingsTarget determines where to query findings: the control-plane
// base URL (KAI_SERVER or remote.DefaultServer — the findings API lives on the
// control plane, independent of the git push remote), the auth token, and the
// org/repo (from --repo org/repo, else the configured "origin" remote).
func resolveFindingsTarget() (baseURL, token, org, repo string, err error) {
	baseURL = os.Getenv("KAI_SERVER")
	if baseURL == "" {
		baseURL = remote.DefaultServer
	}
	baseURL = strings.TrimRight(baseURL, "/")

	token, _ = remote.GetValidAccessToken()
	if token == "" {
		return "", "", "", "", fmt.Errorf("not logged in — run `kai login` first")
	}

	if findingsRepo != "" {
		o, r, ok := strings.Cut(findingsRepo, "/")
		if !ok || o == "" || r == "" {
			return "", "", "", "", fmt.Errorf("--repo must be in the form org/repo")
		}
		return baseURL, token, o, r, nil
	}
	entry, gerr := remote.GetRemote("origin")
	if gerr != nil || entry == nil || entry.Tenant == "" || entry.Repo == "" {
		return "", "", "", "", fmt.Errorf("no repo specified: pass --repo org/repo, or set an 'origin' remote (`kai remote set origin <url> --tenant <org> --repo <repo>`)")
	}
	return baseURL, token, entry.Tenant, entry.Repo, nil
}

// findingsAPIGet performs an authenticated GET against the control plane and
// returns the raw body, translating the common HTTP failures into clear errors.
func findingsAPIGet(baseURL, token, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("unauthorized (401) — your session may have expired; run `kai login`")
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("forbidden (403) — you don't have access to this repo's findings")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("not found (404)")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// findingsServerErrorMessage extracts the "error" field from a JSON error body
// (the {"error": "...", "details": "..."} shape kailab-control's writeError
// produces), so a caller can surface the server's message verbatim rather than
// a raw JSON blob. Falls back to the raw body text, then to fallback, when the
// body doesn't parse or carries no "error" field.
func findingsServerErrorMessage(body []byte, fallback string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return fallback
}

// findingsAPIPost performs an authenticated POST with a JSON body against the
// control plane and returns the raw response body, its HTTP status, and an
// error translating the common failures into clear messages — same 401/403/404
// mapping as findingsAPIGet, plus 409 (conflict — e.g. a duplicate review
// trigger) and 422 (validation, e.g. "no review workflow matched"), both of
// which surface the server's message verbatim via findingsServerErrorMessage.
// The body is always returned alongside the error so a caller (e.g. the 409
// case, which carries a run_id worth showing) can inspect it even on failure.
func findingsAPIPost(baseURL, token, path string, reqBody interface{}) (body []byte, status int, err error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	status = resp.StatusCode

	switch {
	case status == http.StatusUnauthorized:
		return body, status, fmt.Errorf("unauthorized (401) — your session may have expired; run `kai login`")
	case status == http.StatusForbidden:
		return body, status, fmt.Errorf("forbidden (403) — you don't have permission to do this")
	case status == http.StatusNotFound:
		return body, status, fmt.Errorf("not found (404)")
	case status == http.StatusConflict:
		return body, status, fmt.Errorf("%s", findingsServerErrorMessage(body, "conflict (409)"))
	case status == http.StatusUnprocessableEntity:
		return body, status, fmt.Errorf("%s", findingsServerErrorMessage(body, "unprocessable (422)"))
	case status < 200 || status >= 300:
		return body, status, fmt.Errorf("server error %d: %s", status, findingsServerErrorMessage(body, ""))
	}
	return body, status, nil
}

// findingSummary mirrors the flat fields the findings API returns per finding.
type findingSummary struct {
	ID          string `json:"id"`
	PRNumber    int    `json:"pr_number"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Verdict     string `json:"verdict"`
	IntentMatch string `json:"intent_match"`
	Reaches     int    `json:"reaches"`
	Claims      int    `json:"claims"`
	Risk        int    `json:"risk"`
}

var findingsCmd = &cobra.Command{
	Use:   "findings",
	Short: "List and view Kai review findings from the server",
	Long: `List and view the code-review findings Kai produced for a repo's pull requests.

The repo is taken from --repo org/repo, or the configured "origin" remote.
Requires a login (kai login). The findings API lives on the control plane
(override with KAI_SERVER).`,
}

var findingsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent review findings for a repo",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		baseURL, token, org, repo, err := resolveFindingsTarget()
		if err != nil {
			return err
		}
		body, err := findingsAPIGet(baseURL, token, fmt.Sprintf("/api/v1/orgs/%s/repos/%s/findings", org, repo))
		if err != nil {
			return err
		}
		if findingsJSON {
			_, _ = os.Stdout.Write(findingsPrettyJSON(body))
			return nil
		}
		var resp struct {
			Findings []findingSummary `json:"findings"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("parse findings: %w", err)
		}
		if len(resp.Findings) == 0 {
			fmt.Printf("No findings for %s/%s yet.\n", org, repo)
			return nil
		}
		total := len(resp.Findings)
		shown := resp.Findings
		if findingsLimit > 0 && findingsLimit < total {
			shown = shown[:findingsLimit]
		}
		fmt.Printf("Findings for %s/%s:\n\n", org, repo)
		for _, f := range shown {
			pr := ""
			if f.PRNumber > 0 {
				pr = fmt.Sprintf("PR #%-4d ", f.PRNumber)
			}
			fmt.Printf("  %-22s %-9s %srisk:%d claims:%d  %s\n",
				f.ID, findingsVerdictLabel(f.Verdict), pr, f.Risk, f.Claims, findingsTruncate(f.Title, 56))
		}
		if len(shown) < total {
			fmt.Printf("\n%d of %d finding(s) shown (--limit %d).\n", len(shown), total, findingsLimit)
		} else {
			fmt.Printf("\n%d finding(s). View one:  kai findings get <id>\n", total)
		}
		return nil
	},
}

var findingsGetCmd = &cobra.Command{
	Use:   "get <finding-id>",
	Short: "Show a single review finding",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		baseURL, token, org, repo, err := resolveFindingsTarget()
		if err != nil {
			return err
		}
		body, err := findingsAPIGet(baseURL, token, fmt.Sprintf("/api/v1/orgs/%s/repos/%s/findings/%s", org, repo, args[0]))
		if err != nil {
			return err
		}
		if findingsJSON {
			_, _ = os.Stdout.Write(findingsPrettyJSON(body))
			return nil
		}
		return renderFinding(baseURL, org, repo, body)
	},
}

// githubPRURLPattern matches a GitHub pull request URL:
// https://github.com/<owner>/<repo>/pull/<number>.
var githubPRURLPattern = regexp.MustCompile(`^https?://github\.com/([^/]+/[^/]+)/pull/(\d+)/?$`)

// parsePRRef parses --pr, which is either a bare PR number ("123") or a full
// GitHub PR URL. The URL form also yields the owner/repo, which the caller
// sends as github_repository — the URL's repo is authoritative for the review
// target, NOT --repo/the origin remote (those still pick the kai org/repo the
// API call is routed to; github_repository is the separate, possibly-
// different real GitHub identity the pod clones — see 01-trigger-review-api.md).
func parsePRRef(s string) (prNumber int, githubRepository string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, "", fmt.Errorf("--pr is required: a PR number or a GitHub PR URL")
	}
	if m := githubPRURLPattern.FindStringSubmatch(s); m != nil {
		n, perr := strconv.Atoi(m[2])
		if perr != nil || n <= 0 {
			return 0, "", fmt.Errorf("invalid PR number in URL %q", s)
		}
		return n, m[1], nil
	}
	n, perr := strconv.Atoi(s)
	if perr != nil {
		return 0, "", fmt.Errorf("--pr must be a PR number or a GitHub PR URL, got %q", s)
	}
	if n <= 0 {
		return 0, "", fmt.Errorf("--pr must be positive, got %d", n)
	}
	return n, "", nil
}

// runURL builds the web console URL for a workflow run.
func runURL(baseURL, org, repo, runID string) string {
	return fmt.Sprintf("%s/%s/%s/workflows/runs/%s", baseURL, org, repo, runID)
}

// findingsRunPollInterval / findingsRunWaitTimeout govern `kai findings run
// --wait`. Vars (not consts) so tests can shrink them.
var (
	findingsRunPollInterval = 5 * time.Second
	findingsRunWaitTimeout  = 20 * time.Minute
)

// waitForRun polls a run's status (GET .../runs/{run_id}, which already
// exists) until it completes or findingsRunWaitTimeout elapses, printing the
// outcome. A non-success conclusion or a timeout is returned as an error so
// `kai findings run --wait` exits non-zero exactly when the review didn't
// succeed. This is the "or land it in 02 if 01 ships first" fallback the task
// calls for — 02's dedicated status endpoint can replace it later without
// changing this command's UX.
func waitForRun(baseURL, token, org, repo, runID string) error {
	fmt.Printf("Waiting for run %s…\n", runID)
	deadline := time.Now().Add(findingsRunWaitTimeout)
	for {
		body, err := findingsAPIGet(baseURL, token, fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs/%s", org, repo, runID))
		if err != nil {
			return err
		}
		var run struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		}
		if err := json.Unmarshal(body, &run); err != nil {
			return fmt.Errorf("parse run status: %w", err)
		}
		if run.Status == "completed" {
			fmt.Printf("Run %s finished: %s\n", runID, findingsVerdictLabel(run.Conclusion))
			if run.Conclusion != "success" {
				return fmt.Errorf("run %s did not succeed (conclusion=%s)", runID, run.Conclusion)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for run %s (still %q after %s)", runID, run.Status, findingsRunWaitTimeout)
		}
		time.Sleep(findingsRunPollInterval)
	}
}

var findingsRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Trigger a Kai review for a pull request",
	Long: `Starts the exact review a GitHub webhook would start for a PR — from your
machine (or an agent acting on your behalf), against any kai-server
(override with KAI_SERVER), without waiting for GitHub to deliver a webhook.

Examples:
  kai findings run --pr 123
  kai findings run --pr https://github.com/kaicontext/kai-server/pull/123
  kai findings run --pr 123 --repo kai/kai-server --wait`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		baseURL, token, org, repo, err := resolveFindingsTarget()
		if err != nil {
			return err
		}
		prNumber, ghRepo, err := parsePRRef(findingsRunPR)
		if err != nil {
			return err
		}

		reqBody := map[string]interface{}{"pr_number": prNumber}
		if ghRepo != "" {
			reqBody["github_repository"] = ghRepo
		}
		if findingsRunEvent != "" {
			reqBody["event"] = findingsRunEvent
		}
		if findingsRunAllowFallback {
			reqBody["allow_fallback"] = true
		}

		body, status, err := findingsAPIPost(baseURL, token,
			fmt.Sprintf("/api/v1/orgs/%s/repos/%s/reviews", org, repo), reqBody)

		// 409 (a review is already active for this commit) isn't a failure to
		// trigger — it's the review the caller wanted, already running. Report
		// it and (with --wait) watch the EXISTING run rather than erroring.
		if status == http.StatusConflict {
			var conflict struct {
				RunID    string `json:"run_id"`
				ReviewID string `json:"review_id"`
			}
			_ = json.Unmarshal(body, &conflict)
			fmt.Println("A review is already queued or in progress for this commit.")
			if conflict.RunID != "" {
				fmt.Printf("  run:  %s\n", conflict.RunID)
				fmt.Printf("  %s\n", runURL(baseURL, org, repo, conflict.RunID))
			}
			if findingsRunWait && conflict.RunID != "" {
				return waitForRun(baseURL, token, org, repo, conflict.RunID)
			}
			return nil
		}
		if err != nil {
			return err
		}

		var resp struct {
			RunID    string `json:"run_id"`
			ReviewID string `json:"review_id"`
			Status   string `json:"status"`
			Mode     string `json:"mode"`
		}
		if jerr := json.Unmarshal(body, &resp); jerr != nil {
			return fmt.Errorf("parse response: %w", jerr)
		}

		fmt.Printf("Review triggered for PR #%d.\n", prNumber)
		if resp.Mode == "shard_fallback" {
			fmt.Println("  mode: shard_fallback (non-agentic — no CI review workflow matched)")
		}
		if resp.RunID != "" {
			fmt.Printf("  run:  %s\n", resp.RunID)
			fmt.Printf("  %s\n", runURL(baseURL, org, repo, resp.RunID))
		}
		if resp.ReviewID != "" {
			fmt.Printf("  review: %s/%s/%s/findings/%s\n", baseURL, org, repo, resp.ReviewID)
		}

		if findingsRunWait && resp.RunID != "" {
			return waitForRun(baseURL, token, org, repo, resp.RunID)
		}
		return nil
	},
}

// renderFinding prints a human-readable summary of a finding plus its grounded
// claims. The full bundle (blast radius, diff, etc.) is available via --json.
func renderFinding(baseURL, org, repo string, body []byte) error {
	var resp struct {
		findingSummary
		Finding struct {
			Title  string `json:"title"`
			Claims []struct {
				Statement string `json:"statement"`
				Tag       string `json:"tag"`
				Verified  bool   `json:"verified"`
				Resolved  bool   `json:"resolved"`
			} `json:"claims"`
		} `json:"finding"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse finding: %w", err)
	}

	title := resp.Title
	if title == "" {
		title = resp.Finding.Title
	}
	if title == "" {
		title = "(untitled change)"
	}
	fmt.Println(title)
	fmt.Printf("  id:      %s\n", resp.ID)
	if resp.PRNumber > 0 {
		fmt.Printf("  pr:      #%d\n", resp.PRNumber)
	}
	if resp.Author != "" {
		fmt.Printf("  author:  %s\n", resp.Author)
	}
	fmt.Printf("  verdict: %s\n", findingsVerdictLabel(resp.Verdict))
	if resp.IntentMatch != "" {
		fmt.Printf("  intent:  %s\n", resp.IntentMatch)
	}
	fmt.Printf("  blast:   reaches %d symbol(s)\n", resp.Reaches)

	if len(resp.Finding.Claims) > 0 {
		fmt.Printf("\nGrounded claims (%d):\n", len(resp.Finding.Claims))
		for _, c := range resp.Finding.Claims {
			mark := "•"
			if c.Tag == "risk" || c.Tag == "negative_existential" {
				mark = "⚠"
			}
			status := ""
			if c.Verified {
				status = " [verified]"
			} else if c.Resolved {
				status = " [resolved]"
			}
			fmt.Printf("  %s%s %s\n", mark, status, strings.TrimSpace(c.Statement))
		}
	} else {
		fmt.Printf("\nNo grounded claims.\n")
	}

	fmt.Printf("\n%s/%s/%s/findings/%s\n", baseURL, org, repo, resp.ID)
	return nil
}

func findingsVerdictLabel(v string) string {
	if v == "" {
		return "awaiting"
	}
	return v
}

func findingsTruncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func findingsPrettyJSON(b []byte) []byte {
	var v interface{}
	if json.Unmarshal(b, &v) != nil {
		return append(b, '\n')
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return append(b, '\n')
	}
	return append(out, '\n')
}

func init() {
	for _, c := range []*cobra.Command{findingsListCmd, findingsGetCmd, findingsRunCmd} {
		c.Flags().StringVar(&findingsRepo, "repo", "", "Target repo as org/repo (default: the 'origin' remote)")
	}
	for _, c := range []*cobra.Command{findingsListCmd, findingsGetCmd} {
		c.Flags().BoolVar(&findingsJSON, "json", false, "Output raw JSON")
	}
	findingsListCmd.Flags().IntVar(&findingsLimit, "limit", 0, "Maximum findings to show (0 = all)")

	findingsRunCmd.Flags().StringVar(&findingsRunPR, "pr", "", "PR number or GitHub PR URL (required)")
	findingsRunCmd.Flags().StringVar(&findingsRunEvent, "event", "", "Trigger event: review_created or review_updated (default: review_created)")
	findingsRunCmd.Flags().BoolVar(&findingsRunAllowFallback, "allow-fallback", false, "If no CI review workflow matches, fall back to a weaker, non-agentic shard review instead of failing")
	findingsRunCmd.Flags().BoolVar(&findingsRunWait, "wait", false, "Poll until the review run completes")
	_ = findingsRunCmd.MarkFlagRequired("pr")

	findingsCmd.AddCommand(findingsListCmd, findingsGetCmd, findingsRunCmd)
	rootCmd.AddCommand(findingsCmd)
}
