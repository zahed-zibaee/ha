"""Shared dataclasses for pybench."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


@dataclass
class CheckResult:
    name: str
    status: str
    detail: str = ""
    section: str = "General"


@dataclass
class ScenarioResult:
    name: str
    started_at: str
    finished_at: str
    duration_seconds: float
    status: str
    checks: list[CheckResult] = field(default_factory=list)
    failures: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    artifacts: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass
class RunConfig:
    root_dir: Path
    out_dir: Path
    compose_file: Path
    config_path: Path
    default_group: str = "web-health"
    groups_override: str = ""
    publish_bind: str = "127.0.0.1"
    internal_port: int = 8080
    url_mode: str = "auto"
    endpoint_retries: int = 2
    endpoint_delay: float = 0.5
    endpoint_max_time: float = 3.0
    require_all_replicas_health: bool = False
    wait_url_tries: int = 45
    wait_url_sleep: float = 2.0
    wait_leader_timeout: int = 60
    wait_checks_timeout: int = 60
    wait_replica_timeout: int = 60
    auto_groups: bool = True
    force_recreate: bool = False
    verbose: bool = False
    random_seed: int = 7
    keep_reports: bool = True
    print_summary: bool = True
    profile: str = "pragmatic"
    fail_on_warn: bool = False


@dataclass
class MassiveSummary:
    total: int
    passed: int
    failed: int
    duration_seconds: float
