package tui

// multiroot_tui_test.go drives the TUI through the existing
// tuiHarness (harness_test.go) to verify that multi-root-aware
// content surfaces correctly through the chat-activity rendering
// pipeline. The orchestrator + tools work shipped in v0.31.3–v0.31.9
// produces multi-root-prefixed paths and project-tagged gate reasons;
// these tests prove the TUI renders them without mangling.
//
// What's actually being tested:
//   - ChatActivityEvent values with multi-root annotations (project
//     prefixes in paths, "[project]" tags in gate reasons) flow
//     through PumpChatActivity → REPL.Update → formatToolEvent /
//     formatDiffEvent / formatGateVerdict and land in the rendered
//     scrollback intact.
//   - The TUI handles a realistic per-event sequence (tool → diff →
//     gate) for a cross-project agent run without dropping or
//     re-ordering content.
//
// What is NOT tested here:
//   - The orchestrator/tools logic that PRODUCES these events. Those
//     have direct unit tests in internal/orchestrator and
//     internal/agent/tools. The TUI tests are the rendering surface,
//     not the producer.
//   - A full agent run end-to-end (would require a real LLM provider
//     and DB). The harness drives the rendering layer with
//     synthesized events instead — sufficient for the "did the TUI
//     mangle the format?" question and fast enough to run in CI.

import (
	"testing"
	"time"

	"kai/internal/tui/views"
)

// TestTUI_ToolEventWithProjectPrefixRenders confirms a tool-dispatch
// event whose Summary contains a multi-root project prefix
// (e.g. "kai-server/...") surfaces with that prefix intact in the
// rendered scrollback. Regression guard for any future REPL renderer
// that might over-aggressively strip path prefixes thinking they're
// noise.
func TestTUI_ToolEventWithProjectPrefixRenders(t *testing.T) {
	chatCh := make(chan views.ChatActivityEvent, 8)
	// Verbose: this test specifically pins that the tool-event
	// scrollback line carries the multi-root project prefix. The
	// quiet default (post-v0.31.24) suppresses that line, so we
	// opt into verbose mode to keep this assertion meaningful.
	h := newTUI(t, WithChannels(nil, chatCh), WithVerboseTools())
	h.WaitForText("Sync: idle")

	chatCh <- views.ChatActivityEvent{
		Kind:    "tool",
		Summary: "view kai-server/kailab-control/internal/api/search.go",
		When:    time.Now(),
	}
	h.WaitForText("kai-server/kailab-control/internal/api/search.go")
}

// TestTUI_DiffEventWithProjectPrefixRendersPath confirms the diff
// renderer preserves multi-root project prefixes on Path. v0.31.3
// onward, the orchestrator's absorb step emits ChangedPaths prefixed
// with the project basename (e.g. "kai-server/foo.go"). Renderers
// downstream pull from these.
func TestTUI_DiffEventWithProjectPrefixRendersPath(t *testing.T) {
	chatCh := make(chan views.ChatActivityEvent, 8)
	h := newTUI(t, WithChannels(nil, chatCh))
	h.WaitForText("Sync: idle")

	chatCh <- views.ChatActivityEvent{
		Kind: "diff",
		Path: "kai-server/kailab-control/internal/api/search.go",
		Op:   "modified",
		Diff: "@@ -1,3 +1,5 @@\n+const httpClientTimeout = 10 * time.Second\n",
		Added:   2,
		Removed: 0,
		When:    time.Now(),
	}
	h.WaitForText("kai-server/kailab-control/internal/api/search.go")
}

// TestTUI_GateVerdictRendersProjectTaggedReasons mirrors the
// aggregate-verdict shape from v0.31.4: each per-project reason is
// prefixed with "[<project-name>]" in the aggregated GateReasons
// slice. The renderer surfaces GateReasons[0] as the suffix on the
// verdict line; this test confirms the prefix survives the render.
func TestTUI_GateVerdictRendersProjectTaggedReasons(t *testing.T) {
	chatCh := make(chan views.ChatActivityEvent, 8)
	// Wider terminal: the gate-verdict line packs reason + every path
	// into one line; at 80 cols the paths get truncated off-screen.
	h := newTUI(t, WithChannels(nil, chatCh), WithSize(200, 24))
	h.WaitForText("Sync: idle")

	chatCh <- views.ChatActivityEvent{
		Kind:        "gate",
		GateVerdict: "review",
		GateRadius:  12,
		GateReasons: []string{
			"[kai-server] protected path: kailab-control/internal/db/migrations/",
			"[kai] high blast radius (12)",
		},
		GatePaths: []string{
			"kai-server/kailab-control/internal/api/search.go",
			"kai/kai-cli/cmd/kai/main.go",
		},
		When: time.Now(),
	}
	// Verdict line should carry the first project-tagged reason.
	h.WaitForText("[kai-server] protected path")
	// And the path list should include the multi-root-prefixed entries.
	h.WaitForText("kai-server/kailab-control/internal/api/search.go")
	h.WaitForText("kai/kai-cli/cmd/kai/main.go")
}

// TestTUI_CrossProjectAgentRunSequence drives a realistic event
// sequence for a cross-project agent run — tool dispatch into one
// project, diff committed in another, aggregate gate verdict
// covering both — and confirms every piece of multi-root-prefixed
// content lands in the scrollback. This is the end-to-end UX
// confidence test: if any of the v0.31.3–v0.31.9 renderers
// regressed, this would catch it.
func TestTUI_CrossProjectAgentRunSequence(t *testing.T) {
	chatCh := make(chan views.ChatActivityEvent, 16)
	// Verbose: this sequence includes tool events whose multi-root
	// path prefix must land in scrollback. Quiet default would
	// suppress them.
	h := newTUI(t, WithChannels(nil, chatCh), WithVerboseTools())
	h.WaitForText("Sync: idle")

	now := time.Now()

	// 1. Agent inspects kai-server source via view tool.
	chatCh <- views.ChatActivityEvent{
		Kind:    "tool",
		Summary: "view kai-server/kailab-control/internal/api/routes.go",
		When:    now,
	}
	// 2. Agent inspects kai source.
	chatCh <- views.ChatActivityEvent{
		Kind:    "tool",
		Summary: "view kai/kai-cli/internal/agent/tools/kai.go",
		When:    now.Add(time.Second),
	}
	// 3. Agent writes a new file in kai-server.
	chatCh <- views.ChatActivityEvent{
		Kind:    "diff",
		Path:    "kai-server/kailab-control/internal/api/search.go",
		Op:      "created",
		Diff:    "@@ -0,0 +1,3 @@\n+package api\n+// Search proxy handler.\n+func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {}\n",
		Added:   3,
		Removed: 0,
		When:    now.Add(2 * time.Second),
	}
	// 4. Agent edits a file in kai.
	chatCh <- views.ChatActivityEvent{
		Kind:    "diff",
		Path:    "kai/kai-cli/internal/agent/tools/kai_web_search.go",
		Op:      "created",
		Diff:    "@@ -0,0 +1,2 @@\n+package tools\n+// Web search agent tool.\n",
		Added:   2,
		Removed: 0,
		When:    now.Add(3 * time.Second),
	}
	// 5. Aggregate gate verdict (review) covering both projects.
	chatCh <- views.ChatActivityEvent{
		Kind:        "gate",
		GateVerdict: "review",
		GateRadius:  8,
		GateReasons: []string{
			"[kai-server] blast radius 5",
			"[kai] new tool needs review",
		},
		GatePaths: []string{
			"kai-server/kailab-control/internal/api/search.go",
			"kai/kai-cli/internal/agent/tools/kai_web_search.go",
		},
		When: now.Add(4 * time.Second),
	}

	// Every multi-root prefix and tag should be in the rendered
	// scrollback. Check distinctive substrings rather than the
	// formatted lines verbatim — the formatter's exact glyph/spacing
	// may evolve.
	for _, want := range []string{
		"kai-server/kailab-control/internal/api/routes.go",
		"kai/kai-cli/internal/agent/tools/kai.go",
		"kai-server/kailab-control/internal/api/search.go",
		"kai/kai-cli/internal/agent/tools/kai_web_search.go",
		"[kai-server] blast radius 5",
	} {
		h.WaitForText(want)
	}
}

// TestTUI_GateAutoVerdictRendersAcrossProjects exercises the happy
// path of v0.31.4's aggregate-verdict pipeline: when every project
// classifies Auto, the aggregate is Auto, and the rendered line
// reads as a single green "auto" with the multi-root paths still
// visible.
func TestTUI_GateAutoVerdictRendersAcrossProjects(t *testing.T) {
	chatCh := make(chan views.ChatActivityEvent, 8)
	// Wider terminal so the gate-verdict line (which packs the verdict,
	// downstream count, and every touched path into one line) doesn't
	// truncate the second project's path at the 80-col boundary.
	h := newTUI(t, WithChannels(nil, chatCh), WithSize(160, 24))
	h.WaitForText("Sync: idle")

	chatCh <- views.ChatActivityEvent{
		Kind:        "gate",
		GateVerdict: "auto",
		GateRadius:  0,
		GateReasons: nil, // clean aggregate has no reasons
		GatePaths: []string{
			"kai-server/kailab-control/internal/cfg/config.go",
			"kai/kai-cli/internal/agent/tools/kai_search.go",
		},
		When: time.Now(),
	}
	// "auto" label visible; both per-project paths visible.
	h.WaitForText("auto")
	h.WaitForText("kai-server/kailab-control/internal/cfg/config.go")
	h.WaitForText("kai/kai-cli/internal/agent/tools/kai_search.go")
}

// TestTUI_BashConfirmAcrossProjectsRendersCorrectPath confirms a
// bash-confirm prompt that wants to run a command against a sibling
// project's directory surfaces the path with its project prefix. The
// confirm prompt is the only place the user reads a destructive
// command before approving (per the bash-tool fail-closed gate
// shipped in v0.31.0); getting the path right here is load-bearing.
func TestTUI_BashConfirmAcrossProjectsRendersCorrectPath(t *testing.T) {
	chatCh := make(chan views.ChatActivityEvent, 8)
	h := newTUI(t, WithChannels(nil, chatCh))
	h.WaitForText("Sync: idle")

	reply := make(chan bool, 1)
	chatCh <- views.ChatActivityEvent{
		Kind:      "bash_confirm",
		Summary:   "rm -rf /Users/somebody/projects/kai/kai-server/kailab-control/old-thing",
		SpawnName: "cleanup-agent",
		Warning:   "may recursively force-remove files",
		Reply:     reply,
		When:      time.Now(),
	}
	// The destructive warning, the full path including kai-server, and
	// the spawn name should all appear.
	h.WaitForText("may recursively force-remove files")
	h.WaitForText("kai-server/kailab-control/old-thing")
	h.WaitForText("cleanup-agent")
	// Defensive: close reply so the simulated agent goroutine (none
	// in this test) doesn't leak. Real callers do this on shutdown.
	close(reply)
}
