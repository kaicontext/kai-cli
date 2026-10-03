# Defect catalog

Walk the change against each category below, file by file, including tests, fixtures, docs, configuration, styles and translations. Each category lists what to look for and one example of a finding worth reporting, next to one that is not.

## 1. Contracts across a call

Opening both files is not the same as reading the contract between them. For every value the change hands from one place to another (an id, a key, an argument, a token, a return value), write down both sides before judging either: what the producer returns or creates, and what the consumer does with it.

- **Kind of value:** which id, which unit, which encoding. For example, a price in cents passed where dollars are expected, or a map keyed by email looked up by user id.
- **Position:** each argument's meaning where it is passed.
- **Shape:** the value itself or a wrapper around it. For example, a promise used as if it were its result.
- **Freshness:** the current value, or a copy taken before it changed. For example, a configuration object read once and used after it was reloaded.
- **Signatures and callers:** a changed signature, field or return whose callers were not updated.
- **New values:** a new enum value or status that some switch, filter or allowlist does not handle.
- **Ignored results:** a return value the caller needs but drops.

A mismatch you have traced is a defect. Name both ends.

> Finding: `billing/notify.ts:16`: `notify` gets `org.name` as its third argument, which is the recipient address. Every other call passes `org.billingEmail`, so this reminder goes to an address that does not exist.
> Not a finding: "consider validating the email format".

## 2. Shared state and concurrency

For every read, check and write of shared state (a database row, a counter, a balance, a status, a quota), run it twice at the same time in your head. Both requests read the same value, both pass the check, both write.

- A value written as the value read, plus or minus something. A balance updated as the value read minus the amount loses one of two withdrawals.
- A check made in memory and written back afterwards. Stock checked and then decremented can be oversold.
- A status checked and then updated, which lets two workers take the same job.
- Find-or-create without a unique constraint.
- State mutated outside the lock the surrounding code uses.

Each of these is a defect unless something serializes it: an atomic update (an increment, a compare-and-set, `UPDATE … WHERE` on the old value), a transaction holding a row lock, or a unique constraint. Name the two requests and the interleaving. "Not thread-safe" alone is not a finding.

> Finding: `jobs/worker.ts:8`: `attempts: job.attempts + 1` writes back the value read at line 4. Two workers that fail the same job together both read 2 and both write 3, so one failure is lost and the job retries past its limit. Use an atomic increment.
> Not a finding: "this code is not thread-safe".

## 3. Errors and absent values

- A dereference, index or key access on a value the new code can produce as null, nil, None, undefined or empty.
- An error created and dropped, returned to a caller that ignores it, or handled in the wrong branch.
- An exception that escapes where the surrounding code handles the same one.
- A cleanup that runs on the success path only, or a rollback that removes things it never created.
- A partial failure that leaves state half-written.

> Finding: `sync/export.go:57`: when the upload fails, the error is logged and the function returns nil. `runExport` then records the export as complete, so the missing file is never retried.
> Not a finding: "error handling could be more robust here".

## 4. Inputs, security and access

- **Validation:** an input newly trusted without validation at a trust boundary.
- **Injection:**
  - SQL built by string concatenation.
  - A shell command assembled from a path or user input instead of passed as arguments.
  - A template or HTML output that renders user content unescaped.
  - A file path built from user input (path traversal).
  - A request sent to a URL the user controls.
- **Authorization:**
  - A new path missing the authentication or ownership check that its sibling paths have.
  - A permission check that grants more than it names.
  - An authorization default that allows instead of denying.
- **Secrets:** compared with `==` instead of a constant-time comparison, logged, or returned in a response.
- **Sibling paths share guards:** when the change adds a branch beside an existing one that does the same kind of work, list every guard the older branch has that the new one lacks: a status check, an idempotency key, an auth check, a size limit. A guard the author already wrote once and did not carry over is a defect.

> Finding: `api/projects.py:88`: the new `archive_project` view fetches the project by the id in the URL and archives it without the `project.owner == request.user` check that `delete_project` (line 61) performs, so any signed-in user can archive anyone's project.
> Not a finding: "consider adding rate limiting to this endpoint".

## 5. Resources and the environment

Code that works on the author's machine is not yet correct. Name what the change assumes about the world outside the process. The finding is the failure mode when the assumption is false.

- **Subprocesses that never return:** every external command needs a deadline. In Go a context deadline alone is not enough, because the output read can block past the cancel on pipes a child still holds. `WaitDelay` is required too.
- **Leaks:** a goroutine, timer, file, connection or listener opened with no path that stops or closes it.
- **Configuration read from the wrong place:** for example, git identity checked in the environment when it lives in gitconfig. The environment then wins over the real setting.
- **Cost on a hot path:** how often does this run? A subprocess per repository inside a loop that polls every few seconds is a cost the user pays constantly.
- **An optional step that can abort the whole operation:** an optimisation or nicety whose failure is fatal.
- **Writes nobody asked for:** to git history, a dotfile, or anything outside the tool's own state. Name them, and say whether there is an opt-out.
- **New defaults that point somewhere:** a new default URL, host, email address or path ships to everyone who never sets it. Confirm that the target exists and that something here serves it. A default on a domain nobody here owns is a defect.
- **New hosts:** a host being new to the repository is not a defect by itself. Report one only with a concrete incorrect URL (a typo, the wrong environment of a host the code already uses, a path the target does not serve) or a code path that fails because of it. A host in a test fixture or example data, a developer script, docs, or a provider's documented endpoint is not a finding on its own. HOSTS THIS CHANGE INTRODUCES, when present, lists the new ones for you to check: it is context, not a finding list.

## 6. Tests

When a change claims to fix a bug, the question is whether its test **fails with the fix removed**, not whether a test exists. If the test the change presents as the proof of its fix would pass on the old code, the fix is unverified, and that is a finding. Name the test and what it would have to assert. Three shapes look like coverage and are not:

- A test that calls the code and discards the answer.
- A test that asserts the mechanism was configured instead of the behaviour it buys. Checking that a timeout was set proves nothing if the call can still hang.
- A test that skips for an environmental reason on the machine that runs it, so it runs nowhere.

A test that asserts the wrong value, or one that tests something other than its name says, is a defect too.

> Finding: `cache/cache_test.go:40`: `TestEvictsExpired` is the change's proof that expired entries are evicted. It sets an entry, calls `Evict()` and asserts no error, but never checks the entry is gone, so it passes on the old code that evicted nothing.
> Not a finding: "this function has no unit test".

## 7. Data and schema

- A migration that cannot be rolled back, locks a large table, or adds a non-null column with no default or backfill.
- A query or ORM call that names a column or field the schema does not have, or that omits a condition its sibling queries apply.
- Data written without the normalization its readers apply, or read with a different one.

## 8. Small slips on a line

- A copy-paste slip: the wrong variable, field, key, flag or constant.
- A literal that disagrees with the data or comment beside it.
- An off-by-one, or a wrong unit.
- An inverted or incomplete condition, or a branch that can never run.
- Two sides of a comparison normalized differently (case, whitespace, trailing slash, type).
- A docstring or comment that now contradicts the code below it.
- A typo in an identifier, key, message or user-facing string.
