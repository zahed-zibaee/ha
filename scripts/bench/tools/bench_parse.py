#!/usr/bin/env python3
"""CLI for parsing bench API and metrics payloads."""

from __future__ import annotations

import argparse
import sys

from benchlib.parsing import metrics_snapshot, metrics_sum, parse_check_payload, parse_lb_payload, parse_leader_payload


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Bench payload parser.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    for name in ("leader", "check", "lb", "metrics-snapshot"):
        subparsers.add_parser(name)

    metrics = subparsers.add_parser("metrics-sum")
    metrics.add_argument("--metric", required=True)
    metrics.add_argument("--label-key", default="")
    metrics.add_argument("--label-value", default="")
    return parser.parse_args()


def read_stdin() -> str:
    return sys.stdin.read()


def main() -> int:
    args = parse_args()
    body = read_stdin()

    if args.command == "leader":
        parsed = parse_leader_payload(body)
        sys.stdout.write("|".join([parsed["leader"], parsed["status"], parsed["node_id"], parsed["probes_active"]]))
        return 0

    if args.command == "check":
        parsed = parse_check_payload(body)
        sys.stdout.write("|".join([parsed["total"], parsed["reachable"], parsed["redis_status"]]))
        return 0

    if args.command == "lb":
        parsed = parse_lb_payload(body)
        sys.stdout.write("|".join([parsed["name"], parsed["group"], parsed["reachable"]]))
        return 0

    if args.command == "metrics-snapshot":
        sys.stdout.write(metrics_snapshot(body))
        return 0

    if args.command == "metrics-sum":
        sys.stdout.write(metrics_sum(body, args.metric, args.label_key, args.label_value))
        return 0

    return 2


if __name__ == "__main__":
    raise SystemExit(main())
