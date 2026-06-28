// Package livesync implements bidirectional peer file-sync for a workspace:
// a client subscribes to a sync channel, applies incoming peer edits to
// local disk (with 3-way merge), and pushes its own edits up. The engine
// is transport/UI-agnostic — the caller owns the file watcher and feeds
// changed paths to PushChanges; the engine owns the receive loop, the
// merge/conflict logic, the feedback-loop suppression, and the channel
// lifecycle.
//
// This was lifted out of internal/mcp/server.go so the MCP server and a
// plain CLI runner (`kai live run`) share one implementation rather than
// maintaining two divergent copies.
package livesync

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lukechampine.com/blake3"

	"github.com/kaicontext/kai-core/crdt"
	"github.com/kaicontext/kai-core/merge"
	"kai/internal/authorship"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/ref"
	"kai/internal/remote"
	"kai/pkg/synclog"
)

// ConflictInfo is a surfaced sync conflict (peer edits that could not be
// auto-merged into locally-modified content). Exposed via Conflicts().
type ConflictInfo struct {
	File    string `json:"file"`
	Agent   string `json:"agent"`
	Time    string `json:"time"`
	Message string `json:"message"`
}

// persistedSyncState is the on-disk `<kaiDir>/sync-state.json` shape. Kept
// byte-compatible with cmd/kai's liveSyncState so `kai live on/off` and the
// engine agree on the file.
type persistedSyncState struct {
	Enabled bool     `json:"enabled"`
	Files   []string `json:"files,omitempty"`
	LastSeq int64    `json:"last_seq,omitempty"`
}

// Options configures a new Engine.
type Options struct {
	WorkDir   string
	KaiDir    string
	DB        *graph.DB
	Resolver  *ref.Resolver
	Client    *remote.Client
	Agent     string // base agent name, e.g. "kai-cli" / "mcp-client"
	SessionID string // unique per process; syncAgentName = Agent:SessionID
	Workspace string // current workspace ("" = repo-wide, backward compatible)
	Log       *synclog.SyncLogWriter
	// CheckpointWriter is optional. When set, peer-originated edits are
	// attributed via authorship checkpoints so kai blame reflects who wrote
	// each line without waiting for kai capture.
	CheckpointWriter *authorship.CheckpointWriter
	// CRDTMode selects op-transport (line-RGA) sync instead of whole-file +
	// server-merge. The CLI peer-sync path defaults this on (the cutover); the
	// MCP/agent path leaves it off for now. There is no opt-in env var anymore;
	// the CLI exposes only an emergency rollback (KAI_DISABLE_CRDT_SYNC=1).
	CRDTMode bool
}

// Engine owns a single live-sync session.
type Engine struct {
	workDir   string
	kaiDir    string
	db        *graph.DB
	resolver  *ref.Resolver
	client    *remote.Client
	agent     string
	syncAgent string
	workspace string
	log       *synclog.SyncLogWriter
	cpWriter  *authorship.CheckpointWriter

	channelID string
	stopSSE   chan struct{}

	baseMu sync.RWMutex
	base   map[string][]byte // path -> content at last sync point (3-way merge base)

	writtenMu sync.Mutex
	written   map[string]time.Time // path -> time written by sync (feedback-loop skip)

	conflictsMu sync.RWMutex
	conflicts   []ConflictInfo

	billingMu sync.RWMutex
	billing   string

	// live-synced ledger (path -> base/peer digests), persisted to
	// .kai/live-synced.json so `kai capture` can subtract peer changes.
	ledgerMu sync.Mutex
	ledger   map[string]SyncedEntry

	// CRDT op-transport mode (opt-in via KAI_CRDT_SYNC=1). When set, the engine
	// syncs RGA ops instead of whole-file content: convergence happens at each
	// client's per-file Doc rather than via server-side 3-way merge.
	crdtMode bool
	docsMu   sync.Mutex
	docs     map[string]*crdt.Doc // path -> RGA replica
}

// New constructs an Engine. It does not touch the network until Start.
func New(opts Options) *Engine {
	agent := opts.Agent
	if agent == "" {
		agent = "kai-cli"
	}
	syncAgent := agent
	if opts.SessionID != "" {
		syncAgent = agent + ":" + opts.SessionID
	}
	log := opts.Log
	if log == nil {
		log = synclog.NewSyncLogWriter(opts.KaiDir)
	}
	return &Engine{
		workDir:   opts.WorkDir,
		kaiDir:    opts.KaiDir,
		db:        opts.DB,
		resolver:  opts.Resolver,
		client:    opts.Client,
		agent:     agent,
		syncAgent: syncAgent,
		workspace: opts.Workspace,
		log:       log,
		cpWriter:  opts.CheckpointWriter,
		crdtMode:  opts.CRDTMode,
		docs:      make(map[string]*crdt.Doc),
	}
}

// ChannelID returns the active channel ID, or "" when not subscribed.
func (e *Engine) ChannelID() string { return e.channelID }

// SyncAgentName returns the session-unique agent name used on the channel.
func (e *Engine) SyncAgentName() string { return e.syncAgent }

// TrackedFiles returns how many files have a recorded sync base.
func (e *Engine) TrackedFiles() int {
	e.baseMu.RLock()
	defer e.baseMu.RUnlock()
	return len(e.base)
}

// Conflicts returns a snapshot of surfaced conflicts.
func (e *Engine) Conflicts() []ConflictInfo {
	e.conflictsMu.RLock()
	defer e.conflictsMu.RUnlock()
	out := make([]ConflictInfo, len(e.conflicts))
	copy(out, e.conflicts)
	return out
}

// BillingWarning returns a non-empty string if sync was paused by a usage limit.
func (e *Engine) BillingWarning() string {
	e.billingMu.RLock()
	defer e.billingMu.RUnlock()
	return e.billing
}

// Start subscribes to the sync channel, performs the initial pull + durable
// replay, and launches the background SSE receive loop. Returns the number of
// files written during the initial pull.
func (e *Engine) Start(files []string) (synced int, err error) {
	if e.client == nil {
		return 0, fmt.Errorf("no remote client configured")
	}
	resp, err := e.client.SubscribeSync(e.agent, e.client.Actor, e.workspace, files)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stderr, "[kai-sync] subscribed: channel=%s agent=%s workspace=%q\n", resp.ChannelID, e.syncAgent, e.workspace)

	e.channelID = resp.ChannelID
	e.stopSSE = make(chan struct{})
	e.baseMu.Lock()
	e.base = make(map[string][]byte)
	e.baseMu.Unlock()

	synced = e.syncInitialPull()

	// Replay durable sync events missed while offline (best-effort).
	var replaySeq int64
	if prev, ok := e.LoadState(); ok {
		replaySeq = prev.LastSeq
	}
	// In CRDT mode the per-file Doc must reflect the FULL op history (genesis
	// onward) — a partial replay onto a fresh Doc would reconstruct a different
	// Doc on each client and diverge. Two ways to get there:
	//   - restore persisted Doc snapshots and replay only the delta (fast path), or
	//   - cold start: no snapshots → replay everything from seq 0.
	// Snapshots are an optimization; ops are idempotent, so delta-replay onto a
	// restored Doc converges to the same state as a full replay.
	if e.crdtMode {
		if e.loadCRDTDocs() == 0 {
			replaySeq = 0 // cold start — rebuild Docs from genesis
		}
		// else: keep replaySeq = saved LastSeq and replay only the delta.
	}
	if replayResp, rerr := e.client.SyncReplaySince(replaySeq, e.syncAgent, e.workspace, 500); rerr != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] replay skipped (since=%d): %v\n", replaySeq, rerr)
	} else if replayResp != nil {
		applied := 0
		// Poison guard: whole-file history FOLLOWED BY a genesis+ops cutover is
		// fine (migration) — the ops rebuild the Doc and overwrite disk. What's
		// incoherent is a whole-file event arriving AFTER an op event for the same
		// file (e.g. an un-upgraded peer still pushing whole-file into a migrated
		// workspace): the two lineages can't reconcile. Warn only on that order.
		sawOp := map[string]bool{}
		poisoned := map[string]bool{}
		for _, ev := range replayResp.Events {
			if ev.File == "" || ev.Content == "" {
				continue
			}
			raw, decErr := base64.StdEncoding.DecodeString(ev.Content)
			if decErr != nil || len(raw) == 0 {
				continue
			}
			localPath := fromGitRelativePath(e.workDir, ev.File)
			absPath := filepath.Join(e.workDir, localPath)
			if !strings.HasPrefix(absPath, e.workDir) {
				continue
			}
			if ev.Ops {
				sawOp[localPath] = true
				e.applyOps(localPath, absPath, raw, ev.Agent)
			} else {
				if sawOp[localPath] {
					poisoned[localPath] = true // whole-file AFTER op = incoherent
				}
				e.applySyncContent(localPath, absPath, raw, ev.Agent)
			}
			applied++
		}
		if e.crdtMode {
			for path := range poisoned {
				fmt.Fprintf(os.Stderr, "[kai-sync] WARNING: %s received a whole-file edit after op-transport began — likely an un-upgraded peer. Convergence not guaranteed until all peers are on op-mode.\n", path)
			}
		}
		if applied > 0 {
			fmt.Fprintf(os.Stderr, "[kai-sync] replay applied %d event(s), tip=%d\n", applied, replayResp.LatestSeq)
		}
		if replayResp.LatestSeq > replaySeq {
			e.saveSyncSeq(replayResp.LatestSeq)
		}
		// Convert any tracked whole-file files to op-transport (one-time cutover),
		// then persist the rebuilt Docs so the next start is a delta replay.
		if e.crdtMode {
			e.migrateUnconverted(files)
			e.saveAllCRDTDocs()
		}
	}

	go e.readSSEEvents(resp.ChannelID)
	if e.crdtMode {
		go e.periodicDocFlush()
	}
	return synced, nil
}

// periodicDocFlush snapshots the CRDT Docs to disk every 15s so an ungraceful
// kill (no Stop) loses at most ~15s of replay-shortcut — never data, since a
// cold start replays from genesis anyway. Exits with the SSE loop.
func (e *Engine) periodicDocFlush() {
	stop := e.stopSSE // capture; Stop() nils the field after closing
	if stop == nil {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			e.saveAllCRDTDocs()
		}
	}
}

// Stop tears down the SSE loop and releases the subscription. Safe to call
// multiple times. Does not touch the persisted sync-state file — callers that
// want to disable auto-resume should call ClearState separately.
func (e *Engine) Stop() {
	if e.crdtMode {
		e.saveAllCRDTDocs() // persist final state for a fast next start
	}
	if e.stopSSE != nil {
		close(e.stopSSE)
		e.stopSSE = nil
	}
	if e.client != nil && e.channelID != "" {
		_ = e.client.UnsubscribeSync(e.channelID)
	}
	e.channelID = ""
}

// PushChanges pushes the current on-disk content of the given workDir-relative
// paths to the sync channel. Best-effort: per-file errors are logged, not
// returned. Skips files written by sync (feedback-loop prevention) and files
// whose content already matches the last sync base. No-op when not subscribed.
func (e *Engine) PushChanges(paths []string) {
	if e.channelID == "" || e.client == nil {
		return
	}
	for _, path := range paths {
		if e.IsSyncWritten(path) {
			e.log.Write(synclog.SyncLogEntry{
				Event:     synclog.EventSkip,
				File:      path,
				Agent:     e.syncAgent,
				Channel:   e.channelID,
				Timestamp: time.Now().UnixMilli(),
				Detail:    "feedback loop prevention",
			})
			continue
		}
		if e.crdtMode {
			e.pushOpsForChange(path)
			continue
		}
		absPath := filepath.Join(e.workDir, path)
		content, err := os.ReadFile(absPath)
		if err != nil || len(content) > 512*1024 { // skip files > 512KB
			continue
		}
		// Skip if current content matches last-known sync state.
		e.baseMu.RLock()
		base := e.base[path]
		e.baseMu.RUnlock()
		if base != nil && bytes.Equal(base, content) {
			e.log.Write(synclog.SyncLogEntry{
				Event:     synclog.EventSkip,
				File:      path,
				Agent:     e.syncAgent,
				Channel:   e.channelID,
				Timestamp: time.Now().UnixMilli(),
				Detail:    "no change since last sync",
			})
			continue
		}
		syncPath := toGitRelativePath(e.workDir, path)
		encoded := base64.StdEncoding.EncodeToString(content)
		// Send our merge base (common ancestor) so the server can 3-way merge
		// this push into the canonical instead of last-writing over a peer.
		baseEncoded := ""
		if base != nil {
			baseEncoded = base64.StdEncoding.EncodeToString(base)
		}
		if err := e.client.SyncPushFile(e.syncAgent, e.channelID, syncPath, "", encoded, baseEncoded); err != nil {
			if limErr, ok := err.(*remote.CommitLimitError); ok {
				fmt.Fprintf(os.Stderr, "[kai-sync] sync limit reached: %d/%d on %s plan\n", limErr.Used, limErr.Limit, limErr.Tier)
				if limErr.UpgradeURL != "" {
					fmt.Fprintf(os.Stderr, "[kai-sync] upgrade: %s\n", limErr.UpgradeURL)
				}
				e.billingMu.Lock()
				e.billing = fmt.Sprintf("Usage limit reached (%d/%d on %s plan). Live sync paused. Upgrade: %s", limErr.Used, limErr.Limit, limErr.Tier, limErr.UpgradeURL)
				e.billingMu.Unlock()
				break
			}
			fmt.Fprintf(os.Stderr, "[kai-sync] push failed for %s: %v\n", syncPath, err)
		} else {
			fmt.Fprintf(os.Stderr, "[kai-sync] pushed %s (%d bytes)\n", syncPath, len(content))
			e.log.Write(synclog.SyncLogEntry{
				Event:     synclog.EventPush,
				File:      syncPath,
				Agent:     e.syncAgent,
				Channel:   e.channelID,
				Timestamp: time.Now().UnixMilli(),
			})
		}
		// NOTE: deliberately do NOT advance the merge base to our own pushed
		// content. The base must stay at the last COMMON state so a concurrent
		// peer edit 3-way merges (base, mine, theirs) instead of clobbering.
		// Base only advances on receive, to the merged result.
	}
}

// applySyncContent is the single path by which peer-originated file bytes land
// on local disk. Shared by the SSE handler and the replay catch-up loop so the
// guards (feedback-loop suppression, peer-attribution checkpoints, synclog
// audit) can't diverge.
func (e *Engine) applySyncContent(relPath, absPath string, incoming []byte, agent string) {
	local, localErr := os.ReadFile(absPath)

	if localErr == nil && bytes.Equal(local, incoming) {
		// Identical to disk — nothing to write. But this is also how the
		// server echoes our own push's canonical back to us, so seed the
		// merge base from it: keeps a file's author/last-pusher on a current
		// common ancestor instead of a stale one for the next edit.
		e.setBase(relPath, incoming)
		return
	}

	e.baseMu.RLock()
	base := e.base[relPath]
	e.baseMu.RUnlock()

	var toWrite []byte

	if localErr != nil || base == nil {
		toWrite = incoming
	} else if bytes.Equal(local, base) {
		toWrite = incoming
	} else {
		lang := detectSyncLang(relPath)
		if lang != "" {
			mergeResult, mergeErr := merge.Merge3Way(base, local, incoming, lang)
			if mergeErr == nil && mergeResult.Success {
				if merged, ok := mergeResult.Files["file"]; ok {
					toWrite = merged
					fmt.Fprintf(os.Stderr, "[kai-sync] merged %s (auto-resolved)\n", relPath)
					e.log.Write(synclog.SyncLogEntry{
						Event:     synclog.EventMerge,
						File:      relPath,
						Agent:     e.syncAgent,
						PeerAgent: agent,
						Channel:   e.channelID,
						Timestamp: time.Now().UnixMilli(),
						Detail:    "3-way merge auto-resolved",
					})
				}
			}
		} else {
			// No semantic merger for this extension (json, yaml, md, sh, etc.).
			// Try a naive line-based 3-way merge for disjoint edits.
			if merged, ok := naiveLineMerge3(base, local, incoming); ok {
				toWrite = merged
				fmt.Fprintf(os.Stderr, "[kai-sync] line-merged %s (no semantic merger for ext)\n", relPath)
				e.log.Write(synclog.SyncLogEntry{
					Event:     synclog.EventMerge,
					File:      relPath,
					Agent:     e.syncAgent,
					PeerAgent: agent,
					Channel:   e.channelID,
					Timestamp: time.Now().UnixMilli(),
					Detail:    "line-based 3-way merge",
				})
			}
		}
		if toWrite == nil {
			fmt.Fprintf(os.Stderr, "[kai-sync] conflict on %s from %s — local edits preserved\n", relPath, agent)
			e.conflictsMu.Lock()
			e.conflicts = append(e.conflicts, ConflictInfo{
				File:    relPath,
				Agent:   agent,
				Time:    time.Now().Format(time.RFC3339),
				Message: "Both you and " + agent + " edited the same function. Your local edits were preserved.",
			})
			if len(e.conflicts) > 10 {
				e.conflicts = e.conflicts[len(e.conflicts)-10:]
			}
			e.conflictsMu.Unlock()
			e.log.Write(synclog.SyncLogEntry{
				Event:     synclog.EventConflict,
				File:      relPath,
				Agent:     e.syncAgent,
				PeerAgent: agent,
				Channel:   e.channelID,
				Timestamp: time.Now().UnixMilli(),
				Detail:    "local edits preserved",
			})
			e.writePeerCheckpoint(relPath, local, incoming, agent, "conflict")
			// Leave the merge base unchanged on conflict: keep the common
			// ancestor so a later peer edit can still 3-way merge against it.
			return
		}
	}

	os.MkdirAll(filepath.Dir(absPath), 0755)
	if err := os.WriteFile(absPath, toWrite, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] failed to write %s: %v\n", relPath, err)
		return
	}

	// Mark as sync-written so the watcher doesn't push it back (feedback loop).
	e.markSyncWritten(relPath)

	// Emit peer-attribution checkpoints (local→toWrite are the peer's lines).
	e.writePeerCheckpoint(relPath, local, toWrite, agent, "modify")

	// Record the peer contribution so `kai capture` can subtract it. `local`
	// (my pre-peer version, possibly nil for a peer-created file) is the base;
	// toWrite is what's now on disk.
	e.recordPeerContribution(relPath, local, toWrite)

	// Base = the server's canonical we just merged against (incoming), NOT the
	// merged result — so our next push tells the server which canonical our
	// edit was based on, and it folds us in.
	e.setBase(relPath, incoming)

	// If the merge folded in unpushed LOCAL edits (toWrite differs from the
	// server's canonical), push the merged result so the canonical actually
	// incorporates our contribution. The watcher is suppressed (markSyncWritten
	// above), so without this our local edit would be silently dropped from the
	// shared lineage — exactly the "B's funcB never reached A" bug.
	if !bytes.Equal(toWrite, incoming) {
		e.pushMerged(relPath, toWrite)
	}
	e.log.Write(synclog.SyncLogEntry{
		Event:     synclog.EventReceive,
		File:      relPath,
		Agent:     e.syncAgent,
		PeerAgent: agent,
		Channel:   e.channelID,
		Timestamp: time.Now().UnixMilli(),
	})
	fmt.Fprintf(os.Stderr, "[kai-sync] applied %s from %s\n", relPath, agent)
}

// writePeerCheckpoint attributes old→new line ranges to a peer agent so peer
// edits appear in kai blame immediately. No-op without a checkpoint writer.
func (e *Engine) writePeerCheckpoint(relPath string, old, new []byte, agent, action string) {
	if e.cpWriter == nil || agent == "" {
		return
	}
	ranges := authorship.DiffLineRanges(old, new)
	if len(ranges) == 0 {
		return
	}
	ts := time.Now().UnixMilli()
	for _, r := range ranges {
		e.cpWriter.Write(authorship.CheckpointRecord{
			File:       relPath,
			StartLine:  r.Start,
			EndLine:    r.End,
			Action:     action,
			AuthorType: "ai",
			Agent:      agent,
			Timestamp:  ts,
			PeerOrigin: true,
		})
	}
}

func (e *Engine) readSSEEvents(channelID string) {
	if e.client == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "[kai-sync] SSE goroutine started: channel=%s\n", channelID)
	defer fmt.Fprintf(os.Stderr, "[kai-sync] SSE goroutine stopped: channel=%s\n", channelID)

	for {
		select {
		case <-e.stopSSE:
			return
		default:
		}

		url := fmt.Sprintf("%s%s/v1/sync/events?channel=%s",
			e.client.BaseURL, e.client.RepoPath(), e.channelID)
		e.connectSSE(url, e.channelID)

		select {
		case <-e.stopSSE:
			return
		case <-time.After(5 * time.Second):
			fmt.Fprintf(os.Stderr, "[kai-sync] SSE reconnecting...\n")
		}
	}
}

func (e *Engine) connectSSE(url, channelID string) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	if e.client.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.client.AuthToken)
	}

	fmt.Fprintf(os.Stderr, "[kai-sync] connecting SSE to %s\n", url)
	sseClient := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}
	resp, err := sseClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] SSE connect failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		// Channel expired or server restarted — re-subscribe.
		fmt.Fprintf(os.Stderr, "[kai-sync] channel expired, re-subscribing...\n")
		resp.Body.Close()
		newResp, err := e.client.SubscribeSync(e.agent, e.client.Actor, e.workspace, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[kai-sync] re-subscribe failed: %v\n", err)
			return
		}
		e.channelID = newResp.ChannelID
		fmt.Fprintf(os.Stderr, "[kai-sync] re-subscribed on channel %s\n", newResp.ChannelID)
		return
	}
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "[kai-sync] SSE status: %d\n", resp.StatusCode)
		return
	}
	fmt.Fprintf(os.Stderr, "[kai-sync] SSE connected\n")

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0), 10*1024*1024) // 10MB max
	var eventType, eventData string

	for {
		select {
		case <-e.stopSSE:
			return
		default:
		}

		if !scanner.Scan() {
			fmt.Fprintf(os.Stderr, "[kai-sync] SSE connection closed\n")
			return
		}
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			eventData = strings.TrimPrefix(line, "data: ")
		} else if line == "" && eventData != "" {
			if eventType == "file_change" {
				e.handleSyncFileChange(eventData)
			}
			eventType = ""
			eventData = ""
		}
	}
}

// handleSyncFileChange decodes an SSE file_change payload and delegates to
// applySyncContent (the single receive path shared with replay).
func (e *Engine) handleSyncFileChange(data string) {
	var event struct {
		Agent   string `json:"agent"`
		File    string `json:"file"`
		Content string `json:"content"` // base64
		Ops     bool   `json:"ops"`     // Content is a CRDT op-batch
		Time    int64  `json:"time"`    // unix ms
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}
	if event.Content == "" || event.File == "" {
		return
	}
	incoming, err := base64.StdEncoding.DecodeString(event.Content)
	if err != nil {
		return
	}
	localPath := fromGitRelativePath(e.workDir, event.File)
	absPath := filepath.Join(e.workDir, localPath)
	if !strings.HasPrefix(absPath, e.workDir) {
		return
	}
	if event.Ops {
		e.applyOps(localPath, absPath, incoming, event.Agent)
		return
	}
	e.applySyncContent(localPath, absPath, incoming, event.Agent)
}

// syncInitialPull pulls the latest server snapshot and reconciles it against
// local files (3-way merge when both sides diverged from the pre-pull base).
func (e *Engine) syncInitialPull() int {
	db := e.db
	if db == nil {
		return 0
	}

	// Step 1: snapshot LOCAL state before pulling (the 3-way merge base).
	localDigests := make(map[string]string)
	localSnapID, _ := e.latestSnapshotID()
	if localSnapID != nil {
		edges, _ := db.GetEdges(localSnapID, graph.EdgeHasFile)
		for _, edge := range edges {
			node, _ := db.GetNode(edge.Dst)
			if node == nil {
				continue
			}
			path, _ := node.Payload["path"].(string)
			digest, _ := node.Payload["digest"].(string)
			if path != "" && digest != "" {
				localDigests[path] = digest
			}
		}
	}

	// Step 2: pull the latest snapshot from the server.
	if e.client != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] pulling latest snapshot from server...\n")
		cmd := exec.Command("kai", "pull", "--force")
		cmd.Dir = e.workDir
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[kai-sync] pull failed (continuing with local snapshot): %v\n", err)
		}
	}

	// Step 3: get the remote snapshot (now snap.latest after pull).
	remoteSnapID, err := e.latestSnapshotID()
	if err != nil {
		return 0
	}
	edges, err := db.GetEdges(remoteSnapID, graph.EdgeHasFile)
	if err != nil {
		return 0
	}

	synced := 0
	for _, edge := range edges {
		node, err := db.GetNode(edge.Dst)
		if err != nil || node == nil {
			continue
		}
		path, _ := node.Payload["path"].(string)
		remoteDigest, _ := node.Payload["digest"].(string)
		if path == "" || remoteDigest == "" {
			continue
		}

		absPath := filepath.Join(e.workDir, path)
		localContent, readErr := os.ReadFile(absPath)

		if readErr != nil {
			content, err := db.ReadObject(remoteDigest)
			if err != nil || len(content) == 0 {
				continue
			}
			os.MkdirAll(filepath.Dir(absPath), 0755)
			if err := os.WriteFile(absPath, content, 0644); err == nil {
				e.markSyncWritten(path)
				e.setBase(path, content)
				fmt.Fprintf(os.Stderr, "[kai-sync] initial: wrote %s (new file)\n", path)
				synced++
			}
			continue
		}

		localFileDigest := fmt.Sprintf("%x", blake3Sum(localContent))
		if localFileDigest == remoteDigest {
			// Already in sync — seed the merge base with the common content so
			// a later concurrent edit 3-way merges instead of clobbering.
			e.setBase(path, localContent)
			continue
		}

		remoteContent, err := db.ReadObject(remoteDigest)
		if err != nil || len(remoteContent) == 0 {
			continue
		}

		baseDigest := localDigests[path]
		if baseDigest == localFileDigest {
			os.WriteFile(absPath, remoteContent, 0644)
			e.markSyncWritten(path)
			e.setBase(path, remoteContent)
			fmt.Fprintf(os.Stderr, "[kai-sync] initial: updated %s\n", path)
			synced++
			continue
		}

		var baseContent []byte
		if baseDigest != "" {
			baseContent, _ = db.ReadObject(baseDigest)
		}

		if baseContent != nil {
			lang := detectSyncLang(path)
			if lang != "" {
				mergeResult, mergeErr := merge.Merge3Way(baseContent, localContent, remoteContent, lang)
				if mergeErr == nil && mergeResult.Success {
					if merged, ok := mergeResult.Files["file"]; ok {
						os.WriteFile(absPath, merged, 0644)
						e.markSyncWritten(path)
						e.setBase(path, merged)
						fmt.Fprintf(os.Stderr, "[kai-sync] initial: merged %s (auto-resolved)\n", path)
						e.log.Write(synclog.SyncLogEntry{
							Event:     synclog.EventMerge,
							File:      path,
							Agent:     e.syncAgent,
							PeerAgent: "server",
							Channel:   e.channelID,
							Timestamp: time.Now().UnixMilli(),
							Detail:    "initial sync 3-way merge",
						})
						synced++
						continue
					}
				}
			}
			fmt.Fprintf(os.Stderr, "[kai-sync] initial: conflict on %s — local edits preserved\n", path)
			e.conflictsMu.Lock()
			e.conflicts = append(e.conflicts, ConflictInfo{
				File:    path,
				Agent:   "server",
				Time:    time.Now().Format(time.RFC3339),
				Message: "Conflict during initial sync. Your local edits were preserved.",
			})
			e.conflictsMu.Unlock()
		} else {
			fmt.Fprintf(os.Stderr, "[kai-sync] initial: conflict on %s (no base) — local edits preserved\n", path)
			e.conflictsMu.Lock()
			e.conflicts = append(e.conflicts, ConflictInfo{
				File:    path,
				Agent:   "server",
				Time:    time.Now().Format(time.RFC3339),
				Message: "File differs from server but no common base found. Your local edits were preserved.",
			})
			e.conflictsMu.Unlock()
		}
	}

	return synced
}

func (e *Engine) latestSnapshotID() ([]byte, error) {
	if e.resolver == nil {
		return nil, fmt.Errorf("no resolver configured")
	}
	kind := ref.KindSnapshot
	result, err := e.resolver.Resolve("@snap:last", &kind)
	if err != nil {
		return nil, fmt.Errorf("no snapshots found — run 'kai capture' first: %w", err)
	}
	return result.ID, nil
}

// setBase records the merge base (last common state) for a path. A copy is
// stored so later in-place mutations of the caller's slice can't corrupt it.
func (e *Engine) setBase(path string, content []byte) {
	e.baseMu.Lock()
	if e.base == nil {
		e.base = make(map[string][]byte)
	}
	e.base[path] = append([]byte(nil), content...)
	e.baseMu.Unlock()
}

// pushMerged sends a locally-merged result back to the server so the canonical
// incorporates the local edits folded in on receive. base is the canonical we
// merged against, so the server appends our contribution cleanly. Best-effort;
// no-op when not subscribed.
func (e *Engine) pushMerged(path string, content []byte) {
	if e.channelID == "" || e.client == nil {
		return
	}
	e.baseMu.RLock()
	base := e.base[path]
	e.baseMu.RUnlock()
	syncPath := toGitRelativePath(e.workDir, path)
	encoded := base64.StdEncoding.EncodeToString(content)
	baseEncoded := ""
	if base != nil {
		baseEncoded = base64.StdEncoding.EncodeToString(base)
	}
	if err := e.client.SyncPushFile(e.syncAgent, e.channelID, syncPath, "", encoded, baseEncoded); err != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] push-merged failed for %s: %v\n", syncPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[kai-sync] pushed merged %s (%d bytes)\n", syncPath, len(content))
	e.log.Write(synclog.SyncLogEntry{
		Event:     synclog.EventPush,
		File:      syncPath,
		Agent:     e.syncAgent,
		Channel:   e.channelID,
		Timestamp: time.Now().UnixMilli(),
	})
}

// markSyncWritten records that a file was written by sync (so the push side
// skips it — feedback-loop prevention).
func (e *Engine) markSyncWritten(path string) {
	e.writtenMu.Lock()
	if e.written == nil {
		e.written = make(map[string]time.Time)
	}
	e.written[path] = time.Now()
	e.writtenMu.Unlock()
}

// IsSyncWritten reports whether a file was written by sync and not since edited
// by the user. Callers also use this to avoid attributing sync-received files
// to the local agent. 60s TTL; cleared if the file's mtime moves past the write.
func (e *Engine) IsSyncWritten(path string) bool {
	e.writtenMu.Lock()
	defer e.writtenMu.Unlock()
	if e.written == nil {
		return false
	}
	syncTime, ok := e.written[path]
	if !ok {
		return false
	}
	if time.Since(syncTime) > 60*time.Second {
		delete(e.written, path)
		return false
	}
	absPath := filepath.Join(e.workDir, path)
	info, err := os.Stat(absPath)
	if err != nil {
		return false
	}
	if info.ModTime().After(syncTime.Add(time.Second)) {
		delete(e.written, path)
		return false
	}
	return true
}

func blake3Sum(data []byte) []byte {
	h := blake3.Sum256(data)
	return h[:]
}

// --- persisted sync-state.json helpers ---

func (e *Engine) statePath() string {
	return filepath.Join(e.kaiDir, "sync-state.json")
}

// SaveState writes Enabled=true with the given files, preserving LastSeq.
func (e *Engine) SaveState(files []string) {
	var lastSeq int64
	if prev, ok := e.LoadState(); ok {
		lastSeq = prev.LastSeq
	}
	data, err := json.Marshal(persistedSyncState{Enabled: true, Files: files, LastSeq: lastSeq})
	if err != nil {
		return
	}
	_ = os.WriteFile(e.statePath(), data, 0644)
}

func (e *Engine) saveSyncSeq(seq int64) {
	prev, _ := e.LoadState()
	state := persistedSyncState{Enabled: true, LastSeq: seq}
	if prev != nil {
		state.Files = prev.Files
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = os.WriteFile(e.statePath(), data, 0644)
}

// ClearState removes the persisted sync-state file (disables auto-resume).
func (e *Engine) ClearState() {
	_ = os.Remove(e.statePath())
}

// LoadState reads the persisted sync-state; ok=false if missing/disabled.
func (e *Engine) LoadState() (*persistedSyncState, bool) {
	data, err := os.ReadFile(e.statePath())
	if err != nil {
		return nil, false
	}
	var st persistedSyncState
	if json.Unmarshal(data, &st) != nil || !st.Enabled {
		return nil, false
	}
	return &st, true
}

// --- path + merge helpers (sync-only; moved from internal/mcp) ---

// toGitRelativePath converts a workDir-relative path to a git-root-relative
// path so all clones of the same repo use the same file paths in sync.
func toGitRelativePath(workDir, relPath string) string {
	absPath := filepath.Join(workDir, relPath)
	dir := filepath.Dir(absPath)
	for dir != "/" && dir != "." {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			gitRel, err := filepath.Rel(dir, absPath)
			if err == nil {
				return filepath.ToSlash(gitRel)
			}
			break
		}
		dir = filepath.Dir(dir)
	}
	return relPath
}

// fromGitRelativePath converts a git-root-relative path to a workDir-relative path.
func fromGitRelativePath(workDir, gitRelPath string) string {
	dir := workDir
	for dir != "/" && dir != "." {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			absPath := filepath.Join(dir, gitRelPath)
			rel, err := filepath.Rel(workDir, absPath)
			if err == nil {
				return filepath.ToSlash(rel)
			}
			break
		}
		dir = filepath.Dir(dir)
	}
	return gitRelPath
}

// detectSyncLang maps a file path to a language the merge engine supports.
func detectSyncLang(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".js", ".jsx", ".mjs", ".cjs":
		return "js"
	case ".ts", ".tsx":
		return "ts"
	case ".py":
		return "python"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	}
	return ""
}

// naiveLineMerge3 performs a line-based 3-way merge. Returns ok=true with the
// merged bytes when local and incoming edited disjoint hunks relative to base;
// ok=false when they overlap (caller falls through to conflict handling).
func naiveLineMerge3(base, local, incoming []byte) ([]byte, bool) {
	if bytes.Equal(base, local) {
		return incoming, true
	}
	if bytes.Equal(base, incoming) {
		return local, true
	}
	if bytes.Equal(local, incoming) {
		return local, true
	}

	bLines := splitLinesKeepNL(base)
	lLines := splitLinesKeepNL(local)
	iLines := splitLinesKeepNL(incoming)

	lStart, lEnd, lNew := diffRange(bLines, lLines)
	iStart, iEnd, iNew := diffRange(bLines, iLines)

	if lEnd <= iStart {
		out := make([][]byte, 0, len(bLines)+len(lNew)+len(iNew))
		out = append(out, bLines[:lStart]...)
		out = append(out, lNew...)
		out = append(out, bLines[lEnd:iStart]...)
		out = append(out, iNew...)
		out = append(out, bLines[iEnd:]...)
		return bytes.Join(out, nil), true
	}
	if iEnd <= lStart {
		out := make([][]byte, 0, len(bLines)+len(lNew)+len(iNew))
		out = append(out, bLines[:iStart]...)
		out = append(out, iNew...)
		out = append(out, bLines[iEnd:lStart]...)
		out = append(out, lNew...)
		out = append(out, bLines[lEnd:]...)
		return bytes.Join(out, nil), true
	}
	return nil, false
}

// splitLinesKeepNL splits bytes into lines, keeping the trailing newline.
func splitLinesKeepNL(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, b[start:i+1])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// diffRange returns the half-open range [start, end) of base lines changed to
// produce other, plus the replacement lines. Trims common prefix and suffix.
func diffRange(base, other [][]byte) (int, int, [][]byte) {
	n, m := len(base), len(other)
	prefix := 0
	for prefix < n && prefix < m && bytes.Equal(base[prefix], other[prefix]) {
		prefix++
	}
	suffix := 0
	for suffix < n-prefix && suffix < m-prefix && bytes.Equal(base[n-1-suffix], other[m-1-suffix]) {
		suffix++
	}
	return prefix, n - suffix, other[prefix : m-suffix]
}
