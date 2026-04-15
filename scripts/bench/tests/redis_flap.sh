#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${REDIS_FLAP_CYCLES:=3}"
: "${REDIS_FLAP_DOWN_SECS:=3}"
: "${REDIS_FLAP_UP_SECS:=5}"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
	echo "waiting for non-empty check results..." | tee -a "$report"
	if ! wait_for_checks; then
		echo "checks did not become ready in time" | tee -a "$report"
		record_failure "checks did not become ready: redis flap"
		add_check "api" "checks ready" "fail"
	else
		add_check "api" "checks ready" "pass"
	fi
fi

for cycle in $(seq 1 "$REDIS_FLAP_CYCLES"); do
	echo "" | tee -a "$report"
	echo "redis flap cycle ${cycle}: down" | tee -a "$report"
	compose stop redis | tee -a "$report"
	add_check "steps" "redis stop cycle ${cycle}" "pass"
	sleep "$REDIS_FLAP_DOWN_SECS"
	probe_endpoints "redis down cycle ${cycle}"
	probe_metrics "redis down cycle ${cycle}"

	echo "redis flap cycle ${cycle}: up" | tee -a "$report"
	compose start redis | tee -a "$report"
	add_check "steps" "redis start cycle ${cycle}" "pass"
	sleep "$REDIS_FLAP_UP_SECS"
	if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
		if ! wait_for_checks; then
			record_failure "checks did not become ready after redis flap ${cycle}"
			add_check "api" "checks ready after redis flap ${cycle}" "fail"
		else
			add_check "api" "checks ready after redis flap ${cycle}" "pass"
		fi
	fi
	probe_endpoints "redis up cycle ${cycle}"
	probe_metrics "redis up cycle ${cycle}"

done

bench_finish
