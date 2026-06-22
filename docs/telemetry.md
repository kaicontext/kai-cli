# Telemetry

Kai can collect anonymous usage telemetry to help understand command performance and feature usage. Telemetry is **off by default** and must be explicitly enabled.

## What's collected

- Command name (e.g., `capture`, `diff`, `push`)
- Duration and phase timings (milliseconds)
- Aggregate stats (file count, module count) — no file names or paths
- OS and architecture (`darwin/arm64`, `linux/amd64`, etc.)
- CLI version
- A random install ID (UUID, not tied to any account)

## What's NOT collected

- File paths, filenames, or directory names
- Source code or diffs
- Usernames, emails, or account information
- Repository names or remote URLs
- Environment variables (beyond CI detection)
- Error messages or stack traces

## Enable / Disable

```bash
kai telemetry enable     # Turn on telemetry
kai telemetry disable    # Turn off telemetry
kai telemetry status     # Show current state
```

## Environment variable

`KAI_TELEMETRY` overrides the config file:

| Value | Effect |
|-------|--------|
| `KAI_TELEMETRY=0` | Hard kill switch — disables telemetry regardless of config |
| `KAI_TELEMETRY=1` | Enables telemetry regardless of config |
| (unset) | Falls back to config file |

## CI behavior

When `CI=true` is set (as in most CI environments), telemetry is automatically disabled unless `KAI_TELEMETRY=1` is explicitly set.

## Data storage

- Config: `~/.kai/telemetry.json`
- Event spool: `~/.kai/telemetry.jsonl` (append-only, capped at 1 MB)

Events are spooled locally and uploaded in a gzip-compressed batch at most once every 24 hours, piggybacked on the next CLI invocation. No background processes are used.

## Upload endpoint

`POST https://kaicontext.com/v1/telemetry/batch` (gzip-compressed JSON array).
