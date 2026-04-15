#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${LATENCY_AVG_MS:=50}"
: "${LATENCY_MAX_MS:=250}"
: "${LATENCY_PROGRESS_INTERVAL:=15}"
: "${LATENCY_SIEGE_TIMEOUT:=}"

bench_defaults
bench_parse_args "$@"
: "${LATENCY_SIEGE_TIMEOUT:=$(bench_siege_timeout_seconds)}"
bench_prepare

if ! command -v siege >/dev/null 2>&1; then
	add_check "load" "latency budget" "warn" "siege not installed"
	bench_finish
	exit 0
fi

out="$OUT_DIR/siege-latency.txt"
url="${BASE_URL}/v1/lb/${GROUP}"
echo "running siege latency: $url (timeout=${LATENCY_SIEGE_TIMEOUT}s)" | tee -a "$report"
progress_pid=""
if [[ "$LATENCY_PROGRESS_INTERVAL" -gt 0 ]]; then
	(
		while true; do
			echo "latency: load running..."
			sleep "$LATENCY_PROGRESS_INTERVAL"
		done
	) &
	progress_pid=$!
fi
run_siege_capture "$url" "$out" "$LATENCY_SIEGE_TIMEOUT"
if [[ -n "$progress_pid" ]]; then
	kill "$progress_pid" >/dev/null 2>&1 || true
	wait "$progress_pid" >/dev/null 2>&1 || true
fi

avg_s="$(rg -m1 '"response_time"' "$out" | sed 's/[^0-9.]//g')"
max_s="$(rg -m1 '"longest_transaction"' "$out" | sed 's/[^0-9.]//g')"
avg_s="$(echo "$avg_s" | tr -d '\r' || true)"
max_s="$(echo "$max_s" | tr -d '\r' || true)"

avg_ms=0
max_ms=0
if [[ -n "$avg_s" ]]; then
	avg_ms=$(awk -v s="$avg_s" 'BEGIN{printf "%.0f", s*1000}')
fi
if [[ -n "$max_s" ]]; then
	max_ms=$(awk -v s="$max_s" 'BEGIN{printf "%.0f", s*1000}')
fi

if [[ -z "$avg_s" || -z "$max_s" ]]; then
	record_failure "latency parse failed"
	add_check "load" "latency budget" "fail" "avg_ms=${avg_ms} max_ms=${max_ms}"
else
	if [[ "$avg_ms" -le "$LATENCY_AVG_MS" && "$max_ms" -le "$LATENCY_MAX_MS" ]]; then
		add_check "load" "latency budget" "pass" "avg_ms=${avg_ms} max_ms=${max_ms} budgets=${LATENCY_AVG_MS}/${LATENCY_MAX_MS}"
	else
		record_failure "latency budget exceeded"
		add_check "load" "latency budget" "fail" "avg_ms=${avg_ms} max_ms=${max_ms} budgets=${LATENCY_AVG_MS}/${LATENCY_MAX_MS}"
	fi
fi

if [[ "$KEEP_REPORT" != "true" ]]; then
	rm -f "$out"
fi

probe_metrics "latency"
bench_finish
