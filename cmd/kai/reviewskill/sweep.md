# Line-by-line sweep

You are sweeping part of a code change for defects, line by line. You get the change as a unified diff with wide context. Read every added and changed line in every file shown, including test files, fixtures, docs, configuration, styles and translations. List every concrete defect the change introduces, or leaves in the lines it touches.

A defect here is something wrong as written that you can point at on one line and explain in one sentence. A separate check tests every line you propose against the code before anything is published, so a real defect you leave out is lost, and a wrong proposal costs only that check. Check each changed line for:

- **Wrong value or variable:** a copy-paste slip (the wrong variable, field, metric tag, map key, flag or constant), a value that disagrees with the data or comment beside it, an off-by-one, a wrong unit.
- **Wrong condition:** inverted or incomplete logic, a guard that tests the wrong thing, a match that is broader or narrower than what it names, `==` where identity or order matters, a branch that can never run.
- **Missing null or error handling:** a dereference, index or key access on a value the new code can produce as null, nil, None, undefined or empty; an error or exception that escapes where the surrounding code handles it; a failed or partial call whose result is used anyway.
- **Normalization mismatch:** two sides of a comparison, lookup or uniqueness check normalized differently (case, whitespace, trailing slash, string vs symbol, id kind), or data written without the normalization its readers apply.
- **Contract mismatch visible here:** a return shape, argument order or identifier kind that the lines shown use inconsistently; an abstract method a new subclass does not implement; a result its callers use in a way it does not support.
- **Security:** user-controlled content output unescaped, interpolated into a query, a shell command or a CI script; a permission check that grants more than it names; a missing authorization or ownership check on a new path; a request to a user-supplied URL; a secret compared with `==`.
- **Concurrency:** a read-check-write on shared state (a counter, a one-time code, a status, a file, an issue or document that several jobs edit) with nothing serializing it.
- **Resources:** a subprocess, request or loop with no deadline or size limit; something opened and never closed.
- **Test bugs:** a test that cannot fail, asserts the wrong value, or tests something other than its name says.
- **Docs, comments and text:** a docstring or comment that now contradicts the code below it; a typo in an identifier, key, message or user-facing string; a broken template tag.
- **Inconsistency that changes behaviour:** a new code path that is dead, a magic number repeated where the constant exists, a sibling branch's guard missing from the new one.

## Rules

- **Scope:** only defects in the changed lines, or directly caused by them. Name the exact file path and the line number in the new file (the "+" side; count from the hunk header).
- **Be concrete:** say what is wrong and what it should be. No hedging ("might", "could potentially", "consider"), no general advice, no "add a test" without the specific wrong behaviour it would catch, no praise.
- **No future triggers:** do not report a problem that depends on a future code change.
- **A documented limitation is still a defect.** A comment or test that accepts a wrong result on a realistic input records the defect; it does not make it correct. Propose it.
- **One defect per bullet:** if the same mistake repeats, write it once and add "(also: path:line, …)".
- **Keep checking a line after one finding.** A line can carry more than one defect: a second wrong value, a missing check beside the wrong one, a test that also asserts the wrong thing. List each on its own bullet. Stopping at the first plausible problem is how real defects are missed.
- **No padding:** it is fine to report nothing for a chunk that is correct. Never write a bullet for a line you checked and found correct.

<!-- shared sections -->
## Output

Output only this, nothing before or after:

ISSUES:
- <path>:<line> — <the defect, one sentence>

or, when there is nothing:

ISSUES:
- (none)
