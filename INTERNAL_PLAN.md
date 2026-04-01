# HA Health Checker & LB – Engineer Notes

Working doc for building a Raft-backed HA health-check service in Go. Audience: engineers only (not user-facing).

## Goal
- Run N instances in a Raft cluster; only the leader executes health checks and writes results to Redis with TTL per target.
- Followers serve HTTP APIs using cached Redis data (and config fallback for LB) to avoid duplicate checks.
- Health checks are **HTTP-only** in the current implementation.
- APIs:
  - `GET /v1/check/{group}` → JSON of all targets in that group with reachability flags + freshness metadata.
  - `GET /v1/lb/{group}` → returns one target chosen randomly or round-robin among reachable ones; if none reachable/Redis down, still return a record with `reachable=false` and error reason.
  - `GET /v1/leader`, `POST /v1/raft/join`, `GET /metrics`, `GET /health`.

## High-Level Architecture
- Go module `github.com/<org>/ha-health` (set when initializing go mod).
- Packages:
  - `raftnode`: bootstrap/join Raft, expose leadership state + start/stop leader-only jobs.
  - `config`: load/validate env and YAML into structs.
  - `checks`: HTTP probe runner (leader-only).
  - `redisstore`: thin wrapper around go-redis with tuned pool/timeouts.
  - `api`: HTTP server exposing `/v1/check/`, `/v1/lb/`, `/v1/leader`, `/metrics`, `/health`.
- Data flow:
  - On startup, node joins/creates Raft cluster; once leader, spawns check goroutines; on leadership loss, cancels them.
  - Each HTTP check goroutine loops: run probe → write minimal JSON into Redis hash `hc:{group}` field=`{target}` and maintain `hc:{group}:up` set with TTL/backoff/jitter.
  - API handlers read group hashes; LB path prefers cached/hydrated data and only hits Redis when cache is stale.

## Raft Notes
- Uses `github.com/hashicorp/raft` with in-memory transport (no disk persistence).
- Config via env: `RAFT_NODE_ID`, `RAFT_BIND_ADDR`, `RAFT_PEERS`.
- Only leader writes Redis; followers skip scheduling. API endpoints always available; followers strictly read Redis.
- Rejoin flow (no volumes): nodes call `POST /v1/raft/join` using `RAFT_JOIN_ADDRS`. Set `RAFT_BOOTSTRAP=true` on one node so it can form a cluster if no leader exists. `RAFT_JOIN_TIMEOUT` controls retry window.

## Redis Interaction
- Library: `github.com/redis/go-redis/v9` tuned for HA: pool size 100, read timeout 1s, write timeout 500ms; semaphore 256 to avoid local stampede.
- Storage: Hash per group `hc:{group}`; field=`target name`; value JSON `{reachable,status,checked_at,error,latency_ms,type,target}` (target name only). Connection data is hydrated from in-process config map to keep payloads small.
- Up set: `hc:{group}:up` contains reachable target names (SRANDMEMBER fast path) and expires with the hash.
- LB behavior:
  - Cache-first (max age fixed at **5s**, not configurable).
  - If cache stale: use `:up` SRANDMEMBER(1) when available, else `HGETALL`.
  - On Redis errors: short backoff and fallback to config-hydrated targets.
- `/v1/check` returns `redis_status=error` when Redis is down (no fallback payload).

## Health Check Types (Current Implementation)
- **HTTP URL** (only implemented check type)
  - Fields: `url`, `name`, `interval`, `timeout`, `retry`, `redis_ttl`, `response` (expected status list)
  - HTTP options: `method`, `headers`, `auth_basic_user`, `auth_basic_pass`, `auth_bearer`, `follow_redirects`, `max_redirects`.
  - Implementation: HTTP request with per-target timeout; record latency.

## Configuration Model
- Primary source: YAML (`config-targets.yaml`), env overrides for small setups.

Example:
```yaml
default:
  interval: 10s
  timeout: 2s
  redisTTL: 15s
  retry: 1

checks:
  web-health:
    type: http
    interval: 8s
    timeout: 2s
    redisTTL: 15s
    retry: 1
    startTogether: true
    targets:
      - name: public-health
        url: https://example.com/health
        response: 200,204
        headers:
          User-Agent: ha-health
      - name: basic-auth-health
        url: https://example.com/private/health
        auth_basic_user: user
        auth_basic_pass: pass
        method: GET
      - name: no-redirect
        url: https://example.com/redirect
        follow_redirects: false
        response: 302
```

Defaults:
- `follow_redirects` defaults to true.
- `max_redirects` defaults to 10.
- `startTogether` is enforced true (policy).

## Concurrency & Lifecycle
- Leader starts/stops per-target goroutines based on leadership state.
- For each group, targets launch together; each goroutine uses its own ticker/timeout/retry budget.
- Small jitter (5%) to avoid thundering herd.
- Redis write retries with capped backoff.

## HTTP API Behavior
- Server has read/write/idle timeouts and header limits.
- `/v1/check/{group}`: Reads hash and hydrates from local config; on Redis error returns `redis_status=error` and message.
- `/v1/lb/{group}`:
  - Strategy: `LB_STRATEGY` random | round-robin.
  - Cache-first with fixed 5s max age.
  - Uses `:up` fast path; falls back to full hash or config on errors.
- `/v1/leader`, `/metrics`, `/health` provided for ops/visibility.

## Metrics (Prometheus)
- `lb_requests_total{path,cache_hit}`
- `lb_latency_ms{path}`
- `lb_errors_total{type}`
- `check_requests_total{redis_status}`
- `check_latency_ms{redis_status}`
- `check_targets_total{redis_status}`
- `probe_runs_total{check_type}`
- `probe_write_errors_total{check_type}`

## Testing / Bench
- Unit tests: config loader, HTTP probe, API cache/LB fallback.
- Bench scripts: `scripts/bench/tests/*.sh`.
  - Full suite: `./scripts/bench/tests/massive.sh`.
  - Distribution test is warn-only by default; set `LB_DIST_STRICT=true` to enforce.

## Containers / Compose
- Dockerfile: multi-stage builder -> alpine runtime, binary at `/app/ha`.
- `docker-compose.yml`: three app nodes + redis; mounts `config-targets.yaml` into `/app/config-targets.yaml`; exposes HTTP ports 8080/8081/8082 and raft ports 12000/12001/12002.

## Future Work (Not Implemented Yet)
- Additional check types: bucket/object (S3), TCP, DNS, ICMP ping, TLS cert expiry, gRPC health.
- Optional config reload on SIGHUP.
- API authn/authz.
- Persisted Raft logs/snapshots (env `RAFT_DATA_DIR`).

## Open Questions
- Should followers serve stale data beyond TTL with `stale=true`?
- Should `/v1/check` return config fallback payloads during Redis outages?
