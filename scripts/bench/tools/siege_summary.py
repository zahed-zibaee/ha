#!/usr/bin/env python3
"""Parse siege output into shell-friendly fields."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


FIELD_MAP = {
    "SIEGE_TRX": "transactions",
    "SIEGE_RATE": "transaction_rate",
    "SIEGE_RESP": "response_time",
    "SIEGE_AVAIL": "availability",
    "SIEGE_OK": "successful_transactions",
    "SIEGE_FAIL": "failed_transactions",
    "SIEGE_LONGEST": "longest_transaction",
    "SIEGE_SHORTEST": "shortest_transaction",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Parse siege output for bench scripts.")
    parser.add_argument("path")
    parser.add_argument("--json", action="store_true")
    return parser.parse_args()


def extract_value(text: str, key: str) -> str:
    match = re.search(rf'"{re.escape(key)}"\s*:\s*([0-9.]+)', text)
    return match.group(1) if match else ""


def main() -> int:
    args = parse_args()
    text = Path(args.path).read_text(encoding="utf-8", errors="replace")
    data = {env_key: extract_value(text, source_key) for env_key, source_key in FIELD_MAP.items()}

    if args.json:
        print(json.dumps(data, indent=2))
    else:
        for key, value in data.items():
            print(f"{key}={value}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
