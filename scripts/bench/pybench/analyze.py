"""Analyze pybench massive output directories."""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter
from pathlib import Path
from typing import Any


def _load_summary(path: Path) -> dict[str, Any]:
    summary_path = path / "summary.json"
    if not summary_path.exists():
        return {}
    return json.loads(summary_path.read_text(encoding="utf-8"))


def load_reports(target: Path) -> list[dict[str, Any]]:
    if target.is_file() and target.name == "summary.json":
        return [json.loads(target.read_text(encoding="utf-8"))]
    if target.is_dir() and (target / "massive-summary.json").exists():
        reports: list[dict[str, Any]] = []
        for child in sorted(target.iterdir()):
            if child.is_dir():
                data = _load_summary(child)
                if data:
                    data["scenario"] = child.name
                    reports.append(data)
        return reports
    if target.is_dir():
        data = _load_summary(target)
        if data:
            data["scenario"] = target.name
            return [data]
    raise FileNotFoundError(f"No pybench report found at {target}")


def summarize(reports: list[dict[str, Any]]) -> dict[str, Any]:
    status_counts = Counter()
    symptom_counts = Counter()
    for report in reports:
        status_counts[report.get("overall", "unknown")] += 1
        for failure in report.get("failures", []):
            key = failure.split(":", 1)[0]
            symptom_counts[key] += 1
    return {
        "tests_total": len(reports),
        "status_counts": status_counts,
        "top_symptoms": symptom_counts.most_common(10),
    }


def render_text(reports: list[dict[str, Any]], summary: dict[str, Any]) -> str:
    lines = [
        "Massive Bench Analysis",
        f"tests={summary['tests_total']} pass={summary['status_counts'].get('PASS', 0)} fail={summary['status_counts'].get('FAIL', 0)}",
        "",
        "Tests",
    ]
    for report in reports:
        lines.append(
            f"- {report.get('scenario', 'unknown')}: overall={report.get('overall', 'unknown')} "
            f"failures={len(report.get('failures', []))} warnings={len(report.get('warnings', []))}"
        )
    if summary["top_symptoms"]:
        lines.append("")
        lines.append("Top Symptoms")
        for symptom, count in summary["top_symptoms"]:
            lines.append(f"- {symptom}: {count}")
    return "\n".join(lines)


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Analyze pybench reports from a massive output directory.")
    parser.add_argument("target", help="Massive report directory or per-scenario report directory")
    parser.add_argument("--json", action="store_true", help="Emit JSON output")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    target = Path(args.target)
    try:
        reports = load_reports(target)
    except FileNotFoundError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    summary = summarize(reports)
    if args.json:
        payload = {"summary": summary, "reports": reports}
        payload["summary"]["status_counts"] = dict(summary["status_counts"])
        print(json.dumps(payload, indent=2))
    else:
        print(render_text(reports, summary))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
