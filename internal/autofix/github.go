// Package autofix implements kai's headless issue→branch→fix→PR loop.
//
// github.go is a minimal GitHub REST v3 client covering exactly what the
// loop needs: read an issue, list candidate issues, open a draft PR, and
// post comments/labels. It deliberately reuses the raw-HTTP + Bearer-token
// shape already proven in cmd/kai's CI comment poster rather than pulling
// in go-github, so the dependency surface stays flat.
package autofix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

// Client talks to one repository's GitHub REST API.
type Client struct {
	Token string // a PAT or Actions token with repo + pull-request scope
	Repo  string // "owner/name"
	HTTP  *http.Client
}

// NewClient builds a client. Token falls back to $GITHUB_TOKEN; repo
// falls back to $GITHUB_REPOSITORY (the env GitHub Actions sets). Both
// must resolve to non-empty or NewClient errors — a headless run with no
// credentials should fail loudly, not half-act.
func NewClient(token, repo string) (*Client, error) {
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if repo == "" {
		repo = os.Getenv("GITHUB_REPOSITORY")
	}
	if token == "" {
		return nil, fmt.Errorf("no GitHub token (set --token or $GITHUB_TOKEN)")
	}
	if !strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repo must be owner/name, got %q (set --repo or $GITHUB_REPOSITORY)", repo)
	}
	return &Client{
		Token: token,
		Repo:  repo,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// RepoSlugFromRemote turns a git remote URL into an "owner/name" slug.
// Handles both SSH (git@github.com:owner/name.git) and HTTPS
// (https://github.com/owner/name.git) forms. Returns "" on no match.
func RepoSlugFromRemote(url string) string {
	url = strings.TrimSpace(url)
	re := regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$`)
	m := re.FindStringSubmatch(url)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// Issue is the subset of a GitHub issue the fixer reads.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"`
	Labels []Label  `json:"labels"`
	HTML   string   `json:"html_url"`
	PRLink *PRStub  `json:"pull_request,omitempty"` // present ⇒ this "issue" is really a PR
}

// Label is a GitHub label name wrapper.
type Label struct {
	Name string `json:"name"`
}

// PRStub marks issues that are actually pull requests (the issues API
// returns both); the loop skips any issue whose PRLink is non-nil.
type PRStub struct {
	URL string `json:"url"`
}

// LabelNames flattens an issue's labels to a string slice.
func (i Issue) LabelNames() []string {
	out := make([]string, len(i.Labels))
	for k, l := range i.Labels {
		out[k] = l.Name
	}
	return out
}

func (c *Client) do(method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, apiBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(req)
}

// decode reads a successful (2xx) JSON body into v, or returns an error
// carrying the HTTP status and body for any non-2xx response.
func decode(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if v == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// FetchIssue reads a single issue by number.
func (c *Client) FetchIssue(num int) (*Issue, error) {
	resp, err := c.do("GET", fmt.Sprintf("/repos/%s/issues/%d", c.Repo, num), nil)
	if err != nil {
		return nil, err
	}
	var iss Issue
	if err := decode(resp, &iss); err != nil {
		return nil, err
	}
	if iss.PRLink != nil {
		return nil, fmt.Errorf("#%d is a pull request, not an issue", num)
	}
	return &iss, nil
}

// ListOpenIssues returns open issues carrying the given label (empty
// label ⇒ all open issues), excluding pull requests.
func (c *Client) ListOpenIssues(label string) ([]Issue, error) {
	path := fmt.Sprintf("/repos/%s/issues?state=open&per_page=100", c.Repo)
	if label != "" {
		path += "&labels=" + label
	}
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var all []Issue
	if err := decode(resp, &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, iss := range all {
		if iss.PRLink == nil { // drop PRs that the issues endpoint mixes in
			out = append(out, iss)
		}
	}
	return out, nil
}

// PR is the subset of a created/listed pull request we surface.
type PR struct {
	Number int    `json:"number"`
	HTML   string `json:"html_url"`
	Draft  bool   `json:"draft"`
	State  string `json:"state"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// CreatePRInput is the payload for opening a pull request.
type CreatePRInput struct {
	Title string `json:"title"`
	Head  string `json:"head"` // branch with the changes
	Base  string `json:"base"` // branch to merge into
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// CreatePR opens a pull request and returns its number and URL.
func (c *Client) CreatePR(in CreatePRInput) (*PR, error) {
	resp, err := c.do("POST", fmt.Sprintf("/repos/%s/pulls", c.Repo), in)
	if err != nil {
		return nil, err
	}
	var pr PR
	if err := decode(resp, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// FindOpenPRForHead returns the open PR whose head branch matches, or nil
// if none — the idempotency check that stops the loop re-opening a PR for
// an issue it already has one for. head is just the branch name; the API
// wants owner-qualified, which we build from the repo owner.
func (c *Client) FindOpenPRForHead(head string) (*PR, error) {
	owner := strings.SplitN(c.Repo, "/", 2)[0]
	path := fmt.Sprintf("/repos/%s/pulls?state=open&head=%s:%s", c.Repo, owner, head)
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var prs []PR
	if err := decode(resp, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// Comment posts a comment on an issue or PR (same endpoint for both).
func (c *Client) Comment(issueOrPR int, body string) error {
	resp, err := c.do("POST",
		fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, issueOrPR),
		map[string]string{"body": body})
	if err != nil {
		return err
	}
	return decode(resp, nil)
}

// AddLabels adds labels to an issue or PR (used to mark an issue claimed
// so concurrent runners don't both pick it up).
func (c *Client) AddLabels(issueOrPR int, labels ...string) error {
	resp, err := c.do("POST",
		fmt.Sprintf("/repos/%s/issues/%d/labels", c.Repo, issueOrPR),
		map[string][]string{"labels": labels})
	if err != nil {
		return err
	}
	return decode(resp, nil)
}
