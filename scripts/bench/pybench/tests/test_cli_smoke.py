from __future__ import annotations

import io
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path

from scripts.bench.pybench.cli import main


class CliSmokeTests(unittest.TestCase):
    def test_list_command_prints_known_scenario(self) -> None:
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(["list"])
        self.assertEqual(code, 0)
        self.assertIn("consistency", buffer.getvalue())

    def test_analyze_command_reads_scenario_dir(self) -> None:
        with tempfile.TemporaryDirectory(prefix="pybench-cli-") as tmp:
            root = Path(tmp)
            summary = root / "summary.json"
            summary.write_text(
                '{"overall":"PASS","checks":{"pass":1,"fail":0,"warn":0,"other":0},"failures":[],"warnings":[]}',
                encoding="utf-8",
            )
            buffer = io.StringIO()
            with redirect_stdout(buffer):
                code = main(["analyze", str(root)])
            self.assertEqual(code, 0)
            self.assertIn("Massive Bench Analysis", buffer.getvalue())


if __name__ == "__main__":
    unittest.main()
