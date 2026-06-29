# Contributing to Kai

Thanks for your interest in contributing to Kai. This guide covers what we accept, how to set up a development environment, and how to submit changes.

Have questions? Join us on [Slack](https://join.slack.com/t/kailayer/shared_invite/zt-3q8ulczwl-vkZ05GQH~kwudonmH53hGg).

## Scope

The Kai CLI is open source under Apache 2.0. This repo (`kaicontext/kai-cli`) contains the CLI. The agent engine ships as two prebuilt private module dependencies (`kai-core` primitives and `kai-engine` agent loop/tools/providers), and the server components live in their own repos. The single `kai` binary you download statically links the CLI and the engine together. The dependency boundary between this repo and the engine modules is checked automatically — see [Engine module boundary](#engine-module-boundary).

### What we accept

- Bug fixes
- Performance improvements
- Determinism improvements
- Documentation improvements
- Additional language support (parsers, symbol extraction, call graphs)
- CI integration enhancements
- Test coverage improvements
- CLI UX improvements

Server and cloud contributions (authentication, multi-tenancy, hosting, RBAC, SSO, etc.) are welcome in the [kai-server](https://github.com/kaicontext/kai-server) repo.

### What we will not accept in this repo

- Changes that introduce network dependencies into kai-core
- Breaking changes without prior discussion in an issue
- License changes

These boundaries protect the architectural separation between the core engine and server.

## Pull Request Process

### Review requirements

- All PRs require at least one maintainer approval
- PRs must pass CI before merging
- PRs must include tests if behavior changes
- PRs must preserve deterministic behavior
- PRs must not introduce prohibited dependencies (no `net/http` or cloud SDKs in kai-core)

### Before you start

- Check existing issues to avoid duplicating work
- For large changes, open an issue first to discuss the approach
- Keep PRs focused — one logical change per PR

### Response times

- Initial review within 5 business days
- Major design discussions may take longer
- If your PR has been waiting, ping us on Slack or in the PR

### Decision authority

Maintainers reserve the right to decline contributions that conflict with project direction, architectural boundaries, or correctness guarantees. We'll always explain why.

## Determinism Requirements

Kai's core promise is deterministic, reproducible results. Any change that affects the following must include regression tests:

- Graph construction or hashing
- Snapshot content or ordering
- CI plan output or test selection
- ChangeSet computation

Run the regression suite before submitting:

```bash
CGO_ENABLED=1 go test ./cmd/kai/ \
  -run "TestGraph_|TestSelection_|TestFalseNeg_|TestShadow_|TestFlaky_|TestCLI_|TestPerf_" \
  -v -count=1
```

If your change produces different output for the same input, it must be discussed in an issue first.

## Architectural Boundary Rules

The core engine (`kai-core`) is kept "pure": no `net/http`, no cloud SDKs, no
cloud provider URLs, and no server concepts (`tenant`, `org_id`, `sso`,
`billing`). These rules are enforced by `scripts/check-core-purity.sh` in the
`kai-core` repo's CI. CLI contributions in this repo do not need to run it.

### Engine module boundary

The agent engine ships as two versioned module dependencies, `kai-core` and
`kai-engine`, which `kai-cli` *imports* rather than *contains* — the same way
you'd depend on any external module. Keeping that boundary clean avoids source
duplication (which silently diverges from the real module) and keeps the build
reproducible for everyone:

- **Engine code lives in the engine module.** Import it via `go.mod`; don't
  copy engine source (the agent loop, planner, providers, the agent system
  prompt, gate logic, etc.) into this repo. The thin `api/*` packages that
  re-export engine types are wrappers — they `import` the engine, they don't
  duplicate it.
- **Keep `replace` directives in `go.work`, not `go.mod`.** Use a gitignored
  `go.work` for local side-by-side development (see Build above); a committed
  `replace` resolves only against a local checkout and breaks the build for
  others.
- **Don't copy engine-owned content** (e.g. the agent system prompt text) into
  committed source.

The check in [`internal/boundary`](internal/boundary/boundary.go) backstops
these rules: a dependency-free Go test that scans the tree and `go.mod`, run
locally and in CI (the secret-less `boundary` job, so it also covers fork-based
PRs). It **deterministically** catches the high-value cases — a vendored engine
module (by directory name or its `go.mod`), a `replace` for either module, or a
dropped `require` — and **best-effort** flags engine-owned content (the agent
system prompt, gate logic) via a sentinel denylist. The sentinel layer is
defense-in-depth, not a guarantee: a copy of engine source under an unrelated
name with no known sentinel won't be caught, so reviewers are still the
backstop there. To run it yourself:

```bash
go test ./internal/boundary/...
```

If you add a new engine-owned asset, extend the sentinel list in
`boundary.go`.

## Development Setup

### Prerequisites

- Go 1.24+
- GCC or Clang (for CGO — tree-sitter and SQLite)
- Git

### Build

This repo's module (`kai`) depends on two **private** modules:
`github.com/kaicontext/kai-core` (primitives) and
`github.com/kaicontext/kai-engine` (agent loop, planner, tools, providers,
and the agent system prompt). CI resolves them as private modules (via
`GOPRIVATE` + a short-lived read token). For local development with the repos
checked out side by side, use a **gitignored `go.work`** that points the CLI
at your local engine sources:

```bash
# from the kai-cli checkout, with kai-core and kai-engine as sibling dirs
cat > go.work <<'EOF'
go 1.25.0

use .

replace github.com/kaicontext/kai-core => ../kai-core
replace github.com/kaicontext/kai-engine => ../kai-engine
EOF

CGO_ENABLED=1 go build ./cmd/kai
```

> **Keep these `replace` directives in `go.work`, never in `go.mod`.**
> `go.work` is gitignored; `go.mod` is committed. A `replace => ../kai-engine`
> in `go.mod` resolves only against a local checkout, so it breaks the build
> for everyone else. The module-boundary check (below) fails CI if one is ever
> committed.

### Run Tests

```bash
CGO_ENABLED=1 go test ./...
```

## Project Structure

```
cmd/kai/           CLI entrypoint
internal/          CLI implementation (commands, CI plan, shadow mode, TUI)
api/               Public API surface
packages/kai-mcp/  npm wrapper that fetches the released binary
bench/             Benchmark harness
docs/              Reference docs
site/              Homepage (served at get.kaicontext.com)
install.sh         curl installer
```

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- No unnecessary abstractions — simpler is better
- Tests go next to the code they test (`*_test.go`)
- Don't add comments that restate the code

### Commit messages

Write clear, concise commit messages:

```
Fix barrel import re-export handling in extractImports

export { x } from './y' was not producing IMPORTS edges because
extractImports only handled import_statement and call_expression.
Added export_statement case with parseReexportSource helper.
```

- First line: imperative mood, under 72 characters
- Body: explain *why*, not just *what*

## Developer Certificate of Origin (DCO)

All contributions must be signed off under the [Developer Certificate of Origin](https://developercertificate.org/). This certifies that you have the right to submit the code and that it can be distributed under the Apache 2.0 license.

Add a `Signed-off-by` line to your commits:

```bash
git commit -s -m "Fix barrel import re-export handling"
```

This adds a line like:

```
Signed-off-by: Your Name <your.email@example.com>
```

You can configure Git to do this automatically:

```bash
git config --global format.signoff true
```

PRs without DCO sign-off will not be merged.

## Copyright Headers

Source files should include an SPDX copyright header:

```bash
./scripts/check-copyright-headers.sh          # Check
./scripts/check-copyright-headers.sh --fix     # Auto-add missing headers
```

## Submitting Changes

1. Fork the repository
2. Create a branch from `main`
3. Make your changes with tests
4. Sign off your commits (`git commit -s`)
5. Ensure tests pass: `CGO_ENABLED=1 go test ./...`
6. Open a PR against `main`

## Security Reporting

Vulnerabilities should not be submitted as public issues. See [SECURITY.md](SECURITY.md) for responsible disclosure instructions.

## Reporting Issues

- Use the [bug report template](.github/ISSUE_TEMPLATE/bug_report.yml) for bugs
- Use the [feature request template](.github/ISSUE_TEMPLATE/feature_request.yml) for ideas
- Include reproduction steps, expected vs actual behavior, and environment details

## Questions

Open a [discussion](https://github.com/kaicontext/kai-cli/discussions) or join [Slack](https://join.slack.com/t/kailayer/shared_invite/zt-3q8ulczwl-vkZ05GQH~kwudonmH53hGg).
