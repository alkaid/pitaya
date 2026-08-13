#!/usr/bin/env bash
set -euo pipefail

readonly environment_name=benchmark-gate

fail() {
	echo "benchmark gate controller: $*" >&2
	exit 1
}

# Normalize signals received before the controller installs full child and
# artifact ownership below. The supported boundary only exposes 0, 3, and 1.
trap 'exit 1' INT TERM HUP

usage() {
	cat <<'EOF'
Usage: scripts/run-benchmark-gate-controller.sh [--repo OWNER/REPO] [--bootstrap] CANDIDATE_SHA BASE_SHA

Run the fixed-host NATS benchmark gate for the current remote main commit and a
distinct ancestor. Both arguments must be full lowercase Git commit SHAs. The
benchmark-gate Environment is the sole source of the gate and policy pins.
Use --bootstrap once after installing or rotating either pin. Bootstrap evidence
is retained as informational and authorizes later formal runs for that pin tuple.
EOF
}

validate_repo_name() {
	local repo=$1
	[[ $repo != *$'\n'* && $repo != *$'\r'* ]] || return 1
	[[ $repo =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,38}[A-Za-z0-9])?/[A-Za-z0-9._-]{1,100}$ ]]
}

repo_from_remote() {
	local remote=$1
	local path
	case $remote in
	git@github.com:*) path=${remote#git@github.com:} ;;
	https://github.com/*) path=${remote#https://github.com/} ;;
	ssh://git@github.com/*) path=${remote#ssh://git@github.com/} ;;
	*) return 1 ;;
	esac
	path=${path%.git}
	validate_repo_name "$path" || return 1
	printf '%s\n' "$path"
}

requested_repo=
bootstrap_mode=0
while (($# > 0)); do
	case $1 in
	--repo)
		(($# >= 2)) || fail "--repo requires OWNER/REPO"
		[[ -z $requested_repo ]] || fail "--repo may only be specified once"
		requested_repo=$2
		shift 2
		;;
	--bootstrap)
		((bootstrap_mode == 0)) || fail "--bootstrap may only be specified once"
		bootstrap_mode=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	--*) fail "unknown argument: $1" ;;
	*) break ;;
	esac
done

(($# == 2)) || {
	usage >&2
	fail "candidate and base SHA are required"
}
candidate_sha=$1
base_sha=$2
[[ $candidate_sha =~ ^[0-9a-f]{40}$ ]] || fail "candidate must be a full lowercase 40-character SHA"
[[ $base_sha =~ ^[0-9a-f]{40}$ ]] || fail "base must be a full lowercase 40-character SHA"
[[ $base_sha != "$candidate_sha" ]] || fail "base and candidate must be different commits"
if [[ ${BENCH_GATE_SOURCE_SHA+x} == x || ${BENCH_POLICY_SHA256+x} == x ]]; then
	fail "benchmark pins must not be defined in the controller environment"
fi

controller_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
audit_script=$controller_root/scripts/audit-benchmark-gate-config.sh
[[ -x $audit_script ]] || fail "configuration audit is not executable: $audit_script"

git_config_variables=("${!GIT_CONFIG_@}")
for git_config_variable in "${git_config_variables[@]}"; do
	unset "$git_config_variable"
done
unset git_config_variable git_config_variables GIT_CONFIG GIT_CONFIG_PARAMETERS
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY
unset GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_INDEX_FILE GIT_NAMESPACE GIT_REPLACE_REF_BASE
unset GIT_GRAFT_FILE GIT_SHALLOW_FILE
export GIT_NO_REPLACE_OBJECTS=1

git_bin=$(command -v git) || fail "git is required"
mktemp_bin=$(command -v mktemp) || fail "mktemp is required"
mv_bin=$(command -v mv) || fail "mv is required"
stat_bin=$(command -v stat) || fail "stat is required"
jq_bin=$(command -v jq) || fail "jq is required"
cp_bin=$(command -v cp) || fail "cp is required"
setsid_bin=$(command -v setsid) || fail "setsid is required"

if ! origin=$($git_bin -C "$controller_root" config --get remote.origin.url); then
	fail "could not read remote.origin.url"
fi
if ! origin_repo=$(repo_from_remote "$origin"); then
	fail "origin must be an unambiguous github.com OWNER/REPO URL"
fi
if [[ -n $requested_repo ]]; then
	validate_repo_name "$requested_repo" || fail "--repo must be a valid OWNER/REPO"
	[[ ${requested_repo,,} == ${origin_repo,,} ]] ||
		fail "--repo $requested_repo does not match origin $origin_repo"
else
	requested_repo=$origin_repo
fi

canonical_remote=https://github.com/$requested_repo.git
read_remote_main() {
	local remote_output remote_main remote_ref remote_extra
	if ! remote_output=$(env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
		"$git_bin" -C / ls-remote --exit-code "$canonical_remote" refs/heads/main); then
		return 1
	fi
	read -r remote_main remote_ref remote_extra <<<"$remote_output"
	if [[ ! $remote_main =~ ^[0-9a-f]{40}$ || $remote_ref != refs/heads/main || -n $remote_extra ]]; then
		return 2
	fi
	printf '%s\n' "$remote_main"
}

if ! remote_main=$(read_remote_main); then
	fail "could not resolve remote refs/heads/main for $requested_repo"
fi
[[ $candidate_sha == "$remote_main" ]] ||
	fail "candidate $candidate_sha is not current remote main $remote_main"

# Fetch only the explicit identities needed by the run. The low-level gate
# separately validates clean tree provenance from detached worktrees.
if ! $git_bin -C "$controller_root" cat-file -e "$candidate_sha^{commit}" 2>/dev/null; then
	$git_bin -C "$controller_root" fetch --no-tags origin "$candidate_sha" ||
		fail "could not fetch candidate $candidate_sha"
fi
if ! $git_bin -C "$controller_root" cat-file -e "$base_sha^{commit}" 2>/dev/null; then
	$git_bin -C "$controller_root" fetch --no-tags origin "$base_sha" ||
		fail "could not fetch base $base_sha"
fi
resolved_candidate=$($git_bin -C "$controller_root" rev-parse --verify "$candidate_sha^{commit}") ||
	fail "candidate does not resolve to a commit"
resolved_base=$($git_bin -C "$controller_root" rev-parse --verify "$base_sha^{commit}") ||
	fail "base does not resolve to a commit"
[[ $resolved_candidate == "$candidate_sha" ]] || fail "candidate commit identity is ambiguous"
[[ $resolved_base == "$base_sha" ]] || fail "base commit identity is ambiguous"
checkout_head=$($git_bin -C "$controller_root" rev-parse --verify HEAD^{commit}) ||
	fail "could not resolve the candidate checkout HEAD"
[[ $checkout_head == "$candidate_sha" ]] ||
	fail "candidate checkout HEAD $checkout_head does not match $candidate_sha"
checkout_status=$($git_bin -C "$controller_root" status --porcelain=v1 --untracked-files=all) ||
	fail "could not inspect the candidate checkout"
[[ -z $checkout_status ]] || fail "candidate checkout must be clean"
$git_bin -C "$controller_root" merge-base --is-ancestor "$base_sha" "$candidate_sha" ||
	fail "base $base_sha is not an ancestor of candidate $candidate_sha"

read_pin_document() {
	local output=$1
	if ! env -u BENCH_GATE_SOURCE_SHA -u BENCH_POLICY_SHA256 \
		"$audit_script" --repo "$requested_repo" --format json >"$output"; then
		return 1
	fi
	"$jq_bin" -e --arg repo "$requested_repo" '
		type == "object" and length == 4 and
		.repo == $repo and .environment == "benchmark-gate" and
		(.gate_source_sha | type == "string" and test("^[0-9a-f]{40}$")) and
		(.policy_sha256 | type == "string" and test("^[0-9a-f]{64}$"))
	' "$output" >/dev/null
}

scratch=
gate_worktree=
pins_before=
pins_after=
gate_add_started=0
pending_output=
final_output=
output_lock=
output_lock_owned=0
output_lock_identity=
promoted=0
publication_complete=0
bootstrap_marker=
bootstrap_marker_tmp=
bootstrap_marker_installed=0
active_pid=
active_pgid=
signal_critical=0
pending_signal=

gate_worktree_registered() {
	local list_file=$scratch/worktree-list
	local line
	if ! $git_bin -C "$controller_root" worktree list --porcelain >"$list_file"; then
		return 2
	fi
	while IFS= read -r line; do
		if [[ $line == "worktree $gate_worktree" ]]; then
			return 0
		fi
	done <"$list_file"
	return 1
}

cleanup_gate_resources() {
	local registration_status=1
	local cleanup_status=0
	if ((gate_add_started != 0)); then
		if $git_bin -C "$controller_root" worktree remove --force "$gate_worktree"; then
			gate_add_started=0
		else
			if gate_worktree_registered; then
				registration_status=0
			else
				registration_status=$?
			fi
			if ((registration_status == 1)); then
				rm -rf -- "$gate_worktree"
				gate_add_started=0
			else
				echo "benchmark gate controller: retained registered or unknown gate worktree: $gate_worktree" >&2
				cleanup_status=1
			fi
		fi
	fi
	if ((gate_add_started == 0)) && [[ -d $scratch ]]; then
		if ! rm -rf -- "$scratch"; then
			echo "benchmark gate controller: could not remove scratch: $scratch" >&2
			cleanup_status=1
		fi
	fi
	return "$cleanup_status"
}

active_process_alive() {
	[[ -n $active_pid ]] && kill -0 "$active_pid" 2>/dev/null && return 0
	[[ -n $active_pgid ]] && kill -0 -- "-$active_pgid" 2>/dev/null
}

stop_active() {
	local signal=${1:-TERM}
	local deadline
	[[ -n $active_pid ]] || return 0
	if active_process_alive; then
		if kill -0 "$active_pid" 2>/dev/null; then
			kill -"$signal" "$active_pid" 2>/dev/null || true
		else
			kill -TERM -- "-$active_pgid" 2>/dev/null || true
		fi
		deadline=$((SECONDS + 120))
		while active_process_alive && ((SECONDS < deadline)); do
			sleep 1
		done
		if active_process_alive; then
			kill -KILL -- "-$active_pgid" 2>/dev/null || true
		fi
	fi
	wait "$active_pid" 2>/dev/null || true
	active_pid=
	active_pgid=
}

ensure_output_quarantine() {
	[[ -n $output_lock ]] || return 1
	if [[ -d $output_lock && ! -L $output_lock ]]; then
		return 0
	fi
	[[ ! -e $output_lock && ! -L $output_lock ]] || return 1
	mkdir "$output_lock" || return 1
	output_lock_identity=$($stat_bin -c '%d:%i' -- "$output_lock") || return 1
	output_lock_owned=1
}

cleanup() {
	local status=$?
	local failed_source=
	local release_output_lock=1
	trap '' INT TERM HUP
	stop_active TERM || status=1
	cleanup_gate_resources || status=1
	if [[ -n $bootstrap_marker_tmp ]]; then
		rm -f -- "$bootstrap_marker_tmp" || status=1
	fi
	if ((status != 0 && publication_complete == 0 && bootstrap_marker_installed != 0)); then
		rm -f -- "$bootstrap_marker" || status=1
		bootstrap_marker_installed=0
	fi
	if ((status != 0 && publication_complete == 0 && promoted != 0)) &&
		[[ -n $final_output && -d $final_output && ! -L $final_output ]]; then
		failed_source=$final_output
		failed_output=$output_parent/.failed-controller-${run_id:-unknown}-$$
	elif ((status != 0 && publication_complete == 0)) &&
		[[ -n $pending_output && -d $pending_output && ! -L $pending_output ]]; then
		failed_source=$pending_output
		failed_output=$pending_output.failed
	fi
	if [[ -n $failed_source ]]; then
		if [[ ! -e $failed_output && ! -L $failed_output ]] &&
			$mv_bin -T -n -- "$failed_source" "$failed_output" && [[ ! -e $failed_source ]]; then
			echo "benchmark gate controller: retained non-formal evidence: $failed_output" >&2
			promoted=0
		else
			echo "benchmark gate controller: incomplete evidence remains at: $failed_source" >&2
			if [[ $failed_source == "$final_output" ]] && ensure_output_quarantine; then
				release_output_lock=0
				echo "benchmark gate controller: output remains quarantined by lock: $output_lock" >&2
			fi
			status=1
		fi
	fi
	if ((status == 0 || status == 3)) && ((publication_complete == 0)); then
		status=1
	fi
	if ((output_lock_owned != 0 && release_output_lock != 0)); then
		if [[ -d $output_lock && ! -L $output_lock &&
			( -z $output_lock_identity ||
			$($stat_bin -c '%d:%i' -- "$output_lock" 2>/dev/null) == "$output_lock_identity" ) ]]; then
			rmdir "$output_lock" 2>/dev/null || status=1
		else
			status=1
		fi
	fi
	exit "$status"
}

handle_signal() {
	local signal=$1
	if ((publication_complete != 0)); then
		return 0
	fi
	trap '' INT TERM HUP
	# Bash starts asynchronous jobs with SIGINT ignored. TERM always reaches the
	# low-level gate's ordered process/Compose cleanup, including for Ctrl-C.
	stop_active TERM || true
	exit 1
}

dispatch_signal() {
	local signal=$1
	if ((publication_complete != 0)); then
		return 0
	fi
	if ((signal_critical != 0)); then
		[[ -n $pending_signal ]] || pending_signal=$signal
		return 0
	fi
	handle_signal "$signal"
}

leave_signal_critical() {
	signal_critical=0
	if [[ -n $pending_signal ]]; then
		if ((publication_complete != 0)); then
			pending_signal=
			return 0
		fi
		local signal=$pending_signal
		pending_signal=
		handle_signal "$signal"
	fi
}

trap cleanup EXIT
trap 'dispatch_signal INT' INT
trap 'dispatch_signal TERM' TERM
trap 'dispatch_signal HUP' HUP

scratch=$($mktemp_bin -d "${TMPDIR:-/tmp}/pitaya-benchmark-controller-XXXXXX") ||
	fail "could not create controller scratch directory"
gate_worktree=$scratch/gate
pins_before=$scratch/pins-before.json
pins_after=$scratch/pins-after.json

if ! read_pin_document "$pins_before"; then
	fail "could not audit $environment_name pins before execution"
fi
gate_source_sha=$($jq_bin -r '.gate_source_sha' "$pins_before")
policy_sha256=$($jq_bin -r '.policy_sha256' "$pins_before")

if ! $git_bin -C "$controller_root" cat-file -e "$gate_source_sha^{commit}" 2>/dev/null; then
	$git_bin -C "$controller_root" fetch --no-tags origin "$gate_source_sha" ||
		fail "could not fetch reviewed gate source $gate_source_sha"
fi
gate_add_started=1
$git_bin -c core.hooksPath=/dev/null -C "$controller_root" worktree add --detach "$gate_worktree" "$gate_source_sha" ||
	fail "could not create reviewed gate source worktree"
gate_script=$gate_worktree/scripts/benchmark-gate-nats.sh
raw_gate_script=$scratch/benchmark-gate-nats.sh.raw
$git_bin -C "$controller_root" cat-file blob "$gate_source_sha:scripts/benchmark-gate-nats.sh" >"$raw_gate_script" ||
	fail "could not read the reviewed gate script blob"
[[ -f $gate_script && ! -L $gate_script && -x $gate_script ]] ||
	fail "reviewed gate script checkout is not an executable regular file"
cmp -s -- "$raw_gate_script" "$gate_script" ||
	fail "reviewed gate script checkout was transformed by local Git configuration"
rm -f -- "$raw_gate_script"

build_dir=$controller_root/build
output_parent=$build_dir/benchmarks
for output_component in "$build_dir" "$output_parent"; do
	[[ ! -L $output_component ]] || fail "controller output path must not contain symlinks: $output_component"
	if [[ -e $output_component ]]; then
		[[ -d $output_component ]] || fail "controller output path component is not a directory: $output_component"
	else
		mkdir -- "$output_component" || fail "could not create output path component: $output_component"
	fi
done
unset output_component
output_parent=$(cd "$output_parent" && pwd -P) || fail "could not resolve output parent"
[[ $output_parent == "$controller_root/build/benchmarks" ]] ||
	fail "controller output parent must stay below the repository root"
bootstrap_marker=$output_parent/.benchmark-gate-bootstrap.json
if [[ -e $bootstrap_marker || -L $bootstrap_marker ]]; then
	[[ -f $bootstrap_marker && ! -L $bootstrap_marker ]] ||
		fail "bootstrap marker must be a regular file: $bootstrap_marker"
fi
if ((bootstrap_mode == 0)); then
	[[ -f $bootstrap_marker ]] && cmp -s -- "$pins_before" "$bootstrap_marker" ||
		fail "current benchmark pins are not bootstrapped; run the controller once with --bootstrap"
fi
pending_output=$output_parent/.pending-controller-${candidate_sha:0:12}-$$-$RANDOM
[[ ! -e $pending_output && ! -L $pending_output ]] ||
	fail "controller output path already exists"

runner_id=pitaya-benchmark-linux-amd64-01
runner_lock=/var/lock/pitaya/benchmark.lock

signal_critical=1
$setsid_bin env -i \
	HOME="${HOME:-}" \
	PATH="$PATH" \
	TMPDIR="${TMPDIR:-/tmp}" \
	BENCH_REPO_ROOT="$controller_root" \
	BENCH_BASE_REF="$base_sha" \
	BENCH_CANDIDATE_REF="$candidate_sha" \
	BENCH_GATE_SOURCE_SHA="$gate_source_sha" \
	BENCH_POLICY_SHA256="$policy_sha256" \
	BENCH_RUNNER_ID="$runner_id" \
	BENCH_RUNNER_LOCK="$runner_lock" \
	BENCH_GATE_OUT="$pending_output" \
	"$gate_script" &
active_pid=$!
active_pgid=$active_pid
leave_signal_critical
set +e
wait "$active_pid"
gate_status=$?
if kill -0 -- "-$active_pgid" 2>/dev/null; then
	stop_active TERM || true
	gate_status=1
else
	active_pid=
	active_pgid=
fi
set -e
if ((gate_status != 0 && gate_status != 3)); then
	fail "benchmark gate infrastructure failed with status $gate_status"
fi
[[ -d $pending_output && ! -L $pending_output && -f $pending_output/gate.json &&
	! -L $pending_output/gate.json ]] ||
	fail "completed benchmark gate did not publish pending evidence"

if ! read_pin_document "$pins_after"; then
	fail "could not audit $environment_name pins after execution"
fi
cmp -s -- "$pins_before" "$pins_after" || fail "$environment_name pin tuple changed during execution"
if ! remote_main_after=$(read_remote_main); then
	fail "could not re-resolve remote refs/heads/main for $requested_repo"
fi
[[ $remote_main_after == "$candidate_sha" ]] ||
	fail "remote main changed during benchmark execution: $candidate_sha -> $remote_main_after"

run_id=$($jq_bin -er '
	if type == "object" and (.run_id | type == "string" and test("^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"))
	then .run_id else error("invalid run_id") end
' "$pending_output/gate.json") || fail "pending gate evidence has an invalid run_id"
if ((bootstrap_mode != 0)); then
	bootstrap_marker_tmp=$output_parent/.benchmark-gate-bootstrap.json.tmp-$$-$RANDOM
	[[ ! -e $bootstrap_marker_tmp && ! -L $bootstrap_marker_tmp ]] ||
		fail "bootstrap marker staging path already exists"
	$cp_bin -- "$pins_after" "$bootstrap_marker_tmp" || fail "could not stage bootstrap marker"
	final_output=$output_parent/.bootstrap-controller-$run_id
else
	final_output=$output_parent/nats-gate-$run_id
fi
output_lock=$final_output.lock
[[ ! -e $final_output && ! -L $final_output && ! -e $output_lock && ! -L $output_lock ]] ||
	fail "benchmark controller output already exists: $final_output"

if ((bootstrap_mode == 0)); then
	[[ -f $bootstrap_marker && ! -L $bootstrap_marker ]] &&
		cmp -s -- "$pins_after" "$bootstrap_marker" ||
		fail "benchmark bootstrap marker changed during execution"
fi
if ! cleanup_gate_resources; then
	fail "could not clean controller worktree before publication"
fi

signal_critical=1
if ! mkdir "$output_lock"; then
	leave_signal_critical
	fail "could not acquire final output lock"
fi
output_lock_owned=1
if ! output_lock_identity=$($stat_bin -c '%d:%i' -- "$output_lock"); then
	leave_signal_critical
	fail "could not identify final output lock"
fi
[[ $($stat_bin -c '%d:%i' -- "$output_lock") == "$output_lock_identity" ]] ||
	fail "final output lock identity changed"
if ! $mv_bin -T -n -- "$pending_output" "$final_output" || [[ -e $pending_output ]]; then
	fail "could not promote benchmark evidence: $final_output"
fi
promoted=1
rmdir "$output_lock" || fail "could not release final output lock"
output_lock_owned=0
output_lock_identity=
if ((bootstrap_mode != 0)); then
	$mv_bin -T -- "$bootstrap_marker_tmp" "$bootstrap_marker" ||
		fail "could not install bootstrap marker"
	bootstrap_marker_tmp=
	bootstrap_marker_installed=1
fi
leave_signal_critical
publication_complete=1

if ((bootstrap_mode != 0)); then
	echo "benchmark gate controller bootstrap output: $final_output" >&2
else
	echo "benchmark gate controller output: $final_output" >&2
fi
exit "$gate_status"
