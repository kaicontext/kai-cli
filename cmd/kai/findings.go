package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	for _, c := range []*cobra.Command{findingsListCmd, findingsGetCmd} {
		c.Flags().StringVar(&findingsRepo, "repo", "", "Target repo as org/repo (default: the 'origin' remote)")
		c.Flags().BoolVar(&findingsJSON, "json", false, "Output raw JSON")
	}
	findingsListCmd.Flags().IntVar(&findingsLimit, "limit", 0, "Maximum findings to show (0 = all)")
	findingsCmd.AddCommand(findingsListCmd, findingsGetCmd)
	rootCmd.AddCommand(findingsCmd)
}
