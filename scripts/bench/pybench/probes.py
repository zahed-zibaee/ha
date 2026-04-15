"""HTTP/API and metrics probe clients."""

from __future__ import annotations

import json
import socket
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any

from scripts.bench.tools.benchlib.parsing import (
    metrics_snapshot,
    parse_check_payload,
    parse_lb_payload,
    parse_leader_payload,
)
from scripts.bench.tools.benchlib.state import ReplicaInfo

from .models import RunConfig


@dataclass
class FetchResult:
    status: int
    body: str
    elapsed_ms: float


class ProbeClient:
    def __init__(self, config: RunConfig) -> None:
        self.config = config

    def _fetch_once(self, url: str) -> FetchResult:
        start = time.monotonic()
        status = 0
        body = ""
        try:
            with urllib.request.urlopen(url, timeout=self.config.endpoint_max_time) as response:
                status = response.getcode() or 0
                body = response.read().decode("utf-8", errors="replace")
        except urllib.error.HTTPError as exc:
            status = exc.code or 0
            body = exc.read().decode("utf-8", errors="replace")
        except (urllib.error.URLError, TimeoutError, socket.timeout):
            status = 0
            body = ""
        elapsed_ms = (time.monotonic() - start) * 1000.0
        return FetchResult(status=status, body=body, elapsed_ms=elapsed_ms)

    def fetch(self, url: str, expected_status: int | None = None, require_body: bool = False) -> FetchResult:
        attempts = max(1, self.config.endpoint_retries)
        last = FetchResult(status=0, body="", elapsed_ms=0.0)
        for index in range(attempts):
            last = self._fetch_once(url)
            ok_status = expected_status is None or last.status == expected_status
            ok_body = (not require_body) or bool(last.body.strip())
            if ok_status and ok_body:
                return last
            if index < attempts - 1:
                time.sleep(self.config.endpoint_delay)
        return last

    def wait_for_url(self, url: str, expected_status: int = 200, tries: int | None = None) -> bool:
        tries = tries if tries is not None else self.config.wait_url_tries
        for _ in range(max(1, tries)):
            res = self.fetch(url, expected_status=expected_status, require_body=False)
            if res.status == expected_status:
                return True
            time.sleep(self.config.wait_url_sleep)
        return False

    def wait_for_leader(self, replicas: list[ReplicaInfo], timeout_seconds: int | None = None) -> str:
        timeout_seconds = timeout_seconds if timeout_seconds is not None else self.config.wait_leader_timeout
        deadline = time.time() + max(1, timeout_seconds)
        while time.time() < deadline:
            for replica in replicas:
                leader = self.leader_info(replica.url)
                if leader.get("leader") == "true":
                    return replica.name
            time.sleep(1.0)
        return ""

    def wait_for_checks(self, replicas: list[ReplicaInfo], timeout_seconds: int | None = None) -> bool:
        timeout_seconds = timeout_seconds if timeout_seconds is not None else self.config.wait_checks_timeout
        deadline = time.time() + max(1, timeout_seconds)
        while time.time() < deadline:
            all_ok = True
            for replica in replicas:
                info = self.check_info(replica.url, self.config.default_group)
                total = int(info.get("total", "0") or "0")
                if total <= 0:
                    all_ok = False
                    break
            if all_ok:
                return True
            time.sleep(1.0)
        return False

    def leader_info(self, base_url: str) -> dict[str, str]:
        result = self.fetch(f"{base_url}/v1/leader", expected_status=200, require_body=True)
        return parse_leader_payload(result.body)

    def check_info(self, base_url: str, group: str) -> dict[str, str]:
        encoded = urllib.parse.quote(group, safe="")
        result = self.fetch(f"{base_url}/v1/check/{encoded}", expected_status=200, require_body=True)
        if result.status != 200:
            result = self.fetch(f"{base_url}/v1/check?group={encoded}", expected_status=200, require_body=True)
        return parse_check_payload(result.body)

    def lb_info(self, base_url: str, group: str) -> tuple[int, dict[str, str]]:
        encoded = urllib.parse.quote(group, safe="")
        result = self.fetch(f"{base_url}/v1/lb/{encoded}", require_body=True)
        if result.status != 200:
            result = self.fetch(f"{base_url}/v1/lb?group={encoded}", require_body=True)
        return result.status, parse_lb_payload(result.body)

    def health_ok(self, base_url: str) -> bool:
        result = self.fetch(f"{base_url}/health", expected_status=200, require_body=False)
        return result.status == 200

    def metrics_text(self, base_url: str) -> tuple[int, str]:
        result = self.fetch(f"{base_url}/metrics", expected_status=200, require_body=True)
        return result.status, result.body

    def metrics_snapshot(self, base_url: str) -> str:
        status, body = self.metrics_text(base_url)
        if status != 200:
            return ""
        return metrics_snapshot(body)

    def maybe_json(self, text: str) -> dict[str, Any]:
        try:
            data = json.loads(text)
            if isinstance(data, dict):
                return data
        except json.JSONDecodeError:
            pass
        return {}
