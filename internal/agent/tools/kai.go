package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"kai/internal/authorship"
	"kai/internal/graph"
	"kai/internal/projects"
)

// KaiTools wraps semantic-graph queries as agent tools so the model
// can reason about call structure, dependents, and impact mid-edit
// instead of deferring all of that to the post-run safety gate. This
// is the differentiator vs. vanilla file-editing agents — Kai's loop
// can ask "who calls this function?" before changing it.
//
// The graph DB this constructs against is the *main repo's* DB, not
// the spawn dir's. The agent's writes happen in the spawn dir, but
// "who calls X" is a question about the broader codebase, so we want
// the parent repo's view of the world.
//
// These tools implement the graph queries (callers, dependents,
// impact, context, symbols) directly against *graph.DB. They were
// once a parallel implementation to the MCP server's handlers; the
// MCP connector has since been removed, so the agent path is now the
// only consumer of this logic.
// KaiGrapher is the subset of *graph.DB the kai_* tools need.
// Defined as an interface so unit tests can substitute a minimal
// in-memory fake instead of spinning up SQLite. *graph.DB satisfies
// it directly.
type KaiGrapher interface {
	GetEdgesToByPath(filePath string, edgeType graph.EdgeType) ([]*graph.Edge, error)
	GetEdgesOfType(edgeType graph.EdgeType) ([]*graph.Edge, error)
	GetEdgesByDst(edgeType graph.EdgeType, dst []byte) ([]*graph.Edge, error)
	GetNode(id []byte) (*graph.Node, error)
	FindNodesByPayloadPath(kind, path string) ([]*graph.Node, error)
}

type KaiTools struct {
	// Set is the multi-root workspace. When non-nil, All() derives
	// DB and Workspace from Set.Primary() if they're not explicitly
	// set, so the existing single-graph tools continue to work
	// without modification.
	//
	// V1 ROUTING: file tools (view/write/edit, see file.go) route
	// per-path through the Set. The graph tools below
	// (kai_callers, kai_dependents, kai_context, kai_impact,
	// kai_symbols) currently query Set.Primary().DB regardless of
	// which project a queried file lives in. That's fine for
	// single-root and "good-enough" for multi-root where most
	// queries target the primary; per-project graph routing is a
	// known follow-up tracked separately.
	Set *projects.Set

	DB KaiGrapher
	// Workspace is the workspace root for the filesystem-walking
	// tools (kai_files, kai_grep). When empty those tools are
	// omitted from All() so the runner doesn't register tools that
	// can't actually do their job. The graph-only tools
	// (kai_callers, kai_dependents, kai_context) don't need it.
	Workspace string

	// Protected mirrors safetygate.Config.Protected so kai_impact
	// can flag protected paths in its results without depending on
	// the safetygate package (avoids an import cycle through the
	// runner). The runner threads its GateConfig.Protected here.
	Protected []string

	// KaiBinary is the absolute path to the kai executable used by
	// kai_diff to shell out to `kai diff -p`. Empty disables the
	// tool. The runner threads this from the cmd-side wiring.
	KaiBinary string

	// CheckpointWriter, when non-nil, enables kai_checkpoint to
	// record per-edit authorship under .kai/checkpoints/<session>/.
	// Constructed by the runner with kaiDir + sessionID; nil
	// silently omits the tool from registration.
	CheckpointWriter *authorship.CheckpointWriter

	// LiveSyncClient, ChannelID, AgentName configure kai_live_sync.
	// All three must be set for the tool to register. Single-agent
	// chat sessions leave LiveSyncClient nil and the tool is
	// omitted, so the model can't accidentally try to push to a
	// channel that doesn't exist.
	LiveSyncClient LiveSyncClient
	ChannelID      string
	AgentName      string
	// AgentModel is the LLM model id captured on each authorship
	// checkpoint. Optional but recommended; without it `kai blame`
	// can't filter by model.
	AgentModel string

	// ConsultProvider is the LLM transport used by kai_consult to
	// escalate a stuck agent to a stronger model. Typically the same
	// provider as the main agent (kailab) but invoked with a
	// different model id (ConsultModel below). When nil, kai_consult
	// is silently omitted from registration so the model never sees
	// the tool — the agent then has no escalation path and must
	// either edit or surface to the user.
	ConsultProvider Sender
	// ConsultModel is the model id kai_consult uses when invoking
	// ConsultProvider. Empty + non-nil ConsultProvider also omits
	// the tool — model is required to dispatch a consult.
	ConsultModel string
	// ConsultMode is the agent's current mode name (e.g. "coding",
	// "debug") injected into the consult prompt so the strong model
	// knows the operating context. Optional — empty just omits the
	// mode line. Provided by the runner from opts.Mode.String().
	ConsultMode string

	// KailabBaseURL + KailabToken configure kai_web_search. The tool
	// posts to ${KailabBaseURL}/api/v1/search with a Bearer token.
	// Both must be set for the tool to register; either missing
	// silently omits it (e.g. tests, offline runs, anyone who hasn't
	// done `kai auth login`).
	KailabBaseURL string
	KailabToken   string

	// ManagedProcLogger, when non-nil, enables kai_logs to read
	// recent output from the managed dev-server process the TUI is
	// watching (see v0.32.0 host_proc.go). Nil silently omits the
	// tool — orchestrator-spawned agents and tests don't have a
	// managed-process concept and shouldn't see the tool.
	ManagedProcLogger ManagedProcLogger
}

// Sender is the minimal provider surface kai_consult needs. Defined
// here as a one-method interface so the tools package doesn't have to
// import internal/agent/provider just for the type — that import would
// pull in the planner indirectly via shared message types and create
// a cycle. The runner satisfies it by passing its provider.Provider.
type Sender interface {
	Send(ctx context.Context, req SenderRequest) (SenderResponse, error)
}

// SenderRequest / SenderResponse mirror the slim subset of
// provider.Request / provider.Response kai_consult uses. The runner's
// adapter (in internal/agent/tools_consult_adapter.go, wired from
// runner.go) translates between these and the real provider types so
// this package stays cycle-free. Keeping the field names identical
// makes the adapter a one-line struct conversion.
type SenderRequest struct {
	Model     string
	System    string
	UserText  string
	MaxTokens int
}

type SenderResponse struct {
	Text string
}

// routeGraphForPath resolves which project's graph DB the caller
// should query and what path-form to use for the lookup. Closes the
// long-standing limitation (documented at the top of KaiTools) where
// graph tools always queried Set.Primary().DB regardless of which
// project a file lived in — an agent asking "who calls X in
// kai-server" got zero results because the primary's DB never indexed
// kai-server's symbols.
//
// Three return values:
//   - db: the DB to query (primary if not multi-root, or path
//     doesn't match a project, or the matched project has no DB)
//   - relPath: the path stripped of any "<project-name>/" prefix so
//     it matches what the per-project DB indexed
//   - projectName: name of the routed-to project for trace logging,
//     empty for primary
//
// Single-root callers (set nil or one project) pass through unchanged.
// In multi-root, the input path's first segment is matched against
// project names via set.ByName; on hit, the path tail is used against
// that project's DB.
func routeGraphForPath(set *projects.Set, primary KaiGrapher, inputPath string) (db KaiGrapher, relPath string, projectName string) {
	if set == nil || len(set.Projects()) <= 1 || inputPath == "" {
		return primary, inputPath, ""
	}
	head, rest, found := strings.Cut(inputPath, "/")
	if !found || head == "" {
		return primary, inputPath, ""
	}
	p := set.ByName(head)
	if p == nil || p.DB == nil {
		return primary, inputPath, ""
	}
	return p.DB, rest, p.Name
}

// symbolGrepHook lets kaiGrepTool consult the graph before doing a
// full filesystem scan. When the query looks like a single
// identifier and the graph has a symbol by that name, we return a
// terse "defined in X / called from Y" summary instead of raw
// text matches — typically 5-10× fewer tokens than the equivalent
// grep, and structurally more useful to the agent.
//
// Wired through KaiTools so the symbol-aware path stays cleanly
// separated from the text-walking path; tests can build the grep
// tool without a graph and the no-symbol fallback is exactly the
// previous behavior.
type symbolGrepHook interface {
	tryGrepSymbol(query string) (string, bool)
}

// All returns the registered kai_* tools as a slice for the
// runner's tool registry.
//
// Graph-backed tools (kai_callers, kai_dependents, kai_context)
// require DB; filesystem-walking tools (kai_files, kai_grep)
// require Workspace. Each is included only when its dependency is
// present, so partial wiring (graph-only or workspace-only) still
// produces a useful — if smaller — toolkit.
func (k *KaiTools) All() []BaseTool {
	if k == nil {
		return nil
	}
	// Derive DB/Workspace from Set.Primary() when callers haven't set
	// them explicitly. Lets the runner construct KaiTools with just
	// `Set` and have everything that needs a single-root view still
	// work. Tests that pre-set DB/Workspace are unaffected.
	if k.Set != nil {
		if k.DB == nil {
			if p := k.Set.Primary(); p != nil && p.DB != nil {
				k.DB = p.DB
			}
		}
		if k.Workspace == "" {
			if p := k.Set.Primary(); p != nil {
				k.Workspace = p.Path
			}
		}
	}
	var out []BaseTool
	if k.DB != nil {
		out = append(out,
			&kaiCallersTool{db: k.DB, set: k.Set},
			&kaiDependentsTool{db: k.DB, set: k.Set},
			&kaiContextTool{db: k.DB, set: k.Set},
			&kaiImpactTool{db: k.DB, protected: k.Protected, set: k.Set},
			&kaiDiagnoseTool{db: k.DB},
			// Inbound/outbound complements + coverage. Appended after
			// the originals so existing All()[N] index-based call sites
			// (and tests) keep their positions.
			&kaiCalleesTool{db: k.DB, set: k.Set},
			&kaiDependenciesTool{db: k.DB, set: k.Set},
			&kaiTestsTool{db: k.DB, set: k.Set},
		)
	}
	// kai_diff needs the kai binary path to shell out to `kai diff`.
	// Workspace alone isn't sufficient; the runner has to thread
	// the binary location through.
	if k.KaiBinary != "" && k.Workspace != "" {
		out = append(out, &kaiDiffTool{kaiBinary: k.KaiBinary, workspace: k.Workspace})
		out = append(out, &kaiBlameTool{kaiBinary: k.KaiBinary, workspace: k.Workspace})
		out = append(out, &kaiLogTool{kaiBinary: k.KaiBinary, workspace: k.Workspace})
	}
	// kai_consult registers only when both ConsultProvider and
	// ConsultModel are configured. Either missing → tool is silently
	// omitted, which is what we want for non-production wiring
	// (tests, single-agent paths) so the model never advertises a
	// capability it can't actually use.
	if k.ConsultProvider != nil && k.ConsultModel != "" {
		out = append(out, &kaiConsultTool{
			provider:  k.ConsultProvider,
			model:     k.ConsultModel,
			workspace: k.Workspace,
			mode:      k.ConsultMode,
		})
	}
	// kai_web_search registers when both kailab base URL and token
	// are configured. Reads the world (Brave proxy) so it doesn't
	// need workspace/DB; the runner just threads the auth in. Net-new
	// tool surface for facts the workspace can't answer — "latest
	// version of X", "release date of Y", etc.
	if k.KailabBaseURL != "" && k.KailabToken != "" {
		out = append(out, &kaiWebSearchTool{
			baseURL: k.KailabBaseURL,
			token:   k.KailabToken,
			client:  &http.Client{Timeout: webSearchHTTPTimeout},
		})
	}
	// kai_console attaches to any Chromium/Electron app the user is
	// running with --remote-debugging-port=<N>. No workspace/DB/auth
	// required — it just talks CDP over localhost, so it registers
	// unconditionally. This is the RT-1 sense organ: the planner can
	// finally observe runtime errors (sandbox crashes, uncaught
	// exceptions) instead of inferring them from static analysis.
	out = append(out, &kaiConsoleTool{})
	// kai_logs registers when a ManagedProcLogger is configured —
	// the TUI's chat-agent path sets this so the chat agent can
	// answer "do you see the error?" by reading the managed
	// dev-server's recent stdout/stderr. Orchestrator-spawned
	// agents don't get the tool (no managed-process concept).
	if k.ManagedProcLogger != nil {
		out = append(out, &kaiLogsTool{logger: k.ManagedProcLogger})
	}
	// kai_checkpoint needs the authorship writer (which encodes
	// kaiDir + sessionID). Single-agent chat without persistence
	// configured silently omits the tool.
	if k.CheckpointWriter != nil {
		out = append(out, &kaiCheckpointTool{
			writer: k.CheckpointWriter,
			agent:  k.AgentName,
			model:  k.AgentModel,
		})
	}
	// kai_live_sync registers only when the orchestrator wired up a
	// remote client AND a channel id. Chat-only runs leave both nil
	// so the model never sees the tool.
	if k.LiveSyncClient != nil && k.ChannelID != "" {
		out = append(out, &kaiLiveSyncTool{
			client:    k.LiveSyncClient,
			workspace: k.Workspace,
			channelID: k.ChannelID,
			agent:     k.AgentName,
		})
	}
	if k.Workspace != "" {
		// kai_git_state needs only the workspace + (optional) Set —
		// no DB. Registered alongside the other filesystem tools so
		// it shares their multi-root path resolution semantics.
		out = append(out, &kaiGitStateTool{workspace: k.Workspace, set: k.Set})
		// kai_search registers only when both workspace AND a graph
		// DB that satisfies the Searcher interface are available
		// (the FTS5 table lives in the same DB as the symbol graph).
		// The interface check is necessary because the existing
		// KaiGrapher interface doesn't expose IndexFile/SearchText
		// — those are only on the concrete *graph.DB.
		if searcher, ok := k.DB.(Searcher); ok {
			out = append(out, &kaiSearchTool{
				workspace: k.Workspace,
				set:       k.Set,
				db:        searcher,
				grapher:   k.DB, // same DB, used for Symbol-node lookup
			})
		}
		grep := &kaiGrepTool{workspace: k.Workspace, set: k.Set}
		// Hand the graph to kai_grep so an identifier-shaped query
		// can short-circuit through symbol lookup before falling
		// back to a text walk.
		if k.DB != nil {
			grep.symbolHook = &graphSymbolHook{db: k.DB, set: k.Set}
		}
		out = append(out,
			// set: passed to all three so empty-path searches
			// walk EVERY project root, not just the primary
			// workspace. Without this, a multi-root user
			// asking "where is X?" gets back a confidently-
			// wrong "doesn't exist" answer because the tools
			// silently scoped to the first root.
			&kaiFilesTool{workspace: k.Workspace, set: k.Set},
			grep,
			&kaiTreeTool{workspace: k.Workspace, set: k.Set},
		)
	}
	// kai_symbols needs both: graph for the symbol payloads, and
	// workspace for the directory walk it uses when the agent
	// passes a `path` instead of a single `file`. With only one of
	// the two, the partial functionality would be confusing —
	// better to leave it off until both are wired.
	if k.DB != nil && k.Workspace != "" {
		out = append(out, &kaiSymbolsTool{db: k.DB, workspace: k.Workspace, set: k.Set})
	}
	return out
}

// --- kai_callers -----------------------------------------------------

type kaiCallersTool struct {
	db  KaiGrapher
	set *projects.Set
}

type kaiCallersParams struct {
	Symbol string `json:"symbol"`
	File   string `json:"file"`
}

func (t *kaiCallersTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_callers",
		Description: "Find files and line numbers that call the given symbol (function/method). " +
			"Optionally scope to a file (faster + more accurate when the symbol is common). " +
			"Use this BEFORE editing a function to understand who depends on it.",
		Parameters: map[string]any{
			"symbol": map[string]any{
				"type":        "string",
				"description": "Function or method name. Trailing receiver is stripped automatically (e.g. *Resolver.Resolve → Resolve).",
			},
			"file": map[string]any{
				"type":        "string",
				"description": "Optional file path to scope the search to (faster).",
			},
		},
		Required: []string{"symbol"},
	}
}

func (t *kaiCallersTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiCallersParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_callers: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.Symbol) == "" {
		return NewTextErrorResponse("kai_callers: symbol required"), nil
	}
	// Per-project routing: when the caller supplies a file with a
	// project-name prefix (e.g. "kai-server/.../foo.go"), route the
	// graph query to that project's DB and use the project-relative
	// path. With no file, we can't disambiguate the symbol's project
	// from the input alone — stay on primary. (Cross-project symbol
	// search is a future enhancement.)
	db, file, projName := routeGraphForPath(t.set, t.db, p.File)
	if projName != "" {
		TraceRouting("kai_callers symbol=%q file=%q → db=%s rel=%q", p.Symbol, p.File, projName, file)
	} else {
		TraceRouting("kai_callers symbol=%q file=%q → db=primary", p.Symbol, p.File)
	}
	target := normalizeSymbolName(p.Symbol)

	var edges []*graph.Edge
	var err error
	if file != "" {
		edges, err = db.GetEdgesToByPath(file, graph.EdgeCalls)
	} else {
		edges, err = db.GetEdgesOfType(graph.EdgeCalls)
	}
	if err != nil {
		return NewTextErrorResponse("kai_callers: " + err.Error()), nil
	}

	type hit struct{ file string; line int; callee string }
	var hits []hit
	seen := map[string]bool{}
	for _, e := range edges {
		if len(e.At) == 0 {
			continue
		}
		callNode, err := db.GetNode(e.At)
		if err != nil || callNode == nil {
			continue
		}
		callee, _ := callNode.Payload["calleeName"].(string)
		if normalizeSymbolName(callee) != target {
			continue
		}
		caller, _ := callNode.Payload["callerFile"].(string)
		line := 0
		if l, ok := callNode.Payload["line"].(float64); ok {
			line = int(l)
		}
		key := fmt.Sprintf("%s:%d", caller, line)
		if seen[key] {
			continue
		}
		seen[key] = true
		hits = append(hits, hit{file: caller, line: line, callee: callee})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})

	if len(hits) == 0 {
		return NewTextResponse(fmt.Sprintf("kai_callers: no callers of %q found", p.Symbol)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "callers of %s (%d hits):\n", p.Symbol, len(hits))
	for _, h := range hits {
		if h.line > 0 {
			fmt.Fprintf(&b, "  %s:%d  → %s\n", h.file, h.line, h.callee)
		} else {
			fmt.Fprintf(&b, "  %s  → %s\n", h.file, h.callee)
		}
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// --- kai_dependents --------------------------------------------------

type kaiDependentsTool struct {
	db  KaiGrapher
	set *projects.Set
}

type kaiDependentsParams struct {
	File string `json:"file"`
}

func (t *kaiDependentsTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_dependents",
		Description: "List files that import or otherwise depend on the given file (depth 1). " +
			"This is the file-level blast-radius — if you change this file, what else might break? " +
			"Use this BEFORE editing a file with broad imports.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Path of the target file relative to the repo root.",
			},
		},
		Required: []string{"file"},
	}
}

func (t *kaiDependentsTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiDependentsParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_dependents: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.File) == "" {
		return NewTextErrorResponse("kai_dependents: file required"), nil
	}
	db, file, projName := routeGraphForPath(t.set, t.db, p.File)
	if projName != "" {
		TraceRouting("kai_dependents file=%q → db=%s rel=%q", p.File, projName, file)
	} else {
		TraceRouting("kai_dependents file=%q → db=primary", p.File)
	}

	deps, err := dependentsOfFile(db, file)
	if err != nil {
		return NewTextErrorResponse("kai_dependents: " + err.Error()), nil
	}
	if len(deps) == 0 {
		return NewTextResponse(fmt.Sprintf("kai_dependents: nothing depends on %q (depth 1)", p.File)), nil
	}
	// Re-prefix per-project results with the project name so paths
	// are unambiguous when the agent pipes them back into view/edit
	// in a multi-root workspace.
	prefix := ""
	if projName != "" {
		prefix = projName + "/"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dependents of %s (%d):\n", p.File, len(deps))
	for _, d := range deps {
		fmt.Fprintf(&b, "  %s%s\n", prefix, d)
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// --- kai_context -----------------------------------------------------

type kaiContextTool struct {
	db  KaiGrapher
	set *projects.Set
}

type kaiContextParams struct {
	File string `json:"file"`
}

func (t *kaiContextTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_context",
		Description: "Summarize what's in a file (top-level symbols) plus depth-1 dependents " +
			"(files that import it). Cheap-but-informative orientation step before editing — " +
			"shorter than `view` for large files.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Path of the target file relative to the repo root.",
			},
		},
		Required: []string{"file"},
	}
}

func (t *kaiContextTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiContextParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return NewTextErrorResponse("kai_context: invalid input json: " + err.Error()), nil
	}
	if strings.TrimSpace(p.File) == "" {
		return NewTextErrorResponse("kai_context: file required"), nil
	}
	db, file, projName := routeGraphForPath(t.set, t.db, p.File)
	if projName != "" {
		TraceRouting("kai_context file=%q → db=%s rel=%q", p.File, projName, file)
	} else {
		TraceRouting("kai_context file=%q → db=primary", p.File)
	}

	fileNodes, err := db.FindNodesByPayloadPath(string(graph.KindFile), file)
	if err != nil {
		return NewTextErrorResponse("kai_context: " + err.Error()), nil
	}
	if len(fileNodes) == 0 {
		return NewTextErrorResponse(fmt.Sprintf("kai_context: file not found in graph: %s", p.File)), nil
	}
	fileNode := fileNodes[0]

	// Top-level symbols defined in the file via DEFINES_IN edges
	// pointing at this file from symbol nodes.
	defEdges, err := db.GetEdgesByDst(graph.EdgeDefinesIn, fileNode.ID)
	if err != nil {
		return NewTextErrorResponse("kai_context: " + err.Error()), nil
	}
	type sym struct{ name, kind string }
	var syms []sym
	seenSym := map[string]bool{}
	for _, e := range defEdges {
		n, err := db.GetNode(e.Src)
		if err != nil || n == nil {
			continue
		}
		name, _ := n.Payload["fqName"].(string)
		kind, _ := n.Payload["kind"].(string)
		if name == "" || seenSym[name] {
			continue
		}
		seenSym[name] = true
		syms = append(syms, sym{name: name, kind: kind})
	}
	sort.Slice(syms, func(i, j int) bool { return syms[i].name < syms[j].name })

	deps, _ := dependentsOfFile(db, file)

	var b strings.Builder
	fmt.Fprintf(&b, "context for %s\n", p.File)
	if len(syms) == 0 {
		b.WriteString("  symbols: (none indexed)\n")
	} else {
		b.WriteString("  symbols:\n")
		for _, s := range syms {
			if s.kind != "" {
				fmt.Fprintf(&b, "    [%s] %s\n", s.kind, s.name)
			} else {
				fmt.Fprintf(&b, "    %s\n", s.name)
			}
		}
	}
	if len(deps) == 0 {
		b.WriteString("  dependents (depth 1): (none)\n")
	} else {
		// Re-prefix dependent paths with the project name so they
		// match the multi-root convention the rest of the agent uses.
		prefix := ""
		if projName != "" {
			prefix = projName + "/"
		}
		fmt.Fprintf(&b, "  dependents (depth 1, %d):\n", len(deps))
		for _, d := range deps {
			fmt.Fprintf(&b, "    %s%s\n", prefix, d)
		}
	}
	return NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

// --- kai_symbols -----------------------------------------------------

type kaiSymbolsTool struct {
	db        KaiGrapher
	workspace string
	set       *projects.Set
}

type kaiSymbolsParams struct {
	// File is a single workspace-relative file. Mutually exclusive
	// with Path: when both are set, File wins.
	File string `json:"file"`
	// Path is a workspace-relative directory; the tool walks it
	// (skipping the usual noise dirs) and emits symbols for every
	// indexed file underneath. Empty means use the whole workspace
	// — fine for small repos but may produce a lot of output, so
	// the result is capped.
	Path string `json:"path"`
	// Kind, when set, restricts output to a single symbol kind
	// (e.g. "function", "type", "method", "const"). Matching is
	// case-insensitive against the kind payload kai parsed.
	Kind string `json:"kind"`
}

func (t *kaiSymbolsTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_symbols",
		Description: "List top-level symbols (functions, types, methods, constants) defined in a file " +
			"or under a directory, from the kai graph. Replaces patterns like " +
			"`grep -rn '^func ' <dir>`. Faster and accurate because it uses kai's parsed AST, " +
			"not regex on raw text.",
		Parameters: map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "Single workspace-relative file. Pass this OR `path`, not both.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Workspace-relative directory to walk. Empty = whole workspace.",
			},
			"kind": map[string]any{
				"type":        "string",
				"description": "Optional filter: only symbols of this kind (e.g. \"function\", \"type\", \"method\").",
			},
		},
		Required: []string{},
	}
}

func (t *kaiSymbolsTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiSymbolsParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_symbols: invalid input json: " + err.Error()), nil
		}
	}
	kindFilter := strings.ToLower(strings.TrimSpace(p.Kind))

	// Per-project routing. kai_symbols takes either a `file` (single)
	// or a `path` (directory walk). For multi-root, route based on
	// whichever has a project-name prefix; with neither, fall back
	// to primary. Cross-project whole-workspace symbol listing is a
	// known follow-up (would need to walk each project's DB and
	// merge).
	routingHint := p.File
	if routingHint == "" {
		routingHint = p.Path
	}
	db, relRoutingHint, projName := routeGraphForPath(t.set, t.db, routingHint)
	// Workspace also needs to follow routing when we're going to walk
	// a project — walkIndexableFiles needs the right filesystem root.
	walkRoot := t.workspace
	walkPath := p.Path
	if projName != "" {
		// Find the project to get its on-disk Path.
		if proj := t.set.ByName(projName); proj != nil {
			walkRoot = proj.Path
			walkPath = relRoutingHint
		}
	}
	if projName != "" {
		TraceRouting("kai_symbols file=%q path=%q kind=%q → db=%s rel=%q", p.File, p.Path, kindFilter, projName, relRoutingHint)
	} else {
		TraceRouting("kai_symbols file=%q path=%q kind=%q → db=primary", p.File, p.Path, kindFilter)
	}

	var files []string
	if strings.TrimSpace(p.File) != "" {
		// File arg: the path that goes into the DB lookup is the
		// project-relative form (relRoutingHint).
		files = []string{relRoutingHint}
	} else {
		paths, err := walkIndexableFiles(walkRoot, walkPath)
		if err != nil {
			return NewTextErrorResponse("kai_symbols: " + err.Error()), nil
		}
		files = paths
	}

	// Output budgets. kai_symbols can land in the conversation
	// history dozens of times across a session, so a single
	// run-away result (large package, no kind filter) snowballs
	// into thousands of repeat tokens. Caps tune for "show enough
	// to orient" rather than "list everything"; the agent can
	// narrow with `path` or `kind` if it needs more.
	const (
		fileCap     = 60
		perFileCap  = 30
		totalSymCap = 250
	)
	if len(files) > fileCap {
		files = files[:fileCap]
	}

	var b strings.Builder
	totalSyms := 0
	filesShown := 0
	totalCapped := false
	for _, f := range files {
		syms, err := symbolsForFile(db, f)
		if err != nil {
			continue
		}
		// Filter by kind if requested. Done after the lookup so the
		// "(none indexed)" diagnostic still fires when the file's
		// in the graph but has nothing of the requested kind.
		if kindFilter != "" {
			filtered := syms[:0]
			for _, s := range syms {
				if strings.EqualFold(s.kind, kindFilter) {
					filtered = append(filtered, s)
				}
			}
			syms = filtered
		}
		if len(syms) == 0 {
			continue
		}
		filesShown++
		fmt.Fprintf(&b, "%s\n", f)
		shown := 0
		for _, s := range syms {
			if shown >= perFileCap {
				fmt.Fprintf(&b, "  … +%d more in this file\n", len(syms)-shown)
				break
			}
			if s.kind != "" {
				fmt.Fprintf(&b, "  [%s] %s\n", s.kind, s.name)
			} else {
				fmt.Fprintf(&b, "  %s\n", s.name)
			}
			shown++
			totalSyms++
			if totalSyms >= totalSymCap {
				totalCapped = true
				break
			}
		}
		if totalCapped {
			break
		}
	}
	if totalSyms == 0 {
		scope := p.File
		if scope == "" {
			scope = p.Path
		}
		if scope == "" {
			scope = "(workspace)"
		}
		if kindFilter != "" {
			return NewTextResponse(fmt.Sprintf("kai_symbols: no %s symbols indexed under %s", kindFilter, scope)), nil
		}
		return NewTextResponse(fmt.Sprintf("kai_symbols: no symbols indexed under %s", scope)), nil
	}
	header := fmt.Sprintf("kai_symbols: %d symbol(s) across %d file(s)", totalSyms, filesShown)
	switch {
	case totalCapped:
		header += " (output capped — narrow path/kind for more)"
	case len(files) >= fileCap:
		header += " (file walk capped — narrow path for more)"
	}
	return NewTextResponse(header + "\n" + strings.TrimRight(b.String(), "\n")), nil
}

// --- graph-backed symbol grep ----------------------------------------

// graphSymbolHook implements symbolGrepHook for the real graph.
// Two-step lookup: find a symbol node by exact fqName/shortName,
// then collect its definition file and (depth-1) callers from the
// graph. Falls through (returns false) on any miss so kai_grep's
// text-walk takes over — this is a fast-path optimization, not a
// replacement.
// graphSymbolHook resolves a bare-identifier kai_grep query against the
// parsed graph. In a multi-root workspace it searches EVERY initialized
// project's own DB (not just the primary), because a symbol defined in a
// sibling project — e.g. kai-tui's GetEdgesToByPath — does not exist in
// the primary project's graph at all (each project has its own DB). The
// 2026-05-29 dogfood pinned this: a planner correctly tried to look in
// kai-tui per the topology guidance, but symbol grep only queried the
// primary (kai) graph, returned "no matches", and fell back to the wrong
// tree. set is nil / single-root → primary db only (unchanged).
type graphSymbolHook struct {
	db  KaiGrapher
	set *projects.Set
}

func (h *graphSymbolHook) tryGrepSymbol(query string) (string, bool) {
	// Tolerate a nil graph the same way callersOf/importersOf do:
	// the planner agent (and any caller using projects.Single
	// without an opened DB) wires kai_grep through this hook even
	// when no graph is available. Without nil-safety here, the
	// agent loop panics the first time the model issues a kai_grep
	// for an identifier-shaped term.
	if h == nil {
		return "", false
	}
	if !looksLikeIdentifier(query) {
		return "", false
	}

	type defHit struct {
		name   string
		kind   string
		file   string     // project-relative path (for callersOfFile against db)
		db     KaiGrapher // the project DB this def came from
		prefix string     // project-name prefix for display ("" in single-root)
	}

	// Graphs to search. Multi-root: every initialized project's OWN db,
	// so a symbol defined in a sibling (kai-tui) is found even though the
	// primary graph never indexed it. Single-root / unset: the primary db.
	type graphTarget struct {
		db     KaiGrapher
		prefix string
	}
	var targets []graphTarget
	if h.set != nil && len(h.set.Projects()) > 1 {
		for _, p := range h.set.Projects() {
			if p == nil || p.DB == nil {
				continue
			}
			targets = append(targets, graphTarget{db: p.DB, prefix: p.Name + "/"})
		}
	}
	if len(targets) == 0 {
		if h.db == nil {
			return "", false
		}
		targets = append(targets, graphTarget{db: h.db, prefix: ""})
	}

	const maxDefs = 10
	var defs []defHit
	for _, tg := range targets {
		if len(defs) >= maxDefs {
			break
		}
		// We don't have a direct "find symbol by name" on the interface
		// yet; iterate DEFINES_IN edges and check Src node payloads.
		edges, err := tg.db.GetEdgesOfType(graph.EdgeDefinesIn)
		if err != nil || len(edges) == 0 {
			continue
		}
		for _, e := range edges {
			if len(defs) >= maxDefs {
				break
			}
			sym, err := tg.db.GetNode(e.Src)
			if err != nil || sym == nil {
				continue
			}
			fq, _ := sym.Payload["fqName"].(string)
			if fq == "" {
				continue
			}
			// Match the full fqName or the trailing component, so
			// "Login" hits both "auth.Login" and "Login".
			if fq != query && trailingComponent(fq) != query {
				continue
			}
			file, _ := tg.db.GetNode(e.Dst)
			if file == nil {
				continue
			}
			filePath, _ := file.Payload["path"].(string)
			// Drop stale .claude/worktrees/ mirror copies — git-worktree
			// dupes left in the graph from before .claude was excluded
			// from the capture walk. They produce N identical defs for
			// one symbol, which (with the maxDefs cap) starve the real
			// defs — including sibling projects' — out of the results.
			// kai_search drops these too (dropWorktreeHits). 2026-05-29:
			// GetEdgesToByPath returned 11 worktree copies and 0 kai-tui.
			if strings.Contains(filePath, ".claude/worktrees/") {
				continue
			}
			kind, _ := sym.Payload["kind"].(string)
			defs = append(defs, defHit{name: fq, kind: kind, file: filePath, db: tg.db, prefix: tg.prefix})
		}
	}
	if len(defs) == 0 {
		return "", false
	}

	// Pull caller files for each definition — against the SAME db the def
	// came from, using the project-relative path; display is prefixed.
	var b strings.Builder
	fmt.Fprintf(&b, "kai_grep (symbol mode): %q resolved via graph — %d definition(s)\n",
		query, len(defs))
	for _, d := range defs {
		disp := d.prefix + d.file
		if d.kind != "" {
			fmt.Fprintf(&b, "  [%s] %s — defined in %s\n", d.kind, d.name, disp)
		} else {
			fmt.Fprintf(&b, "  %s — defined in %s\n", d.name, disp)
		}
		callers := callersOfFile(d.db, d.file)
		if len(callers) == 0 {
			b.WriteString("    callers: (none in graph)\n")
			continue
		}
		const maxCallers = 8
		display := callers
		extra := 0
		if len(display) > maxCallers {
			extra = len(display) - maxCallers
			display = display[:maxCallers]
		}
		if d.prefix != "" {
			for i := range display {
				display[i] = d.prefix + display[i]
			}
		}
		fmt.Fprintf(&b, "    callers (%d): %s", len(callers), strings.Join(display, ", "))
		if extra > 0 {
			fmt.Fprintf(&b, ", … +%d more", extra)
		}
		b.WriteByte('\n')
	}
	b.WriteString("(symbol-mode results from kai's parsed graph across all projects; no text scan run. Pass `regex: true` or a non-identifier query to force text-walk.)")
	return b.String(), true
}

// looksLikeIdentifier returns true when the query is plausibly a
// single code identifier — letters/digits/underscore, no
// whitespace, modest length. Heuristic, not a parser: false
// positives just mean we attempt a graph lookup that returns no
// match and fall through, which is cheap.
func looksLikeIdentifier(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 80 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '.', r == ':':
			continue
		default:
			return false
		}
	}
	return true
}

// trailingComponent returns the last "." or "::" delimited piece
// of a possibly-qualified symbol name. Mirrors normalizeSymbolName
// but doesn't strip — we check both forms when matching.
func trailingComponent(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// callersOfFile returns the file paths that import or call into
// the given file (depth 1). Used by graphSymbolHook to flesh out
// the symbol's reach without a separate kai_callers call.
func callersOfFile(db KaiGrapher, file string) []string {
	if file == "" {
		return nil
	}
	out := map[string]bool{}
	for _, et := range []graph.EdgeType{graph.EdgeCalls, graph.EdgeImports} {
		edges, err := db.GetEdgesToByPath(file, et)
		if err != nil {
			continue
		}
		for _, e := range edges {
			n, err := db.GetNode(e.Src)
			if err != nil || n == nil {
				continue
			}
			p, _ := n.Payload["path"].(string)
			if p == "" || p == file {
				continue
			}
			out[p] = true
		}
	}
	res := make([]string, 0, len(out))
	for k := range out {
		res = append(res, k)
	}
	sort.Strings(res)
	return res
}

// --- shared helpers --------------------------------------------------

// symInfo is the (name, kind) pair the tools emit per symbol. Kept
// unexported because callers don't construct it; symbolsForFile is
// the only producer.
type symInfo struct{ name, kind string }

// symbolsForFile pulls top-level symbols defined in a single file
// from the graph. Returns an empty slice when the file isn't
// indexed (vs. an error) so callers can iterate over many files
// without bailing on the first miss.
func symbolsForFile(db KaiGrapher, filePath string) ([]symInfo, error) {
	fileNodes, err := db.FindNodesByPayloadPath(string(graph.KindFile), filePath)
	if err != nil {
		return nil, err
	}
	if len(fileNodes) == 0 {
		return nil, nil
	}
	defEdges, err := db.GetEdgesByDst(graph.EdgeDefinesIn, fileNodes[0].ID)
	if err != nil {
		return nil, err
	}
	var out []symInfo
	seen := map[string]bool{}
	for _, e := range defEdges {
		n, err := db.GetNode(e.Src)
		if err != nil || n == nil {
			continue
		}
		name, _ := n.Payload["fqName"].(string)
		kind, _ := n.Payload["kind"].(string)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, symInfo{name: name, kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// dependentsOfFile collects unique paths that have an inbound IMPORTS
// or CALLS edge to the given file. Mirrors the safety gate's
// blast-radius primitive (`safetygate.blastRadius`); when the MCP +
// agent paths are unified (post-Slice 6) this should consolidate
// into one shared helper.
func dependentsOfFile(db KaiGrapher, filePath string) ([]string, error) {
	out := map[string]bool{}
	for _, et := range []graph.EdgeType{graph.EdgeImports, graph.EdgeCalls} {
		edges, err := db.GetEdgesToByPath(filePath, et)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			n, err := db.GetNode(e.Src)
			if err != nil || n == nil {
				continue
			}
			p, _ := n.Payload["path"].(string)
			if p == "" || p == filePath {
				continue
			}
			out[p] = true
		}
	}
	deps := make([]string, 0, len(out))
	for d := range out {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return deps, nil
}

// normalizeSymbolName strips qualifying prefixes the parser might
// have stored on a CALLS edge's calleeName payload — `Type.Method`
// → `Method`, `crate::foo::bar` → `bar`. Same logic as the MCP
// server's findCallersViaFileEdges; once we extract to a shared
// helper this duplication goes away.
func normalizeSymbolName(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	return s
}
