package planner

import (
	"fmt"
	"strings"
)

// FormatCallChains renders the resolved call chains as the
// structured-text payload that will travel as the body of a
// synthetic context_lookup tool result. The opening paragraph is the
// soft-escape clause from the spec — it tells the agent that the
// injection is a hint, not a constraint, so a false-positive match
// doesn't anchor the model on the wrong area.
//
// Empty input produces empty output (caller skips injection in that
// case). Chains with zero nodes (file-only resolutions) render as a
// single-line entry naming the file; the agent gets a path hint
// without a misleading function chain.
func FormatCallChains(chains []CallChain) string {
	if len(chains) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Pre-resolved entry points (from code-shaped tokens in your message):\n\n")
	b.WriteString("These entry points were resolved against the project's call graph. ")
	b.WriteString("They're a starting hint, not a constraint — if they don't seem related to the actual issue, search normally. ")
	b.WriteString("Tracing forward from the entry point is usually more productive than brainstorming hypotheses.\n\n")

	for i, c := range chains {
		if i > 0 {
			b.WriteByte('\n')
		}
		writeOneChain(&b, c)
	}
	return b.String()
}

// writeOneChain renders one chain: the entry-point header line,
// then each node indented by its depth. NoteOnly nodes get a "—
// stdlib, not expanded" suffix so the agent knows the chain stops
// at that boundary intentionally.
func writeOneChain(b *strings.Builder, c CallChain) {
	header := c.Entry.Token
	if c.Entry.HandlerName != "" && c.Entry.HandlerName != header {
		header = fmt.Sprintf("%s → %s", c.Entry.Token, c.Entry.HandlerName)
	}
	switch c.Entry.Stage {
	case StageCommand:
		header += " (via command index)"
	case StageSymbol:
		header += " (via symbol index)"
	case StageFile:
		header += " (via file index)"
	}
	fmt.Fprintf(b, "Entry: %s\n", header)

	if len(c.Nodes) == 0 {
		// File-only entry — surface the path as the body and stop.
		if c.Entry.FilePath != "" {
			fmt.Fprintf(b, "  file: %s\n", c.Entry.FilePath)
		}
		return
	}

	for _, n := range c.Nodes {
		indent := strings.Repeat("  ", n.Depth+1)
		label := n.FullName
		if label == "" {
			label = n.ShortName
		}
		fmt.Fprintf(b, "%s→ %s", indent, label)
		if n.FilePath != "" {
			fmt.Fprintf(b, " (%s)", n.FilePath)
		}
		if n.NoteOnly {
			b.WriteString(" — stdlib, not expanded")
		}
		b.WriteByte('\n')
	}
	if c.Truncated {
		fmt.Fprintf(b, "  (more callees not shown — chain truncated at the per-request cap)\n")
	}
}

// BuildInjectedContext is the convenience entry point the orchestrator
// (or planner) calls: tokenize → resolve → walk → format. Returns
// empty string when nothing resolves, which the caller treats as
// "skip injection." Workspace-bound parts (the command index, the
// graph) are passed in by the caller so this function stays pure
// w.r.t. external state.
func BuildInjectedContext(request string, g GraphAccess, cmds *CommandIndex) string {
	tokens := ExtractEntryPointTokens(request)
	if len(tokens) == 0 {
		return ""
	}
	entries := ResolveEntryPoints(tokens, g, cmds)
	if len(entries) == 0 {
		return ""
	}
	chains := WalkCallChains(entries, g)
	return FormatCallChains(chains)
}
