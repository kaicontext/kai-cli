package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kaicontext/kai-engine/remote"
)

// usageView is the server's usage answer as `kai usage` shows it: the fields
// kai-engine's remote.UsageResponse already reads, plus the review breakdowns
// a newer server adds. Read here rather than in kai-engine so the CLI can
// show them without an engine release. An older server sends neither, and
// the output is what it was apart from the seats line.
type usageView struct {
	remote.UsageResponse
	// Present only while review depths are on for the org, and only for its
	// members.
	ReviewUsage *usageReviews `json:"review_usage"`
	// Present only while Free runs on Google models for the org and its
	// reviews are paid from the free allowance.
	FreeReviews *usageFree `json:"free_reviews"`
}

// usageReviews is what each review depth, and the repos that used the most,
// took of this month's allowance. Shares only, never amounts.
type usageReviews struct {
	Allowance string       `json:"allowance"` // "org" or "billing_person"
	ByTier    []usageShare `json:"by_tier"`
	ByRepo    []usageShare `json:"by_repo"`
	PaidFrom  []usagePot   `json:"paid_from"`
}

type usageShare struct {
	Tier     string  `json:"tier"`
	Name     string  `json:"name"`
	Repo     string  `json:"repo"`
	SharePct float64 `json:"share_pct"`
	Reviews  int     `json:"reviews"`
}

type usagePot struct {
	Source  string `json:"source"`
	Reviews int    `json:"reviews"`
}

// usageFree is the Free lines: the plan's name, how the one allowance splits
// between the org's reviews and the billing person's own use, and about how
// many pull requests are left (absent when the server can't yet say).
type usageFree struct {
	Label           string  `json:"label"`
	ReviewsSharePct float64 `json:"reviews_share_pct"`
	OwnUseSharePct  float64 `json:"own_use_share_pct"`
	PRsLeft         *int    `json:"prs_left"`
	BilledToYou     bool    `json:"billed_to_you"`
}

// fetchUsage reads GET /api/v1/billing/usage the way remote.Client.GetUsage
// does, errors included, into a usageView.
func fetchUsage(c *remote.Client, org string) (*usageView, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/v1/billing/usage?org="+url.QueryEscape(org), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		var e remote.ErrorResponse
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			if e.Details != "" {
				return nil, fmt.Errorf("%s: %s", e.Error, e.Details)
			}
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
	}
	var v usageView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &v, nil
}

// paidPlan reports whether the org is on a plan with seats. Any plan but
// Free has them; the CLI used to show seats for "pro" only.
func paidPlan(tier string) bool {
	return tier != "" && tier != "free"
}

// seatLabel names the viewer's seat by the org's plan: "Max seat", not
// "Pro seat" for everyone.
func seatLabel(tier string) string {
	if !paidPlan(tier) {
		return "seat"
	}
	return strings.ToUpper(tier[:1]) + tier[1:] + " seat"
}

// planLabel is the plan as the Plan line names it.
func planLabel(u *usageView) string {
	if u.FreeReviews != nil && u.FreeReviews.Label != "" {
		return u.FreeReviews.Label
	}
	return u.Tier
}

// fmtShare is a share of the allowance: one decimal below 10%, whole numbers
// above.
func fmtShare(p float64) string {
	switch {
	case p > 0 && p < 0.1:
		return "<0.1%"
	case p < 10:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", p), ".0") + "%"
	}
	return fmt.Sprintf("%.0f%%", p)
}

func usagePlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// printFreeLines prints the Free lines, when the server sent them.
func printFreeLines(w io.Writer, org string, u *usageView) {
	f := u.FreeReviews
	if f == nil {
		return
	}
	fmt.Fprintln(w)
	if f.PRsLeft != nil {
		fmt.Fprintf(w, "  Free:    about %s left this month\n", usagePlural(*f.PRsLeft, "pull request", "pull requests"))
	}
	own := "the billing person's own use"
	if f.BilledToYou {
		own = "your own use"
	}
	fmt.Fprintf(w, "  Split:   reviews of %s %s · %s %s\n", org, fmtShare(f.ReviewsSharePct), own, fmtShare(f.OwnUseSharePct))
}

// printReviewUsage prints what each depth and the top repos took of this
// month's allowance, when the server sent it.
func printReviewUsage(w io.Writer, u *usageView) {
	r := u.ReviewUsage
	if r == nil {
		return
	}
	fmt.Fprintln(w)
	if len(r.ByTier) == 0 {
		fmt.Fprintln(w, "  Reviews this month: none on the allowance yet")
	} else {
		whose := "the allowance your reviews are charged to"
		if r.Allowance == "org" {
			whose = "the org's shared budget"
		}
		fmt.Fprintf(w, "  Reviews this month (share of %s):\n", whose)
		for _, t := range r.ByTier {
			name := t.Name
			if name == "" {
				name = t.Tier
			}
			fmt.Fprintf(w, "    %-12s %6s  %s\n", name, fmtShare(t.SharePct), usagePlural(t.Reviews, "review", "reviews"))
		}
		if len(r.ByRepo) > 0 {
			fmt.Fprintln(w, "  Top repos:")
			for _, p := range r.ByRepo {
				fmt.Fprintf(w, "    %-24s %6s  %s\n", p.Repo, fmtShare(p.SharePct), usagePlural(p.Reviews, "review", "reviews"))
			}
		}
	}
	credit := 0
	for _, p := range r.PaidFrom {
		if p.Source == "user_credit" || p.Source == "org_credit" {
			credit += p.Reviews
		}
	}
	if credit > 0 {
		verb := "were"
		if credit == 1 {
			verb = "was"
		}
		fmt.Fprintf(w, "  %s %s paid from credit and not counted here\n", usagePlural(credit, "review", "reviews"), verb)
	}
}
