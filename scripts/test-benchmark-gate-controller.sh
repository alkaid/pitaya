#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
source_controller=$repo_root/scripts/run-benchmark-gate-controller.sh
real_git=$(command -v git)
real_mkdir=$(command -v mkdir)
real_mv=$(command -v mv)
real_stat=$(command -v stat)

fail() {
	echo "benchmark gate controller test: $*" >&2
	exit 1
}

assert_contains() {
	local file=$1
	local expected=$2
	grep -Fq -- "$expected" "$file" || {
		echo "expected $file to contain: $expected" >&2
		sed -n '1,200p' "$file" >&2 || true
		return 1
	}
}

test_root=$(mktemp -d)
cleanup() {
	rm -rf -- "${test_root:?}"
}
trap cleanup EXIT

fixture=$test_root/repo
mkdir -p "$fixture/scripts"
cp "$source_controller" "$fixture/scripts/run-benchmark-gate-controller.sh"
controller=$fixture/scripts/run-benchmark-gate-controller.sh

base_sha=1111111111111111111111111111111111111111
candidate_sha=2222222222222222222222222222222222222222
gate_sha=3333333333333333333333333333333333333333
policy_sha=4444444444444444444444444444444444444444444444444444444444444444
rotated_gate_sha=6666666666666666666666666666666666666666
rotated_policy_sha=7777777777777777777777777777777777777777777777777777777777777777

cat >"$fixture/scripts/audit-benchmark-gate-config.sh" <<'MOCK_AUDIT'
#!/usr/bin/env bash
set -euo pipefail
count_file=$MOCK_ROOT/audit-count
count=0
[[ ! -f $count_file ]] || read -r count <"$count_file"
count=$((count + 1))
printf '%s\n' "$count" >"$count_file"
if [[ ${MOCK_AUDIT_FAIL_AT:-0} == "$count" ]]; then
	exit 1
fi
gate=${MOCK_GATE_SHA:-3333333333333333333333333333333333333333}
policy=${MOCK_POLICY_SHA:-4444444444444444444444444444444444444444444444444444444444444444}
if [[ ${MOCK_DRIFT:-0} == 1 && $count -gt 1 ]]; then
	policy=5555555555555555555555555555555555555555555555555555555555555555
fi
if [[ ${MOCK_AUDIT_MALFORMED_AT:-0} == "$count" ]]; then
	printf '{\n'
	exit 0
fi
jq -cn --arg gate "$gate" --arg policy "$policy" '{
	repo:"acme/pitaya",environment:"benchmark-gate",
	gate_source_sha:$gate,policy_sha256:$policy
}'
MOCK_AUDIT
chmod +x "$fixture/scripts/audit-benchmark-gate-config.sh"

fake_bin=$test_root/fake-bin
mkdir -p "$fake_bin"
cat >"$fake_bin/git" <<MOCK_GIT
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "\$@" >>"\$MOCK_ROOT/git.log"
printf '\n' >>"\$MOCK_ROOT/git.log"
case "\$*" in
*'config --get remote.origin.url') printf 'git@github.com:acme/pitaya.git\n' ;;
*'ls-remote --exit-code https://github.com/acme/pitaya.git refs/heads/main')
	remote_count_file=\$MOCK_ROOT/remote-count
	remote_count=0
	[[ ! -f \$remote_count_file ]] || read -r remote_count <"\$remote_count_file"
	remote_count=\$((remote_count + 1))
	printf '%s\n' "\$remote_count" >"\$remote_count_file"
	if [[ \${MOCK_REMOTE_FAIL:-0} == 1 ||
		\${MOCK_REMOTE_FAIL_AT:-0} == "\$remote_count" ]]; then
		exit 1
	fi
	remote_sha=\${MOCK_REMOTE_SHA:-$candidate_sha}
	if [[ \${MOCK_REMOTE_DRIFT:-0} == 1 && \$remote_count -gt 1 ]]; then
		remote_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
	fi
	printf '%s\trefs/heads/main\n' "\$remote_sha"
	;;
*'cat-file blob '*'scripts/benchmark-gate-nats.sh') cat "$test_root/raw-gate-script" ;;
*'cat-file -e'*) exit 0 ;;
*'rev-parse --verify'*'^{commit}')
	case "\$*" in
	*'HEAD^{commit}'*) printf '%s\n' "\${MOCK_HEAD_SHA:-$candidate_sha}" ;;
	*'$candidate_sha'*) printf '$candidate_sha\n' ;;
	*'$base_sha'*) printf '$base_sha\n' ;;
	*'$gate_sha'*) printf '$gate_sha\n' ;;
	*'$rotated_gate_sha'*) printf '$rotated_gate_sha\n' ;;
	*) exit 1 ;;
	esac
	;;
*'status --porcelain=v1 --untracked-files=all') printf '%s' "\${MOCK_STATUS:-}" ;;
*'merge-base --is-ancestor'*) [[ \${MOCK_ANCESTOR:-1} == 1 ]] ;;
*'worktree add --detach'*)
	gate_worktree=\${*: -2:1}
	printf '%s\n' "\$gate_worktree" >"\$MOCK_ROOT/gate-worktree"
	"$real_mkdir" -p "\$gate_worktree/scripts"
	cat >"\$gate_worktree/scripts/benchmark-gate-nats.sh" <<'MOCK_GATE'
#!/usr/bin/env bash
set -euo pipefail
trap 'printf "TERM\n" >"$test_root/gate-signal-forwarded"; exit 1' TERM
env | sort >"$test_root/gate-env-\${MOCK_CASE_NAME:-unknown}"
mkdir "\$BENCH_GATE_OUT"
read -r run_id <"$test_root/gate-run-id"
jq -n --arg run_id "\$run_id" '{run_id:\$run_id}' >"\$BENCH_GATE_OUT/gate.json"
read -r remove_evidence <"$test_root/gate-remove-evidence"
if [[ \$remove_evidence == 1 ]]; then
	rm -rf -- "\$BENCH_GATE_OUT"
fi
read -r signal_parent <"$test_root/gate-signal-parent"
if [[ \$signal_parent != NONE ]]; then
	kill -"\$signal_parent" "\$PPID"
	while :; do sleep 1; done
fi
read -r remove_bootstrap <"$test_root/gate-remove-bootstrap"
if [[ \$remove_bootstrap == 1 ]]; then
	rm -f -- "$fixture/build/benchmarks/.benchmark-gate-bootstrap.json"
fi
read -r status <"$test_root/gate-status"
exit "\$status"
MOCK_GATE
	chmod +x "\$gate_worktree/scripts/benchmark-gate-nats.sh"
	cp "\$gate_worktree/scripts/benchmark-gate-nats.sh" "$test_root/raw-gate-script"
	;;
*'worktree remove --force'*)
	if [[ \${MOCK_WORKTREE_REMOVE_FAIL:-0} == 1 ]]; then exit 1; fi
	rm -rf -- "\${*: -1}"
	;;
*'worktree list --porcelain'*)
	if [[ \${MOCK_WORKTREE_REMOVE_FAIL:-0} == 1 ]]; then
		read -r gate_worktree <"\$MOCK_ROOT/gate-worktree"
		printf 'worktree %s\n\n' "\$gate_worktree"
	fi
	;;
*'fetch --no-tags'*) ;;
*) echo "mock git: unexpected command: \$*" >&2; exit 90 ;;
esac
MOCK_GIT
chmod +x "$fake_bin/git"

cat >"$fake_bin/mv" <<MOCK_MV
#!/usr/bin/env bash
set -euo pipefail
"$real_mv" "\$@"
destination=\${*: -1}
if [[ \${MOCK_SIGNAL_AFTER_PROMOTE:-0} == 1 && \$destination == */nats-gate-* ]]; then
	kill -TERM "\$PPID"
fi
MOCK_MV
chmod +x "$fake_bin/mv"

cat >"$fake_bin/stat" <<MOCK_STAT
#!/usr/bin/env bash
set -euo pipefail
target=\${*: -1}
if [[ \${MOCK_STAT_FAIL_ON_OUTPUT_LOCK:-0} == 1 && \$target == *.lock ]]; then
	exit 1
fi
exec "$real_stat" "\$@"
MOCK_STAT
chmod +x "$fake_bin/stat"

run_case() {
	local name=$1
	shift
	case_root=$test_root/cases/$name
	mkdir -p "$case_root"
	gate_status=0
	gate_run_id=test-run-1
	gate_remove_evidence=0
	gate_signal_parent=NONE
	gate_remove_bootstrap=0
	output_parent_symlink=0
	seed_bootstrap=1
	existing_formal=0
	bootstrap_gate=$gate_sha
	bootstrap_policy=$policy_sha
	for argument in "$@"; do
		case $argument in
		MOCK_GATE_STATUS=*) gate_status=${argument#*=} ;;
		MOCK_RUN_ID=*) gate_run_id=${argument#*=} ;;
		MOCK_REMOVE_EVIDENCE=*) gate_remove_evidence=${argument#*=} ;;
		MOCK_SIGNAL_PARENT=*) gate_signal_parent=${argument#*=} ;;
		MOCK_REMOVE_BOOTSTRAP=*) gate_remove_bootstrap=${argument#*=} ;;
		MOCK_OUTPUT_PARENT_SYMLINK=*) output_parent_symlink=${argument#*=} ;;
		MOCK_EXISTING_FORMAL=*) existing_formal=${argument#*=} ;;
		MOCK_NO_BOOTSTRAP=1) seed_bootstrap=0 ;;
		MOCK_GATE_SHA=*) bootstrap_gate=${argument#*=} ;;
		MOCK_POLICY_SHA=*) bootstrap_policy=${argument#*=} ;;
		MOCK_STALE_BOOTSTRAP=1)
			bootstrap_gate=$gate_sha
			bootstrap_policy=$policy_sha
			;;
		esac
	done
	printf '%s\n' "$gate_status" >"$test_root/gate-status"
	printf '%s\n' "$gate_run_id" >"$test_root/gate-run-id"
	printf '%s\n' "$gate_remove_evidence" >"$test_root/gate-remove-evidence"
	printf '%s\n' "$gate_signal_parent" >"$test_root/gate-signal-parent"
	printf '%s\n' "$gate_remove_bootstrap" >"$test_root/gate-remove-bootstrap"
	rm -f -- "$test_root/gate-signal-forwarded"
	rm -rf -- "$fixture/build"
	if [[ $output_parent_symlink == 1 ]]; then
		mkdir -p "$case_root/external-output"
		ln -s "$case_root/external-output" "$fixture/build"
	else
		mkdir -p "$fixture/build/benchmarks"
		if ((seed_bootstrap != 0)); then
			jq -cn --arg gate "$bootstrap_gate" --arg policy "$bootstrap_policy" '{
				repo:"acme/pitaya",environment:"benchmark-gate",
				gate_source_sha:$gate,policy_sha256:$policy
			}' >"$fixture/build/benchmarks/.benchmark-gate-bootstrap.json"
		fi
		if ((existing_formal != 0)); then
			mkdir "$fixture/build/benchmarks/nats-gate-$gate_run_id"
			printf 'existing formal evidence\n' \
				>"$fixture/build/benchmarks/nats-gate-$gate_run_id/sentinel"
		fi
	fi
	case_out=$fixture/build/benchmarks
	: >"$case_root/git.log"
	MOCK_ROOT=$case_root \
	PATH="$fake_bin:$PATH" \
	"$@" >"$case_root/stdout" 2>"$case_root/stderr"
}

expect_status() {
	local want=$1
	local name=$2
	shift 2
	local status=0
	set +e
	run_case "$name" "$@"
	status=$?
	set -e
	((status == want)) || {
		echo "$name returned $status, want $want" >&2
		sed -n '1,200p' "$case_root/stderr" >&2 || true
		return 1
	}
}

expect_status 0 pass "$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
published=$(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*')
[[ -n $published && -f $published/gate.json ]] || fail 'pass did not promote formal evidence'
[[ $(<"$case_root/audit-count") == 2 ]] || fail 'pass did not read pins twice'
gate_env=$test_root/gate-env-unknown
if grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|BENCH_GATE_SOURCE_SHA=caller|BENCH_POLICY_SHA256=caller)' "$gate_env"; then
	fail 'credential or caller pin leaked into gate environment'
fi
assert_contains "$gate_env" 'BENCH_RUNNER_ID=pitaya-benchmark-linux-amd64-01'
assert_contains "$gate_env" 'BENCH_RUNNER_LOCK=/var/lock/pitaya/benchmark.lock'
assert_contains "$gate_env" "BENCH_BASE_REF=$base_sha"
assert_contains "$gate_env" "BENCH_CANDIDATE_REF=$candidate_sha"
assert_contains "$gate_env" "BENCH_GATE_SOURCE_SHA=$gate_sha"
assert_contains "$gate_env" "BENCH_POLICY_SHA256=$policy_sha"

expect_status 0 rotated-pins env MOCK_GATE_SHA="$rotated_gate_sha" \
	MOCK_POLICY_SHA="$rotated_policy_sha" \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$gate_env" "BENCH_GATE_SOURCE_SHA=$rotated_gate_sha"
assert_contains "$gate_env" "BENCH_POLICY_SHA256=$rotated_policy_sha"

expect_status 1 unbootstrapped env MOCK_NO_BOOTSTRAP=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'current benchmark pins are not bootstrapped'

expect_status 1 rotated-unbootstrapped env MOCK_GATE_SHA="$rotated_gate_sha" \
	MOCK_POLICY_SHA="$rotated_policy_sha" MOCK_STALE_BOOTSTRAP=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'current benchmark pins are not bootstrapped'

expect_status 0 bootstrap env MOCK_NO_BOOTSTRAP=1 \
	"$controller" --repo acme/pitaya --bootstrap "$candidate_sha" "$base_sha"
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'bootstrap published formal evidence'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.bootstrap-controller-*') ]] ||
	fail 'bootstrap did not retain informational evidence'
jq -e --arg gate "$gate_sha" --arg policy "$policy_sha" '
	.gate_source_sha == $gate and .policy_sha256 == $policy
' "$case_out/.benchmark-gate-bootstrap.json" >/dev/null ||
	fail 'bootstrap did not install the audited pin marker'

expect_status 3 bootstrap-regression env MOCK_NO_BOOTSTRAP=1 MOCK_GATE_STATUS=3 \
	MOCK_RUN_ID=bootstrap-regression \
	"$controller" --repo acme/pitaya --bootstrap "$candidate_sha" "$base_sha"
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'regression bootstrap published formal evidence'
[[ -d $case_out/.bootstrap-controller-bootstrap-regression ]] ||
	fail 'regression bootstrap did not retain informational evidence'
[[ -f $case_out/.benchmark-gate-bootstrap.json ]] ||
	fail 'regression bootstrap did not install the audited pin marker'

expect_status 3 regression env MOCK_GATE_STATUS=3 "$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
published=$(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*')
[[ -n $published && -f $published/gate.json ]] || fail 'regression did not promote formal evidence'

expect_status 1 infrastructure env MOCK_GATE_STATUS=2 "$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'infrastructure failed with status 2'
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'infrastructure failure published formal evidence'

expect_status 1 signal-int env TMPDIR="$case_root" MOCK_SIGNAL_PARENT=INT \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'signal published formal evidence'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.pending-*.failed') ]] ||
	fail 'signal did not retain explicitly non-formal evidence'
[[ -z $(find "$case_root" -mindepth 1 -maxdepth 1 -type d -name 'pitaya-benchmark-controller-*') ]] ||
	fail 'signal did not remove controller scratch'
[[ $(<"$test_root/gate-signal-forwarded") == TERM ]] ||
	fail 'controller did not convert INT into ordered gate TERM cleanup'

expect_status 1 output-lock-stat env MOCK_STAT_FAIL_ON_OUTPUT_LOCK=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -name 'nats-gate-*.lock') ]] ||
	fail 'output lock stat failure retained a stale lock'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.pending-*.failed') ]] ||
	fail 'output lock stat failure did not retain non-formal evidence'

expect_status 1 promotion-signal env MOCK_SIGNAL_AFTER_PROMOTE=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'promotion signal left formal evidence'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.failed-controller-*') ]] ||
	fail 'promotion signal did not demote evidence'

expect_status 1 worktree-cleanup env MOCK_WORKTREE_REMOVE_FAIL=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'could not clean controller worktree before publication'
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'worktree cleanup failure published formal evidence'

expect_status 1 output-parent-symlink env MOCK_OUTPUT_PARENT_SYMLINK=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'controller output path must not contain symlinks'
[[ ! -e $case_root/external-output/benchmarks ]] ||
	fail 'controller wrote through an output-parent symlink before rejecting it'

expect_status 1 drift env MOCK_DRIFT=1 "$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'pin tuple changed during execution'
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'pin drift published formal evidence'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.pending-*.failed') ]] ||
	fail 'pin drift did not retain explicitly non-formal evidence'

expect_status 1 remote-drift env MOCK_REMOTE_DRIFT=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'remote main changed during benchmark execution'
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'remote drift published formal evidence'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.pending-*.failed') ]] ||
	fail 'remote drift did not retain non-formal evidence'

expect_status 1 remote-post-error env MOCK_REMOTE_FAIL_AT=2 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'could not re-resolve remote refs/heads/main'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.pending-*.failed') ]] ||
	fail 'post-run remote error did not retain non-formal evidence'

expect_status 1 bootstrap-marker-drift env MOCK_REMOVE_BOOTSTRAP=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'benchmark bootstrap marker changed during execution'
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name 'nats-gate-*') ]] ||
	fail 'bootstrap marker drift published formal evidence'

expect_status 1 formal-collision env MOCK_EXISTING_FORMAL=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'benchmark controller output already exists'
assert_contains "$case_out/nats-gate-test-run-1/sentinel" 'existing formal evidence'
[[ -z $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.failed-controller-*') ]] ||
	fail 'formal collision moved existing evidence'
[[ -n $(find "$case_out" -mindepth 1 -maxdepth 1 -type d -name '.pending-*.failed') ]] ||
	fail 'formal collision did not retain the new non-formal evidence'

expect_status 1 post-audit env MOCK_AUDIT_FAIL_AT=2 "$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'could not audit benchmark-gate pins after execution'

expect_status 1 malformed-post-audit env MOCK_AUDIT_MALFORMED_AT=2 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'could not audit benchmark-gate pins after execution'

expect_status 1 missing-evidence env MOCK_REMOVE_EVIDENCE=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'did not publish pending evidence'

expect_status 1 invalid-run-id env MOCK_RUN_ID='../escape' \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'invalid run_id'

expect_status 1 stale-candidate env MOCK_REMOTE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'is not current remote main'
[[ ! -f $case_root/audit-count ]] || fail 'stale candidate queried protected pins'

expect_status 1 remote-error env MOCK_REMOTE_FAIL=1 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'could not resolve remote refs/heads/main'

expect_status 1 non-ancestor env MOCK_ANCESTOR=0 \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'is not an ancestor of candidate'

expect_status 1 wrong-head env MOCK_HEAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'candidate checkout HEAD'

expect_status 1 dirty env MOCK_STATUS=' M tracked.go' \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'candidate checkout must be clean'

expect_status 1 same-commit "$controller" --repo acme/pitaya "$candidate_sha" "$candidate_sha"
assert_contains "$case_root/stderr" 'base and candidate must be different commits'

expect_status 1 short-candidate "$controller" --repo acme/pitaya deadbeef "$base_sha"
assert_contains "$case_root/stderr" 'candidate must be a full lowercase 40-character SHA'

expect_status 1 host-gate-pin env \
	BENCH_GATE_SOURCE_SHA=caller \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'benchmark pins must not be defined in the controller environment'
[[ ! -f $case_root/audit-count ]] || fail 'host gate pin queried protected pins'

expect_status 1 host-policy-pin env \
	BENCH_POLICY_SHA256=caller \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
assert_contains "$case_root/stderr" 'benchmark pins must not be defined in the controller environment'
[[ ! -f $case_root/audit-count ]] || fail 'host policy pin queried protected pins'

expect_status 0 credential-env env \
	GH_TOKEN=secret-token \
	GITHUB_TOKEN=other-token \
	"$controller" --repo acme/pitaya "$candidate_sha" "$base_sha"
if grep -Eq '^(GH_TOKEN|GITHUB_TOKEN)=' "$gate_env"; then
	fail 'controller credential leaked into gate process'
fi

echo 'benchmark gate controller tests passed'
