package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"kai/internal/graph"
	"kai/internal/projects"
)

// kaiImpactTool exposes "what's the blast radius of changing this file"
// as an agent-callable tool. Same primitive `safetygate.Classify` uses
// internally; here it's surfaced so the model can ask before editing,
// and so gate-every-write can share one query path with agent-side
// reasoning.
//
// Output format is structured JSON (callers, dependents, counts, risk
// classifier) rather than the prose summary kai_dependents emits.
// Models reason better over JSON for "I need to count things" tasks
// like deciding whether a change is safe.
type kaiImpactTool struct {
	db kaiImpactGrapher
	// protected mirrors safetygate.Config.Protected so the tool can
	// flag protected status without depending on safetygate. The
	// runner threads it through KaiTools.Protected.
	protected []string
	// set is the multi-root workspace. When non-nil and a queried
	// target carries a project-name prefix, the tool routes its
	// graph queries to that project's DB instead of the primary's.
	// See routeGraphForPath in kai.go.
	set *projects.Set
}

// kaiImpactGrapher widens KaiGrapher's surface only enough to walk
// callers/dependents at depth ≥ 1. The shared interface in kai.go
// already has everything we need; aliased here for clarity at the
// call site.
type kaiImpactGrapher = KaiGrapher

type kaiImpactParams struct {
	Target   string `json:"target"`
	Function string `json:"function"`
	Depth    int    `json:"depth"`
}

// kaiImpactCaller is one inbound caller in the JSON output.
type kaiImpactCaller struct {
	Function  string `json:"function,omitempty"`
	File      string `json:"file"`
	Protected bool   `json:"protected"`
}

type kaiImpactResult struct {
	Target         string            `json:"target"`
	Callers        []kaiImpactCaller `json:"callers"`
	Dependents     []kaiImpactCaller `json:"dependents"`
	CallerCount    int               `json:"caller_count"`
	DependentCount int               `json:"dependent_count"`
	HasProtected   bool              `json:"has_protected"`
	Risk           string            `json:"risk"`
}

func (t *kaiImpactTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_impact",
		Description: "Compute the blast radius of changing a file or function. Returns " +
			"direct callers and dependents (depth 1 by default), counts, and a risk " +
			"classification (none/low/medium/high). Call this BEFORE editing to " +
			"understand who might break, especially in Planning and Debug modes.",
		Parameters: map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "File path relative to repo root (e.g. \"auth.py\", \"internal/api/routes.go\").",
			},
			"function": map[string]any{
				"type":        "string",
				"description": "Optional specific function name. If omitted, returns impact for the whole file.",
			},
			"depth": map[string]any{
				"type":        "integer",
				"description": "Traversal depth. Defaults to 1; Debug mode may pass 2 for callers-of-callers.",
			},
		},
		Required: []string{"target"},
	}
}

func (t *kaiImpactTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiImpactParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_impact: invalid input json: " + err.Error()), nil
	}
	target := strings.TrimSpace(p.Target)
	if target == "" {
		return NewTextErrorResponse("kai_impact: target required"), nil
	}
	// Per-project routing: target is a file path, so a "kai-server/..."
	// prefix routes the graph walk into that project's DB and uses the
	// project-relative path against it. Re-prefix the target string we
	// pass to walkInbound so its result-shaping reflects the routed
	// project; the display path remains the prefixed form.
	db, routedTarget, projName := routeGraphForPath(t.set, t.db, target)
	if projName != "" {
		TraceRouting("kai_impact target=%q function=%q depth=%d → db=%s rel=%q", target, p.Function, p.Depth, projName, routedTarget)
	} else {
		TraceRouting("kai_impact target=%q function=%q depth=%d → db=primary", target, p.Function, p.Depth)
	}
	depth := p.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > 3 {
		// Cap defensively. depth=3 already pulls in too much for a
		// single tool result; the model can call repeatedly with
		// new targets if it needs more.
		depth = 3
	}

	callers, err := walkInbound(db, routedTarget, graph.EdgeCalls, depth, t.protected, p.Function)
	if err != nil {
		return NewTextErrorResponse("kai_impact: " + err.Error()), nil
	}
	dependents, err := walkInbound(db, routedTarget, graph.EdgeImports, depth, t.protected, "")
	if err != nil {
		return NewTextErrorResponse("kai_impact: " + err.Error()), nil
	}

	hasProtected := isPathProtected(target, t.protected)
	for _, c := range callers {
		if c.Protected {
			hasProtected = true
			break
		}
	}
	if !hasProtected {
		for _, d := range dependents {
			if d.Protected {
				hasProtected = true
				break
			}
		}
	}

	out := kaiImpactResult{
		Target:         displayTarget(target, p.Function),
		Callers:        callers,
		Dependents:     dependents,
		CallerCount:    len(callers),
		DependentCount: len(dependents),
		HasProtected:   hasProtected,
		Risk:           classifyImpactRisk(len(callers), len(dependents), hasProtected),
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return NewTextErrorResponse("kai_impact: marshal: " + err.Error()), nil
	}
	return NewTextResponse(string(body)), nil
}

// walkInbound BFS walks inbound edges of the given type up to `depth`
// hops from `target`. Returns one entry per unique source path with
// protected status filled in; deduped by file path. functionFilter
// (when set) keeps only entries whose CALLS-edge calleeName matches —
// used so kai_impact's `function` parameter actually narrows.
func walkInbound(
	db kaiImpactGrapher,
	target string,
	edgeType graph.EdgeType,
	depth int,
	protected []string,
	functionFilter string,
) ([]kaiImpactCaller, error) {
	visited := map[string]bool{target: true}
	frontier := []string{target}
	var results []kaiImpactCaller
	wantFn := normalizeSymbolName(functionFilter)

	for hop := 1; hop <= depth && len(frontier) > 0; hop++ {
		var next []string
		for _, current := range frontier {
			edges, err := db.GetEdgesToByPath(current, edgeType)
			if err != nil {
				return nil, fmt.Errorf("walking %s into %s: %w", edgeType, current, err)
			}
			for _, e := range edges {
				node, err := db.GetNode(e.Src)
				if err != nil || node == nil {
					continue
				}
				srcPath, _ := node.Payload["path"].(string)
				if srcPath == "" || visited[srcPath] {
					continue
				}
				// Function filter: only meaningful for CALLS edges,
				// and only when the caller-side context node carries
				// a calleeName matching the filter.
				if wantFn != "" && edgeType == graph.EdgeCalls {
					if !edgeMatchesCallee(db, e, wantFn) {
						continue
					}
				}
				visited[srcPath] = true
				results = append(results, kaiImpactCaller{
					File:      srcPath,
					Function:  callerFnFromEdge(db, e),
					Protected: isPathProtected(srcPath, protected),
				})
				next = append(next, srcPath)
			}
		}
		frontier = next
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].File != results[j].File {
			return results[i].File < results[j].File
		}
		return results[i].Function < results[j].Function
	})
	return results, nil
}

// edgeMatchesCallee reads the call-site context node off the edge
// (e.At) and compares its calleeName to the requested function. We
// match on normalized names — the parser sometimes stores
// "Type.Method" forms in the payload.
func edgeMatchesCallee(db kaiImpactGrapher, e *graph.Edge, wantFn string) bool {
	if len(e.At) == 0 {
		return false
	}
	n, err := db.GetNode(e.At)
	if err != nil || n == nil {
		return false
	}
	callee, _ := n.Payload["calleeName"].(string)
	return normalizeSymbolName(callee) == wantFn
}

// callerFnFromEdge tries to read a caller-side function name off the
// edge's context node. Returns "" when the parser didn't record one
// — many graphs only carry callee info, which is fine for the
// blast-radius use case.
func callerFnFromEdge(db kaiImpactGrapher, e *graph.Edge) string {
	if len(e.At) == 0 {
		return ""
	}
	n, err := db.GetNode(e.At)
	if err != nil || n == nil {
		return ""
	}
	if name, ok := n.Payload["callerName"].(string); ok && name != "" {
		return name
	}
	return ""
}

// isPathProtected matches a path against the gate's protected globs.
// Re-implemented (vs. importing safetygate) so this package stays
// import-cycle-free; the patterns are a small surface and stay in
// sync via the runner that threads the same Protected list into both.
func isPathProtected(p string, patterns []string) bool {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
		if matchDoubleStarLocal(pat, p) {
			return true
		}
	}
	return false
}

// matchDoubleStarLocal handles "a/b/**" and "**/x.go" forms that
// path.Match doesn't understand. Mirrors safetygate.matchDoubleStar.
func matchDoubleStarLocal(pattern, p string) bool {
	const ds = "/**"
	if n := len(pattern); n >= len(ds) && pattern[n-len(ds):] == ds {
		prefix := pattern[:n-len(ds)] + "/"
		return len(p) >= len(prefix) && p[:len(prefix)] == prefix
	}
	const sd = "**/"
	if len(pattern) >= len(sd) && pattern[:len(sd)] == sd {
		suffix := pattern[len(sd):]
		return len(p) >= len(suffix) && p[len(p)-len(suffix):] == suffix
	}
	return false
}

// classifyImpactRisk maps caller+dependent counts and protected
// status to a coarse risk label. Thresholds match the spec: any
// protected upgrades to medium; >10 total is high; <=3 is low; 0 is
// none. The model uses these labels to phrase warnings to the
// developer without having to reason about counts every time.
func classifyImpactRisk(callers, dependents int, hasProtected bool) string {
	total := callers + dependents
	if total == 0 && !hasProtected {
		return "none"
	}
	if total > 10 {
		return "high"
	}
	if total > 3 || hasProtected {
		return "medium"
	}
	return "low"
}

// displayTarget is the "target" field rendered back to the model.
// File-only when no function was passed; "file::function" form
// otherwise so the model echoes the right scope in its response.
func displayTarget(file, function string) string {
	if function == "" {
		return file
	}
	return file + "::" + function
}
