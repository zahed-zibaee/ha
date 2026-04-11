# Common helpers for benchmark scripts.
# Works with deploy.replicas-based ha service (Docker Compose v2).

bench_defaults() {
	if [[ -z "${ROOT_DIR:-}" ]]; then
		echo "scripts/bench/lib/common.sh: set ROOT_DIR before bench_defaults" >&2
		exit 1
	fi
	# Single bench stack: mock-target + isolated Redis (no host 6379) + ha replicas.
	export COMPOSE_FILE="${COMPOSE_FILE:-scripts/bench/docker-compose.test.yml}"
	export BENCH_CONFIG="${BENCH_CONFIG:-$ROOT_DIR/scripts/bench/config-targets.test.yaml}"
	: "${GROUP:=web-health}"
	: "${GROUPS_OVERRIDE:=}"
	: "${AUTO_GROUPS:=true}"
	: "${BASE_URL:=}"
	: "${CONCURRENCY:=50}"
	: "${DURATION:=30s}"
	: "${STOP_TARGET:=leader}"
	: "${BUILD:=true}"
	: "${RESTART_STOPPED:=true}"
	: "${OUT_DIR:=/tmp/ha-bench}"
	: "${WAIT_FOR_CHECKS:=true}"
	: "${WAIT_CHECKS_TIMEOUT:=30}"
	: "${WAIT_FOR_LEADER:=true}"
	: "${WAIT_LEADER_TIMEOUT:=30}"
	: "${STABILIZE_ON_START:=true}"
	: "${METRICS:=true}"
	: "${REJOIN_TEST:=true}"
	: "${ROTATE_LEADER_TEST:=true}"
	: "${REDIS_TEST:=true}"
	: "${STRICT:=false}"
	: "${QUIET:=true}"
	: "${KEEP_REPORT:=false}"
	: "${REPORT_FILE:=}"
	: "${PRINT_SUMMARY:=true}"
	: "${PRINT_RAW_REPORT:=true}"
	: "${HA_INTERNAL_PORT:=8080}"
	# auto: try published host port (curl from your machine) then bridge IP. host|bridge: force one mode.
	: "${BENCH_REPLICA_URL_MODE:=auto}"
	: "${BENCH_DOCKER_PUBLISH_BIND:=127.0.0.1}"
	: "${BENCH_WAIT_URL_TRIES:=45}"
}

bench_now_ts() {
	date -u +%Y-%m-%dT%H:%M:%SZ
}

bench_test_name() {
	if [[ -n "${TEST_NAME:-}" ]]; then
		echo "$TEST_NAME"
		return
	fi
	basename "$0" .sh
}

bench_usage() {
	cat <<EOF
Usage: $0 [options]

Defaults (override via env): COMPOSE_FILE=scripts/bench/docker-compose.test.yml,
BENCH_CONFIG=<repo>/scripts/bench/config-targets.test.yaml

Options:
  --group NAME           Check group (default: ${GROUP})
  --groups LIST          Comma-separated group list (default: auto from config)
  --no-auto-groups       Do not auto-detect groups from config
  --base-url URL         Base URL for siege (default: auto-discovered)
  --concurrency N        Siege concurrency (default: ${CONCURRENCY})
  --duration DUR         Siege duration (default: ${DURATION})
  --stop TARGET          leader|1|2|3|none (default: ${STOP_TARGET})
  --no-build             Skip docker build on up
  --no-restart           Do not restart stopped node
  --out-dir DIR          Report directory (default: ${OUT_DIR})
  --no-wait-checks       Skip waiting for non-empty check results
  --wait-timeout SECS    Wait timeout for checks (default: ${WAIT_CHECKS_TIMEOUT})
  --no-wait-leader       Skip waiting for lock leader (or degraded quorum)
  --leader-timeout SECS  Wait timeout for leader (default: ${WAIT_LEADER_TIMEOUT})
  --no-stabilize         Skip stability wait on start (default: ${STABILIZE_ON_START})
  --no-metrics           Skip /metrics probe
  --no-rejoin-test       Skip restart + rejoin leader test
  --no-rotate-test       Skip leader rotation test
  --no-redis-test        Skip Redis down test
  --strict               Exit non-zero on leader mismatch, empty targets, or redis errors
  --verbose              Print step output as it runs
  --keep-report          Keep the raw report file (default: discard)
  --report-file PATH     Write raw report to PATH (default: temp file)
  -h, --help             Show this help
EOF
}

bench_parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--group) GROUP="$2"; shift 2 ;;
			--groups) GROUPS_OVERRIDE="$2"; shift 2 ;;
			--no-auto-groups) AUTO_GROUPS="false"; shift ;;
			--base-url) BASE_URL="$2"; shift 2 ;;
			--concurrency) CONCURRENCY="$2"; shift 2 ;;
			--duration) DURATION="$2"; shift 2 ;;
			--stop) STOP_TARGET="$2"; shift 2 ;;
			--no-build) BUILD="false"; shift ;;
			--no-restart) RESTART_STOPPED="false"; shift ;;
			--out-dir) OUT_DIR="$2"; shift 2 ;;
			--no-wait-checks) WAIT_FOR_CHECKS="false"; shift ;;
			--wait-timeout) WAIT_CHECKS_TIMEOUT="$2"; shift 2 ;;
			--no-wait-leader) WAIT_FOR_LEADER="false"; shift ;;
			--leader-timeout) WAIT_LEADER_TIMEOUT="$2"; shift 2 ;;
			--no-stabilize) STABILIZE_ON_START="false"; shift ;;
			--no-metrics) METRICS="false"; shift ;;
			--no-rejoin-test) REJOIN_TEST="false"; shift ;;
			--no-rotate-test) ROTATE_LEADER_TEST="false"; shift ;;
			--no-redis-test) REDIS_TEST="false"; shift ;;
			--strict) STRICT="true"; shift ;;
			--verbose) QUIET="false"; shift ;;
			--keep-report) KEEP_REPORT="true"; shift ;;
			--report-file) REPORT_FILE="$2"; shift 2 ;;
			-h|--help) bench_usage; exit 0 ;;
			*) echo "unknown option: $1"; bench_usage; exit 1 ;;
		esac
	done
}

require_cmd() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "missing required command: $1" >&2
		exit 1
	fi
}

bench_init_state() {
	FAIL=0
	FAILURES=()
	CHECKS=()
	METRICS_SAMPLES=()
	STOPPED_REPLICAS=()
	declare -gA METRICS_LAST
	declare -gA METRICS_LAST_TS
	declare -gA LEADER_LOGGED
	METRICS_LAST=()
	METRICS_LAST_TS=()
	LEADER_LOGGED=()
	leader_detected=""
	leader_count=0
	leader_states=()
	REPLICA_CONTAINERS=()
	REPLICA_IPS=()
	REPLICA_URLS=()
}

record_failure() {
	local msg="$1"
	FAIL=1
	FAILURES+=("$msg")
	if [[ "$STRICT" == "true" ]]; then
		echo "strict failure: ${msg}" | tee -a "$report"
	fi
}

add_check() {
	local category="$1"
	local name="$2"
	local status="$3"
	local detail="${4:-}"
	CHECKS+=("${category}|${name}|${status}|${detail}")
	if [[ "$status" == "fail" ]]; then
		FAIL=1
	fi
}

# --- Replica discovery ---

# replica_url_for_container picks a URL the bench host can use. Bridge IPs often time out from
# WSL2 / Docker Desktop hosts; published ports (127.0.0.1:<mapped>) usually work.
replica_url_for_container() {
	local cid="$1"
	local ip
	ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$cid" 2>/dev/null)"
	local bridge_url=""
	if [[ -n "$ip" ]]; then
		bridge_url="http://${ip}:${HA_INTERNAL_PORT}"
	fi
	local port_line host_url=""
	port_line="$(docker port "$cid" "${HA_INTERNAL_PORT}/tcp" 2>/dev/null | head -n1 || true)"
	if [[ -n "$port_line" ]] && [[ "$port_line" =~ :([0-9]+)$ ]]; then
		host_url="http://${BENCH_DOCKER_PUBLISH_BIND}:${BASH_REMATCH[1]}"
	fi
	local mode="${BENCH_REPLICA_URL_MODE:-auto}"
	case "$mode" in
		bridge)
			echo "$bridge_url"
			return
			;;
		host)
			echo "${host_url:-$bridge_url}"
			return
			;;
		auto|*)
			if [[ -n "$host_url" ]] && curl -fsS --max-time 2 "${host_url}/health" >/dev/null 2>&1; then
				echo "$host_url"
				return
			fi
			if [[ -n "$bridge_url" ]] && curl -fsS --max-time 2 "${bridge_url}/health" >/dev/null 2>&1; then
				echo "$bridge_url"
				return
			fi
			if [[ -n "$host_url" ]]; then
				echo "$host_url"
				return
			fi
			echo "$bridge_url"
			return
			;;
	esac
}

discover_replicas() {
	REPLICA_CONTAINERS=()
	REPLICA_IPS=()
	REPLICA_URLS=()
	local ids
	ids="$(compose ps -q ha 2>/dev/null)"
	if [[ -z "$ids" ]]; then
		echo "no ha replicas found" >&2
		return 1
	fi
	while IFS= read -r cid; do
		[[ -z "$cid" ]] && continue
		local name ip url
		name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's|^/||')"
		ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$cid" 2>/dev/null)"
		url="$(replica_url_for_container "$cid")"
		if [[ -n "$name" && -n "$url" ]]; then
			REPLICA_CONTAINERS+=("$name")
			REPLICA_IPS+=("$ip")
			REPLICA_URLS+=("$url")
		fi
	done <<< "$ids"
	echo "discovered ${#REPLICA_CONTAINERS[@]} replicas: ${REPLICA_CONTAINERS[*]}" | tee -a "${report:-/dev/null}" 2>/dev/null || true
	local u
	for u in "${REPLICA_URLS[@]}"; do
		echo "replica_url ${u}" | tee -a "${report:-/dev/null}" 2>/dev/null || true
	done
}

replica_url() {
	local idx="$1"
	echo "${REPLICA_URLS[$idx]:-}"
}

replica_name() {
	local idx="$1"
	echo "${REPLICA_CONTAINERS[$idx]:-}"
}

replica_count() {
	echo "${#REPLICA_CONTAINERS[@]}"
}

is_replica_stopped() {
	local name="$1"
	for n in "${STOPPED_REPLICAS[@]}"; do
		if [[ "$n" == "$name" ]]; then
			return 0
		fi
	done
	return 1
}

stop_replica() {
	local name="$1"
	docker stop "$name" >/dev/null 2>&1 || true
	STOPPED_REPLICAS+=("$name")
}

start_replica() {
	local name="$1"
	docker start "$name" >/dev/null 2>&1 || true
	local next=()
	for n in "${STOPPED_REPLICAS[@]}"; do
		if [[ "$n" != "$name" ]]; then
			next+=("$n")
		fi
	done
	STOPPED_REPLICAS=("${next[@]+"${next[@]}"}")
}

running_replicas() {
	local names=()
	for name in "${REPLICA_CONTAINERS[@]}"; do
		if ! is_replica_stopped "$name"; then
			names+=("$name")
		fi
	done
	echo "${names[*]}"
}

index_of_replica() {
	local target="$1"
	for i in "${!REPLICA_CONTAINERS[@]}"; do
		if [[ "${REPLICA_CONTAINERS[$i]}" == "$target" ]]; then
			echo "$i"
			return
		fi
	done
	echo "-1"
}

# --- Leader & log helpers ---

emit_leader_logs() {
	local label="$1"
	if [[ -n "${LEADER_LOGGED[$label]:-}" ]]; then
		return
	fi
	LEADER_LOGGED["$label"]=1
	echo "" | tee -a "$report"
	echo "leader logs: ${label}" | tee -a "$report"
	for name in "${REPLICA_CONTAINERS[@]}"; do
		local logs
		logs="$(docker logs --tail 200 "$name" 2>/dev/null | rg -n "leadership acquired|starting checks|stopping checks|leader lock lost|leader lock renew failed" || true)"
		if [[ -n "$logs" ]]; then
			echo "${name} logs:" | tee -a "$report"
			echo "$logs" | tail -n 5 | tee -a "$report"
		fi
	done
}

detect_leader() {
	for i in "${!REPLICA_URLS[@]}"; do
		local body
		body="$(curl -sS --max-time 2 "${REPLICA_URLS[$i]}/v1/leader" || true)"
		if [[ "$(parse_bool "$body")" == "true" ]]; then
			echo "${REPLICA_CONTAINERS[$i]}"
			return
		fi
	done
	echo ""
}

# --- URL helpers ---

bench_detect_compose() {
	compose_cmd=()
	if command -v docker-compose >/dev/null 2>&1; then
		compose_cmd=(docker-compose)
	elif command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
		compose_cmd=(docker compose)
	else
		echo "docker compose not found" >&2
		exit 1
	fi
}

compose() {
	( cd "$ROOT_DIR" && "${compose_cmd[@]}" "$@" )
}

bench_require_cmds() {
	require_cmd curl
	require_cmd rg
	require_cmd docker
}

pick_live_base_url() {
	local prefer="$1"
	if [[ -n "$prefer" ]]; then
		local idx
		idx="$(index_of_replica "$prefer")"
		if [[ "$idx" -ge 0 ]]; then
			local url="${REPLICA_URLS[$idx]}"
			if curl -fsS --max-time 2 "$url/health" >/dev/null 2>&1; then
				echo "$url"
				return
			fi
		fi
	fi
	for url in "${REPLICA_URLS[@]}"; do
		if curl -fsS --max-time 2 "$url/health" >/dev/null 2>&1; then
			echo "$url"
			return
		fi
	done
	echo "$BASE_URL"
}

# Re-discover replica URLs and point BASE_URL at a healthy replica (needed after docker stop/start).
refresh_live_base_url() {
	discover_replicas 2>/dev/null || true
	BASE_URL="$(pick_live_base_url "")"
}

# --- Report / lifecycle ---

bench_init_report() {
	bench_init_state
	timestamp="$(date -u +%Y%m%d-%H%M%S)"
	mkdir -p "$OUT_DIR"
	if [[ -z "$REPORT_FILE" ]]; then
		REPORT_FILE="$(mktemp -t ha-bench-report-XXXXXX.txt)"
	fi
	report="$REPORT_FILE"
	TEST_NAME="$(bench_test_name)"
	if [[ "$QUIET" == "true" ]]; then
		exec 2>>"$report"
		tee() {
			command tee "$@" >/dev/null
		}
	fi
	echo "TEST_START name=${TEST_NAME} ts=$(bench_now_ts)" | tee "$report"
	echo "starting benchmark at $timestamp" | tee -a "$report"
	echo "group=$GROUP base_url=$BASE_URL concurrency=$CONCURRENCY duration=$DURATION stop_target=$STOP_TARGET" | tee -a "$report"
	echo "compose=${compose_cmd[*]}" | tee -a "$report"
}

bench_compose_up() {
	local up_args=(up -d)
	if [[ "$BUILD" == "true" ]]; then
		up_args+=(--build)
	fi
	compose "${up_args[@]}" | tee -a "$report"
	add_check "steps" "compose up" "pass"
}

bench_discover() {
	local retries=15
	for _ in $(seq 1 "$retries"); do
		discover_replicas && [[ "${#REPLICA_CONTAINERS[@]}" -gt 0 ]] && break
		sleep 2
	done
	if [[ "${#REPLICA_CONTAINERS[@]}" -eq 0 ]]; then
		echo "FATAL: could not discover any ha replicas" >&2
		exit 1
	fi
	if [[ -z "$BASE_URL" ]]; then
		BASE_URL="$(pick_live_base_url "")"
	fi
	echo "base_url=${BASE_URL} replicas=${#REPLICA_CONTAINERS[@]}" | tee -a "$report"
}

wait_for_url() {
	local url="$1"
	local tries="${BENCH_WAIT_URL_TRIES:-45}"
	for _ in $(seq 1 "$tries"); do
		if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	return 1
}

collect_groups() {
	if [[ "$AUTO_GROUPS" != "true" ]]; then
		if [[ -n "$GROUPS_OVERRIDE" ]]; then
			echo "$GROUPS_OVERRIDE"
			return
		fi
		echo "$GROUP"
		return
	fi
	local cfg="${BENCH_CONFIG:-$ROOT_DIR/scripts/bench/config-targets.test.yaml}"
	if [[ ! -f "$cfg" ]]; then
		if [[ -n "$GROUPS_OVERRIDE" ]]; then
			echo "$GROUPS_OVERRIDE"
			return
		fi
		echo "$GROUP"
		return
	fi
	local in_checks=0
	local groups=()
	while IFS= read -r line; do
		if [[ "$line" =~ ^checks: ]]; then
			in_checks=1
			continue
		fi
		if [[ "$in_checks" -eq 1 ]]; then
			if [[ "$line" =~ ^[^[:space:]] ]]; then
				in_checks=0
				continue
			fi
			if [[ "$line" =~ ^[[:space:]]{2}[^[:space:]#][^:]*: ]]; then
				local name="${line%%:*}"
				name="${name##  }"
				groups+=("$name")
			fi
		fi
	done <"$cfg"
	if [[ "${#groups[@]}" -eq 0 ]]; then
		if [[ -n "$GROUPS_OVERRIDE" ]]; then
			echo "$GROUPS_OVERRIDE"
			return
		fi
		echo "$GROUP"
		return
	fi
	if [[ -n "$GROUPS_OVERRIDE" ]]; then
		local filtered=()
		IFS=',' read -r -a requested <<< "$GROUPS_OVERRIDE"
		for req in "${requested[@]}"; do
			for g in "${groups[@]}"; do
				if [[ "$req" == "$g" ]]; then
					filtered+=("$req")
					break
				fi
			done
		done
		if [[ "${#filtered[@]}" -gt 0 ]]; then
			IFS=','; echo "${filtered[*]}"
			return
		fi
		echo "warning: GROUPS override did not match config; using parsed groups" | tee -a "$report" >&2
	fi
	IFS=','; echo "${groups[*]}"
}

bench_collect_groups() {
	GROUPS_RESOLVED="$(collect_groups)"
	IFS=',' read -r -a GROUP_LIST <<< "$GROUPS_RESOLVED"
	echo "groups=${GROUPS_RESOLVED}" | tee -a "$report"
	local group_found=0
	for g in "${GROUP_LIST[@]}"; do
		if [[ "$g" == "$GROUP" ]]; then
			group_found=1
			break
		fi
	done
	if [[ "$group_found" -eq 0 && "${#GROUP_LIST[@]}" -gt 0 ]]; then
		echo "group ${GROUP} not in config; using ${GROUP_LIST[0]} for siege" | tee -a "$report"
		GROUP="${GROUP_LIST[0]}"
	fi
}

bench_wait_lb() {
	echo "waiting for lb endpoint..." | tee -a "$report"
	local lb_ready=1
	if ! wait_for_url "${BASE_URL}/v1/lb/${GROUP}"; then
		echo "lb endpoint did not become ready in time" | tee -a "$report"
		lb_ready=0
	fi
	if [[ "$lb_ready" -eq 1 ]]; then
		add_check "steps" "lb endpoint ready" "pass"
	else
		record_failure "lb endpoint not ready"
		add_check "steps" "lb endpoint ready" "fail"
	fi
}

# --- JSON helpers ---

parse_bool() {
	echo "$1" | rg -o '"leader":(true|false)' | rg -o 'true|false' || true
}

parse_probes_active() {
	echo "$1" | rg -o '"probes_active":(true|false)' | rg -o 'true|false' || true
}

# Returns 1 if BASE_URL /v1/check/GROUP reports redis_status error, else 0.
bench_redis_check_error() {
	local body
	body="$(curl -sS --max-time 2 "${BASE_URL}/v1/check/${GROUP}" 2>/dev/null || true)"
	local st
	st="$(parse_redis_status "$body")"
	if [[ "$st" == "error" ]]; then
		echo 1
		return
	fi
	echo 0
}

parse_status() {
	echo "$1" | rg -o '"status":"[^"]+"' | sed 's/"status":"//;s/"$//' || true
}

parse_node() {
	echo "$1" | rg -o '"node_id":"[^"]+"' | sed 's/"node_id":"//;s/"$//' || true
}

parse_redis_status() {
	echo "$1" | rg -o '"redis_status":"[^"]+"' | sed 's/"redis_status":"//;s/"$//' || true
}

count_reachable() {
	local total
	total="$(echo "$1" | rg -o '"reachable":(true|false)' | rg -c 'true' || true)"
	echo "${total:-0}"
}

count_total() {
	local total
	total="$(echo "$1" | rg -o '"reachable":(true|false)' | wc -l | tr -d ' ')"
	echo "${total:-0}"
}

# --- Probing functions ---

leader_detected=""
leader_count=0
leader_states=()
probe_leaders() {
	local label="$1"
	leader_detected=""
	leader_count=0
	leader_states=()
	echo "" | tee -a "$report"
	echo "leader probe: $label" | tee -a "$report"
	local all_probes=1
	for i in "${!REPLICA_URLS[@]}"; do
		local url="${REPLICA_URLS[$i]}/v1/leader"
		local name="${REPLICA_CONTAINERS[$i]}"
		local stopped=0
		if is_replica_stopped "$name"; then
			stopped=1
		fi
		local body
		body="$(curl -sS --max-time 2 "$url" || true)"
		local leader
		leader="$(parse_bool "$body")"
		local status
		status="$(parse_status "$body")"
		local node
		node="$(parse_node "$body")"
		local pa
		pa="$(parse_probes_active "$body")"
		if [[ "$leader" == "true" ]]; then
			leader_detected="$name"
			leader_count=$((leader_count + 1))
		fi
		if [[ "$stopped" -eq 0 && "$pa" != "true" ]]; then
			all_probes=0
		fi
		leader_states+=("${name}=${leader}:${status}:${node}:probes=${pa}")
		echo "leader ${name}: ${body}" | tee -a "$report"
	done
	echo "leader count=${leader_count} leader=${leader_detected:-unknown}" | tee -a "$report"
	if [[ "$leader_count" -eq 1 ]]; then
		add_check "leader" "leader count ${label}" "pass"
	elif [[ "$leader_count" -eq 0 && "$all_probes" -eq 1 && "${#REPLICA_URLS[@]}" -gt 0 ]]; then
		echo "leader check: no Redis lock holder; all replicas report probes_active (Redis unreachable mode)" | tee -a "$report"
		add_check "leader" "leader count ${label}" "pass" "degraded_all_probes"
	else
		echo "leader check warning: expected 1 lock leader or all-replica probes, got leaders=${leader_count} all_probes=${all_probes}" | tee -a "$report"
		record_failure "leader count ${leader_count} at ${label}"
		emit_leader_logs "$label"
		add_check "leader" "leader count ${label}" "fail" "count=${leader_count}"
	fi
}

wait_for_leader() {
	local tries="${WAIT_LEADER_TIMEOUT}"
	for _ in $(seq 1 "$tries"); do
		discover_replicas 2>/dev/null || true
		local count=0
		local running=0
		local probes_ok=0
		for i in "${!REPLICA_URLS[@]}"; do
			local url="${REPLICA_URLS[$i]}/v1/leader"
			local name="${REPLICA_CONTAINERS[$i]}"
			if is_replica_stopped "$name"; then
				continue
			fi
			running=$((running + 1))
			local body
			body="$(curl -sS --max-time 2 "$url" || true)"
			local leader
			leader="$(parse_bool "$body")"
			if [[ "$leader" == "true" ]]; then
				count=$((count + 1))
			fi
			if [[ "$(parse_probes_active "$body")" == "true" ]]; then
				probes_ok=$((probes_ok + 1))
			fi
		done
		if [[ "$count" -eq 1 ]]; then
			return 0
		fi
		if [[ "$count" -eq 0 && "$running" -gt 0 && "$probes_ok" -eq "$running" ]]; then
			if [[ "$(bench_redis_check_error)" -eq 1 ]]; then
				return 0
			fi
		fi
		sleep 1
	done
	return 1
}

wait_for_checks() {
	local tries="${WAIT_CHECKS_TIMEOUT}"
	for _ in $(seq 1 "$tries"); do
		local ok=1
		for group in "${GROUP_LIST[@]}"; do
			local url="${BASE_URL}/v1/check/${group}"
			local body
			body="$(curl -sS --max-time 2 "$url" || true)"
			local total
			total="$(count_total "$body")"
			local redis
			redis="$(parse_redis_status "$body")"
			if [[ "$total" -le 0 || "$redis" != "ok" ]]; then
				ok=0
				break
			fi
		done
		if [[ "$ok" -eq 1 ]]; then
			return 0
		fi
		sleep 1
	done
	return 1
}

probe_endpoints() {
	local label="$1"
	local api_ok=1
	local api_notes=()
	local allow_redis_error=0
	if echo "$label" | rg -q "redis down"; then
		allow_redis_error=1
	fi
	echo "" | tee -a "$report"
	echo "endpoint probe: $label" | tee -a "$report"
	for group in "${GROUP_LIST[@]}"; do
		echo "group ${group}:" | tee -a "$report"
		for i in "${!REPLICA_URLS[@]}"; do
			local base="${REPLICA_URLS[$i]}"
			local name="${REPLICA_CONTAINERS[$i]}"
			local stopped=0
			if is_replica_stopped "$name"; then
				stopped=1
			fi
			local url="${base}/v1/lb/${group}"
			local out
			out="$(curl -sS --max-time 2 -w " code=%{http_code} time=%{time_total}s" "$url" || true)"
			echo "lb ${name}: ${out}" | tee -a "$report"
			local code
			code="$(echo "$out" | rg -o 'code=[0-9]+' | sed 's/code=//')"
			if [[ "$code" != "200" && "$stopped" -eq 0 ]]; then
				echo "lb warning: status ${code:-unknown} on ${name}" | tee -a "$report"
				api_ok=0
				api_notes+=("lb status ${code:-unknown} on ${name}")
				record_failure "lb status ${code:-unknown} on ${name} group=${group} (${label})"
			fi
			if ! echo "$out" | rg -q '"name"' && [[ "$stopped" -eq 0 ]]; then
				echo "lb warning: missing name on ${name}" | tee -a "$report"
				api_ok=0
				api_notes+=("lb missing name on ${name}")
				record_failure "lb missing name on ${name} group=${group} (${label})"
			fi
		done
		for i in "${!REPLICA_URLS[@]}"; do
			local base="${REPLICA_URLS[$i]}"
			local name="${REPLICA_CONTAINERS[$i]}"
			local stopped=0
			if is_replica_stopped "$name"; then
				stopped=1
			fi
			local url="${base}/v1/check/${group}"
			local out
			out="$(curl -sS --max-time 2 "$url" || true)"
			local total reachable redis
			total="$(count_total "$out")"
			reachable="$(count_reachable "$out")"
			redis="$(parse_redis_status "$out")"
			local meta="targets=${total} reachable=${reachable} redis=${redis:-unknown}"
			echo "check ${name}: ${out} ${meta}" | tee -a "$report"
			if [[ "$total" -eq 0 && "$stopped" -eq 0 ]]; then
				echo "check warning: no targets returned on ${name}" | tee -a "$report"
				if [[ "$allow_redis_error" -eq 0 ]]; then
					record_failure "empty targets on ${name} group=${group} (${label})"
					api_ok=0
					api_notes+=("check empty targets on ${name}")
				fi
			elif [[ "$reachable" -eq 0 ]]; then
				echo "check warning: zero reachable targets on ${name}" | tee -a "$report"
			fi
			if [[ "$redis" != "ok" && -n "$redis" && "$stopped" -eq 0 && "$allow_redis_error" -eq 0 ]]; then
				echo "check warning: redis_status=${redis} on ${name}" | tee -a "$report"
				record_failure "redis_status=${redis} on ${name} group=${group} (${label})"
				api_ok=0
				api_notes+=("redis_status ${redis} on ${name}")
			fi
		done
	done
	if [[ "$api_ok" -eq 1 ]]; then
		add_check "api" "endpoint probe ${label}" "pass"
	else
		add_check "api" "endpoint probe ${label}" "fail" "$(IFS='; '; echo "${api_notes[*]}")"
	fi
}

# --- Metrics ---

metrics_sum() {
	local body="$1"
	local metric="$2"
	local label_key="${3:-}"
	local label_val="${4:-}"
	echo "$body" | awk -v metric="$metric" -v lkey="$label_key" -v lval="$label_val" '
		$1 ~ ("^"metric) {
			if (lkey == "" || index($1, lkey"=\""lval"\"") > 0) {
				sum += $NF
			}
		}
		END { if (sum == "") sum = 0; printf "%.6f", sum }
	'
}

metrics_snapshot() {
	local body="$1"
	local req_total req_hit req_miss err_total check_total check_targets probe_runs
	req_total="$(metrics_sum "$body" "lb_requests_total")"
	req_hit="$(metrics_sum "$body" "lb_requests_total" "cache_hit" "true")"
	req_miss="$(metrics_sum "$body" "lb_requests_total" "cache_hit" "false")"
	err_total="$(metrics_sum "$body" "lb_errors_total")"
	check_total="$(metrics_sum "$body" "check_requests_total")"
	check_targets="$(metrics_sum "$body" "check_targets_total")"
	probe_runs="$(metrics_sum "$body" "probe_runs_total")"
	echo "${req_total}|${req_hit}|${req_miss}|${err_total}|${check_total}|${check_targets}|${probe_runs}"
}

probe_metrics() {
	local label="$1"
	if [[ "$METRICS" != "true" ]]; then
		return
	fi
	local metrics_ok=1
	local metrics_notes=()
	echo "" | tee -a "$report"
	echo "metrics probe: $label" | tee -a "$report"
	for i in "${!REPLICA_URLS[@]}"; do
		local base="${REPLICA_URLS[$i]}"
		local name="${REPLICA_CONTAINERS[$i]}"
		local stopped=0
		if is_replica_stopped "$name"; then
			stopped=1
		fi
		local url="${base}/metrics"
		local body
		body="$(curl -sS --max-time 2 "$url" || true)"
		if [[ -z "$body" ]]; then
			echo "metrics ${name}: (no data)" | tee -a "$report"
			if [[ "$stopped" -eq 0 ]]; then
				metrics_ok=0
				metrics_notes+=("missing metrics on ${name}")
			fi
			METRICS_SAMPLES+=("${label}|${name}|0|0|0|0|0|0")
			continue
		fi
		echo "metrics ${name}:" | tee -a "$report"
		echo "$body" | rg -e '^lb_requests_total' -e '^lb_errors_total' -e '^check_requests_total' -e '^check_targets_total' -e '^lb_latency_ms_(sum|count)' -e '^check_latency_ms_(sum|count)' -e '^probe_runs_total' -e '^probe_write_errors_total' | tee -a "$report" || true
		local snapshot
		snapshot="$(metrics_snapshot "$body")"
		local req_total req_hit req_miss err_total check_total check_targets probe_runs
		IFS='|' read -r req_total req_hit req_miss err_total check_total check_targets probe_runs <<< "$snapshot"
		local key_base="replica_${name}"
		local last="${METRICS_LAST[$key_base]:-}"
		local now
		now="$(date +%s)"
		local req_d=0 err_d=0 hit_d=0 check_d=0 probe_d=0 dt=0
		local hit_rate="n/a" err_rate="n/a" err_per_sec="n/a"
		if [[ -n "$last" ]]; then
			local last_req last_hit last_miss last_err last_check last_targets last_probe
			IFS='|' read -r last_req last_hit last_miss last_err last_check last_targets last_probe <<< "$last"
			dt=$((now - ${METRICS_LAST_TS[$key_base]:-0}))
			req_d="$(awk -v a="$req_total" -v b="$last_req" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')"
			err_d="$(awk -v a="$err_total" -v b="$last_err" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')"
			hit_d="$(awk -v a="$req_hit" -v b="$last_hit" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')"
			check_d="$(awk -v a="$check_total" -v b="$last_check" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')"
			probe_d="$(awk -v a="$probe_runs" -v b="$last_probe" 'BEGIN{d=a-b; if(d<0)d=0; printf "%.0f", d}')"
			if [[ "$req_d" -gt 0 ]]; then
				hit_rate="$(awk -v h="$hit_d" -v r="$req_d" 'BEGIN{printf "%.2f", (h/r)*100}')"
				err_rate="$(awk -v e="$err_d" -v r="$req_d" 'BEGIN{printf "%.2f", (e/r)*100}')"
			fi
			if [[ "$dt" -gt 0 ]]; then
				err_per_sec="$(awk -v e="$err_d" -v t="$dt" 'BEGIN{printf "%.2f", (e/t)}')"
			fi
		fi
		echo "metrics delta ${name}: lb_req=${req_d} hit%=${hit_rate} err%=${err_rate} err/s=${err_per_sec} check_req=${check_d} probe_runs=${probe_d}" | tee -a "$report"
		METRICS_SAMPLES+=("${label}|${name}|${req_d}|${hit_d}|${err_d}|${check_d}|${probe_d}|${dt}")
		METRICS_LAST[$key_base]="$snapshot"
		METRICS_LAST_TS[$key_base]="$now"
	done
	if [[ "$metrics_ok" -eq 1 ]]; then
		add_check "metrics" "metrics probe ${label}" "pass"
	else
		record_failure "metrics missing: ${label} $(IFS='; '; echo "${metrics_notes[*]}")"
		add_check "metrics" "metrics probe ${label}" "fail" "$(IFS='; '; echo "${metrics_notes[*]}")"
	fi
}

# --- Load testing ---

run_siege() {
	local label="$1"
	local url="${BASE_URL}/v1/lb/${GROUP}"
	local out="$OUT_DIR/siege-${label}.txt"
	if ! command -v siege >/dev/null 2>&1; then
		echo "siege not installed; skipping load test ${label}" | tee -a "$report"
		add_check "load" "siege ${label}" "warn" "siege not installed"
		return 0
	fi
	echo "" | tee -a "$report"
	echo "running siege ${label}: $url" | tee -a "$report"
	siege -c "${CONCURRENCY}" -t "${DURATION}" "$url" >"$out" 2>&1 || true
	echo "siege output: $out" | tee -a "$report"
	local trx rate resp
	trx="$(rg -m1 "\"transactions\"" "$out" || true)"
	trx="$(echo "$trx" | sed 's/[^0-9.]//g')"
	rate="$(rg -m1 "\"transaction_rate\"" "$out" || true)"
	rate="$(echo "$rate" | sed 's/[^0-9.]//g')"
	resp="$(rg -m1 "\"response_time\"" "$out" || true)"
	resp="$(echo "$resp" | sed 's/[^0-9.]//g')"
	if [[ -n "$trx" || -n "$rate" || -n "$resp" ]]; then
		echo "siege ${label} summary: transactions=${trx:-n/a} transaction_rate=${rate:-n/a} response_time=${resp:-n/a}" | tee -a "$report"
		if [[ "${trx:-0}" != "0" ]]; then
			add_check "load" "siege ${label}" "pass" "trx=${trx:-0} rate=${rate:-0} resp=${resp:-0}"
		else
			record_failure "siege ${label} no transactions"
			add_check "load" "siege ${label}" "fail" "no transactions"
		fi
	else
		add_check "load" "siege ${label}" "warn" "no summary parsed"
	fi
	if [[ "$KEEP_REPORT" != "true" ]]; then
		rm -f "$out"
	fi
}

# --- Health probe ---

probe_health() {
	local label="$1"
	local ok=1
	local notes=()
	echo "" | tee -a "$report"
	echo "health probe: $label" | tee -a "$report"
	for i in "${!REPLICA_URLS[@]}"; do
		local url="${REPLICA_URLS[$i]}/health"
		local name="${REPLICA_CONTAINERS[$i]}"
		if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
			echo "health ${name}: ok" | tee -a "$report"
		else
			echo "health ${name}: fail" | tee -a "$report"
			ok=0
			notes+=("${name}")
			record_failure "health failed on ${name}"
		fi
	done
	if [[ "$ok" -eq 1 ]]; then
		add_check "api" "health probe ${label}" "pass"
	else
		add_check "api" "health probe ${label}" "fail" "replicas=$(IFS=','; echo "${notes[*]}")"
	fi
}

# --- Stabilize & prepare ---

bench_stabilize() {
	if [[ "$STABILIZE_ON_START" != "true" ]]; then
		return
	fi
	if [[ "$WAIT_FOR_LEADER" == "true" ]]; then
		echo "waiting for Redis lock leader (or all-replica probes if Redis is down)..." | tee -a "$report"
		if ! wait_for_leader; then
			echo "leader did not converge in time" | tee -a "$report"
			record_failure "leader did not converge on start"
			emit_leader_logs "startup"
			add_check "leader" "leader converge (startup)" "fail"
		else
			add_check "leader" "leader converge (startup)" "pass"
		fi
	fi
	if [[ "$WAIT_FOR_CHECKS" == "true" ]]; then
		echo "waiting for non-empty check results..." | tee -a "$report"
		if ! wait_for_checks; then
			echo "checks did not become ready in time" | tee -a "$report"
			record_failure "checks did not become ready on start"
			add_check "api" "checks ready (startup)" "fail"
		else
			add_check "api" "checks ready (startup)" "pass"
		fi
	fi
}

bench_prepare() {
	bench_detect_compose
	bench_init_report
	bench_require_cmds
	bench_compose_up
	bench_discover
	bench_collect_groups
	bench_wait_lb
	bench_stabilize
}

# --- Summary / finish ---

status_label() {
	local status="$1"
	case "$status" in
		pass) echo "PASS" ;;
		fail) echo "FAIL" ;;
		warn) echo "WARN" ;;
		*) echo "${status}" ;;
	esac
}

print_checks_section() {
	local category="$1"
	local title="$2"
	local found=0
	echo ""
	echo "$title"
	for entry in "${CHECKS[@]}"; do
		local cat name status detail
		IFS='|' read -r cat name status detail <<< "$entry"
		if [[ "$cat" != "$category" ]]; then
			continue
		fi
		found=1
		local label
		label="$(status_label "$status")"
		if [[ -n "$detail" ]]; then
			echo "- ${label} ${name} (${detail})"
		else
			echo "- ${label} ${name}"
		fi
	done
	if [[ "$found" -eq 0 ]]; then
		echo "- (no checks)"
	fi
}

print_metrics_summary() {
	if [[ "${#METRICS_SAMPLES[@]}" -eq 0 ]]; then
		echo ""
		echo "Metrics Aggregates"
		echo "- (no metrics samples)"
		return
	fi
	local -A req_sum hit_sum err_sum check_sum probe_sum dt_sum dt_count
	local labels=()
	for entry in "${METRICS_SAMPLES[@]}"; do
		local label name req_d hit_d err_d check_d probe_d dt
		IFS='|' read -r label name req_d hit_d err_d check_d probe_d dt <<< "$entry"
		if [[ -z "${req_sum[$label]+x}" ]]; then
			labels+=("$label")
			req_sum[$label]=0
			hit_sum[$label]=0
			err_sum[$label]=0
			check_sum[$label]=0
			probe_sum[$label]=0
			dt_sum[$label]=0
			dt_count[$label]=0
		fi
		req_sum[$label]=$((req_sum[$label] + req_d))
		hit_sum[$label]=$((hit_sum[$label] + hit_d))
		err_sum[$label]=$((err_sum[$label] + err_d))
		check_sum[$label]=$((check_sum[$label] + check_d))
		probe_sum[$label]=$((probe_sum[$label] + probe_d))
		if [[ "$dt" -gt 0 ]]; then
			dt_sum[$label]=$((dt_sum[$label] + dt))
			dt_count[$label]=$((dt_count[$label] + 1))
		fi
	done
	echo ""
	echo "Metrics Aggregates"
	for label in "${labels[@]}"; do
		local req hit err checks probes hit_rate err_rate err_per_sec
		req="${req_sum[$label]}"
		hit="${hit_sum[$label]}"
		err="${err_sum[$label]}"
		checks="${check_sum[$label]}"
		probes="${probe_sum[$label]}"
		hit_rate="n/a"
		err_rate="n/a"
		err_per_sec="n/a"
		if [[ "$req" -gt 0 ]]; then
			hit_rate="$(awk -v h="$hit" -v r="$req" 'BEGIN{printf "%.2f", (h/r)*100}')"
			err_rate="$(awk -v e="$err" -v r="$req" 'BEGIN{printf "%.2f", (e/r)*100}')"
		fi
		if [[ "${dt_count[$label]}" -gt 0 ]]; then
			local avg_dt
			avg_dt=$((dt_sum[$label] / dt_count[$label]))
			if [[ "$avg_dt" -gt 0 ]]; then
				err_per_sec="$(awk -v e="$err" -v t="$avg_dt" 'BEGIN{printf "%.2f", (e/t)}')"
			fi
		fi
		echo "- ${label}: lb_req=${req} hit%=${hit_rate} err%=${err_rate} err/s=${err_per_sec} check_req=${checks} probe_runs=${probes}"
	done
}

print_summary_impl() {
	echo ""
	echo "Benchmark Summary"
	echo "timestamp: ${timestamp}"
	echo "group: ${GROUP} base_url: ${BASE_URL}"
	echo "concurrency: ${CONCURRENCY} duration: ${DURATION} stop_target: ${STOP_TARGET}"
	echo "replicas: ${#REPLICA_CONTAINERS[@]}"
	if [[ "$FAIL" -eq 0 ]]; then
		echo "overall: PASS"
	else
		echo "overall: FAIL"
	fi
	print_checks_section "steps" "Process Steps"
	print_checks_section "leader" "Leader / lock"
	print_checks_section "api" "API"
	print_checks_section "metrics" "Metrics"
	print_checks_section "load" "Load"
	print_checks_section "multi_group" "Multi-Group"
	print_checks_section "leak" "Leak Detection"
	print_checks_section "stress" "Stress"
	print_metrics_summary
	if [[ "${#FAILURES[@]}" -gt 0 ]]; then
		echo ""
		echo "Failures"
		for msg in "${FAILURES[@]}"; do
			echo "- ${msg}"
		done
	fi
}

# Use >&1 for terminal output: opening /dev/stdout for append breaks on some environments (e.g. WSL) when fd 1 is redirected.
print_summary() {
	if [[ -n "${1:-}" ]]; then
		print_summary_impl >>"$1"
	else
		print_summary_impl >&1
	fi
}

bench_finish() {
	echo "" | tee -a "$report"
	compose ps | tee -a "$report"
	echo "report written to $report" | tee -a "$report"
	print_summary "$report"
	echo "TEST_END name=${TEST_NAME} ts=$(bench_now_ts)" | tee -a "$report"
	if [[ "$PRINT_SUMMARY" == "true" ]]; then
		print_summary
		echo "TEST_END name=${TEST_NAME} ts=$(bench_now_ts)"
	fi
	if [[ "$KEEP_REPORT" == "true" ]]; then
		if [[ "$PRINT_RAW_REPORT" == "true" ]]; then
			echo ""
			echo "raw report: ${report}"
		fi
	else
		rm -f "$report"
	fi
	if [[ "$STRICT" == "true" && "$FAIL" -ne 0 ]]; then
		exit 1
	fi
}
