"""Parsing helpers for bench API and metrics payloads."""

from __future__ import annotations

import json
import re


def parse_json(text: str) -> object | None:
    text = text.strip()
    if not text:
        return None
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return None


def parse_leader_payload(text: str) -> dict[str, str]:
    data = parse_json(text)
    if isinstance(data, dict):
        return {
            "leader": "true" if bool(data.get("leader")) else "false",
            "probes_active": "true" if bool(data.get("probes_active")) else "false",
            "status": str(data.get("status", "")),
            "node_id": str(data.get("node_id", "")),
        }

    leader = re.search(r'"leader":(true|false)', text)
    probes = re.search(r'"probes_active":(true|false)', text)
    status = re.search(r'"status":"([^"]*)"', text)
    node_id = re.search(r'"node_id":"([^"]*)"', text)
    return {
        "leader": leader.group(1) if leader else "",
        "probes_active": probes.group(1) if probes else "",
        "status": status.group(1) if status else "",
        "node_id": node_id.group(1) if node_id else "",
    }


def _extract_targets(obj: object) -> list[dict]:
    if isinstance(obj, list):
        return [item for item in obj if isinstance(item, dict)]
    if isinstance(obj, dict):
        for key in ("targets", "results", "items"):
            value = obj.get(key)
            if isinstance(value, list):
                return [item for item in value if isinstance(item, dict)]
    return []


def parse_check_payload(text: str) -> dict[str, str]:
    data = parse_json(text)
    if data is not None:
        targets = _extract_targets(data)
        total = len(targets)
        reachable = sum(1 for item in targets if item.get("reachable") is True)
        redis_status = ""
        if isinstance(data, dict):
            redis_status = str(data.get("redis_status", ""))
        return {
            "total": str(total),
            "reachable": str(reachable),
            "redis_status": redis_status,
        }

    reachable_flags = re.findall(r'"reachable":(true|false)', text)
    redis = re.search(r'"redis_status":"([^"]*)"', text)
    reachable = sum(1 for item in reachable_flags if item == "true")
    return {
        "total": str(len(reachable_flags)),
        "reachable": str(reachable),
        "redis_status": redis.group(1) if redis else "",
    }


def parse_lb_payload(text: str) -> dict[str, str]:
    data = parse_json(text)
    if isinstance(data, dict):
        reachable = data.get("reachable")
        return {
            "name": str(data.get("name", "")),
            "group": str(data.get("group", "")),
            "reachable": "true" if reachable is True else "false" if reachable is False else "",
        }

    name = re.search(r'"name":"([^"]*)"', text)
    group = re.search(r'"group":"([^"]*)"', text)
    reachable = re.search(r'"reachable":(true|false)', text)
    return {
        "name": name.group(1) if name else "",
        "group": group.group(1) if group else "",
        "reachable": reachable.group(1) if reachable else "",
    }


def metrics_sum(text: str, metric: str, label_key: str = "", label_value: str = "") -> str:
    total = 0.0
    prefix = metric
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if not line.startswith(prefix):
            continue

        parts = line.split()
        if len(parts) < 2:
            continue
        metric_part, value_part = parts[0], parts[-1]
        if label_key and f'{label_key}="{label_value}"' not in metric_part:
            continue
        try:
            total += float(value_part)
        except ValueError:
            continue
    return f"{total:.6f}"


def metrics_snapshot(text: str) -> str:
    req_total = metrics_sum(text, "lb_requests_total")
    req_hit = metrics_sum(text, "lb_requests_total", "cache_hit", "true")
    req_miss = metrics_sum(text, "lb_requests_total", "cache_hit", "false")
    err_total = metrics_sum(text, "lb_errors_total")
    check_total = metrics_sum(text, "check_requests_total")
    check_targets = metrics_sum(text, "check_targets_total")
    probe_runs = metrics_sum(text, "probe_runs_total")
    return "|".join([req_total, req_hit, req_miss, err_total, check_total, check_targets, probe_runs])
