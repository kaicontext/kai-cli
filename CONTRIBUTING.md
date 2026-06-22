# Contributing to Kai

Thanks for your interest in contributing to Kai. This guide covers what we accept, how to set up a development environment, and how to submit changes.

Have questions? Join us on [Slack](https://join.slack.com/t/kailayer/shared_invite/zt-3q8ulczwl-vkZ05GQH~kwudonmH53hGg).

## Scope

The Kai CLI is open source under Apache 2.0. This repo (`kaicontext/kai-cli`) contains the CLI. The core engine (`kai-core`) ships as a prebuilt module dependency, and the server components live in their own repos. The single `kai` binary you download statically links the CLI and the core engine together.

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

## Development Setup

### Prerequisites

- Go 1.24+
- GCC or Clang (for CGO — tree-sitter and SQLite)
- Git

### Build

This repo's module (`kai`) depends on `github.com/kaicontext/kai-core`. CI
resolves it as a private module (via `GOPRIVATE` + a read token). For local
development with a checkout of both repos side by side, use a gitignored
`go.work` that points the CLI at your local core:

```bash
# from the kai-cli checkout, with kai-core checked out as a sibling dir
cat > go.work <<'EOF'
go 1.24.2

use .

replace github.com/kaicontext/kai-core => ../kai-core
EOF

CGO_ENABLED=1 go build ./cmd/kai
```

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
