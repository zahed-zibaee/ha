#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${CHURN_STOP_DELAY:=5}"
: "${CHURN_RESTART_DELAY:=10}"
: "${CHURN_PROGRESS_INTERVAL:=15}"
: "${CHURN_SIEGE_TIMEOUT:=120}"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for single leader..." | tee -a "$report"
	if ! wait_for_leader; then
		echo "leader did not converge in time" | tee -a "$report"
		record_failure "leader did not converge: churn"
		emit_leader_logs "churn"
		add_check "raft" "leader converge" "fail"
	else
		add_check "raft" "leader converge" "pass"
	fi
fi

probe_leaders "churn"
leader_before="${leader_detected:-}"
if [[ -z "$leader_before" ]]; then
	leader_before="$(detect_leader)"
fi

if ! command -v siege >/dev/null 2>&1; then
	add_check "load" "churn load" "warn" "siege not installed"
	bench_finish
	exit 0
fi

out="$OUT_DIR/siege-churn.txt"
url="${BASE_URL}/v1/lb/${GROUP}"
echo "running siege churn: $url" | tee -a "$report"
if command -v timeout >/dev/null 2>&1; then
	timeout "${CHURN_SIEGE_TIMEOUT}s" siege -c "${CONCURRENCY}" -t "${DURATION}" "$url" >"$out" 2>&1 &
	siege_pid=$!
else
	siege -c "${CONCURRENCY}" -t "${DURATION}" "$url" >"$out" 2>&1 &
	siege_pid=$!
	( sleep "$CHURN_SIEGE_TIMEOUT"; kill "$siege_pid" >/dev/null 2>&1 || true ) &
	watchdog_pid=$!
fi

progress_pid=""
if [[ "$CHURN_PROGRESS_INTERVAL" -gt 0 ]]; then
	(
		while true; do
			echo "churn: load running..."
			sleep "$CHURN_PROGRESS_INTERVAL"
		done
	) &
	progress_pid=$!
fi

sleep "$CHURN_STOP_DELAY"
if [[ -n "$leader_before" ]]; then
	echo "churn: stopping leader ${leader_before}" | tee -a "$report"
	ignore_port "$(port_from_svc "$leader_before")"
	compose stop "$leader_before" | tee -a "$report"
	add_check "steps" "stop ${leader_before} (churn)" "pass"
	if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
		if ! wait_for_leader; then
			record_failure "leader did not converge during churn"
			add_check "raft" "leader converge during churn" "fail"
		else
			add_check "raft" "leader converge during churn" "pass"
		fi
	fi
fi

sleep "$CHURN_RESTART_DELAY"
if [[ -n "$leader_before" && "$RESTART_STOPPED" == "true" ]]; then
	echo "churn: restarting leader ${leader_before}" | tee -a "$report"
	compose start "$leader_before" | tee -a "$report"
	unignore_port "$(port_from_svc "$leader_before")"
	add_check "steps" "restart ${leader_before} (churn)" "pass"
fi

wait "$siege_pid" || true
if [[ -n "${watchdog_pid:-}" ]]; then
	kill "$watchdog_pid" >/dev/null 2>&1 || true
	wait "$watchdog_pid" >/dev/null 2>&1 || true
fi
if [[ -n "$progress_pid" ]]; then
	kill "$progress_pid" >/dev/null 2>&1 || true
	wait "$progress_pid" >/dev/null 2>&1 || true
fi

trx="$(rg -m1 '"transactions"' "$out" | sed 's/[^0-9.]//g')"
rate="$(rg -m1 '"transaction_rate"' "$out" | sed 's/[^0-9.]//g')"
resp="$(rg -m1 '"response_time"' "$out" | sed 's/[^0-9.]//g')"
if [[ -n "$trx" || -n "$rate" || -n "$resp" ]]; then
	if [[ "${trx:-0}" != "0" ]]; then
		add_check "load" "churn load" "pass" "trx=${trx:-0} rate=${rate:-0} resp=${resp:-0}"
	else
		record_failure "churn load no transactions"
		add_check "load" "churn load" "fail" "no transactions"
	fi
else
	add_check "load" "churn load" "warn" "no summary parsed"
fi

if [[ "$KEEP_REPORT" != "true" ]]; then
	rm -f "$out"
fi

probe_metrics "churn"
bench_finish
