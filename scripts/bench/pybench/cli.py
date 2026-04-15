"""CLI entrypoint for python-first bench harness."""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path

from .analyze import main as analyze_main
from .massive import DEFAULT_TESTS, run_massive
from .models import RunConfig
from .scenarios import SCENARIOS, run_scenario


def bool_env(name: str, default: bool) -> bool:
    value = os.environ.get(name)
    if value is None:
        return default
    return value.lower() == "true"


def build_config(repo_root: Path, args: argparse.Namespace) -> RunConfig:
    profile = os.environ.get("PYBENCH_PROFILE", "pragmatic").strip().lower()
    if profile not in {"pragmatic", "strict"}:
        profile = "pragmatic"
    strict_mode = profile == "strict" or bool_env("PYBENCH_STRICT", False)
    return RunConfig(
        root_dir=repo_root,
        out_dir=repo_root / "tmp" / "ha-bench",
        compose_file=Path(os.environ.get("COMPOSE_FILE", repo_root / "scripts/bench/docker-compose.test.yml")),
        config_path=Path(os.environ.get("BENCH_CONFIG", repo_root / "scripts/bench/config-targets.test.yaml")),
        default_group=os.environ.get("GROUP", "web-health"),
        groups_override=os.environ.get("GROUPS", ""),
        publish_bind=os.environ.get("BENCH_PUBLISH_BIND", "127.0.0.1"),
        internal_port=int(os.environ.get("BENCH_INTERNAL_PORT", "8080")),
        url_mode=os.environ.get("BENCH_REPLICA_URL_MODE", "auto"),
        endpoint_retries=int(os.environ.get("BENCH_ENDPOINT_FETCH_RETRIES", "2")),
        endpoint_delay=float(os.environ.get("BENCH_ENDPOINT_FETCH_DELAY", "0.5")),
        endpoint_max_time=float(os.environ.get("BENCH_ENDPOINT_MAX_TIME", "3.0")),
        require_all_replicas_health=bool_env("BENCH_REQUIRE_ALL_REPLICAS_HEALTH", False) or strict_mode,
        wait_url_tries=int(os.environ.get("BENCH_WAIT_URL_TRIES", "45")),
        wait_url_sleep=float(os.environ.get("BENCH_WAIT_URL_SLEEP", "2.0")),
        wait_leader_timeout=int(os.environ.get("WAIT_LEADER_TIMEOUT", "60")),
        wait_checks_timeout=int(os.environ.get("WAIT_CHECKS_TIMEOUT", "60")),
        wait_replica_timeout=int(os.environ.get("WAIT_REPLICA_TIMEOUT", "60")),
        auto_groups=bool_env("BENCH_AUTO_GROUPS", True),
        force_recreate=bool_env("BENCH_FORCE_RECREATE", False),
        verbose=bool_env("PYBENCH_VERBOSE", False),
        random_seed=int(os.environ.get("PYBENCH_RANDOM_SEED", "7")),
        keep_reports=bool_env("PYBENCH_KEEP_REPORTS", True),
        print_summary=bool_env("PYBENCH_PRINT_SUMMARY", True),
        profile="strict" if strict_mode else profile,
        fail_on_warn=bool_env("PYBENCH_FAIL_ON_WARN", False) or strict_mode,
    )


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Python-first benchmark harness.")
    sub = parser.add_subparsers(dest="command", required=True)

    run_parser = sub.add_parser("run", help="Run one scenario")
    run_parser.add_argument("--scenario", required=True, choices=sorted(SCENARIOS.keys()))
    run_parser.add_argument("--report-dir", help="Write scenario report to this directory")

    massive_parser = sub.add_parser("massive", help="Run the full suite")
    massive_parser.add_argument(
        "--tests",
        default="",
        help=f"Space-separated scenarios (default MASSIVE_TESTS or {DEFAULT_TESTS})",
    )

    sub.add_parser("list", help="List available scenarios")

    analyze_parser = sub.add_parser("analyze", help="Analyze pybench report outputs")
    analyze_parser.add_argument("target")
    analyze_parser.add_argument("--json", action="store_true")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    args = parse_args(argv)
    repo_root = Path(__file__).resolve().parents[3]
    config = build_config(repo_root, args)

    if args.command == "list":
        for scenario in sorted(SCENARIOS):
            print(scenario)
        return 0

    if args.command == "analyze":
        forward = [args.target]
        if args.json:
            forward.append("--json")
        return analyze_main(forward)

    if args.command == "run":
        report_dir = Path(args.report_dir) if args.report_dir else (repo_root / "tmp" / "ha-bench" / args.scenario)
        result = run_scenario(args.scenario, config, report_dir)
        print((report_dir / "summary.txt").read_text(encoding="utf-8"))
        return 0 if result.status == "PASS" else 1

    if args.command == "massive":
        tests = args.tests.split() if args.tests.strip() else None
        _, summary = run_massive(config, tests=tests)
        return 0 if summary.failed == 0 else 1

    raise RuntimeError(f"unsupported command {args.command}")


if __name__ == "__main__":
    raise SystemExit(main())
