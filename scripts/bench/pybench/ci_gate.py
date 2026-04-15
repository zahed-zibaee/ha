#!/usr/bin/env python3
"""Simple CI gate for pybench massive summary."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Evaluate pybench massive summary against CI thresholds.")
    parser.add_argument("summary_json", help="Path to massive-summary.json")
    parser.add_argument("--max-fail", type=int, default=0, help="Maximum failing scenarios allowed")
    parser.add_argument("--max-warn", type=int, default=0, help="Maximum warning scenarios allowed")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    path = Path(args.summary_json)
    if not path.exists():
        print(f"missing summary: {path}", file=sys.stderr)
        return 2
    payload = json.loads(path.read_text(encoding="utf-8"))
    tests = payload.get("tests", {})
    failed = int(tests.get("fail", 0))
    warn_scenarios = 0
    for entry in payload.get("results", []):
        if entry.get("warnings"):
            warn_scenarios += 1
    print(f"ci_gate: fail={failed} warn_scenarios={warn_scenarios}")
    if failed > args.max_fail:
        print(f"ci_gate: failing scenarios {failed} exceeds max {args.max_fail}", file=sys.stderr)
        return 1
    if warn_scenarios > args.max_warn:
        print(f"ci_gate: warning scenarios {warn_scenarios} exceeds max {args.max_warn}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
