#!/usr/bin/env python3
"""Tests for review_regressions_report.py: a run that measured nothing must be
flagged, not rendered as "0/0 passed, no sustained regression"."""
import json
import os
import subprocess
import sys
import tempfile
import unittest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "review_regressions_report.py")


def report(finished, total, baseline_finished=None, fail_ids=(), history=None):
    """Run the report over a synthetic run where `finished` of `total` cases graded."""
    with tempfile.TemporaryDirectory() as d:
        cases = {"cases": [{"id": f"c{i}", "expect": [{}]} for i in range(total)]}
        json.dump(cases, open(os.path.join(d, "cases.json"), "w"))

        def make_run(name, n_finished):
            run = os.path.join(d, name)
            os.makedirs(run)
            with open(os.path.join(run, "grades.jsonl"), "w") as f:
                for i in range(n_finished):
                    f.write(json.dumps({"id": f"c{i}", "pass": f"c{i}" not in fail_ids, "missed": [], "leaked": [],
                                        "exceeded": [], "published": 1}) + "\n")
            return run

        argv = [sys.executable, SCRIPT, "--run", make_run("run", finished),
                "--cases", os.path.join(d, "cases.json"), "--ref", "main", "--sha", "abc123def456",
                "--run-url", "https://example/run/1", "--record",
                "--out-comment", os.path.join(d, "comment.md"), "--out-body", os.path.join(d, "body.md"),
                "--out-alert", os.path.join(d, "alert.txt"), "--out-broken", os.path.join(d, "broken.txt")]
        if history is not None:
            old = os.path.join(d, "old-body.md")
            open(old, "w").write("<!-- review-regressions-history\n" + json.dumps(history) + "\n-->")
            argv += ["--issue-body", old]
        if baseline_finished is not None:
            argv += ["--baseline-run", make_run("base", baseline_finished), "--baseline-ref", "v1"]
        subprocess.run(argv, check=True)
        read = lambda n: open(os.path.join(d, n)).read()
        return read("broken.txt"), read("comment.md"), read("body.md")


class BrokenRun(unittest.TestCase):
    def test_nothing_finished_is_broken(self):
        broken, comment, body = report(0, 8)
        self.assertIn("only 0 of 8 reviews finished", broken)
        self.assertIn("not a measurement", comment)
        self.assertNotIn("No sustained regression", body)

    def test_under_half_is_broken(self):
        self.assertTrue(report(3, 8)[0])

    def test_half_finished_is_a_measurement(self):
        broken, comment, body = report(4, 8)
        self.assertEqual(broken, "")
        self.assertNotIn("not a measurement", comment)
        self.assertIn("No sustained regression", body)

    def test_broken_baseline_is_named(self):
        broken, _, _ = report(8, 8, baseline_finished=0)
        self.assertIn("v1: only 0 of 8", broken)


class BrokenAndRegressing(unittest.TestCase):
    def test_dashboard_keeps_sustained_regression_when_run_is_broken(self):
        # c0 passed in the four runs before, failed last time, and fails again now,
        # while only 3 of 8 reviews finished: both lines belong on the dashboard.
        entry = lambda s: {"date": "d", "ref": "main", "sha": "abc", "url": "u",
                           "cases": {"c0": s}, "totals": {"passed": 1, "finished": 1, "cases": 1,
                           "found": 0, "expected": 0, "leaked": 0, "forbidden": 0, "published": 0}}
        history = [entry("pass")] * 4 + [entry("fail")]
        broken, comment, body = report(3, 8, fail_ids=("c0",), history=history)
        self.assertTrue(broken)
        self.assertIn("Latest run is not a measurement", body)
        self.assertIn("Sustained regression:** c0", body)
        self.assertIn("Sustained regression:** c0", comment)


if __name__ == "__main__":
    unittest.main()
