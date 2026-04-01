#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${LB_DIST_SAMPLES:=1000}"
: "${LB_DIST_TOLERANCE:=0.35}"
: "${LB_DIST_BATCH:=20}"
: "${LB_DIST_BATCH_SLEEP:=6}"
: "${DIST_SKIP_STARTUP_STABILIZE:=true}"
: "${LB_DIST_STRICT:=false}"

if [[ "$DIST_SKIP_STARTUP_STABILIZE" == "true" ]]; then
	STABILIZE_ON_START=false
fi

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
	echo "waiting for non-empty check results..." | tee -a "$report"
	if ! wait_for_checks; then
		echo "checks did not become ready in time" | tee -a "$report"
		add_check "api" "checks ready" "warn"
	else
		add_check "api" "checks ready" "pass"
	fi
fi

reachable_names() {
	local body="$1"
	local matches names
	matches="$(echo "$body" | rg -o '"reachable":true[^}]*"target":"[^"]+"' || true)"
	if [[ -z "$matches" ]]; then
		echo ""
		return
	fi
	names="$(echo "$matches" | rg -o '"target":"[^"]+"' | sed 's/"target":"//;s/"$//' | sort -u | paste -sd, -)"
	echo "$names"
}

sort_names() {
	echo "$1" | tr ',' '\n' | sed '/^$/d' | sort -u | paste -sd, -
}

count_names() {
	if [[ -z "$1" ]]; then
		echo 0
		return
	fi
	echo "$1" | tr ',' '\n' | sed '/^$/d' | wc -l | tr -d ' '
}

check_body_before="$(curl -sS --max-time 2 "${BASE_URL}/v1/check/${GROUP}" || true)"
reachable_before="$(reachable_names "$check_body_before")"
reachable_before="$(sort_names "$reachable_before")"
reachable_before_count="$(count_names "$reachable_before")"
if [[ "$reachable_before_count" -lt 2 ]]; then
	add_check "api" "lb distribution" "warn" "reachable=${reachable_before_count} (need >=2)"
	bench_finish
	exit 0
fi

sample_counts() {
	local samples="$1"
	local batch="${2:-0}"
	local batch_sleep="${3:-0}"
	declare -A counts
	local errors=0
	if [[ "$batch" -le 0 || "$batch" -ge "$samples" ]]; then
		batch="$samples"
	fi
	local total=0
	while [[ "$total" -lt "$samples" ]]; do
		local remaining=$((samples - total))
		local take="$batch"
		if [[ "$remaining" -lt "$batch" ]]; then
			take="$remaining"
		fi
		for _ in $(seq 1 "$take"); do
			local body
			body="$(curl -sS --max-time 2 "${BASE_URL}/v1/lb/${GROUP}" || true)"
			local name
			name="$(echo "$body" | rg -o '"name":"[^"]+"' | head -n1 | sed 's/"name":"//;s/"$//' || true)"
			if [[ -z "$name" ]]; then
				errors=$((errors + 1))
				continue
			fi
			: "${counts["$name"]:=0}"
			((counts["$name"]+=1))
		done
		total=$((total + take))
		if [[ "$total" -lt "$samples" && "$batch_sleep" -gt 0 ]]; then
			sleep "$batch_sleep"
		fi
	done
	local keys=()
	for k in "${!counts[@]}"; do
		keys+=("$k")
	done
	echo "${errors}|${#keys[@]}|${keys[*]}"
	for k in "${keys[@]}"; do
		echo "${k}=${counts[$k]}"
	done
}

echo "distribution sampling: samples=${LB_DIST_SAMPLES} batch=${LB_DIST_BATCH} sleep=${LB_DIST_BATCH_SLEEP}s" | tee -a "$report"
result="$(sample_counts "$LB_DIST_SAMPLES" "$LB_DIST_BATCH" "$LB_DIST_BATCH_SLEEP")"
errors="$(echo "$result" | head -n1 | cut -d'|' -f1)"
unique="$(echo "$result" | head -n1 | cut -d'|' -f2)"
keys_line="$(echo "$result" | head -n1 | cut -d'|' -f3-)"

declare -A counts
while IFS= read -r line; do
	if [[ "$line" == *"="* ]]; then
		key="${line%%=*}"
		val="${line#*=}"
		counts["$key"]="$val"
	fi
done < <(echo "$result" | tail -n +2)

check_body_after="$(curl -sS --max-time 2 "${BASE_URL}/v1/check/${GROUP}" || true)"
reachable_after="$(reachable_names "$check_body_after")"
reachable_after="$(sort_names "$reachable_after")"
if [[ -z "$reachable_after" || "$reachable_after" != "$reachable_before" ]]; then
	add_check "api" "lb distribution" "warn" "reachable changed (${reachable_before} -> ${reachable_after:-unknown})"
	bench_finish
	exit 0
fi

if [[ "$errors" -gt 0 ]]; then
	record_failure "distribution: ${errors} errors"
	add_check "api" "lb distribution" "fail" "errors=${errors}"
	bench_finish
	exit 0
fi

if [[ "$unique" -le 1 ]]; then
	add_check "api" "lb distribution" "warn" "targets=${unique}"
	bench_finish
	exit 0
fi

expected=$(awk -v n="$LB_DIST_SAMPLES" -v u="$unique" 'BEGIN{printf "%.2f", n/u}')
max_dev=0
max_target=""
for k in "${!counts[@]}"; do
	val="${counts[$k]}"
	dev=$(awk -v v="$val" -v e="$expected" 'BEGIN{d=v-e; if(d<0)d=-d; printf "%.4f", d/e}')
	cmp=$(awk -v d="$dev" -v m="$max_dev" 'BEGIN{print (d>m)?1:0}')
	if [[ "$cmp" -eq 1 ]]; then
		max_dev="$dev"
		max_target="$k"
	fi
done

within=$(awk -v d="$max_dev" -v t="$LB_DIST_TOLERANCE" 'BEGIN{print (d<=t)?1:0}')
echo "note: LB cache can skew distribution; set LB_DIST_STRICT=true to enforce" | tee -a "$report"
if [[ "$within" -eq 1 ]]; then
	add_check "api" "lb distribution" "pass" "targets=${unique} max_dev=$(awk -v d="$max_dev" 'BEGIN{printf "%.2f", d*100}')%"
else
	if [[ "$LB_DIST_STRICT" == "true" ]]; then
		record_failure "distribution skew max_dev=${max_dev}"
		add_check "api" "lb distribution" "fail" "targets=${unique} max_dev=$(awk -v d="$max_dev" 'BEGIN{printf "%.2f", d*100}')% target=${max_target}"
	else
		add_check "api" "lb distribution" "warn" "targets=${unique} max_dev=$(awk -v d="$max_dev" 'BEGIN{printf "%.2f", d*100}')% target=${max_target}"
	fi
fi

probe_metrics "distribution"
bench_finish
