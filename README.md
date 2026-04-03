# ha

Raft-backed HA health checker with Redis. **HTTP checks only** in the current implementation.
The leader runs probes and writes results to Redis; followers serve the API from Redis and local config.

## Quickstart

1) Copy and edit config:

```bash
cp config-targets.example.yaml config-targets.yaml
```

2) Start the cluster:

```bash
docker compose up --build -d
```

3) Query endpoints:

```bash
curl http://localhost:8080/v1/check/web-health
curl http://localhost:8080/v1/lb/web-health
curl http://localhost:8080/v1/leader
curl http://localhost:8080/metrics
curl http://localhost:8080/health
```

## Endpoints

- `GET /v1/check/{group}`
- `GET /v1/lb/{group}`
- `GET /v1/leader`
- `POST /v1/raft/join` (internal)
- `GET /metrics`
- `GET /health`

## Behavior Notes

- LB cache max age is **fixed at 5s** (not configurable).
- LB uses Redis `:up` when available. On Redis errors, it falls back to config-hydrated targets.
- `/v1/lb` responses are **flattened**: always include `group`, `reachable`, `name`, and `error` (if any).
- You can override LB response fields per target via `checks.<group>.lb.response_targets[].response`.
- LB strategy supports `random`, `round-robin`, `weighted` (latency-based), and `weighted-rr` (latency-weighted round-robin). Per-group override via `checks.<group>.lb.type`.
- `/v1/check` returns `redis_status=error` when Redis is down (no fallback payload).
- Raft rejoin uses `POST /v1/raft/join` and `RAFT_JOIN_ADDRS`. Use `RAFT_BOOTSTRAP=true` on exactly one node to form a cluster if none exists.

Example LB override:

```yaml
checks:
  web-health:
    lb:
      type: round-robin
      response_targets:
        - name: public-health
          response:
            url: https://example.com/home
            bucket: my-bucket
            description: "Custom LB response"
```

## Metrics (Prometheus)

- `lb_requests_total{path,cache_hit}`
- `lb_latency_ms{path}`
- `lb_errors_total{type}`
- `check_requests_total{redis_status}`
- `check_latency_ms{redis_status}`
- `check_targets_total{redis_status}`
- `probe_runs_total{check_type}`
- `probe_write_errors_total{check_type}`

## Bench Suite

Tests live in `scripts/bench/tests/`.

Run the full suite:

```bash
./scripts/bench/tests/massive.sh
```
