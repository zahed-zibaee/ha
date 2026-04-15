#!/usr/bin/env python3
"""Compatibility wrapper that forwards to pybench massive."""

from __future__ import annotations

import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from scripts.bench.pybench.cli import main


if __name__ == "__main__":
    raise SystemExit(main(["massive", *sys.argv[1:]]))
