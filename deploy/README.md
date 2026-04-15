# Deployment Profiles

This directory provides parity-oriented deployment references for Docker, Kubernetes, and service mode.

## Docker (reference)

- Use `docker-compose.yml` for local and baseline production-like behavior.
- Liveness endpoint: `/health`
- Readiness endpoint: `/ready`

## Kubernetes

- Apply manifests from `deploy/k8s/`:
  - `configmap.yaml`
  - `deployment.yaml`
  - `service.yaml`
- Probes:
  - startup/readiness: `/ready`
  - liveness: `/health`

## Service mode (systemd)

- Install `deploy/systemd/ha.service` and adapt paths/user.
- Keep environment variables aligned with Kubernetes and Docker:
  - `CONFIG_PATH`
  - `LISTEN_ADDR`
  - `REDIS_ADDR`
  - `REDIS_DB`
  - lock controls (`LOCK_TTL`, `LOCK_RENEW_EVERY`, `LOCK_REDIS_TIMEOUT`)
