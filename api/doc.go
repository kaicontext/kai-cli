// Package api is the public surface the TUI uses to talk to the
// kai engine. Long-term goal: kai-cli/internal/tui/ imports
// nothing from kai-cli/internal/ except internal/tui/* itself —
// every engine type/function it needs is re-exported (or
// implemented as an interface) here in api/.
//
// Why this exists:
//   1. Architectural enforcement. The CI gate (scripts/
//      check-tui-imports.sh) flags any direct internal/* import
//      from inside the TUI tree. Once we're at zero direct
//      imports, the gate can flip to --strict and prevent new
//      coupling.
//   2. Migration runway. The day we want the TUI to live in its
//      own Go module (kai-tui/) and version independently, the
//      api package becomes the importable surface between kai-cli
//      and kai-tui. Today api is internal to kai-cli; tomorrow
//      it can be hoisted into kai-cli/pkg/api/ or kai-api/.
//
// Migration plan:
//   - Phase 0 (this commit): create the api/ destination + CI
//     gate that measures current coupling. Nothing migrates yet.
//   - Phase 1: migrate one engine package at a time, starting
//     with the high-fanout ones (agent/provider — 11 TUI files
//     import it). Each migration is its own commit so a
//     regression bisects cleanly.
//   - Phase 2: when the gate reports zero direct imports, flip
//     the script default to --strict and add it to CI as a
//     blocking check. Optionally hoist api/ to a module boundary
//     for the kai-tui separation.
//
// See docs/architecture/tui-api-extraction.md for the full plan.
package api
