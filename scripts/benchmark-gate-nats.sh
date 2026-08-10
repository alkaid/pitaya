#!/usr/bin/env bash
set -euo pipefail

# Commit identities must resolve in the explicit -C repositories below. Ignore
# inherited repository routing and replacement-object state before the first Git
# command, or a clean checkout could expose bytes other than the pinned object.
git_config_variables=("${!GIT_CONFIG_@}")
for git_config_variable in "${git_config_variables[@]}"; do
	unset "$git_config_variable"
done
unset git_config_variable git_config_variables GIT_CONFIG GIT_CONFIG_PARAMETERS
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY
unset GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_INDEX_FILE GIT_NAMESPACE GIT_REPLACE_REF_BASE
unset GIT_GRAFT_FILE GIT_SHALLOW_FILE GIT_ATTR_GLOBAL GIT_ATTR_SYSTEM GIT_ATTR_SOURCE
export GIT_NO_REPLACE_OBJECTS=1
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_ATTR_NOSYSTEM=1
export GIT_CONFIG_COUNT=4
export GIT_CONFIG_KEY_0=core.hooksPath
export GIT_CONFIG_VALUE_0=/dev/null
export GIT_CONFIG_KEY_1=core.fsmonitor
export GIT_CONFIG_VALUE_1=false
export GIT_CONFIG_KEY_2=core.untrackedCache
export GIT_CONFIG_VALUE_2=false
export GIT_CONFIG_KEY_3=core.attributesFile
export GIT_CONFIG_VALUE_3=/dev/null

gate_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
requested_repo_root=${BENCH_REPO_ROOT:-$gate_root}
if ! repo_root=$(cd "$requested_repo_root" 2>/dev/null && pwd -P); then
	echo "BENCH_REPO_ROOT must name an accessible Git checkout: $requested_repo_root" >&2
	exit 2
fi
if [[ $gate_root == *$'\n'* || $gate_root == *$'\r'* || $repo_root == *$'\n'* || $repo_root == *$'\r'* ]]; then
	echo "benchmark gate and candidate checkout paths must be single-line" >&2
	exit 2
fi
base_ref=${BENCH_BASE_REF:-HEAD^}
candidate_ref=${BENCH_CANDIDATE_REF:-HEAD}
runner_id=${BENCH_RUNNER_ID:-}
runner_lock=${BENCH_RUNNER_LOCK:-}
expected_policy_digest=${BENCH_POLICY_SHA256:-}
expected_gate_source_sha=${BENCH_GATE_SOURCE_SHA:-}
bench_pattern=.
bench_time=1s
bench_count=10
bench_timeout=20m
outer_timeout_seconds=1800

resolve_requested_commit() {
	local name=$1
	local revision=$2
	local resolved
	if ! resolved=$(git -C "$repo_root" rev-parse --verify "$revision^{commit}"); then
		echo "$name does not resolve to a commit: $revision" >&2
		return 2
	fi
	printf '%s\n' "$resolved"
}

reject_git_metadata_overrides() {
	local label=$1
	local repo=$2
	local common_dir grafts attributes
	if ! common_dir=$(git -C "$repo" rev-parse --path-format=absolute --git-common-dir); then
		echo "could not locate $label Git metadata" >&2
		return 1
	fi
	grafts=$common_dir/info/grafts
	# GIT_NO_REPLACE_OBJECTS does not disable legacy grafts. A graft can make an
	# unrelated base pass the ancestry check without changing either reported SHA.
	if [[ -L $grafts || -e $grafts && ! -f $grafts ]]; then
		echo "$label Git grafts path must be a regular non-symlink file: $grafts" >&2
		return 2
	fi
	if [[ -s $grafts ]]; then
		echo "$label uses unsupported legacy Git grafts: $grafts" >&2
		return 2
	fi
	attributes=$common_dir/info/attributes
	# Repository-private attributes can alter archive selection and attach clean
	# or smudge filters without changing the pinned commit or clean status.
	if [[ -L $attributes || -e $attributes && ! -f $attributes ]]; then
		echo "$label Git attributes path must be a regular non-symlink file: $attributes" >&2
		return 2
	fi
	if [[ -s $attributes ]]; then
		echo "$label uses unsupported private Git attributes: $attributes" >&2
		return 2
	fi
}

ensure_directory_without_symlinks() {
	local path=$1
	local current=/
	local component
	local -a components
	IFS=/ read -r -a components <<<"${path#/}"
	for component in "${components[@]}"; do
		[[ -z $component || $component == . ]] && continue
		if [[ $component == .. ]]; then
			echo "benchmark output parent must not contain '..': $path" >&2
			return 2
		fi
		current=${current%/}/$component
		if [[ -L $current ]]; then
			echo "benchmark output parent contains a symlink: $current" >&2
			return 2
		fi
		if [[ -e $current ]]; then
			if [[ ! -d $current ]]; then
				echo "benchmark output parent component is not a directory: $current" >&2
				return 2
			fi
		elif ! mkdir -- "$current"; then
			echo "could not create benchmark output parent: $current" >&2
			return 1
		fi
	done
}

# Trust boundary: a protected runner invokes this script from a separately
# reviewed gate checkout and pins its commit plus policy digest. BENCH_REPO_ROOT
# identifies a trusted maintainer-authored candidate; the same-UID harness is not
# a sandbox for hostile code. It need not contain the verifier or policy. Local
# same-checkout and untrusted-PR runs remain informational.
if [[ -z $runner_id || ! $runner_id =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$ ]]; then
	echo "BENCH_RUNNER_ID must identify a dedicated fixed runner" >&2
	exit 2
fi
if [[ $runner_lock != /* ]]; then
	echo "BENCH_RUNNER_LOCK must be an absolute host-wide lock path" >&2
	exit 2
fi
if [[ ! $expected_policy_digest =~ ^[0-9a-f]{64}$ ]]; then
	echo "BENCH_POLICY_SHA256 must be the reviewed 64-character lowercase policy digest" >&2
	exit 2
fi
if [[ ! $expected_gate_source_sha =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]]; then
	echo "BENCH_GATE_SOURCE_SHA must be the reviewed full gate checkout commit" >&2
	exit 2
fi
# The policy hashes Linux-only harness inputs and normalizes GOAMD64 below, so
# another host platform would be a different experiment rather than a valid run.
if ! host_platform=$(uname -sm); then
	echo "could not identify benchmark runner platform" >&2
	exit 1
fi
if [[ $host_platform != "Linux x86_64" ]]; then
	echo "benchmark gate requires a Linux x86_64 fixed runner, found $host_platform" >&2
	exit 1
fi
if [[ -L $runner_lock || -e $runner_lock && ! -f $runner_lock ]]; then
	echo "BENCH_RUNNER_LOCK must name a regular file, not a symlink: $runner_lock" >&2
	exit 2
fi
if [[ ! -d $(dirname "$runner_lock") ]]; then
	echo "BENCH_RUNNER_LOCK parent does not exist: $(dirname "$runner_lock")" >&2
	exit 2
fi

reject_git_metadata_overrides "trusted gate source" "$gate_root" || exit $?
if [[ $repo_root != "$gate_root" ]]; then
	reject_git_metadata_overrides "candidate checkout" "$repo_root" || exit $?
fi

if ! gate_source_sha=$(git -C "$gate_root" rev-parse --verify HEAD^{commit}); then
	echo "could not resolve the trusted gate source checkout" >&2
	exit 1
fi
if [[ $gate_source_sha != "$expected_gate_source_sha" ]]; then
	echo "trusted gate source is $gate_source_sha, want reviewed $expected_gate_source_sha" >&2
	exit 2
fi
if ! gate_source_status=$(git -C "$gate_root" status --porcelain=v1 --untracked-files=all); then
	echo "could not inspect the trusted gate source checkout" >&2
	exit 1
fi
if [[ -n $gate_source_status ]]; then
	echo "benchmark gate requires a clean trusted source checkout:" >&2
	echo "$gate_source_status" >&2
	exit 2
fi

if ! candidate_sha=$(resolve_requested_commit BENCH_CANDIDATE_REF "$candidate_ref"); then
	exit 2
fi
if ! base_sha=$(resolve_requested_commit BENCH_BASE_REF "$base_ref"); then
	exit 2
fi
if ! checkout_sha=$(git -C "$repo_root" rev-parse --verify HEAD^{commit}); then
	echo "could not resolve the candidate checkout" >&2
	exit 1
fi
if [[ $candidate_sha != "$checkout_sha" ]]; then
	echo "BENCH_CANDIDATE_REF resolves to $candidate_sha, but BENCH_REPO_ROOT is $checkout_sha" >&2
	exit 2
fi
if [[ $base_sha == "$candidate_sha" ]]; then
	echo "benchmark base and candidate must be different commits" >&2
	exit 2
fi
if ! git -C "$repo_root" merge-base --is-ancestor "$base_sha" "$candidate_sha"; then
	echo "benchmark base $base_sha is not an ancestor of candidate $candidate_sha" >&2
	exit 2
fi
if ! checkout_status=$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all); then
	echo "could not inspect the candidate checkout" >&2
	exit 1
fi
if [[ -n $checkout_status ]]; then
	echo "benchmark gate requires a clean candidate checkout:" >&2
	echo "$checkout_status" >&2
	exit 2
fi

if ! read -r run_nonce </proc/sys/kernel/random/uuid; then
	echo "could not generate a benchmark run nonce" >&2
	exit 1
fi
run_nonce=${run_nonce//-/}
candidate_short=${candidate_sha:0:12}
if [[ -n ${GITHUB_RUN_ID:-} ]]; then
	run_id=github-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT:-1}-${candidate_short}-${run_nonce:0:12}
else
	run_id=local-$(date -u +%Y%m%dT%H%M%SZ)-$$-${candidate_short}-${run_nonce:0:12}
fi
if [[ ! $run_id =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$ ]]; then
	echo "generated benchmark run ID is invalid: $run_id" >&2
	exit 2
fi

gate_out=${BENCH_GATE_OUT:-build/benchmarks/nats-gate-$run_id}
if [[ -z $gate_out ]]; then
	echo "BENCH_GATE_OUT must name a new output directory" >&2
	exit 2
fi
if [[ $gate_out == *$'\n'* || $gate_out == *$'\r'* ]]; then
	echo "BENCH_GATE_OUT must be a single-line path" >&2
	exit 2
fi
relative_gate_out=0
if [[ $gate_out != /* ]]; then
	relative_gate_out=1
	gate_out=$repo_root/$gate_out
fi
gate_leaf=$(basename -- "$gate_out")
if [[ $gate_leaf == . || $gate_leaf == .. || -z $gate_leaf ]]; then
	echo "BENCH_GATE_OUT must name a directory below an output parent: $gate_out" >&2
	exit 2
fi
if [[ -e $gate_out || -L $gate_out ]]; then
	echo "BENCH_GATE_OUT already exists; refusing stale output: $gate_out" >&2
	exit 2
fi
gate_parent=$(dirname "$gate_out")
output_lock=$gate_out.lock
stage=$gate_out.incomplete-$run_id
failed=$gate_out.failed-$run_id

lock_owned=0
output_lock_identity=
stage_created=0
runner_lock_open=0
scratch=
base_worktree=
candidate_worktree=
binary_dir=
benchgate_binary=
benchstat_binary=
reviewed_policy=
trusted_source=
trusted_source_archive=
base_added=0
candidate_added=0
published=0
output_moved=0
active_pid=
active_pgid=
usage_failure=0
signal_critical=0
pending_signal=
pending_signal_status=0
signal_exit_status=0

worktree_registered() {
	local path=$1
	local entry
	local list_file=$scratch/worktrees-$run_nonce.list
	local registered=1
	# Keep "not registered" distinct from "Git could not answer". Treating both
	# as false could let cleanup delete a worktree directory while stale Git
	# metadata still points at it after a partially successful worktree add.
	if ! git -C "$repo_root" worktree list --porcelain -z >"$list_file"; then
		rm -f -- "$list_file"
		echo "could not inspect registered benchmark worktrees" >&2
		return 2
	fi
	while IFS= read -r -d '' entry; do
		if [[ $entry == "worktree $path" ]]; then
			registered=0
			break
		fi
	done <"$list_file"
	rm -f -- "$list_file"
	return "$registered"
}

output_lock_matches() {
	[[ -d $output_lock && ! -L $output_lock && -n $output_lock_identity ]] || return 1
	[[ $(stat -c '%d:%i' -- "$output_lock" 2>/dev/null) == "$output_lock_identity" ]]
}

ensure_quarantine_lock() {
	if output_lock_matches; then
		return 0
	fi
	if [[ ! -e $output_lock && ! -L $output_lock ]] && mkdir "$output_lock"; then
		output_lock_identity=$(stat -c '%d:%i' -- "$output_lock") || return 1
		lock_owned=1
		return 0
	fi
	return 1
}

release_owned_output_lock() {
	# Keep the canonical marker visible until the final rmdir. Moving it to a
	# private claim would make a crash leave formal output looking unlocked. The
	# identity check and rmdir are separate pathname operations; replacing the
	# directory between them requires the same-UID mutation excluded by this
	# trusted-maintainer threat model.
	if ! output_lock_matches; then
		return 1
	fi
	if ! rmdir "$output_lock"; then
		return 1
	fi
	lock_owned=0
	output_lock_identity=
	return 0
}

remove_worktrees() {
	local status=0
	local registration_status
	# Registration is authoritative even if worktree add exited after creating
	# the Git metadata but before the shell recorded candidate_added/base_added.
	if [[ -n $candidate_worktree ]]; then
		registration_status=1
		if ((candidate_added != 0)); then
			registration_status=0
		elif worktree_registered "$candidate_worktree"; then
			registration_status=0
		else
			registration_status=$?
		fi
		if ((registration_status == 0)); then
			if git -C "$repo_root" worktree remove --force "$candidate_worktree"; then
				candidate_added=0
			else
				status=1
			fi
		elif ((registration_status != 1)); then
			status=1
		fi
	fi
	if [[ -n $base_worktree ]]; then
		registration_status=1
		if ((base_added != 0)); then
			registration_status=0
		elif worktree_registered "$base_worktree"; then
			registration_status=0
		else
			registration_status=$?
		fi
		if ((registration_status == 0)); then
			if git -C "$repo_root" worktree remove --force "$base_worktree"; then
				base_added=0
			else
				status=1
			fi
		elif ((registration_status != 1)); then
			status=1
		fi
	fi
	return "$status"
}

remove_scratch() {
	local registration_status
	if [[ -z $scratch ]]; then
		return 0
	fi
	if ((base_added != 0 || candidate_added != 0)); then
		echo "benchmark scratch retained because a worktree is still registered: $scratch" >&2
		return 1
	fi
	if worktree_registered "$base_worktree"; then
		registration_status=0
	else
		registration_status=$?
	fi
	if ((registration_status != 1)); then
		echo "benchmark scratch retained because worktree registration is not clear: $scratch" >&2
		return 1
	fi
	if worktree_registered "$candidate_worktree"; then
		registration_status=0
	else
		registration_status=$?
	fi
	if ((registration_status != 1)); then
		echo "benchmark scratch retained because worktree registration is not clear: $scratch" >&2
		return 1
	fi
	if [[ -n $trusted_source_archive ]]; then
		rm -f -- "$trusted_source_archive"
		trusted_source_archive=
	fi
	if [[ -n $trusted_source ]]; then
		if [[ $trusted_source != "$scratch/"* ]]; then
			echo "refusing unexpected trusted source cleanup path: $trusted_source" >&2
			return 1
		fi
		rm -rf -- "$trusted_source"
		trusted_source=
	fi
	if [[ -n $benchgate_binary ]]; then
		rm -f -- "$benchgate_binary"
	fi
	if [[ -n $benchstat_binary ]]; then
		rm -f -- "$benchstat_binary"
	fi
	if [[ -n $reviewed_policy ]]; then
		rm -f -- "$reviewed_policy"
	fi
	if [[ -n $binary_dir && -d $binary_dir ]] && ! rmdir "$binary_dir"; then
		echo "benchmark binary directory is not empty: $binary_dir" >&2
		return 1
	fi
	if [[ -d $scratch ]] && ! rmdir "$scratch"; then
		echo "benchmark scratch directory is not empty: $scratch" >&2
		return 1
	fi
	# EXIT cleanup may run after the success path already removed scratch. Clear
	# its child paths together so the second pass is a no-op, not a fresh query.
	base_worktree=
	candidate_worktree=
	scratch=
}

wait_for_active_exit() {
	local attempts=$1
	local i
	for ((i = 0; i < attempts; i++)); do
		if ! active_process_alive; then
			return 0
		fi
		sleep 1
	done
	return 1
}

active_process_alive() {
	if [[ -n $active_pid ]] && kill -0 "$active_pid" 2>/dev/null; then
		return 0
	fi
	if [[ -n $active_pgid ]] && kill -0 -- "-$active_pgid" 2>/dev/null; then
		return 0
	fi
	return 1
}

stop_active() {
	local status=0
	if [[ -z $active_pid ]]; then
		return 0
	fi
	if active_process_alive; then
		# Let the harness owner perform its ordered child/Compose cleanup first.
		# Only address the group directly if the leader has already disappeared.
		if kill -0 "$active_pid" 2>/dev/null; then
			kill -TERM "$active_pid" 2>/dev/null || true
		else
			kill -TERM -- "-$active_pgid" 2>/dev/null || true
			status=1
		fi
		if ! wait_for_active_exit 90; then
			echo "benchmark harness did not exit after TERM; stopping its process group" >&2
			kill -TERM -- "-$active_pgid" 2>/dev/null || true
			if ! wait_for_active_exit 10; then
				kill -KILL -- "-$active_pgid" 2>/dev/null || true
			fi
			if ! wait_for_active_exit 10; then
				echo "benchmark harness process group did not exit after KILL" >&2
				return 1
			fi
			status=1
		fi
	fi
	wait "$active_pid" 2>/dev/null || true
	active_pid=
	active_pgid=
	return "$status"
}

cleanup() {
	local status=$?
	local cleanup_status=0
	local failed_source
	local release_output_lock=1
	trap '' INT TERM HUP
	# Only explicit post-trap usage failures and a formally published comparison
	# may retain statuses 2 and 3. Any internal command using those values is an
	# infrastructure failure, never a user error or regression decision.
	if ((status == 2 && usage_failure == 0)); then
		status=1
	elif ((status == 3 && published == 0)); then
		status=1
	elif ((status != 0 && status != 1 && status != 2 && status != 3 && status != signal_exit_status)); then
		status=1
	fi
	if ! stop_active; then
		cleanup_status=1
	fi
	if ! remove_worktrees; then
		cleanup_status=1
	fi
	if ! remove_scratch; then
		cleanup_status=1
	fi
	if ((cleanup_status != 0)); then
		status=1
	fi
	# Infrastructure failures retain evidence under a failed name. Exit 3 is a
	# completed regression decision and reaches here only after formal publish.
	if ((status != 0 && published == 0 && stage_created != 0)); then
		failed_source=$stage
		if ((output_moved != 0)); then
			failed_source=$gate_out
		fi
		if [[ -d $failed_source && ! -L $failed_source && ! -e $failed && ! -L $failed ]] &&
			mv -T -n -- "$failed_source" "$failed" && [[ ! -e $failed_source && ! -L $failed_source ]]; then
			echo "failed benchmark evidence: $failed" >&2
			output_moved=0
		else
			echo "incomplete benchmark evidence: $failed_source" >&2
			if ((output_moved != 0)); then
				# Never leave a failed post-rename directory looking published. If
				# rollback cannot claim its no-clobber target, retain or recreate the
				# inode-checked lock as a quarantine marker for manual recovery.
				if ensure_quarantine_lock; then
					release_output_lock=0
					echo "benchmark output remains quarantined by lock: $output_lock" >&2
				else
					echo "benchmark output could not be quarantined: $output_lock" >&2
					status=1
				fi
			fi
		fi
	fi
	if ((lock_owned != 0 && release_output_lock != 0)); then
		if ! release_owned_output_lock; then
			echo "could not release benchmark output lock: $output_lock" >&2
			status=1
		fi
	elif ((lock_owned != 0)); then
		status=1
	fi
	if ((runner_lock_open != 0)); then
		flock -u 9 || status=1
		exec 9>&-
		runner_lock_open=0
	fi
	# A signal deferred during publication still owns the final process status;
	# consume it only after rollback/quarantine and lock cleanup are coherent.
	if [[ -n $pending_signal ]]; then
		status=$pending_signal_status
		pending_signal=
		pending_signal_status=0
	fi
	exit "$status"
}

handle_signal() {
	local signal=$1
	local status=$2
	signal_exit_status=$status
	trap '' INT TERM HUP
	if [[ -n $active_pid ]] && active_process_alive; then
		if kill -0 "$active_pid" 2>/dev/null; then
			kill -"$signal" "$active_pid" 2>/dev/null || true
		else
			kill -"$signal" -- "-$active_pgid" 2>/dev/null || true
		fi
		if ! wait_for_active_exit 90; then
			kill -TERM -- "-$active_pgid" 2>/dev/null || true
			wait_for_active_exit 10 || true
		fi
		if active_process_alive; then
			kill -KILL -- "-$active_pgid" 2>/dev/null || true
			wait_for_active_exit 10 || true
		fi
		wait "$active_pid" 2>/dev/null || true
		active_pid=
		active_pgid=
	fi
	exit "$status"
}

dispatch_signal() {
	local signal=$1
	local status=$2
	if ((signal_critical != 0)); then
		if [[ -z $pending_signal ]]; then
			pending_signal=$signal
			pending_signal_status=$status
		fi
		return 0
	fi
	handle_signal "$signal" "$status"
}

leave_signal_critical() {
	signal_critical=0
	local signal=$pending_signal
	local status=$pending_signal_status
	pending_signal=
	pending_signal_status=0
	if [[ -n $signal ]]; then
		handle_signal "$signal" "$status"
	fi
}

run_owned() {
	local status had_errexit=0
	[[ $- == *e* ]] && had_errexit=1
	# A foreground external command would defer Bash's signal trap until it
	# returned. Run it as an owned session so TERM/HUP/INT can be forwarded now.
	signal_critical=1
	setsid "$@" 9>&- &
	active_pid=$!
	active_pgid=$active_pid
	leave_signal_critical
	set +e
	wait "$active_pid"
	status=$?
	if ((had_errexit != 0)); then
		set -e
	fi
	if kill -0 -- "-$active_pgid" 2>/dev/null; then
		echo "owned command left processes running after its leader exited: $*" >&2
		stop_active || true
		return 1
	fi
	active_pid=
	active_pgid=
	return "$status"
}

# Install signal/cleanup ownership before opening locks or creating any paths.
trap cleanup EXIT
trap 'dispatch_signal INT 130' INT
trap 'dispatch_signal TERM 143' TERM
trap 'dispatch_signal HUP 129' HUP

signal_critical=1
if ! exec 9>>"$runner_lock"; then
	leave_signal_critical
	echo "could not open dedicated benchmark runner lock: $runner_lock" >&2
	exit 1
fi
runner_lock_open=1
leave_signal_critical
# This lock is host-wide; the output lock below only protects one artifact path.
if ! flock -n 9; then
	echo "dedicated benchmark runner is already in use: $runner_lock" >&2
	exit 1
fi

set +e
ensure_directory_without_symlinks "$gate_parent"
parent_status=$?
set -e
if ((parent_status != 0)); then
	if ((parent_status == 2)); then
		usage_failure=1
	fi
	exit "$parent_status"
fi
gate_parent=$(cd "$gate_parent" && pwd -P)
if ((relative_gate_out != 0)) && [[ $gate_parent != "$repo_root" && $gate_parent != "$repo_root/"* ]]; then
	usage_failure=1
	echo "relative BENCH_GATE_OUT escapes BENCH_REPO_ROOT: $gate_parent" >&2
	exit 2
fi
gate_out=$gate_parent/$gate_leaf
output_lock=$gate_out.lock
stage=$gate_out.incomplete-$run_id
failed=$gate_out.failed-$run_id
if [[ -e $gate_out || -L $gate_out ]]; then
	usage_failure=1
	echo "BENCH_GATE_OUT already exists; refusing stale output: $gate_out" >&2
	exit 2
fi
signal_critical=1
if mkdir "$output_lock"; then
	lock_owned=1
	if ! output_lock_identity=$(stat -c '%d:%i' -- "$output_lock"); then
		leave_signal_critical
		echo "could not identify benchmark gate output lock: $output_lock" >&2
		exit 1
	fi
	leave_signal_critical
else
	acquire_status=1
	if [[ -e $output_lock || -L $output_lock ]]; then
		acquire_status=2
		usage_failure=1
	fi
	leave_signal_critical
	echo "could not acquire benchmark gate output lock: $output_lock" >&2
	exit "$acquire_status"
fi
if [[ -e $stage || -L $stage || -e $failed || -L $failed ]]; then
	usage_failure=1
	echo "benchmark gate staging path already exists for run $run_id" >&2
	exit 2
fi
signal_critical=1
if mkdir "$stage"; then
	stage_created=1
	leave_signal_critical
else
	acquire_status=1
	if [[ -e $stage || -L $stage ]]; then
		acquire_status=2
		usage_failure=1
	fi
	leave_signal_critical
	echo "could not create benchmark staging directory: $stage" >&2
	exit "$acquire_status"
fi

scratch=$(mktemp -d "$(dirname "$repo_root")/.pitaya-benchmark-gate-XXXXXX")
base_worktree=$scratch/base
candidate_worktree=$scratch/candidate
binary_dir=$scratch/bin
benchgate_binary=$binary_dir/benchgate
benchstat_binary=$binary_dir/benchstat

export LC_ALL=C
export TZ=UTC
# Remove caller-controlled inputs that can change generated code, scheduling,
# GC behavior, dependency selection, PGO, or the workload outside the policy.
export NATS_WORKLOAD=benchmark
export GOFLAGS="-mod=readonly -pgo=off"
export GOAMD64=v1
export GOEXPERIMENT=
export CGO_ENABLED=1
export GODEBUG=
export GOGC=100
export GOMEMLIMIT=off
export GOTOOLCHAIN=local
unset GOMAXPROCS

mkdir "$binary_dir"

# Build only in-repository source bytes exported from the pinned commit. The Go
# toolchain and verified module cache remain external, recorded build inputs.
# The long-lived gate checkout may contain ignored files (notably vendor), which
# Git's clean status omits.
trusted_source=$scratch/trusted-source
trusted_source_archive=$scratch/trusted-source.tar
mkdir "$trusted_source"
run_owned git -C "$gate_root" archive --format=tar --output="$trusted_source_archive" "$gate_source_sha" -- tools scripts/benchmark-policy.json
run_owned tar -xf "$trusted_source_archive" -C "$trusted_source"
rm -f -- "$trusted_source_archive"
trusted_source_archive=

# go mod verify covers the extracted module cache; explicit module/PGO flags
# keep both trusted tools and measured builds on the same selection rules.
run_owned env GOWORK=off GOFLAGS= go -C "$trusted_source/tools" mod verify
run_owned env GOWORK=off GOFLAGS= go -C "$trusted_source/tools" build -mod=readonly -pgo=off -o "$benchgate_binary" ./cmd/benchgate
run_owned env GOWORK=off GOFLAGS= go -C "$trusted_source/tools" build -mod=readonly -pgo=off -o "$benchstat_binary" golang.org/x/perf/cmd/benchstat

# Copy the commit-exported policy into owned scratch before parsing or hashing
# it, so every later command consumes the same reviewed regular file.
trusted_policy_source=$trusted_source/scripts/benchmark-policy.json
if [[ -L $trusted_policy_source || ! -f $trusted_policy_source ]]; then
	echo "trusted benchmark policy must be a regular non-symlink file: $trusted_policy_source" >&2
	exit 1
fi
reviewed_policy=$binary_dir/reviewed-policy.json
run_owned install -m 0444 -- "$trusted_policy_source" "$reviewed_policy"
rm -rf -- "$trusted_source"
trusted_source=

run_owned git -c core.hooksPath=/dev/null -C "$repo_root" worktree add --detach "$base_worktree" "$base_sha"
base_added=1
run_owned git -c core.hooksPath=/dev/null -C "$repo_root" worktree add --detach "$candidate_worktree" "$candidate_sha"
candidate_added=1
# Both checkouts must exist before either is accepted. A checkout callback must
# never get an opportunity to mutate an already verified peer worktree.
run_owned "$benchgate_binary" verify-tree -repo "$base_worktree" -commit "$base_sha"
run_owned "$benchgate_binary" verify-tree -repo "$candidate_worktree" -commit "$candidate_sha"
policy_hash_file=$binary_dir/policy.sha256
run_owned sha256sum -- "$reviewed_policy" >"$policy_hash_file"
policy_hash_output=$(<"$policy_hash_file")
rm -f -- "$policy_hash_file"
if [[ -z $policy_hash_output ]]; then
	echo "could not hash reviewed benchmark policy: $reviewed_policy" >&2
	exit 1
fi
actual_policy_digest=${policy_hash_output%% *}
if [[ ! $actual_policy_digest =~ ^[0-9a-f]{64}$ ]]; then
	echo "benchmark policy hash output is invalid: $policy_hash_output" >&2
	exit 1
fi
if [[ $actual_policy_digest != "$expected_policy_digest" ]]; then
	echo "benchmark policy digest is $actual_policy_digest, want reviewed $expected_policy_digest" >&2
	exit 1
fi

base_fingerprint_file=$binary_dir/base.fingerprint
candidate_fingerprint_file=$binary_dir/candidate.fingerprint
run_owned "$benchgate_binary" fingerprint \
	-policy "$reviewed_policy" \
	-repo "$base_worktree" \
	-pattern "$bench_pattern" \
	-benchtime "$bench_time" \
	-count "$bench_count" \
	-timeout "$bench_timeout" >"$base_fingerprint_file"
run_owned "$benchgate_binary" fingerprint \
	-policy "$reviewed_policy" \
	-repo "$candidate_worktree" \
	-pattern "$bench_pattern" \
	-benchtime "$bench_time" \
	-count "$bench_count" \
	-timeout "$bench_timeout" >"$candidate_fingerprint_file"
base_fingerprint=$(<"$base_fingerprint_file")
candidate_fingerprint=$(<"$candidate_fingerprint_file")
rm -f -- "$base_fingerprint_file" "$candidate_fingerprint_file"
if [[ $base_fingerprint != "$candidate_fingerprint" ]]; then
	echo "benchmark measurement apparatus differs:" >&2
	echo "base:      $base_fingerprint" >&2
	echo "candidate: $candidate_fingerprint" >&2
	exit 1
fi
read -r measured_policy_digest measured_apparatus_digest extra_fingerprint <<<"$base_fingerprint"
if [[ ! $measured_policy_digest =~ ^[0-9a-f]{64}$ || ! $measured_apparatus_digest =~ ^[0-9a-f]{64}$ || -n $extra_fingerprint ]]; then
	echo "benchmark fingerprint output is invalid: $base_fingerprint" >&2
	exit 1
fi
if [[ $measured_policy_digest != "$expected_policy_digest" ]]; then
	echo "benchmark policy digest is $measured_policy_digest, want reviewed $expected_policy_digest" >&2
	exit 1
fi

run_benchmark() {
	local role=$1
	local worktree=$2
	local commit=$3
	local raw=$stage/$role.raw.txt
	local result=$stage/$role.txt
	local metadata=$stage/$role.txt.meta.json
	local runner_before=$stage/$role.runner.before.json
	local runner_after=$stage/$role.runner.after.json
	local started_at finished_at deadline status

	if [[ -e $raw || -L $raw || -e $result || -L $result || -e $metadata || -L $metadata || -e $runner_before || -L $runner_before || -e $runner_after || -L $runner_after ]]; then
		echo "benchmark artifact path already exists for $role" >&2
		return 1
	fi
	if [[ $(git -C "$worktree" rev-parse --verify HEAD^{commit}) != "$commit" || -n $(git -C "$worktree" status --porcelain=v1 --untracked-files=all) ]]; then
		echo "$role benchmark worktree is not the expected clean commit" >&2
		return 1
	fi
	run_owned env GOWORK=off GOFLAGS= go -C "$worktree" mod verify
	# Bracket each measured revision's harness with deterministic runner probes.
	# The stamp step samples once more and binds the after-probe into sealed
	# metadata, closing the gap between the second probe and evidence creation.
	run_owned "$benchgate_binary" probe-runner -runner-id "$runner_id" >"$runner_before"
	started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	signal_critical=1
	# The workload is maintainer-trusted but still receives no copy of the
	# host-wide lock FD; an inherited flock could otherwise be released by a child.
	setsid env \
		GITHUB_RUN_ID="$run_id-$role" \
		NATS_WORKLOAD=benchmark \
		BENCH_PATTERN="$bench_pattern" \
		BENCH_TIME="$bench_time" \
		BENCH_COUNT="$bench_count" \
		BENCH_TIMEOUT="$bench_timeout" \
		BENCH_OUT="$raw" \
		"$worktree/scripts/test-benchmark-nats.sh" 9>&- &
	active_pid=$!
	active_pgid=$active_pid
	leave_signal_critical
	deadline=$((SECONDS + outer_timeout_seconds))
	while kill -0 "$active_pid" 2>/dev/null; do
		if ((SECONDS >= deadline)); then
			echo "$role benchmark exceeded outer timeout ${outer_timeout_seconds}s" >&2
			stop_active || true
			return 1
		fi
		sleep 1
	done
	set +e
	wait "$active_pid"
	status=$?
	set -e
	if kill -0 -- "-$active_pgid" 2>/dev/null; then
		echo "$role benchmark harness left processes running after its leader exited" >&2
		stop_active || true
		return 1
	fi
	active_pid=
	active_pgid=
	if ((status != 0)); then
		echo "$role benchmark harness exited with status $status" >&2
		return 1
	fi
	finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	run_owned "$benchgate_binary" probe-runner -runner-id "$runner_id" >"$runner_after"
	if ! run_owned cmp -s -- "$runner_before" "$runner_after"; then
		echo "$role runner state changed while the benchmark was running" >&2
		return 1
	fi
	run_owned "$benchgate_binary" stamp \
		-policy "$reviewed_policy" \
		-repo "$worktree" \
		-raw "$raw" \
		-result "$result" \
		-metadata "$metadata" \
		-role "$role" \
		-commit "$commit" \
		-run-id "$run_id" \
		-runner-id "$runner_id" \
		-runner-probe "$runner_after" \
		-gate-source-sha "$gate_source_sha" \
		-started-at "$started_at" \
		-finished-at "$finished_at"
}

# Sequential execution avoids simultaneous base/candidate contention. Fixed
# base-then-candidate order can still carry thermal/cache effects, so the runner
# must be quiescent and thermally stabilized; this gate does not erase that bias.
run_benchmark base "$base_worktree" "$base_sha"
run_benchmark candidate "$candidate_worktree" "$candidate_sha"

run_owned "$benchstat_binary" "$stage/base.raw.txt" "$stage/candidate.raw.txt" \
	>"$stage/benchstat.txt" 2>"$stage/benchstat.stderr"

set +e
run_owned "$benchgate_binary" compare \
	-base-policy "$reviewed_policy" \
	-candidate-policy "$reviewed_policy" \
	-base-repo "$base_worktree" \
	-candidate-repo "$candidate_worktree" \
	-base "$stage/base.txt" \
	-candidate "$stage/candidate.txt" \
	-base-metadata "$stage/base.txt.meta.json" \
	-candidate-metadata "$stage/candidate.txt.meta.json" \
	-base-sha "$base_sha" \
	-candidate-sha "$candidate_sha" \
	-run-id "$run_id" \
	-runner-id "$runner_id" \
	-gate-source-sha "$gate_source_sha" \
	-report "$stage/gate.json" \
	>"$stage/gate.txt" 2>"$stage/gate.stderr"
gate_status=$?
set -e
if [[ -s $stage/gate.txt ]]; then
	cat "$stage/gate.txt"
fi
if [[ -s $stage/gate.stderr ]]; then
	cat "$stage/gate.stderr" >&2
fi
# Exit 3 means the comparison completed and found a regression. Its full bundle
# is published below exactly like a passing decision.
if ((gate_status != 0 && gate_status != 3)); then
	exit "$gate_status"
fi
if [[ ! -s $stage/gate.json ]]; then
	echo "completed benchmark decision did not publish gate.json" >&2
	exit 1
fi
expected_decision=pass
if ((gate_status == 3)); then
	expected_decision=regression
fi
# Exit status and structured report are two views of one decision; require the
# trusted verifier to reconcile them before either can be published.
run_owned "$benchgate_binary" verify-report \
	-report "$stage/gate.json" \
	-policy "$reviewed_policy" \
	-base-result "$stage/base.txt" \
	-candidate-result "$stage/candidate.txt" \
	-base-metadata "$stage/base.txt.meta.json" \
	-candidate-metadata "$stage/candidate.txt.meta.json" \
	-base-sha "$base_sha" \
	-candidate-sha "$candidate_sha" \
	-run-id "$run_id" \
	-runner-id "$runner_id" \
	-gate-source-sha "$gate_source_sha" \
	-policy-sha256 "$expected_policy_digest" \
	-apparatus-sha256 "$measured_apparatus_digest" \
	-expect "$expected_decision"

if ! remove_worktrees; then
	echo "benchmark worktree cleanup failed" >&2
	exit 1
fi
if ! remove_scratch; then
	exit 1
fi
checkout_head_file=$stage/candidate-head.after
checkout_status_file=$stage/candidate-status.after
gate_head_file=$stage/gate-head.after
gate_status_file=$stage/gate-status.after
run_owned git -C "$repo_root" rev-parse --verify HEAD^{commit} >"$checkout_head_file"
run_owned git -C "$repo_root" status --porcelain=v1 --untracked-files=all >"$checkout_status_file"
run_owned git -C "$gate_root" rev-parse --verify HEAD^{commit} >"$gate_head_file"
run_owned git -C "$gate_root" status --porcelain=v1 --untracked-files=all >"$gate_status_file"
checkout_after=$(<"$checkout_head_file")
checkout_status=$(<"$checkout_status_file")
gate_source_after=$(<"$gate_head_file")
gate_status_after=$(<"$gate_status_file")
rm -f -- "$checkout_head_file" "$checkout_status_file" "$gate_head_file" "$gate_status_file"
if [[ $checkout_after != "$candidate_sha" || -n $checkout_status ]]; then
	echo "candidate checkout changed during benchmark gate" >&2
	exit 1
fi
if [[ $gate_source_after != "$gate_source_sha" || -n $gate_status_after ]]; then
	echo "trusted gate source checkout changed during benchmark gate" >&2
	exit 1
fi

# Defer handled signals through the short commit section so ownership flags
# cannot lag a completed rename or lock release. GNU mv -n supplies the
# no-replace guarantee; the source postcondition also catches implementations
# that report a skipped no-clobber move as success.
signal_critical=1
if ! output_lock_matches; then
	echo "benchmark output lock identity changed before publication: $output_lock" >&2
	exit 1
fi
if ! mv -T -n -- "$stage" "$gate_out" || [[ -e $stage || -L $stage ]]; then
	echo "could not publish benchmark gate output: $gate_out" >&2
	exit 1
fi
output_moved=1
# Keep the output lock until every other fallible ownership transition succeeds;
# cleanup can then roll back or leave a real quarantine marker.
if ! flock -u 9; then
	echo "could not release dedicated benchmark runner lock: $runner_lock" >&2
	exit 1
fi
exec 9>&-
runner_lock_open=0
if ! release_owned_output_lock; then
	echo "could not release benchmark output lock: $output_lock" >&2
	exit 1
fi
published=1
output_moved=0
leave_signal_critical
echo "benchmark gate output: $gate_out" >&2
exit "$gate_status"
