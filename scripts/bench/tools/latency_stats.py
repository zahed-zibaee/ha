#!/usr/bin/env python3
"""Latency sampling helper for bench scripts."""

from __future__ import annotations

import argparse
import math
import socket
import statistics
import sys
import time
import urllib.error
import urllib.request


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Collect latency stats for repeated HTTP requests.")
    parser.add_argument("url")
    parser.add_argument("--samples", type=int, default=200)
    parser.add_argument("--max-time", type=float, default=2.0)
    return parser.parse_args()


def fetch_status(url: str, timeout: float) -> tuple[int, float]:
    start = time.monotonic()
    status = 0
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            status = response.getcode() or 0
            response.read()
    except urllib.error.HTTPError as exc:
        status = exc.code or 0
        exc.read()
    except (urllib.error.URLError, TimeoutError, socket.timeout):
        status = 0
    elapsed_ms = (time.monotonic() - start) * 1000.0
    return status, elapsed_ms


def percentile(values: list[float], pct: int) -> float:
    if not values:
        return math.nan
    idx = math.ceil(len(values) * pct / 100) - 1
    idx = max(0, min(idx, len(values) - 1))
    return values[idx]


def fmt_number(value: float) -> str:
    if math.isnan(value):
        return "n/a"
    return f"{value:.1f}"


def main() -> int:
    args = parse_args()
    samples = max(args.samples, 0)
    ok_latencies: list[float] = []
    fail = 0

    for _ in range(samples):
        status, elapsed_ms = fetch_status(args.url, args.max_time)
        if status == 200:
            ok_latencies.append(elapsed_ms)
        else:
            fail += 1

    ok = len(ok_latencies)
    total = ok + fail
    err_pct = 100.0 if total == 0 else (fail / total) * 100.0

    if ok == 0:
        sys.stdout.write(f"{ok}|{fail}|{total}|{err_pct:.1f}|n/a|n/a|n/a|n/a")
        return 0

    ok_latencies.sort()
    avg = statistics.fmean(ok_latencies)
    p95 = percentile(ok_latencies, 95)
    p99 = percentile(ok_latencies, 99)
    max_v = ok_latencies[-1]
    sys.stdout.write(
        f"{ok}|{fail}|{total}|{err_pct:.1f}|{fmt_number(avg)}|{fmt_number(p95)}|{fmt_number(p99)}|{fmt_number(max_v)}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
