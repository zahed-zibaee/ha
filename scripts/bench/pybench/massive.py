"""Massive suite orchestration for pybench."""

from __future__ import annotations

import os
import tempfile
from pathlib import Path

from .models import MassiveSummary, RunConfig, ScenarioResult
from .reporting import write_massive_summary
from .scenarios import run_scenario

DEFAULT_TESTS = (
    "consistency leader health api loadbalancer distribution latency concurrency "
    "resilience redis_flap cold_start churn chaos concurrent_chaos_load dns_failover "
    "leader_kill_during_probes full_restart multi_group goroutine_leak multi_group_stress"
)


def default_tests_from_env() -> list[str]:
    return os.environ.get("MASSIVE_TESTS", DEFAULT_TESTS).split()


def run_massive(config: RunConfig, tests: list[str] | None = None) -> tuple[list[ScenarioResult], MassiveSummary]:
    tests = tests or default_tests_from_env()
    fixed_dir = os.environ.get("MASSIVE_REPORTS_DIR", "").strip()
    if fixed_dir:
        run_root = Path(fixed_dir)
        run_root.mkdir(parents=True, exist_ok=True)
    else:
        run_root = Path(tempfile.mkdtemp(prefix="ha-bench-massive-"))
    print(f"massive: reports dir {run_root}")
    print(f"massive: profile {config.profile}")
    results: list[ScenarioResult] = []
    for name in tests:
        scenario_dir = run_root / name
        print(f"starting scenario: {name}")
        metadata = {
            "dist_samples": os.environ.get("MASSIVE_DIST_SAMPLES", "240"),
            "stress_max_error_pct": os.environ.get("STRESS_MAX_ERROR_PCT", "5.0"),
        }
        result = run_scenario(name=name, config=config, scenario_dir=scenario_dir, metadata=metadata)
        results.append(result)
        print(f"finished scenario: {name} status={result.status} duration={int(result.duration_seconds)}s")
    summary = write_massive_summary(run_root, results)
    print((run_root / "massive-summary.txt").read_text(encoding="utf-8"))
    return results, summary
