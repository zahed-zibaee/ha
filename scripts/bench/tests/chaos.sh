#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${CHAOS_STEPS:=8}"
: "${CHAOS_SLEEP:=3}"
: "${CHAOS_REDIS_PROB:=20}"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for single leader..." | tee -a "$report"
	if ! wait_for_leader; then
		echo "leader did not converge in time" | tee -a "$report"
		record_failure "leader did not converge: chaos"
		emit_leader_logs "chaos"
		add_check "raft" "leader converge" "fail"
	else
		add_check "raft" "leader converge" "pass"
	fi
fi

stopped_nodes=()
redis_down=0

is_stopped() {
	local node="$1"
	for n in "${stopped_nodes[@]}"; do
		if [[ "$n" == "$node" ]]; then
			return 0
		fi
	done
	return 1
}

running_nodes() {
	local nodes=()
	for n in ha1 ha2 ha3; do
		if ! is_stopped "$n"; then
			nodes+=("$n")
		fi
	done
	echo "${nodes[*]}"
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
		running="$(running_nodes)"
		read -r -a running_list <<< "$running"
		if [[ "${#running_list[@]}" -gt 1 ]]; then
			# stop a random running node
			idx=$((RANDOM % ${#running_list[@]}))
			node="${running_list[$idx]}"
			echo "chaos: stopping ${node}" | tee -a "$report"
			ignore_port "$(port_from_svc "$node")"
			compose stop "$node" | tee -a "$report"
			stopped_nodes+=("$node")
			add_check "steps" "stop ${node} chaos ${step}" "pass"
		else
			# restart a stopped node if possible
			if [[ "${#stopped_nodes[@]}" -gt 0 ]]; then
				node="${stopped_nodes[0]}"
				echo "chaos: starting ${node}" | tee -a "$report"
				compose start "$node" | tee -a "$report"
				unignore_port "$(port_from_svc "$node")"
				stopped_nodes=(${stopped_nodes[@]:1})
				add_check "steps" "start ${node} chaos ${step}" "pass"
			fi
		fi
	fi

	sleep "$CHAOS_SLEEP"
	probe_leaders "chaos step ${step}"
	label="chaos step ${step}"
	if [[ "$redis_down" -eq 1 ]]; then
		label="redis down chaos step ${step}"
	fi
	probe_endpoints "$label"
	probe_metrics "chaos step ${step}"

done

bench_finish
