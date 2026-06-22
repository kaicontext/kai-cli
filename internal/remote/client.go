// Package remote provides client functionality for communicating with Kailab servers.
package remote

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/kaicontext/kai-core/cas"
	"kai/internal/kaipath"
)

// DefaultServer is the production Kailab server URL.
// This is used when no explicit remote is configured.
// Can be overridden via KAI_SERVER environment variable.
// The app lives at app.kaicontext.com; kaicontext.com is the marketing site.
const DefaultServer = "https://atlas.kaicontext.com"

// Client communicates with a Kailab server.
type Client struct {
	BaseURL    string
	Tenant     string
	Repo       string
	HTTPClient *http.Client
	Actor      string
	AuthToken  string
	Message    string // Optional push message (e.g., git commit message)
}

// NewClient creates a new Kailab client.
// baseURL should be the server base (e.g., http://localhost:7447)
// tenant and repo specify the repository to operate on.
func NewClient(baseURL, tenant, repo string) *Client {
	// Try to load auth token
	token, _ := GetValidAccessToken()

	return &Client{
		BaseURL: baseURL,
		Tenant:  tenant,
		Repo:    repo,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		Actor:     os.Getenv("USER"),
		AuthToken: token,
	}
}

// repoPath returns the path prefix for repo-scoped endpoints.
func (c *Client) repoPath() string {
	return "/" + c.Tenant + "/" + c.Repo
}

// RepoPath returns the /{tenant}/{repo} path prefix.
func (c *Client) RepoPath() string {
	return c.repoPath()
}

// CommitLimitError is returned when the org has exceeded its commit limit.
type CommitLimitError struct {
	Tier       string
	Used       int
	Limit      int
	UpgradeURL string
}

func (e *CommitLimitError) Error() string {
	return fmt.Sprintf("commit limit reached: %d/%d on %s plan", e.Used, e.Limit, e.Tier)
}

// UsageResponse is the response from the billing usage API.
type UsageResponse struct {
	Tier         string  `json:"tier"`
	CommitsUsed  int     `json:"commits_used"`
	CommitsLimit int     `json:"commits_limit"`
	Period       string  `json:"period"`
	OverageRate  *string `json:"overage_rate"`
	UpgradeURL   *string `json:"upgrade_url"`
}

// GetUsage retrieves the current billing usage for an org.
func (c *Client) GetUsage(orgSlug string) (*UsageResponse, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/v1/billing/usage?org="+orgSlug, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// CatalogEntry mirrors the kailab-control side. Wire-format owned by
// the server; we keep field names in sync so json.Unmarshal works
// without translation.
type CatalogEntry struct {
	ID       string `json:"id"`
	Display  string `json:"display"`
	Provider string `json:"provider"`
	Tier     string `json:"tier"`
	Default  bool   `json:"default,omitempty"`
}

// ListModels fetches the kailab model catalog. Public endpoint —
// no auth needed. Cached for 5 minutes by the server's response
// header; if you want fresh data, hit it directly.
//
// Returns ErrNotFound (or HTTP error) when the server doesn't
// expose /api/v1/models — older kailab builds lack this endpoint;
// callers should fall back to a hardcoded default catalog so the
// CLI keeps working against pre-feature servers.
func (c *Client) ListModels() ([]CatalogEntry, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}
	var body struct {
		Models []CatalogEntry `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return body.Models, nil
}

// parseUsageHeaders extracts billing usage info from response headers.
func parseUsageHeaders(resp *http.Response) *UsageInfo {
	plan := resp.Header.Get("X-Kai-Plan")
	if plan == "" {
		return nil
	}
	used, _ := strconv.Atoi(resp.Header.Get("X-Kai-Usage"))
	limit, _ := strconv.Atoi(resp.Header.Get("X-Kai-Limit"))
	return &UsageInfo{
		Plan:        plan,
		Used:        used,
		Limit:       limit,
		UpgradeURL:  resp.Header.Get("X-Kai-Upgrade-URL"),
		OverageRate: resp.Header.Get("X-Kai-Overage-Rate"),
	}
}

// --- Wire types (matching kailab/proto/wire.go) ---

// NegotiateRequest is sent to negotiate which objects need pushing.
type NegotiateRequest struct {
	Digests [][]byte `json:"digests,omitempty"`
}

// NegotiateResponse tells the client which objects are missing.
type NegotiateResponse struct {
	Missing [][]byte `json:"missing"`
}

// PackIngestResponse is returned after ingesting a pack.
type PackIngestResponse struct {
	SegmentID int64 `json:"segmentId"`
	Indexed   int   `json:"indexedCount"`
}

// RefUpdateRequest updates a ref.
type RefUpdateRequest struct {
	Old   []byte `json:"old,omitempty"`
	New   []byte `json:"new"`
	Force bool   `json:"force,omitempty"`
}

// RefUpdateResponse is returned after updating a ref.
type RefUpdateResponse struct {
	OK        bool   `json:"ok"`
	UpdatedAt int64  `json:"updatedAt"`
	PushID    string `json:"pushId"`
	Error     string `json:"error,omitempty"`
}

// BatchRefUpdate represents a single ref update in a batch.
type BatchRefUpdate struct {
	Name  string            `json:"name"`
	Old   []byte            `json:"old,omitempty"`
	New   []byte            `json:"new"`
	Force bool              `json:"force,omitempty"`
	Meta  map[string]string `json:"meta,omitempty"`
}

// BatchRefUpdateRequest updates multiple refs atomically.
type BatchRefUpdateRequest struct {
	Updates []BatchRefUpdate `json:"updates"`
}

// BatchRefResult is the result for a single ref in a batch update.
type BatchRefResult struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

// BatchRefUpdateResponse is returned after updating multiple refs.
type BatchRefUpdateResponse struct {
	PushID  string           `json:"pushId"`
	Results []BatchRefResult `json:"results"`
	Usage   *UsageInfo       `json:"-"` // populated from response headers
}

// UsageInfo contains billing usage data returned via response headers.
type UsageInfo struct {
	Plan        string
	Used        int
	Limit       int
	UpgradeURL  string
	OverageRate string
}

// RefEntry represents a single ref.
type RefEntry struct {
	Name      string `json:"name"`
	Target    []byte `json:"target"`
	UpdatedAt int64  `json:"updatedAt"`
	Actor     string `json:"actor"`
}

// RefsListResponse contains refs.
type RefsListResponse struct {
	Refs []*RefEntry `json:"refs"`
}

// LogEntry represents a log entry.
type LogEntry struct {
	Kind     string `json:"kind"`
	ID       []byte `json:"id"`
	Parent   []byte `json:"parent,omitempty"`
	Time     int64  `json:"time"`
	Actor    string `json:"actor"`
	Ref      string `json:"ref,omitempty"`
	Old      []byte `json:"old,omitempty"`
	New      []byte `json:"new,omitempty"`
	NodeID   []byte `json:"nodeId,omitempty"`
	NodeKind string `json:"nodeKind,omitempty"`
}

// LogEntriesResponse contains log entries.
type LogEntriesResponse struct {
	Entries []*LogEntry `json:"entries"`
}

// LogHeadResponse returns the log head.
type LogHeadResponse struct {
	Head []byte `json:"head"`
}

// ErrorResponse is returned for API errors.
type ErrorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// --- API Methods ---

// Negotiate sends object digests and returns which are missing on the server.
func (c *Client) Negotiate(digests [][]byte) ([][]byte, error) {
	req := NegotiateRequest{Digests: digests}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	resp, err := c.post(c.repoPath()+"/v1/push/negotiate", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result NegotiateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return result.Missing, nil
}

// PushPack sends a pack of objects to the server.
// maxPackHeaderBytes keeps each pushed pack's (uncompressed) JSON header
// safely under the server's 10MB MaxHeaderSize. The header carries one entry
// per object, so a batch with very many small objects overflows it even when
// the content is well under the pack-size cap — that's the "header too large"
// ingest error seen on large stores. We split into header-bounded sub-batches.
const maxPackHeaderBytes = 8 * 1024 * 1024

// splitByHeaderBudget partitions objects so each partition's JSON pack header
// stays under maxHeaderBytes. Per-object header ≈ base64(digest) + kind +
// fixed JSON punctuation/offset/length; estimated conservatively.
func splitByHeaderBudget(objects []PackObject, maxHeaderBytes int) [][]PackObject {
	const perEntryOverhead = 80 // {"digest":"","kind":"","offset":N,"length":N},
	const skeleton = len(`{"objects":[]}`)
	var batches [][]PackObject
	var cur []PackObject
	curBytes := skeleton
	for _, o := range objects {
		entryBytes := (len(o.Digest)+2)/3*4 + len(o.Kind) + perEntryOverhead // base64 len + kind + overhead
		if len(cur) > 0 && curBytes+entryBytes > maxHeaderBytes {
			batches = append(batches, cur)
			cur = nil
			curBytes = skeleton
		}
		cur = append(cur, o)
		curBytes += entryBytes
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

func (c *Client) PushPack(objects []PackObject) (*PackIngestResponse, error) {
	batches := splitByHeaderBudget(objects, maxPackHeaderBytes)
	if len(batches) <= 1 {
		return c.pushPackOnce(objects)
	}
	agg := &PackIngestResponse{}
	for i, b := range batches {
		r, err := c.pushPackOnce(b)
		if err != nil {
			return nil, fmt.Errorf("pushing pack sub-batch %d/%d: %w", i+1, len(batches), err)
		}
		agg.SegmentID = r.SegmentID
		agg.Indexed += r.Indexed
	}
	return agg, nil
}

func (c *Client) pushPackOnce(objects []PackObject) (*PackIngestResponse, error) {
	pack, err := BuildPack(objects)
	if err != nil {
		return nil, fmt.Errorf("building pack: %w", err)
	}

	req, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/objects/pack", bytes.NewReader(pack))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Kailab-Actor", c.Actor)
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result PackIngestResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// UpdateRef updates a ref on the server.
func (c *Client) UpdateRef(name string, old, new []byte, force bool) (*RefUpdateResponse, error) {
	req := RefUpdateRequest{Old: old, New: new, Force: force}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequest("PUT", c.BaseURL+c.repoPath()+"/v1/refs/"+name, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Kailab-Actor", c.Actor)
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	var result RefUpdateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if resp.StatusCode == http.StatusConflict {
		return &result, fmt.Errorf("ref conflict: %s", result.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	return &result, nil
}

// BatchUpdateRefs updates multiple refs atomically in a single request.
func (c *Client) BatchUpdateRefs(updates []BatchRefUpdate) (*BatchRefUpdateResponse, error) {
	req := BatchRefUpdateRequest{Updates: updates}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/refs/batch", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Kailab-Actor", c.Actor)
	if c.Message != "" {
		httpReq.Header.Set("X-Kailab-Message", c.Message)
	}
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPaymentRequired {
		var limitErr struct {
			Error      string `json:"error"`
			Tier       string `json:"tier"`
			Limit      int    `json:"limit"`
			Used       int    `json:"used"`
			UpgradeURL string `json:"upgrade_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&limitErr); err == nil && limitErr.Error == "commit_limit_reached" {
			return nil, &CommitLimitError{
				Tier:       limitErr.Tier,
				Used:       limitErr.Used,
				Limit:      limitErr.Limit,
				UpgradeURL: limitErr.UpgradeURL,
			}
		}
		return nil, fmt.Errorf("payment required")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result BatchRefUpdateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	result.Usage = parseUsageHeaders(resp)

	return &result, nil
}

// GetRef retrieves a single ref.
func (c *Client) GetRef(name string) (*RefEntry, error) {
	resp, err := c.get(c.repoPath() + "/v1/refs/" + name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result RefEntry
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// ListRefs lists refs, optionally filtered by prefix.
func (c *Client) ListRefs(prefix string) ([]*RefEntry, error) {
	url := c.repoPath() + "/v1/refs"
	if prefix != "" {
		url += "?prefix=" + prefix
	}

	resp, err := c.get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result RefsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return result.Refs, nil
}

// GetObject retrieves a single object by digest.
func (c *Client) GetObject(digest []byte) ([]byte, string, error) {
	digestHex := hex.EncodeToString(digest)
	resp, err := c.get(c.repoPath() + "/v1/objects/" + digestHex)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", c.parseError(resp)
	}

	kind := resp.Header.Get("X-Kailab-Kind")
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading body: %w", err)
	}

	// Content is stored as "Kind\n{json...}" for digest verification.
	// Strip the kind prefix to return just the JSON payload.
	if idx := bytes.IndexByte(content, '\n'); idx >= 0 {
		content = content[idx+1:]
	}

	return content, kind, nil
}

// GetReviewComments fetches comments for a review from the server.
func (c *Client) GetReviewComments(reviewID string) ([]map[string]interface{}, error) {
	resp, err := c.get(c.repoPath() + "/v1/reviews/" + reviewID + "/comments")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result struct {
		Comments []map[string]interface{} `json:"comments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Comments, nil
}

// SnapshotFile represents a file in a remote snapshot.
type SnapshotFile struct {
	Path          string `json:"path"`
	Digest        string `json:"digest"`
	ContentDigest string `json:"contentDigest"`
	Lang          string `json:"lang"`
}

// SnapshotFilesResponse is the response from the files endpoint.
type SnapshotFilesResponse struct {
	SnapshotDigest string         `json:"snapshotDigest"`
	Files          []SnapshotFile `json:"files"`
}

// ListSnapshotFiles lists all files in a snapshot by ref name or hex digest.
func (c *Client) ListSnapshotFiles(refOrDigest string) (*SnapshotFilesResponse, error) {
	resp, err := c.get(c.repoPath() + "/v1/files/" + refOrDigest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result SnapshotFilesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// GetRawContent fetches raw file content by digest.
func (c *Client) GetRawContent(digest string) ([]byte, error) {
	resp, err := c.get(c.repoPath() + "/v1/raw/" + digest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("content not found: %s", digest)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	return io.ReadAll(resp.Body)
}

// GetLogHead returns the current log head.
func (c *Client) GetLogHead() ([]byte, error) {
	resp, err := c.get(c.repoPath() + "/v1/log/head")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result LogHeadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return result.Head, nil
}

// GetLogEntries retrieves log entries.
func (c *Client) GetLogEntries(refFilter string, afterSeq, limit int) ([]*LogEntry, error) {
	url := fmt.Sprintf(c.repoPath()+"/v1/log/entries?after=%d&limit=%d", afterSeq, limit)
	if refFilter != "" {
		url += "&ref=" + refFilter
	}

	resp, err := c.get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result LogEntriesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return result.Entries, nil
}

// Health checks if the server is healthy.
func (c *Client) Health() error {
	resp, err := c.get("/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// --- Helper methods ---

func (c *Client) get(path string) (*http.Response, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	return c.HTTPClient.Do(req)
}

func (c *Client) post(path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	return c.HTTPClient.Do(req)
}

func (c *Client) parseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var errResp ErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
		if errResp.Details != "" {
			return fmt.Errorf("%s: %s", errResp.Error, errResp.Details)
		}
		return fmt.Errorf("%s", errResp.Error)
	}
	return fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
}

// --- Pack building ---

// PackObject represents an object to pack.
type PackObject struct {
	Digest  []byte
	Kind    string
	Content []byte
}

// PackHeader describes objects in a pack.
type PackHeader struct {
	Objects []PackObjectEntry `json:"objects"`
}

// PackObjectEntry describes a single object in a pack.
type PackObjectEntry struct {
	Digest []byte `json:"digest"`
	Kind   string `json:"kind"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

const headerLengthSize = 4

// BuildPack creates a zstd-compressed pack.
func BuildPack(objects []PackObject) ([]byte, error) {
	var header PackHeader
	var data bytes.Buffer

	for _, obj := range objects {
		entry := PackObjectEntry{
			Digest: obj.Digest,
			Kind:   obj.Kind,
			Offset: int64(data.Len()),
			Length: int64(len(obj.Content)),
		}
		header.Objects = append(header.Objects, entry)
		data.Write(obj.Content)
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("marshaling header: %w", err)
	}

	var pack bytes.Buffer
	headerLen := make([]byte, headerLengthSize)
	binary.BigEndian.PutUint32(headerLen, uint32(len(headerJSON)))
	pack.Write(headerLen)
	pack.Write(headerJSON)
	pack.Write(data.Bytes())

	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		return nil, fmt.Errorf("creating encoder: %w", err)
	}
	if _, err := encoder.Write(pack.Bytes()); err != nil {
		encoder.Close()
		return nil, fmt.Errorf("compressing: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("closing encoder: %w", err)
	}

	return compressed.Bytes(), nil
}

// --- Config ---

// RemoteEntry holds configuration for a single remote.
type RemoteEntry struct {
	URL    string `json:"url"`
	Tenant string `json:"tenant"`
	Repo   string `json:"repo"`
}

// Config holds remote configuration.
type Config struct {
	Remotes map[string]*RemoteEntry `json:"remotes"` // name -> entry
}

// LocalConfigPath returns the project-local remote config path
// (<kaiDir>/remotes.json — typically .git/kai/remotes.json in a git
// repo, .kai/remotes.json otherwise). Resolves to an absolute path
// based on the current working directory.
func LocalConfigPath() string {
	rel := filepath.Join(kaipath.Resolve("."), "remotes.json")
	abs, err := filepath.Abs(rel)
	if err != nil {
		return rel
	}
	return abs
}

// GlobalConfigPath returns the global remote config path (~/.kai/remotes.json).
func GlobalConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kai", "remotes.json")
}

// ConfigPath returns the path to the remote config file.
// Prefers local .kai/remotes.json if it exists, falls back to global.
func ConfigPath() string {
	local := LocalConfigPath()
	if _, err := os.Stat(local); err == nil {
		return local
	}
	return GlobalConfigPath()
}

// loadConfigFromPath loads config from a specific path.
func loadConfigFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Try to migrate from old format (remotes were strings)
		var oldCfg struct {
			Remotes map[string]string `json:"remotes"`
		}
		if err2 := json.Unmarshal(data, &oldCfg); err2 == nil && len(oldCfg.Remotes) > 0 {
			cfg.Remotes = make(map[string]*RemoteEntry)
			for name, url := range oldCfg.Remotes {
				cfg.Remotes[name] = &RemoteEntry{
					URL:    url,
					Tenant: "default",
					Repo:   "main",
				}
			}
			return &cfg, nil
		}
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Remotes == nil {
		cfg.Remotes = make(map[string]*RemoteEntry)
	}
	return &cfg, nil
}

// LoadConfig loads the remote configuration from the local .kai/remotes.json only.
// No global fallback — each project must have its own remote config.
func LoadConfig() (*Config, error) {
	if cfg, err := loadConfigFromPath(LocalConfigPath()); err == nil {
		return cfg, nil
	}

	return &Config{Remotes: make(map[string]*RemoteEntry)}, nil
}

// SaveConfig saves the remote configuration to the project-local
// <kaiDir>/remotes.json. Returns an error if the kai data directory
// doesn't exist (project must be initialized first).
func SaveConfig(cfg *Config) error {
	path := LocalConfigPath()
	kaiAbs, _ := filepath.Abs(kaipath.Resolve("."))
	if _, err := os.Stat(kaiAbs); os.IsNotExist(err) {
		return fmt.Errorf("no kai data directory — run 'kai init' first")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// GetRemote gets the entry for a named remote.
// If the remote is not configured and the name is "origin", it falls back to:
// 1. KAI_SERVER environment variable
// 2. DefaultServer constant (production server)
func GetRemote(name string) (*RemoteEntry, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}

	entry, ok := cfg.Remotes[name]
	if !ok {
		// For "origin", fall back to default server
		if name == "origin" {
			serverURL := os.Getenv("KAI_SERVER")
			if serverURL == "" {
				serverURL = DefaultServer
			}
			return &RemoteEntry{
				URL:    serverURL,
				Tenant: "default",
				Repo:   "main",
			}, nil
		}
		return nil, fmt.Errorf("remote %q not configured", name)
	}
	return entry, nil
}

// GetRemoteURL gets the URL for a named remote (backwards compatible).
func GetRemoteURL(name string) (string, error) {
	entry, err := GetRemote(name)
	if err != nil {
		return "", err
	}
	return entry.URL, nil
}

// SetRemote sets the entry for a named remote.
// If the remote already exists, it is NOT overwritten.
// Use ForceSetRemote for explicit user changes (kai remote set).
func SetRemote(name string, entry *RemoteEntry) error {
	cfg, err := LoadConfig()
	if err != nil {
		cfg = &Config{Remotes: make(map[string]*RemoteEntry)}
	}

	if existing, ok := cfg.Remotes[name]; ok && existing != nil {
		return nil
	}

	cfg.Remotes[name] = entry
	return SaveConfig(cfg)
}

// ForceSetRemote overwrites the entry for a named remote.
// Use this only for explicit user actions (kai remote add, kai remote set-url).
func ForceSetRemote(name string, entry *RemoteEntry) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	cfg.Remotes[name] = entry
	return SaveConfig(cfg)
}

// SetRemoteURL sets the URL for a named remote with default tenant/repo.
func SetRemoteURL(name, url string) error {
	return SetRemote(name, &RemoteEntry{
		URL:    url,
		Tenant: "default",
		Repo:   "main",
	})
}

// NewClientForRemote creates a new client for a named remote.
func NewClientForRemote(name string) (*Client, error) {
	entry, err := GetRemote(name)
	if err != nil {
		return nil, err
	}
	return NewClient(entry.URL, entry.Tenant, entry.Repo), nil
}

// DeleteRemote deletes a named remote.
func DeleteRemote(name string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	if _, ok := cfg.Remotes[name]; !ok {
		return fmt.Errorf("remote %q not found", name)
	}

	delete(cfg.Remotes, name)
	return SaveConfig(cfg)
}

// ListRemotes returns all configured remotes.
func ListRemotes() (map[string]*RemoteEntry, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	return cfg.Remotes, nil
}

// CollectObjects collects all objects reachable from a set of node IDs.
// This is a helper for building packs - it traverses the graph to find all related objects.
func CollectObjects(db interface {
	GetNode([]byte) (interface{ GetPayload() map[string]interface{} }, error)
	GetEdges([]byte, string) ([]interface{ GetDstID() []byte }, error)
	ReadObject(string) ([]byte, error)
}, nodeIDs [][]byte) ([]PackObject, error) {
	visited := make(map[string]bool)
	var objects []PackObject

	var collect func([]byte) error
	collect = func(id []byte) error {
		idHex := hex.EncodeToString(id)
		if visited[idHex] {
			return nil
		}
		visited[idHex] = true

		// Get the node
		node, err := db.GetNode(id)
		if err != nil {
			return err
		}
		if node == nil {
			return nil
		}

		// Serialize the node as JSON
		payload := node.GetPayload()
		content, err := cas.CanonicalJSON(payload)
		if err != nil {
			return err
		}

		// Determine kind from the node
		kind := "node" // Default kind

		objects = append(objects, PackObject{
			Digest:  id,
			Kind:    kind,
			Content: content,
		})

		return nil
	}

	for _, id := range nodeIDs {
		if err := collect(id); err != nil {
			return nil, err
		}
	}

	return objects, nil
}

// EdgeData represents an edge to push to the server.
type EdgeData struct {
	Src  string `json:"src"`  // hex digest
	Type string `json:"type"` // IMPORTS, TESTS, etc.
	Dst  string `json:"dst"`  // hex digest
	At   string `json:"at"`   // hex digest (optional)
}

// PushEdgesResponse is the response from POST /edges.
type PushEdgesResponse struct {
	Inserted int `json:"inserted"`
}

// PushEdges sends edges to the server.
func (c *Client) PushEdges(edges []EdgeData) (*PushEdgesResponse, error) {
	if len(edges) == 0 {
		return &PushEdgesResponse{Inserted: 0}, nil
	}

	req := struct {
		Edges []EdgeData `json:"edges"`
	}{Edges: edges}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/edges", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Kailab-Actor", c.Actor)
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result PushEdgesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// --- Authorship ---

// AuthorshipData represents an authorship range for push/fetch.
type AuthorshipData struct {
	SnapshotID string `json:"snapshot_id"` // hex
	FilePath   string `json:"file_path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	AuthorType string `json:"author_type"`
	Agent      string `json:"agent"`
	Model      string `json:"model"`
	SessionID  string `json:"session_id"`
}

// PushAuthorshipResponse is the server response after ingesting authorship data.
type PushAuthorshipResponse struct {
	Inserted int `json:"inserted"`
}

// PushAuthorship sends authorship ranges to the server.
func (c *Client) PushAuthorship(data []AuthorshipData) (*PushAuthorshipResponse, error) {
	if len(data) == 0 {
		return &PushAuthorshipResponse{Inserted: 0}, nil
	}

	req := struct {
		Ranges []AuthorshipData `json:"ranges"`
	}{Ranges: data}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/authorship", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Kailab-Actor", c.Actor)
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result PushAuthorshipResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// --- Project Name Detection ---

// DetectProjectName attempts to detect the project name from various sources.
// It checks (in order): package.json, go.mod, Gemfile, Cargo.toml, pyproject.toml,
// setup.py, then falls back to the directory name.
func DetectProjectName() string {
	// Try git remote URL first — most universal and reliable
	if out, err := exec.Command("git", "remote", "get-url", "origin").Output(); err == nil {
		url := strings.TrimSpace(string(out))
		// Extract repo name from URL patterns:
		//   git@github.com:org/repo.git  →  repo
		//   https://github.com/org/repo.git  →  repo
		//   https://github.com/org/repo  →  repo
		name := url
		// Strip .git suffix
		name = strings.TrimSuffix(name, ".git")
		// Get last path segment
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		} else if idx := strings.LastIndex(name, ":"); idx >= 0 {
			// SSH format: git@host:org/repo
			name = name[idx+1:]
			if idx2 := strings.LastIndex(name, "/"); idx2 >= 0 {
				name = name[idx2+1:]
			}
		}
		if name != "" {
			return sanitizeRepoName(name)
		}
	}

	// Try package.json (Node.js)
	if data, err := os.ReadFile("package.json"); err == nil {
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Name != "" {
			// Handle scoped packages like @org/name
			name := pkg.Name
			if idx := bytes.LastIndexByte([]byte(name), '/'); idx >= 0 {
				name = name[idx+1:]
			}
			return sanitizeRepoName(name)
		}
	}

	// Try go.mod (Go)
	if data, err := os.ReadFile("go.mod"); err == nil {
		lines := bytes.Split(data, []byte("\n"))
		for _, line := range lines {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("module ")) {
				modPath := string(bytes.TrimSpace(line[7:]))
				// Extract last segment of module path
				if idx := bytes.LastIndexByte([]byte(modPath), '/'); idx >= 0 {
					return sanitizeRepoName(modPath[idx+1:])
				}
				return sanitizeRepoName(modPath)
			}
		}
	}

	// Try Gemfile with gemspec (Ruby)
	if _, err := os.Stat("Gemfile"); err == nil {
		// Look for .gemspec files
		entries, _ := os.ReadDir(".")
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".gemspec" {
				name := e.Name()
				return sanitizeRepoName(name[:len(name)-8]) // Remove .gemspec
			}
		}
	}

	// Try Cargo.toml (Rust)
	if data, err := os.ReadFile("Cargo.toml"); err == nil {
		lines := bytes.Split(data, []byte("\n"))
		inPackage := false
		for _, line := range lines {
			line = bytes.TrimSpace(line)
			if bytes.Equal(line, []byte("[package]")) {
				inPackage = true
				continue
			}
			if inPackage && bytes.HasPrefix(line, []byte("name")) {
				// Parse name = "..."
				if idx := bytes.Index(line, []byte("=")); idx >= 0 {
					value := bytes.TrimSpace(line[idx+1:])
					value = bytes.Trim(value, "\"'")
					return sanitizeRepoName(string(value))
				}
			}
			if inPackage && bytes.HasPrefix(line, []byte("[")) {
				break // End of [package] section
			}
		}
	}

	// Try pyproject.toml (Python)
	if data, err := os.ReadFile("pyproject.toml"); err == nil {
		lines := bytes.Split(data, []byte("\n"))
		inProject := false
		for _, line := range lines {
			line = bytes.TrimSpace(line)
			if bytes.Equal(line, []byte("[project]")) || bytes.Equal(line, []byte("[tool.poetry]")) {
				inProject = true
				continue
			}
			if inProject && bytes.HasPrefix(line, []byte("name")) {
				if idx := bytes.Index(line, []byte("=")); idx >= 0 {
					value := bytes.TrimSpace(line[idx+1:])
					value = bytes.Trim(value, "\"'")
					return sanitizeRepoName(string(value))
				}
			}
			if inProject && bytes.HasPrefix(line, []byte("[")) {
				break
			}
		}
	}

	// Try setup.py (Python legacy)
	if data, err := os.ReadFile("setup.py"); err == nil {
		// Look for name='...' or name="..."
		content := string(data)
		for _, prefix := range []string{"name='", "name=\"", "name = '", "name = \""} {
			if idx := bytes.Index([]byte(content), []byte(prefix)); idx >= 0 {
				start := idx + len(prefix)
				quote := content[start-1]
				if end := bytes.IndexByte([]byte(content[start:]), quote); end >= 0 {
					return sanitizeRepoName(content[start : start+end])
				}
			}
		}
	}

	// Fall back to directory name
	wd, err := os.Getwd()
	if err != nil {
		return "my-project"
	}
	return sanitizeRepoName(filepath.Base(wd))
}

// sanitizeRepoName cleans up a name to be valid as a repo name.
func sanitizeRepoName(name string) string {
	// Convert to lowercase
	name = strings.ToLower(name)
	// Replace invalid characters with hyphens
	var result []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			result = append(result, c)
		} else if c == ' ' || c == '.' {
			result = append(result, '-')
		}
	}
	// Remove leading/trailing hyphens
	return strings.Trim(string(result), "-")
}

// --- Organization and Repo API ---

// OrgInfo represents an organization.
type OrgInfo struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"`
	CreatedAt string `json:"created_at"`
}

// RepoInfo represents a repository.
type RepoInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	CreatedAt  string `json:"created_at"`
}

// ControlClient communicates with kailab-control for org/repo management.
type ControlClient struct {
	BaseURL    string
	HTTPClient *http.Client
	AuthToken  string
}

// NewControlClient creates a new control plane client.
func NewControlClient(baseURL string) *ControlClient {
	token, _ := GetValidAccessToken()
	return &ControlClient{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		AuthToken: token,
	}
}

// ListOrgs lists organizations the user belongs to.
func (c *ControlClient) ListOrgs() ([]OrgInfo, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/v1/orgs", nil)
	if err != nil {
		return nil, err
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("not authenticated")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
	}

	var result struct {
		Orgs []OrgInfo `json:"orgs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return result.Orgs, nil
}

// CreateOrg creates a new organization.
func (c *ControlClient) CreateOrg(slug, name string) (*OrgInfo, error) {
	body, _ := json.Marshal(map[string]string{"slug": slug, "name": name})
	req, err := http.NewRequest("POST", c.BaseURL+"/api/v1/orgs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
	}

	var result OrgInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// DeleteOrg hard-deletes an org, every repo in it, and all dependent
// data. Requires the caller to be the org owner. The server additionally
// requires ?confirm=<slug> — we pass it automatically.
//
// On success returns a summary: number of repos deleted.
func (c *ControlClient) DeleteOrg(slug string) (reposDeleted int, err error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s?confirm=%s", c.BaseURL, slug, slug)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return 0, err
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
	}
	var result struct {
		DeletedOrg   string `json:"deleted_org"`
		ReposDeleted int    `json:"repos_deleted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decoding response: %w", err)
	}
	return result.ReposDeleted, nil
}

// ListRepos lists repositories in an organization.
func (c *ControlClient) ListRepos(orgSlug string) ([]RepoInfo, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/v1/orgs/"+orgSlug+"/repos", nil)
	if err != nil {
		return nil, err
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
	}

	var result []RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return result, nil
}

// CreateRepo creates a new repository in an organization.
func (c *ControlClient) CreateRepo(orgSlug, name, visibility string) (*RepoInfo, error) {
	body, _ := json.Marshal(map[string]string{"name": name, "visibility": visibility})
	req, err := http.NewRequest("POST", c.BaseURL+"/api/v1/orgs/"+orgSlug+"/repos", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
	}

	var result RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// --- CI Protocol (per docs/protocol.md section 3) ---

// CIRun represents a workflow run from the remote server.
type CIRun struct {
	ID           string `json:"id"`
	RunNumber    int    `json:"run_number"`
	WorkflowName string `json:"workflow_name"`
	TriggerEvent string `json:"trigger_event"`
	TriggerRef   string `json:"trigger_ref"`
	TriggerSHA   string `json:"trigger_sha"`
	SnapshotID   string `json:"snapshot_id,omitempty"` // Kai snapshot ID (hex), set when CI was triggered by a push
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"created_at"`
	StartedAt    string `json:"started_at"`
	CompletedAt  string `json:"completed_at"`
}

// CIJob represents a job within a run.
type CIJob struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	Conclusion  string   `json:"conclusion"`
	ExitCode    *int     `json:"exit_code,omitempty"`
	StartedAt   string   `json:"started_at"`
	CompletedAt string   `json:"completed_at"`
	Steps       []CIStep `json:"steps,omitempty"`
}

// CIStep represents a step within a job.
type CIStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// CILogEntry represents a log chunk.
type CILogEntry struct {
	Content  string `json:"content"`
	ChunkSeq int   `json:"chunk_seq"`
}

func (c *ControlClient) ciGet(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

func (c *ControlClient) ciPost(path string) ([]byte, error) {
	req, err := http.NewRequest("POST", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// ListCIRuns lists workflow runs for a repo.
func (c *ControlClient) ListCIRuns(org, repo string, limit int) ([]CIRun, int, error) {
	data, err := c.ciGet(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs?limit=%d", org, repo, limit))
	if err != nil {
		return nil, 0, err
	}
	var result struct {
		Runs  []CIRun `json:"runs"`
		Total int     `json:"total"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, 0, err
	}
	return result.Runs, result.Total, nil
}

// GetCIRun gets a single workflow run.
func (c *ControlClient) GetCIRun(org, repo, runID string) (*CIRun, error) {
	data, err := c.ciGet(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs/%s", org, repo, runID))
	if err != nil {
		return nil, err
	}
	var run CIRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// ListCIJobs lists jobs for a run.
func (c *ControlClient) ListCIJobs(org, repo, runID string) ([]CIJob, error) {
	data, err := c.ciGet(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs/%s/jobs", org, repo, runID))
	if err != nil {
		return nil, err
	}
	var result struct {
		Jobs []CIJob `json:"jobs"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Jobs, nil
}

// GetCILogs gets logs for a job.
func (c *ControlClient) GetCILogs(org, repo, runID, jobID string) ([]CILogEntry, error) {
	data, err := c.ciGet(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs/%s/jobs/%s/logs", org, repo, runID, jobID))
	if err != nil {
		return nil, err
	}
	var result struct {
		Logs []CILogEntry `json:"logs"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Logs, nil
}

// GetCILogsSince gets logs for a job after a given sequence number (for incremental polling).
func (c *ControlClient) GetCILogsSince(org, repo, runID, jobID string, afterSeq int) ([]CILogEntry, error) {
	data, err := c.ciGet(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs/%s/jobs/%s/logs?after=%d", org, repo, runID, jobID, afterSeq))
	if err != nil {
		return nil, err
	}
	var result struct {
		Logs []CILogEntry `json:"logs"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Logs, nil
}

// CancelCIRun cancels a workflow run.
func (c *ControlClient) CancelCIRun(org, repo, runID string) error {
	_, err := c.ciPost(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs/%s/cancel", org, repo, runID))
	return err
}

// RerunCI re-runs a workflow run.
func (c *ControlClient) RerunCI(org, repo, runID string) (string, error) {
	data, err := c.ciPost(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/runs/%s/rerun", org, repo, runID))
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(data, &result)
	return result.ID, nil
}

// SetCISecret sets a CI secret.
func (c *ControlClient) SetCISecret(org, repo, name, value string) error {
	body, _ := json.Marshal(map[string]string{"value": value})
	req, err := http.NewRequest("PUT", c.BaseURL+fmt.Sprintf("/api/v1/orgs/%s/repos/%s/secrets/%s", org, repo, name), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ListCISecrets lists CI secret names.
func (c *ControlClient) ListCISecrets(org, repo string) ([]string, error) {
	data, err := c.ciGet(fmt.Sprintf("/api/v1/orgs/%s/repos/%s/secrets", org, repo))
	if err != nil {
		return nil, err
	}
	var result struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	names := make([]string, len(result.Secrets))
	for i, s := range result.Secrets {
		names[i] = s.Name
	}
	return names, nil
}

// ActivityFile represents a file in an activity heartbeat.
type ActivityFile struct {
	Path      string `json:"path"`
	Operation string `json:"op"`
	Timestamp int64  `json:"ts"`
}

// OverlapWarning indicates another agent is editing files related to yours.
type OverlapWarning struct {
	Agent     string `json:"agent"`
	Actor     string `json:"actor"`
	File      string `json:"file"`
	RelatedTo string `json:"relatedTo"`
	Relation  string `json:"relation"`
}

// PushActivity sends an activity heartbeat to the server.
// Returns any overlap warnings detected by the server.
func (c *Client) PushActivity(agent string, files []ActivityFile, relatedFiles []string) ([]OverlapWarning, error) {
	req := struct {
		Agent        string         `json:"agent"`
		Actor        string         `json:"actor"`
		Files        []ActivityFile `json:"files"`
		RelatedFiles []string       `json:"relatedFiles,omitempty"`
	}{Agent: agent, Actor: c.Actor, Files: files, RelatedFiles: relatedFiles}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling activity: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/activity", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Kailab-Actor", c.Actor)
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending activity: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("activity heartbeat failed: %d", resp.StatusCode)
	}

	var result struct {
		OK       bool             `json:"ok"`
		Warnings []OverlapWarning `json:"warnings,omitempty"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Warnings, nil
}

// FileLock represents an active advisory lock visible to clients.
type FileLock struct {
	Path  string `json:"path"`
	Agent string `json:"agent"`
	Actor string `json:"actor"`
	Since int64  `json:"since"`
	Ago   string `json:"ago,omitempty"`
}

// LockDenied indicates a file is already locked by another agent.
type LockDenied struct {
	Path  string `json:"path"`
	Agent string `json:"agent"`
	Actor string `json:"actor"`
}

// AcquireLocks requests advisory locks on files.
func (c *Client) AcquireLocks(agent string, files []string) (acquired []string, denied []LockDenied, err error) {
	req := struct {
		Agent string   `json:"agent"`
		Actor string   `json:"actor"`
		Files []string `json:"files"`
	}{Agent: agent, Actor: c.Actor, Files: files}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling lock request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/locks", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Kailab-Actor", c.Actor)
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("sending lock request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("lock request failed: %d", resp.StatusCode)
	}

	var result struct {
		Acquired []string     `json:"acquired"`
		Denied   []LockDenied `json:"denied"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Acquired, result.Denied, nil
}

// ReleaseLocks releases advisory locks on files.
func (c *Client) ReleaseLocks(agent string, files []string) error {
	req := struct {
		Agent string   `json:"agent"`
		Files []string `json:"files"`
	}{Agent: agent, Files: files}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling unlock request: %w", err)
	}

	httpReq, err := http.NewRequest("DELETE", c.BaseURL+c.repoPath()+"/v1/locks", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending unlock request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unlock request failed: %d", resp.StatusCode)
	}
	return nil
}

// EdgeSyncEntry represents a single edge change from another agent.
type EdgeSyncEntry struct {
	Seq      int64  `json:"seq"`
	Agent    string `json:"agent"`
	Actor    string `json:"actor"`
	Time     int64  `json:"time"`
	File     string `json:"file"`
	Action   string `json:"action"`
	Src      string `json:"src"`
	EdgeType string `json:"edge_type"`
	Dst      string `json:"dst"`
}

// EdgeSyncResponse contains edge changes since a sequence number.
type EdgeSyncResponse struct {
	Entries   []EdgeSyncEntry `json:"entries"`
	LatestSeq int64           `json:"latest_seq"`
	HasMore   bool            `json:"has_more"`
}

// SyncEdges fetches edge changes from other agents since the given sequence number.
func (c *Client) SyncEdges(sinceSeq int64, agent string) (*EdgeSyncResponse, error) {
	url := fmt.Sprintf("%s%s/v1/edges/sync?since=%d&agent=%s&limit=200",
		c.BaseURL, c.repoPath(), sinceSeq, agent)

	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating sync request: %w", err)
	}
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("syncing edges: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("edge sync failed: %d", resp.StatusCode)
	}

	var result EdgeSyncResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return &result, nil
}

// SyncSubscribeResponse confirms a live sync subscription.
type SyncSubscribeResponse struct {
	ChannelID string `json:"channel_id"`
	ExpiresAt int64  `json:"expires_at"`
}

// SyncPushFile pushes a file change with content to the live sync channel.
// Uses reason="live_push" by default. For semantic checkpoints emitted by
// kai_checkpoint_now or similar, use SyncPushFileReason to override.
func (c *Client) SyncPushFile(agent, channelID, filePath, digest, contentBase64, baseContentBase64 string) error {
	return c.SyncPushFileReason(agent, channelID, filePath, digest, contentBase64, "", "", baseContentBase64)
}

// SyncPushFileReason is SyncPushFile with explicit reason/label fields.
// reason="" means the server will default to "live_push". baseContentBase64 is
// the common-ancestor content the edit was based on; the server uses it to
// 3-way merge against the current canonical (empty = last-write relay).
func (c *Client) SyncPushFileReason(agent, channelID, filePath, digest, contentBase64, reason, label, baseContentBase64 string) error {
	req := map[string]interface{}{
		"agent":   agent,
		"channel": channelID,
		"file":    filePath,
		"digest":  digest,
		"content": contentBase64,
	}
	if baseContentBase64 != "" {
		req["base_content"] = baseContentBase64
	}
	if reason != "" {
		req["reason"] = reason
	}
	if label != "" {
		req["label"] = label
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/sync/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPaymentRequired {
		var limitErr struct {
			Error      string `json:"error"`
			Tier       string `json:"tier"`
			Limit      int    `json:"limit"`
			Used       int    `json:"used"`
			UpgradeURL string `json:"upgrade_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&limitErr); err == nil && limitErr.Error == "commit_limit_reached" {
			return &CommitLimitError{
				Tier:       limitErr.Tier,
				Used:       limitErr.Used,
				Limit:      limitErr.Limit,
				UpgradeURL: limitErr.UpgradeURL,
			}
		}
		return fmt.Errorf("sync limit reached")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sync push failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// SyncPushOps pushes a batch of CRDT ops for a file (op-transport sync). The
// server relays the ops verbatim (no merge) — convergence happens at each
// client's RGA. opsBase64 is base64(crdt.MarshalOps(...)). The "ops" flag tells
// the server to store+broadcast without running the semantic 3-way merge.
func (c *Client) SyncPushOps(agent, channelID, filePath, opsBase64 string) error {
	req := map[string]interface{}{
		"agent":   agent,
		"channel": channelID,
		"file":    filePath,
		"content": opsBase64,
		"ops":     true,
		"reason":  "live_ops",
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/sync/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPaymentRequired {
		var limitErr struct {
			Error      string `json:"error"`
			Tier       string `json:"tier"`
			Limit      int    `json:"limit"`
			Used       int    `json:"used"`
			UpgradeURL string `json:"upgrade_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&limitErr); err == nil && limitErr.Error == "commit_limit_reached" {
			return &CommitLimitError{Tier: limitErr.Tier, Used: limitErr.Used, Limit: limitErr.Limit, UpgradeURL: limitErr.UpgradeURL}
		}
		return fmt.Errorf("sync limit reached")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sync ops push failed: HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// SyncPushMilestone pushes an agent milestone event with optional trust assertions.
// Used by kai_checkpoint_now. Separate from SyncPushFileReason to avoid signature churn.
func (c *Client) SyncPushMilestone(agent, channelID, label, assert, planHash string) error {
	req := map[string]interface{}{
		"agent":   agent,
		"channel": channelID,
		"file":    ".kai/checkpoint",
		"digest":  "",
		"content": "",
		"reason":  "agent_milestone",
	}
	if label != "" {
		req["label"] = label
	}
	if assert != "" {
		req["assert"] = assert
	}
	if planHash != "" {
		req["plan_hash"] = planHash
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/sync/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("milestone push failed: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SubscribeSync registers for live sync events.
func (c *Client) SubscribeSync(agent, actor, workspace string, files []string) (*SyncSubscribeResponse, error) {
	filter := map[string]interface{}{}
	if len(files) > 0 {
		filter["files"] = files
	} else {
		filter["all"] = true
	}
	req := map[string]interface{}{
		"agent":  agent,
		"actor":  actor,
		"filter": filter,
	}
	// Per-workspace scoping: peers only sync with others on the same workspace.
	// Empty workspace ("main"/no checkout) is omitted, so the server treats it
	// as repo-wide — backward compatible with servers that don't read it.
	if workspace != "" {
		req["workspace"] = workspace
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/sync/subscribe", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Kailab-Actor", actor)
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("subscribing to sync: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sync subscribe failed: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result SyncSubscribeResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return &result, nil
}

// SyncLiveSummary is the response from /v1/sync/live: a rollup of how far
// ahead the uncompacted sync state is from the last snapshot, without
// transferring file content.
type SyncLiveSummary struct {
	TipSeq       int64    `json:"tip_seq"`
	SinceSeq     int64    `json:"since_seq"`
	EventCount   int64    `json:"event_count"`
	FileCount    int64    `json:"file_count"`
	AgentCount   int64    `json:"agent_count"`
	FirstEventTs int64    `json:"first_event_ts"`
	LastEventTs  int64    `json:"last_event_ts"`
	SnapLatest   string   `json:"snap_latest"`
	Files        []string `json:"files"`
}

// GetSyncLive fetches the live-state summary for the current repo. Read-only;
// does not transfer content. Useful for "how far ahead am I right now?"
// checks before deciding to kai capture or review --live.
func (c *Client) GetSyncLive(sinceSeq int64, includeFiles bool, workspace string, limit int) (*SyncLiveSummary, error) {
	filesParam := "true"
	if !includeFiles {
		filesParam = "false"
	}
	if limit <= 0 {
		limit = 100
	}
	url := fmt.Sprintf("%s%s/v1/sync/live?since=%d&files=%s&workspace=%s&limit=%d",
		c.BaseURL, c.repoPath(), sinceSeq, filesParam, workspace, limit)
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sync live request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sync live failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out SyncLiveSummary
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sync live decode: %w", err)
	}
	return &out, nil
}

// SyncReplayEvent is one durable sync event returned by the replay endpoint.
type SyncReplayEvent struct {
	Seq       int64  `json:"seq"`
	ParentSeq int64  `json:"parent_seq,omitempty"`
	File      string `json:"file"`
	Digest    string `json:"digest,omitempty"`
	Content   string `json:"content,omitempty"` // base64
	Agent     string `json:"agent"`
	Channel   string `json:"channel,omitempty"`
	Reason    string `json:"reason"`
	Label     string `json:"label,omitempty"`
	Time      int64  `json:"time"`
	Ops       bool   `json:"ops,omitempty"` // Content is a CRDT op-batch, not whole-file bytes
}

// SyncReplaySinceResponse is the response from /v1/sync/events/replay.
type SyncReplaySinceResponse struct {
	Events    []SyncReplayEvent `json:"events"`
	LatestSeq int64             `json:"latest_seq"`
}

// SyncReplaySince fetches durable sync events with seq > sinceSeq from the
// server's append-only sync_events log. Used by late-joining subscribers
// to catch up on pushes that happened before they subscribed. Pass
// excludeAgent to skip replaying the caller's own pushes.
func (c *Client) SyncReplaySince(sinceSeq int64, excludeAgent, workspace string, limit int) (*SyncReplaySinceResponse, error) {
	if limit <= 0 {
		limit = 500
	}
	url := fmt.Sprintf("%s%s/v1/sync/replay?since=%d&agent=%s&workspace=%s&limit=%d",
		c.BaseURL, c.repoPath(), sinceSeq, excludeAgent, workspace, limit)
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sync replay request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sync replay failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out SyncReplaySinceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sync replay decode: %w", err)
	}
	return &out, nil
}

// UnsubscribeSync removes a live sync subscription.
func (c *Client) UnsubscribeSync(channelID string) error {
	httpReq, err := http.NewRequest("DELETE", c.BaseURL+c.repoPath()+"/v1/sync/subscribe/"+channelID, nil)
	if err != nil {
		return err
	}
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("unsubscribing from sync: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

// EdgeDelta represents a single edge to add or remove.
type EdgeDelta struct {
	Src  string `json:"src"`  // hex node ID
	Type string `json:"type"` // IMPORTS, CALLS, TESTS, DEFINES_IN
	Dst  string `json:"dst"`  // hex node ID
}

// IncrementalEdgeUpdate represents edge changes for a single file.
type IncrementalEdgeUpdate struct {
	File         string      `json:"file"`
	AddedEdges   []EdgeDelta `json:"added_edges,omitempty"`
	RemovedEdges []EdgeDelta `json:"removed_edges,omitempty"`
}

// PushEdgesIncremental sends edge deltas to the server, anchored to the given
// snapshot digest. The snapshot becomes the `at` context of every inserted
// edge; the server rejects the request without it.
func (c *Client) PushEdgesIncremental(snapshot []byte, updates []IncrementalEdgeUpdate) (int, error) {
	req := struct {
		Snapshot string                  `json:"snapshot"`
		Updates  []IncrementalEdgeUpdate `json:"updates"`
	}{Snapshot: hex.EncodeToString(snapshot), Updates: updates}

	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("marshaling edge deltas: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+c.repoPath()+"/v1/edges/incremental", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("sending edge deltas: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("edge delta push failed: %d", resp.StatusCode)
	}

	var result struct {
		Applied int `json:"applied"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Applied, nil
}
