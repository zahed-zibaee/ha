#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
	echo "waiting for non-empty check results..." | tee -a "$report"
	if ! wait_for_checks; then
		echo "checks did not become ready in time" | tee -a "$report"
		record_failure "checks did not become ready: concurrency"
		add_check "api" "checks ready" "fail"
	else
		add_check "api" "checks ready" "pass"
	fi
fi

probe_metrics "before load"
run_siege "concurrency"
probe_metrics "after load"
bench_finish
