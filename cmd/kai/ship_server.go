package main

// kai ship --server: publish through the kailab control-plane publisher
// instead of local git. The request carries the changeset delta as
// per-file content; the server fetches the recorded base commit,
// applies the delta, commits with session trailers, force-pushes the
// kai/ branch via the GitHub App, and opens a draft PR. This is the
// path for spawned workspaces (whose git repo is an orphan `git init`
// with no remote) and for any machine without GitHub credentials.
//
// The base commit resolution order mirrors the design doc: the spawn
// registry's recorded BaseGitSHA when cwd is a spawn, else the current
// git HEAD. Either way the server fetches exactly that commit — if
// GitHub doesn't serve it (e.g. an unpushed local HEAD), the ship
// fails visibly with a prescription rather than publishing onto a
// different base.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/gitio"
	"github.com/kaicontext/kai-engine/remote"
	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// Server-side caps, mirrored client-side so an oversized delta fails
// here with a clear prescription instead of a 413 after the upload.
const (
	shipServerMaxFiles     = 1000
	shipServerMaxFileBytes = 1 << 20
	shipServerMaxTotal     = 16 << 20
)

type shipServerFile struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64,omitempty"`
	Delete     bool   `json:"delete,omitempty"`
}

type shipServerRequest struct {
	SessionID    string           `json:"session_id,omitempty"`
	Workspace    string           `json:"workspace,omitempty"`
	Branch       string           `json:"branch"`
	BaseGitSHA   string           `json:"base_git_sha"`
	BaseSnapshot string           `json:"base_snapshot,omitempty"`
	HeadSnapshot string           `json:"head_snapshot,omitempty"`
	Title        string           `json:"title,omitempty"`
	Body         string           `json:"body,omitempty"`
	Ready        bool             `json:"ready"`
	Files        []shipServerFile `json:"files"`
}

type shipServerStatus struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	PRNumber  int    `json:"pr_number,omitempty"`
	PRURL     string `json:"pr_url,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	// Overlaps are the server's concurrency advisories: other sessions'
	// recent ships that touched some of the same files.
	Overlaps []struct {
		Branch   string   `json:"branch"`
		PRNumber int      `json:"pr_number,omitempty"`
		PRURL    string   `json:"pr_url,omitempty"`
		Files    []string `json:"files"`
	} `json:"overlaps,omitempty"`
}

// runShipServer is the --server arm of runShip: collect the delta,
// enqueue it on the control plane, and poll the handle to completion.
func runShipServer(cwd, branch, sessionID string) error {
	baseURL, token, org, repoName, err := resolveShipServerTarget()
	if err != nil {
		return err
	}

	// Delta = everything the session did vs its BASELINE. In a spawn
	// that is baseline-commit-vs-tree — agents COMMIT their work there
	// (the Last-Mile discipline), so the dirty set alone reads a
	// disciplined session as "nothing to ship". In a plain checkout the
	// baseline is HEAD and the dirty set is the delta, as before.
	changed, err := shipDeltaNames(cwd)
	if err != nil {
		return fmt.Errorf("not a git-backed tree — `kai ship --server` needs the spawn's baseline repo or a git checkout: %w", err)
	}
	changed = gitio.FilterKaiArtifacts(changed)
	if len(changed) == 0 {
		return fmt.Errorf("nothing to ship — the working tree has no changes")
	}
	if len(changed) > shipServerMaxFiles {
		return fmt.Errorf("%d files exceeds the server-ship cap of %d — use `kai ship` locally", len(changed), shipServerMaxFiles)
	}

	// Base commit: the spawn registry's recorded anchor wins; a plain
	// checkout ships against its own HEAD.
	baseSHA, baseSnapshot := shipServerBase(cwd)
	if baseSHA == "" {
		return fmt.Errorf("no git anchor: this tree has no recorded base commit (spawn registry) and no resolvable HEAD — ship locally instead")
	}

	total := 0
	files := make([]shipServerFile, 0, len(changed))
	for _, p := range changed {
		abs := filepath.Join(cwd, filepath.FromSlash(p))
		content, rerr := os.ReadFile(abs)
		if os.IsNotExist(rerr) {
			files = append(files, shipServerFile{Path: p, Delete: true})
			continue
		}
		if rerr != nil {
			return fmt.Errorf("reading %s: %w", p, rerr)
		}
		if len(content) > shipServerMaxFileBytes {
			return fmt.Errorf("%s is %dKB — over the server-ship per-file cap (%dMB); use `kai ship` locally",
				p, len(content)>>10, shipServerMaxFileBytes>>20)
		}
		total += len(content)
		files = append(files, shipServerFile{Path: p, ContentB64: base64.StdEncoding.EncodeToString(content)})
	}
	if total > shipServerMaxTotal {
		return fmt.Errorf("delta is %dMB — over the server-ship cap (%dMB); use `kai ship` locally", total>>20, shipServerMaxTotal>>20)
	}

	payload := shipServerRequest{
		SessionID:    sessionID,
		Workspace:    shipServerWorkspace(cwd),
		Branch:       branch,
		BaseGitSHA:   baseSHA,
		BaseSnapshot: baseSnapshot,
		HeadSnapshot: shipSnapshotHex(cwd),
		Title:        shipTitle,
		Body:         shipPRBody(branch, sessionID, "", changed),
		Ready:        shipReady,
		Files:        files,
	}

	if shipDryRun {
		fmt.Printf("would server-ship %d file(s) on %s (base %.12s) to %s/%s via %s\n",
			len(files), branch, baseSHA, org, repoName, baseURL)
		for _, f := range files {
			mark := ""
			if f.Delete {
				mark = " (delete)"
			}
			fmt.Printf("  %s%s\n", f.Path, mark)
		}
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/orgs/%s/repos/%s/ship", org, repoName)
	st, err := shipServerCall(baseURL, token, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	fmt.Printf("queued ship %s on %s/%s (branch %s)\n", st.ID, org, repoName, branch)

	// Poll to completion. The row is durable — a ^C here loses nothing;
	// `kai ship status` semantics live behind the same GET.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if time.Now().After(deadline) {
			fmt.Printf("still publishing — check later: GET %s/%s\n", path, st.ID)
			return nil
		}
		time.Sleep(3 * time.Second)
		st, err = shipServerCall(baseURL, token, http.MethodGet, path+"/"+st.ID, nil)
		if err != nil {
			return fmt.Errorf("polling ship: %w", err)
		}
		switch st.Status {
		case "done":
			fmt.Printf("shipped: PR #%d %s (commit %.12s)\n", st.PRNumber, st.PRURL, st.CommitSHA)
			// The dirty-main fix: the shipped changes' home is the PR
			// now. --clean stashes exactly those paths (recoverable),
			// returning the tree to pristine; without it, say how.
			if shipClean {
				msg := fmt.Sprintf("kai ship: PR #%d (%s) — `git stash pop` restores", st.PRNumber, branch)
				switch err := gitio.StashPushPaths(cwd, msg, changed); {
				case err == nil:
					fmt.Println("workspace clean — shipped changes stashed; pull main after the PR merges")
				case errors.Is(err, gitio.ErrNothingToStash):
					fmt.Println("workspace already clean")
				default:
					fmt.Printf("shipped, but the clean failed (changes are safe on the PR): %v\n", err)
				}
			} else {
				fmt.Println("tip: --clean stashes the shipped changes so main stays pristine (git stash pop restores)")
			}
			// Advisory, not an error: sessions are isolated by branch;
			// overlap just means the merges will interact.
			for _, o := range st.Overlaps {
				ref := o.Branch
				if o.PRURL != "" {
					ref = fmt.Sprintf("PR #%d (%s)", o.PRNumber, o.PRURL)
				}
				fmt.Printf("note: %s recently shipped changes to %s\n", ref, strings.Join(o.Files, ", "))
			}
			return nil
		case "failed":
			return fmt.Errorf("ship failed: %s", st.Error)
		}
	}
}

// resolveShipServerTarget mirrors the findings CLI: control-plane base
// URL + login token, kai org/repo from --repo or the origin remote.
func resolveShipServerTarget() (baseURL, token, org, repoName string, err error) {
	baseURL = os.Getenv("KAI_SERVER")
	if baseURL == "" {
		baseURL = remote.DefaultServer
	}
	baseURL = strings.TrimRight(baseURL, "/")

	token, _ = remote.GetValidAccessToken()
	if token == "" {
		return "", "", "", "", fmt.Errorf("not logged in — run `kai login` first (or use `kai ship` locally)")
	}

	if shipRepo != "" {
		o, r, ok := strings.Cut(shipRepo, "/")
		if !ok || o == "" || r == "" {
			return "", "", "", "", fmt.Errorf("--repo must be kai org/repo for --server")
		}
		return baseURL, token, o, r, nil
	}
	// The tracked .kai.yaml marker is the repo's DESIGNED identity —
	// it survives clones and spawns, unlike remotes.json (a spawn's
	// copy has been observed carrying the wrong repo). Marker first,
	// remote entry as fallback.
	if o, r := kaiMarkerIdentity(); o != "" && r != "" {
		return baseURL, token, o, r, nil
	}
	entry, gerr := remote.GetRemote("origin")
	if gerr != nil || entry == nil || entry.Tenant == "" || entry.Repo == "" {
		return "", "", "", "", fmt.Errorf("no kai repo: pass --repo org/repo or set an origin remote (`kai remote set origin …`)")
	}
	return baseURL, token, entry.Tenant, entry.Repo, nil
}

// shipServerBase resolves the base git commit the delta applies onto:
// the spawn registry's recorded anchor for a spawn dir, else this
// tree's HEAD.
func shipServerBase(cwd string) (sha, baseSnapshot string) {
	if reg, err := spawnpkg.Load(); err == nil {
		resolved, _ := filepath.EvalSymlinks(cwd)
		for _, e := range reg.Spawned {
			p, _ := filepath.EvalSymlinks(e.Path)
			if e.Path == cwd || (resolved != "" && p == resolved) {
				return e.BaseGitSHA, e.SourceSnapshot
			}
		}
	}
	head, _ := spawnpkg.GitHeadState(cwd)
	return head, ""
}

// shipServerWorkspace best-effort names the workspace for provenance.
func shipServerWorkspace(cwd string) string {
	if ws, err := getCurrentWorkspace(); err == nil && ws != "" {
		return ws
	}
	return ""
}

// shipServerCall performs one authenticated control-plane call and
// decodes the ship status payload.
func shipServerCall(baseURL, token, method, path string, body []byte) (*shipServerStatus, error) {
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
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("unauthorized (401) — your session may have expired; run `kai login`")
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("forbidden (403) — shipping needs the developer role on this repo")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var st shipServerStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &st, nil
}

// shipDeltaNames lists the paths of the session's delta: baseline-vs-
// tree when this dir is a registered spawn (kai-baseline tag, falling
// back to the root/materialization commit), dirty-vs-HEAD otherwise.
func shipDeltaNames(cwd string) ([]string, error) {
	isSpawn := false
	if reg, err := spawnpkg.Load(); err == nil {
		resolved, _ := filepath.EvalSymlinks(cwd)
		for _, e := range reg.Spawned {
			p, _ := filepath.EvalSymlinks(e.Path)
			if e.Path == cwd || (resolved != "" && p == resolved) {
				isSpawn = true
				break
			}
		}
	}
	if !isSpawn {
		return gitio.DirtyPaths(cwd)
	}
	base := "HEAD"
	if out, err := gitOut(cwd, "rev-parse", "-q", "--verify", "kai-baseline^{commit}"); err == nil && out != "" {
		base = out
	} else if out, err := gitOut(cwd, "rev-list", "--max-parents=0", "HEAD"); err == nil {
		roots := strings.Fields(out)
		if len(roots) > 0 {
			base = roots[len(roots)-1]
		}
	}
	out, err := gitOut(cwd, "diff", "--name-only", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, p := range strings.Split(out, "\n") {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		names = append(names, p)
	}
	if un, err := gitOut(cwd, "ls-files", "--others", "--exclude-standard"); err == nil {
		for _, p := range strings.Split(un, "\n") {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			names = append(names, p)
		}
	}
	return names, nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// kaiMarkerIdentity reads org:/repo: from the cwd's tracked .kai.yaml
// marker (line-based on purpose — no YAML dependency, mirroring the
// desktop's reader).
func kaiMarkerIdentity() (org, repo string) {
	raw, err := os.ReadFile(".kai.yaml")
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "org:"); ok {
			org = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "repo:"); ok {
			repo = strings.TrimSpace(v)
		}
	}
	return org, repo
}
