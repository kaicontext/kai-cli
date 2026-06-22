package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"kai/internal/ai"
	"kai/internal/graph"
	"kai/internal/safetygate"
)

// Completer is the LLM interface the planner needs. Defined here as
// an interface so tests can substitute a fixture without making real
// API calls. *aiAdapter (below) wraps the real ai.Client.
type Completer interface {
	Complete(system string, messages []ai.Message, maxTokens int) (string, error)
}

// Config controls a planner call. Caller supplies it from
// internal/config.PlannerConfig (the on-disk config).
type Config struct {
	// Model is the Anthropic model id. Currently the underlying
	// ai.Client hardcodes its model — Model here is recorded in the
	// system prompt for forward compatibility but does not yet
	// override the client's choice. Wire-up is a one-line patch in
	// ai.Client when needed.
	Model string

	// MaxAgents caps how many agents the LLM is allowed to propose.
	// The cap is communicated to the LLM via the system prompt and
	// enforced after parsing as defense-in-depth.
	MaxAgents int

	// MaxTokens sized for one structured response. Defaults to 4096
	// if zero.
	MaxTokens int
}

// ErrTooVague is returned when the LLM produces a plan with no agents
// — typically because the request didn't name anything concrete the
// graph could resolve. The REPL surfaces this so the user can rephrase.
var ErrTooVague = errors.New("planner: request too vague — name a file, package, or feature")

// ErrUnparseable is returned when the LLM response can't be parsed as
// JSON matching WorkPlan. The error wraps the raw text so the REPL
// can show it to the user verbatim.
type ErrUnparseable struct {
	Raw string
	Err error
}

func (e *ErrUnparseable) Error() string {
	return fmt.Sprintf("planner: could not parse LLM response: %v", e.Err)
}
func (e *ErrUnparseable) Unwrap() error { return e.Err }

// Plan resolves a natural-language request into a WorkPlan. Steps:
//
//  1. Resolve concrete files in the request (substring match against
//     the graph's file paths). Best-effort; an unresolved request
//     still gets planned, just with less context.
//  2. Build a context payload (resolved files + callers/dependents
//     at depth 1 + protected globs + top-level dir tree).
//  3. One LLM call with a JSON-schema system prompt.
//  4. Parse + validate. Refuse empty plans.
//
// The graph parameter must be the live DB from the main repo. If it's
// nil the planner falls back to "no resolvable context" and asks the
// LLM to plan from the bare request — useful for tests.
func Plan(ctx context.Context, request string, g GraphAccess, gateCfg safetygate.Config, cfg Config, llm Completer) (*WorkPlan, error) {
	request = strings.TrimSpace(request)
	if request == "" {
		return nil, fmt.Errorf("planner: empty request")
	}
	if llm == nil {
		return nil, fmt.Errorf("planner: no LLM client")
	}

	resolved := resolveFiles(request, g)
	context := buildContext(request, resolved, g, gateCfg, cfg)

	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 4096
	}

	raw, err := llm.Complete(systemPrompt(cfg.MaxAgents), []ai.Message{
		{Role: "user", Content: context},
	}, cfg.MaxTokens)
	if err != nil {
		return nil, fmt.Errorf("planner: LLM call: %w", err)
	}

	plan, err := parsePlan(raw)
	if err != nil {
		return nil, &ErrUnparseable{Raw: raw, Err: err}
	}
	if len(plan.Agents) == 0 {
		return nil, ErrTooVague
	}
	// Defense-in-depth: clip to MaxAgents even if the LLM ignored
	// the system prompt's instruction.
	if cfg.MaxAgents > 0 && len(plan.Agents) > cfg.MaxAgents {
		plan.Agents = plan.Agents[:cfg.MaxAgents]
		plan.RiskNotes = append(plan.RiskNotes,
			fmt.Sprintf("plan truncated to MaxAgents=%d", cfg.MaxAgents))
	}
	return plan, nil
}

// Chat runs a one-shot conversational reply for input that's too
// vague to plan ("hi", "what can you do?", "thanks"). The REPL falls
// back to this when Plan returns ErrTooVague so users don't get
// stuck on the error message.
//
// Intentionally simple: no graph context, no tools, no streaming —
// just a system-prompted single completion. If the user actually
// wants a change, the system prompt nudges them toward describing
// a file or feature.
func Chat(ctx context.Context, request string, cfg Config, llm Completer) (string, error) {
	if llm == nil {
		return "", fmt.Errorf("planner: no LLM client")
	}
	request = strings.TrimSpace(request)
	if request == "" {
		return "", fmt.Errorf("planner: empty chat request")
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	resp, err := llm.Complete(chatSystemPrompt(), []ai.Message{
		{Role: "user", Content: request},
	}, maxTokens)
	if err != nil {
		return "", fmt.Errorf("planner: chat: %w", err)
	}
	return strings.TrimSpace(resp), nil
}

// chatSystemPrompt instructs the model to be brief and to nudge the
// user toward planner-friendly requests when they're trying to make
// a change. Kept short so it doesn't eat tokens.
func chatSystemPrompt() string {
	return `You are the conversational fallback for the kai CLI's REPL. The user typed something that wasn't a recognized command and wasn't concrete enough to plan a code change.

Reply briefly (1–3 sentences) and helpfully:
  - For greetings or chitchat, respond naturally.
  - For questions about kai, answer if you can.
  - If they seem to want a code change, ask them to name a file, package, or specific behavior so the planner can act (e.g. "Add a /health endpoint to index.js", "Update README to mention X").

Don't write code. Don't propose plans. Just chat.`
}

// Replan combines the original request with feedback and re-plans.
// One LLM call, no conversation. If the user wants more rounds they
// can keep providing feedback in the REPL — each round is independent.
func Replan(ctx context.Context, original, feedback string, g GraphAccess, gateCfg safetygate.Config, cfg Config, llm Completer) (*WorkPlan, error) {
	combined := fmt.Sprintf("%s\n\nFeedback on the previous plan: %s",
		strings.TrimSpace(original), strings.TrimSpace(feedback))
	return Plan(ctx, combined, g, gateCfg, cfg, llm)
}

// GraphAccess is the subset of *graph.DB the planner needs. Defined
// as an interface so tests can substitute an in-memory fake without
// spinning up SQLite.
type GraphAccess interface {
	GetNodesByKind(kind graph.NodeKind) ([]*graph.Node, error)
	GetEdgesToByPath(filePath string, edgeType graph.EdgeType) ([]*graph.Edge, error)
	GetEdgesOfType(edgeType graph.EdgeType) ([]*graph.Edge, error)
	GetNode(id []byte) (*graph.Node, error)
}

// systemPrompt returns the instructions sent to the LLM. JSON schema
// is described inline so the model knows the exact shape to produce.
//
// We deliberately don't include examples — they bias the model toward
// the example's task split. The schema description plus the user
// context is enough.
func systemPrompt(maxAgents int) string {
	if maxAgents <= 0 {
		maxAgents = 5
	}
	return fmt.Sprintf(`You are a work planner for a code-modifying agent system. The user describes a change they want; your job is to split it into independent agent tasks.

Output ONLY a single JSON object matching this schema:

{
  "summary": "one short sentence describing the change",
  "agents": [
    {
      "name": "short-kebab-case",
      "prompt": "what this agent should do, 1-3 sentences, concrete",
      "files": ["path/relative/to/repo.go", ...],
      "dont_touch": ["path/that/this/agent/must/not/edit", ...]
    }
  ],
  "risk_notes": ["bullet about risk or impact, optional"]
}

Rules:
- Maximum %d agents. Fewer is better. One agent is fine if the change is small.
- Each agent must be independent — they run in parallel.
- "files" should be the planner's best guess at what each agent will touch. May overlap (live sync handles it) but try to minimize overlap.
- "dont_touch" is for files this specific agent must not edit (e.g. another agent's responsibility, or a protected path the user surfaced).
- If the request is too vague to plan (no named file, package, feature, or concrete behavior), return {"summary":"too vague","agents":[]}.
- Output ONLY the JSON object. No prose, no markdown fences, no explanation.`, maxAgents)
}

// buildContext composes the user-side message: the request, any files
// the planner could resolve from substring matching, depth-1 graph
// neighbors of those files, the gate's protected globs, and a summary
// of the top-level repo layout.
func buildContext(request string, resolved []string, g GraphAccess, gateCfg safetygate.Config, cfg Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Request: %s\n\n", request)

	if len(resolved) > 0 {
		b.WriteString("Files mentioned or matched in the request:\n")
		for _, p := range resolved {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteByte('\n')

		// Depth-1 callers/dependents per resolved file.
		neighbors := neighborMap(resolved, g)
		if len(neighbors) > 0 {
			b.WriteString("Direct callers / importers (depth 1):\n")
			for _, p := range resolved {
				ns := neighbors[p]
				if len(ns) == 0 {
					continue
				}
				fmt.Fprintf(&b, "  %s ← %s\n", p, strings.Join(ns, ", "))
			}
			b.WriteByte('\n')
		}
	}

	if len(gateCfg.Protected) > 0 {
		b.WriteString("Protected paths (the safety gate blocks any edit to these):\n")
		for _, p := range gateCfg.Protected {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteByte('\n')
	}

	if tree := topLevelDirs(g); tree != "" {
		b.WriteString("Top-level repository structure:\n")
		b.WriteString(tree)
		b.WriteByte('\n')
	}

	return b.String()
}

// resolveFiles returns paths whose names appear as substrings in the
// request. Cheap heuristic; the LLM handles ambiguity. We cap the
// list at 30 to avoid drowning the prompt.
func resolveFiles(request string, g GraphAccess) []string {
	if g == nil {
		return nil
	}
	seen := make(map[string]bool)
	var matched []string

	// Pass 1: symbol-aware resolution. Tokenize the request into
	// identifier-shaped words; for each, look up Symbol nodes whose
	// fqName (or trailing component) matches; expand to the defining
	// file via DEFINES_IN. This is the "fix parseFoo" → "the file
	// where parseFoo lives" path that pure substring matching misses
	// because `parseFoo` rarely appears verbatim in a path.
	//
	// Cheap: scans DEFINES_IN edges (already an indexed slice) and
	// caps total hits to keep prompt size sane.
	for _, p := range resolveSymbolFiles(request, g) {
		if !seen[p] {
			seen[p] = true
			matched = append(matched, p)
		}
	}

	// Pass 2: substring match on file paths. Same as before — picks
	// up "fix the homepage" → files with "homepage" in the path.
	all, err := g.GetNodesByKind(graph.KindFile)
	if err == nil {
		low := strings.ToLower(request)
		for _, n := range all {
			p, _ := n.Payload["path"].(string)
			if p == "" || seen[p] {
				continue
			}
			base := p
			if i := strings.LastIndex(p, "/"); i >= 0 {
				base = p[i+1:]
			}
			if strings.Contains(low, strings.ToLower(base)) || strings.Contains(low, strings.ToLower(p)) {
				seen[p] = true
				matched = append(matched, p)
			}
		}
	}

	sort.Strings(matched)
	if len(matched) > 30 {
		matched = matched[:30]
	}
	return matched
}

// resolveSymbolFiles extracts identifier-shaped tokens from the
// request, looks each up as a Symbol fqName (or trailing component),
// and returns the defining files. Mirrors the kai_grep symbol-mode
// fallback (tools/kai.go:tryGrepSymbol) so the planner gets the same
// graph leverage the agent already has at runtime — but at planning
// time, before any LLM tokens are spent.
//
// Stopwords filtered: very common English words that are
// identifier-shaped but never useful as symbol queries. We don't try
// to be exhaustive; if a stopword survives, it just produces zero
// hits and adds nothing to the prompt.
func resolveSymbolFiles(request string, g GraphAccess) []string {
	if g == nil {
		return nil
	}
	tokens := extractIdentifierTokens(request)
	if len(tokens) == 0 {
		return nil
	}
	edges, err := g.GetEdgesOfType(graph.EdgeDefinesIn)
	if err != nil || len(edges) == 0 {
		return nil
	}
	// Build lookup set. Lowercased so we don't depend on the user's
	// casing — graph fqNames preserve original case, but a planner
	// resolution only needs to match approximately.
	want := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		want[strings.ToLower(t)] = true
	}
	const maxHits = 12
	var out []string
	seen := make(map[string]bool)
	for _, e := range edges {
		if len(out) >= maxHits {
			break
		}
		sym, err := g.GetNode(e.Src)
		if err != nil || sym == nil {
			continue
		}
		fq, _ := sym.Payload["fqName"].(string)
		if fq == "" {
			continue
		}
		// Match either full fqName or trailing piece — same rule as
		// tools/kai.go so "Login" hits both "auth.Login" and "Login".
		if !want[strings.ToLower(fq)] && !want[strings.ToLower(plannerTrailing(fq))] {
			continue
		}
		file, err := g.GetNode(e.Dst)
		if err != nil || file == nil {
			continue
		}
		filePath, _ := file.Payload["path"].(string)
		if filePath == "" || seen[filePath] {
			continue
		}
		seen[filePath] = true
		out = append(out, filePath)
	}
	return out
}

// extractIdentifierTokens tokenizes the request and returns
// identifier-shaped words minus a small stopword set. Identifier
// rules match looksLikeIdentifier in tools/kai.go: 2-80 chars,
// alphanumerics + `_`, `.`, `:`. Splits on whitespace and common
// punctuation that isn't part of an identifier.
func extractIdentifierTokens(request string) []string {
	fields := strings.FieldsFunc(request, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '.', r == ':':
			return false
		default:
			return true
		}
	})
	var out []string
	for _, t := range fields {
		t = strings.Trim(t, ".:")
		if len(t) < 3 || len(t) > 80 {
			continue
		}
		if plannerStopwords[strings.ToLower(t)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// plannerStopwords is the small set of identifier-shaped English
// words that show up in user requests and never make useful symbol
// queries. Not exhaustive — anything missing just costs us one extra
// edge scan that returns no hits.
var plannerStopwords = map[string]bool{
	"the": true, "and": true, "but": true, "for": true, "not": true,
	"you": true, "are": true, "can": true, "all": true, "any": true,
	"add": true, "fix": true, "use": true, "make": true, "new": true,
	"old": true, "this": true, "that": true, "with": true, "from": true,
	"into": true, "onto": true, "when": true, "what": true, "where": true,
	"which": true, "while": true, "have": true, "has": true, "had": true,
	"some": true, "more": true, "less": true, "than": true, "then": true,
	"also": true, "just": true, "only": true, "very": true, "really": true,
	"please": true, "should": true, "would": true, "could": true,
	"broken": true, "fixed": true, "working": true, "doesnt": true,
	"isnt": true, "wasnt": true, "function": true, "variable": true,
	"method": true, "class": true, "type": true, "value": true,
	"file": true, "files": true, "code": true, "test": true, "tests": true,
}

// plannerTrailing returns the last `.` or `::` delimited piece of a
// possibly-qualified symbol name. Local copy of trailingComponent
// from tools/kai.go — duplicated rather than imported because the
// planner package can't depend on tools without a cycle.
func plannerTrailing(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// neighborMap returns, for each path in `paths`, the depth-1
// callers/importers (files that have an inbound IMPORTS or CALLS edge
// to that file). Same primitive the gate uses for blast radius.
func neighborMap(paths []string, g GraphAccess) map[string][]string {
	if g == nil {
		return nil
	}
	out := make(map[string][]string, len(paths))
	for _, p := range paths {
		set := make(map[string]bool)
		for _, et := range []graph.EdgeType{graph.EdgeImports, graph.EdgeCalls} {
			edges, err := g.GetEdgesToByPath(p, et)
			if err != nil {
				continue
			}
			for _, e := range edges {
				node, err := g.GetNode(e.Src)
				if err != nil || node == nil {
					continue
				}
				src, _ := node.Payload["path"].(string)
				if src != "" && src != p {
					set[src] = true
				}
			}
		}
		if len(set) > 0 {
			ns := make([]string, 0, len(set))
			for k := range set {
				ns = append(ns, k)
			}
			sort.Strings(ns)
			// Cap to keep the prompt small.
			if len(ns) > 8 {
				ns = ns[:8]
			}
			out[p] = ns
		}
	}
	return out
}

// topLevelDirs returns a one-line-per-directory summary of the repo
// rooted at the snapshot. Helps the LLM orient when the request
// doesn't name a specific file.
func topLevelDirs(g GraphAccess) string {
	if g == nil {
		return ""
	}
	all, err := g.GetNodesByKind(graph.KindFile)
	if err != nil {
		return ""
	}
	count := make(map[string]int)
	for _, n := range all {
		p, _ := n.Payload["path"].(string)
		if p == "" {
			continue
		}
		i := strings.Index(p, "/")
		root := "."
		if i > 0 {
			root = p[:i]
		}
		count[root]++
	}
	dirs := make([]string, 0, len(count))
	for d := range count {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return count[dirs[i]] > count[dirs[j]] })
	if len(dirs) > 12 {
		dirs = dirs[:12]
	}
	var b strings.Builder
	for _, d := range dirs {
		fmt.Fprintf(&b, "  %s/  (%d files)\n", d, count[d])
	}
	return b.String()
}

// parsePlan extracts a WorkPlan from the LLM's response. We do a
// best-effort cleanup: strip code fences if the model added them
// despite instructions, then unmarshal.
func parsePlan(raw string) (*WorkPlan, error) {
	s := strings.TrimSpace(raw)
	// Strip ```json ... ``` fences if present. The LLM is told not
	// to emit them but defensive parsing is cheap.
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	var plan WorkPlan
	if err := json.Unmarshal([]byte(s), &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// NewAIAdapter wraps a real ai.Client so it satisfies the Completer
// interface. Constructed by callers that want the live API; tests
// pass a fake instead.
func NewAIAdapter(c *ai.Client) Completer {
	return &aiAdapter{c: c}
}

type aiAdapter struct{ c *ai.Client }

func (a *aiAdapter) Complete(system string, messages []ai.Message, maxTokens int) (string, error) {
	if a.c == nil {
		return "", fmt.Errorf("planner: nil ai client")
	}
	return a.c.Complete(system, messages, maxTokens)
}
