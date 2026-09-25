#!/usr/bin/env python3
"""Turn a scripts/review-regressions.sh run into the pinned-issue dashboard.

  review_regressions_report.py --run DIR --cases cases.json --issue-body old.md \
      --ref main --sha abc123 --run-url URL [--baseline-run DIR --baseline-ref v0.35.89] \
      --out-body new.md --out-comment comment.md --out-alert alert.txt

The history lives inside the issue body, in an HTML comment, so the dashboard
needs no branch or file of its own. Each run appends one entry (per-case
status plus the totals) and the body is re-rendered from the history.

A case is "unfinished" when the review never finished (an incomplete bundle
with no claims): that is review infrastructure, not the reviewer, and it is
neither a pass nor a fail. An alert fires only for a sustained regression: a
case that passed in at least three of the five runs before, and has now
failed (not merely been unfinished) twice in a row. A single failure is noise;
the model varies between runs.
"""
import argparse
import datetime as dt
import json
import os
import re
import sys

HISTORY_RE = re.compile(r"<!-- review-regressions-history\n(.*?)\n-->", re.S)
KEEP = 60


def load_run(run_dir, cases):
    """Per-case result of one run: status, what was missed/leaked, counts."""
    grades = {}
    path = os.path.join(run_dir, "grades.jsonl")
    if os.path.exists(path):
        for line in open(path):
            line = line.strip()
            if line:
                g = json.loads(line)
                grades[g["id"]] = g
    out = {}
    for c in cases:
        cid = c["id"]
        finding = {}
        fp = os.path.join(run_dir, cid + ".json")
        try:
            finding = json.load(open(fp))
        except (OSError, ValueError):
            pass
        g = grades.get(cid)
        if g is None or (finding.get("incomplete") and not finding.get("claims")):
            out[cid] = {"status": "unfinished"}
            continue
        out[cid] = {
            "status": "pass" if g["pass"] else "fail",
            "missed": g["missed"], "leaked": g["leaked"], "exceeded": g["exceeded"],
            "published": g["published"],
            "expect": len(c.get("expect", [])), "forbid": len(c.get("forbid", [])),
        }
    return out


def totals(run):
    done = [r for r in run.values() if r["status"] != "unfinished"]
    return {
        "passed": sum(r["status"] == "pass" for r in done),
        "finished": len(done),
        "cases": len(run),
        "found": sum(r["expect"] - len(r["missed"]) for r in done),
        "expected": sum(r["expect"] for r in done),
        "leaked": sum(len(r["leaked"]) for r in done),
        "forbidden": sum(r["forbid"] for r in done),
        "published": sum(r["published"] for r in done),
    }


def alerts(history, case_ids):
    """Cases in a sustained regression, judged over the stored history."""
    out = []
    for cid in case_ids:
        seq = [h["cases"].get(cid, "unfinished") for h in history]
        if len(seq) < 2 or seq[-1] != "fail" or seq[-2] != "fail":
            continue
        before = [s for s in seq[:-2] if s != "unfinished"][-5:]
        if sum(s == "pass" for s in before) >= 3:
            out.append(cid)
    return out


def fmt_totals(t):
    return (f"{t['passed']}/{t['finished']} passed · golden {t['found']}/{t['expected']} · "
            f"leaked {t['leaked']}/{t['forbidden']} · {t['published']} published")


def case_table(run):
    rows = ["| Case | Result | Missed | Leaked / over cap |", "|---|---|---|---|"]
    for cid, r in run.items():
        if r["status"] == "unfinished":
            rows.append(f"| {cid} | ⚪ review did not finish | — | — |")
            continue
        icon = "✅" if r["status"] == "pass" else "❌"
        missed = "<br>".join(m[:140] for m in r["missed"]) or "—"
        leaked = "<br>".join((l[:140] for l in r["leaked"] + r["exceeded"])) or "—"
        rows.append(f"| {cid} | {icon} {r['published']} published | {missed} | {leaked} |")
    return "\n".join(rows)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--run", required=True)
    ap.add_argument("--cases", required=True)
    ap.add_argument("--issue-body", default="")
    ap.add_argument("--ref", required=True)
    ap.add_argument("--sha", required=True)
    ap.add_argument("--run-url", required=True)
    ap.add_argument("--baseline-run", default="")
    ap.add_argument("--baseline-ref", default="")
    ap.add_argument("--record", action="store_true", help="append this run to the nightly history")
    ap.add_argument("--out-body", default="")
    ap.add_argument("--out-comment", required=True)
    ap.add_argument("--out-alert", default="")
    a = ap.parse_args()

    cases = json.load(open(a.cases))["cases"]
    ids = [c["id"] for c in cases]
    run = load_run(a.run, cases)
    t = totals(run)
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")

    history = []
    old = open(a.issue_body).read() if a.issue_body and os.path.exists(a.issue_body) else ""
    m = HISTORY_RE.search(old)
    if m:
        try:
            history = json.loads(m.group(1))
        except ValueError:
            history = []
    if a.record:
        history.append({"date": now, "ref": a.ref, "sha": a.sha[:12], "url": a.run_url,
                        "cases": {cid: r["status"] for cid, r in run.items()}, "totals": t})
        history = history[-KEEP:]
    firing = alerts(history, ids) if a.record else []

    # The run's own comment: this run in full, and the baseline beside it.
    lines = [f"### Review regressions — {a.ref} @ `{a.sha[:12]}` ({now})", "",
             f"**{fmt_totals(t)}** · [run]({a.run_url})", ""]
    if a.baseline_run:
        bt = totals(load_run(a.baseline_run, cases))
        lines += ["| | " + (a.baseline_ref or "baseline") + " | " + a.ref + " |", "|---|---|---|",
                  f"| Cases passed | {bt['passed']}/{bt['finished']} | {t['passed']}/{t['finished']} |",
                  f"| Golden bugs found | {bt['found']}/{bt['expected']} | {t['found']}/{t['expected']} |",
                  f"| Forbidden findings leaked | {bt['leaked']}/{bt['forbidden']} | {t['leaked']}/{t['forbidden']} |",
                  f"| Findings published | {bt['published']} | {t['published']} |", "",
                  "_One run each; the model varies between runs. Unfinished reviews are left out of the counts._", ""]
    if firing:
        lines += ["🚨 **Sustained regression:** " + ", ".join(firing) +
                  " passed in most recent runs and has now failed twice in a row.", ""]
    lines += [case_table(run)]
    open(a.out_comment, "w").write("\n".join(lines) + "\n")

    if a.out_body:
        last = history[-1]["totals"] if history else t
        trend = ["| When | Ref | Passed | Golden | Leaked | Published | Cases |", "|---|---|---|---|---|---|---|"]
        for h in reversed(history[-14:]):
            ht = h["totals"]
            marks = "".join({"pass": "✅", "fail": "❌"}.get(h["cases"].get(cid), "⚪") for cid in ids)
            trend.append(f"| [{h['date']}]({h['url']}) | {h['ref']} `{h['sha']}` | {ht['passed']}/{ht['finished']} | "
                         f"{ht['found']}/{ht['expected']} | {ht['leaked']}/{ht['forbidden']} | {ht['published']} | {marks} |")
        body = [
            "Nightly replay of `kai review-commit` on six real PRs from Martian's Code Review Bench, graded against their golden comments ([`cases.json`](https://github.com/kaicontext/kai-cli/blob/main/cmd/kai/testdata/review-regressions/cases.json), [`review-regressions.sh`](https://github.com/kaicontext/kai-cli/blob/main/scripts/review-regressions.sh)). Updated by the `review-regressions` workflow; do not edit by hand.",
            "",
            f"**Latest ({history[-1]['date'] if history else now}):** {fmt_totals(last)}",
            "",
            ("🚨 **Sustained regression:** " + ", ".join(firing)) if firing else "No sustained regression.",
            "",
            "Case columns, in order: " + ", ".join(f"`{cid}`" for cid in ids) + ". ✅ pass · ❌ fail · ⚪ review did not finish.",
            "",
            "\n".join(trend),
            "",
            "A case fails when a golden bug is missed (`expect`), a known-bad finding is published (`forbid`), or one cause is posted too many times (`at_most`). One run is noisy; read the trend.",
            "",
            "<!-- review-regressions-history\n" + json.dumps(history) + "\n-->",
        ]
        open(a.out_body, "w").write("\n".join(body) + "\n")

    if a.out_alert:
        open(a.out_alert, "w").write(",".join(firing))
    return 0


if __name__ == "__main__":
    sys.exit(main())
