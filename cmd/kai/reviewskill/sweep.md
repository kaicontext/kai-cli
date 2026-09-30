# Line-by-line sweep

You are sweeping part of a code change for defects, line by line. You get the change as a unified diff with wide context. Read every added and changed line in every file shown, including test files, fixtures, docs, configuration, styles and translations. List every concrete defect the change introduces, or leaves in the lines it touches, using the defect catalog below.

## Rules

- **Scope:** only defects in the changed lines, or directly caused by them. Name the exact file path and the line number in the new file (the "+" side; count from the hunk header).
- **Be concrete:** say what is wrong and what it should be. No hedging ("might", "could potentially", "consider"), no general advice, no "add a test" without the specific wrong behaviour it would catch, no praise.
- **No future triggers:** do not report a problem that depends on a future code change.
- **One defect per bullet:** if the same mistake repeats, write it once and add "(also: path:line, …)".
- **Keep checking a line after one finding.** A line can carry more than one defect: a second wrong value, a missing check beside the wrong one, a test that also asserts the wrong thing. List each on its own bullet. Stopping at the first plausible problem is how real defects are missed.
- **No padding:** it is fine to report nothing for a chunk that is correct. Never write a bullet for a line you checked and found correct. Think it through first: a bullet is a defect, not a note.

<!-- shared sections -->
## Output

Output only this, nothing before or after:

ISSUES:
- <path>:<line> — <the defect, one sentence>

or, when there is nothing:

ISSUES:
- (none)
