#!/usr/bin/env bash
set -euo pipefail

# Goroutine / memory leak detector.
# Runs the multi-group test config and performs repeated leader churn cycles
# while sampling go_goroutines and process_resident_memory_bytes from /metrics.
# Fails if goroutine count grows unboundedly or memory ratchets up significantly.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${LEAK_SETTLE:=15}"
: "${LEAK_CHURN_CYCLES:=6}"
: "${LEAK_CYCLE_SLEEP:=8}"
: "${LEAK_GOROUTINE_GROWTH_LIMIT:=50}"
: "${LEAK_MEM_GROWTH_PCT:=80}"

bench_defaults
bench_parse_args "$@"
bench_prepare

echo "goroutine_leak: settling for ${LEAK_SETTLE}s" | tee -a "$report"
sleep "$LEAK_SETTLE"

extract_metric() {
	local body="$1"
	local name="$2"
	echo "$body" | rg "^${name} " | head -n1 | awk '{print $2}' | tr -d '\r'
}

sample_all_replicas() {
	local label="$1"
	local max_goroutines=0
	local max_mem=0
	for i in "${!REPLICA_URLS[@]}"; do
		local url="${REPLICA_URLS[$i]}"
		local name="${REPLICA_CONTAINERS[$i]}"
		local body
		body="$(curl -sS --max-time 3 "${url}/metrics" 2>/dev/null || true)"
		if [[ -z "$body" ]]; then
			echo "  ${name}: unreachable" | tee -a "$report"
			continue
		fi
		local gr
		gr="$(extract_metric "$body" "go_goroutines")"
		local mem
		mem="$(extract_metric "$body" "process_resident_memory_bytes")"
		local mem_mb=""
		if [[ -n "$mem" ]]; then
			mem_mb="$(awk -v m="$mem" 'BEGIN{printf "%.1f", m/1048576}')"
		fi
		echo "  ${name}: goroutines=${gr:-?} mem=${mem_mb:-?}MB" | tee -a "$report"
		if [[ -n "$gr" ]]; then
			local cmp
			cmp=$(awk -v a="$gr" -v b="$max_goroutines" 'BEGIN{print (a>b)?1:0}')
			if [[ "$cmp" -eq 1 ]]; then max_goroutines="$gr"; fi
		fi
		if [[ -n "$mem" ]]; then
			local cmp
			cmp=$(awk -v a="$mem" -v b="$max_mem" 'BEGIN{print (a>b)?1:0}')
			if [[ "$cmp" -eq 1 ]]; then max_mem="$mem"; fi
		fi
	done
	echo "${max_goroutines}|${max_mem}"
}

echo "" | tee -a "$report"
echo "=== baseline sample ===" | tee -a "$report"
baseline="$(sample_all_replicas "baseline")"
base_gr="$(echo "$baseline" | cut -d'|' -f1)"
base_mem="$(echo "$baseline" | cut -d'|' -f2)"
echo "baseline: max_goroutines=${base_gr} max_mem_bytes=${base_mem}" | tee -a "$report"

if [[ -z "$base_gr" || "$base_gr" == "0" ]]; then
	echo "goroutine_leak: could not read baseline goroutine count" | tee -a "$report"
	add_check "leak" "baseline metrics" "warn" "goroutines=${base_gr}"
	bench_finish
	exit 0
fi
add_check "leak" "baseline metrics" "pass" "goroutines=${base_gr}"

declare -a goroutine_samples=("$base_gr")
declare -a mem_samples=("$base_mem")

for cycle in $(seq 1 "$LEAK_CHURN_CYCLES"); do
	echo "" | tee -a "$report"
	echo "=== churn cycle ${cycle}/${LEAK_CHURN_CYCLES} ===" | tee -a "$report"

	# Kill a random replica to trigger leader change
	read -r -a running_list <<< "$(running_replicas)"
	if [[ "${#running_list[@]}" -gt 1 ]]; then
		idx=$((RANDOM % ${#running_list[@]}))
		victim="${running_list[$idx]}"
		echo "churn: stopping ${victim}" | tee -a "$report"
		stop_replica "$victim"
		add_check "steps" "stop ${victim} cycle ${cycle}" "pass"
		sleep 3

		echo "churn: restarting ${victim}" | tee -a "$report"
		start_replica "$victim"
		add_check "steps" "restart ${victim} cycle ${cycle}" "pass"
	fi

	sleep "$LEAK_CYCLE_SLEEP"
	discover_replicas 2>/dev/null || true

	echo "--- sample after cycle ${cycle} ---" | tee -a "$report"
	result="$(sample_all_replicas "cycle ${cycle}")"
	gr="$(echo "$result" | cut -d'|' -f1)"
	mem="$(echo "$result" | cut -d'|' -f2)"
	echo "cycle ${cycle}: max_goroutines=${gr} max_mem_bytes=${mem}" | tee -a "$report"
	goroutine_samples+=("$gr")
	mem_samples+=("$mem")
done

echo "" | tee -a "$report"
echo "=== leak analysis ===" | tee -a "$report"

final_gr="${goroutine_samples[$(( ${#goroutine_samples[@]} - 1 ))]}"
gr_growth=$((final_gr - base_gr))
echo "goroutine growth: ${base_gr} -> ${final_gr} (delta=${gr_growth}, limit=${LEAK_GOROUTINE_GROWTH_LIMIT})" | tee -a "$report"
echo "goroutine samples: ${goroutine_samples[*]}" | tee -a "$report"

if [[ "$gr_growth" -le "$LEAK_GOROUTINE_GROWTH_LIMIT" ]]; then
	add_check "leak" "goroutine growth" "pass" "base=${base_gr} final=${final_gr} delta=${gr_growth}"
else
	record_failure "goroutine growth exceeded limit: delta=${gr_growth} limit=${LEAK_GOROUTINE_GROWTH_LIMIT}"
	add_check "leak" "goroutine growth" "fail" "base=${base_gr} final=${final_gr} delta=${gr_growth}"
fi

# Check for monotonic goroutine increase (strong leak signal)
monotonic=1
for i in $(seq 1 $((${#goroutine_samples[@]} - 1))); do
	prev="${goroutine_samples[$((i - 1))]}"
	curr="${goroutine_samples[$i]}"
	if [[ "$curr" -le "$prev" ]]; then
		monotonic=0
		break
	fi
done
if [[ "$monotonic" -eq 1 && "${#goroutine_samples[@]}" -ge 4 ]]; then
	record_failure "goroutine count monotonically increasing across ${#goroutine_samples[@]} samples"
	add_check "leak" "goroutine monotonic" "fail" "samples=${goroutine_samples[*]}"
else
	add_check "leak" "goroutine monotonic" "pass" "not monotonic"
fi

# Memory analysis
if [[ -n "$base_mem" && "$base_mem" != "0" ]]; then
	final_mem="${mem_samples[$(( ${#mem_samples[@]} - 1 ))]}"
	if [[ -n "$final_mem" && "$final_mem" != "0" ]]; then
		mem_growth_pct="$(awk -v b="$base_mem" -v f="$final_mem" 'BEGIN{if(b>0) printf "%.0f", ((f-b)/b)*100; else print "0"}')"
		base_mb="$(awk -v m="$base_mem" 'BEGIN{printf "%.1f", m/1048576}')"
		final_mb="$(awk -v m="$final_mem" 'BEGIN{printf "%.1f", m/1048576}')"
		echo "memory growth: ${base_mb}MB -> ${final_mb}MB (${mem_growth_pct}%, limit=${LEAK_MEM_GROWTH_PCT}%)" | tee -a "$report"
		echo "memory samples bytes: ${mem_samples[*]}" | tee -a "$report"

		if [[ "$mem_growth_pct" -le "$LEAK_MEM_GROWTH_PCT" ]]; then
			add_check "leak" "memory growth" "pass" "base=${base_mb}MB final=${final_mb}MB growth=${mem_growth_pct}%"
		else
			record_failure "memory growth exceeded: ${mem_growth_pct}% > ${LEAK_MEM_GROWTH_PCT}%"
			add_check "leak" "memory growth" "fail" "base=${base_mb}MB final=${final_mb}MB growth=${mem_growth_pct}%"
		fi
	else
		add_check "leak" "memory growth" "warn" "final mem unavailable"
	fi
else
	add_check "leak" "memory growth" "warn" "baseline mem unavailable"
fi

# Verify all groups still returning data after churn
echo "" | tee -a "$report"
echo "=== post-churn group health ===" | tee -a "$report"
refresh_live_base_url
echo "post-churn base_url=${BASE_URL}" | tee -a "$report"
IFS=',' read -r -a groups <<< "$GROUPS_RESOLVED"
groups_ok=0
for group in "${groups[@]}"; do
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "${BASE_URL}/v1/check/${group}" || echo "000")"
	if [[ "$code" == "200" ]]; then
		groups_ok=$((groups_ok + 1))
	else
		echo "group ${group}: check endpoint returned ${code}" | tee -a "$report"
	fi
done
echo "post-churn group health: ${groups_ok}/${#groups[@]} groups responding" | tee -a "$report"
if [[ "$groups_ok" -eq "${#groups[@]}" ]]; then
	add_check "leak" "post-churn groups" "pass" "${groups_ok}/${#groups[@]}"
else
	record_failure "some groups unhealthy after churn: ${groups_ok}/${#groups[@]}"
	add_check "leak" "post-churn groups" "fail" "${groups_ok}/${#groups[@]}"
fi

probe_metrics "goroutine_leak final"
bench_finish
