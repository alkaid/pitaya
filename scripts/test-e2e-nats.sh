#!/usr/bin/env bash
set -euo pipefail

host_os=$(uname -s) || {
	echo "NATS E2E could not identify the host operating system" >&2
	exit 2
}
if [[ $host_os != Linux ]]; then
	echo "NATS E2E requires Linux, found $host_os" >&2
	exit 2
fi
if ! command -v timeout >/dev/null 2>&1; then
	echo "NATS E2E requires GNU timeout" >&2
	exit 2
fi
if ! timeout --help 2>&1 | grep -q -- '--kill-after'; then
	echo "NATS E2E requires a timeout implementation with --kill-after" >&2
	exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
compose_file=$repo_root/examples/testing/docker-compose.yml
project_name="pitaya-e2e-${GITHUB_RUN_ID:-local}-$$"
tmpdir=
server_binary=
compose=(docker compose --project-name "$project_name" --file "$compose_file")
bounded_timeout=(timeout --signal=TERM --kill-after=5s)
compose_attempted=0
active_pid=
pending_signal=0

# The E2E lane owns dynamic loopback ports; inherited example settings must not
# make parallel or repeated runs collide.
export PITAYA_NATS_HOST_PORT=
export PITAYA_ETCD_HOST_PORT=
export PITAYA_REDIS_HOST_PORT=

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
		"${bounded_timeout[@]}" 10s "${compose[@]}" ps --all || true
		"${bounded_timeout[@]}" 10s "${compose[@]}" logs --no-color --tail=200 || true
	fi
	if ((compose_attempted == 1)) && \
		! "${bounded_timeout[@]}" 20s "${compose[@]}" down --volumes --remove-orphans --timeout 10; then
		echo "failed to remove NATS E2E Compose project $project_name" >&2
		((status != 0)) || status=1
		"${bounded_timeout[@]}" 10s "${compose[@]}" ps --all || true
		"${bounded_timeout[@]}" 10s "${compose[@]}" logs --no-color --tail=200 || true
	fi
	if [[ -n $tmpdir ]] && ! rm -rf -- "$tmpdir"; then
		echo "failed to remove NATS E2E scratch directory $tmpdir" >&2
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

tmpdir=$(mktemp -d)
server_binary=$tmpdir/server

run_active "${bounded_timeout[@]}" 5m env GOWORK=off GOFLAGS= \
	go -C "$repo_root" build -trimpath -buildvcs=false -o "$server_binary" ./examples/testing

compose_attempted=1
run_active "${bounded_timeout[@]}" 75s "${compose[@]}" up --detach --wait --wait-timeout 60

published_port() {
	local service=$1
	local container_port=$2
	local address_file=$3
	local address
	run_active "${bounded_timeout[@]}" 10s "${compose[@]}" port "$service" "$container_port" >"$address_file"
	IFS= read -r address <"$address_file"
	printf -v "$service" '%s' "${address##*:}"
}

published_port nats 4222 "$tmpdir/nats.port"
published_port etcd 2379 "$tmpdir/etcd.port"
published_port redis 6379 "$tmpdir/redis.port"
nats_port=$nats
etcd_port=$etcd
redis_port=$redis

run_active "${bounded_timeout[@]}" 11m env \
	PITAYA_TEST_SERVER_BINARY="$server_binary" \
	PITAYA_TEST_NATS_URL="nats://127.0.0.1:$nats_port" \
	PITAYA_TEST_ETCD_URL="127.0.0.1:$etcd_port" \
	PITAYA_TEST_REDIS_URL="127.0.0.1:$redis_port" \
	GOWORK=off GOFLAGS= \
		go -C "$repo_root" test -tags=e2e ./e2e -count=1 -timeout=10m
