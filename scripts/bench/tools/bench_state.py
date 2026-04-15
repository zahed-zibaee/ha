#!/usr/bin/env python3
"""CLI for bench state and discovery helpers."""

from __future__ import annotations

import argparse
from pathlib import Path
import sys

from benchlib.state import collect_groups, discover_replicas, http_healthy


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Bench state helper.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    discover = subparsers.add_parser("discover")
    discover.add_argument("--root-dir", required=True)
    discover.add_argument("--internal-port", type=int, default=8080)
    discover.add_argument("--publish-bind", default="127.0.0.1")
    discover.add_argument("--mode", default="auto")

    healthy = subparsers.add_parser("healthy")
    healthy.add_argument("--url", required=True)
    healthy.add_argument("--max-time", type=float, default=2.0)

    groups = subparsers.add_parser("collect-groups")
    groups.add_argument("--config", required=True)
    groups.add_argument("--auto-groups", default="true")
    groups.add_argument("--groups-override", default="")
    groups.add_argument("--group-default", default="web-health")

    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.command == "discover":
        replicas = discover_replicas(
            root_dir=Path(args.root_dir),
            internal_port=args.internal_port,
            publish_bind=args.publish_bind,
            mode=args.mode,
        )
        for replica in replicas:
            print(f"{replica.name}\t{replica.ip}\t{replica.url}")
        return 0 if replicas else 1

    if args.command == "healthy":
        return 0 if http_healthy(args.url, timeout=args.max_time) else 1

    if args.command == "collect-groups":
        auto_groups = args.auto_groups.lower() == "true"
        groups = collect_groups(
            config_path=Path(args.config),
            auto_groups=auto_groups,
            groups_override=args.groups_override,
            default_group=args.group_default,
        )
        sys.stdout.write(groups)
        return 0

    return 2


if __name__ == "__main__":
    raise SystemExit(main())
