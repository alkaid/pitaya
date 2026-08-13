#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
audit_script=$repo_root/scripts/audit-benchmark-gate-config.sh
real_git=$(command -v git)

fail() {
	echo "benchmark gate configuration audit test: $*" >&2
	exit 1
}

assert_contains() {
	local file=$1
	local expected=$2
	grep -Fq -- "$expected" "$file" || {
		echo "expected $file to contain: $expected" >&2
		sed -n '1,160p' "$file" >&2 || true
		return 1
	}
}

run_success() {
	local name=$1
	shift
	if ! "$@" >"$case_root/stdout" 2>"$case_root/stderr"; then
		echo "$name failed unexpectedly" >&2
		sed -n '1,200p' "$case_root/stderr" >&2 || true
		return 1
	fi
	assert_contains "$case_root/stdout" 'benchmark gate configuration audit passed: acme/pitaya/benchmark-gate'
}

run_json_success() {
	local name=$1
	shift
	if ! "$@" >"$case_root/stdout" 2>"$case_root/stderr"; then
		echo "$name failed unexpectedly" >&2
		sed -n '1,200p' "$case_root/stderr" >&2 || true
		return 1
	fi
	jq -e --arg gate "$gate_sha" --arg policy "$policy_digest" '
		type == "object" and length == 4 and
		.repo == "acme/pitaya" and .environment == "benchmark-gate" and
		.gate_source_sha == $gate and .policy_sha256 == $policy
	' "$case_root/stdout" >/dev/null || fail "$name returned unexpected JSON"
}

run_failure() {
	local name=$1
	local expected=$2
	local status
	shift 2
	set +e
	"$@" >"$case_root/stdout" 2>"$case_root/stderr"
	status=$?
	set -e
	((status != 0)) || fail "$name succeeded unexpectedly"
	assert_contains "$case_root/stderr" "$expected"
}

[[ -x $audit_script ]] || fail "missing executable audit script: $audit_script"
[[ $real_git == /* ]] || fail "could not resolve git"

test_root=$(mktemp -d)
cleanup() {
	rm -rf -- "${test_root:?}"
}
trap cleanup EXIT

fake_bin=$test_root/fake-bin
mkdir -p "$fake_bin"

cat >"$fake_bin/gh" <<'MOCK_GH'
#!/usr/bin/env bash
set -euo pipefail

[[ ${1:-} == api ]] || { echo 'mock gh: expected api command' >&2; exit 90; }
shift
hostname=
method=
include=0
accept_seen=0
version_seen=0
endpoint=
while (($# > 0)); do
	case $1 in
	--hostname) hostname=${2:-}; shift 2 ;;
	--method) method=${2:-}; shift 2 ;;
	--include) include=1; shift ;;
	-H)
		case ${2:-} in
		'Accept: application/vnd.github+json') accept_seen=1 ;;
		'X-GitHub-Api-Version: 2022-11-28') version_seen=1 ;;
		esac
		shift 2
		;;
	*)
		[[ -z $endpoint ]] || { echo "mock gh: unexpected argument: $1" >&2; exit 90; }
		endpoint=$1
		shift
		;;
	esac
done
[[ $hostname == github.com ]] || { echo 'mock gh: hostname was not pinned' >&2; exit 90; }
[[ $method == GET ]] || { echo 'mock gh: method was not GET' >&2; exit 90; }
((accept_seen == 1 && version_seen == 1)) || { echo 'mock gh: required headers missing' >&2; exit 90; }
printf '%s\n' "$endpoint" >>"$MOCK_API_LOG"

case $endpoint in
repos/acme/pitaya) key=repository ;;
repos/acme/pitaya/environments/benchmark-gate) key=environment ;;
repos/acme/pitaya/environments/benchmark-gate/variables/BENCH_GATE_SOURCE_SHA)
	key=environment-gate-variable
	;;
repos/acme/pitaya/environments/benchmark-gate/variables/BENCH_POLICY_SHA256)
	key=environment-policy-variable
	;;
repos/acme/pitaya/actions/runners\?per_page=100\&page=*)
	key=repository-runners-${endpoint##*page=}
	;;
repos/acme/pitaya/actions/variables\?per_page=1\&page=1) key=repository-variables-probe ;;
orgs/acme/actions/variables\?per_page=1\&page=1) key=organization-variables-probe ;;
repos/acme/pitaya/actions/variables/BENCH_GATE_SOURCE_SHA) key=repository-gate-variable ;;
repos/acme/pitaya/actions/variables/BENCH_POLICY_SHA256) key=repository-policy-variable ;;
orgs/acme/actions/variables/BENCH_GATE_SOURCE_SHA) key=organization-gate-variable ;;
orgs/acme/actions/variables/BENCH_POLICY_SHA256) key=organization-policy-variable ;;
repos/acme/pitaya/environments/benchmark-gate/deployment-branch-policies\?per_page=100\&page=*)
	key=branch-policies-${endpoint##*page=}
	;;
repos/acme/pitaya/git/commits/*) key=gate-commit ;;
repos/acme/pitaya/contents/scripts/benchmark-policy.json\?ref=*) key=policy-content ;;
*) echo "mock gh: unexpected endpoint: $endpoint" >&2; exit 90 ;;
esac

status=0
[[ ! -f $MOCK_API_ROOT/$key.status ]] || read -r status <"$MOCK_API_ROOT/$key.status"
if ((include != 0)); then
	if ((status == 0)); then
		printf 'HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n'
	else
		printf 'HTTP/2.0 %s %s\r\nContent-Type: application/json\r\n\r\n' \
			"$status" "$([[ $status == 404 ]] && printf 'Not Found' || printf 'Error')"
	fi
fi
[[ -f $MOCK_API_ROOT/$key.json ]] || { echo "mock gh: no fixture for $key" >&2; exit 90; }
cat "$MOCK_API_ROOT/$key.json"
((status == 0)) || exit 1
MOCK_GH
chmod +x "$fake_bin/gh"

cat >"$fake_bin/git" <<MOCK_GIT
#!/usr/bin/env bash
set -euo pipefail
if [[ -n \${MOCK_GIT_REMOTE:-} && \${*: -3} == 'config --get remote.origin.url' ]]; then
	printf '%s\n' "\$MOCK_GIT_REMOTE"
	exit 0
fi
exec "$real_git" "\$@"
MOCK_GIT
chmod +x "$fake_bin/git"

policy_bytes=$test_root/policy.json
printf '{"schema_version":1,"profile":"synthetic"}\n' >"$policy_bytes"
policy_digest=$(sha256sum "$policy_bytes")
policy_digest=${policy_digest%% *}
gate_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

write_absent() {
	local key=$1
	printf '404\n' >"$fixtures/$key.status"
	printf '{"message":"Not Found","status":"404"}\n' >"$fixtures/$key.json"
}

prepare_case() {
	local name=$1
	case_root=$test_root/$name
	fixtures=$case_root/fixtures
	mkdir -p "$fixtures"
	: >"$case_root/api.log"
	cat >"$fixtures/repository.json" <<'EOF'
{"full_name":"acme/pitaya","default_branch":"main","owner":{"login":"acme","type":"Organization"}}
EOF
	cat >"$fixtures/environment.json" <<'EOF'
{"name":"benchmark-gate","deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}
EOF
	jq -n --arg value "$gate_sha" '{name:"BENCH_GATE_SOURCE_SHA",value:$value}' \
		>"$fixtures/environment-gate-variable.json"
	jq -n --arg value "$policy_digest" '{name:"BENCH_POLICY_SHA256",value:$value}' \
		>"$fixtures/environment-policy-variable.json"
	printf '{"total_count":0,"runners":[]}\n' >"$fixtures/repository-runners-1.json"
	printf '{"total_count":0,"variables":[]}\n' >"$fixtures/repository-variables-probe.json"
	printf '{"total_count":0,"variables":[]}\n' >"$fixtures/organization-variables-probe.json"
	write_absent repository-gate-variable
	write_absent repository-policy-variable
	write_absent organization-gate-variable
	write_absent organization-policy-variable
	cat >"$fixtures/branch-policies-1.json" <<'EOF'
{"total_count":1,"branch_policies":[{"id":1,"node_id":"branch-node","name":"main","type":"branch"}]}
EOF
	jq -n --arg sha "$gate_sha" '{sha:$sha,tree:{sha:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}' \
		>"$fixtures/gate-commit.json"
	encoded=$(base64 -w0 "$policy_bytes")
	size=$(wc -c <"$policy_bytes")
	jq -n --arg content "$encoded" --argjson size "$size" '{
		type:"file",path:"scripts/benchmark-policy.json",encoding:"base64",
		content:$content,size:$size,sha:"cccccccccccccccccccccccccccccccccccccccc"
	}' >"$fixtures/policy-content.json"
}

audit_explicit() {
	env \
		PATH="$fake_bin:$PATH" \
		MOCK_API_ROOT="$fixtures" \
		MOCK_API_LOG="$case_root/api.log" \
		"$audit_script" --repo acme/pitaya "$@"
}

prepare_case success
run_success success audit_explicit
assert_contains "$case_root/api.log" 'repos/acme/pitaya/actions/variables/BENCH_GATE_SOURCE_SHA'
assert_contains "$case_root/api.log" 'orgs/acme/actions/variables/BENCH_POLICY_SHA256'

prepare_case json
run_json_success json audit_explicit --format json

prepare_case user-owner
jq '.owner.type = "User"' "$fixtures/repository.json" >"$fixtures/repository.next"
mv "$fixtures/repository.next" "$fixtures/repository.json"
run_success 'user owner' audit_explicit
if grep -Fq 'orgs/acme/' "$case_root/api.log"; then
	fail 'user owner unexpectedly queried organization variables'
fi

prepare_case repo-runner
printf '{"total_count":1,"runners":[{"id":1}]}\n' >"$fixtures/repository-runners-1.json"
run_failure 'repository runner' 'repository must have zero self-hosted runners' audit_explicit

prepare_case repo-runner-page-two
jq -n '{total_count:101,runners:[range(1; 101) | {id:.}]}' \
	>"$fixtures/repository-runners-1.json"
printf '{"total_count":101,"runners":[{"id":101}]}\n' \
	>"$fixtures/repository-runners-2.json"
run_failure 'repository runner page two' 'repository must have zero self-hosted runners' audit_explicit
assert_contains "$case_root/api.log" 'repos/acme/pitaya/actions/runners?per_page=100&page=2'

prepare_case runner-pagination-empty
jq -n '{total_count:101,runners:[range(1; 101) | {id:.}]}' \
	>"$fixtures/repository-runners-1.json"
printf '{"total_count":101,"runners":[]}\n' >"$fixtures/repository-runners-2.json"
run_failure 'runner pagination empty' 'collection ended before total_count' audit_explicit

prepare_case runner-pagination-drift
jq -n '{total_count:101,runners:[range(1; 101) | {id:.}]}' \
	>"$fixtures/repository-runners-1.json"
printf '{"total_count":102,"runners":[{"id":101}]}\n' \
	>"$fixtures/repository-runners-2.json"
run_failure 'runner pagination drift' 'collection changed during audit' audit_explicit

prepare_case runner-api-error
printf '500\n' >"$fixtures/repository-runners-1.status"
run_failure 'runner API error' 'GitHub API request failed: repos/acme/pitaya/actions/runners' audit_explicit

prepare_case repository-shadow
rm "$fixtures/repository-gate-variable.status"
printf '{"name":"BENCH_GATE_SOURCE_SHA","value":"shadow"}\n' >"$fixtures/repository-gate-variable.json"
run_failure 'repository shadow' 'repository must not define' audit_explicit

prepare_case organization-shadow
rm "$fixtures/organization-policy-variable.status"
printf '{"name":"BENCH_POLICY_SHA256","value":"shadow"}\n' >"$fixtures/organization-policy-variable.json"
run_failure 'organization shadow' 'organization must not define' audit_explicit

prepare_case shadow-permission-error
printf '403\n' >"$fixtures/repository-policy-variable.status"
printf '{"message":"Forbidden","status":"403"}\n' >"$fixtures/repository-policy-variable.json"
run_failure 'shadow permission error' 'could not prove repository variable absent' audit_explicit

prepare_case permission-probe-error
printf '403\n' >"$fixtures/repository-variables-probe.status"
run_failure 'permission probe error' 'GitHub API request failed: repos/acme/pitaya/actions/variables' audit_explicit

prepare_case permission-probe-schema
printf '{}\n' >"$fixtures/repository-variables-probe.json"
run_failure 'permission probe schema' 'repository variable permission probe schema is invalid' audit_explicit

prepare_case shadow-malformed-404
printf '{"message":"Not Found"}\n' >"$fixtures/repository-policy-variable.json"
run_failure 'malformed shadow 404' 'GitHub absence response schema is invalid' audit_explicit

prepare_case missing-pin
printf '404\n' >"$fixtures/environment-gate-variable.status"
printf '{"message":"Not Found","status":"404"}\n' >"$fixtures/environment-gate-variable.json"
run_failure 'missing pin' 'GitHub API request failed' audit_explicit

prepare_case wrong-exact-pin-name
jq '.name = "bench_gate_source_sha"' "$fixtures/environment-gate-variable.json" \
		>"$fixtures/environment-gate-variable.next"
mv "$fixtures/environment-gate-variable.next" "$fixtures/environment-gate-variable.json"
run_failure 'wrong exact pin name' 'exact lookup schema is invalid' audit_explicit

prepare_case invalid-gate-pin
jq '.value = "short"' "$fixtures/environment-gate-variable.json" \
		>"$fixtures/environment-gate-variable.next"
mv "$fixtures/environment-gate-variable.next" "$fixtures/environment-gate-variable.json"
run_failure 'invalid gate pin' 'must be a 40-character lowercase commit SHA' audit_explicit

prepare_case invalid-policy-pin
jq '.value = "short"' "$fixtures/environment-policy-variable.json" \
		>"$fixtures/environment-policy-variable.next"
mv "$fixtures/environment-policy-variable.next" "$fixtures/environment-policy-variable.json"
run_failure 'invalid policy pin' 'must be a 64-character lowercase SHA-256 digest' audit_explicit

prepare_case wrong-branch-policy
jq '.branch_policies[0].name = "main*"' "$fixtures/branch-policies-1.json" \
		>"$fixtures/branch-policies.next"
mv "$fixtures/branch-policies.next" "$fixtures/branch-policies-1.json"
run_failure 'wrong branch policy' 'exactly one deployment policy for the main branch' audit_explicit

prepare_case digest-mismatch
jq '.value = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"' \
		"$fixtures/environment-policy-variable.json" >"$fixtures/environment-policy-variable.next"
mv "$fixtures/environment-policy-variable.next" "$fixtures/environment-policy-variable.json"
run_failure 'policy digest mismatch' 'pinned scripts/benchmark-policy.json digest is' audit_explicit

prepare_case policy-symlink
jq '.type = "symlink"' "$fixtures/policy-content.json" >"$fixtures/policy-content.next"
mv "$fixtures/policy-content.next" "$fixtures/policy-content.json"
run_failure 'policy symlink' 'pinned benchmark policy content schema is invalid' audit_explicit

prepare_case commit-mismatch
jq '.sha = "dddddddddddddddddddddddddddddddddddddddd"' "$fixtures/gate-commit.json" \
		>"$fixtures/gate-commit.next"
mv "$fixtures/gate-commit.next" "$fixtures/gate-commit.json"
run_failure 'gate commit mismatch' 'does not identify the exact pinned Git commit' audit_explicit

prepare_case api-error
printf '500\n' >"$fixtures/environment.status"
run_failure 'API error' 'GitHub API request failed' audit_explicit

prepare_case bad-repo
run_failure 'bad repo input' '--repo must be a valid OWNER/REPO' env \
	PATH="$fake_bin:$PATH" \
	MOCK_API_ROOT="$fixtures" \
	MOCK_API_LOG="$case_root/api.log" \
	"$audit_script" --repo bad/repo/extra

prepare_case remote-resolution
if ! env \
	PATH="$fake_bin:$PATH" \
	MOCK_API_ROOT="$fixtures" \
	MOCK_API_LOG="$case_root/api.log" \
	MOCK_GIT_REMOTE='git@github.com:acme/pitaya.git' \
	"$audit_script" >"$case_root/stdout" 2>"$case_root/stderr"; then
	fail 'remote repository resolution failed unexpectedly'
fi

echo 'benchmark gate configuration audit tests passed'
