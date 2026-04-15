from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from scripts.bench.pybench.analyze import load_reports, render_text, summarize


def _write_summary(path: Path, overall: str, failures: list[str], warnings: list[str]) -> None:
    payload = {
        "overall": overall,
        "checks": {"pass": 1, "fail": 1 if overall == "FAIL" else 0, "warn": len(warnings), "other": 0},
        "failures": failures,
        "warnings": warnings,
    }
    (path / "summary.json").write_text(json.dumps(payload), encoding="utf-8")


class AnalyzeTests(unittest.TestCase):
    def test_load_and_summarize_massive_dir(self) -> None:
        with tempfile.TemporaryDirectory(prefix="pybench-analyze-") as tmp:
            root = Path(tmp)
            (root / "massive-summary.json").write_text("{}", encoding="utf-8")
            a = root / "api"
            b = root / "leader"
            a.mkdir()
            b.mkdir()
            _write_summary(a, "PASS", [], [])
            _write_summary(b, "FAIL", ["API/lb: status=500"], [])
            reports = load_reports(root)
            self.assertEqual(len(reports), 2)
            summary = summarize(reports)
            self.assertEqual(summary["tests_total"], 2)
            self.assertEqual(summary["status_counts"]["PASS"], 1)
            self.assertEqual(summary["status_counts"]["FAIL"], 1)
            text = render_text(reports, summary)
            self.assertIn("Top Symptoms", text)


if __name__ == "__main__":
    unittest.main()
