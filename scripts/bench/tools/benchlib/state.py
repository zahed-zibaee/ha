"""Replica discovery and config helpers for bench scripts."""

from __future__ import annotations

from dataclasses import dataclass
import re
import shutil
import socket
import subprocess
from pathlib import Path
import urllib.error
import urllib.request


@dataclass
class ReplicaInfo:
    name: str
    ip: str
    url: str
    container_id: str = ""


def detect_compose_command() -> list[str]:
    if shutil.which("docker-compose"):
        return ["docker-compose"]
    if shutil.which("docker"):
        check = subprocess.run(["docker", "compose", "version"], capture_output=True, text=True)
        if check.returncode == 0:
            return ["docker", "compose"]
    raise RuntimeError("docker compose not found")


def run_command(args: list[str], cwd: Path) -> str:
    completed = subprocess.run(args, cwd=cwd, capture_output=True, text=True, check=False)
    return completed.stdout


def http_healthy(url: str, timeout: float = 2.0) -> bool:
    try:
        with urllib.request.urlopen(f"{url}/health", timeout=timeout) as response:
            response.read()
            return (response.getcode() or 0) == 200
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, socket.timeout):
        return False


def inspect_value(container_id: str, template: str) -> str:
    completed = subprocess.run(
        ["docker", "inspect", "-f", template, container_id],
        capture_output=True,
        text=True,
        check=False,
    )
    return completed.stdout.strip()


def docker_port(container_id: str, internal_port: int) -> str:
    completed = subprocess.run(
        ["docker", "port", container_id, f"{internal_port}/tcp"],
        capture_output=True,
        text=True,
        check=False,
    )
    return completed.stdout.strip().splitlines()[0] if completed.stdout.strip() else ""


def select_replica_url(
    container_id: str,
    internal_port: int,
    publish_bind: str,
    mode: str,
) -> tuple[str, str]:
    ip = inspect_value(container_id, "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}")
    bridge_url = f"http://{ip}:{internal_port}" if ip else ""

    port_line = docker_port(container_id, internal_port)
    host_url = ""
    port_match = re.search(r":(\d+)$", port_line)
    if port_match:
        host_url = f"http://{publish_bind}:{port_match.group(1)}"

    if mode == "bridge":
        return ip, bridge_url
    if mode == "host":
        return ip, host_url or bridge_url

    if host_url and http_healthy(host_url):
        return ip, host_url
    if bridge_url and http_healthy(bridge_url):
        return ip, bridge_url
    return ip, host_url or bridge_url


def discover_replicas(
    root_dir: Path,
    internal_port: int,
    publish_bind: str,
    mode: str,
    compose_file: Path | None = None,
) -> list[ReplicaInfo]:
    compose_cmd = detect_compose_command()
    compose_args: list[str] = [*compose_cmd]
    if compose_file is not None:
        compose_args.extend(["-f", str(compose_file)])
    ids_text = run_command([*compose_args, "ps", "-q", "ha"], cwd=root_dir)
    ids = [line.strip() for line in ids_text.splitlines() if line.strip()]
    replicas: list[ReplicaInfo] = []
    for container_id in ids:
        name = inspect_value(container_id, "{{.Name}}").lstrip("/")
        ip, url = select_replica_url(container_id, internal_port, publish_bind, mode)
        if name and url:
            replicas.append(ReplicaInfo(name=name, ip=ip, url=url, container_id=container_id))
    return replicas


def collect_groups(config_path: Path, auto_groups: bool, groups_override: str, default_group: str) -> str:
    if not auto_groups:
        return groups_override or default_group
    if not config_path.exists():
        return groups_override or default_group

    groups: list[str] = []
    in_checks = False
    for raw_line in config_path.read_text(encoding="utf-8", errors="replace").splitlines():
        if raw_line.startswith("checks:"):
            in_checks = True
            continue
        if in_checks and re.match(r"^[^\s]", raw_line):
            in_checks = False
            continue
        if in_checks:
            match = re.match(r"^\s{2}([^:#\s][^:]*):\s*$", raw_line)
            if match:
                groups.append(match.group(1))

    if not groups:
        return groups_override or default_group

    if groups_override:
        requested = [item for item in groups_override.split(",") if item]
        filtered = [item for item in requested if item in groups]
        if filtered:
            return ",".join(filtered)
    return ",".join(groups)
