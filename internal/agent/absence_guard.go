package agent

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/kaicontext/kai-engine/message"
)

// absenceGuardNudge is the user-role message injected into the
// transcript when the guard fires. Phrased so the model treats it as
// a coaching note about its own answer, not a contradicting claim
// from the user. Lists the three search axes the agent should cover
// before re-answering.
const absenceGuardNudge = `You concluded that something doesn't exist in the codebase, but you only ran a small number of searches relevant to that claim — and the plan's wording is the hypothesis, not the truth. The implementation may use different names than you searched for.

Before concluding "not found," do at least three additional relevant searches covering:
(a) name variants — different casings, snake_case vs camelCase, abbreviated forms (e.g. "extractMutatedPaths" if you searched for "invalidateCache"), and adjacent verbs ("drop", "invalidate", "evict", "purge").
(b) the surrounding behavior — call sites of the tool or hook that would use it, dispatch tables, switch statements, the handler for the relevant event.
(c) tests that describe the behavior — a test named for the feature is strong evidence the feature exists even if its implementation is named oddly.

Only conclude "not found" if all three return empty. Otherwise revise your answer.`

// assistantFinalText extracts the assistant's user-facing text from
// the parts of a finished response. Used by the absence guard, which
// must operate on the model's actual final answer — not on tool-call
// arguments or reasoning content. Mirrors message.Message.Text() but
// works on a Parts slice directly.
func assistantFinalText(parts []message.ContentPart) string {
	var out strings.Builder
	for _, p := range parts {
		if t, ok := p.(message.TextContent); ok {
			out.WriteString(t.Text)
		}
	}
	return out.String()
}

// The absence guard prevents an agent from declaring "X doesn't exist
// in the codebase" after a single failed search. Real failure mode: a
// research agent greps for the plan's literal vocabulary (e.g.
// "invalidateCache"), gets zero hits, and concludes the feature is
// missing — when the implementation actually lives under a different
// name ("extractMutatedPaths"). The guard intercepts the agent's final
// message, checks whether the negative claim is backed by at least 3
// relevance-matched search calls, and if not, sends the agent back to
// try again. This file defines the three pure-function primitives the
// runner-level integration (a separate PR) will call.

// negativePhrases matches phrasings an agent uses to declare absence.
// Designed to over-trigger on the final answer rather than miss real
// claims: false positives cost one extra turn, false negatives let
// the bug we're trying to fix slip through. Patterns are matched
// case-insensitively against the trimmed final assistant text — NOT
// against intermediate reasoning, which often contains hedged forms
// like "let me check whether X exists" that aren't real conclusions.
var negativePhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bnot found\b`),
	regexp.MustCompile(`(?i)\bno .{0,40}\bfound\b`),
	regexp.MustCompile(`(?i)\bfound no\b`),
	regexp.MustCompile(`(?i)\bfound (?:nothing|none|zero)\b`),
	regexp.MustCompile(`(?i)\bdoesn['’]t exist\b`),
	regexp.MustCompile(`(?i)\bdoes not exist\b`),
	regexp.MustCompile(`(?i)\bnot implemented\b`),
	regexp.MustCompile(`(?i)\bnot present\b`),
	regexp.MustCompile(`(?i)\bno evidence\b`),
	regexp.MustCompile(`(?i)\bcouldn['’]t find\b`),
	regexp.MustCompile(`(?i)\bcould not find\b`),
	regexp.MustCompile(`(?i)\bcan['’]t find\b`),
	regexp.MustCompile(`(?i)\bcannot find\b`),
	regexp.MustCompile(`(?i)\bunable to find\b`),
	regexp.MustCompile(`(?i)\bnothing matches\b`),
	regexp.MustCompile(`(?i)\bappears to be (?:absent|missing)\b`),
	regexp.MustCompile(`(?i)\bis (?:absent|missing)\b`),
	regexp.MustCompile(`(?i)\bno such (?:function|method|symbol|file|type)\b`),
	// Dismissal-class phrasings: not strictly "X is absent" but
	// functionally equivalent — the agent has concluded there's
	// nothing to do here, often after a shallow look. The kai-code
	// audit on 2026-05-11 closed a real bug with "already implemented
	// (no bug)"; without these patterns the guard wouldn't catch
	// that failure mode.
	regexp.MustCompile(`(?i)\balready implemented\b`),
	regexp.MustCompile(`(?i)\balready (?:done|exists|fixed|handled)\b`),
	regexp.MustCompile(`(?i)\bno bug\b`),
	regexp.MustCompile(`(?i)\bnot a bug\b`),
	regexp.MustCompile(`(?i)\bby design\b`),
	regexp.MustCompile(`(?i)\bworks as (?:intended|designed|expected)\b`),
	regexp.MustCompile(`(?i)\bworking as (?:intended|designed|expected)\b`),
	regexp.MustCompile(`(?i)\bno (?:code )?changes? (?:are |is )?needed\b`),
	regexp.MustCompile(`(?i)\bnothing to (?:do|fix|change)\b`),
	regexp.MustCompile(`(?i)\bnothing missing\b`),
}

// IsNegativeClaim reports whether the text reads as an "X doesn't
// exist" conclusion. Operates on the assistant's final answer text
// (the runner already separates final from intermediate). Empty or
// whitespace-only text is not a claim.
func IsNegativeClaim(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, re := range negativePhrases {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// SearchCall is one search-shaped tool invocation extracted from a
// transcript. Query is best-effort extracted from the tool's JSON
// input — empty when the tool's schema doesn't expose a query field
// (e.g. kai_callers takes a symbol id), in which case the caller
// falls back to relevance via the tool's other input fields.
type SearchCall struct {
	Tool  string // "kai_grep", "kai_symbols", "kai_callers", "bash", etc.
	Query string // extracted from input JSON, may be empty
}

// searchTools is the set of tool names that count as "doing a search"
// for the purposes of the absence guard. Read-only lookups only —
// view/read tools count as searches because an agent that has
// directly opened a file has stronger evidence than one that
// greppedfor a name and missed.
var searchTools = map[string]bool{
	"kai_grep":       true,
	"kai_symbols":    true,
	"kai_callers":    true,
	"kai_dependents": true,
	"kai_context":    true,
	"kai_files":      true,
	"kai_tree":       true,
	"kai_search":     true,
	"view":           true,
	"read":           true,
}

// SearchCalls pulls every search-shaped tool invocation out of the
// transcript. The runner passes the full message slice; this function
// walks tool-use parts and emits one SearchCall per matched tool. The
// Query field is best-effort: we try common JSON field names
// (`query`, `pattern`, `symbol`, `path`, `file_path`, `glob`) and
// stringify whichever is present. Unknown shapes fall through with
// Query left empty — Relevance can still match on the file argument.
//
// Also emits synthetic SearchCalls for entry points injected by the
// graph-powered context_lookup mechanism. Each "Entry: <token>" line
// in the context_lookup tool result becomes one search credit
// because the planner already did the lookup work the absence guard
// would otherwise demand from the agent. Without this, an agent
// that read the injected chain and then concluded "X doesn't exist"
// would get nudged to re-search what it was already given.
func SearchCalls(msgs []message.Message) []SearchCall {
	var out []SearchCall
	for _, m := range msgs {
		for _, p := range m.Parts {
			if tc, ok := p.(message.ToolCall); ok {
				tool := tc.Name
				if tool == "bash" {
					if q, ok := extractBashSearchQuery(tc.Input); ok {
						out = append(out, SearchCall{Tool: "bash", Query: q})
					}
					continue
				}
				if !searchTools[tool] {
					continue
				}
				out = append(out, SearchCall{
					Tool:  tool,
					Query: extractSearchQuery(tc.Input),
				})
				continue
			}
			if tr, ok := p.(message.ToolResult); ok && tr.Name == contextLookupToolName {
				// Each "Entry: <token>" line in the injection body
				// represents a pre-resolved lookup the planner did
				// before the agent started. Credit each as one
				// synthetic search.
				for _, q := range extractInjectedEntryQueries(tr.Content) {
					out = append(out, SearchCall{
						Tool:  contextLookupToolName,
						Query: q,
					})
				}
			}
		}
	}
	return out
}

// queryFields is the priority order for pulling a query string out of
// a tool's input JSON. First non-empty hit wins. Order matters: we
// want the most semantically interesting field first (an explicit
// search query beats a path argument).
var queryFields = []string{
	"query",
	"pattern",
	"symbol",
	"name",
	"q",
	"file_path",
	"path",
	"glob",
}

func extractSearchQuery(inputJSON string) string {
	if inputJSON == "" {
		return ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &raw); err != nil {
		return ""
	}
	for _, k := range queryFields {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// bashGrepLike matches bash commands that are doing search-shaped
// work — grep / rg / ag / ack / find — so the guard credits an agent
// that searched via bash rather than via kai_grep.
var bashGrepLike = regexp.MustCompile(`^\s*(?:grep|rg|ag|ack|find)\b`)

// bashCommandQuery pulls the first quoted or first non-flag token
// after the command name. Cheap heuristic; misses some shapes but
// covers the common cases.
var bashCommandQuery = regexp.MustCompile(
	`^\s*(?:grep|rg|ag|ack|find)(?:\s+-{1,2}\S+)*\s+(?:"([^"]+)"|'([^']+)'|(\S+))`,
)

func extractBashSearchQuery(inputJSON string) (string, bool) {
	if inputJSON == "" {
		return "", false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &raw); err != nil {
		return "", false
	}
	cmd, _ := raw["command"].(string)
	if !bashGrepLike.MatchString(cmd) {
		return "", false
	}
	m := bashCommandQuery.FindStringSubmatch(cmd)
	if m == nil {
		// Search-shaped command but no parseable query — still counts.
		return "", true
	}
	for _, g := range m[1:] {
		if g != "" {
			return g, true
		}
	}
	return "", true
}

// entryLineRE matches the "Entry: <token>[ → <handler>] (...)" lines
// emitted by FormatCallChains in the planner package. The capture
// group is the user-visible token — for command-stage entries that's
// the full backticked phrase (e.g. "kai code"); for symbol/file
// stages it's the token itself. Each match becomes one synthetic
// search credited to the agent.
var entryLineRE = regexp.MustCompile(`(?m)^Entry:\s+([^\n(→]+?)(?:\s+→|\s+\(|$)`)

// extractInjectedEntryQueries parses the body of a context_lookup
// tool result and returns one query per resolved entry point. Order
// in the result matches order in the injection body; the absence
// guard's relevance counter doesn't care about order, but tests do
// (and order-preserving is the more defensible default).
func extractInjectedEntryQueries(body string) []string {
	if body == "" {
		return nil
	}
	matches := entryLineRE.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		q := strings.TrimSpace(m[1])
		if q != "" {
			out = append(out, q)
		}
	}
	return out
}

// stopwords is a small fixed list to avoid false-positive token
// matches on filler words. Kept deliberately short — better to over-
// credit a search than to silently let a "not found" through because
// the only shared token was filtered out.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "to": true, "of": true,
	"in": true, "on": true, "for": true, "and": true, "or": true,
	"but": true, "not": true, "no": true, "this": true, "that": true,
	"it": true, "its": true, "as": true, "at": true, "by": true,
	"with": true, "from": true, "into": true, "any": true, "all": true,
	"some": true, "such": true, "than": true, "then": true, "so": true,
	"do": true, "does": true, "did": true, "done": true, "have": true,
	"has": true, "had": true, "i": true, "we": true, "you": true,
	"function": true, "method": true, "code": true, "file": true,
	"codebase": true, "exists": true, "exist": true, "found": true,
	"find": true, "search": true, "searched": true, "searching": true,
}

// tokenRE picks out identifier-shaped tokens: letters, digits,
// underscores, dots, slashes, hyphens. Anything else is a delimiter.
// This intentionally splits "kai_grep" and "kai.grep" the same way
// they'd appear in code so symbol-name overlap works.
var tokenRE = regexp.MustCompile(`[A-Za-z0-9_./\-]+`)

// tokenize extracts comparable tokens from arbitrary text. Lowercases
// for case-insensitive matching, drops stopwords and tokens shorter
// than 3 chars (which are too noisy — `a`, `if`, `id` show up
// everywhere). Splits camelCase and snake_case at boundaries so
// `extractMutatedPaths` matches both `extract` and `mutated`.
func tokenize(text string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range tokenRE.FindAllString(text, -1) {
		for _, t := range splitCamelSnake(raw) {
			t = strings.ToLower(t)
			if len(t) < 3 || stopwords[t] {
				continue
			}
			out[t] = true
		}
	}
	return out
}

// splitCamelSnake returns the token itself plus its sub-tokens after
// splitting on `_`, `-`, `.`, `/`, and camelCase boundaries. Keeping
// the original token in the set is important: when the agent claims
// "extractMutatedPaths not found", a search whose query is the exact
// string `extractMutatedPaths` should match by identity, not only via
// its sub-tokens.
func splitCamelSnake(s string) []string {
	out := []string{s}
	// snake / kebab / dot / slash splits
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/'
	})
	for _, p := range parts {
		out = append(out, p)
		// camelCase: split before each uppercase that follows a lower.
		var sub []rune
		for i, r := range p {
			if i > 0 && r >= 'A' && r <= 'Z' && p[i-1] >= 'a' && p[i-1] <= 'z' {
				out = append(out, string(sub))
				sub = sub[:0]
			}
			sub = append(sub, r)
		}
		if len(sub) > 0 {
			out = append(out, string(sub))
		}
	}
	return out
}

// RelevantSearches counts how many of the supplied search calls share
// at least one non-stopword token with the claim text. Symmetric
// tokenization: same rules applied to both sides so a query of
// `kai_grep` matches a claim about `kai_grep` regardless of which
// side wrote the underscore. Calls with empty Query are credited
// only when the tool itself implies relevance (kai_callers without a
// symbol field is rare — when it happens we leave it uncounted
// rather than guess).
func RelevantSearches(claim string, calls []SearchCall) int {
	claimTokens := tokenize(claim)
	if len(claimTokens) == 0 {
		return 0
	}
	n := 0
	for _, c := range calls {
		if c.Query == "" {
			continue
		}
		qTokens := tokenize(c.Query)
		for t := range qTokens {
			if claimTokens[t] {
				n++
				break
			}
		}
	}
	return n
}
