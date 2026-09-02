package main

// kai review list/view --server: read the control plane's change
// reviews — the acceptance object a `kai ship --server` publish opens
// (kai-server schema/0055). The local `kai review` commands operate on
// the graph's own Review nodes; --server reads the server's row for the
// same idea, which is the one the desktop and Atlas show.

import (
	"bytes"
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
	return serverCallRaw(baseURL, token, http.MethodGet, path, nil)
}

func serverCallRaw(baseURL, token, method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	case resp.StatusCode == http.StatusConflict:
		return nil, fmt.Errorf("refused (409): %s", strings.TrimSpace(string(raw)))
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

// changeReviewVerbSegments maps a verb onto the server's route.
var changeReviewVerbSegments = map[string]string{
	"approve":         "approve",
	"request_changes": "request-changes",
	"reopen":          "reopen",
	"comment":         "comments",
	"land":            "land",
	"set_approve":     "set/approve",
	"set_land":        "set/land",
}

type serverVerbResponse struct {
	Review      serverChangeReview `json:"review"`
	Mirrored    bool               `json:"mirrored"`
	MirrorError string             `json:"mirror_error"`
	// land only
	LandSHA string `json:"land_sha"`
	Base    string `json:"base"`
	Rebased bool   `json:"rebased"`
	// set verbs only
	OK      bool                     `json:"ok"`
	Members []map[string]interface{} `json:"members"`
	Landed  []map[string]interface{} `json:"landed"`
	Failed  map[string]interface{}   `json:"failed"`
	Held    []string                 `json:"held"`
}

// runReviewVerbServer records a reviewer's action on the server's change
// review and says how it fared on GitHub. Kai's row is written before the
// mirror is attempted, so a mirror failure is reported, not fatal.
func runReviewVerbServer(id, verb, body, file string, line int) error {
	seg, ok := changeReviewVerbSegments[verb]
	if !ok {
		return fmt.Errorf("unknown review action %q", verb)
	}
	baseURL, token, org, repoName, err := resolveServerTarget(reviewRepo)
	if err != nil {
		return err
	}
	payload := map[string]any{"body": strings.TrimSpace(body)}
	if file != "" {
		payload["file"] = file
		if line > 0 {
			payload["line"] = line
		}
	}
	enc, _ := json.Marshal(payload)
	raw, err := serverCallRaw(baseURL, token, http.MethodPost,
		fmt.Sprintf("/api/v1/orgs/%s/repos/%s/changes/%s/%s", org, repoName, id, seg), enc)
	if err != nil {
		return err
	}
	if reviewJSON {
		os.Stdout.Write(raw)
		fmt.Println()
		return nil
	}
	var out serverVerbResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Print(formatVerbOutcome(verb, &out))
	return nil
}

func formatVerbOutcome(verb string, out *serverVerbResponse) string {
	if verb == "set_approve" || verb == "set_land" {
		return formatSetOutcome(verb, out)
	}
	what := map[string]string{
		"approve": "approved", "request_changes": "changes requested", "reopen": "reopened", "comment": "comment recorded", "land": "landed",
	}[verb]
	line := fmt.Sprintf("Review %s: %s (state %s)", shortReviewID(out.Review.ID), what, out.Review.State)
	switch {
	case verb == "land":
		line += fmt.Sprintf(" — %s is now %.12s", out.Base, out.LandSHA)
		if out.Rebased {
			line += " (replayed onto the moved base)"
		}
	case out.Mirrored:
		line += " — mirrored to GitHub"
	case out.MirrorError != "":
		line += " — recorded in Kai, but GitHub was not updated: " + out.MirrorError
	}
	return line + "\n"
}

// formatSetOutcome renders a set verb: one line per member, then the
// verdict on the whole.
func formatSetOutcome(verb string, out *serverVerbResponse) string {
	var b strings.Builder
	str := func(m map[string]interface{}, k string) string {
		if v, ok := m[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	if verb == "set_approve" {
		for _, m := range out.Members {
			line := fmt.Sprintf("  %-28s %s", str(m, "repo"), str(m, "state"))
			if s := str(m, "skipped"); s != "" {
				line += "  (" + s + ")"
			}
			if e := str(m, "error"); e != "" {
				line += "  — " + e
			}
			b.WriteString(line + "\n")
		}
		if out.OK {
			b.WriteString("Every member is approved.\n")
		} else {
			b.WriteString("Not every member could be approved — see above.\n")
		}
		return b.String()
	}
	for _, m := range out.Landed {
		line := fmt.Sprintf("  %-28s landed %.12s", str(m, "repo"), str(m, "land_sha"))
		if s := str(m, "skipped"); s != "" {
			line = fmt.Sprintf("  %-28s %s", str(m, "repo"), s)
		}
		b.WriteString(line + "\n")
	}
	if out.Failed != nil {
		b.WriteString(fmt.Sprintf("  %-28s FAILED — %s\n", str(out.Failed, "repo"), str(out.Failed, "error")))
	}
	for _, r := range out.Held {
		b.WriteString(fmt.Sprintf("  %-28s held (not attempted)\n", r))
	}
	if out.OK {
		b.WriteString("Every member landed, in order.\n")
	} else {
		b.WriteString("Stopped at the first member that did not land; the rest are held. Fix it and run again — landed members are skipped.\n")
	}
	return b.String()
}

// reviewUseServer decides where a review command acts. --server forces
// the control plane; --local forces the graph; otherwise the server is
// used when this checkout can name a kai repo and the caller is logged
// in — the server's change review is the one the desktop and Atlas
// show, so it is the default answer to "where does this change stand".
func reviewUseServer() bool {
	if reviewServer {
		return true
	}
	if reviewLocal {
		return false
	}
	_, _, _, _, err := resolveServerTarget(reviewRepo)
	return err == nil
}
