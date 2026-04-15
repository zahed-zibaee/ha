#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# All sub-tests use scripts/bench/docker-compose.test.yml + config-targets.test.yaml (see bench_defaults in common.sh).
# Override with MASSIVE_TESTS="..." or COMPOSE_FILE / BENCH_CONFIG when invoking a single test.
: "${MASSIVE_TESTS:=consistency leader health api loadbalancer distribution latency concurrency resilience redis_flap cold_start churn chaos concurrent_chaos_load dns_failover leader_kill_during_probes full_restart multi_group goroutine_leak multi_group_stress}"
: "${MASSIVE_KEEP_REPORTS:=false}"
: "${MASSIVE_VERBOSE:=false}"
# Seconds between heartbeats while a sub-test runs (0 = disable). Sleeps once before the first line.
: "${MASSIVE_PROGRESS_INTERVAL:=20}"
# distribution.sh alone uses 1000 samples + long sleeps (~5m); under massive we shorten (override freely).
: "${MASSIVE_DIST_SAMPLES:=400}"
: "${MASSIVE_DIST_BATCH:=50}"
: "${MASSIVE_DIST_BATCH_SLEEP:=0}"

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
		extra_env+=(
			"LB_DIST_STRICT=false"
			"LB_DIST_SAMPLES=${MASSIVE_DIST_SAMPLES}"
			"LB_DIST_BATCH=${MASSIVE_DIST_BATCH}"
			"LB_DIST_BATCH_SLEEP=${MASSIVE_DIST_BATCH_SLEEP}"
		)
	fi
	if [[ "$name" == "resilience" ]]; then
		extra_env+=("WAIT_CHECKS_TIMEOUT=90")
	fi
	if [[ "$name" == "full_restart" ]]; then
		extra_env+=("WAIT_CHECKS_TIMEOUT=90" "BENCH_WAIT_URL_TRIES=60")
	fi
	if [[ "$name" == "cold_start" ]]; then
		extra_env+=("BENCH_WAIT_URL_TRIES=60" "WAIT_CHECKS_TIMEOUT=90")
	fi
	if [[ "$name" == "chaos" ]]; then
		# Leader loop ticks every 5s; need time for degraded (all-probe) mode after Redis stops.
		extra_env+=("CHAOS_SLEEP=8")
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
tests=()
reports=()
statuses=()
summaries=()
overalls=()
failures=()
durations=()

echo "" >&2
echo "massive: running ${MASSIVE_TESTS// /, }" >&2
echo "massive: each sub-test uses COMPOSE_FILE=scripts/bench/docker-compose.test.yml from repo root; first run may take several minutes (image build + health waits)." >&2
echo "massive: live sub-test logs: MASSIVE_VERBOSE=true $0   |   reuse images: $0 --no-build   |   heartbeats every ${MASSIVE_PROGRESS_INTERVAL}s (MASSIVE_PROGRESS_INTERVAL=0 to disable)" >&2
echo "massive: per-test log file e.g. tail -f ${tmp_dir}/consistency.txt" >&2
echo "massive: distribution uses LB_DIST_SAMPLES=${MASSIVE_DIST_SAMPLES} (not 1000) so the suite finishes in minutes; run distribution.sh alone for the full sample." >&2
echo "massive: stack includes mock-target nginx; Redis has no host port (no clash with a dev redis on 6379)." >&2
echo "" >&2

progress_pid=""
start_progress() {
	local name="$1"
	if [[ "${MASSIVE_PROGRESS_INTERVAL}" -le 0 ]]; then
		return
	fi
	(
		local interval="${MASSIVE_PROGRESS_INTERVAL}"
		local elapsed=0
		while true; do
			sleep "$interval"
			elapsed=$((elapsed + interval))
			echo "massive: still on '${name}' (${elapsed}s) - live log: tail -f the report path printed above (some tests are slow by design, e.g. distribution or siege)." >&2
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
	echo "massive: report file ${report} (tail -f for this test's log)" >&2
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
