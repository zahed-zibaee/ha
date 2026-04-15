"""Assertion helpers for pybench scenarios."""

from __future__ import annotations

from .models import CheckResult


def pass_check(name: str, detail: str = "", section: str = "General") -> CheckResult:
    return CheckResult(name=name, status="PASS", detail=detail, section=section)


def fail_check(name: str, detail: str = "", section: str = "General") -> CheckResult:
    return CheckResult(name=name, status="FAIL", detail=detail, section=section)


def warn_check(name: str, detail: str = "", section: str = "General") -> CheckResult:
    return CheckResult(name=name, status="WARN", detail=detail, section=section)


def bool_check(name: str, ok: bool, detail: str = "", section: str = "General") -> CheckResult:
    if ok:
        return pass_check(name, detail, section)
    return fail_check(name, detail, section)
