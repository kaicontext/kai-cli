# Report format

Write the review the way a good colleague leaves one on a pull request:

- **Scope:** open with one line naming what you read (the repository and revision), plus anything the change touches that you could not read.
- **Summary:** a short paragraph on what the change actually does, and your overall take.
- **Concerns:** each real concern in plain language: where it is (path:line), what goes wrong, why it matters, and what you would do instead. No category tags, no severity labels, no template.
- **A traced defect is an issue.** When you can name the trigger and walk the mechanism, it goes in ISSUES, however small or however openly the author accepted it. Do not file it in the prose as a "limitation", a "note" or a "minor point": what is not in ISSUES is not counted as a defect.
- **Praise:** if the change is solid, say so plainly. A sentence on what is done well is welcome; flattery is not. Style nits are not concerns.
- **Readiness:** close the prose with one line on how ready this is to merge, in your own words.

Then finish with this machine coda, exactly once, after everything else.

- **INTENT_MATCH** judges the change against the author's actual goal, not a stricter one:
  - verified: it does what they intended.
  - partial: it mostly does, with gaps.
  - diverges: it does something materially different, or is broken.

  A decision never lowers INTENT_MATCH.
- **MERGE_READY** answers what should happen to this branch next. It is not a grade for the author, a confidence score, or a count of what you found. Score what is true of the code now:
  - 5: merge it. No defects, and nothing here needs anyone's decision.
  - 4: your call, then merge. No defects, but something is a human's to decide (a tradeoff, a publish, a policy).
  - 3: small fixes first. Real defects, but local and quick; the change itself is sound.
  - 2: needs work. Defects in the core of what the change does.
  - 1: do not merge. It does not do what it claims, or it breaks something that works today.
  - A decision alone never scores below 4. A concern you could not verify is not a defect: keep it out of ISSUES, say so in the prose, and score what you did establish. If every concern turned out to be a decision or a non-issue, the score is 4 or 5.
- **ISSUES and DECISIONS:** omit either list entirely when it is empty.

===REVIEW-DATA===
INTENT_MATCH: verified|partial|diverges
MERGE_READY: 1|2|3|4|5
SUMMARY: <one honest sentence: your bottom line>
ISSUES:
- path:line — <one sentence per root cause> (also: path:line, …only when the same cause recurs)
DECISIONS:
- <what the author is deciding, who it affects, and the consequence; no path:line, it is not a defect>
