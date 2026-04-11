#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

bench_defaults
bench_parse_args "$@"

run_resilience() {
	bench_prepare

	if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
		echo "waiting for Redis lock leader (or degraded probes if Redis down)..." | tee -a "$report"
		if ! wait_for_leader; then
			echo "leader did not converge in time" | tee -a "$report"
			record_failure "leader did not converge: before"
			emit_leader_logs "before"
			add_check "leader" "leader converge before" "fail"
		else
			add_check "leader" "leader converge before" "pass"
		fi
	fi

	probe_leaders "before"
	local leader_before="${leader_detected:-}"
	if [[ -z "$leader_before" ]]; then
		leader_before="$(detect_leader)"
		echo "leader before (logs): ${leader_before:-unknown}" | tee -a "$report"
	fi

	if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
		echo "waiting for non-empty check results..." | tee -a "$report"
		if ! wait_for_checks; then
			echo "checks did not become ready in time" | tee -a "$report"
			record_failure "checks did not become ready: before"
			add_check "api" "checks ready before" "fail"
		else
			add_check "api" "checks ready before" "pass"
		fi
	fi

	probe_endpoints "before"
	probe_metrics "before"
	run_siege "baseline"

	local stopped=""
	if [[ "$STOP_TARGET" != "none" ]]; then
		if [[ "$STOP_TARGET" == "leader" ]]; then
			stopped="${leader_before:-${REPLICA_CONTAINERS[0]}}"
		elif [[ "$STOP_TARGET" =~ ^[0-9]+$ ]]; then
			local idx=$(( STOP_TARGET - 1 ))
			stopped="${REPLICA_CONTAINERS[$idx]:-${REPLICA_CONTAINERS[0]}}"
		else
			stopped="$STOP_TARGET"
		fi
		echo "" | tee -a "$report"
		probe_metrics "before stop"
		echo "stopping ${stopped}" | tee -a "$report"
		stop_replica "$stopped"
		add_check "steps" "stop ${stopped}" "pass"
		sleep 2
	fi

	if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
		echo "waiting for leader after stop..." | tee -a "$report"
		if ! wait_for_leader; then
			echo "leader did not converge after stop" | tee -a "$report"
			record_failure "leader did not converge: after stop"
			emit_leader_logs "after stop"
			add_check "leader" "leader converge after stop" "fail"
		else
			add_check "leader" "leader converge after stop" "pass"
		fi
	fi

	probe_leaders "after stop"
	local leader_after="${leader_detected:-}"
	if [[ -z "$leader_after" ]]; then
		leader_after="$(detect_leader)"
		echo "leader after (logs): ${leader_after:-unknown}" | tee -a "$report"
	fi

	if [[ -n "$stopped" && "$stopped" == "$(echo "$BASE_URL" | sed -n 's#http://\([^:]*\).*#\1#p')" ]]; then
		local new_base
		new_base="$(pick_live_base_url "$leader_after")"
		if [[ "$new_base" != "$BASE_URL" ]]; then
			echo "base url moved from ${BASE_URL} to ${new_base}" | tee -a "$report"
			BASE_URL="$new_base"
		fi
	elif ! curl -fsS --max-time 2 "$BASE_URL/health" >/dev/null 2>&1; then
		local new_base
		new_base="$(pick_live_base_url "$leader_after")"
		if [[ "$new_base" != "$BASE_URL" ]]; then
			echo "base url moved from ${BASE_URL} to ${new_base} (health check failed)" | tee -a "$report"
			BASE_URL="$new_base"
		fi
	fi

	probe_endpoints "after stop"
	probe_metrics "after stop"
	run_siege "after-stop"

	local rejoined=""
	if [[ -n "$stopped" && "$RESTART_STOPPED" == "true" ]]; then
		echo "" | tee -a "$report"
		echo "restarting ${stopped}" | tee -a "$report"
		start_replica "$stopped"
		rejoined="$stopped"
		add_check "steps" "restart ${stopped}" "pass"
	fi

	if [[ "$REJOIN_TEST" == "true" && -n "$rejoined" ]]; then
		if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
			echo "waiting for leader after restart..." | tee -a "$report"
			if ! wait_for_leader; then
				echo "leader did not converge after restart" | tee -a "$report"
				record_failure "leader did not converge: after restart"
				emit_leader_logs "after restart"
				add_check "leader" "leader converge after restart" "fail"
			else
				add_check "leader" "leader converge after restart" "pass"
			fi
		fi
		probe_leaders "after restart"
		if [[ -n "$leader_detected" ]]; then
			echo "rejoin: leader=${leader_detected} restarted=${rejoined}" | tee -a "$report"
			add_check "leader" "rejoin after restart" "pass" "leader=${leader_detected} restarted=${rejoined}"
		else
			record_failure "rejoin failed after restart"
			add_check "leader" "rejoin after restart" "fail"
		fi
	fi

	if [[ "$ROTATE_LEADER_TEST" == "true" && -n "$rejoined" ]]; then
		local local_leader="${leader_detected:-}"
		if [[ -z "$local_leader" ]]; then
			local_leader="$(detect_leader)"
		fi
		if [[ -n "$local_leader" ]]; then
			echo "" | tee -a "$report"
			probe_metrics "before rotation stop"
			echo "rotation: stopping leader ${local_leader}" | tee -a "$report"
			stop_replica "$local_leader"
			add_check "steps" "stop ${local_leader} (rotation)" "pass"
			sleep 2
			if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
				echo "waiting for leader after rotation..." | tee -a "$report"
				if ! wait_for_leader; then
					echo "leader did not converge after rotation" | tee -a "$report"
					record_failure "leader did not converge: after rotation"
					emit_leader_logs "after rotation"
					add_check "leader" "leader converge after rotation" "fail"
				else
					add_check "leader" "leader converge after rotation" "pass"
				fi
			fi
			probe_leaders "after rotation"
			if [[ -n "$leader_detected" ]]; then
				if [[ "$leader_detected" == "$rejoined" ]]; then
					echo "rotation: restarted node elected leader" | tee -a "$report"
				else
					echo "rotation: leader=${leader_detected} restarted=${rejoined}" | tee -a "$report"
				fi
				add_check "leader" "leader rotation" "pass" "leader=${leader_detected} restarted=${rejoined}"
			else
				record_failure "leader rotation failed"
				add_check "leader" "leader rotation" "fail"
			fi
			echo "rotation: restarting ${local_leader}" | tee -a "$report"
			start_replica "$local_leader"
			add_check "steps" "restart ${local_leader} (rotation)" "pass"
		fi
	fi

	if [[ "$REDIS_TEST" == "true" ]]; then
		echo "" | tee -a "$report"
		probe_metrics "before redis stop"
		echo "stopping redis" | tee -a "$report"
		compose stop redis | tee -a "$report"
		add_check "steps" "redis stop" "pass"
		sleep 2
		probe_endpoints "redis down"
		probe_metrics "redis down"
		echo "restarting redis" | tee -a "$report"
		compose start redis | tee -a "$report"
		add_check "steps" "redis restart" "pass"
		if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
			echo "waiting for checks after redis restart..." | tee -a "$report"
			if ! wait_for_checks; then
				echo "checks did not become ready after redis restart" | tee -a "$report"
				record_failure "checks did not become ready: after redis restart"
				add_check "api" "checks ready after redis restart" "fail"
			else
				add_check "api" "checks ready after redis restart" "pass"
			fi
		fi
		probe_endpoints "after redis restart"
		probe_metrics "after redis restart"
	fi

	bench_finish
}

run_resilience
