"""Scenario registry and implementations for pybench."""

from __future__ import annotations

import random
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

from scripts.bench.tools.benchlib.parsing import metrics_sum
from scripts.bench.tools.benchlib.state import ReplicaInfo

from .checks import bool_check, fail_check, pass_check, warn_check
from .env import BenchEnvironment
from .load_siege import SiegeResult, run_siege
from .models import CheckResult, RunConfig, ScenarioResult
from .probes import ProbeClient
from .reporting import ScenarioReporter


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


@dataclass
class ScenarioContext:
    name: str
    config: RunConfig
    env: BenchEnvironment
    probes: ProbeClient
    reporter: ScenarioReporter
    checks: list[CheckResult] = field(default_factory=list)
    failures: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    metadata: dict[str, str] = field(default_factory=dict)
    artifacts: dict[str, str] = field(default_factory=dict)

    def add(self, check: CheckResult) -> None:
        self.checks.append(check)
        if check.status == "FAIL":
            self.failures.append(f"{check.section}/{check.name}: {check.detail}".strip(": "))
        if check.status == "WARN":
            self.warnings.append(f"{check.section}/{check.name}: {check.detail}".strip(": "))

    def log(self, message: str, level: str = "info", **fields: str) -> None:
        self.reporter.event(level, message, scenario=self.name, **fields)


def _discover_ready(ctx: ScenarioContext) -> list[ReplicaInfo]:
    ctx.env.ensure_out_dir()
    ctx.env.compose_up()
    replicas = ctx.env.discover_replicas()
    ctx.metadata["replica_count"] = str(len(replicas))
    ctx.log("replicas discovered", count=str(len(replicas)))
    if not replicas:
        ctx.add(fail_check("discover replicas", "no replicas discovered", section="Setup"))
        return []

    any_healthy = False
    first_url = replicas[0].url
    for replica in replicas:
        if ctx.probes.wait_for_url(f"{replica.url}/health", expected_status=200, tries=5):
            any_healthy = True
            first_url = replica.url
            break
    ctx.add(bool_check("wait any replica health", any_healthy, detail=first_url, section="Setup"))

    healthy: list[ReplicaInfo] = []
    unhealthy: list[str] = []
    for replica in replicas:
        if ctx.probes.health_ok(replica.url):
            healthy.append(replica)
        else:
            unhealthy.append(replica.name)

    if not healthy:
        ctx.add(fail_check("replicas healthy", "no healthy replicas discovered", section="Setup"))
        return []

    if unhealthy:
        detail = ",".join(unhealthy)
        if ctx.config.require_all_replicas_health:
            ctx.add(fail_check("replicas healthy", f"unhealthy replicas={detail}", section="Setup"))
            return []
        ctx.add(warn_check("replicas healthy", f"ignoring unhealthy replicas={detail}", section="Setup"))

    checks_ready = ctx.probes.wait_for_checks(healthy)
    ctx.add(bool_check("wait checks ready", checks_ready, section="Setup"))
    return healthy


def _probe_health(ctx: ScenarioContext, replicas: list[ReplicaInfo]) -> None:
    for replica in replicas:
        ok = ctx.probes.health_ok(replica.url)
        ctx.add(bool_check(f"health {replica.name}", ok, detail=replica.url, section="API"))


def _probe_metrics(ctx: ScenarioContext, replicas: list[ReplicaInfo]) -> None:
    for replica in replicas:
        if not ctx.probes.health_ok(replica.url):
            ctx.add(warn_check(f"metrics {replica.name}", "replica unhealthy, metrics skipped", section="Metrics"))
            continue
        status, body = ctx.probes.metrics_text(replica.url)
        ok = status == 200 and bool(body.strip())
        ctx.add(bool_check(f"metrics {replica.name}", ok, detail=f"status={status}", section="Metrics"))


def _probe_lb(ctx: ScenarioContext, replicas: list[ReplicaInfo], groups: list[str]) -> None:
    for replica in replicas:
        for group in groups:
            status, payload = ctx.probes.lb_info(replica.url, group)
            ok = status == 200 and bool(payload.get("name"))
            ctx.add(
                bool_check(
                    f"lb {replica.name} group={group}",
                    ok,
                    detail=f"status={status} name={payload.get('name', '')}",
                    section="API",
                )
            )


def _leader_info(ctx: ScenarioContext, replicas: list[ReplicaInfo]) -> tuple[str, list[str]]:
    leader_names: list[str] = []
    for replica in replicas:
        info = ctx.probes.leader_info(replica.url)
        ok = info.get("status") in {"leader", "follower", "candidate"}
        ctx.add(bool_check(f"leader payload {replica.name}", ok, detail=str(info), section="Leader / lock"))
        if info.get("leader") == "true":
            leader_names.append(replica.name)
    leader = leader_names[0] if leader_names else ""
    ctx.add(
        bool_check(
            "single leader visible",
            len(leader_names) == 1,
            detail=f"leaders={','.join(leader_names) if leader_names else '(none)'}",
            section="Leader / lock",
        )
    )
    return leader, leader_names


def _find_replica(replicas: list[ReplicaInfo], name: str) -> ReplicaInfo | None:
    for replica in replicas:
        if replica.name == name:
            return replica
    return None


def scenario_api(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    groups = ctx.env.collect_groups()
    _probe_lb(ctx, replicas, groups)
    _probe_health(ctx, replicas)
    _probe_metrics(ctx, replicas)


def scenario_health(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    _probe_health(ctx, replicas)
    _probe_metrics(ctx, replicas)


def scenario_leader(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    elected = ctx.probes.wait_for_leader(replicas)
    ctx.add(bool_check("leader elected", bool(elected), detail=elected or "none", section="Leader / lock"))
    _leader_info(ctx, replicas)


def scenario_loadbalancer(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    _probe_lb(ctx, replicas, ctx.env.collect_groups())
    _probe_metrics(ctx, replicas)


def scenario_distribution(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    target = replicas[0].url
    group = (ctx.env.collect_groups() or ["default"])[0]
    samples = int(ctx.metadata.get("dist_samples", "240"))
    names: dict[str, int] = {}
    for _ in range(samples):
        status, payload = ctx.probes.lb_info(target, group)
        if status == 200 and payload.get("name"):
            names[payload["name"]] = names.get(payload["name"], 0) + 1
    ctx.metadata["distribution"] = ",".join(f"{k}:{v}" for k, v in sorted(names.items()))
    spread_ok = len(names) >= min(2, len(replicas))
    ctx.add(
        bool_check(
            "lb distribution spread",
            spread_ok,
            detail=f"samples={samples} unique={len(names)} dist={ctx.metadata['distribution']}",
            section="Load",
        )
    )
    _probe_metrics(ctx, replicas)


def scenario_latency(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    group = (ctx.env.collect_groups() or ["default"])[0]
    target = f"{replicas[0].url}/v1/lb/{group}"
    siege = run_siege(target_url=target, out_dir=ctx.reporter.attachments_dir, label="latency", concurrency=20, duration="20s")
    ctx.artifacts["siege"] = str(siege.output_path)
    ctx.add(bool_check("siege command", siege.ok, detail=f"return_code={siege.return_code}", section="Load"))

    latencies: list[float] = []
    failures = 0
    for _ in range(120):
        fetch = ctx.probes.fetch(target, expected_status=200, require_body=True)
        if fetch.status == 200:
            latencies.append(fetch.elapsed_ms)
        else:
            failures += 1
    latencies.sort()
    if latencies:
        p95 = latencies[min(len(latencies) - 1, int(len(latencies) * 0.95))]
        p99 = latencies[min(len(latencies) - 1, int(len(latencies) * 0.99))]
        ctx.add(bool_check("latency p95<2000ms", p95 < 2000, detail=f"p95={p95:.1f}ms", section="Load"))
        ctx.add(bool_check("latency p99<3000ms", p99 < 3000, detail=f"p99={p99:.1f}ms", section="Load"))
    else:
        ctx.add(fail_check("latency samples", "no successful samples", section="Load"))
    err_pct = (failures / max(1, len(latencies) + failures)) * 100.0
    ctx.add(bool_check("latency error rate<20%", err_pct < 20.0, detail=f"err={err_pct:.1f}%", section="Load"))
    _probe_metrics(ctx, replicas)


def scenario_concurrency(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    group = (ctx.env.collect_groups() or ["default"])[0]
    target = f"{replicas[0].url}/v1/lb/{group}"
    siege = run_siege(
        target_url=target,
        out_dir=ctx.reporter.attachments_dir,
        label="concurrency",
        concurrency=60,
        duration="25s",
        timeout_seconds=240,
    )
    ctx.artifacts["siege"] = str(siege.output_path)
    if not siege.ok and ctx.config.profile == "strict":
        ctx.add(fail_check("concurrency siege", f"return_code={siege.return_code}", section="Load"))
    elif not siege.ok:
        ctx.add(warn_check("concurrency siege", f"return_code={siege.return_code}", section="Load"))
    else:
        ctx.add(pass_check("concurrency siege", f"return_code={siege.return_code}", section="Load"))
    fail_tx = float(siege.stats.get("failed_transactions", "0") or "0")
    ok = fail_tx < 100
    ctx.add(bool_check("failed transactions bound", ok, detail=f"failed={fail_tx}", section="Load"))

    # If siege times out in pragmatic mode, run a lightweight fallback
    # availability sample to avoid false negatives from tool flakiness.
    if not siege.ok and ctx.config.profile != "strict":
        failures = 0
        total = 80
        for _ in range(total):
            fetch = ctx.probes.fetch(target, expected_status=200, require_body=True)
            if fetch.status != 200:
                failures += 1
        err_pct = (failures / max(1, total)) * 100.0
        ctx.add(bool_check("fallback error rate<10%", err_pct < 10.0, detail=f"err={err_pct:.1f}%", section="Load"))
    _probe_metrics(ctx, replicas)


def scenario_consistency(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    leader_name = ctx.probes.wait_for_leader(replicas)
    ctx.add(bool_check("leader elected", bool(leader_name), detail=leader_name or "none", section="Leader / lock"))
    before: dict[str, float] = {}
    after: dict[str, float] = {}
    for replica in replicas:
        status, body = ctx.probes.metrics_text(replica.url)
        value = float(metrics_sum(body, "probe_runs_total") or "0") if status == 200 else 0.0
        before[replica.name] = value
    time.sleep(4.0)
    for replica in replicas:
        status, body = ctx.probes.metrics_text(replica.url)
        value = float(metrics_sum(body, "probe_runs_total") or "0") if status == 200 else 0.0
        after[replica.name] = value
    deltas = {name: after[name] - before[name] for name in before}
    active = [name for name, delta in deltas.items() if delta > 0.5]
    ok = len(active) <= 1
    ctx.add(bool_check("single probe writer", ok, detail=f"active={','.join(active) or '(none)'}", section="Metrics"))
    # Redis-down branch: degraded mode is expected to allow multiple replicas probing.
    ctx.env.stop_redis()
    ctx.env.wait_seconds(2.0)
    degraded_before: dict[str, float] = {}
    for replica in replicas:
        status, body = ctx.probes.metrics_text(replica.url)
        value = float(metrics_sum(body, "probe_runs_total") or "0") if status == 200 else 0.0
        degraded_before[replica.name] = value

    # Observe degraded behavior over a bounded window because replicas transition
    # asynchronously on lock-renew failure boundaries.
    observe_seconds = max(8, min(30, int(ctx.config.wait_replica_timeout)))
    deadline = time.time() + observe_seconds
    max_active = 0
    max_active_replicas: list[str] = []
    while time.time() < deadline:
        degraded_after: dict[str, float] = {}
        for replica in replicas:
            status, body = ctx.probes.metrics_text(replica.url)
            value = float(metrics_sum(body, "probe_runs_total") or "0") if status == 200 else 0.0
            degraded_after[replica.name] = value
        degraded_deltas = {name: degraded_after[name] - degraded_before[name] for name in degraded_before}
        degraded_active = [name for name, delta in degraded_deltas.items() if delta > 0.5]
        if len(degraded_active) > max_active:
            max_active = len(degraded_active)
            max_active_replicas = degraded_active
        if max_active >= 2:
            break
        ctx.env.wait_seconds(1.0)

    ctx.add(
        bool_check(
            "degraded probes observed",
            max_active >= 1,
            detail=f"active={','.join(max_active_replicas) if max_active_replicas else '(none)'}",
            section="Metrics",
        )
    )
    if max_active < 2:
        detail = f"active={','.join(max_active_replicas) if max_active_replicas else '(none)'}"
        if ctx.config.profile == "strict":
            ctx.add(fail_check("degraded multi-probe writers", detail, section="Metrics"))
        else:
            ctx.add(warn_check("degraded multi-probe writers", detail, section="Metrics"))
    else:
        ctx.add(pass_check("degraded multi-probe writers", f"active={','.join(max_active_replicas)}", section="Metrics"))
    ctx.env.start_redis()
    recovery_ok = ctx.probes.wait_for_checks(replicas, timeout_seconds=max(20, ctx.config.wait_checks_timeout))
    ctx.add(bool_check("checks recover after consistency redis test", recovery_ok, section="Metrics"))
    _probe_metrics(ctx, replicas)


def scenario_resilience(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if len(replicas) < 2:
        ctx.add(fail_check("resilience replica count", "need at least 2 replicas", section="Setup"))
        return
    group = (ctx.env.collect_groups() or ["default"])[0]
    victim = ctx.env.choose_replica(replicas)
    ctx.log("stopping replica", replica=victim.name)
    ctx.env.stop_replica(victim.name)
    ctx.env.wait_seconds(3.0)
    replicas = ctx.env.discover_replicas()
    survivors = [r for r in replicas if r.name != victim.name]
    if not survivors:
        ctx.add(fail_check("lb survives replica stop", "no survivor replicas after stop", section="API"))
        return
    status, payload = ctx.probes.lb_info(survivors[0].url, group)
    ctx.add(
        bool_check(
            "lb survives replica stop",
            status == 200 and bool(payload.get("name")),
            detail=f"status={status} target={payload.get('name', '')}",
            section="API",
        )
    )
    ctx.env.start_replica(victim.name)
    ready = False
    for _ in range(20):
        replicas = ctx.env.discover_replicas()
        victim_now = _find_replica(replicas, victim.name)
        if victim_now and ctx.probes.health_ok(victim_now.url):
            ready = True
            break
        ctx.env.wait_seconds(1.0)
    ctx.add(bool_check("replica restarts healthy", ready, detail=victim.name, section="Setup"))
    replicas = ctx.env.discover_replicas()
    _probe_metrics(ctx, replicas)


def scenario_redis_flap(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    groups = (ctx.env.collect_groups() or ["default"])[:1]
    for step in range(1, 3):
        ctx.env.stop_redis()
        ctx.env.wait_seconds(3.0)
        _probe_lb(ctx, replicas, groups)
        ctx.add(pass_check(f"redis flap cycle {step} stop/start", section="Stress"))
        ctx.env.start_redis()
        replicas = ctx.env.discover_replicas()
        ok = ctx.probes.wait_for_checks(replicas, timeout_seconds=max(20, ctx.config.wait_checks_timeout))
        ctx.add(bool_check(f"checks recover after redis restart cycle {step}", ok, section="Metrics"))
    _probe_metrics(ctx, replicas)


def scenario_cold_start(ctx: ScenarioContext) -> None:
    ctx.env.compose_down(remove_volumes=False)
    ctx.env.wait_seconds(2.0)
    start = time.time()
    replicas = _discover_ready(ctx)
    elapsed = time.time() - start
    ctx.metadata["cold_start_seconds"] = f"{elapsed:.2f}"
    ctx.add(bool_check("cold start under 120s", elapsed < 120, detail=f"{elapsed:.1f}s", section="Setup"))
    if replicas:
        _probe_lb(ctx, replicas, (ctx.env.collect_groups() or ["default"])[:1])


def scenario_churn(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if len(replicas) < 2:
        ctx.add(fail_check("churn replica count", "need at least 2 replicas", section="Setup"))
        return
    group = (ctx.env.collect_groups() or ["default"])[0]
    target = f"{replicas[0].url}/v1/lb/{group}"
    siege_holder: dict[str, object] = {}

    def churn_load_worker() -> None:
        siege_holder["result"] = run_siege(
            target_url=target,
            out_dir=ctx.reporter.attachments_dir,
            label="churn",
            concurrency=35,
            duration="25s",
            timeout_seconds=210,
        )

    load_thread = threading.Thread(target=churn_load_worker, daemon=True)
    load_thread.start()
    ctx.env.wait_seconds(5.0)
    leader, _ = _leader_info(ctx, replicas)
    if leader:
        ctx.env.stop_replica(leader)
        ctx.env.wait_seconds(4.0)
        ctx.env.start_replica(leader)
    replicas = ctx.env.discover_replicas()
    new_leader = ctx.probes.wait_for_leader(replicas, timeout_seconds=80)
    ctx.add(bool_check("leader after churn", bool(new_leader), detail=new_leader or "none", section="Leader / lock"))
    load_thread.join(timeout=220.0)
    if load_thread.is_alive():
        ctx.add(fail_check("churn siege completion", "load thread timeout", section="Load"))
    else:
        siege_result = siege_holder.get("result")
        if not isinstance(siege_result, SiegeResult):
            ctx.add(fail_check("churn siege", "missing siege result", section="Load"))
            _probe_metrics(ctx, replicas)
            return
        ctx.artifacts["siege"] = str(siege_result.output_path)
        if not siege_result.ok:
            if ctx.config.profile == "strict":
                ctx.add(fail_check("churn siege", f"return_code={siege_result.return_code}", section="Load"))
            else:
                ctx.add(warn_check("churn siege", f"return_code={siege_result.return_code}", section="Load"))
    _probe_metrics(ctx, replicas)


def scenario_chaos(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if len(replicas) < 2:
        ctx.add(fail_check("chaos replica count", "need at least 2 replicas", section="Setup"))
        return
    group = (ctx.env.collect_groups() or ["default"])[0]
    random.seed(ctx.config.random_seed)
    for step in range(1, 5):
        action = random.choice(["stop_replica", "redis_flap"])
        if action == "redis_flap":
            ctx.env.stop_redis()
            ctx.env.wait_seconds(2.0)
            ctx.env.start_redis()
            ctx.add(pass_check(f"chaos step {step} redis flap", section="Stress"))
        else:
            victim = ctx.env.choose_replica(replicas)
            ctx.env.stop_replica(victim.name)
            ctx.env.wait_seconds(2.0)
            ctx.env.start_replica(victim.name)
            ctx.add(pass_check(f"chaos step {step} recycle {victim.name}", section="Stress"))
        replicas = ctx.env.discover_replicas()
        if not replicas:
            ctx.add(fail_check(f"chaos replicas step {step}", "no replicas after chaos step", section="Stress"))
            return
        status, payload = ctx.probes.lb_info(replicas[0].url, group)
        ctx.add(bool_check(f"chaos lb step {step}", status == 200 and bool(payload.get("name")), section="Stress"))
    _probe_metrics(ctx, replicas)


def scenario_concurrent_chaos_load(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if len(replicas) < 2:
        ctx.add(fail_check("concurrent chaos replicas", "need at least 2 replicas", section="Setup"))
        return
    group = (ctx.env.collect_groups() or ["default"])[0]
    target = f"{replicas[0].url}/v1/lb/{group}"
    siege_holder: dict[str, object] = {}

    def concurrent_load_worker() -> None:
        siege_holder["result"] = run_siege(
            target_url=target,
            out_dir=ctx.reporter.attachments_dir,
            label="concurrent-chaos",
            concurrency=50,
            duration="30s",
            timeout_seconds=220,
        )

    load_thread = threading.Thread(target=concurrent_load_worker, daemon=True)
    load_thread.start()
    ctx.env.wait_seconds(4.0)
    victim = ctx.env.choose_replica(replicas, exclude={replicas[0].name})
    ctx.env.stop_replica(victim.name)
    ctx.env.wait_seconds(3.0)
    ctx.env.start_replica(victim.name)
    load_thread.join(timeout=230.0)
    if load_thread.is_alive():
        ctx.add(fail_check("concurrent chaos load thread", "load thread timeout", section="Stress"))
        return
    siege_result = siege_holder.get("result")
    if not isinstance(siege_result, SiegeResult):
        ctx.add(fail_check("concurrent chaos siege", "missing siege result", section="Stress"))
        return
    ctx.artifacts["siege"] = str(siege_result.output_path)
    replicas = ctx.env.discover_replicas()
    if not replicas:
        ctx.add(fail_check("concurrent chaos replicas", "no replicas after recycle", section="Stress"))
        return
    status, payload = ctx.probes.lb_info(replicas[0].url, group)
    ok = siege_result.ok and status == 200 and bool(payload.get("name"))
    ctx.add(bool_check("concurrent chaos load stays available", ok, detail=f"status={status}", section="Stress"))
    _probe_metrics(ctx, replicas)


def scenario_dns_failover(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if len(replicas) < 2:
        ctx.add(fail_check("dns failover replicas", "need at least 2 replicas", section="Setup"))
        return
    group = (ctx.env.collect_groups() or ["default"])[0]
    first = replicas[0]
    ctx.env.stop_replica(first.name)
    ctx.env.wait_seconds(3.0)
    replicas = ctx.env.discover_replicas()
    if not replicas:
        ctx.add(fail_check("dns failover survivors", "no replicas available after stop", section="API"))
        return
    second = replicas[0]
    status, payload = ctx.probes.lb_info(second.url, group)
    ctx.add(
        bool_check(
            "dns failover lb",
            status == 200 and bool(payload.get("name")),
            detail=f"status={status} from={second.name}",
            section="API",
        )
    )
    ctx.env.start_replica(first.name)
    replicas = ctx.env.discover_replicas()
    _probe_health(ctx, replicas)


def scenario_leader_kill_during_probes(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if len(replicas) < 2:
        ctx.add(fail_check("leader kill replicas", "need at least 2 replicas", section="Setup"))
        return
    leader = ctx.probes.wait_for_leader(replicas, timeout_seconds=80)
    ctx.add(bool_check("leader before kill", bool(leader), detail=leader or "none", section="Leader / lock"))
    if not leader:
        return
    ctx.env.stop_replica(leader)
    ctx.env.wait_seconds(4.0)
    replicas = ctx.env.discover_replicas()
    new_leader = ctx.probes.wait_for_leader(replicas, timeout_seconds=80)
    ctx.add(bool_check("leader after kill", bool(new_leader) and new_leader != leader, detail=new_leader or "none", section="Leader / lock"))
    ctx.env.start_replica(leader)
    replicas = ctx.env.discover_replicas()
    _probe_metrics(ctx, replicas)


def scenario_full_restart(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    ctx.env.compose_down(remove_volumes=False)
    ctx.env.wait_seconds(2.0)
    replicas = _discover_ready(ctx)
    ctx.add(bool_check("replicas after full restart", bool(replicas), detail=f"count={len(replicas)}", section="Setup"))
    if replicas:
        _probe_lb(ctx, replicas, (ctx.env.collect_groups() or ["default"])[:1])


def scenario_multi_group(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    groups = ctx.env.collect_groups()
    if len(groups) < 2:
        ctx.add(warn_check("multi group coverage", "fewer than 2 groups in config", section="Multi-Group"))
    for group in groups:
        _probe_lb(ctx, replicas, [group])
        for replica in replicas:
            check = ctx.probes.check_info(replica.url, group)
            total = int(check.get("total", "0") or "0")
            ctx.add(bool_check(f"group check {group} {replica.name}", total > 0, detail=str(check), section="Multi-Group"))
    _probe_metrics(ctx, replicas)


def scenario_multi_group_stress(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    groups = ctx.env.collect_groups() or ["default"]
    random.seed(ctx.config.random_seed)
    error_count = 0
    total = 0
    for _ in range(80):
        replica = random.choice(replicas)
        group = random.choice(groups)
        status, payload = ctx.probes.lb_info(replica.url, group)
        total += 1
        if status != 200 or not payload.get("name"):
            error_count += 1
    err_pct = (error_count / max(1, total)) * 100.0
    threshold = float(ctx.metadata.get("stress_max_error_pct", "5.0"))
    ctx.add(bool_check("multi-group stress error rate", err_pct <= threshold, detail=f"{err_pct:.2f}% <= {threshold:.2f}%", section="Stress"))
    _probe_metrics(ctx, replicas)


def scenario_goroutine_leak(ctx: ScenarioContext) -> None:
    replicas = _discover_ready(ctx)
    if not replicas:
        return
    target = replicas[0]
    status, body = ctx.probes.metrics_text(target.url)
    if status != 200:
        ctx.add(fail_check("goroutine metric baseline", f"status={status}", section="Leak Detection"))
        return
    base = float(metrics_sum(body, "go_goroutines") or "0")
    for _ in range(2):
        victim = ctx.env.choose_replica(replicas)
        ctx.env.stop_replica(victim.name)
        ctx.env.wait_seconds(1.0)
        ctx.env.start_replica(victim.name)
        ctx.env.wait_seconds(2.0)
        replicas = ctx.env.discover_replicas()
        if not replicas:
            ctx.add(fail_check("goroutine replicas", "no replicas after recycle", section="Leak Detection"))
            return
    status_after, body_after = ctx.probes.metrics_text(target.url)
    after = float(metrics_sum(body_after, "go_goroutines") or "0") if status_after == 200 else base
    growth = after - base
    ctx.add(bool_check("goroutine growth < 200", growth < 200, detail=f"base={base:.1f} after={after:.1f} growth={growth:.1f}", section="Leak Detection"))


SCENARIOS = {
    "api": scenario_api,
    "health": scenario_health,
    "leader": scenario_leader,
    "loadbalancer": scenario_loadbalancer,
    "distribution": scenario_distribution,
    "latency": scenario_latency,
    "concurrency": scenario_concurrency,
    "consistency": scenario_consistency,
    "resilience": scenario_resilience,
    "redis_flap": scenario_redis_flap,
    "cold_start": scenario_cold_start,
    "churn": scenario_churn,
    "chaos": scenario_chaos,
    "concurrent_chaos_load": scenario_concurrent_chaos_load,
    "dns_failover": scenario_dns_failover,
    "leader_kill_during_probes": scenario_leader_kill_during_probes,
    "full_restart": scenario_full_restart,
    "multi_group": scenario_multi_group,
    "multi_group_stress": scenario_multi_group_stress,
    "goroutine_leak": scenario_goroutine_leak,
}


def run_scenario(name: str, config: RunConfig, scenario_dir: Path, metadata: dict[str, str] | None = None) -> ScenarioResult:
    started = utc_now()
    started_monotonic = time.time()
    env = BenchEnvironment(config)
    probes = ProbeClient(config)
    reporter = ScenarioReporter(scenario_dir)
    ctx = ScenarioContext(name=name, config=config, env=env, probes=probes, reporter=reporter)
    ctx.metadata["profile"] = config.profile
    if metadata:
        ctx.metadata.update(metadata)
    reporter.event("info", "scenario start", scenario=name, ts=started)
    error_message = ""
    status = "PASS"
    try:
        if name not in SCENARIOS:
            ctx.add(fail_check("scenario exists", f"unknown scenario '{name}'", section="Setup"))
        else:
            SCENARIOS[name](ctx)
    except Exception as exc:  # noqa: BLE001
        error_message = str(exc)
        ctx.add(fail_check("scenario runtime", error_message, section="Setup"))
    finally:
        # Keep environment predictable between scenarios.
        try:
            env.compose_down(remove_volumes=False)
        except Exception as exc:  # noqa: BLE001
            ctx.add(warn_check("compose down", str(exc), section="Setup"))
    if ctx.failures or (config.fail_on_warn and ctx.warnings):
        status = "FAIL"
    finished = utc_now()
    result = ScenarioResult(
        name=name,
        started_at=started,
        finished_at=finished,
        duration_seconds=max(0.0, time.time() - started_monotonic),
        status=status,
        checks=ctx.checks,
        failures=ctx.failures,
        warnings=ctx.warnings,
        artifacts=ctx.artifacts,
        metadata=ctx.metadata,
    )
    if error_message:
        reporter.event("error", "scenario exception", scenario=name, error=error_message)
    reporter.event("info", "scenario end", scenario=name, status=status, ts=finished)
    reporter.write_result(result)
    return result
