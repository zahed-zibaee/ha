#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

: "${MASSIVE_TESTS:=consistency leader health api loadbalancer distribution latency concurrency resilience redis_flap cold_start churn chaos}"
: "${MASSIVE_KEEP_REPORTS:=false}"
: "${MASSIVE_VERBOSE:=false}"
: "${MASSIVE_PROGRESS_INTERVAL:=15}"

now_ts() {
	date -u +%Y-%m-%dT%H:%M:%SZ
}

now_epoch() {
	date -u +%s
}

run_test() {
	local name="$1"
	local path="$ROOT_DIR/scripts/bench/tests/${name}.sh"
	if [[ ! -x "$path" ]]; then
		echo "missing test: ${name}" >&2
		exit 1
	fi
	local report="$2"
	local status=0
	shift 2
	local -a extra_env=()
	extra_env=("TEST_NAME=$name")
	if [[ "$name" == "cold_start" ]]; then
		extra_env+=("COLD_START_USE_DOWN=false")
	fi
	if [[ "$name" == "distribution" ]]; then
		extra_env+=("LB_DIST_STRICT=false")
	fi
	if [[ "$MASSIVE_VERBOSE" == "true" ]]; then
		PRINT_SUMMARY=true PRINT_RAW_REPORT=true KEEP_REPORT=true REPORT_FILE="$report" env "${extra_env[@]}" "$path" "$@" 1>&2 || status=$?
	else
		PRINT_SUMMARY=false PRINT_RAW_REPORT=false KEEP_REPORT=true QUIET=true REPORT_FILE="$report" env "${extra_env[@]}" "$path" "$@" >/dev/null || status=$?
	fi
	echo "$status"
}

tmp_dir="$(mktemp -d -t ha-bench-massive-XXXXXX)"
declare -a tests reports statuses summaries overalls failures durations

progress_pid=""
start_progress() {
	local name="$1"
	if [[ "${MASSIVE_PROGRESS_INTERVAL}" -le 0 ]]; then
		echo "running: ${name}..."
		return
	fi
	(
		while true; do
			echo "running: ${name}..."
			sleep "$MASSIVE_PROGRESS_INTERVAL"
		done
	) &
	progress_pid=$!
}

stop_progress() {
	if [[ -n "$progress_pid" ]]; then
		kill "$progress_pid" >/dev/null 2>&1 || true
		wait "$progress_pid" >/dev/null 2>&1 || true
		progress_pid=""
	fi
}

print_test_report() {
	local name="$1"
	local report="$2"
	local summary
	if [[ -s "$report" ]]; then
		summary="$(awk 'BEGIN{p=0} /^Benchmark Summary$/ {p=1} p {print}' "$report")"
	fi
	echo "test ${name} report:"
	if [[ -n "${summary:-}" ]]; then
		echo "$summary"
	else
		echo "(no summary found)"
	fi
}

for t in $MASSIVE_TESTS; do
	report="${tmp_dir}/${t}.txt"
	: >"$report"
	echo ""
	echo "starting ${t}..."
	echo "TEST_START name=${t} ts=$(now_ts)"
	start_epoch="$(now_epoch)"
	start_progress "$t"
	status="$(run_test "$t" "$report" "$@")"
	stop_progress
	end_epoch="$(now_epoch)"
	duration=$((end_epoch - start_epoch))
	echo "TEST_END name=${t} ts=$(now_ts)"
	echo "finished ${t} (exit=${status} duration=${duration}s)"
	print_test_report "$t" "$report"
	status="${status##*$'\n'}"
	if [[ ! "$status" =~ ^[0-9]+$ ]]; then
		failures+=("${t}: (non-numeric status '${status}')")
		status=1
	fi
	tests+=("$t")
	reports+=("$report")
	statuses+=("$status")
	durations+=("$duration")
	if [[ ! -s "$report" ]]; then
		summaries+=("")
		overalls+=("unknown")
		failures+=("${t}: (no report)")
		continue
	fi
	summary="$(awk 'BEGIN{p=0} /^Benchmark Summary$/ {p=1} p {print}' "$report")"
	summaries+=("$summary")
	overall="$(echo "$summary" | awk '/^overall:/ {print $2; exit}')"
	overalls+=("${overall:-unknown}")
	fblock="$(echo "$summary" | awk 'f{print} /^Failures$/ {f=1}')"
	if [[ -n "$fblock" ]]; then
		while IFS= read -r line; do
			if [[ -n "$line" && "$line" != TEST_END* ]]; then
				failures+=("${t}: ${line}")
			fi
		done <<< "$fblock"
	fi
done

overall_pass=1
for i in "${!tests[@]}"; do
	status="${statuses[$i]:-1}"
	overall="${overalls[$i]:-unknown}"
	if [[ "$status" -ne 0 || "$overall" != "PASS" ]]; then
		overall_pass=0
	fi
done

echo ""
echo "Massive Summary"
if [[ "$overall_pass" -eq 1 ]]; then
	echo "overall: PASS"
else
	echo "overall: FAIL"
fi
echo ""
echo "Tests"
for i in "${!tests[@]}"; do
	name="${tests[$i]}"
	status="${statuses[$i]:-1}"
	overall="${overalls[$i]:-unknown}"
	duration="${durations[$i]:-0}"
	if [[ "$status" -eq 0 && "$overall" == "PASS" ]]; then
		echo "- PASS ${name} (duration=${duration}s)"
	else
		echo "- FAIL ${name} (exit=${status} overall=${overall} duration=${duration}s)"
	fi
done

if [[ "${#failures[@]}" -gt 0 ]]; then
	echo ""
	echo "Failures"
	for f in "${failures[@]}"; do
		echo "- ${f}"
	done
fi

echo ""
echo "Details"
for i in "${!tests[@]}"; do
	echo ""
	echo "=== ${tests[$i]} ==="
	if [[ -n "${summaries[$i]}" ]]; then
		echo "${summaries[$i]}"
	else
		echo "(no summary found)"
	fi
done

if [[ "$MASSIVE_KEEP_REPORTS" == "true" ]]; then
	echo ""
	echo "reports dir: ${tmp_dir}"
else
	rm -rf "$tmp_dir"
fi
