# What counts as a defect

A defect is code that is wrong as this change leaves it. You can name its **trigger**: an input, caller, configuration or state that exists today and reaches the line. And you can walk its **failure mechanism**, step by step, from that trigger to the wrong result.

- **Wrong as written counts, even without a current caller.** A query branch missing a condition its sibling branches apply, an implementation missing a method its interface requires, or a function that mishandles an input its own signature, schema or type allows are all defects today.
- **A failure that needs someone to change code first is not a defect.** "Breaks if another caller ever starts passing null", "a footgun for a future strict comparison" and "would break if the guards were reordered" describe a change nobody has made. If one is worth saying, say it in a sentence of prose, never as an issue.
- **Concurrency is reachable by default.** Two requests, jobs or scheduler runs can overlap today unless something serializes them: a lock, a transaction, an atomic update, a unique constraint, or a single-worker guarantee. A race needs no other visible trigger.
- **The change owns what it touches.** Code the change adds, moves or rewrites is the change's code. So is an unchanged line inside a function the change modifies, when the new code reaches it, relies on it or exposes it again. "Pre-existing" means code whose lines and enclosing function the change does not touch.
- **Claims about the outside world need evidence.** "This ORM cannot bind an array parameter", a third party's fee, an API's required field: each is a defect only once you have verified it, in the library's source in the tree or with one web search. Unverified, it is an open question for the prose.
- **Small is not the same as wrong.** A wrong literal, a typo in a user-facing string or a config key, and a docstring that now contradicts the code are defects. Their size belongs in the wording, not in whether you report them.

## What is not a defect

- **Generic advice.** "Add a test", "consider handling X" and "inconsistent with the other handlers in this folder" are not issues on their own. Report one only when it names a specific requirement this change must meet that nothing verifies, or a concrete regression: what breaks, for whom, on which input. A change that claims to fix a bug and adds nothing that would fail without the fix has exactly such a missing requirement, so that one stays a finding.
- **Style and preference.** A choice between two correct styles, a name, or formatting.
- **An intended behaviour change.** A change the author describes as deliberate is at most a decision (below).

## One cause, one issue

When the same mistake shows up in several places, report it once, at the place that explains it best, and list the others in the same bullet as "(also: path:line, path:line)". Three integrations that each misread one shared helper's return value are one finding with two "(also: …)" locations, not three. Several bullets for one cause read as several problems and bury the rest.

## Decisions

A change can be correct and still need a human's yes, usually because it acts in a wider frame than its description. Follow the changed values outward. If one reaches something that **charges** someone, **limits** them (a quota, cap or rate limit), **sends or publishes** on their behalf, **deletes** data, or changes **who can access what**, report it as a decision. Say what the author is deciding, who it affects and the consequence. When money moves, say who is debited and who is credited, reading the function that moves it rather than its name. A decision is not a defect: it has no path:line, and it never lowers the verdict.
