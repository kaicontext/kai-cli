package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// kaiLiveSyncTool wraps the live-sync SSE infrastructure as an
// agent-callable tool. Coding and Debug modes (under orchestration)
// list this so a multi-agent run can push intermediate state to peers.
//
// Action breakdown:
//   - push: read a file from the workspace, base64-encode it, hash
//     it, and POST to the sync channel.
//   - status: report channel config and connection state. The
//     orchestrator subscribes to the sync stream out-of-band; the
//     agent doesn't drive it. status surfaces what's configured so
//     the model can reason about whether peers will see its push.
//   - pull: not actually performed by this tool. Incoming peer
//     changes arrive via the orchestrator's SSE subscription and
//     are written to the workspace by infrastructure, NOT by the
//     agent. We return a clear note explaining this rather than
//     silently no-op'ing — the model needs to know its file view
//     is being kept fresh by something outside the tool call.
type kaiLiveSyncTool struct {
	client    LiveSyncClient
	workspace string
	// channelID is the orchestrator-assigned sync channel for this
	// agent. Empty means sync isn't configured (single-agent mode).
	channelID string
	// agent is this agent's identifier on the channel ("agent-1",
	// "backend-api", etc.). Used as the SyncPushFile `agent`
	// parameter so peers can attribute events.
	agent string
}

// LiveSyncClient is the subset of *remote.Client this tool needs.
// Defined as an interface so unit tests can stub the push without
// hitting the network. Exported so agent.Options can carry one
// without taking a hard dependency on internal/remote.
type LiveSyncClient interface {
	SyncPushFile(agent, channelID, filePath, digest, contentBase64 string) error
}

type kaiLiveSyncParams struct {
	Action string `json:"action"`
	File   string `json:"file"`
}

type kaiLiveSyncPushResult struct {
	Pushed         string `json:"pushed"`
	PeersNotified  int    `json:"peers_notified"`
	Note           string `json:"note,omitempty"`
}

type kaiLiveSyncStatusResult struct {
	Channel   string   `json:"channel"`
	Connected bool     `json:"connected"`
	Peers     []string `json:"peers,omitempty"`
	Note      string   `json:"note,omitempty"`
}

func (t *kaiLiveSyncTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_live_sync",
		Description: "Coordinate file changes across orchestrated multi-agent runs. " +
			"Actions: \"push\" sends a file to peers; \"status\" reports channel state; " +
			"\"pull\" is informational (incoming peer changes arrive automatically via the " +
			"orchestrator's sync subscription). Only useful in orchestrated runs — chat-mode " +
			"agents have no peers and should not call this.",
		Parameters: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "One of: push, pull, status.",
			},
			"file": map[string]any{
				"type":        "string",
				"description": "File path relative to workspace root (required for push).",
			},
		},
		Required: []string{"action"},
	}
}

func (t *kaiLiveSyncTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiLiveSyncParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_live_sync: invalid input json: " + err.Error()), nil
	}
	action := strings.ToLower(strings.TrimSpace(p.Action))
	switch action {
	case "push":
		return t.runPush(p.File)
	case "pull":
		return t.runPull()
	case "status":
		return t.runStatus()
	default:
		return NewTextErrorResponse(
			"kai_live_sync: action must be one of push, pull, status; got " + p.Action,
		), nil
	}
}

func (t *kaiLiveSyncTool) runPush(file string) (ToolResponse, error) {
	if t.client == nil || t.channelID == "" {
		// Sync isn't configured — chat mode or a non-orchestrated
		// run. Treat as a clear error so the model stops trying.
		return NewTextErrorResponse(
			"kai_live_sync: not connected to a sync channel (single-agent run?). " +
				"Skip this tool unless you're in an orchestrated session.",
		), nil
	}
	if strings.TrimSpace(file) == "" {
		return NewTextErrorResponse("kai_live_sync: file required for action=push"), nil
	}
	if t.workspace == "" {
		return NewTextErrorResponse("kai_live_sync: workspace not configured"), nil
	}
	rel := filepath.Clean(file)
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return NewTextErrorResponse(
			"kai_live_sync: file must be a workspace-relative path inside the repo",
		), nil
	}
	abs := filepath.Join(t.workspace, rel)
	data, err := os.ReadFile(abs)
	if err != nil {
		return NewTextErrorResponse("kai_live_sync: read " + rel + ": " + err.Error()), nil
	}
	digest := sha256Digest(data)
	encoded := base64.StdEncoding.EncodeToString(data)
	if err := t.client.SyncPushFile(t.agent, t.channelID, rel, digest, encoded); err != nil {
		return NewTextErrorResponse("kai_live_sync: push: " + err.Error()), nil
	}
	out := kaiLiveSyncPushResult{
		Pushed: rel,
		// Server-side fanout count isn't returned by SyncPushFile.
		// Surface -1 / 0 instead of fabricating a number — model
		// can read the note for the actual semantics.
		PeersNotified: 0,
		Note: "Push accepted by sync server; per-peer delivery is handled server-side. " +
			"Run kai_live_sync action=status for the current peer list.",
	}
	body, _ := json.MarshalIndent(out, "", "  ")
	return NewTextResponse(string(body)), nil
}

func (t *kaiLiveSyncTool) runPull() (ToolResponse, error) {
	// The orchestrator owns the SSE subscription that delivers peer
	// edits. The agent's view of those edits lands as workspace
	// files between turns. We don't poll here — the model just
	// needs to know "trust the workspace; peer changes are already
	// applied if they arrived."
	out := map[string]any{
		"files_received":  []any{},
		"merge_conflicts": 0,
		"note": "Incoming peer changes are applied to the workspace by the orchestrator's " +
			"sync subscription, not by this tool call. Re-read any file you care about with " +
			"`view` to see the latest state.",
	}
	body, _ := json.MarshalIndent(out, "", "  ")
	return NewTextResponse(string(body)), nil
}

func (t *kaiLiveSyncTool) runStatus() (ToolResponse, error) {
	connected := t.client != nil && t.channelID != ""
	out := kaiLiveSyncStatusResult{
		Channel:   t.channelID,
		Connected: connected,
	}
	if !connected {
		out.Note = "No sync channel configured (single-agent run)."
	} else {
		// Peer list comes from the SSE stream, which this tool
		// doesn't have a handle to. Leaving Peers empty is honest;
		// the orchestrator's status pane is the source of truth.
		out.Note = "Channel connected. Peer list available in the orchestrator's UI."
	}
	body, _ := json.MarshalIndent(out, "", "  ")
	return NewTextResponse(string(body)), nil
}

// sha256Digest returns the lowercase hex-encoded SHA-256 of the
// supplied content. Matches what the sync server expects in
// SyncPushFile's digest field — used for content-addressed
// deduplication on the receive side.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

