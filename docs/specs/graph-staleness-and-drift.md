# Spec: Graph Staleness and Git Drift Semantics

**Status:** Implemented (steps 1–6; step 7 is a separate spec: [drift-server-tier.md](drift-server-tier.md))
**Components:** kai-core, kai-engine, kai-cli, kai-server
**Landed via:** kai-engine#23 (tag v0.5.0), kai-cli#35 — 2026-07-11
**Surfaces touched:** `kai shadow`, `kai bridge`, `kai hook`, `kai doctor`, `kai status`, `kai query`, `kai import`, `kai watch`, `kai prime`, `kai blame`, `kai test affected`

## Problem

Git moves without Kai's permission. Pushes from machines without Kai installed, CI commits, web-editor edits, rebases, branch switches. The semantic graph can always diverge from HEAD. Drift is not preventable. This spec therefore defines the semantics of a drifted graph, not a mechanism to prevent drift.

Failure mode to eliminate: a blast-radius or impact query returning a confidently wrong answer computed against a graph that doesn't match the working tree's Git state, with no indication of the mismatch. Stale-and-silent is worse than an error.

## Principles

1. Kai always knows exactly how drifted it is. Drift is a first-class, queryable property, not an error state.
2. Reads never block unboundedly on catch-up. Inline catch-up is bounded by a time budget; beyond it, reads degrade honestly.
3. Drift only invalidates a query if unprocessed changes intersect the queried subgraph. Compute the intersection cheaply and conservatively: false `stale-suspect` is acceptable, false `stale-valid` is not.
4. Catch-up is incremental, checkpointed, interruptible. Never a half-applied graph. Same append-and-supersede discipline as verdict records.
5. Never couple Kai into Git's write path. No pre-push blocking, no transactional commit+capture. Git won by tolerating state divergence; Kai absorbs that property.

## Drift model

Drift is a signed relationship between graph state and Git state, not a scalar behind-count.

**Per-ref pinning.** Graph states are keyed by commit SHA. `.kai/graph-refs.json` records a map of ref → last-processed SHA (`graph_refs`), plus a bounded set of individually processed commits (cap 500, pruned oldest-first; ref pins are never pruned — this is the retention policy: older states stay reachable through ref pins and ancestry, they just lose exact-SHA resolution). The effective graph state for a query is resolved through the current Git HEAD, in order:

1. the current symbolic ref's pin (`graph_refs[ref]`),
2. exact-SHA match (detached checkout of a processed commit),
3. nearest processed ancestor (an unpinned new branch resolves to its fork point and reports `graph-behind`, never `unpinned` or `diverged`),
4. the pin whose merge-base is closest to HEAD (divergence).

Branch switching (`kai ws`, worktrees, plain `git checkout`) is a resolution step, not a drift event.

**Relationship classes**, computed from `merge-base(graph_state, git_head)`:

| Relationship | Condition | Meaning |
|---|---|---|
| `synced` | graph_state = git_head | No drift |
| `graph-behind` | graph_state is ancestor of git_head | Normal drift; unprocessed commits ahead |
| `graph-ahead` | git_head is ancestor of graph_state | Checkout of an older commit; graph contains the working tree's future |
| `diverged` | Neither is ancestor of the other | Rebase/amend/branch hop; drift measured from merge-base in both directions |
| `orphaned` | No merge-base (history rewrite) | Reconcile by re-import from the current line |
| `unpinned` | No pin recorded yet | Staleness unmeasurable until first capture/import |

`graph-ahead` and `diverged` are never reported as `synced`. Pinned SHAs come from git — ancestry always asks git (`merge-base`, `rev-list`); snapshots deliberately carry no ancestry of their own (F-14).

**Drift set.** For `graph-behind` (and each leg of `diverged`):

```
drift = commits in merge-base(graph_state, git_head)..git_head
```

## Intersection

**Granularity: file-level in v1.** Computed from `git diff-tree -r -M --name-status` per drift commit (rename-aware; a rename maps the old path's symbols — where the stale graph still knows them — to the intersection check). Hunk-level intersection is a deferred refinement.

**Intersecting** = drift commits whose changed files contain symbols in the query's neighborhood, plus the conservative new-edge rule.

**Conservative new-edge rule.** The neighborhood is computed from the stale graph, which cannot see new inbound edges. A drift commit therefore also intersects if it:

- adds an indexed-language file in the same package/directory as any neighborhood symbol (Go same-package references need no import statement; this is also the fallback for languages whose imports can't be cheaply resolved), or
- adds imports (in new files, or newly added to modified files) that resolve into any directory containing neighborhood symbols. Go imports resolve through the go.mod module path; JS/TS relative imports resolve against the importing file's directory. External/unresolvable specifiers resolve to nothing — the same-package rule is their fallback signal.

New files failing every test (docs, config, unrelated modules) do not intersect. This trades some false `stale-suspect`s for eliminating the false-negative hole; per principle 3, pay it.

**Drift manifest.** The per-commit changed-file record (with added-file dirs and import targets) is cached in `.kai/drift-manifest.json`, keyed to (graph_state, git_head). Entries depend only on the commit itself, so they're reused across rebuilds (a rebase mid-drift rebuilds cheaply) and retired per commit as catch-up advances. Diverged states record both legs — the unprocessed commits and the graph's superseded phantom commits; both intersect queries. Commits too large to analyze are flagged `truncated` and conservatively intersect everything — an honest flag, never a silent cap. Per-query intersection is a set operation against the manifest, never git archaeology.

## Query result classes

| Class | Condition | Behavior |
|---|---|---|
| `fresh` | relationship = synced | Answer as-is, silent |
| `stale-valid` | drift > 0, intersecting = 0 | Answer as-is, one-line annotation |
| `stale-suspect` | intersecting > 0 | Answer, annotated with intersecting commits |
| `stale-refused` | intersecting ≥ threshold, or relationship = orphaned | Refuse with explicit reason before printing the answer |

An `unpinned` graph yields **no** staleness block: staleness is unmeasurable there, and a fabricated class would be dishonest. `orphaned` always refuses — with no shared history there is no honest annotation.

Wired into `kai query callers|dependents|impact`, `kai test affected`, `kai blame` (JSON output embeds the block), and `kai prime` (a `## Staleness` section rides in injected context, outside the char budget so truncation can never drop the trust signal). The neighborhood is every file in the answer plus the query target.

Exit codes: `stale-suspect` exits 0 by default. `--strict` (or `kai config` `staleness.strict`) makes it exit **75** (the CI tripwire convention). Refusal threshold: `staleness.refuse_after_intersecting` (default 0 = never refuse, annotate only; refusal is a CI opt-in). Annotation goes to stderr (stdout stays parseable) and `--quiet` suppresses it.

## Detection and reporting

**`kai status`** — drift section: relationship, graph state, git HEAD, drift count (both legs if diverged), oldest unprocessed commit age, action hint. `--json` carries the full `drift` block. Rev-list/merge-base only — no semantic work on the status path.

**`kai shadow drift`** (no flags) — the detailed view: relationship, per-commit list with files touched and import targets, the superseded leg on divergence, manifest state. Explicit `--snap`/`--git-ref` keep the legacy snapshot-content comparison.

**`kai doctor`** — relationship with hint (orphaned is an error), `graph-refs.json` readability, manifest consistency with the current (graph_state, git_head). `--fix` rebuilds a stale/corrupt manifest — it is derived cache; graph_refs pins are assertions and are never auto-modified.

## Catch-up

**Incremental processing.** Drift commits are processed oldest-first, one checkpoint per commit: semantic snapshot built from the git ref (the working tree is never touched), `git.<sha>` ref, graph_refs pin, manifest retirement. The pin is written only after the snapshot lands, and snapshots are content-addressed, so interruption at any point — budget, ctrl-C, SIGKILL — leaves the graph at the last completed commit and a resume re-does at most one commit's work.

**Snapshot isolation.** Queries resolve a snapshot ID up front and read immutable content-addressed state; concurrent catch-up appends new snapshots and moves refs without disturbing in-flight readers.

**Inline catch-up (bounded).** On query, drift is caught up inline under `staleness.inline_budget_ms` (default 2000; explicit 0 disables). A time budget, not a commit count — commit cost varies too much for a count to be a meaningful knob. A commit is started only while inside budget; whatever remains past it is classified honestly. Diverged graphs are never caught up inline — a history rewrite deserves an explicit action.

**`kai watch`** subsumes continuous catch-up: fsnotify on `.git`'s HEAD/ORIG_HEAD/packed-refs/refs (index and object churn excluded — staging is not drift), debounced single-flight convergence passes, and a 30s poll backstop for missed events. Behind/diverged → unbounded checkpointed catch-up; ref switch → manifest resync. With the daemon live, queries stop paying the inline cost.

**Hooks (`kai hook`).** post-commit, post-merge, post-checkout, and post-rewrite hooks trigger capture and pinning. post-rewrite keeps a rebase/amend on the new line instead of sitting diverged on orphaned SHAs. Documented as best-effort: hooks don't travel with clones and break silently — the watch daemon and inline catch-up are the safety net. `kai doctor` verifies presence and version; `kai hook uninstall` removes every kai-managed hook. Never a pre-push or pre-commit blocking hook for graph currency.

**Bulk catch-up.** `kai import --since <sha>` treats `<sha>` as the last-processed commit and imports everything after it, checkpointed with progress. A re-run resumes from the advanced pin (which never moves backward) instead of re-importing.

## Verdict interaction

Unifying rule: **everything in `.kai/` is an assertion pinned to a Git state; distance from HEAD is a confidence discount.**

- Verdicts keep hash-gated de-emphasis. Graph drift composes with it, with a stricter test for verdict marking than for answer annotation: a verdict shows `pending-revalidation` only when an intersecting drift commit touches the verdict's own symbol's file — not on mere neighborhood intersection, and never on conservative new-edge-rule hits alone. Verdict records are never mutated; append-and-supersede only.
- Catch-up processes verdict reweighting as part of each commit's delta, so verdict confidence and graph state advance atomically per checkpoint. *(Deferred to the verdict-loop line; not in steps 1–6.)*

## Out of scope

- Preventing drift (impossible by design)
- Transactional Git+Kai commits, pre-push blocking
- Hunk-level intersection (deferred refinement)
- Multi-remote drift reconciliation
- Server tier — webhook graph updates, `kai pull` graph fast-forward, stale finding badges: [drift-server-tier.md](drift-server-tier.md)

## Acceptance criteria (all verified in implementation)

1. `kai status` on a drifted repo reports relationship + drift count within 100ms (measured ~60ms; fixed cost regardless of drift size).
2. Query on a symbol untouched by drift → `stale-valid` with correct annotation.
3. Query on a symbol touched by a drift commit → `stale-suspect` listing the intersecting commits.
4. New-caller case: drift commit adds a new file importing the queried symbol's module → `stale-suspect`, not `stale-valid`.
5. Branch hop to an unprocessed branch resolves via nearest processed ancestor → `graph-behind`, both-leg counting only on true divergence.
6. Detached checkout of a processed commit reports `synced` against that exact-SHA state.
7. Bulk `import --since` is resumable and serves queries throughout; SIGKILL mid-run lands on a completed checkpoint with an honest remaining count.
8. `kai doctor` detects an orphaned pin, a stale/corrupt manifest, and missing/outdated hooks; `--fix` heals derived state only.
9. `kai prime` output includes the staleness block; an agent consuming it can cite drift in its plan.
10. A renamed file maps its symbols to the intersection check via its old path.
11. `stale-suspect` exits 0 without `--strict`; exits 75 with it.
12. With `kai watch` running, a commit landed with git hooks disabled converges within the debounce window.

## Sequencing (as landed)

1. Per-ref graph pinning + relationship classification + drift computation + `kai status` reporting.
2. Drift manifest (rename-aware, new-file import targets) + file-level intersection + conservative new-edge rule; `kai shadow drift` detail view.
3. Query-time staleness classes + JSON/prime plumbing + `--strict`.
4. Checkpointed incremental catch-up + time-budgeted inline catch-up + `kai import --since`.
5. post-rewrite hook + full hook uninstall + doctor drift checks.
6. Watch-daemon integration (git-state channel, poll backstop).
7. Server webhook + pull fast-forward — separate spec: [drift-server-tier.md](drift-server-tier.md).
