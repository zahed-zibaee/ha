"""Siege adapter for load scenarios."""

from __future__ import annotations

import json
import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path


@dataclass
class SiegeResult:
    ok: bool
    output_path: Path
    stats: dict[str, str]
    return_code: int


def parse_siege_json_output(text: str) -> dict[str, str]:
    data: dict[str, str] = {}
    try:
        payload = json.loads(text)
    except json.JSONDecodeError:
        payload = {}
    if isinstance(payload, dict):
        for key in (
            "transactions",
            "transaction_rate",
            "response_time",
            "availability",
            "successful_transactions",
            "failed_transactions",
            "longest_transaction",
            "shortest_transaction",
        ):
            data[key] = str(payload.get(key, ""))
    return data


def run_siege(
    *,
    target_url: str,
    out_dir: Path,
    label: str,
    concurrency: int = 20,
    duration: str = "20s",
    timeout_seconds: int = 180,
) -> SiegeResult:
    out_dir.mkdir(parents=True, exist_ok=True)
    output_path = out_dir / f"siege-{label}.json"
    if not shutil.which("siege"):
        return SiegeResult(ok=False, output_path=output_path, stats={}, return_code=127)

    cmd = [
        "siege",
        "--json-output",
        "--concurrent",
        str(concurrency),
        "--time",
        str(duration),
        target_url,
    ]
    try:
        completed = subprocess.run(cmd, capture_output=True, text=True, check=False, timeout=timeout_seconds)
        payload = completed.stdout.strip() or completed.stderr.strip()
        output_path.write_text(payload, encoding="utf-8")
        stats = parse_siege_json_output(payload)
        return SiegeResult(ok=completed.returncode == 0, output_path=output_path, stats=stats, return_code=completed.returncode)
    except subprocess.TimeoutExpired:
        payload = f"siege timeout after {timeout_seconds}s"
        output_path.write_text(payload, encoding="utf-8")
        return SiegeResult(ok=False, output_path=output_path, stats={}, return_code=124)
