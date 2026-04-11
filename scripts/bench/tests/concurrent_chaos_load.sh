#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${CHAOS_LOAD_DURATION:=30s}"
: "${CHAOS_LOAD_CONCURRENCY:=100}"
: "${CHAOS_KILL_INTERVAL:=5}"
: "${CHAOS_KILL_COUNT:=4}"

bench_defaults
DURATION="$CHAOS_LOAD_DURATION"
CONCURRENCY="$CHAOS_LOAD_CONCURRENCY"
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	if ! wait_for_leader; then
		record_failure "leader did not converge: concurrent chaos"
		add_check "leader" "initial leader" "fail"
	else
		add_check "leader" "initial leader" "pass"
	fi
fi

probe_leaders "before chaos"
probe_endpoints "before chaos"

if ! command -v siege >/dev/null 2>&1; then
	echo "siege not installed; using curl loop instead" | tee -a "$report"
	use_curl=1
else
	use_curl=0
fi

out="$OUT_DIR/siege-concurrent-chaos.txt"
url="${BASE_URL}/v1/lb/${GROUP}"
echo "concurrent_chaos: starting load against ${url}" | tee -a "$report"
if [[ "$use_curl" -eq 0 ]]; then
	siege -c "${CONCURRENCY}" -t "${DURATION}" "$url" >"$out" 2>&1 &
	load_pid=$!
else
	(
		end_time=$((SECONDS + 30))
		success=0
		fail=0
		while [[ $SECONDS -lt $end_time ]]; do
			code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "$url" || echo "000")"
			if [[ "$code" == "200" ]]; then
				success=$((success + 1))
			else
				fail=$((fail + 1))
			fi
		done
		echo "{\"success\": $success, \"fail\": $fail}" >"$out"
	) &
	load_pid=$!
fi

sleep 3
for step in $(seq 1 "$CHAOS_KILL_COUNT"); do
	echo "" | tee -a "$report"
	echo "concurrent_chaos: kill cycle ${step}" | tee -a "$report"

	read -r -a running_list <<< "$(running_replicas)"
	if [[ "${#running_list[@]}" -gt 1 ]]; then
		idx=$((RANDOM % ${#running_list[@]}))
		victim="${running_list[$idx]}"
		echo "concurrent_chaos: stopping ${victim}" | tee -a "$report"
		stop_replica "$victim"
		add_check "steps" "chaos stop ${victim} cycle ${step}" "pass"
	fi

	sleep "$CHAOS_KILL_INTERVAL"

	if [[ "${#STOPPED_REPLICAS[@]}" -gt 0 ]]; then
		revive="${STOPPED_REPLICAS[0]}"
		echo "concurrent_chaos: restarting ${revive}" | tee -a "$report"
		start_replica "$revive"
		add_check "steps" "chaos restart ${revive} cycle ${step}" "pass"
		sleep 2
		discover_replicas 2>/dev/null || true
	fi
done

wait "$load_pid" || true

if [[ "$use_curl" -eq 0 && -f "$out" ]]; then
	trx="$(rg -m1 '"transactions"' "$out" | sed 's/[^0-9.]//g' || true)"
	rate="$(rg -m1 '"transaction_rate"' "$out" | sed 's/[^0-9.]//g' || true)"
	resp="$(rg -m1 '"response_time"' "$out" | sed 's/[^0-9.]//g' || true)"
	failed="$(rg -m1 '"failed_transactions"' "$out" | sed 's/[^0-9.]//g' || true)"
	echo "concurrent_chaos: trx=${trx:-0} rate=${rate:-0} resp=${resp:-0} failed=${failed:-0}" | tee -a "$report"

	if [[ -n "$trx" && "${trx:-0}" != "0" ]]; then
		err_pct="$(awk -v f="${failed:-0}" -v t="$trx" 'BEGIN{if(t>0) printf "%.2f", (f/t)*100; else print "100"}')"
		echo "concurrent_chaos: error rate ${err_pct}%" | tee -a "$report"
		if awk -v e="$err_pct" 'BEGIN{exit !(e < 5)}'; then
			add_check "load" "concurrent chaos load" "pass" "trx=${trx} err%=${err_pct}"
		else
			record_failure "concurrent chaos error rate too high: ${err_pct}%"
			add_check "load" "concurrent chaos load" "fail" "trx=${trx} err%=${err_pct}"
		fi
	else
		add_check "load" "concurrent chaos load" "warn" "no transactions"
	fi
elif [[ -f "$out" ]]; then
	echo "concurrent_chaos: curl results: $(cat "$out")" | tee -a "$report"
	add_check "load" "concurrent chaos load (curl)" "pass"
fi

if [[ "$KEEP_REPORT" != "true" ]]; then
	rm -f "$out"
fi

for name in "${STOPPED_REPLICAS[@]+"${STOPPED_REPLICAS[@]}"}"; do
	start_replica "$name"
done
sleep 3
discover_replicas 2>/dev/null || true

probe_leaders "after chaos"
probe_endpoints "after chaos"
probe_metrics "after chaos"

bench_finish
