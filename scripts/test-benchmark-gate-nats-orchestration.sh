#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
gate_source=$repo_root/scripts/benchmark-gate-nats.sh
policy_source=$repo_root/scripts/benchmark-policy.json
real_setsid=$(command -v setsid)
real_rmdir=$(command -v rmdir)
real_git=$(command -v git)
reviewed_policy_digest=$(sha256sum "$policy_source")
reviewed_policy_digest=${reviewed_policy_digest%% *}
reviewed_apparatus_digest=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

fail() {
	echo "benchmark gate orchestration test: $*" >&2
	exit 1
}

assert_contains() {
	local file=$1
	local expected=$2
	if ! grep -Fq -- "$expected" "$file"; then
		echo "expected $file to contain: $expected" >&2
		sed -n '1,160p' "$file" >&2 || true
		return 1
	fi
}

run_expect_status() {
	local expected=$1
	local name=$2
	local stdout=$3
	local stderr=$4
	local status
	shift 4
	set +e
	"$@" >"$stdout" 2>"$stderr"
	status=$?
	set -e
	if ((status != expected)); then
		echo "$name exited with status $status, want $expected" >&2
		sed -n '1,160p' "$stderr" >&2 || true
		return 1
	fi
}

wait_for_file() {
	local file=$1
	local attempts=${2:-1000}
	local index
	for ((index = 0; index < attempts; index++)); do
		if [[ -e $file ]]; then
			return 0
		fi
		sleep 0.01
	done
	return 1
}

[[ -x $gate_source ]] || fail "missing executable production gate: $gate_source"
[[ -f $policy_source ]] || fail "missing benchmark policy: $policy_source"
[[ $real_setsid == /* ]] || fail "could not resolve the real setsid binary"
[[ $real_rmdir == /* ]] || fail "could not resolve the real rmdir binary"
[[ $real_git == /* ]] || fail "could not resolve the real git binary"
[[ $reviewed_policy_digest =~ ^[0-9a-f]{64}$ ]] || fail "could not hash the benchmark policy"

test_root=$(mktemp -d)
gate_pid=
watchdog_pid=
paused_rmdir_pid=
busy_fd_open=0
cleanup() {
	set +e
	if [[ -n $watchdog_pid ]] && kill -0 "$watchdog_pid" 2>/dev/null; then
		kill -TERM "$watchdog_pid" 2>/dev/null
		wait "$watchdog_pid" 2>/dev/null
	fi
	if [[ -n $gate_pid ]] && kill -0 "$gate_pid" 2>/dev/null; then
		kill -TERM "$gate_pid" 2>/dev/null
		wait "$gate_pid" 2>/dev/null
	fi
	if [[ -n $paused_rmdir_pid ]] && kill -0 "$paused_rmdir_pid" 2>/dev/null; then
		kill -KILL "$paused_rmdir_pid" 2>/dev/null
	fi
	if ((busy_fd_open != 0)); then
		flock -u "$busy_fd" 2>/dev/null
		exec {busy_fd}>&-
	fi
	rm -rf -- "${test_root:?}"
}
trap cleanup EXIT

fixture=$test_root/repo
fake_bin=$test_root/fake-bin
templates=$test_root/templates
logs=$test_root/logs
mkdir -p "$fixture/scripts" "$fixture/tools" "$fake_bin" "$templates" "$logs"

cat >"$fixture/scripts/test-benchmark-nats.sh" <<'HARNESS'
#!/usr/bin/env bash
set -euo pipefail
: "${BENCH_OUT:?BENCH_OUT is required}"
[[ ${GOFLAGS:-} == '-mod=readonly -pgo=off' ]] || { echo "synthetic harness: unexpected GOFLAGS: ${GOFLAGS:-<unset>}" >&2; exit 90; }
[[ ! -e /proc/self/fd/9 ]] || { echo 'synthetic harness: inherited runner lock FD 9' >&2; exit 90; }
if [[ -n ${FAKE_HARNESS_MARKER:-} ]]; then
	: >"$FAKE_HARNESS_MARKER"
fi
if [[ ${FAKE_HARNESS_STATUS:-0} != 0 ]]; then
	exit "$FAKE_HARNESS_STATUS"
fi
printf '%s\n' \
	'goos: linux' \
	'goarch: amd64' \
	'pkg: example.com/bench' \
	'cpu: Synthetic CPU' \
	'' \
	'BenchmarkSynthetic-8 1 10 ns/op 8 B/op 1 allocs/op' \
	'PASS' >"$BENCH_OUT"
HARNESS
chmod +x "$fixture/scripts/test-benchmark-nats.sh"

cat >"$templates/benchgate" <<'BENCHGATE'
#!/usr/bin/env bash
set -euo pipefail

argument_value() {
	local key=$1
	shift
	while (($# > 0)); do
		if [[ $1 == "$key" && $# -ge 2 ]]; then
			printf '%s\n' "$2"
			return 0
		fi
		shift
	done
	return 1
}

command=${1:-}
if (($# > 0)); then
	shift
fi
case $command in
verify-tree)
	repo=$(argument_value -repo "$@")
	commit=$(argument_value -commit "$@")
	[[ $(git -C "$repo" rev-parse --verify HEAD^{commit}) == "$commit" ]] || { echo 'synthetic verify-tree: checkout identity mismatch' >&2; exit 90; }
	expected=$(git -C "$repo" rev-parse "$commit:revision.txt")
	actual=$(git -C "$repo" hash-object --no-filters -- revision.txt)
	[[ $actual == "$expected" ]] || { echo 'synthetic verify-tree: raw blob mismatch' >&2; exit 90; }
	echo "checkout tree verified: $commit"
	;;
fingerprint)
	policy=$(argument_value -policy "$@")
	repo=$(argument_value -repo "$@")
	pattern=$(argument_value -pattern "$@")
	benchtime=$(argument_value -benchtime "$@")
	count=$(argument_value -count "$@")
	timeout=$(argument_value -timeout "$@")
	if [[ -n ${FAKE_FINGERPRINT_LOG:-} ]]; then
		printf '%s|%s|%s|%s|%s|%s\n' "$policy" "$repo" "$pattern" "$benchtime" "$count" "$timeout" >>"$FAKE_FINGERPRINT_LOG"
	fi
	if [[ ${FAKE_DIFFERENT_CANDIDATE_FINGERPRINT:-0} == 1 && $repo == */candidate ]]; then
		echo "$FAKE_POLICY_DIGEST cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		exit 0
	fi
	echo "$FAKE_POLICY_DIGEST $FAKE_APPARATUS_DIGEST"
	;;
probe-runner)
	runner_id=$(argument_value -runner-id "$@")
	probe_state=stable
	if [[ -n ${FAKE_PROBE_STATE:-} ]]; then
		probe_count=0
		if [[ -s $FAKE_PROBE_STATE ]]; then
			read -r probe_count <"$FAKE_PROBE_STATE"
		fi
		probe_count=$((probe_count + 1))
		printf '%s\n' "$probe_count" >"$FAKE_PROBE_STATE"
		if [[ $probe_count == "${FAKE_RUNNER_DRIFT_AT:-0}" ]]; then
			probe_state=drifted
		fi
	fi
	printf '{"runner_id":"%s","state":"%s"}\n' "$runner_id" "$probe_state"
	;;
stamp)
	raw=$(argument_value -raw "$@")
	result=$(argument_value -result "$@")
	metadata=$(argument_value -metadata "$@")
	runner_id=$(argument_value -runner-id "$@")
	runner_probe=$(argument_value -runner-probe "$@")
	gate_source_sha=$(argument_value -gate-source-sha "$@")
	[[ -s $runner_probe ]] || { echo 'synthetic stamp: missing runner probe' >&2; exit 90; }
	grep -Fq -- "\"runner_id\":\"$runner_id\"" "$runner_probe" || {
		echo 'synthetic stamp: runner probe identity mismatch' >&2
		exit 90
	}
	cp "$raw" "$result"
	printf '{"synthetic":true,"gate_source_sha":"%s"}\n' "$gate_source_sha" >"$metadata"
	;;
compare)
	report=$(argument_value -report "$@")
	base_result=$(argument_value -base "$@")
	candidate_result=$(argument_value -candidate "$@")
	base_metadata=$(argument_value -base-metadata "$@")
	candidate_metadata=$(argument_value -candidate-metadata "$@")
	base_sha=$(argument_value -base-sha "$@")
	candidate_sha=$(argument_value -candidate-sha "$@")
	gate_source_sha=$(argument_value -gate-source-sha "$@")
	run_id=$(argument_value -run-id "$@")
	runner_id=$(argument_value -runner-id "$@")
	compare_status=${FAKE_COMPARE_STATUS:-3}
	passed=false
	regression_count=1
	if ((compare_status == 0)); then
		passed=true
		regression_count=0
	fi
	if [[ -n ${FAKE_REPORT_PASSED:-} ]]; then
		passed=$FAKE_REPORT_PASSED
		if [[ $passed == true ]]; then
			regression_count=0
		else
			regression_count=1
		fi
	fi
	if [[ ${FAKE_SKIP_REPORT:-0} != 1 ]]; then
		printf '{"schema_version":2,"base_sha":"%s","candidate_sha":"%s","gate_source_sha":"%s","run_id":"%s","runner_id":"%s","base_result_sha256":"%s","candidate_result_sha256":"%s","base_metadata_sha256":"%s","candidate_metadata_sha256":"%s","passed":%s,"regression_count":%d}\n' \
			"$base_sha" "$candidate_sha" "$gate_source_sha" "$run_id" "$runner_id" \
			"$(sha256sum "$base_result" | cut -d' ' -f1)" "$(sha256sum "$candidate_result" | cut -d' ' -f1)" \
			"$(sha256sum "$base_metadata" | cut -d' ' -f1)" "$(sha256sum "$candidate_metadata" | cut -d' ' -f1)" \
			"$passed" "$regression_count" >"$report"
	fi
	echo 'synthetic comparison report'
	if ((compare_status == 3)); then
		echo 'benchmark regression' >&2
	fi
	if [[ ${FAKE_LEAVE_BINARY:-0} == 1 ]]; then
		printf 'force scratch rmdir failure\n' >"$(dirname "$0")/leftover"
	fi
	if [[ -n ${FAKE_COMPARE_READY:-} ]]; then
		: >"$FAKE_COMPARE_READY"
	fi
	if [[ -n ${FAKE_COMPARE_PID_FILE:-} ]]; then
		printf '%s\n' "$$" >"$FAKE_COMPARE_PID_FILE"
	fi
	if [[ -n ${FAKE_COMPARE_RELEASE:-} ]]; then
		for ((index = 0; index < 3000; index++)); do
			if [[ -e $FAKE_COMPARE_RELEASE ]]; then
				exit "$compare_status"
			fi
			sleep 0.01
		done
		echo 'timed out waiting for synthetic compare release' >&2
		exit 90
	fi
	exit "$compare_status"
	;;
verify-report)
	report=$(argument_value -report "$@")
	policy=$(argument_value -policy "$@")
	base_result=$(argument_value -base-result "$@")
	candidate_result=$(argument_value -candidate-result "$@")
	base_metadata=$(argument_value -base-metadata "$@")
	candidate_metadata=$(argument_value -candidate-metadata "$@")
	base_sha=$(argument_value -base-sha "$@")
	candidate_sha=$(argument_value -candidate-sha "$@")
	gate_source_sha=$(argument_value -gate-source-sha "$@")
	run_id=$(argument_value -run-id "$@")
	runner_id=$(argument_value -runner-id "$@")
	policy_sha256=$(argument_value -policy-sha256 "$@")
	apparatus_sha256=$(argument_value -apparatus-sha256 "$@")
	expect=$(argument_value -expect "$@")
	[[ $policy_sha256 == "$FAKE_POLICY_DIGEST" ]] || { echo 'synthetic verify-report: policy digest mismatch' >&2; exit 90; }
	[[ $apparatus_sha256 == "$FAKE_APPARATUS_DIGEST" ]] || { echo 'synthetic verify-report: apparatus digest mismatch' >&2; exit 90; }
	[[ $(sha256sum "$policy" | cut -d' ' -f1) == "$policy_sha256" ]] || { echo 'synthetic verify-report: policy file mismatch' >&2; exit 90; }
	case $expect in
	pass)
		passed=true
		regression_count=0
		;;
	regression)
		passed=false
		regression_count=1
		;;
	*)
		echo "synthetic verify-report: invalid expectation $expect" >&2
		exit 90
		;;
	esac
	expected=$(printf '{"schema_version":2,"base_sha":"%s","candidate_sha":"%s","gate_source_sha":"%s","run_id":"%s","runner_id":"%s","base_result_sha256":"%s","candidate_result_sha256":"%s","base_metadata_sha256":"%s","candidate_metadata_sha256":"%s","passed":%s,"regression_count":%d}' \
		"$base_sha" "$candidate_sha" "$gate_source_sha" "$run_id" "$runner_id" \
		"$(sha256sum "$base_result" | cut -d' ' -f1)" "$(sha256sum "$candidate_result" | cut -d' ' -f1)" \
		"$(sha256sum "$base_metadata" | cut -d' ' -f1)" "$(sha256sum "$candidate_metadata" | cut -d' ' -f1)" \
		"$passed" "$regression_count")
	if [[ ! -f $report || $(<"$report") != "$expected" ]]; then
		echo 'synthetic verify-report: report identity or outcome mismatch' >&2
		exit 1
	fi
	echo "gate report verified: $expect"
	;;
*)
	echo "unexpected synthetic benchgate command: $command" >&2
	exit 90
	;;
esac
BENCHGATE

cat >"$templates/benchstat" <<'BENCHSTAT'
#!/usr/bin/env bash
set -euo pipefail
echo 'synthetic benchstat report'
BENCHSTAT
chmod +x "$templates/benchgate" "$templates/benchstat"

cat >"$fake_bin/go" <<'FAKEGO'
#!/usr/bin/env bash
set -euo pipefail
arguments=("$@")
for ((index = 0; index + 1 < ${#arguments[@]}; index++)); do
	if [[ ${arguments[index]} == -C && -n ${FAKE_FORBIDDEN_GO_ROOT:-} && ${arguments[index + 1]} == "$FAKE_FORBIDDEN_GO_ROOT" ]]; then
		echo "synthetic go: build used mutable trusted checkout $FAKE_FORBIDDEN_GO_ROOT" >&2
		exit 90
	fi
done
if [[ -n ${FAKE_GO_LOG:-} ]]; then
	printf '%s\n' "$*" >>"$FAKE_GO_LOG"
fi
previous=
for argument in "$@"; do
	if [[ $previous == mod && $argument == verify ]]; then
		exit "${FAKE_GO_MOD_VERIFY_STATUS:-0}"
	fi
	previous=$argument
done
output=
target=
while (($# > 0)); do
	case $1 in
	-o)
		shift
		output=${1:-}
		;;
	*)
		target=$1
		;;
	esac
	shift
done
[[ -n $output ]] || { echo 'synthetic go: missing -o' >&2; exit 90; }
case $target in
./cmd/benchgate)
	install -m 0755 "$FAKE_BENCHGATE_TEMPLATE" "$output"
	;;
golang.org/x/perf/cmd/benchstat)
	install -m 0755 "$FAKE_BENCHSTAT_TEMPLATE" "$output"
	;;
*)
	echo "synthetic go: unexpected build target $target" >&2
	exit 90
	;;
esac
FAKEGO
chmod +x "$fake_bin/go"

cat >"$fake_bin/setsid" <<'FAKESETSID'
#!/usr/bin/env bash
set -euo pipefail
: "${REAL_SETSID:?REAL_SETSID is required}"
if [[ ${FAKE_SPAWN_SIGNAL_PARENT:-0} == 1 ]]; then
	: "${FAKE_SETSID_PID_FILE:?FAKE_SETSID_PID_FILE is required}"
	printf '%s\n' "$$" >"$FAKE_SETSID_PID_FILE"
	kill -TERM "$PPID"
	sleep 5
fi
exec "$REAL_SETSID" "$@"
FAKESETSID
chmod +x "$fake_bin/setsid"

cat >"$fake_bin/rmdir" <<'FAKERMDIR'
#!/usr/bin/env bash
set -euo pipefail
: "${REAL_RMDIR:?REAL_RMDIR is required}"
if [[ -n ${FAKE_RMDIR_PAUSE_PATH:-} && $# == 1 && $1 == "$FAKE_RMDIR_PAUSE_PATH" ]]; then
	: "${FAKE_RMDIR_READY:?FAKE_RMDIR_READY is required}"
	: "${FAKE_RMDIR_PID_FILE:?FAKE_RMDIR_PID_FILE is required}"
	printf '%s\n' "$$" >"$FAKE_RMDIR_PID_FILE"
	: >"$FAKE_RMDIR_READY"
	for ((index = 0; index < 3000; index++)); do
		[[ -n ${FAKE_RMDIR_RELEASE:-} && -e $FAKE_RMDIR_RELEASE ]] && break
		sleep 0.01
	done
fi
if [[ -n ${FAKE_RMDIR_FAIL_ONCE_PATH:-} && $# == 1 && $1 == "$FAKE_RMDIR_FAIL_ONCE_PATH" ]]; then
	: "${FAKE_RMDIR_STATE:?FAKE_RMDIR_STATE is required}"
	if [[ ! -e $FAKE_RMDIR_STATE ]]; then
		: >"$FAKE_RMDIR_STATE"
		if [[ ${FAKE_RMDIR_SIGNAL_PARENT:-0} == 1 ]]; then
			kill -TERM "$PPID"
		fi
		if [[ ${FAKE_RMDIR_CREATE_ROLLBACK_COLLISION:-0} == 1 ]]; then
			published=${FAKE_RMDIR_FAIL_ONCE_PATH%.lock}
			report=$published/gate.json
			[[ -s $report ]] || { echo 'synthetic rmdir: missing published report' >&2; exit 90; }
			run_id=$(sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p' "$report")
			[[ -n $run_id ]] || { echo 'synthetic rmdir: missing run ID' >&2; exit 90; }
			mkdir "$published.failed-$run_id"
		fi
		exit 1
	fi
fi
exec "$REAL_RMDIR" "$@"
FAKERMDIR
chmod +x "$fake_bin/rmdir"

cat >"$fake_bin/git" <<'FAKEGIT'
#!/usr/bin/env bash
set -euo pipefail
: "${REAL_GIT:?REAL_GIT is required}"
if [[ ${FAKE_GIT_REQUIRE_SANITIZED_ENV:-0} == 1 ]]; then
	[[ -z ${GIT_CONFIG+x} ]] || { echo 'synthetic git: inherited GIT_CONFIG' >&2; exit 90; }
	[[ -z ${GIT_GRAFT_FILE+x} ]] || { echo 'synthetic git: inherited GIT_GRAFT_FILE' >&2; exit 90; }
	[[ -z ${GIT_SHALLOW_FILE+x} ]] || { echo 'synthetic git: inherited GIT_SHALLOW_FILE' >&2; exit 90; }
	[[ -z ${GIT_ATTR_GLOBAL+x} && -z ${GIT_ATTR_SYSTEM+x} && -z ${GIT_ATTR_SOURCE+x} ]] || { echo 'synthetic git: inherited Git attribute override' >&2; exit 90; }
	[[ ${GIT_CONFIG_GLOBAL:-} == /dev/null && ${GIT_CONFIG_NOSYSTEM:-} == 1 && ${GIT_ATTR_NOSYSTEM:-} == 1 ]] || { echo 'synthetic git: deterministic Git config controls missing' >&2; exit 90; }
	[[ ${GIT_CONFIG_COUNT:-} == 4 && ${GIT_CONFIG_KEY_3:-} == core.attributesFile && ${GIT_CONFIG_VALUE_3:-} == /dev/null ]] || { echo 'synthetic git: user attributes are not disabled' >&2; exit 90; }
fi
if [[ ${FAKE_GIT_FAIL_WORKTREE_LIST:-0} == 1 && $# -ge 4 && $1 == -C && $3 == worktree && $4 == list ]]; then
	echo 'synthetic git: worktree list failed' >&2
	exit 90
fi
exec "$REAL_GIT" "$@"
FAKEGIT
chmod +x "$fake_bin/git"

cat >"$fake_bin/git-smudge" <<'FAKESMUDGE'
#!/usr/bin/env bash
set -euo pipefail
sed 's/^candidate$/altered/'
FAKESMUDGE
cat >"$fake_bin/git-clean" <<'FAKECLEAN'
#!/usr/bin/env bash
set -euo pipefail
sed 's/^altered$/candidate/'
FAKECLEAN
chmod +x "$fake_bin/git-smudge" "$fake_bin/git-clean"

printf 'base\n' >"$fixture/revision.txt"
printf 'revision.txt filter=synthetic\n' >"$fixture/.gitattributes"
mkdir "$test_root/escaped-output"
ln -s "$test_root/escaped-output" "$fixture/output-link"
git -C "$fixture" init -q
git -C "$fixture" config user.name 'Benchmark Gate Test'
git -C "$fixture" config user.email 'benchmark-gate-test@example.invalid'
git -C "$fixture" config commit.gpgsign false
git -C "$fixture" add .
git -C "$fixture" commit -q -m base
base_fixture_sha=$(git -C "$fixture" rev-parse --verify HEAD^{commit})
# Neither measured revision owns the blocking verifier or policy.
printf 'candidate\n' >"$fixture/revision.txt"
git -C "$fixture" add revision.txt
git -C "$fixture" commit -q -m candidate
candidate_fixture_sha=$(git -C "$fixture" rev-parse --verify HEAD^{commit})
base_fixture_tree=$(git -C "$fixture" rev-parse --verify "$base_fixture_sha^{tree}")
unrelated_fixture_sha=$(git -C "$fixture" commit-tree "$base_fixture_tree" -m unrelated)
if git -C "$fixture" cat-file -e "$base_fixture_sha:scripts/benchmark-gate-nats.sh" 2>/dev/null ||
	git -C "$fixture" cat-file -e "$base_fixture_sha:scripts/benchmark-policy.json" 2>/dev/null; then
	fail 'base fixture unexpectedly contains the trusted gate or policy'
fi

trusted_gate=$test_root/trusted-gate
mkdir -p "$trusted_gate/scripts" "$trusted_gate/tools"
cp "$gate_source" "$trusted_gate/scripts/benchmark-gate-nats.sh"
cp "$policy_source" "$trusted_gate/scripts/benchmark-policy.json"
chmod +x "$trusted_gate/scripts/benchmark-gate-nats.sh"
: >"$trusted_gate/tools/.keep"
printf 'tools/vendor/\n' >"$trusted_gate/.gitignore"
git -C "$trusted_gate" init -q
git -C "$trusted_gate" config user.name 'Benchmark Gate Test'
git -C "$trusted_gate" config user.email 'benchmark-gate-test@example.invalid'
git -C "$trusted_gate" config commit.gpgsign false
git -C "$trusted_gate" add .
git -C "$trusted_gate" commit -q -m trusted-gate
gate=$trusted_gate/scripts/benchmark-gate-nats.sh
gate_source_sha=$(git -C "$trusted_gate" rev-parse --verify HEAD^{commit})
mkdir -p "$trusted_gate/tools/vendor/example.invalid/ignored"
printf 'ignored bytes must never enter the trusted build\n' >"$trusted_gate/tools/vendor/example.invalid/ignored/ignored.go"
common_env=(
	env
	-u BENCH_REPO_ROOT
	-u BENCH_BASE_REF
	-u BENCH_CANDIDATE_REF
	-u BENCH_RUNNER_ID
	-u BENCH_RUNNER_LOCK
	-u BENCH_POLICY_SHA256
	-u BENCH_GATE_SOURCE_SHA
	-u BENCH_GATE_OUT
	-u GITHUB_RUN_ID
	-u GITHUB_RUN_ATTEMPT
	-u FAKE_COMPARE_READY
	-u FAKE_COMPARE_RELEASE
	-u FAKE_COMPARE_STATUS
	-u FAKE_COMPARE_PID_FILE
	-u FAKE_SKIP_REPORT
	-u FAKE_REPORT_PASSED
	-u FAKE_LEAVE_BINARY
	-u FAKE_HARNESS_STATUS
	-u FAKE_HARNESS_MARKER
	-u FAKE_PROBE_STATE
	-u FAKE_RUNNER_DRIFT_AT
	-u FAKE_FINGERPRINT_LOG
	-u FAKE_DIFFERENT_CANDIDATE_FINGERPRINT
	-u FAKE_SPAWN_SIGNAL_PARENT
	-u FAKE_SETSID_PID_FILE
	-u FAKE_RMDIR_FAIL_ONCE_PATH
	-u FAKE_RMDIR_STATE
	-u FAKE_RMDIR_CREATE_ROLLBACK_COLLISION
	-u FAKE_RMDIR_SIGNAL_PARENT
	-u FAKE_RMDIR_PAUSE_PATH
	-u FAKE_RMDIR_READY
	-u FAKE_RMDIR_PID_FILE
	-u FAKE_RMDIR_RELEASE
	-u FAKE_GIT_FAIL_WORKTREE_LIST
	-u FAKE_GIT_REQUIRE_SANITIZED_ENV
	-u FAKE_GO_LOG
	-u FAKE_GO_MOD_VERIFY_STATUS
	-u FAKE_FORBIDDEN_GO_ROOT
	PATH="$fake_bin:$PATH"
	REAL_SETSID="$real_setsid"
	REAL_RMDIR="$real_rmdir"
	REAL_GIT="$real_git"
	BENCH_REPO_ROOT="$fixture"
	BENCH_POLICY_SHA256="$reviewed_policy_digest"
	BENCH_GATE_SOURCE_SHA="$gate_source_sha"
	FAKE_BENCHGATE_TEMPLATE="$templates/benchgate"
	FAKE_BENCHSTAT_TEMPLATE="$templates/benchstat"
	FAKE_POLICY_DIGEST="$reviewed_policy_digest"
	FAKE_APPARATUS_DIGEST="$reviewed_apparatus_digest"
	FAKE_FORBIDDEN_GO_ROOT="$trusted_gate/tools"
)
shopt -s nullglob

run_expect_status 2 'invalid BENCH_REPO_ROOT' "$logs/invalid-repo-root.out" "$logs/invalid-repo-root.err" \
	"${common_env[@]}" BENCH_REPO_ROOT="$test_root/missing-repository" "$gate"
assert_contains "$logs/invalid-repo-root.err" 'BENCH_REPO_ROOT must name an accessible Git checkout'

run_expect_status 2 'missing BENCH_RUNNER_ID' "$logs/missing-id.out" "$logs/missing-id.err" \
	"${common_env[@]}" "$gate"
assert_contains "$logs/missing-id.err" 'BENCH_RUNNER_ID must identify a dedicated fixed runner'

run_expect_status 2 'invalid BENCH_RUNNER_ID' "$logs/invalid-id.out" "$logs/invalid-id.err" \
	"${common_env[@]}" BENCH_RUNNER_ID='invalid/id' "$gate"
assert_contains "$logs/invalid-id.err" 'BENCH_RUNNER_ID must identify a dedicated fixed runner'

run_expect_status 2 'missing BENCH_RUNNER_LOCK' "$logs/missing-lock.out" "$logs/missing-lock.err" \
	"${common_env[@]}" BENCH_RUNNER_ID=fixed-runner "$gate"
assert_contains "$logs/missing-lock.err" 'BENCH_RUNNER_LOCK must be an absolute host-wide lock path'

run_expect_status 2 'relative BENCH_RUNNER_LOCK' "$logs/relative-lock.out" "$logs/relative-lock.err" \
	"${common_env[@]}" BENCH_RUNNER_ID=fixed-runner BENCH_RUNNER_LOCK=relative.lock "$gate"
assert_contains "$logs/relative-lock.err" 'BENCH_RUNNER_LOCK must be an absolute host-wide lock path'

run_expect_status 2 'missing BENCH_POLICY_SHA256' "$logs/missing-policy-digest.out" "$logs/missing-policy-digest.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/missing-policy-digest.lock" \
	BENCH_POLICY_SHA256= \
	"$gate"
assert_contains "$logs/missing-policy-digest.err" 'BENCH_POLICY_SHA256 must be the reviewed 64-character lowercase policy digest'

run_expect_status 2 'invalid BENCH_POLICY_SHA256' "$logs/invalid-policy-digest.out" "$logs/invalid-policy-digest.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/invalid-policy-digest.lock" \
	BENCH_POLICY_SHA256=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA \
	"$gate"
assert_contains "$logs/invalid-policy-digest.err" 'BENCH_POLICY_SHA256 must be the reviewed 64-character lowercase policy digest'

run_expect_status 2 'missing BENCH_GATE_SOURCE_SHA' "$logs/missing-gate-source.out" "$logs/missing-gate-source.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/missing-gate-source.lock" \
	BENCH_GATE_SOURCE_SHA= \
	"$gate"
assert_contains "$logs/missing-gate-source.err" 'BENCH_GATE_SOURCE_SHA must be the reviewed full gate checkout commit'

run_expect_status 2 'invalid BENCH_GATE_SOURCE_SHA' "$logs/invalid-gate-source.out" "$logs/invalid-gate-source.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/invalid-gate-source.lock" \
	BENCH_GATE_SOURCE_SHA=not-a-full-commit \
	"$gate"
assert_contains "$logs/invalid-gate-source.err" 'BENCH_GATE_SOURCE_SHA must be the reviewed full gate checkout commit'

run_expect_status 2 'mismatched BENCH_GATE_SOURCE_SHA' "$logs/mismatched-gate-source.out" "$logs/mismatched-gate-source.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/mismatched-gate-source.lock" \
	BENCH_GATE_SOURCE_SHA=0000000000000000000000000000000000000000 \
	"$gate"
assert_contains "$logs/mismatched-gate-source.err" 'trusted gate source is'

run_expect_status 2 'invalid BENCH_BASE_REF' "$logs/invalid-base.out" "$logs/invalid-base.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/invalid-base-runner.lock" \
	BENCH_BASE_REF=missing-base-ref \
	"$gate"
assert_contains "$logs/invalid-base.err" 'BENCH_BASE_REF does not resolve to a commit'

run_expect_status 2 'invalid BENCH_CANDIDATE_REF' "$logs/invalid-candidate.out" "$logs/invalid-candidate.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/invalid-candidate-runner.lock" \
	BENCH_CANDIDATE_REF=missing-candidate-ref \
	"$gate"
assert_contains "$logs/invalid-candidate.err" 'BENCH_CANDIDATE_REF does not resolve to a commit'

: >"$test_root/lock-target"
ln -s "$test_root/lock-target" "$test_root/symlink.lock"
run_expect_status 2 'symlink BENCH_RUNNER_LOCK' "$logs/symlink-lock.out" "$logs/symlink-lock.err" \
	"${common_env[@]}" BENCH_RUNNER_ID=fixed-runner BENCH_RUNNER_LOCK="$test_root/symlink.lock" "$gate"
assert_contains "$logs/symlink-lock.err" 'BENCH_RUNNER_LOCK must name a regular file, not a symlink'

stale_out=$test_root/stale-output
mkdir "$stale_out"
run_expect_status 2 'stale BENCH_GATE_OUT' "$logs/stale.out" "$logs/stale.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/stale-runner.lock" \
	BENCH_GATE_OUT="$stale_out" \
	"$gate"
assert_contains "$logs/stale.err" 'BENCH_GATE_OUT already exists; refusing stale output'

run_expect_status 2 'newline BENCH_GATE_OUT' "$logs/newline-output.out" "$logs/newline-output.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/newline-output-runner.lock" \
	BENCH_GATE_OUT=$'results/newline\noutput' \
	"$gate"
assert_contains "$logs/newline-output.err" 'BENCH_GATE_OUT must be a single-line path'

run_expect_status 2 'carriage-return BENCH_GATE_OUT' "$logs/carriage-output.out" "$logs/carriage-output.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/carriage-output-runner.lock" \
	BENCH_GATE_OUT=$'results/carriage\routput' \
	"$gate"
assert_contains "$logs/carriage-output.err" 'BENCH_GATE_OUT must be a single-line path'

run_expect_status 2 'symlink output parent' "$logs/output-symlink.out" "$logs/output-symlink.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/output-symlink-runner.lock" \
	BENCH_GATE_OUT=output-link/formal-result \
	"$gate"
assert_contains "$logs/output-symlink.err" 'benchmark output parent contains a symlink'
[[ ! -e $test_root/escaped-output/formal-result ]] || fail 'symlink output parent escaped the candidate checkout'

replace_out=$test_root/results/replace-object
"$real_git" -C "$fixture" replace "$candidate_fixture_sha" "$base_fixture_sha"
run_expect_status 0 'replacement objects are disabled' "$logs/replace-object.out" "$logs/replace-object.err" \
	"${common_env[@]}" \
	GIT_NO_REPLACE_OBJECTS=0 \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/replace-object-runner.lock" \
	BENCH_GATE_OUT="$replace_out" \
	FAKE_COMPARE_STATUS=0 \
	"$gate"
"$real_git" -C "$fixture" replace -d "$candidate_fixture_sha" >/dev/null
[[ -s $replace_out/gate.json ]] || fail 'replacement-object defense did not publish a passing report'

printf '%s %s\n' "$candidate_fixture_sha" "$base_fixture_sha" >"$fixture/.git/info/grafts"
run_expect_status 2 'legacy Git grafts are rejected' "$logs/git-grafts.out" "$logs/git-grafts.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/git-grafts-runner.lock" \
	BENCH_GATE_OUT="$test_root/results/git-grafts" \
	"$gate"
assert_contains "$logs/git-grafts.err" 'candidate checkout uses unsupported legacy Git grafts'
rm -f -- "$fixture/.git/info/grafts"

printf 'revision.txt export-ignore\n' >"$fixture/.git/info/attributes"
run_expect_status 2 'private Git attributes are rejected' "$logs/git-attributes.out" "$logs/git-attributes.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/git-attributes-runner.lock" \
	BENCH_GATE_OUT="$test_root/results/git-attributes" \
	"$gate"
assert_contains "$logs/git-attributes.err" 'candidate checkout uses unsupported private Git attributes'
rm -f -- "$fixture/.git/info/attributes"

git_xdg=$test_root/git-xdg
mkdir -p "$git_xdg/git"
printf 'scripts/benchmark-policy.json export-ignore\n' >"$git_xdg/git/attributes"
filter_out=$test_root/results/config-filter
run_expect_status 0 'inherited Git filter config is cleared' "$logs/config-filter.out" "$logs/config-filter.err" \
	"${common_env[@]}" \
	XDG_CONFIG_HOME="$git_xdg" \
	GIT_CONFIG_COUNT=2 \
	GIT_CONFIG_KEY_0=filter.synthetic.smudge \
	GIT_CONFIG_VALUE_0="$fake_bin/git-smudge" \
	GIT_CONFIG_KEY_1=filter.synthetic.clean \
	GIT_CONFIG_VALUE_1="$fake_bin/git-clean" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/config-filter-runner.lock" \
	BENCH_GATE_OUT="$filter_out" \
	FAKE_COMPARE_STATUS=0 \
	"$gate"
[[ -s $filter_out/gate.json ]] || fail 'Git config sanitization did not publish a passing report'

env_graft_file=$test_root/inherited-grafts
printf '%s %s\n' "$candidate_fixture_sha" "$unrelated_fixture_sha" >"$env_graft_file"
run_expect_status 2 'inherited Git metadata environment is cleared' "$logs/git-env.out" "$logs/git-env.err" \
	"${common_env[@]}" \
	GIT_CONFIG="$test_root/nonexistent-git-config" \
	GIT_GRAFT_FILE="$env_graft_file" \
	GIT_SHALLOW_FILE="$test_root/nonexistent-shallow-file" \
	GIT_ATTR_GLOBAL="$test_root/nonexistent-global-attributes" \
	GIT_ATTR_SYSTEM="$test_root/nonexistent-system-attributes" \
	GIT_ATTR_SOURCE=HEAD \
	FAKE_GIT_REQUIRE_SANITIZED_ENV=1 \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/git-env-runner.lock" \
	BENCH_BASE_REF="$unrelated_fixture_sha" \
	BENCH_GATE_OUT="$test_root/results/git-env" \
	"$gate"
assert_contains "$logs/git-env.err" "benchmark base $unrelated_fixture_sha is not an ancestor of candidate $candidate_fixture_sha"

busy_lock=$test_root/busy-runner.lock
: >"$busy_lock"
exec {busy_fd}>>"$busy_lock"
busy_fd_open=1
flock -n "$busy_fd"
run_expect_status 1 'busy host lock' "$logs/busy.out" "$logs/busy.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$busy_lock" \
	BENCH_GATE_OUT="$test_root/busy-output" \
	"$gate"
assert_contains "$logs/busy.err" 'dedicated benchmark runner is already in use'
flock -u "$busy_fd"
exec {busy_fd}>&-
busy_fd_open=0

fingerprint_out=$test_root/results/fingerprint-mismatch
fingerprint_log=$logs/fingerprint.log
fingerprint_harness_marker=$logs/fingerprint-harness.marker
run_expect_status 1 'measurement apparatus mismatch' "$logs/fingerprint.out" "$logs/fingerprint.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/fingerprint-runner.lock" \
	BENCH_GATE_OUT="$fingerprint_out" \
	FAKE_FINGERPRINT_LOG="$fingerprint_log" \
	FAKE_DIFFERENT_CANDIDATE_FINGERPRINT=1 \
	FAKE_HARNESS_MARKER="$fingerprint_harness_marker" \
	"$gate"
assert_contains "$logs/fingerprint.err" 'benchmark measurement apparatus differs'
assert_contains "$fingerprint_log" '/base|.|1s|10|20m'
assert_contains "$fingerprint_log" '/candidate|.|1s|10|20m'
(( $(wc -l <"$fingerprint_log") == 2 )) || fail 'fingerprint mismatch did not make exactly two fingerprint calls'
(( $(grep -Fc '/bin/reviewed-policy.json|' "$fingerprint_log") == 2 )) || fail 'both fingerprints did not use the reviewed trusted policy copy'
[[ ! -e $fingerprint_harness_marker ]] || fail 'fingerprint mismatch started the benchmark harness'

reported_policy_out=$test_root/results/reported-policy-mismatch
reported_policy_marker=$logs/reported-policy-harness.marker
run_expect_status 1 'fingerprint policy digest mismatch' "$logs/reported-policy.out" "$logs/reported-policy.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/reported-policy-runner.lock" \
	BENCH_GATE_OUT="$reported_policy_out" \
	FAKE_POLICY_DIGEST=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd \
	FAKE_HARNESS_MARKER="$reported_policy_marker" \
	"$gate"
assert_contains "$logs/reported-policy.err" "benchmark policy digest is dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd, want reviewed $reviewed_policy_digest"
[[ ! -e $reported_policy_marker ]] || fail 'policy digest mismatch started the benchmark harness'

harness_three_out=$test_root/results/harness-three
run_expect_status 1 'harness exit 3 is infrastructure failure' "$logs/harness-three.out" "$logs/harness-three.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/harness-three-runner.lock" \
	BENCH_GATE_OUT="$harness_three_out" \
	FAKE_HARNESS_STATUS=3 \
	"$gate"
assert_contains "$logs/harness-three.err" 'base benchmark harness exited with status 3'
[[ ! -e $harness_three_out && ! -L $harness_three_out ]] || fail 'harness exit 3 published a formal output directory'
harness_three_failed=("$harness_three_out".failed-*)
(( ${#harness_three_failed[@]} == 1 )) || fail "expected one failed harness evidence bundle, found ${#harness_three_failed[@]}"

signal_out=$test_root/results/spawn-signal
signal_pid_file=$test_root/spawn-signal.pid
run_expect_status 143 'TERM during process registration' "$logs/spawn-signal.out" "$logs/spawn-signal.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/spawn-signal-runner.lock" \
	BENCH_GATE_OUT="$signal_out" \
	FAKE_SPAWN_SIGNAL_PARENT=1 \
	FAKE_SETSID_PID_FILE="$signal_pid_file" \
	"$gate"
[[ -s $signal_pid_file ]] || fail 'spawn-window signal test did not record the setsid process'
signal_pid=$(<"$signal_pid_file")
if kill -0 "$signal_pid" 2>/dev/null; then
	fail "spawn-window signal left process $signal_pid running"
fi
[[ ! -e $signal_out && ! -L $signal_out ]] || fail 'spawn-window signal published a formal output directory'
[[ ! -e $signal_out.lock && ! -L $signal_out.lock ]] || fail 'spawn-window signal left the output lock'
signal_stages=("$signal_out".incomplete-*)
signal_failed=("$signal_out".failed-*)
(( ${#signal_stages[@]} == 0 )) || fail 'spawn-window signal left an incomplete staging directory'
(( ${#signal_failed[@]} == 1 )) || fail "expected one failed spawn-window evidence bundle, found ${#signal_failed[@]}"

missing_report_out=$test_root/results/missing-report
run_expect_status 1 'completed decision without report' "$logs/missing-report.out" "$logs/missing-report.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/missing-report-runner.lock" \
	BENCH_GATE_OUT="$missing_report_out" \
	FAKE_COMPARE_STATUS=0 \
	FAKE_SKIP_REPORT=1 \
	"$gate"
assert_contains "$logs/missing-report.err" 'completed benchmark decision did not publish gate.json'
[[ ! -e $missing_report_out && ! -L $missing_report_out ]] || fail 'reportless decision published a formal output directory'
missing_report_failed=("$missing_report_out".failed-*)
(( ${#missing_report_failed[@]} == 1 )) || fail "expected one failed reportless evidence bundle, found ${#missing_report_failed[@]}"

runner_drift_out=$test_root/results/runner-drift
runner_drift_state=$test_root/runner-drift.count
run_expect_status 1 'runner probe drift' "$logs/runner-drift.out" "$logs/runner-drift.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/runner-drift.lock" \
	BENCH_GATE_OUT="$runner_drift_out" \
	FAKE_PROBE_STATE="$runner_drift_state" \
	FAKE_RUNNER_DRIFT_AT=2 \
	"$gate"
assert_contains "$logs/runner-drift.err" 'base runner state changed while the benchmark was running'
[[ $(<"$runner_drift_state") == 2 ]] || fail 'runner drift did not occur on the after probe'
[[ ! -e $runner_drift_out && ! -L $runner_drift_out ]] || fail 'runner drift published a formal output directory'
runner_drift_failed=("$runner_drift_out".failed-*)
(( ${#runner_drift_failed[@]} == 1 )) || fail "expected one failed runner-drift bundle, found ${#runner_drift_failed[@]}"
[[ -s ${runner_drift_failed[0]}/base.runner.before.json ]] || fail 'runner-drift evidence is missing the before probe'
[[ -s ${runner_drift_failed[0]}/base.runner.after.json ]] || fail 'runner-drift evidence is missing the after probe'

report_mismatch_out=$test_root/results/report-mismatch
run_expect_status 1 'report and exit mismatch' "$logs/report-mismatch.out" "$logs/report-mismatch.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/report-mismatch.lock" \
	BENCH_GATE_OUT="$report_mismatch_out" \
	FAKE_COMPARE_STATUS=0 \
	FAKE_REPORT_PASSED=false \
	"$gate"
assert_contains "$logs/report-mismatch.err" 'synthetic verify-report: report identity or outcome mismatch'
[[ ! -e $report_mismatch_out && ! -L $report_mismatch_out ]] || fail 'inconsistent report published a formal output directory'
report_mismatch_failed=("$report_mismatch_out".failed-*)
(( ${#report_mismatch_failed[@]} == 1 )) || fail "expected one failed report-mismatch bundle, found ${#report_mismatch_failed[@]}"

pass_out=$test_root/results/pass
pass_go_log=$logs/pass-go.log
run_expect_status 0 'passing comparison' "$logs/pass.out" "$logs/pass.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/pass.lock" \
	BENCH_GATE_OUT="$pass_out" \
	FAKE_COMPARE_STATUS=0 \
	FAKE_GO_LOG="$pass_go_log" \
	"$gate"
assert_contains "$pass_out/gate.json" '"passed":true,"regression_count":0'
assert_contains "$logs/pass.err" "benchmark gate output: $pass_out"
[[ ! -e $pass_out.lock && ! -L $pass_out.lock ]] || fail 'output lock remained after passing publication'
pass_failed=("$pass_out".failed-*)
(( ${#pass_failed[@]} == 0 )) || fail 'passing decision was retained as failed evidence'
(( $(grep -Fc 'mod verify' "$pass_go_log") == 3 )) || fail 'formal pass did not verify trusted, base, and candidate modules'

for artifact in \
	base.raw.txt base.txt base.txt.meta.json base.runner.before.json base.runner.after.json \
	candidate.raw.txt candidate.txt candidate.txt.meta.json candidate.runner.before.json candidate.runner.after.json \
	benchstat.txt benchstat.stderr gate.json gate.txt gate.stderr; do
	[[ -f $pass_out/$artifact ]] || fail "published passing bundle is missing $artifact"
done

atomic_out=$test_root/results/regression
atomic_ready=$test_root/atomic.ready
atomic_release=$test_root/atomic.release
"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/atomic-runner.lock" \
	BENCH_GATE_OUT="$atomic_out" \
	FAKE_COMPARE_READY="$atomic_ready" \
	FAKE_COMPARE_RELEASE="$atomic_release" \
	"$gate" >"$logs/atomic.out" 2>"$logs/atomic.err" &
gate_pid=$!
if ! wait_for_file "$atomic_ready"; then
	: >"$atomic_release"
	wait "$gate_pid" 2>/dev/null || true
	gate_pid=
	fail 'timed out waiting for synthetic compare synchronization point'
fi
[[ ! -e $atomic_out && ! -L $atomic_out ]] || fail 'final output became visible before compare completed'
atomic_stages=("$atomic_out".incomplete-*)
(( ${#atomic_stages[@]} == 1 )) || fail "expected one incomplete atomic stage, found ${#atomic_stages[@]}"
[[ -s ${atomic_stages[0]}/gate.json ]] || fail 'synchronized stage is missing gate.json'
: >"$atomic_release"
set +e
wait "$gate_pid"
atomic_status=$?
set -e
gate_pid=
((atomic_status == 3)) || fail "regression comparison exited $atomic_status, want 3"

for artifact in \
	base.raw.txt base.txt base.txt.meta.json base.runner.before.json base.runner.after.json \
	candidate.raw.txt candidate.txt candidate.txt.meta.json candidate.runner.before.json candidate.runner.after.json \
	benchstat.txt benchstat.stderr gate.json gate.txt gate.stderr; do
	[[ -f $atomic_out/$artifact ]] || fail "published regression bundle is missing $artifact"
done
[[ -s $atomic_out/gate.json ]] || fail 'published regression bundle has an empty gate.json'
atomic_stages=("$atomic_out".incomplete-*)
atomic_failed=("$atomic_out".failed-*)
(( ${#atomic_stages[@]} == 0 )) || fail 'incomplete stage remained after atomic publication'
(( ${#atomic_failed[@]} == 0 )) || fail 'regression decision was incorrectly retained as failed evidence'
[[ ! -e $atomic_out.lock && ! -L $atomic_out.lock ]] || fail 'output lock remained after atomic publication'
assert_contains "$logs/atomic.err" "benchmark gate output: $atomic_out"

term_out=$test_root/results/compare-term
term_ready=$test_root/compare-term.ready
term_release=$test_root/compare-term.release
term_child_pid_file=$test_root/compare-term.child.pid
"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/compare-term.lock" \
	BENCH_GATE_OUT="$term_out" \
	FAKE_COMPARE_READY="$term_ready" \
	FAKE_COMPARE_RELEASE="$term_release" \
	FAKE_COMPARE_PID_FILE="$term_child_pid_file" \
	"$gate" >"$logs/compare-term.out" 2>"$logs/compare-term.err" &
gate_pid=$!
if ! wait_for_file "$term_ready" || ! wait_for_file "$term_child_pid_file"; then
	kill -KILL "$gate_pid" 2>/dev/null || true
	wait "$gate_pid" 2>/dev/null || true
	gate_pid=
	fail 'timed out waiting for the foreground compare process'
fi
term_child_pid=$(<"$term_child_pid_file")
kill -TERM "$gate_pid"
(
	sleep 10
	kill -KILL "$gate_pid" 2>/dev/null || true
) &
watchdog_pid=$!
term_started=$SECONDS
set +e
wait "$gate_pid"
term_status=$?
set -e
gate_pid=
kill -TERM "$watchdog_pid" 2>/dev/null || true
wait "$watchdog_pid" 2>/dev/null || true
watchdog_pid=
((term_status == 143)) || fail "TERM during foreground compare exited $term_status, want 143"
((SECONDS - term_started < 10)) || fail 'TERM during foreground compare did not exit promptly'
if kill -0 "$term_child_pid" 2>/dev/null; then
	fail "TERM left foreground compare process $term_child_pid running"
fi
[[ ! -e $term_out && ! -L $term_out ]] || fail 'TERM during compare published a formal output directory'
[[ ! -e $term_out.lock && ! -L $term_out.lock ]] || fail 'TERM during compare left the output lock'
term_failed=("$term_out".failed-*)
(( ${#term_failed[@]} == 1 )) || fail "expected one failed TERM evidence bundle, found ${#term_failed[@]}"

crash_out=$test_root/results/output-lock-crash
crash_ready=$test_root/output-lock-crash.ready
crash_rmdir_pid_file=$test_root/output-lock-crash.rmdir.pid
"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/output-lock-crash-runner.lock" \
	BENCH_GATE_OUT="$crash_out" \
	FAKE_COMPARE_STATUS=0 \
	FAKE_RMDIR_PAUSE_PATH="$crash_out.lock" \
	FAKE_RMDIR_READY="$crash_ready" \
	FAKE_RMDIR_PID_FILE="$crash_rmdir_pid_file" \
	"$gate" >"$logs/output-lock-crash.out" 2>"$logs/output-lock-crash.err" &
gate_pid=$!
if ! wait_for_file "$crash_ready" || ! wait_for_file "$crash_rmdir_pid_file"; then
	fail 'timed out waiting for final output-lock removal'
fi
paused_rmdir_pid=$(<"$crash_rmdir_pid_file")
[[ -s $crash_out/gate.json ]] || fail 'formal evidence was not renamed before final lock removal'
[[ -d $crash_out.lock && ! -L $crash_out.lock ]] || fail 'canonical output lock disappeared before final removal'
kill -KILL "$gate_pid"
set +e
wait "$gate_pid" 2>/dev/null
crash_status=$?
set -e
gate_pid=
kill -KILL "$paused_rmdir_pid" 2>/dev/null || true
paused_rmdir_pid=
((crash_status == 137)) || fail "SIGKILL during final lock removal exited $crash_status, want 137"
[[ -d $crash_out.lock && ! -L $crash_out.lock ]] || fail 'SIGKILL left formal output without its quarantine lock'

rollback_out=$test_root/results/output-lock-release
rollback_state=$test_root/output-lock-release.failed-once
run_expect_status 1 'output lock release failure rolls back publication' "$logs/output-lock-release.out" "$logs/output-lock-release.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/output-lock-release-runner.lock" \
	BENCH_GATE_OUT="$rollback_out" \
	FAKE_RMDIR_FAIL_ONCE_PATH="$rollback_out.lock" \
	FAKE_RMDIR_STATE="$rollback_state" \
	"$gate"
[[ -e $rollback_state ]] || fail 'output-lock release fault was not injected'
assert_contains "$logs/output-lock-release.err" "could not release benchmark output lock: $rollback_out.lock"
[[ ! -e $rollback_out && ! -L $rollback_out ]] || fail 'output-lock release failure left a formal output directory'
rollback_stages=("$rollback_out".incomplete-*)
rollback_failed=("$rollback_out".failed-*)
(( ${#rollback_stages[@]} == 0 )) || fail 'output-lock release failure left an incomplete staging directory'
(( ${#rollback_failed[@]} == 1 )) || fail "expected one rolled-back failed evidence bundle, found ${#rollback_failed[@]}"
[[ -s ${rollback_failed[0]}/gate.json ]] || fail 'rolled-back failed evidence is missing gate.json'
[[ ! -e $rollback_out.lock && ! -L $rollback_out.lock ]] || fail 'cleanup did not release the output lock after rollback'

signal_rollback_out=$test_root/results/output-lock-release-term
signal_rollback_state=$test_root/output-lock-release-term.failed-once
run_expect_status 143 'TERM during output lock release failure' "$logs/output-lock-release-term.out" "$logs/output-lock-release-term.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/output-lock-release-term-runner.lock" \
	BENCH_GATE_OUT="$signal_rollback_out" \
	FAKE_RMDIR_FAIL_ONCE_PATH="$signal_rollback_out.lock" \
	FAKE_RMDIR_STATE="$signal_rollback_state" \
	FAKE_RMDIR_SIGNAL_PARENT=1 \
	"$gate"
[[ -e $signal_rollback_state ]] || fail 'signal-time lock release fault was not injected'
[[ ! -e $signal_rollback_out && ! -L $signal_rollback_out ]] || fail 'signal-time lock release failure left a formal output directory'
signal_rollback_failed=("$signal_rollback_out".failed-*)
(( ${#signal_rollback_failed[@]} == 1 )) || fail "expected one signal-time failed evidence bundle, found ${#signal_rollback_failed[@]}"
[[ ! -e $signal_rollback_out.lock && ! -L $signal_rollback_out.lock ]] || fail 'signal-time cleanup left the output lock'

quarantine_out=$test_root/results/output-quarantine
quarantine_state=$test_root/output-quarantine.failed-once
run_expect_status 1 'rollback collision retains quarantine lock' "$logs/output-quarantine.out" "$logs/output-quarantine.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/output-quarantine-runner.lock" \
	BENCH_GATE_OUT="$quarantine_out" \
	FAKE_RMDIR_FAIL_ONCE_PATH="$quarantine_out.lock" \
	FAKE_RMDIR_STATE="$quarantine_state" \
	FAKE_RMDIR_CREATE_ROLLBACK_COLLISION=1 \
	"$gate"
[[ -e $quarantine_state ]] || fail 'quarantine rollback fault was not injected'
assert_contains "$logs/output-quarantine.err" "benchmark output remains quarantined by lock: $quarantine_out.lock"
[[ -d $quarantine_out && ! -L $quarantine_out ]] || fail 'quarantined output evidence is missing'
[[ -d $quarantine_out.lock && ! -L $quarantine_out.lock ]] || fail 'rollback collision did not retain the quarantine lock'
quarantine_failed=("$quarantine_out".failed-*)
(( ${#quarantine_failed[@]} == 1 )) || fail "expected one occupied rollback target, found ${#quarantine_failed[@]}"

clobber_out=$test_root/results/concurrent-output
clobber_ready=$test_root/clobber.ready
clobber_release=$test_root/clobber.release
"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/clobber-runner.lock" \
	BENCH_GATE_OUT="$clobber_out" \
	FAKE_COMPARE_READY="$clobber_ready" \
	FAKE_COMPARE_RELEASE="$clobber_release" \
	"$gate" >"$logs/clobber.out" 2>"$logs/clobber.err" &
gate_pid=$!
if ! wait_for_file "$clobber_ready"; then
	: >"$clobber_release"
	wait "$gate_pid" 2>/dev/null || true
	gate_pid=
	fail 'timed out waiting for concurrent-output synchronization point'
fi
mkdir "$clobber_out"
clobber_inode=$(stat -c %i "$clobber_out")
: >"$clobber_release"
set +e
wait "$gate_pid"
clobber_status=$?
set -e
gate_pid=
((clobber_status == 1)) || fail "concurrent output creation exited $clobber_status, want 1"
[[ -d $clobber_out && ! -L $clobber_out ]] || fail 'concurrent output directory was removed or replaced'
[[ $(stat -c %i "$clobber_out") == "$clobber_inode" ]] || fail 'concurrent output directory inode was replaced'
clobber_failed=("$clobber_out".failed-*)
(( ${#clobber_failed[@]} == 1 )) || fail "expected one failed no-clobber evidence bundle, found ${#clobber_failed[@]}"
assert_contains "$logs/clobber.err" 'could not publish benchmark gate output'

cleanup_out=$test_root/results/cleanup-failure
run_expect_status 1 'cleanup failure upgrades regression' "$logs/cleanup.out" "$logs/cleanup.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/cleanup-runner.lock" \
	BENCH_GATE_OUT="$cleanup_out" \
	FAKE_LEAVE_BINARY=1 \
	"$gate"
assert_contains "$logs/cleanup.err" 'benchmark binary directory is not empty'
[[ ! -e $cleanup_out && ! -L $cleanup_out ]] || fail 'cleanup failure published a formal output directory'
cleanup_failed=("$cleanup_out".failed-*)
(( ${#cleanup_failed[@]} == 1 )) || fail "expected one failed evidence bundle, found ${#cleanup_failed[@]}"
[[ -s ${cleanup_failed[0]}/gate.json ]] || fail 'failed evidence bundle is missing the completed regression report'
[[ ! -e $cleanup_out.lock && ! -L $cleanup_out.lock ]] || fail 'output lock remained after cleanup failure handling'

worktree_list_scratch_before=("$test_root"/.pitaya-benchmark-gate-*)
worktree_list_out=$test_root/results/worktree-list-failure
run_expect_status 1 'worktree list failure retains scratch' "$logs/worktree-list-failure.out" "$logs/worktree-list-failure.err" \
	"${common_env[@]}" \
	BENCH_RUNNER_ID=fixed-runner \
	BENCH_RUNNER_LOCK="$test_root/worktree-list-failure-runner.lock" \
	BENCH_GATE_OUT="$worktree_list_out" \
	FAKE_COMPARE_STATUS=0 \
	FAKE_GIT_FAIL_WORKTREE_LIST=1 \
	"$gate"
assert_contains "$logs/worktree-list-failure.err" 'could not inspect registered benchmark worktrees'
assert_contains "$logs/worktree-list-failure.err" 'benchmark scratch retained because worktree registration is not clear'
[[ ! -e $worktree_list_out && ! -L $worktree_list_out ]] || fail 'worktree-list failure published a formal output directory'
worktree_list_failed=("$worktree_list_out".failed-*)
(( ${#worktree_list_failed[@]} == 1 )) || fail "expected one worktree-list failed evidence bundle, found ${#worktree_list_failed[@]}"
worktree_list_scratch=("$test_root"/.pitaya-benchmark-gate-*)
(( ${#worktree_list_scratch[@]} == ${#worktree_list_scratch_before[@]} + 1 )) ||
	fail 'worktree-list failure did not retain exactly one additional scratch directory'

echo 'benchmark gate orchestration tests passed'
