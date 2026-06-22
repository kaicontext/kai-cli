package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// tool_summary.go: render a per-tool-call one-liner the TUI can show
// next to the verb. The previous behavior surfaced bare verbs ("→
// add-config-show-command: view") which gave the user no signal about
// what file was being viewed or what pattern was being grepped. The
// failing dogfood transcript was visually homogeneous for 37 turns
// even though each call had different args — the args were the only
// way to tell wasted reads from purposeful ones.
//
// Keep summaries short (one line, <80 chars). Truncate long values
// with `…`. The full input is still in the runlog and persisted to
// `.kai/runs/<id>/N.request.json` — this is just the activity feed.

const summaryArgCap = 60

// summarizeToolCall returns "[<name>] <key-arg>" or just "[<name>]"
// when the input has no obvious key field. Unknown tools fall through
// to the bracketed bare name so a new tool gets correct (if
// uninformative) output before this list is updated. Bracketing the
// name visually separates the verb from its args in the activity feed.
func summarizeToolCall(name, inputJSON string) string {
	bare := "[" + name + "]"
	if inputJSON == "" {
		return bare
	}
	// Decode lazily into a flat map so we don't pay the cost of
	// per-tool struct decoding on the activity hot path.
	var raw map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &raw); err != nil {
		return bare
	}
	switch name {
	case "view":
		path := stringField(raw, "file_path")
		offset := intField(raw, "offset")
		limit := intField(raw, "limit")
		if path == "" {
			return bare
		}
		// Always show the view window so the activity feed
		// distinguishes "whole file" from "narrow slice" at a glance.
		// Earlier rev hid the range when offset==0 && limit==default,
		// which made every full-file view render as "[view] path" —
		// indistinguishable from a tiny 50-line slice on the next
		// line. 2026-05-14 dogfood: users couldn't tell whether the
		// agent was reading 50 lines or 2000 from each '[view]' line.
		return fmt.Sprintf("[%s] %s @%d:%d", name, truncate(path), offset, lineWindowEnd(offset, limit))
	case "write", "edit":
		path := stringField(raw, "file_path")
		if path == "" {
			return bare
		}
		return fmt.Sprintf("[%s] %s", name, truncate(path))
	case "bash":
		cmd := strings.TrimSpace(stringField(raw, "command"))
		if cmd == "" {
			return bare
		}
		// Take only the first command line for the activity feed;
		// the run-log captures the full input. Compound commands
		// (cmd1 && cmd2 && …) show only the first segment.
		if idx := strings.IndexAny(cmd, "\n"); idx > 0 {
			cmd = cmd[:idx]
		}
		return fmt.Sprintf("[%s] %s", name, truncate(cmd))
	case "kai_grep", "kai_search":
		pat := stringField(raw, "pattern")
		if pat == "" {
			pat = stringField(raw, "query")
		}
		if pat == "" {
			return bare
		}
		return fmt.Sprintf("[%s] %q", name, truncate(pat))
	case "kai_callers", "kai_dependents", "kai_symbols", "kai_impact":
		sym := stringField(raw, "symbol")
		if sym == "" {
			sym = stringField(raw, "file_path")
		}
		if sym == "" {
			return bare
		}
		return fmt.Sprintf("[%s] %s", name, truncate(sym))
	case "kai_context":
		f := stringField(raw, "file_path")
		if f == "" {
			return bare
		}
		return fmt.Sprintf("[%s] %s", name, truncate(f))
	case "kai_files", "kai_tree":
		d := stringField(raw, "dir")
		if d == "" {
			d = stringField(raw, "path")
		}
		if d == "" {
			return bare
		}
		return fmt.Sprintf("[%s] %s", name, truncate(d))
	case "kai_checkpoint":
		f := stringField(raw, "file")
		s, e := intField(raw, "start_line"), intField(raw, "end_line")
		if f == "" {
			return bare
		}
		return fmt.Sprintf("[%s] %s:%d-%d", name, truncate(f), s, e)
	}
	return bare
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func intField(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	// JSON numbers decode to float64 by default.
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func lineWindowEnd(offset, limit int) int {
	if limit <= 0 {
		limit = 2000
	}
	return offset + limit
}

func truncate(s string) string {
	if len(s) <= summaryArgCap {
		return s
	}
	return s[:summaryArgCap-1] + "…"
}
