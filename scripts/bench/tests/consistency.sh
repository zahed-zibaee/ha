#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${CONSISTENCY_WINDOW:=5}"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for single leader..." | tee -a "$report"
	if ! wait_for_leader; then
		echo "leader did not converge in time" | tee -a "$report"
		record_failure "leader did not converge: consistency"
		emit_leader_logs "consistency"
		add_check "raft" "leader converge" "fail"
	else
		add_check "raft" "leader converge" "pass"
	fi
fi

probe_leaders "consistency"
leader_svc="${leader_detected:-}"
leader_port="$(port_from_svc "$leader_svc")"

metrics_probe_value() {
	local port="$1"
	local body
	body="$(curl -sS --max-time 2 "http://localhost:${port}/metrics" || true)"
	if [[ -z "$body" ]]; then
		echo "0"
		return
	fi
	metrics_sum "$body" "probe_runs_total"
}

base_8080="$(metrics_probe_value 8080)"
base_8081="$(metrics_probe_value 8081)"
base_8082="$(metrics_probe_value 8082)"

sleep "$CONSISTENCY_WINDOW"

after_8080="$(metrics_probe_value 8080)"
after_8081="$(metrics_probe_value 8081)"
after_8082="$(metrics_probe_value 8082)"

delta_8080=$(awk -v a="$after_8080" -v b="$base_8080" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')
delta_8081=$(awk -v a="$after_8081" -v b="$base_8081" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')
delta_8082=$(awk -v a="$after_8082" -v b="$base_8082" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')

active_ports=()
if [[ "$delta_8080" -gt 0 ]]; then active_ports+=(8080); fi
if [[ "$delta_8081" -gt 0 ]]; then active_ports+=(8081); fi
if [[ "$delta_8082" -gt 0 ]]; then active_ports+=(8082); fi

if [[ "${#active_ports[@]}" -eq 1 && -n "$leader_port" && "${active_ports[0]}" == "$leader_port" ]]; then
	add_check "raft" "leader-only probes" "pass" "leader=${leader_svc}"
else
	record_failure "leader-only probes mismatch"
	add_check "raft" "leader-only probes" "fail" "leader=${leader_svc:-unknown} active=$(IFS=','; echo "${active_ports[*]}")"
fi

probe_metrics "consistency"
bench_finish
