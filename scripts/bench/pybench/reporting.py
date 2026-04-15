"""Structured reporting and artifact writers."""

from __future__ import annotations

import json
from dataclasses import asdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .models import CheckResult, MassiveSummary, ScenarioResult


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def format_duration(total_seconds: float) -> str:
    total = max(int(total_seconds), 0)
    hours = total // 3600
    minutes = (total % 3600) // 60
    seconds = total % 60
    if hours > 0:
        return f"{hours}h{minutes:02d}m{seconds:02d}s"
    if minutes > 0:
        return f"{minutes}m{seconds:02d}s"
    return f"{seconds}s"


class ScenarioReporter:
    def __init__(self, scenario_dir: Path) -> None:
        self.scenario_dir = scenario_dir
        self.attachments_dir = self.scenario_dir / "attachments"
        self.events_path = self.scenario_dir / "events.jsonl"
        self.scenario_dir.mkdir(parents=True, exist_ok=True)
        self.attachments_dir.mkdir(parents=True, exist_ok=True)

    def event(self, level: str, message: str, **fields: Any) -> None:
        payload = {"ts": utc_now(), "level": level, "message": message, **fields}
        with self.events_path.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(payload, sort_keys=True) + "\n")

    def attach_text(self, name: str, content: str) -> str:
        path = self.attachments_dir / name
        path.write_text(content, encoding="utf-8")
        return str(path)

    def write_result(self, result: ScenarioResult) -> None:
        run_path = self.scenario_dir / "run.json"
        summary_json = self.scenario_dir / "summary.json"
        checks_json = self.scenario_dir / "checks.json"
        summary_text = self.scenario_dir / "summary.txt"

        run_payload = {
            "name": result.name,
            "started_at": result.started_at,
            "finished_at": result.finished_at,
            "duration_seconds": result.duration_seconds,
            "status": result.status,
            "metadata": result.metadata,
            "artifacts": result.artifacts,
        }
        run_path.write_text(json.dumps(run_payload, indent=2, sort_keys=True), encoding="utf-8")
        checks_json.write_text(
            json.dumps([asdict(check) for check in result.checks], indent=2, sort_keys=True),
            encoding="utf-8",
        )
        summary_payload = {
            "overall": result.status,
            "checks": {
                "pass": sum(1 for c in result.checks if c.status == "PASS"),
                "fail": sum(1 for c in result.checks if c.status == "FAIL"),
                "warn": sum(1 for c in result.checks if c.status == "WARN"),
                "other": sum(1 for c in result.checks if c.status not in {"PASS", "FAIL", "WARN"}),
            },
            "failures": result.failures,
            "warnings": result.warnings,
            "artifacts": result.artifacts,
            "duration_seconds": result.duration_seconds,
        }
        summary_json.write_text(json.dumps(summary_payload, indent=2, sort_keys=True), encoding="utf-8")
        summary_text.write_text(render_scenario_text(result), encoding="utf-8")


def render_scenario_text(result: ScenarioResult) -> str:
    pass_count = sum(1 for c in result.checks if c.status == "PASS")
    fail_count = sum(1 for c in result.checks if c.status == "FAIL")
    warn_count = sum(1 for c in result.checks if c.status == "WARN")
    other_count = len(result.checks) - pass_count - fail_count - warn_count
    lines = [
        "Benchmark Summary",
        f"scenario: {result.name}",
        f"overall: {result.status}",
        f"checks: pass={pass_count} fail={fail_count} warn={warn_count} other={other_count}",
        f"duration: {format_duration(result.duration_seconds)}",
        "",
        "Checks",
    ]
    for check in result.checks:
        suffix = f" ({check.detail})" if check.detail else ""
        lines.append(f"- {check.status} {check.section}/{check.name}{suffix}")
    lines.append("")
    lines.append("Failures")
    if result.failures:
        for item in result.failures:
            lines.append(f"- {item}")
    else:
        lines.append("- (none)")
    lines.append("")
    lines.append("Warnings")
    if result.warnings:
        for item in result.warnings:
            lines.append(f"- {item}")
    else:
        lines.append("- (none)")
    lines.append("")
    lines.append(f"TEST_END name={result.name} ts={result.finished_at}")
    return "\n".join(lines)


def write_massive_summary(base_dir: Path, results: list[ScenarioResult]) -> MassiveSummary:
    total = len(results)
    passed = sum(1 for result in results if result.status == "PASS")
    failed = total - passed
    duration_seconds = sum(item.duration_seconds for item in results)
    summary = MassiveSummary(total=total, passed=passed, failed=failed, duration_seconds=duration_seconds)
    payload = {
        "overall": "PASS" if failed == 0 else "FAIL",
        "tests": {"total": total, "pass": passed, "fail": failed},
        "duration_seconds": duration_seconds,
        "results": [
            {
                "name": item.name,
                "status": item.status,
                "duration_seconds": item.duration_seconds,
                "failures": item.failures,
                "warnings": item.warnings,
                "artifacts": item.artifacts,
            }
            for item in results
        ],
    }
    (base_dir / "massive-summary.json").write_text(json.dumps(payload, indent=2, sort_keys=True), encoding="utf-8")

    lines = [
        "Massive Summary",
        f"overall: {'PASS' if failed == 0 else 'FAIL'}",
        f"tests: total={total} pass={passed} fail={failed}",
        f"duration: {format_duration(duration_seconds)}",
        "",
        "Tests",
    ]
    for item in results:
        lines.append(f"- {item.status} {item.name} ({format_duration(item.duration_seconds)})")
    if any(item.failures for item in results):
        lines.append("")
        lines.append("Failures")
        for item in results:
            for failure in item.failures:
                lines.append(f"- {item.name}: {failure}")
    (base_dir / "massive-summary.txt").write_text("\n".join(lines), encoding="utf-8")
    return summary


def aggregate_check(section: str, name: str, ok: bool, detail: str = "") -> CheckResult:
    status = "PASS" if ok else "FAIL"
    return CheckResult(name=name, section=section, status=status, detail=detail)
