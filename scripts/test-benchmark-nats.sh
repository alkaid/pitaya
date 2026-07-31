#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
compose_file=$repo_root/examples/testing/docker-compose.yml
run_id=${GITHUB_RUN_ID:-local}
run_id=${run_id//[^a-zA-Z0-9_-]/-}
project_name="pitaya-benchmark-${run_id,,}-$$"
bench_pattern=${BENCH_PATTERN:-.}
bench_time=${BENCH_TIME:-1s}
bench_count=${BENCH_COUNT:-10}
bench_timeout=${BENCH_TIMEOUT:-20m}
bench_out=${BENCH_OUT:-build/benchmarks/nats-$project_name.txt}

is_positive_duration() {
	local value=$1
	local significant
	if [[ ! $value =~ ^([0-9]+(ns|us|ms|s|m|h))+$ ]]; then
		return 1
	fi
	significant=${value//[a-z]/}
	significant=${significant//./}
	significant=${significant//0/}
	[[ -n $significant ]]
}

if [[ ! $bench_count =~ ^[1-9][0-9]*$ ]]; then
	echo "BENCH_COUNT must be a positive integer: $bench_count" >&2
	exit 2
fi
if ((${#bench_count} > 3)) || ((bench_count > 100)); then
	echo "BENCH_COUNT must not exceed 100: $bench_count" >&2
	exit 2
fi
if [[ $bench_time == *x ]]; then
	if [[ ! $bench_time =~ ^([1-9][0-9]*)x$ ]] || ((${#BASH_REMATCH[1]} > 7)) || ((BASH_REMATCH[1] > 1000000)); then
		echo "iteration BENCH_TIME must be between 1x and 1000000x: $bench_time" >&2
		exit 2
	fi
fi
if ! is_positive_duration "$bench_timeout"; then
	echo "BENCH_TIMEOUT must be a positive, finite duration: $bench_timeout" >&2
	exit 2
fi
if [[ -z $bench_out ]]; then
	echo "BENCH_OUT must name an output file" >&2
	exit 2
fi
if [[ $bench_out != /* ]]; then
	bench_out=$repo_root/$bench_out
fi
if [[ -d $bench_out ]] || [[ -e $bench_out && ! -f $bench_out ]]; then
	echo "BENCH_OUT must name a regular output file: $bench_out" >&2
	exit 2
fi

# Never inherit the fixed interactive ports supported by the Compose file.
unset PITAYA_NATS_HOST_PORT PITAYA_ETCD_HOST_PORT PITAYA_REDIS_HOST_PORT

tmpdir=$(mktemp -d)
server_binary=$tmpdir/server
supervisor_binary=$tmpdir/benchmark-harness
benchmark_binary=$tmpdir/benchmark.test
supervisor_ready=$tmpdir/supervisor.ready
supervisor_log=$tmpdir/supervisor.log
probe_log=$tmpdir/probe.log
diagnostics_dir=$tmpdir/server-logs
compose=(docker compose --project-name "$project_name" --file "$compose_file")
supervisor_pid=
benchmark_pid=
compose_attempted=0
compose_stopped=0
output_published=0
output_lock=$bench_out.lock
output_lock_owned=0

mkdir -p "$(dirname "$bench_out")"
bench_tmp=$(mktemp "$(dirname "$bench_out")/.pitaya-benchmark-XXXXXX")

wait_for_exit() {
	local pid=$1
	local attempts=$2
	local i
	for ((i = 0; i < attempts; i++)); do
		if ! kill -0 "$pid" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	return 1
}

stop_child() {
	local name=$1
	local pid=$2
	local require_request=${3:-0}
	local requested_signal=
	local status=0
	[[ -z $pid ]] && return 0
	if kill -0 "$pid" 2>/dev/null; then
		if kill -TERM "$pid" 2>/dev/null; then
			requested_signal=TERM
		fi
		if ! wait_for_exit "$pid" 200; then
			echo "$name did not exit after TERM; sending KILL" >&2
			if kill -KILL "$pid" 2>/dev/null; then
				requested_signal=KILL
			fi
			if ! wait_for_exit "$pid" 30; then
				echo "$name did not exit after KILL" >&2
				return 1
			fi
		fi
	fi
	if wait "$pid"; then
		status=0
	else
		status=$?
	fi
	if ((status == 0)); then
		if ((require_request != 0)) && [[ -z $requested_signal ]]; then
			echo "$name exited before requested shutdown" >&2
			return 1
		fi
		return 0
	fi
	if [[ $requested_signal == TERM && $status == 143 ]]; then
		return 0
	fi
	if [[ $requested_signal == KILL && $status == 137 ]]; then
		return 0
	fi
	echo "$name exited with status $status" >&2
	return 1
}

stop_compose() {
	if ((compose_attempted == 0 || compose_stopped != 0)); then
		return 0
	fi
	if ! "${compose[@]}" down --volumes --remove-orphans >&2; then
		return 1
	fi
	compose_stopped=1
}

print_diagnostics() {
	if [[ -s $supervisor_log ]]; then
		echo "benchmark supervisor output:" >&2
		cat "$supervisor_log" >&2
	fi
	for log in "$diagnostics_dir"/*.log; do
		if [[ -s $log ]]; then
			echo "$(basename "$log"):" >&2
			cat "$log" >&2
		fi
	done
	if ((compose_attempted != 0 && compose_stopped == 0)); then
		"${compose[@]}" ps --all >&2 || true
		"${compose[@]}" logs --no-color --tail=200 >&2 || true
	fi
	if [[ -s $probe_log ]]; then
		echo "last readiness probe output:" >&2
		cat "$probe_log" >&2
	fi
}

cleanup() {
	local status=$?
	local cleanup_status=0
	local failed_output
	trap - EXIT
	trap '' INT TERM HUP
	if [[ -n $benchmark_pid ]] && ! stop_child "benchmark" "$benchmark_pid"; then
		cleanup_status=1
	fi
	benchmark_pid=
	if [[ -n $supervisor_pid ]] && ! stop_child "benchmark supervisor" "$supervisor_pid"; then
		cleanup_status=1
	fi
	supervisor_pid=
	if ((status != 0 || cleanup_status != 0)); then
		print_diagnostics
	fi
	if ! stop_compose; then
		cleanup_status=1
	fi
	if ((output_published == 0)) && [[ -s $bench_tmp ]]; then
		failed_output=$bench_out.failed-$project_name
		if mv -f "$bench_tmp" "$failed_output"; then
			echo "failed benchmark output: $failed_output" >&2
		else
			echo "could not retain failed benchmark output: $failed_output" >&2
			cleanup_status=1
		fi
	else
		if ! rm -f "$bench_tmp"; then
			cleanup_status=1
		fi
	fi
	if ! rm -rf "$tmpdir"; then
		cleanup_status=1
	fi
	if ((output_lock_owned != 0)); then
		if ! rmdir "$output_lock"; then
			echo "could not release benchmark output lock: $output_lock" >&2
			cleanup_status=1
		fi
		output_lock_owned=0
	fi
	if ((status == 0 && cleanup_status != 0)); then
		status=$cleanup_status
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if ! mkdir "$output_lock" 2>/dev/null; then
	echo "BENCH_OUT is already in use; choose a distinct output or remove a stale lock: $output_lock" >&2
	exit 2
fi
output_lock_owned=1

(
	cd "$repo_root"
	GOWORK=off go build -trimpath -buildvcs=false -o "$server_binary" ./examples/testing
	GOWORK=off go build -trimpath -buildvcs=false -o "$supervisor_binary" ./benchmark/harness
	GOWORK=off go test -c -tags=benchmark -o "$benchmark_binary" ./benchmark
)

compose_attempted=1
"${compose[@]}" up --detach --wait --wait-timeout 60 nats etcd redis >&2

published_port() {
	local service=$1
	local container_port=$2
	local address
	address=$("${compose[@]}" port "$service" "$container_port")
	printf '%s\n' "${address##*:}"
}

nats_port=$(published_port nats 4222)
etcd_port=$(published_port etcd 2379)
redis_port=$(published_port redis 6379)

PITAYA_TEST_SERVER_BINARY="$server_binary" \
PITAYA_TEST_NATS_URL="nats://127.0.0.1:$nats_port" \
PITAYA_TEST_ETCD_URL="127.0.0.1:$etcd_port" \
PITAYA_TEST_REDIS_URL="127.0.0.1:$redis_port" \
"$supervisor_binary" \
	-ready-file "$supervisor_ready" \
	-diagnostics-dir "$diagnostics_dir" \
	>"$supervisor_log" 2>&1 &
supervisor_pid=$!

ready_deadline=$((SECONDS + 60))
while [[ ! -s $supervisor_ready ]]; do
	if ! kill -0 "$supervisor_pid" 2>/dev/null; then
		echo "benchmark supervisor exited before readiness" >&2
		exit 1
	fi
	if ((SECONDS >= ready_deadline)); then
		echo "benchmark supervisor readiness timed out" >&2
		exit 1
	fi
	sleep 0.1
done

frontend_address=$(<"$supervisor_ready")
frontend_port=${frontend_address##*:}
if [[ ! $frontend_port =~ ^[1-9][0-9]*$ ]] || ((frontend_port > 65535)); then
	echo "invalid benchmark frontend address: $frontend_address" >&2
	exit 1
fi

probe_deadline=$((SECONDS + 30))
until "$benchmark_binary" \
	-test.run='^TestBenchmarkTargetReady$' \
	-test.count=1 \
	-test.timeout=5s \
	-readiness-probe=true \
	-host=127.0.0.1 \
	-port="$frontend_port" \
	>"$probe_log" 2>&1; do
	if ! kill -0 "$supervisor_pid" 2>/dev/null; then
		echo "benchmark supervisor exited during request readiness" >&2
		exit 1
	fi
	if ((SECONDS >= probe_deadline)); then
		echo "benchmark request readiness timed out" >&2
		exit 1
	fi
	sleep 0.1
done

"$benchmark_binary" \
	-test.run='^$' \
	-test.bench="$bench_pattern" \
	-test.benchmem \
	-test.benchtime="$bench_time" \
	-test.count="$bench_count" \
	-test.timeout="$bench_timeout" \
	-host=127.0.0.1 \
	-port="$frontend_port" \
	>"$bench_tmp" 2>&1 &
benchmark_pid=$!
while kill -0 "$benchmark_pid" 2>/dev/null; do
	if ! kill -0 "$supervisor_pid" 2>/dev/null; then
		if wait "$supervisor_pid"; then
			supervisor_status=0
		else
			supervisor_status=$?
		fi
		supervisor_pid=
		echo "benchmark supervisor exited during benchmark with status $supervisor_status" >&2
		exit 1
	fi
	sleep 0.1
done
if wait "$benchmark_pid"; then
	benchmark_status=0
else
	benchmark_status=$?
fi
benchmark_pid=
cat "$bench_tmp"
if ((benchmark_status != 0)); then
	exit "$benchmark_status"
fi
if ! awk '$1 ~ /^Benchmark/ && $2 ~ /^[0-9]+$/ { found = 1 } END { exit found ? 0 : 1 }' "$bench_tmp"; then
	echo "benchmark pattern matched no benchmark samples: $bench_pattern" >&2
	exit 1
fi

if ! stop_child "benchmark supervisor" "$supervisor_pid" 1; then
	supervisor_pid=
	exit 1
fi
supervisor_pid=
if ! stop_compose; then
	exit 1
fi

mv -f "$bench_tmp" "$bench_out"
output_published=1
echo "benchmark output: $bench_out" >&2
