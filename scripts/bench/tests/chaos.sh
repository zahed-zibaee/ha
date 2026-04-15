#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${CHAOS_STEPS:=8}"
: "${CHAOS_SLEEP:=3}"
: "${CHAOS_REDIS_PROB:=20}"
: "${CHAOS_RECOVERY_TIMEOUT:=15}"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for Redis lock leader (or degraded probes if Redis down)..." | tee -a "$report"
	if ! wait_for_leader; then
		echo "leader did not converge in time" | tee -a "$report"
		record_failure "leader did not converge: chaos"
		emit_leader_logs "chaos"
		add_check "leader" "leader converge" "fail"
	else
		add_check "leader" "leader converge" "pass"
	fi
fi

redis_down=0

chaos_wait_recovery() {
	local saved_leader_timeout="${WAIT_LEADER_TIMEOUT}"
	local saved_checks_timeout="${WAIT_CHECKS_TIMEOUT}"
	WAIT_LEADER_TIMEOUT="$CHAOS_RECOVERY_TIMEOUT"
	WAIT_CHECKS_TIMEOUT="$CHAOS_RECOVERY_TIMEOUT"
	if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
		wait_for_leader >/dev/null 2>&1 || true
	fi
	if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
		wait_for_checks >/dev/null 2>&1 || true
	fi
	WAIT_LEADER_TIMEOUT="$saved_leader_timeout"
	WAIT_CHECKS_TIMEOUT="$saved_checks_timeout"
}

for step in $(seq 1 "$CHAOS_STEPS"); do
	echo "" | tee -a "$report"
	echo "chaos step ${step}" | tee -a "$report"

	rnd=$((RANDOM % 100))
	if [[ "$rnd" -lt "$CHAOS_REDIS_PROB" ]]; then
		if [[ "$redis_down" -eq 0 ]]; then
			echo "chaos: stopping redis" | tee -a "$report"
			compose stop redis | tee -a "$report"
			redis_down=1
			add_check "steps" "redis stop chaos ${step}" "pass"
		else
			echo "chaos: starting redis" | tee -a "$report"
			compose start redis | tee -a "$report"
			redis_down=0
			add_check "steps" "redis start chaos ${step}" "pass"
		fi
	else
		read -r -a running_list <<< "$(running_replicas)"
		if [[ "${#running_list[@]}" -gt 1 ]]; then
			idx=$((RANDOM % ${#running_list[@]}))
			node="${running_list[$idx]}"
			echo "chaos: stopping ${node}" | tee -a "$report"
			stop_replica "$node"
			add_check "steps" "stop ${node} chaos ${step}" "pass"
		else
			if [[ "${#STOPPED_REPLICAS[@]}" -gt 0 ]]; then
				node="${STOPPED_REPLICAS[0]}"
				echo "chaos: starting ${node}" | tee -a "$report"
				start_replica "$node"
				add_check "steps" "start ${node} chaos ${step}" "pass"
			fi
		fi
	fi

	sleep "$CHAOS_SLEEP"
	discover_replicas 2>/dev/null || true
	ensure_live_base_url >/dev/null 2>&1 || true
	if [[ "$redis_down" -eq 0 ]]; then
		chaos_wait_recovery
	fi
	probe_leaders "chaos step ${step}"
	label="chaos step ${step}"
	if [[ "$redis_down" -eq 1 ]]; then
		label="redis down chaos step ${step}"
	fi
	probe_endpoints "$label"
	probe_metrics "chaos step ${step}"

done

bench_finish
