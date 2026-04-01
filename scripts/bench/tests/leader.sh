#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

bench_defaults
bench_parse_args "$@"
bench_prepare

if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
	echo "waiting for single leader..." | tee -a "$report"
	if ! wait_for_leader; then
		echo "leader did not converge in time" | tee -a "$report"
		record_failure "leader did not converge: leader-only"
		emit_leader_logs "leader-only"
		add_check "raft" "leader converge" "fail"
	else
		add_check "raft" "leader converge" "pass"
	fi
fi

probe_leaders "leader-only"
bench_finish
