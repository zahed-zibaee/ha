#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

bench_defaults
bench_parse_args "$@"
bench_prepare

echo "leader_kill: identifying leader and waiting for probes" | tee -a "$report"

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	if ! wait_for_leader; then
		record_failure "leader did not converge: initial"
		add_check "leader" "initial leader" "fail"
	else
		add_check "leader" "initial leader" "pass"
	fi
fi

probe_leaders "before kill"
old_leader="${leader_detected:-}"
if [[ -z "$old_leader" ]]; then
	old_leader="$(detect_leader)"
fi
if [[ -z "$old_leader" ]]; then
	echo "leader_kill: no leader detected, skipping test" | tee -a "$report"
	add_check "leader" "leader detection" "fail" "no leader found"
	bench_finish
	exit 0
fi
echo "leader_kill: leader is ${old_leader}" | tee -a "$report"

if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
	echo "waiting for checks before kill..." | tee -a "$report"
	if ! wait_for_checks; then
		record_failure "checks not ready before leader kill"
		add_check "api" "checks before kill" "fail"
	else
		add_check "api" "checks before kill" "pass"
	fi
fi

probe_endpoints "before kill"
probe_metrics "before kill"

echo "" | tee -a "$report"
echo "leader_kill: killing leader ${old_leader} mid-probe-cycle" | tee -a "$report"
stop_replica "$old_leader"
add_check "steps" "kill leader ${old_leader}" "pass"

echo "leader_kill: verifying lb still responds during transition" | tee -a "$report"
live_base="$(pick_live_base_url "")"
transition_success=0
transition_fail=0
for _ in $(seq 1 15); do
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${live_base}/v1/lb/${GROUP}" || echo "000")"
	if [[ "$code" == "200" ]]; then
		transition_success=$((transition_success + 1))
	else
		transition_fail=$((transition_fail + 1))
	fi
	sleep 0.5
done
echo "leader_kill: transition requests success=${transition_success} fail=${transition_fail}" | tee -a "$report"
if [[ "$transition_success" -gt 0 ]]; then
	add_check "api" "lb during transition" "pass" "success=${transition_success} fail=${transition_fail}"
else
	record_failure "lb unresponsive during leader transition"
	add_check "api" "lb during transition" "fail"
fi

echo "leader_kill: waiting for new leader election" | tee -a "$report"
if ! wait_for_leader; then
	record_failure "new leader did not emerge after kill"
	add_check "leader" "new leader election" "fail"
else
	add_check "leader" "new leader election" "pass"
fi

probe_leaders "after kill"
new_leader="${leader_detected:-}"
if [[ -n "$new_leader" && "$new_leader" != "$old_leader" ]]; then
	echo "leader_kill: leadership transferred from ${old_leader} to ${new_leader}" | tee -a "$report"
	add_check "leader" "leader transfer" "pass" "old=${old_leader} new=${new_leader}"
elif [[ -n "$new_leader" ]]; then
	add_check "leader" "leader transfer" "pass" "same leader re-elected: ${new_leader}"
else
	record_failure "no leader after kill"
	add_check "leader" "leader transfer" "fail"
fi

if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
	echo "waiting for probes to resume under new leader..." | tee -a "$report"
	if ! wait_for_checks; then
		record_failure "probes did not resume after leader kill"
		add_check "api" "probes resume" "fail"
	else
		add_check "api" "probes resume" "pass"
	fi
fi

probe_endpoints "after kill"
probe_metrics "after kill"

echo "leader_kill: restarting ${old_leader}" | tee -a "$report"
start_replica "$old_leader"
add_check "steps" "restart ${old_leader}" "pass"
sleep 5
discover_replicas 2>/dev/null || true

bench_finish
