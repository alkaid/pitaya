#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
compose_file=$repo_root/examples/testing/docker-compose.yml
run_id=${GITHUB_RUN_ID:-local}
run_id=${run_id//[^a-zA-Z0-9_-]/-}
workload_mode=${NATS_WORKLOAD:-benchmark}
case $workload_mode in
benchmark | stress) ;;
*)
	echo "NATS_WORKLOAD must be benchmark or stress: $workload_mode" >&2
	exit 2
	;;
esac
project_name="pitaya-${workload_mode}-${run_id,,}-$$"
bench_pattern=${BENCH_PATTERN:-.}
bench_time=${BENCH_TIME:-1s}
bench_count=${BENCH_COUNT:-10}
bench_timeout=${BENCH_TIMEOUT:-20m}
bench_out=${BENCH_OUT:-build/benchmarks/nats-$project_name.txt}
stress_clients=${STRESS_CLIENTS:-10000}
stress_connect_concurrency=${STRESS_CONNECT_CONCURRENCY:-256}
stress_request_concurrency=${STRESS_REQUEST_CONCURRENCY:-256}
stress_reconnects=${STRESS_RECONNECTS:-1000}
stress_reconnect_concurrency=${STRESS_RECONNECT_CONCURRENCY:-64}
stress_bind_retries=${STRESS_BIND_RETRIES:-1000}
stress_request_timeout=${STRESS_REQUEST_TIMEOUT:-10s}
stress_internal_timeout=${STRESS_INTERNAL_TIMEOUT:-12m}
stress_cleanup_timeout=${STRESS_CLEANUP_TIMEOUT:-30s}
stress_cleanup_max=${STRESS_CLEANUP_MAX:-30s}
stress_goroutine_delta_max=${STRESS_GOROUTINE_DELTA_MAX:-5}
stress_timeout=${STRESS_TIMEOUT:-15m}
# Cleanup closes every resident session and can publish 10,000 SessionClosed
# notifications faster than the isolated backend drains them. This stress-only
# profile covers that declared burst; it does not change the framework default.
stress_nats_messages_buffer=${STRESS_NATS_MESSAGES_BUFFER:-16384}
stress_out=${STRESS_OUT:-build/stress/nats-$project_name.json}

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

validate_positive_integer() {
	local name=$1
	local value=$2
	local maximum=$3
	if [[ ! $value =~ ^[1-9][0-9]*$ ]] || ((${#value} > ${#maximum})) || ((value > maximum)); then
		echo "$name must be an integer between 1 and $maximum: $value" >&2
		exit 2
	fi
}

validate_regular_output_target() {
	local name=$1
	local path=$2
	if [[ -d $path ]] || [[ -e $path && ! -f $path ]]; then
		echo "$name must name a regular output file: $path" >&2
		return 1
	fi
}

if [[ $workload_mode == benchmark ]]; then
	validate_positive_integer BENCH_COUNT "$bench_count" 100
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
	artifact_out=$bench_out
	artifact_name=BENCH_OUT
else
	# This is a Linux capacity lane. Record and validate host limits instead of
	# changing sysctls or silently running fewer than the configured clients.
	validate_positive_integer STRESS_CLIENTS "$stress_clients" 20000
	validate_positive_integer STRESS_CONNECT_CONCURRENCY "$stress_connect_concurrency" 1024
	validate_positive_integer STRESS_REQUEST_CONCURRENCY "$stress_request_concurrency" 1024
	validate_positive_integer STRESS_RECONNECTS "$stress_reconnects" 10000
	validate_positive_integer STRESS_RECONNECT_CONCURRENCY "$stress_reconnect_concurrency" 1024
	validate_positive_integer STRESS_BIND_RETRIES "$stress_bind_retries" 100000
	validate_positive_integer STRESS_NATS_MESSAGES_BUFFER "$stress_nats_messages_buffer" 65536
	if [[ ! $stress_goroutine_delta_max =~ ^[0-9]+$ ]] || ((${#stress_goroutine_delta_max} > 4)) || ((stress_goroutine_delta_max > 1000)); then
		echo "STRESS_GOROUTINE_DELTA_MAX must be an integer between 0 and 1000: $stress_goroutine_delta_max" >&2
		exit 2
	fi
	if ((stress_clients < 2)); then
		echo "STRESS_CLIENTS must be at least 2: $stress_clients" >&2
		exit 2
	fi
	if ((stress_reconnects >= stress_clients)); then
		echo "STRESS_RECONNECTS must be less than STRESS_CLIENTS: $stress_reconnects >= $stress_clients" >&2
		exit 2
	fi
	if ((stress_connect_concurrency > stress_clients || stress_request_concurrency > stress_clients)); then
		echo "stress connect/request concurrency must not exceed STRESS_CLIENTS" >&2
		exit 2
	fi
	if ((stress_reconnect_concurrency > stress_reconnects)); then
		echo "STRESS_RECONNECT_CONCURRENCY must not exceed STRESS_RECONNECTS" >&2
		exit 2
	fi
	for duration_name in STRESS_REQUEST_TIMEOUT STRESS_INTERNAL_TIMEOUT STRESS_CLEANUP_TIMEOUT STRESS_CLEANUP_MAX STRESS_TIMEOUT; do
		case $duration_name in
		STRESS_REQUEST_TIMEOUT) duration_value=$stress_request_timeout ;;
		STRESS_INTERNAL_TIMEOUT) duration_value=$stress_internal_timeout ;;
		STRESS_CLEANUP_TIMEOUT) duration_value=$stress_cleanup_timeout ;;
		STRESS_CLEANUP_MAX) duration_value=$stress_cleanup_max ;;
		STRESS_TIMEOUT) duration_value=$stress_timeout ;;
		esac
		if ! is_positive_duration "$duration_value"; then
			echo "$duration_name must be a positive, finite duration: $duration_value" >&2
			exit 2
		fi
	done
	stress_soft_file_limit=$(ulimit -Sn)
	stress_file_limit_unlimited=false
	if [[ $stress_soft_file_limit == unlimited ]]; then
		stress_soft_file_limit=-1
		stress_file_limit_unlimited=true
	elif [[ ! $stress_soft_file_limit =~ ^[1-9][0-9]*$ ]]; then
		echo "stress requires a finite numeric soft file limit: $stress_soft_file_limit" >&2
		exit 2
	fi
	if [[ ! -r /proc/sys/net/ipv4/ip_local_port_range ]]; then
		echo "stress requires Linux /proc/sys/net/ipv4/ip_local_port_range" >&2
		exit 2
	fi
	read -r stress_ephemeral_low stress_ephemeral_high </proc/sys/net/ipv4/ip_local_port_range
	if [[ ! $stress_ephemeral_low =~ ^[1-9][0-9]*$ || ! $stress_ephemeral_high =~ ^[1-9][0-9]*$ ]]; then
		echo "could not read the Linux ephemeral port range" >&2
		exit 2
	fi
	artifact_out=$stress_out
	artifact_name=STRESS_OUT
fi

if [[ -z $artifact_out ]]; then
	echo "$artifact_name must name an output file" >&2
	exit 2
fi
if [[ $artifact_out != /* ]]; then
	artifact_out=$repo_root/$artifact_out
fi
if ! validate_regular_output_target "$artifact_name" "$artifact_out"; then
	exit 2
fi
if [[ $workload_mode == benchmark ]]; then
	bench_out=$artifact_out
else
	stress_out=$artifact_out
	if ! validate_regular_output_target STRESS_OUT.log "$stress_out.log"; then
		exit 2
	fi
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
output_lock=$artifact_out.lock
output_lock_owned=0
uncommitted_outputs=()
stress_report_tmp=

mkdir -p "$(dirname "$artifact_out")"
bench_tmp=$(mktemp "$(dirname "$artifact_out")/.pitaya-${workload_mode}-log-XXXXXX")
if [[ $workload_mode == stress ]]; then
	stress_report_tmp=$(mktemp "$(dirname "$stress_out")/.pitaya-stress-report-XXXXXX")
	# The workload must atomically create this unique path; an empty file from
	# mktemp would otherwise let a zero-match test look like fresh evidence.
	rm -f "$stress_report_tmp"
fi

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
	# Cleanup may accept signal exits so resources are not leaked. Formal stress
	# evidence requires the supervisor to handle TERM, join both servers, write
	# diagnostics, and return success itself.
	if ((require_request != 0)); then
		if [[ $requested_signal != TERM ]]; then
			echo "$name did not receive the requested TERM shutdown" >&2
			return 1
		fi
		if ((status != 0)); then
			echo "$name exited with status $status after TERM; want 0" >&2
			return 1
		fi
		return 0
	fi
	if ((status == 0)); then
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

finalize_success_resources() {
	if [[ -n $tmpdir ]]; then
		if ! rm -rf -- "$tmpdir"; then
			echo "could not remove benchmark temporary directory" >&2
			return 1
		fi
		tmpdir=
	fi
	if ! validate_regular_output_target "$artifact_name" "$artifact_out"; then
		return 1
	fi
	if [[ $workload_mode == stress ]] && ! validate_regular_output_target STRESS_OUT.log "$stress_out.log"; then
		return 1
	fi
}

release_output_lock() {
	if ((output_lock_owned != 0)); then
		if ! rmdir -- "$output_lock"; then
			echo "could not release benchmark output lock: $output_lock" >&2
			return 1
		fi
		output_lock_owned=0
	fi
}

rollback_uncommitted_outputs() {
	local path
	local rollback_status=0
	for path in "${uncommitted_outputs[@]}"; do
		if ! rm -f -- "$path"; then
			echo "could not retract unpublished output: $path" >&2
			rollback_status=1
		fi
	done
	uncommitted_outputs=()
	return "$rollback_status"
}

publish_regular_output() {
	local source=$1
	local target=$2
	if ! validate_regular_output_target output "$target"; then
		return 1
	fi
	# GNU mv can reject a directory target atomically. Other platforms retain the
	# existing benchmark support and fail the postcondition if the target changes.
	if mv --help 2>&1 | grep -q -- '--no-target-directory'; then
		if ! mv -T -f -- "$source" "$target"; then
			return 1
		fi
	elif ! mv -f "$source" "$target"; then
		return 1
	fi
	if [[ ! -f $target || -L $target ]]; then
		echo "published output is not an exact regular file: $target" >&2
		return 1
	fi
	uncommitted_outputs+=("$target")
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

validate_stress_diagnostics() {
	local effective_buffer_marker="pitaya test effective NATS messages buffer: $stress_nats_messages_buffer"
	local name
	local log
	local grep_status
	for name in frontend backend; do
		log=$diagnostics_dir/$name.log
		if [[ ! -f $log || -L $log || ! -s $log ]]; then
			echo "stress supervisor produced no regular $name diagnostic log" >&2
			return 1
		fi
		if grep -Fqx -- "$effective_buffer_marker" "$log"; then
			:
		else
			grep_status=$?
			if ((grep_status == 1)); then
				echo "stress $name did not confirm $effective_buffer_marker" >&2
			else
				echo "could not verify stress server profile in $log: grep exited $grep_status" >&2
			fi
			return 1
		fi
		# The supervisor owns drop matching and retains those markers after its
		# bounded log tail rolls. Its required zero exit proves neither server saw a
		# drop; this pass independently confirms the declared effective profile.
	done
}

cleanup() {
	local status=$?
	local cleanup_status=0
	local diagnostic_name
	local diagnostic_source
	local failed_output
	local failed_log
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
	if ((output_published == 0)) && ! rollback_uncommitted_outputs; then
		cleanup_status=1
	fi
	if ((output_published == 0)) && [[ $workload_mode == stress ]]; then
		failed_output=$stress_out.failed-$project_name
		failed_log=$failed_output.log
		for diagnostic_name in supervisor frontend backend; do
			case $diagnostic_name in
			supervisor) diagnostic_source=$supervisor_log ;;
			frontend | backend) diagnostic_source=$diagnostics_dir/$diagnostic_name.log ;;
			esac
			if [[ -f $diagnostic_source && ! -L $diagnostic_source && -s $diagnostic_source ]]; then
				if cp -f -- "$diagnostic_source" "$failed_output.$diagnostic_name.log"; then
					echo "failed stress $diagnostic_name diagnostics: $failed_output.$diagnostic_name.log" >&2
				else
					echo "could not retain failed stress $diagnostic_name diagnostics" >&2
					cleanup_status=1
				fi
			fi
		done
		if [[ -s $stress_report_tmp ]]; then
			if mv -f "$stress_report_tmp" "$failed_output"; then
				echo "failed stress report: $failed_output" >&2
			else
				echo "could not retain failed stress report: $failed_output" >&2
				cleanup_status=1
			fi
		else
			rm -f "$stress_report_tmp"
		fi
		if [[ -s $bench_tmp ]]; then
			if mv -f "$bench_tmp" "$failed_log"; then
				echo "failed stress log: $failed_log" >&2
			else
				echo "could not retain failed stress log: $failed_log" >&2
				cleanup_status=1
			fi
		else
			rm -f "$bench_tmp"
		fi
	elif ((output_published == 0)) && [[ -s $bench_tmp ]]; then
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
		if [[ -n $stress_report_tmp ]] && ! rm -f "$stress_report_tmp"; then
			cleanup_status=1
		fi
	fi
	if [[ -n $tmpdir ]] && ! rm -rf -- "$tmpdir"; then
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
	echo "$artifact_name is already in use; choose a distinct output or remove a stale lock: $output_lock" >&2
	exit 2
fi
output_lock_owned=1

benchmark_tags=benchmark
if [[ $workload_mode == stress ]]; then
	benchmark_tags=benchmark,stress
fi
(
	cd "$repo_root"
	GOWORK=off go build -trimpath -buildvcs=false -o "$server_binary" ./examples/testing
	GOWORK=off go build -trimpath -buildvcs=false -o "$supervisor_binary" ./benchmark/harness
	GOWORK=off go test -c -tags="$benchmark_tags" -o "$benchmark_binary" ./benchmark
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
supervisor_nats_messages_buffer=
if [[ $workload_mode == stress ]]; then
	supervisor_nats_messages_buffer=$stress_nats_messages_buffer
fi

PITAYA_TEST_SERVER_BINARY="$server_binary" \
PITAYA_TEST_NATS_URL="nats://127.0.0.1:$nats_port" \
PITAYA_TEST_ETCD_URL="127.0.0.1:$etcd_port" \
PITAYA_TEST_REDIS_URL="127.0.0.1:$redis_port" \
PITAYA_TEST_NATS_MESSAGES_BUFFER="$supervisor_nats_messages_buffer" \
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

if [[ $workload_mode == benchmark ]]; then
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
else
	# Exact selection, the explicit run flag, and a fresh run-id-bound report are
	# all required so a zero-match, skipped test, or stale artifact cannot pass.
	"$benchmark_binary" \
		-test.run='^TestNATSStress$' \
		-test.v \
		-test.count=1 \
		-test.timeout="$stress_timeout" \
		-stress-run=true \
		-stress-report="$stress_report_tmp" \
		-stress-run-id="$project_name" \
		-stress-clients="$stress_clients" \
		-stress-connect-concurrency="$stress_connect_concurrency" \
		-stress-request-concurrency="$stress_request_concurrency" \
		-stress-reconnects="$stress_reconnects" \
		-stress-reconnect-concurrency="$stress_reconnect_concurrency" \
		-stress-bind-retries="$stress_bind_retries" \
		-stress-request-timeout="$stress_request_timeout" \
		-stress-internal-timeout="$stress_internal_timeout" \
		-stress-cleanup-timeout="$stress_cleanup_timeout" \
		-stress-cleanup-max="$stress_cleanup_max" \
		-stress-goroutine-delta-max="$stress_goroutine_delta_max" \
		-stress-nats-messages-buffer="$stress_nats_messages_buffer" \
		-stress-soft-file-limit="$stress_soft_file_limit" \
		-stress-file-limit-unlimited="$stress_file_limit_unlimited" \
		-stress-ephemeral-low="$stress_ephemeral_low" \
		-stress-ephemeral-high="$stress_ephemeral_high" \
		-host=127.0.0.1 \
		-port="$frontend_port" \
		>"$bench_tmp" 2>&1 &
fi
benchmark_pid=$!
while kill -0 "$benchmark_pid" 2>/dev/null; do
	if ! kill -0 "$supervisor_pid" 2>/dev/null; then
		if wait "$supervisor_pid"; then
			supervisor_status=0
		else
			supervisor_status=$?
		fi
		supervisor_pid=
		echo "benchmark supervisor exited during $workload_mode with status $supervisor_status" >&2
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
if [[ $workload_mode == benchmark ]]; then
	if ! awk '$1 ~ /^Benchmark/ && $2 ~ /^[0-9]+$/ { found = 1 } END { exit found ? 0 : 1 }' "$bench_tmp"; then
		echo "benchmark pattern matched no benchmark samples: $bench_pattern" >&2
		exit 1
	fi
else
	if [[ ! -f $stress_report_tmp || -L $stress_report_tmp || ! -s $stress_report_tmp ]]; then
		echo "stress workload produced no fresh regular report" >&2
		exit 1
	fi
	if ! "$benchmark_binary" \
		-test.run='^TestNATSStress$' \
		-test.count=1 \
		-test.timeout=30s \
		-stress-validate-report=true \
		-stress-report="$stress_report_tmp" \
		-stress-run-id="$project_name" \
		-stress-clients="$stress_clients" \
		-stress-connect-concurrency="$stress_connect_concurrency" \
		-stress-request-concurrency="$stress_request_concurrency" \
		-stress-reconnects="$stress_reconnects" \
		-stress-reconnect-concurrency="$stress_reconnect_concurrency" \
		-stress-bind-retries="$stress_bind_retries" \
		-stress-request-timeout="$stress_request_timeout" \
		-stress-internal-timeout="$stress_internal_timeout" \
		-stress-cleanup-timeout="$stress_cleanup_timeout" \
		-stress-cleanup-max="$stress_cleanup_max" \
		-stress-goroutine-delta-max="$stress_goroutine_delta_max" \
		-stress-nats-messages-buffer="$stress_nats_messages_buffer" \
		-stress-soft-file-limit="$stress_soft_file_limit" \
		-stress-file-limit-unlimited="$stress_file_limit_unlimited" \
		-stress-ephemeral-low="$stress_ephemeral_low" \
		-stress-ephemeral-high="$stress_ephemeral_high" \
		-host=127.0.0.1 \
		-port="$frontend_port" \
		>>"$bench_tmp" 2>&1; then
		echo "stress report validation failed" >&2
		cat "$bench_tmp" >&2
		exit 1
	fi
fi

if ! stop_child "benchmark supervisor" "$supervisor_pid" 1; then
	supervisor_pid=
	exit 1
fi
supervisor_pid=
if [[ $workload_mode == stress ]] && ! validate_stress_diagnostics; then
	exit 1
fi
if ! stop_compose; then
	exit 1
fi
if ! finalize_success_resources; then
	exit 1
fi

# Keep the output lock through the atomic renames. Ignoring handled signals over
# this short commit section prevents EXIT cleanup from retracting an output after
# another process acquires the released lock.
trap '' INT TERM HUP
if [[ $workload_mode == benchmark ]]; then
	if ! publish_regular_output "$bench_tmp" "$bench_out"; then
		exit 1
	fi
else
	# The report is moved last and becomes formal only after the output lock is
	# released. The adjacent log remains supporting evidence.
	if ! publish_regular_output "$bench_tmp" "$stress_out.log"; then
		exit 1
	fi
	if ! publish_regular_output "$stress_report_tmp" "$stress_out"; then
		exit 1
	fi
fi
if ! release_output_lock; then
	exit 1
fi
output_published=1
uncommitted_outputs=()
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if [[ $workload_mode == benchmark ]]; then
	echo "benchmark output: $bench_out" >&2
else
	echo "stress report: $stress_out" >&2
	echo "stress log: $stress_out.log" >&2
fi
