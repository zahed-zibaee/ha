from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from scripts.bench.pybench.models import CheckResult, ScenarioResult
from scripts.bench.pybench.reporting import ScenarioReporter, render_scenario_text, utc_now


class ReportingTests(unittest.TestCase):
    def test_render_scenario_text_includes_counts(self) -> None:
        result = ScenarioResult(
            name="api",
            started_at=utc_now(),
            finished_at=utc_now(),
            duration_seconds=3.2,
            status="FAIL",
            checks=[
                CheckResult(name="a", section="API", status="PASS"),
                CheckResult(name="b", section="API", status="FAIL", detail="bad"),
                CheckResult(name="c", section="API", status="WARN", detail="slow"),
            ],
            failures=["API/b: bad"],
            warnings=["API/c: slow"],
        )
        text = render_scenario_text(result)
        self.assertIn("overall: FAIL", text)
        self.assertIn("checks: pass=1 fail=1 warn=1 other=0", text)
        self.assertIn("API/b: bad", text)

    def test_reporter_writes_expected_files(self) -> None:
        with tempfile.TemporaryDirectory(prefix="pybench-report-test-") as tmp:
            scenario_dir = Path(tmp)
            reporter = ScenarioReporter(scenario_dir)
            result = ScenarioResult(
                name="health",
                started_at=utc_now(),
                finished_at=utc_now(),
                duration_seconds=1.1,
                status="PASS",
                checks=[CheckResult(name="healthy", section="API", status="PASS")],
            )
            reporter.write_result(result)
            self.assertTrue((scenario_dir / "run.json").exists())
            self.assertTrue((scenario_dir / "checks.json").exists())
            self.assertTrue((scenario_dir / "summary.json").exists())
            self.assertTrue((scenario_dir / "summary.txt").exists())
            payload = json.loads((scenario_dir / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual(payload["overall"], "PASS")


if __name__ == "__main__":
    unittest.main()
