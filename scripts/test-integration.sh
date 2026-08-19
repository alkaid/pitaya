#!/usr/bin/env bash
set -euo pipefail

if command -v timeout >/dev/null 2>&1; then
	timeout_command=timeout
elif command -v gtimeout >/dev/null 2>&1; then
	timeout_command=gtimeout
else
	echo "Redis integration requires GNU timeout (or gtimeout from coreutils)" >&2
	exit 2
fi
if ! "$timeout_command" --help 2>&1 | grep -q -- '--kill-after'; then
	echo "Redis integration requires a timeout implementation with --kill-after" >&2
	exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
compose_file=$repo_root/examples/testing/docker-compose.yml
tmpdir=
project_name=
owner_file=
session_test_binary=
service_test_binary=
session_proof_path=
service_proof_path=
session_proof_expected=
service_proof_expected=
compose=()
bounded_timeout=("$timeout_command" --signal=TERM --kill-after=5s)
compose_attempted=0
compose_stopped=0
active_pid=
pending_signal=0

# Override inherited shell values and Compose .env files so every run receives
# a new loopback port even when the caller normally pins example ports.
export PITAYA_NATS_HOST_PORT=
export PITAYA_ETCD_HOST_PORT=
export PITAYA_REDIS_HOST_PORT=

diagnostics() {
	"${bounded_timeout[@]}" 10s "${compose[@]}" ps --all || true
	"${bounded_timeout[@]}" 10s "${compose[@]}" logs --no-color --tail=200 redis || true
}

stop_active_process() {
	local deadline
	[[ -n $active_pid ]] || return 0
	if ! kill -0 "$active_pid" 2>/dev/null && ! kill -0 -- "-$active_pid" 2>/dev/null; then
		wait "$active_pid" 2>/dev/null || true
		active_pid=
		return 0
	fi
	kill -TERM "$active_pid" 2>/dev/null || true
	kill -TERM -- "-$active_pid" 2>/dev/null || true
	deadline=$((SECONDS + 10))
	while { kill -0 "$active_pid" 2>/dev/null || kill -0 -- "-$active_pid" 2>/dev/null; } && \
		((SECONDS < deadline)); do
		kill -TERM "$active_pid" 2>/dev/null || true
		kill -TERM -- "-$active_pid" 2>/dev/null || true
		sleep 0.1
	done
	if kill -0 "$active_pid" 2>/dev/null || kill -0 -- "-$active_pid" 2>/dev/null; then
		kill -KILL "$active_pid" 2>/dev/null || true
		kill -KILL -- "-$active_pid" 2>/dev/null || true
	fi
	wait "$active_pid" 2>/dev/null || true
	active_pid=
}

cleanup() {
	local original_status=$?
	local status=$original_status
	trap - EXIT HUP INT TERM
	trap '' HUP INT TERM

	stop_active_process
	if ((status != 0 && compose_attempted == 1)); then
		diagnostics
	fi

	if ((compose_attempted == 1 && compose_stopped == 0)); then
		if [[ ! -f $owner_file || -L $owner_file ]] || \
			! cmp -s "$owner_file" <(printf '%s\n' "$project_name"); then
			echo "refusing Compose cleanup: ownership claim mismatch" >&2
			((status != 0)) || status=1
		elif "${bounded_timeout[@]}" 20s "${compose[@]}" down --volumes --remove-orphans --timeout 10; then
			compose_stopped=1
		else
			echo "failed to remove owned Compose project $project_name" >&2
			diagnostics
			((status != 0)) || status=1
		fi
	fi

	if [[ -n $tmpdir ]] && ! rm -rf -- "$tmpdir"; then
		echo "failed to remove integration scratch directory $tmpdir" >&2
		((status != 0)) || status=1
	fi
	exit "$status"
}

signal_exit() {
	local status=$1
	exit "$status"
}

run_active() {
	local status
	pending_signal=0
	trap 'pending_signal=129' HUP
	trap 'pending_signal=130' INT
	trap 'pending_signal=143' TERM
	"$@" &
	active_pid=$!
	trap 'signal_exit 129' HUP
	trap 'signal_exit 130' INT
	trap 'signal_exit 143' TERM
	if ((pending_signal != 0)); then
		signal_exit "$pending_signal"
	fi
	if wait "$active_pid"; then
		status=0
	else
		status=$?
	fi
	active_pid=
	return "$status"
}

trap cleanup EXIT
trap 'signal_exit 129' HUP
trap 'signal_exit 130' INT
trap 'signal_exit 143' TERM

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/pitaya-redis-integration.XXXXXXXX")
chmod 700 "$tmpdir"
nonce=$(printf '%s' "${tmpdir##*.}" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9')
project_name="pitaya-integration-${nonce}-$$"
owner_file=$tmpdir/compose.owner
session_test_binary=$tmpdir/session-integration.test
service_test_binary=$tmpdir/service-integration.test
session_proof_path=$tmpdir/session-test.proof
service_proof_path=$tmpdir/service-test.proof
session_proof_expected=$tmpdir/session-test.proof.expected
service_proof_expected=$tmpdir/service-test.proof.expected
session_inventory_output=$tmpdir/session-test.inventory
service_inventory_output=$tmpdir/service-test.inventory
project_output=$tmpdir/compose.project
port_output=$tmpdir/redis.port
sentinel_output=$tmpdir/redis.sentinel
compose=(docker compose --project-name "$project_name" --file "$compose_file")

random_uuid() {
	local hex
	hex=$(od -An -N16 -tx1 /dev/urandom | tr -d '[:space:]')
	[[ $hex =~ ^[0-9a-f]{32}$ ]] || {
		echo "failed to generate integration UUID" >&2
		return 1
	}
	printf '%s-%s-%s-%s-%s\n' \
		"${hex:0:8}" "${hex:8:4}" "${hex:12:4}" "${hex:16:4}" "${hex:20:12}"
}

session_test_name=TestFlushFrontendDataRedisRoundTrip
service_test_name=TestRedisIdempotencyStoreRedisIntegration
run_active "${bounded_timeout[@]}" 5m env GOWORK=off GOFLAGS= \
	go test -c -tags=integration -o "$session_test_binary" ./session
run_active "${bounded_timeout[@]}" 5m env GOWORK=off GOFLAGS= \
	go test -c -tags=integration -o "$service_test_binary" ./service
run_active "${bounded_timeout[@]}" 10s "$session_test_binary" \
	-test.list "^${session_test_name}$" >"$session_inventory_output"
session_list_output=$(<"$session_inventory_output")
if [[ $session_list_output != "$session_test_name" ]]; then
	echo "session integration test inventory mismatch: expected exactly $session_test_name" >&2
	exit 1
fi
run_active "${bounded_timeout[@]}" 10s "$service_test_binary" \
	-test.list "^${service_test_name}$" >"$service_inventory_output"
service_list_output=$(<"$service_inventory_output")
if [[ $service_list_output != "$service_test_name" ]]; then
	echo "service integration test inventory mismatch: expected exactly $service_test_name" >&2
	exit 1
fi

run_active "${bounded_timeout[@]}" 10s "${compose[@]}" ps --all --quiet >"$project_output"
existing=$(<"$project_output")
if [[ -n $existing ]]; then
	echo "refusing to reuse non-empty Compose project $project_name" >&2
	exit 1
fi
umask 077
printf '%s\n' "$project_name" >"$owner_file"

compose_attempted=1
run_active "${bounded_timeout[@]}" 75s "${compose[@]}" up --detach --wait --wait-timeout 60 redis

run_active "${bounded_timeout[@]}" 10s "${compose[@]}" port redis 6379 >"$port_output"
published_address=$(<"$port_output")
if [[ ! $published_address =~ ^127\.0\.0\.1:([1-9][0-9]{0,4})$ ]]; then
	echo "Redis published address is not one literal loopback host:port: $published_address" >&2
	exit 1
fi
redis_port=${BASH_REMATCH[1]}
if ((10#$redis_port > 65535)); then
	echo "Redis published port is out of range: $redis_port" >&2
	exit 1
fi

owner_id=$(random_uuid)
owner_key="pitaya:test:ownership:$owner_id"
owner_token=$(random_uuid)
session_uid=$(random_uuid)
session_proof_token=$(random_uuid)
service_proof_token=$(random_uuid)
printf '%s\n' "$session_proof_token" >"$session_proof_expected"
printf '%s\n' "$service_proof_token" >"$service_proof_expected"

run_active "${bounded_timeout[@]}" 10s "${compose[@]}" exec -T redis \
	redis-cli --raw SET "$owner_key" "$owner_token" EX 120 NX >"$sentinel_output"
sentinel_result=$(<"$sentinel_output")
if [[ $sentinel_result != OK ]]; then
	echo "failed to create the Redis ownership sentinel from inside the container" >&2
	exit 1
fi

run_active "${bounded_timeout[@]}" 130s env \
	PITAYA_INTEGRATION_REDIS_ADDR="127.0.0.1:$redis_port" \
	PITAYA_INTEGRATION_REDIS_OWNER_KEY="$owner_key" \
	PITAYA_INTEGRATION_REDIS_OWNER_TOKEN="$owner_token" \
	PITAYA_INTEGRATION_SESSION_UID="$session_uid" \
	PITAYA_INTEGRATION_PROOF_PATH="$session_proof_path" \
	PITAYA_INTEGRATION_PROOF_TOKEN="$session_proof_token" \
	"$session_test_binary" \
		-test.run="^${session_test_name}$" \
		-test.count=1 \
		-test.timeout=2m \
		-test.v

if [[ ! -f $session_proof_path || -L $session_proof_path ]] || \
	! cmp -s "$session_proof_expected" "$session_proof_path"; then
	echo "session integration test did not publish the exact completion proof" >&2
	exit 1
fi

run_active "${bounded_timeout[@]}" 190s env \
	PITAYA_REDIS_URL="redis://127.0.0.1:$redis_port" \
	PITAYA_INTEGRATION_REDIS_ADDR="127.0.0.1:$redis_port" \
	PITAYA_INTEGRATION_REDIS_OWNER_KEY="$owner_key" \
	PITAYA_INTEGRATION_REDIS_OWNER_TOKEN="$owner_token" \
	PITAYA_INTEGRATION_SERVICE_PROOF_PATH="$service_proof_path" \
	PITAYA_INTEGRATION_SERVICE_PROOF_TOKEN="$service_proof_token" \
	"$service_test_binary" \
		-test.run="^${service_test_name}$" \
		-test.count=1 \
		-test.timeout=3m \
		-test.v

if [[ ! -f $service_proof_path || -L $service_proof_path ]] || \
	! cmp -s "$service_proof_expected" "$service_proof_path"; then
	echo "service integration test did not publish the exact completion proof" >&2
	exit 1
fi
