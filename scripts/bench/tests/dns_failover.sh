#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

bench_defaults
bench_parse_args "$@"
bench_prepare

echo "dns_failover: testing that lb continues serving after replica kill" | tee -a "$report"

probe_leaders "dns before"
probe_endpoints "dns before"

local_base="$BASE_URL"
echo "dns_failover: baseline requests to ${local_base}" | tee -a "$report"
for _ in $(seq 1 10); do
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${local_base}/v1/lb/${GROUP}" || echo "000")"
	if [[ "$code" != "200" ]]; then
		record_failure "dns_failover baseline request failed: code=${code}"
	fi
done
add_check "api" "dns baseline requests" "pass"

target_replica="${REPLICA_CONTAINERS[0]}"
echo "dns_failover: killing replica ${target_replica}" | tee -a "$report"
stop_replica "$target_replica"
add_check "steps" "stop ${target_replica}" "pass"
sleep 3

discover_replicas 2>/dev/null || true
new_base="$(pick_live_base_url "")"
echo "dns_failover: using ${new_base} after kill" | tee -a "$report"

success=0
fail=0
for _ in $(seq 1 20); do
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${new_base}/v1/lb/${GROUP}" || echo "000")"
	if [[ "$code" == "200" ]]; then
		success=$((success + 1))
	else
		fail=$((fail + 1))
	fi
done
echo "dns_failover: after kill success=${success} fail=${fail}" | tee -a "$report"
if [[ "$success" -gt 0 ]]; then
	add_check "api" "dns failover requests" "pass" "success=${success} fail=${fail}"
else
	record_failure "dns failover: all requests failed"
	add_check "api" "dns failover requests" "fail"
fi

echo "dns_failover: restarting ${target_replica}" | tee -a "$report"
start_replica "$target_replica"
add_check "steps" "restart ${target_replica}" "pass"
sleep 5

discover_replicas 2>/dev/null || true
if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for leader after restart..." | tee -a "$report"
	if ! wait_for_leader; then
		record_failure "leader did not converge after dns_failover restart"
		add_check "leader" "leader converge after restart" "fail"
	else
		add_check "leader" "leader converge after restart" "pass"
	fi
fi

probe_leaders "dns after restart"
probe_endpoints "dns after restart"

bench_finish
