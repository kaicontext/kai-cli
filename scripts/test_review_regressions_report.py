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


def report(finished, total, baseline_finished=None):
    """Run the report over a synthetic run where `finished` of `total` cases graded."""
    with tempfile.TemporaryDirectory() as d:
        cases = {"cases": [{"id": f"c{i}", "expect": [{}]} for i in range(total)]}
        json.dump(cases, open(os.path.join(d, "cases.json"), "w"))

        def make_run(name, n_finished):
            run = os.path.join(d, name)
            os.makedirs(run)
            with open(os.path.join(run, "grades.jsonl"), "w") as f:
                for i in range(n_finished):
                    f.write(json.dumps({"id": f"c{i}", "pass": True, "missed": [], "leaked": [],
                                        "exceeded": [], "published": 1}) + "\n")
            return run

        argv = [sys.executable, SCRIPT, "--run", make_run("run", finished),
                "--cases", os.path.join(d, "cases.json"), "--ref", "main", "--sha", "abc123def456",
                "--run-url", "https://example/run/1", "--record",
                "--out-comment", os.path.join(d, "comment.md"), "--out-body", os.path.join(d, "body.md"),
                "--out-alert", os.path.join(d, "alert.txt"), "--out-broken", os.path.join(d, "broken.txt")]
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


if __name__ == "__main__":
    unittest.main()
