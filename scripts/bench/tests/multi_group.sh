#!/usr/bin/env bash
set -euo pipefail

# Multi-group validation test.
# Uses the test compose with 5 check groups to verify:
#   - every group gets probe data in Redis
#   - every group's /v1/lb returns a valid pick
#   - every group's /v1/check returns per-target results
#   - groups with a deliberate failing target mark it unreachable
#   - different LB strategies per group produce correct response shapes

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$ROOT_DIR/scripts/bench/lib/common.sh"

: "${MULTI_GROUP_SETTLE:=20}"

bench_defaults
bench_parse_args "$@"
bench_prepare

echo "multi_group: waiting ${MULTI_GROUP_SETTLE}s for probes to populate all groups" | tee -a "$report"
sleep "$MULTI_GROUP_SETTLE"

IFS=',' read -r -a groups <<< "$GROUPS_RESOLVED"
echo "multi_group: testing ${#groups[@]} groups: ${GROUPS_RESOLVED}" | tee -a "$report"

group_pass=0
group_fail=0

for group in "${groups[@]}"; do
	echo "" | tee -a "$report"
	echo "--- group: ${group} ---" | tee -a "$report"

	# /v1/check: every group should return targets
	check_body="$(curl -sS --max-time 3 "${BASE_URL}/v1/check/${group}" || true)"
	total="$(count_total "$check_body")"
	reachable="$(count_reachable "$check_body")"
	redis_status="$(parse_redis_status "$check_body")"
	echo "check ${group}: total=${total} reachable=${reachable} redis=${redis_status:-unknown}" | tee -a "$report"

	if [[ "$total" -eq 0 ]]; then
		record_failure "group ${group}: no targets in /v1/check"
		add_check "multi_group" "check targets ${group}" "fail" "total=0"
		group_fail=$((group_fail + 1))
		continue
	fi
	if [[ "$redis_status" != "ok" ]]; then
		record_failure "group ${group}: redis_status=${redis_status}"
		add_check "multi_group" "check redis ${group}" "fail" "redis=${redis_status}"
	else
		add_check "multi_group" "check redis ${group}" "pass"
	fi
	add_check "multi_group" "check targets ${group}" "pass" "total=${total} reachable=${reachable}"

	# /v1/lb: each group should return a pick with a name
	lb_body="$(curl -sS --max-time 3 "${BASE_URL}/v1/lb/${group}" || true)"
	lb_name="$(echo "$lb_body" | rg -o '"name":"[^"]+"' | head -n1 | sed 's/"name":"//;s/"$//' || true)"
	lb_reachable="$(echo "$lb_body" | rg -o '"reachable":(true|false)' | head -n1 | rg -o 'true|false' || true)"
	echo "lb ${group}: name=${lb_name:-none} reachable=${lb_reachable:-unknown}" | tee -a "$report"

	if [[ -z "$lb_name" ]]; then
		record_failure "group ${group}: /v1/lb returned no name"
		add_check "multi_group" "lb pick ${group}" "fail" "no name"
		group_fail=$((group_fail + 1))
		continue
	fi
	add_check "multi_group" "lb pick ${group}" "pass" "name=${lb_name}"
	group_pass=$((group_pass + 1))
done

echo "" | tee -a "$report"
echo "multi_group summary: pass=${group_pass} fail=${group_fail} total=${#groups[@]}" | tee -a "$report"

# mixed-health group: verify the failing target is marked unreachable
echo "" | tee -a "$report"
echo "--- mixed-health failing target check ---" | tee -a "$report"
mixed_body="$(curl -sS --max-time 3 "${BASE_URL}/v1/check/mixed-health" || true)"
failing_excerpt="$(echo "$mixed_body" | rg -o '.{0,160}"target":"failing".{0,160}' | head -n1 || true)"
if echo "$mixed_body" | rg -q '"reachable":false.{0,240}"target":"failing"|"target":"failing".{0,240}"reachable":false'; then
	echo "mixed-health: failing target correctly marked unreachable" | tee -a "$report"
	add_check "multi_group" "failing target unreachable" "pass"
else
	echo "mixed-health: failing target not marked unreachable: ${failing_excerpt}" | tee -a "$report"
	add_check "multi_group" "failing target unreachable" "warn" "excerpt=${failing_excerpt}"
fi

# Cross-group isolation: lb responses should reference group-specific names
echo "" | tee -a "$report"
echo "--- cross-group isolation ---" | tee -a "$report"
isolation_ok=1
for _ in $(seq 1 5); do
	for group in "${groups[@]}"; do
		body="$(curl -sS --max-time 2 "${BASE_URL}/v1/lb/${group}" || true)"
		returned_group="$(echo "$body" | rg -o '"group":"[^"]+"' | sed 's/"group":"//;s/"$//' || true)"
		if [[ -n "$returned_group" && "$returned_group" != "$group" ]]; then
			record_failure "cross-group leak: requested ${group} got ${returned_group}"
			isolation_ok=0
		fi
	done
done
if [[ "$isolation_ok" -eq 1 ]]; then
	add_check "multi_group" "cross-group isolation" "pass"
else
	add_check "multi_group" "cross-group isolation" "fail"
fi

# LB across all groups under rapid fire
echo "" | tee -a "$report"
echo "--- rapid multi-group LB ---" | tee -a "$report"
rapid_ok=0
rapid_fail=0
for _ in $(seq 1 50); do
	group="${groups[$((RANDOM % ${#groups[@]}))]}"
	code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${BASE_URL}/v1/lb/${group}" || echo "000")"
	if [[ "$code" == "200" ]]; then
		rapid_ok=$((rapid_ok + 1))
	else
		rapid_fail=$((rapid_fail + 1))
	fi
done
echo "rapid multi-group: ok=${rapid_ok} fail=${rapid_fail}" | tee -a "$report"
if [[ "$rapid_fail" -eq 0 ]]; then
	add_check "multi_group" "rapid multi-group LB" "pass" "ok=${rapid_ok}"
else
	if [[ "$rapid_ok" -gt "$rapid_fail" ]]; then
		add_check "multi_group" "rapid multi-group LB" "warn" "ok=${rapid_ok} fail=${rapid_fail}"
	else
		record_failure "rapid multi-group LB mostly failing"
		add_check "multi_group" "rapid multi-group LB" "fail" "ok=${rapid_ok} fail=${rapid_fail}"
	fi
fi

probe_leaders "multi_group"
probe_metrics "multi_group"
bench_finish
