# Spec: Drift Catch-Up, Server Tier

**Status:** Draft
**Components:** kai-server, kai-cli, kai-engine
**Depends on:** [graph-staleness-and-drift.md](graph-staleness-and-drift.md) (steps 1–6, landed via kai-engine#23 / kai-cli#35)

## Problem

The local tier makes every machine pay for its own catch-up: each client that falls behind recomputes the same semantic snapshots for the same commits. On a team, one push means N teammates each burning the same capture work — and machines without Kai installed (CI, a colleague's laptop) produce drift that only heals when someone's daemon or query pays for it.

The server already holds a graph for the repo. It should process each commit exactly once, at push time, and clients should catch up by **fetching** graph state instead of recomputing it. This is the reason the server layer exists for this problem.

## Principles

Inherited from the local spec, plus:

1. The server's graph states are the same checkpointed, commit-pinned, content-addressed records the client produces. A pull is a ref fast-forward plus object fetch — no new state model.
2. A client must never be *worse off* for having a server: if the server is unreachable or behind, the local catch-up path (inline, daemon, `import --since`) proceeds exactly as today.
3. No silent re-computation of findings. A finding computed at an old graph state gets badged, not overwritten; re-review is an explicit action.

## Server-side processing

**Forge webhook on push.** The forge (GitHub/GitLab) webhook delivers the pushed ref and commit range. The server runs the same oldest-first checkpointed catch-up the client runs (`drift` package semantics: snapshot from git ref, `git.<sha>` ref, pin, manifest retirement), against its own clone.

- Same interruption discipline: a webhook worker killed mid-range leaves the server graph at the last completed commit; delivery retries resume from the pin.
- Force-push / rewritten refs follow the local `diverged`/post-rewrite semantics: process the new line, SUPERSEDES-link the old one.
- The server maintains its own `graph_refs` per repo. It is the team's shared answer to "what has been processed."

## Client fast-forward

**`kai pull` grows a graph fast-forward step.** After the existing snapshot/changeset pull:

1. Client sends its `graph_refs` for the current ref.
2. Server responds with its pin for that ref and the chain of checkpointed states between (commit SHA → snapshot ID, plus objects the client lacks).
3. Client fetches objects (existing `fetch` machinery — content-addressed, so shared objects transfer once), writes the `git.<sha>` refs, and advances its local pin to the server's.

Fast-forward only: if the client's pin is not an ancestor of the server's (client processed commits the server hasn't seen — e.g. local-only commits), the client keeps its pin and the local tier covers the gap. No merge of graph lineages in v1.

**Ordering with local catch-up.** When drift is detected and a remote is configured, prefer pull-fast-forward over local recompute when the server's pin covers the drift range; fall back to local catch-up for whatever remains (local-only commits, server lag). The inline budget applies to the whole operation.

## Finding surface

Findings display the graph state they were computed at. When the server knows the ref has advanced past it (webhook or client push), the finding is badged stale with the intersecting-commit count, using the same file-level intersection against the finding's symbols (the finding's files are its neighborhood). No silent re-computation; re-review is an explicit action that produces a new finding superseding the old.

## Out of scope

- Merging divergent graph lineages between client and server (fast-forward only)
- Multi-remote reconciliation
- Server-side inline answers to client queries (clients always answer locally)

## Acceptance criteria

1. A push from a machine without Kai installed results in the server graph reaching the pushed tip within one webhook delivery (retries included).
2. `kai pull` on a client 50 commits behind advances the graph pin to the server's tip without running local capture, in time bounded by object transfer, not by semantic analysis.
3. A client with local-only commits keeps its pin; pull fast-forwards only the ancestor prefix and local catch-up covers the rest.
4. Killing the webhook worker mid-range leaves the server graph at a completed checkpoint; the retry resumes.
5. A finding on the web surface whose ref has advanced shows a stale badge with the intersecting count; the underlying record is unmodified.

## Open questions

- Webhook auth/verification reuses the existing kai review webhook verification (see KAI_WEBHOOK_VERIFY.md) or needs its own secret rotation story.
- Whether the server exposes its manifest so clients can classify `stale-suspect` for ranges they haven't fetched yet (cheap honesty before an expensive pull).
- Retention on the server: per-commit exact-SHA states for a whole org's push history is unbounded; likely the same cap-plus-ref-pins policy as the client, per repo.
