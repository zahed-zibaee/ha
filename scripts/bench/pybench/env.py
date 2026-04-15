"""Runtime environment and docker-compose helpers."""

from __future__ import annotations

import os
import random
import shutil
import subprocess
import time
from pathlib import Path

from scripts.bench.tools.benchlib.state import ReplicaInfo, collect_groups, discover_replicas

from .models import RunConfig


def detect_compose_command() -> list[str]:
    if shutil.which("docker-compose"):
        return ["docker-compose"]
    if shutil.which("docker"):
        check = subprocess.run(["docker", "compose", "version"], capture_output=True, text=True, check=False)
        if check.returncode == 0:
            return ["docker", "compose"]
    raise RuntimeError("docker compose not found")


class BenchEnvironment:
    def __init__(self, config: RunConfig) -> None:
        self.config = config
        self._compose_cmd = detect_compose_command()
        self._replica_id_map: dict[str, str] = {}

    @property
    def compose_cmd(self) -> list[str]:
        return self._compose_cmd

    def _compose(self, *args: str) -> subprocess.CompletedProcess[str]:
        cmd = [*self._compose_cmd, *self._compose_env_flags(), *args]
        return subprocess.run(cmd, cwd=self.config.root_dir, capture_output=True, text=True, check=False)

    def _compose_env_flags(self) -> list[str]:
        return ["-f", str(self.config.compose_file)]

    def ensure_out_dir(self) -> None:
        self.config.out_dir.mkdir(parents=True, exist_ok=True)

    def compose_up(self) -> None:
        args = ["up", "-d"]
        if self.config.force_recreate:
            args.extend(["--force-recreate", "--renew-anon-volumes"])
        result = self._compose(*args)
        if result.returncode != 0:
            raise RuntimeError(f"compose up failed: {result.stderr.strip()}")

    def compose_down(self, remove_volumes: bool = False) -> None:
        args = ["down"]
        if remove_volumes:
            args.append("-v")
        result = self._compose(*args)
        if result.returncode != 0:
            raise RuntimeError(f"compose down failed: {result.stderr.strip()}")

    def compose_ps(self) -> str:
        result = self._compose("ps")
        return result.stdout + ("\n" + result.stderr if result.stderr.strip() else "")

    def discover_replicas(self) -> list[ReplicaInfo]:
        replicas = discover_replicas(
            self.config.root_dir,
            internal_port=self.config.internal_port,
            publish_bind=self.config.publish_bind,
            mode=self.config.url_mode,
            compose_file=self.config.compose_file,
        )
        self._replica_id_map = {item.name: item.container_id for item in replicas if item.name and item.container_id}
        return replicas

    def collect_groups(self) -> list[str]:
        groups_text = collect_groups(
            config_path=self.config.config_path,
            auto_groups=self.config.auto_groups,
            groups_override=self.config.groups_override,
            default_group=self.config.default_group,
        )
        return [item for item in groups_text.split(",") if item]

    def running_replica_names(self) -> set[str]:
        result = self._compose("ps", "--services", "--status", "running")
        if result.returncode != 0:
            return set()
        return {line.strip() for line in result.stdout.splitlines() if line.strip()}

    def stop_replica(self, name: str) -> None:
        key = name.strip()
        identifier = self._replica_id_map.get(key, key)
        result = subprocess.run(["docker", "stop", identifier], capture_output=True, text=True, check=False)
        if result.returncode != 0:
            raise RuntimeError(f"failed to stop {name}: {result.stderr.strip()}")

    def start_replica(self, name: str) -> None:
        key = name.strip()
        identifier = self._replica_id_map.get(key, key)
        result = subprocess.run(["docker", "start", identifier], capture_output=True, text=True, check=False)
        if result.returncode != 0:
            raise RuntimeError(f"failed to start {name}: {result.stderr.strip()}")

    def stop_redis(self) -> None:
        result = self._compose("stop", "redis")
        if result.returncode != 0:
            raise RuntimeError(f"failed to stop redis: {result.stderr.strip()}")

    def start_redis(self) -> None:
        result = self._compose("start", "redis")
        if result.returncode != 0:
            raise RuntimeError(f"failed to start redis: {result.stderr.strip()}")

    def wait_seconds(self, seconds: float) -> None:
        if seconds > 0:
            time.sleep(seconds)

    def choose_replica(self, replicas: list[ReplicaInfo], exclude: set[str] | None = None) -> ReplicaInfo:
        exclude = exclude or set()
        candidates = [item for item in replicas if item.name not in exclude]
        if not candidates:
            raise RuntimeError("no replicas available for selection")
        random.seed(self.config.random_seed + int(time.time()))
        return random.choice(candidates)

    def effective_env(self) -> dict[str, str]:
        env = os.environ.copy()
        env["COMPOSE_FILE"] = str(self.config.compose_file)
        env["BENCH_CONFIG"] = str(self.config.config_path)
        return env
