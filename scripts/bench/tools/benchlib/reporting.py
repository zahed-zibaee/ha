"""Shared parsing helpers for bench report files."""

from __future__ import annotations

from dataclasses import dataclass
import re


OVERALL_RE = re.compile(r"^overall:\s+(?P<overall>\S+)")
CHECKS_RE = re.compile(r"^checks:\s+pass=(?P<pass>\d+)\s+fail=(?P<fail>\d+)\s+warn=(?P<warn>\d+)\s+other=(?P<other>\d+)")


@dataclass
class SummaryInfo:
    summary_text: str
    overall: str = "unknown"
    checks_line: str = ""
    failure_lines: list[str] | None = None

    def __post_init__(self) -> None:
        if self.failure_lines is None:
            self.failure_lines = []


def extract_summary_block(text: str) -> str:
    marker = "Benchmark Summary"
    idx = text.rfind(marker)
    if idx == -1:
        return ""
    return text[idx:].strip()


def parse_summary_block(summary_text: str) -> SummaryInfo:
    info = SummaryInfo(summary_text=summary_text, failure_lines=[])
    if not summary_text:
        return info

    in_failures = False
    for raw_line in summary_text.splitlines():
        line = raw_line.strip()
        if not line:
            continue

        match = OVERALL_RE.match(line)
        if match:
            info.overall = match.group("overall")
            continue

        if CHECKS_RE.match(line):
            info.checks_line = line
            continue

        if line == "Failures":
            in_failures = True
            continue

        if line.startswith("TEST_END"):
            break

        if in_failures and line.startswith("- "):
            info.failure_lines.append(line[2:])

    return info


def format_duration(total_seconds: int) -> str:
    total = max(int(total_seconds), 0)
    hours = total // 3600
    minutes = (total % 3600) // 60
    seconds = total % 60
    if hours > 0:
        return f"{hours}h{minutes:02d}m{seconds:02d}s"
    if minutes > 0:
        return f"{minutes}m{seconds:02d}s"
    return f"{seconds}s"
