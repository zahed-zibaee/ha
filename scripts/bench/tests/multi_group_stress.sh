#!/usr/bin/env bash
set -euo pipefail

# Multi-group stress test.
# Blasts concurrent HTTP requests across ALL configured groups while
# performing chaos operations (replica kills, redis flaps). Validates
# that no group goes dark and error rates stay within bounds.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${STRESS_SETTLE:=15}"
: "${STRESS_DURATION:=40}"
: "${STRESS_CONCURRENCY_PER_GROUP:=10}"
: "${STRESS_CHAOS_INTERVAL:=6}"
: "${STRESS_CHAOS_STEPS:=5}"
: "${STRESS_MAX_ERROR_PCT:=15}"
: "${STRESS_RECOVERY_WAIT:=20}"
# Default 0: stopping Redis during load causes most LB requests to fail; set e.g. 20 to include Redis chaos.
: "${STRESS_REDIS_FLAP_PROB:=0}"

bench_defaults
bench_parse_args "$@"
bench_prepare
EXPECTED_REPLICAS="$(replica_count)"

wait_for_replica_metrics() {
	local expected="${1:-$EXPECTED_REPLICAS}"
	for _ in $(seq 1 "$STRESS_RECOVERY_WAIT"); do
		discover_replicas 2>/dev/null || true
		if [[ "${#REPLICA_URLS[@]}" -lt "$expected" ]]; then
			sleep 1
			continue
		fi
		local ready=1
		local idx
		for idx in "${!REPLICA_URLS[@]}"; do
			if ! metrics_body_for_replica "${REPLICA_CONTAINERS[$idx]}" >/dev/null 2>&1; then
				ready=0
				break
			fi
		done
		if [[ "$ready" -eq 1 && "${#REPLICA_URLS[@]}" -gt 0 ]]; then
			return 0
		fi
		sleep 1
	done
	return 1
}

echo "multi_group_stress: settling for ${STRESS_SETTLE}s" | tee -a "$report"
sleep "$STRESS_SETTLE"

IFS=',' read -r -a groups <<< "$GROUPS_RESOLVED"
echo "multi_group_stress: ${#groups[@]} groups, ${STRESS_CONCURRENCY_PER_GROUP} concurrent per group" | tee -a "$report"

# Pre-check: all groups should have data
for group in "${groups[@]}"; do
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "${BASE_URL}/v1/check/${group}" || echo "000")"
	if [[ "$code" != "200" ]]; then
		record_failure "pre-check: group ${group} not ready (${code})"
		add_check "stress" "pre-check ${group}" "fail" "code=${code}"
	else
		add_check "stress" "pre-check ${group}" "pass"
	fi
done

# Start concurrent load workers - one per group
declare -A load_pids
declare -A load_files
for group in "${groups[@]}"; do
	outfile="$(mktemp)"
	load_files["$group"]="$outfile"
	(
		end_time=$((SECONDS + STRESS_DURATION))
		ok=0
		fail=0
		while [[ $SECONDS -lt $end_time ]]; do
			base="$(pick_live_base_url "$BASE_URL")"
			for _ in $(seq 1 "$STRESS_CONCURRENCY_PER_GROUP"); do
				code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${base}/v1/lb/${group}" 2>/dev/null || echo "000")"
				if [[ "$code" != "200" ]]; then
					retry_base="$(pick_live_base_url "")"
					if [[ -n "$retry_base" ]]; then
						base="$retry_base"
						code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${base}/v1/lb/${group}" 2>/dev/null || echo "000")"
					fi
				fi
				if [[ "$code" == "200" ]]; then
					ok=$((ok + 1))
				else
					fail=$((fail + 1))
				fi
			done
			sleep 0.2
		done
		echo "${ok}|${fail}" >"$outfile"
	) &
	load_pids["$group"]=$!
done
echo "multi_group_stress: load workers started for ${#groups[@]} groups" | tee -a "$report"

# Chaos loop while load is running
redis_down=0
sleep 3
for step in $(seq 1 "$STRESS_CHAOS_STEPS"); do
	echo "" | tee -a "$report"
	echo "stress chaos step ${step}" | tee -a "$report"

	rnd=$((RANDOM % 100))
	if [[ "$rnd" -lt "$STRESS_REDIS_FLAP_PROB" ]]; then
		if [[ "$redis_down" -eq 0 ]]; then
			echo "stress: stopping redis" | tee -a "$report"
			compose stop redis 2>/dev/null | tee -a "$report" || true
			redis_down=1
			add_check "steps" "redis stop stress ${step}" "pass"
		else
			echo "stress: starting redis" | tee -a "$report"
			compose start redis 2>/dev/null | tee -a "$report" || true
			redis_down=0
			add_check "steps" "redis start stress ${step}" "pass"
		fi
	else
		read -r -a running_list <<< "$(running_replicas)"
		if [[ "${#running_list[@]}" -gt 1 ]]; then
			idx=$((RANDOM % ${#running_list[@]}))
			victim="${running_list[$idx]}"
			echo "stress: stopping ${victim}" | tee -a "$report"
			stop_replica "$victim"
			add_check "steps" "stop ${victim} stress ${step}" "pass"
			sleep 2
			echo "stress: restarting ${victim}" | tee -a "$report"
			start_replica "$victim"
			add_check "steps" "restart ${victim} stress ${step}" "pass"
			discover_replicas 2>/dev/null || true
		fi
	fi

	sleep "$STRESS_CHAOS_INTERVAL"
done

# Ensure redis is back up
if [[ "$redis_down" -eq 1 ]]; then
	echo "stress: ensuring redis is up" | tee -a "$report"
	compose start redis 2>/dev/null | tee -a "$report" || true
	redis_down=0
fi

# Restart any stopped replicas
for name in "${STOPPED_REPLICAS[@]+"${STOPPED_REPLICAS[@]}"}"; do
	start_replica "$name"
done

# Wait for all load workers
echo "" | tee -a "$report"
echo "multi_group_stress: waiting for load workers to finish" | tee -a "$report"
for group in "${groups[@]}"; do
	wait "${load_pids[$group]}" 2>/dev/null || true
done

# Collect results
echo "" | tee -a "$report"
echo "=== per-group load results ===" | tee -a "$report"
total_ok=0
total_fail=0
worst_group=""
worst_err_pct="0"
for group in "${groups[@]}"; do
	outfile="${load_files[$group]}"
	if [[ ! -s "$outfile" ]]; then
		echo "  ${group}: no data" | tee -a "$report"
		add_check "stress" "load ${group}" "warn" "no data"
		rm -f "$outfile"
		continue
	fi
	result="$(cat "$outfile")"
	rm -f "$outfile"
	ok="$(echo "$result" | cut -d'|' -f1)"
	fail="$(echo "$result" | cut -d'|' -f2)"
	total=$((ok + fail))
	err_pct="0"
	if [[ "$total" -gt 0 ]]; then
		err_pct="$(awk -v f="$fail" -v t="$total" 'BEGIN{printf "%.1f", (f/t)*100}')"
	fi
	echo "  ${group}: ok=${ok} fail=${fail} total=${total} err=${err_pct}%" | tee -a "$report"
	total_ok=$((total_ok + ok))
	total_fail=$((total_fail + fail))

	over_limit=$(awk -v e="$err_pct" -v l="$STRESS_MAX_ERROR_PCT" 'BEGIN{print (e>l)?1:0}')
	if [[ "$over_limit" -eq 1 ]]; then
		record_failure "group ${group}: error rate ${err_pct}% exceeds ${STRESS_MAX_ERROR_PCT}%"
		add_check "stress" "load ${group}" "fail" "ok=${ok} fail=${fail} err=${err_pct}%"
	else
		add_check "stress" "load ${group}" "pass" "ok=${ok} fail=${fail} err=${err_pct}%"
	fi

	worse=$(awk -v a="$err_pct" -v b="$worst_err_pct" 'BEGIN{print (a>b)?1:0}')
	if [[ "$worse" -eq 1 ]]; then
		worst_err_pct="$err_pct"
		worst_group="$group"
	fi
done

grand_total=$((total_ok + total_fail))
grand_err="0"
if [[ "$grand_total" -gt 0 ]]; then
	grand_err="$(awk -v f="$total_fail" -v t="$grand_total" 'BEGIN{printf "%.1f", (f/t)*100}')"
fi
echo "" | tee -a "$report"
echo "aggregate: ok=${total_ok} fail=${total_fail} total=${grand_total} err=${grand_err}% worst_group=${worst_group:-none}(${worst_err_pct}%)" | tee -a "$report"

over_limit=$(awk -v e="$grand_err" -v l="$STRESS_MAX_ERROR_PCT" 'BEGIN{print (e>l)?1:0}')
if [[ "$over_limit" -eq 1 ]]; then
	record_failure "aggregate error rate ${grand_err}% exceeds ${STRESS_MAX_ERROR_PCT}%"
	add_check "stress" "aggregate load" "fail" "ok=${total_ok} fail=${total_fail} err=${grand_err}%"
else
	add_check "stress" "aggregate load" "pass" "ok=${total_ok} fail=${total_fail} err=${grand_err}%"
fi

# Post-stress: verify all groups recovered
echo "" | tee -a "$report"
echo "=== post-stress group health ===" | tee -a "$report"
sleep 10
refresh_live_base_url
echo "post-stress base_url=${BASE_URL}" | tee -a "$report"
groups_ok=0
for group in "${groups[@]}"; do
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "${BASE_URL}/v1/check/${group}" || echo "000")"
	if [[ "$code" == "200" ]]; then
		groups_ok=$((groups_ok + 1))
	else
		echo "group ${group}: post-stress check returned ${code}" | tee -a "$report"
	fi
done
if [[ "$groups_ok" -eq "${#groups[@]}" ]]; then
	add_check "stress" "post-stress recovery" "pass" "${groups_ok}/${#groups[@]}"
else
	record_failure "post-stress: ${groups_ok}/${#groups[@]} groups healthy"
	add_check "stress" "post-stress recovery" "fail" "${groups_ok}/${#groups[@]}"
fi

if ! wait_for_all_replicas_health; then
	echo "multi_group_stress: not every replica became healthy before final metrics probe" | tee -a "$report"
fi
if ! wait_for_replica_metrics "$EXPECTED_REPLICAS"; then
	echo "multi_group_stress: metrics did not settle on every replica before final probe" | tee -a "$report"
fi
probe_metrics "multi_group_stress final"
bench_finish
