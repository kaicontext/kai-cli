package main

// kai review list/view --server: read the control plane's change
// reviews — the acceptance object a `kai ship --server` publish opens
// (kai-server schema/0055). The local `kai review` commands operate on
// the graph's own Review nodes; --server reads the server's row for the
// same idea, which is the one the desktop and Atlas show.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type serverChangeReview struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Mode       string `json:"mode"`
	Title      string `json:"title"`
	Branch     string `json:"branch"`
	SessionID  string `json:"session_id"`
	BaseGitSHA string `json:"base_git_sha"`
	HeadSHA    string `json:"head_sha"`
	PRNumber   int    `json:"pr_number"`
	PRURL      string `json:"pr_url"`
	LandSHA    string `json:"land_sha"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

type serverChangeEvent struct {
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Actor     string `json:"actor"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

type serverChangeFinding struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Verdict string `json:"verdict"`
	Risk    int    `json:"risk"`
}

type serverChangeDetail struct {
	serverChangeReview
	Events   []serverChangeEvent   `json:"events"`
	Findings []serverChangeFinding `json:"findings"`
}

// serverGetRaw performs one authenticated GET and returns the body, with
// the same status-to-prescription mapping shipServerCall uses.
func serverGetRaw(baseURL, token, path string) ([]byte, error) {
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("unauthorized (401) — your session may have expired; run `kai login`")
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("forbidden (403) — reading reviews needs membership of this repo")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("not found (404) — no such review, or this server does not serve change reviews yet")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func runReviewListServer() error {
	baseURL, token, org, repoName, err := resolveServerTarget(reviewRepo)
	if err != nil {
		return err
	}
	raw, err := serverGetRaw(baseURL, token, fmt.Sprintf("/api/v1/orgs/%s/repos/%s/changes", org, repoName))
	if err != nil {
		return err
	}
	if reviewJSON {
		os.Stdout.Write(raw)
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			fmt.Println()
		}
		return nil
	}
	var out struct {
		Reviews []serverChangeReview `json:"reviews"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(out.Reviews) == 0 {
		fmt.Printf("No change reviews on %s/%s yet — one opens when a session ships.\n", org, repoName)
		return nil
	}
	fmt.Print(formatChangeReviewTable(out.Reviews))
	return nil
}

// formatChangeReviewTable renders the list the way `kai review list`
// renders local reviews: one line per review, id first.
func formatChangeReviewTable(reviews []serverChangeReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-12s  %-18s  %-8s  %-30s  %s\n", "ID", "STATE", "PR", "TITLE", "BRANCH")
	b.WriteString(strings.Repeat("-", 90))
	b.WriteString("\n")
	for _, r := range reviews {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		if len(title) > 30 {
			title = title[:27] + "..."
		}
		pr := "-"
		if r.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", r.PRNumber)
		}
		fmt.Fprintf(&b, "%-12s  %-18s  %-8s  %-30s  %s\n", shortReviewID(r.ID), r.State, pr, title, r.Branch)
	}
	return b.String()
}

func shortReviewID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func runReviewViewServer(id string) error {
	baseURL, token, org, repoName, err := resolveServerTarget(reviewRepo)
	if err != nil {
		return err
	}
	raw, err := serverGetRaw(baseURL, token, fmt.Sprintf("/api/v1/orgs/%s/repos/%s/changes/%s", org, repoName, id))
	if err != nil {
		return err
	}
	if reviewJSON {
		os.Stdout.Write(raw)
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			fmt.Println()
		}
		return nil
	}
	var d serverChangeDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Print(formatChangeReviewDetail(&d))
	return nil
}

func formatChangeReviewDetail(d *serverChangeDetail) string {
	var b strings.Builder
	title := d.Title
	if title == "" {
		title = d.Branch
	}
	fmt.Fprintf(&b, "%s\n", title)
	fmt.Fprintf(&b, "  id:      %s\n", d.ID)
	fmt.Fprintf(&b, "  state:   %s\n", d.State)
	if d.Mode != "" {
		fmt.Fprintf(&b, "  mode:    %s\n", d.Mode)
	}
	fmt.Fprintf(&b, "  branch:  %s\n", d.Branch)
	if d.BaseGitSHA != "" || d.HeadSHA != "" {
		fmt.Fprintf(&b, "  commits: %.12s → %.12s\n", d.BaseGitSHA, d.HeadSHA)
	}
	if d.PRNumber > 0 {
		fmt.Fprintf(&b, "  pr:      #%d %s\n", d.PRNumber, d.PRURL)
	}
	if d.LandSHA != "" {
		fmt.Fprintf(&b, "  landed:  %.12s\n", d.LandSHA)
	}
	if d.SessionID != "" {
		fmt.Fprintf(&b, "  session: %s\n", d.SessionID)
	}
	if len(d.Findings) > 0 {
		b.WriteString("\nFindings\n")
		for _, f := range d.Findings {
			t := f.Title
			if t == "" {
				t = "(untitled)"
			}
			fmt.Fprintf(&b, "  %-12s  %-9s  %d risk  %s\n", shortReviewID(f.ID), f.Verdict, f.Risk, t)
		}
	}
	if len(d.Events) > 0 {
		b.WriteString("\nActivity\n")
		for _, ev := range d.Events {
			when := ""
			if ev.CreatedAt > 0 {
				when = time.Unix(ev.CreatedAt, 0).Local().Format("2006-01-02 15:04")
			}
			what := ev.Body
			if ev.Kind == "state" {
				if ev.FromState != "" {
					what = ev.FromState + " → " + ev.ToState
				} else {
					what = "opened as " + ev.ToState
				}
			}
			who := ev.Actor
			if ev.Source == "github" {
				who = strings.TrimSpace(who + " via GitHub")
			}
			if who != "" {
				what += "  (" + who + ")"
			}
			fmt.Fprintf(&b, "  %-16s  %s\n", when, what)
		}
	}
	return b.String()
}
