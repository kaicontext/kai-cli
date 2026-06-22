# Third-party attribution

The `kai-cli/internal/agent/` package and its subpackages (`tools/`,
`message/`, future `provider/`, `session/`) draw architectural patterns
and selected source from the OpenCode project:

- Project: `opencode-ai/opencode`
- Repository: https://github.com/opencode-ai/opencode
- License: MIT
- Copyright (c) 2025 Kujtim Hoxha

The MIT license terms apply to any files in this package that derive
from OpenCode — currently:

- `tools/tools.go` — derived from `internal/llm/tools/tools.go` (verbatim
  except for package name and removal of session/message context-key
  helpers that pull in OpenCode's session subsystem).
- `message/content.go`, `message/attachment.go` — derived from
  OpenCode's `internal/message/` types, with the persistence-bound
  `message.go` and the `models.ModelID` dependency replaced by a plain
  `string` model identifier.

Files NOT carried over from OpenCode (intentional):

- `internal/llm/agent/agent.go` — Kai writes its own agent loop in
  `runner.go` (Slice 1). The OpenCode loop is tightly coupled to its
  pubsub, session, and permission subsystems, none of which Kai needs.
- `internal/llm/provider/*` — replaced by a Kai-native provider that
  wraps `kai/internal/planner.Completer` (the kailab proxy client).
- `internal/llm/tools/{view,edit,write,bash}.go` — Kai writes Kai-native
  versions in `tools/` so they integrate with kai's safety gate,
  watcher, and live sync without dragging in OpenCode's config / lsp /
  permission / history packages. Architectural patterns (Tool
  interface, parameter schema) are preserved.

The MIT LICENSE text from OpenCode is included verbatim in
`LICENSE-OpenCode.md` next to this file. When new files are derived
from OpenCode, add them to the list above.
