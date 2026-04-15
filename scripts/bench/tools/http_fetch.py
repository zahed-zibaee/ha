#!/usr/bin/env python3
"""HTTP fetch helper for bench scripts."""

from __future__ import annotations

import argparse
import socket
import sys
import time
import urllib.error
import urllib.request


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Fetch a URL with retries for bench scripts.")
    parser.add_argument("url")
    parser.add_argument("--max-time", type=float, default=3.0)
    parser.add_argument("--retries", type=int, default=1)
    parser.add_argument("--delay", type=float, default=0.0)
    parser.add_argument("--success-status", type=int)
    parser.add_argument("--require-body", action="store_true")
    parser.add_argument("--append-meta", action="store_true")
    parser.add_argument("--silent-body", action="store_true")
    return parser.parse_args()


def fetch_once(url: str, timeout: float) -> tuple[str, int, float]:
    start = time.monotonic()
    body = ""
    status = 0
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            status = response.getcode() or 0
            raw = response.read()
            body = raw.decode("utf-8", errors="replace")
    except urllib.error.HTTPError as exc:
        status = exc.code or 0
        raw = exc.read()
        body = raw.decode("utf-8", errors="replace")
    except (urllib.error.URLError, TimeoutError, socket.timeout):
        status = 0
        body = ""
    elapsed = time.monotonic() - start
    return body, status, elapsed


def is_success(args: argparse.Namespace, body: str, status: int) -> bool:
    if args.success_status is not None and status != args.success_status:
        return False
    if args.require_body and not body:
        return False
    if args.success_status is None and not args.require_body:
        return status > 0
    return True


def render_output(body: str, status: int, elapsed: float, append_meta: bool, silent_body: bool) -> str:
    pieces: list[str] = []
    if not silent_body:
        pieces.append(body)
    if append_meta:
        pieces.append(f" code={status:03d} time={elapsed:.3f}s")
    return "".join(pieces)


def main() -> int:
    args = parse_args()
    retries = max(args.retries, 1)
    last_body = ""
    last_status = 0
    last_elapsed = 0.0

    for attempt in range(1, retries + 1):
        last_body, last_status, last_elapsed = fetch_once(args.url, args.max_time)
        if is_success(args, last_body, last_status):
            sys.stdout.write(
                render_output(
                    last_body,
                    last_status,
                    last_elapsed,
                    append_meta=args.append_meta,
                    silent_body=args.silent_body,
                )
            )
            return 0
        if attempt < retries and args.delay > 0:
            time.sleep(args.delay)

    sys.stdout.write(
        render_output(
            last_body,
            last_status,
            last_elapsed,
            append_meta=args.append_meta,
            silent_body=args.silent_body,
        )
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
