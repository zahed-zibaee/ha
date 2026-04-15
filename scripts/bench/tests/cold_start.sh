#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

bench_defaults
bench_parse_args "$@"

bench_detect_compose
bench_init_report
bench_require_cmds

start_ts=$(date +%s)
: "${COLD_START_USE_DOWN:=false}"
if [[ "$COLD_START_USE_DOWN" == "true" ]]; then
	compose down -v | tee -a "$report"
	add_check "steps" "compose down" "pass"
else
	compose stop | tee -a "$report"
	add_check "steps" "compose stop" "pass"
fi

if [[ "$COLD_START_USE_DOWN" == "true" ]]; then
	bench_compose_up
else
	compose start | tee -a "$report"
	add_check "steps" "compose start" "pass"
fi
bench_discover
echo "waiting for all replica health endpoints..." | tee -a "$report"
if ! wait_for_all_replicas_health; then
	echo "not all replicas became healthy in time" | tee -a "$report"
	record_failure "replicas did not become healthy: cold start"
	add_check "api" "replicas healthy" "fail"
else
	add_check "api" "replicas healthy" "pass"
fi
bench_collect_groups
bench_wait_lb

leader_ready=0
if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for Redis lock leader (or degraded probes if Redis down)..." | tee -a "$report"
	if ! wait_for_leader; then
		echo "leader did not converge in time" | tee -a "$report"
		record_failure "leader did not converge: cold start"
		emit_leader_logs "cold start"
		add_check "leader" "leader converge" "fail"
	else
		leader_ready=1
		add_check "leader" "leader converge" "pass"
	fi
fi

checks_ready=0
if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
	echo "waiting for non-empty check results..." | tee -a "$report"
	if ! wait_for_checks; then
		echo "checks did not become ready in time" | tee -a "$report"
		record_failure "checks did not become ready: cold start"
		add_check "api" "checks ready" "fail"
	else
		checks_ready=1
		add_check "api" "checks ready" "pass"
	fi
fi

end_ts=$(date +%s)
startup_secs=$((end_ts - start_ts))
if [[ "$leader_ready" -eq 1 && "$checks_ready" -eq 1 ]]; then
	add_check "steps" "cold start ready" "pass" "seconds=${startup_secs}"
else
	add_check "steps" "cold start ready" "fail" "seconds=${startup_secs}"
fi

probe_endpoints "cold start"
probe_metrics "cold start"
bench_finish
