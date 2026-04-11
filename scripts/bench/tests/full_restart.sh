#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

bench_defaults
bench_parse_args "$@"

bench_detect_compose
bench_init_report
bench_require_cmds

echo "full_restart: bringing cluster up" | tee -a "$report"
bench_compose_up
bench_discover
bench_collect_groups
bench_wait_lb

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for initial leader..." | tee -a "$report"
	if ! wait_for_leader; then
		record_failure "leader did not converge: initial"
		add_check "leader" "initial leader" "fail"
	else
		add_check "leader" "initial leader" "pass"
	fi
fi

probe_leaders "initial"
probe_endpoints "initial"

echo "" | tee -a "$report"
echo "full_restart: tearing down entire cluster" | tee -a "$report"
compose down | tee -a "$report"
add_check "steps" "compose down" "pass"
sleep 3

echo "full_restart: bringing cluster back up" | tee -a "$report"
bench_compose_up
sleep 5
bench_discover

echo "full_restart: waiting for cluster to reform..." | tee -a "$report"
if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	if ! wait_for_leader; then
		record_failure "leader did not converge after full restart"
		add_check "leader" "leader after restart" "fail"
	else
		add_check "leader" "leader after restart" "pass"
	fi
fi

if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
	echo "waiting for checks after full restart..." | tee -a "$report"
	if ! wait_for_checks; then
		record_failure "checks not ready after full restart"
		add_check "api" "checks after restart" "fail"
	else
		add_check "api" "checks after restart" "pass"
	fi
fi

probe_leaders "after restart"
probe_endpoints "after restart"

BASE_URL="$(pick_live_base_url "")"
success=0
for _ in $(seq 1 10); do
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${BASE_URL}/v1/lb/${GROUP}" || echo "000")"
	if [[ "$code" == "200" ]]; then
		success=$((success + 1))
	fi
done
echo "full_restart: post-restart lb requests success=${success}/10" | tee -a "$report"
if [[ "$success" -ge 8 ]]; then
	add_check "api" "post-restart lb" "pass" "success=${success}/10"
else
	record_failure "post-restart lb low success rate: ${success}/10"
	add_check "api" "post-restart lb" "fail" "success=${success}/10"
fi

bench_finish
