package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/projects"
)

// kai_graph_inverse.go: the inbound/outbound complements of the
// existing graph tools.
//
//   kai_callers     → kai_callees       (who calls X / what X calls)
//   kai_dependents  → kai_dependencies  (who imports X / what X imports)
//   (new)           → kai_tests         (tests covering a file)
//
// All three mirror the kai_callers / kai_dependents implementations:
// per-project routing via routeGraphForPath, then a graph walk. The
// MCP server already exposed these to external clients; this brings
// them to the in-process coding agent, which had only the inbound half.

// --- kai_callees -----------------------------------------------------

type kaiCalleesTool struct {
	db  KaiGrapher
	set *projects.Set
}

type kaiCalleesParams struct {
	File string `json:"file"`
}

func (t *kaiCalleesTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_callees",
		Description: "List the functions/methods that the given file calls OUT to (the outbound " +
			"complement of kai_callers). Use it to understand what a file depends on at the " +
			"call level before changing it, or to trace a code path forward.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Path of the file whose outgoing calls you want, relative to the repo root.",
			},
		},
		Required: []string{"file"},
	}
}

func (t *kaiCalleesTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiCalleesParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_callees: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.File) == "" {
		return NewTextErrorResponse("kai_callees: file required"), nil
	}
	db, file, projName := routeGraphForPath(t.set, t.db, p.File)
	if projName != "" {
		TraceRouting("kai_callees file=%q → db=%s rel=%q", p.File, projName, file)
	} else {
		TraceRouting("kai_callees file=%q → db=primary", p.File)
	}

	edges, err := db.GetEdgesOfType(graph.EdgeCalls)
	if err != nil {
		return NewTextErrorResponse("kai_callees: " + err.Error()), nil
	}
	type hit struct {
		callee string
		line   int
	}
	var hits []hit
	seen := map[string]bool{}
	for _, e := range edges {
		if len(e.At) == 0 {
			continue
		}
		n, err := db.GetNode(e.At)
		if err != nil || n == nil {
			continue
		}
		caller, _ := n.Payload["callerFile"].(string)
		if caller != file {
			continue
		}
		callee, _ := n.Payload["calleeName"].(string)
		if callee == "" {
			continue
		}
		line := 0
		if l, ok := n.Payload["line"].(float64); ok {
			line = int(l)
		}
		key := fmt.Sprintf("%s:%d", callee, line)
		if seen[key] {
			continue
		}
		seen[key] = true
		hits = append(hits, hit{callee: callee, line: line})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].line != hits[j].line {
			return hits[i].line < hits[j].line
		}
		return hits[i].callee < hits[j].callee
	})
	if len(hits) == 0 {
		return NewTextResponse(fmt.Sprintf("kai_callees: %q makes no recorded calls", p.File)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "calls made by %s (%d):\n", p.File, len(hits))
	for _, h := range hits {
		if h.line > 0 {
			fmt.Fprintf(&b, "  %s:%d  → %s\n", p.File, h.line, h.callee)
		} else {
			fmt.Fprintf(&b, "  → %s\n", h.callee)
		}
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// --- kai_dependencies ------------------------------------------------

type kaiDependenciesTool struct {
	db  KaiGrapher
	set *projects.Set
}

type kaiDependenciesParams struct {
	File string `json:"file"`
}

func (t *kaiDependenciesTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_dependencies",
		Description: "List the files the given file imports or otherwise depends on (depth 1, the " +
			"outbound complement of kai_dependents). Use it to see what a file pulls in before " +
			"moving or refactoring it.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Path of the target file relative to the repo root.",
			},
		},
		Required: []string{"file"},
	}
}

func (t *kaiDependenciesTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiDependenciesParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_dependencies: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.File) == "" {
		return NewTextErrorResponse("kai_dependencies: file required"), nil
	}
	db, file, projName := routeGraphForPath(t.set, t.db, p.File)
	if projName != "" {
		TraceRouting("kai_dependencies file=%q → db=%s rel=%q", p.File, projName, file)
	} else {
		TraceRouting("kai_dependencies file=%q → db=primary", p.File)
	}

	deps, err := dependenciesOfFile(db, file)
	if err != nil {
		return NewTextErrorResponse("kai_dependencies: " + err.Error()), nil
	}
	if len(deps) == 0 {
		return NewTextResponse(fmt.Sprintf("kai_dependencies: %q depends on nothing (depth 1)", p.File)), nil
	}
	prefix := ""
	if projName != "" {
		prefix = projName + "/"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dependencies of %s (%d):\n", p.File, len(deps))
	for _, d := range deps {
		fmt.Fprintf(&b, "  %s%s\n", prefix, d)
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// dependenciesOfFile returns the files filePath imports/calls into
// (depth 1). The inverse of dependentsOfFile: that one indexes by dst
// path (GetEdgesToByPath); there's no by-src-path index, so we scan the
// edge type once and keep edges whose SOURCE node is filePath.
func dependenciesOfFile(db KaiGrapher, filePath string) ([]string, error) {
	out := map[string]bool{}
	for _, et := range []graph.EdgeType{graph.EdgeImports, graph.EdgeCalls} {
		edges, err := db.GetEdgesOfType(et)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			src, err := db.GetNode(e.Src)
			if err != nil || src == nil {
				continue
			}
			if sp, _ := src.Payload["path"].(string); sp != filePath {
				continue
			}
			dst, err := db.GetNode(e.Dst)
			if err != nil || dst == nil {
				continue
			}
			dp, _ := dst.Payload["path"].(string)
			if dp == "" || dp == filePath {
				continue
			}
			out[dp] = true
		}
	}
	deps := make([]string, 0, len(out))
	for d := range out {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return deps, nil
}

// --- kai_tests -------------------------------------------------------

type kaiTestsTool struct {
	db  KaiGrapher
	set *projects.Set
}

type kaiTestsParams struct {
	File string `json:"file"`
}

func (t *kaiTestsTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_tests",
		Description: "List the test files that cover the given file — via recorded TESTS edges " +
			"and via test files that import it. Use it BEFORE editing to know which tests to run, " +
			"or to check whether code under a bug is tested at all.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Path of the file to find tests for, relative to the repo root.",
			},
		},
		Required: []string{"file"},
	}
}

func (t *kaiTestsTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiTestsParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_tests: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.File) == "" {
		return NewTextErrorResponse("kai_tests: file required"), nil
	}
	db, file, projName := routeGraphForPath(t.set, t.db, p.File)
	if projName != "" {
		TraceRouting("kai_tests file=%q → db=%s rel=%q", p.File, projName, file)
	} else {
		TraceRouting("kai_tests file=%q → db=primary", p.File)
	}

	tests, err := testsOfFile(db, file)
	if err != nil {
		return NewTextErrorResponse("kai_tests: " + err.Error()), nil
	}
	if len(tests) == 0 {
		return NewTextResponse(fmt.Sprintf("kai_tests: no tests found covering %q", p.File)), nil
	}
	prefix := ""
	if projName != "" {
		prefix = projName + "/"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tests covering %s (%d):\n", p.File, len(tests))
	for _, tf := range tests {
		fmt.Fprintf(&b, "  %s%s\n", prefix, tf)
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// testsOfFile finds test files covering filePath: direct TESTS edges
// (src = the test) plus test files that import it.
func testsOfFile(db KaiGrapher, filePath string) ([]string, error) {
	out := map[string]bool{}
	add := func(et graph.EdgeType, onlyTests bool) error {
		edges, err := db.GetEdgesToByPath(filePath, et)
		if err != nil {
			return err
		}
		for _, e := range edges {
			n, err := db.GetNode(e.Src)
			if err != nil || n == nil {
				continue
			}
			path, _ := n.Payload["path"].(string)
			if path == "" || path == filePath {
				continue
			}
			if onlyTests && !isTestPath(path) {
				continue
			}
			out[path] = true
		}
		return nil
	}
	if err := add(graph.EdgeTests, false); err != nil {
		return nil, err
	}
	if err := add(graph.EdgeImports, true); err != nil {
		return nil, err
	}
	tests := make([]string, 0, len(out))
	for tf := range out {
		tests = append(tests, tf)
	}
	sort.Strings(tests)
	return tests, nil
}

// isTestPath reports whether a path looks like a test file across the
// common conventions (Go, JS/TS, Python). Mirrors isTestFileCLI in the
// CLI; kept local to avoid a cross-package dependency.
func isTestPath(p string) bool {
	l := strings.ToLower(p)
	base := l
	if i := strings.LastIndex(l, "/"); i >= 0 {
		base = l[i+1:]
	}
	return strings.HasSuffix(l, "_test.go") ||
		strings.Contains(l, ".test.") ||
		strings.Contains(l, ".spec.") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(l, "/tests/") ||
		strings.Contains(l, "/test/") ||
		strings.Contains(l, "__tests__/")
}
