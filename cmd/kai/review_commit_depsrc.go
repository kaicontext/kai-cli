package main

// review_commit_depsrc.go — reading the contract instead of guessing at it.
//
// review_commit_deps.go names the modules a diff moves and tells the reviewer
// that an unread contract is a limitation, not a defect. That is the honest
// floor. This is the part that makes the floor unnecessary when it can.
//
// TUI#92 bumped kai-engine to v0.6.59-0.20260908192613-5ab1b102fc2f and moved
// three call sites onto kaipath.UserPath. The review then spent most of its
// output asking whether that function is variadic, whether it accepts zero
// trailing components, and whether an empty override preserves the default —
// and called the arity question "a real defect". The answer was 17 lines long,
// and the commit holding it was written in the diff the reviewer was handed.
//
// So fetch it. The pod has no Go toolchain and no module cache (see
// review_commit_deps.go), but it does have GITHUB_TOKEN — an installation
// token for an app installed across the org — and the GitHub contents API
// serves a directory at an exact commit. That is the whole mechanism.
//
// WHAT IT WILL NOT DO. It fetches only the packages the diff actually imports,
// only from github.com, only within hard byte and file caps, and only inside
// one short deadline. A review that spends ninety seconds pulling a large
// dependency has taken that time from reading the change itself, and the point
// of preflight is to buy turns, not sell them. Everything it does not fetch —
// too big, no token, third-party, a 404 — falls back to the limitation block,
// which is why the two are rendered from the same result.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	// rcDepFetchBudget bounds the whole preflight, every request together.
	// Turn 0 waits on this, so it is deliberately short: a dependency that
	// cannot be read in ten seconds is one the reviewer states as a limit.
	rcDepFetchBudget = 10 * time.Second
	// rcDepMaxFiles and rcDepMaxBytes bound what lands in the prompt. The
	// contract a diff rests on is usually one small file; a package that
	// needs more than this is not something to inline into every turn.
	rcDepMaxFiles = 6
	rcDepMaxBytes = 48 << 10
	// rcDepMaxPkgFetch bounds how many packages are fetched per review.
	rcDepMaxPkgFetch = 3
)

// rcDepSource is one file read out of a moved dependency, at the exact commit
// the diff pins.
type rcDepSource struct {
	Module string
	Pkg    string
	Path   string // path within the repo, e.g. kaipath/user.go
	Body   string
}

// rcGitHubRepo maps a module path to an owner/repo pair.
//
// Only the plain three-segment github.com form. A module with a major-version
// suffix, a module living in a subdirectory of its repo, or anything not on
// github.com is left alone: guessing a repo from a module path is how you
// fetch the wrong file and state it with confidence, which is worse than
// fetching nothing.
func rcGitHubRepo(module string) (owner, repo string, ok bool) {
	parts := strings.Split(module, "/")
	if len(parts) != 3 || parts[0] != "github.com" {
		return "", "", false
	}
	if parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// rcGHContents fetches one contents-API path at a ref. Returns the raw JSON so
// the caller can decode either shape the endpoint serves — an object for a
// file, an array for a directory.
func rcGHContents(ctx context.Context, token, owner, repo, p, ref string) ([]byte, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", owner, repo, p, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 covers both "no such path" and "this token cannot see this
		// repo", and the caller treats them the same: it did not read it.
		return nil, fmt.Errorf("contents %s/%s/%s@%s: %s", owner, repo, p, ref, resp.Status)
	}
	return io.ReadAll(http.MaxBytesReader(nil, resp.Body, rcDepMaxBytes*4))
}

// ghContentEntry is the subset of a contents-API entry this needs.
type ghContentEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	Size     int    `json:"size"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// rcFetchPkg reads the Go source of one package directory at a commit.
//
// Test files are skipped: they are usually the largest thing in a package and
// the reviewer is asking what the contract IS, not how it is exercised.
func rcFetchPkg(ctx context.Context, token, owner, repo, pkg, ref string, budget *int) []rcDepSource {
	raw, err := rcGHContents(ctx, token, owner, repo, pkg, ref)
	if err != nil {
		return nil
	}
	var entries []ghContentEntry
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Size < entries[j].Size })

	var out []rcDepSource
	for _, e := range entries {
		if len(out) >= rcDepMaxFiles || *budget <= 0 {
			break
		}
		if e.Type != "file" || !strings.HasSuffix(e.Name, ".go") || strings.HasSuffix(e.Name, "_test.go") {
			continue
		}
		if e.Size <= 0 || e.Size > *budget {
			continue
		}
		body := e.Content
		if body == "" {
			one, err := rcGHContents(ctx, token, owner, repo, e.Path, ref)
			if err != nil {
				continue
			}
			var f ghContentEntry
			if json.Unmarshal(one, &f) != nil {
				continue
			}
			body, e.Encoding = f.Content, f.Encoding
		}
		if e.Encoding == "base64" {
			dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\n", ""))
			if err != nil {
				continue
			}
			body = string(dec)
		}
		if body == "" {
			continue
		}
		*budget -= len(body)
		out = append(out, rcDepSource{Pkg: pkg, Path: e.Path, Body: body})
	}
	return out
}

// rcFetchDepSources resolves what it can of the moved dependencies and reports
// which ones it could not.
//
// The two returns are rendered together on purpose. A block saying "you cannot
// read these" beside one containing the source would be its own contradiction,
// so whatever is fetched here is removed from what the limitation block claims.
func rcFetchDepSources(ctx context.Context, deps []rcDepChange) (got []rcDepSource, unresolved []rcDepChange) {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	ctx, cancel := context.WithTimeout(ctx, rcDepFetchBudget)
	defer cancel()

	budget := rcDepMaxBytes
	fetched := 0
	for _, d := range deps {
		owner, repo, ok := rcGitHubRepo(d.Module)
		// Without a commit there is no exact source to ask for: a version tag
		// may not exist on the default branch, and reading the wrong revision
		// of a contract is worse than reading none.
		if !ok || d.Commit == "" || len(d.Pkgs) == 0 || token == "" {
			unresolved = append(unresolved, d)
			continue
		}
		var forDep []rcDepSource
		for _, pkg := range d.Pkgs {
			if fetched >= rcDepMaxPkgFetch || budget <= 0 {
				break
			}
			src := rcFetchPkg(ctx, token, owner, repo, pkg, d.Commit, &budget)
			if len(src) == 0 {
				continue
			}
			fetched++
			for i := range src {
				src[i].Module = d.Module
			}
			forDep = append(forDep, src...)
		}
		if len(forDep) == 0 {
			unresolved = append(unresolved, d)
			continue
		}
		got = append(got, forDep...)
	}
	return got, unresolved
}

// rcDepSourceBlock renders the fetched source into the prompt, in the same
// voice as the identifier lookups: an answer already obtained, not a place to
// go looking.
func rcDepSourceBlock(got []rcDepSource) string {
	if len(got) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("SOURCE FROM THE DEPENDENCIES THIS DIFF MOVES (fetched at the pinned commit before\n")
	b.WriteString("this review started — treat as already read; do not go looking for it, and do not\n")
	b.WriteString("say you could not verify these contracts, because they are printed here):\n\n")
	for _, s := range got {
		fmt.Fprintf(&b, "--- %s/%s (package %s) ---\n", s.Module, path.Base(s.Path), s.Pkg)
		b.WriteString(strings.TrimRight(s.Body, "\n"))
		b.WriteString("\n\n")
	}
	return b.String()
}
