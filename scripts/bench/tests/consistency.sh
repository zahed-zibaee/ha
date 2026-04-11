#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${CONSISTENCY_WINDOW:=5}"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for Redis lock leader (or degraded probes if Redis down)..." | tee -a "$report"
	if ! wait_for_leader; then
		echo "leader did not converge in time" | tee -a "$report"
		record_failure "leader did not converge: consistency"
		emit_leader_logs "consistency"
		add_check "leader" "leader converge" "fail"
	else
		add_check "leader" "leader converge" "pass"
	fi
fi

probe_leaders "consistency"
leader_name="${leader_detected:-}"

metrics_probe_value() {
	local url="$1"
	local body
	body="$(curl -sS --max-time 2 "${url}/metrics" || true)"
	if [[ -z "$body" ]]; then
		echo "0"
		return
	fi
	metrics_sum "$body" "probe_runs_total"
}

declare -A base_values
for i in "${!REPLICA_URLS[@]}"; do
	local_url="${REPLICA_URLS[$i]}"
	local_name="${REPLICA_CONTAINERS[$i]}"
	base_values["$local_name"]="$(metrics_probe_value "$local_url")"
done

sleep "$CONSISTENCY_WINDOW"

declare -A after_values
for i in "${!REPLICA_URLS[@]}"; do
	local_url="${REPLICA_URLS[$i]}"
	local_name="${REPLICA_CONTAINERS[$i]}"
	after_values["$local_name"]="$(metrics_probe_value "$local_url")"
done

active_replicas=()
for i in "${!REPLICA_CONTAINERS[@]}"; do
	local_name="${REPLICA_CONTAINERS[$i]}"
	delta=$(awk -v a="${after_values[$local_name]}" -v b="${base_values[$local_name]}" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')
	echo "probe delta ${local_name}: ${delta}" | tee -a "$report"
	if [[ "$delta" -gt 0 ]]; then
		active_replicas+=("$local_name")
	fi
done

redis_err="$(bench_redis_check_error)"
n_active="${#active_replicas[@]}"
n_replicas="${#REPLICA_CONTAINERS[@]}"

if [[ "$redis_err" -eq 1 && "$n_active" -eq "$n_replicas" && "$n_replicas" -gt 0 ]]; then
	echo "consistency: Redis check error; expecting all ${n_replicas} replicas to run probes" | tee -a "$report"
	add_check "leader" "probe placement (redis down)" "pass" "all_replicas=${n_active}"
elif [[ "${#active_replicas[@]}" -eq 1 && -n "$leader_name" && "${active_replicas[0]}" == "$leader_name" ]]; then
	add_check "leader" "single-lock probes" "pass" "leader=${leader_name}"
else
	record_failure "probe placement mismatch"
	add_check "leader" "single-lock probes" "fail" "leader=${leader_name:-unknown} active=$(IFS=','; echo "${active_replicas[*]}") redis_err=${redis_err}"
fi

probe_metrics "consistency"
bench_finish
