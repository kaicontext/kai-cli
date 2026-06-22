# kai-mcp

MCP server for [Kai](https://kaicontext.com) — semantic code intelligence for AI coding assistants.

## Quick Install

```bash
claude mcp add kai -- npx -y kai-mcp
```

## What It Does

Gives your AI coding assistant (Claude Code, Cursor, etc.) access to Kai's semantic graph — call graphs, dependency maps, impact analysis, and test coverage — via the [Model Context Protocol](https://modelcontextprotocol.io).

## Setup

### Claude Code

```bash
claude mcp add kai -- npx -y kai-mcp
```

### Cursor

Add to `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "kai": {
      "command": "npx",
      "args": ["-y", "kai-mcp"]
    }
  }
}
```

### Generic stdio

```bash
npx -y kai-mcp
```

## Tools

| Tool | Description |
|------|-------------|
| `kai_symbols` | List symbols in a file (functions, classes, methods) |
| `kai_callers` | Find all callers of a symbol |
| `kai_callees` | Find all symbols called by a symbol |
| `kai_dependents` | Find files that depend on a file |
| `kai_dependencies` | Find files a file depends on |
| `kai_tests` | Find tests covering a file |
| `kai_diff` | Semantic diff between two refs |
| `kai_context` | Bundled context for a file/symbol |
| `kai_impact` | Transitive downstream impact analysis |
| `kai_files` | List files in the repo with language/module filters |
| `kai_status` | Check graph freshness |
| `kai_refresh` | Re-capture the semantic graph |

No setup required — the server lazily initializes the Kai semantic graph on first use.

## Links

- [Full docs](https://github.com/kaicontext/kai-cli/blob/main/docs/mcp.md)
- [Kai](https://kaicontext.com)
- [GitHub](https://github.com/kaicontext/kai-cli)
