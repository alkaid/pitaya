#!/usr/bin/env bash
set -euo pipefail

readonly environment_name=benchmark-gate
readonly gate_variable=BENCH_GATE_SOURCE_SHA
readonly policy_variable=BENCH_POLICY_SHA256
readonly policy_path=scripts/benchmark-policy.json

fail() {
	echo "benchmark gate configuration audit: $*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/audit-benchmark-gate-config.sh [--repo OWNER/REPO] [--format human|json]

Audit the protected benchmark-gate GitHub Environment, its two trust pins, and
the repository's local-controller policy. When --repo is omitted, the
repository is resolved from the GitHub Actions context or a strictly parsed
github.com origin remote. JSON output is intended for the local controller.
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
	git@github.com:*)
		path=${remote#git@github.com:}
		;;
	https://github.com/*)
		path=${remote#https://github.com/}
		;;
	ssh://git@github.com/*)
		path=${remote#ssh://git@github.com/}
		;;
	*)
		return 1
		;;
	esac
	path=${path%.git}
	validate_repo_name "$path" || return 1
	printf '%s\n' "$path"
}

requested_repo=
output_format=human
while (($# > 0)); do
	case $1 in
	--repo)
		(($# >= 2)) || fail "--repo requires OWNER/REPO"
		[[ -z $requested_repo ]] || fail "--repo may only be specified once"
		requested_repo=$2
		shift 2
		;;
	--format)
		(($# >= 2)) || fail "--format requires human or json"
		[[ $output_format == human ]] || fail "--format may only be specified once"
		output_format=$2
		[[ $output_format == human || $output_format == json ]] ||
			fail "--format requires human or json"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		fail "unknown argument: $1"
		;;
	esac
done

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
if [[ -n $requested_repo ]]; then
	validate_repo_name "$requested_repo" || fail "--repo must be a valid OWNER/REPO"
elif [[ ${GITHUB_ACTIONS:-} == true ]]; then
	[[ ${GITHUB_SERVER_URL:-} == https://github.com ]] ||
		fail "GITHUB_SERVER_URL must be https://github.com"
	[[ ${GITHUB_API_URL:-} == https://api.github.com ]] ||
		fail "GITHUB_API_URL must be https://api.github.com"
	requested_repo=${GITHUB_REPOSITORY:-}
	validate_repo_name "$requested_repo" ||
		fail "GITHUB_REPOSITORY must be a valid OWNER/REPO"
	context_owner=${GITHUB_REPOSITORY_OWNER:-}
	requested_owner=${requested_repo%%/*}
	[[ -n $context_owner && ${context_owner,,} == ${requested_owner,,} ]] ||
		fail "GITHUB_REPOSITORY_OWNER does not match GITHUB_REPOSITORY"
else
	git_bin=$(command -v git) || fail "git is required to resolve the origin repository"
	if ! origin=$($git_bin -C "$repo_root" config --get remote.origin.url); then
		fail "could not read remote.origin.url; pass --repo OWNER/REPO"
	fi
	if ! requested_repo=$(repo_from_remote "$origin"); then
		fail "origin must be an unambiguous github.com OWNER/REPO URL; pass --repo OWNER/REPO"
	fi
fi

gh_bin=$(command -v gh) || fail "gh is required"
jq_bin=$(command -v jq) || fail "jq is required"
base64_bin=$(command -v base64) || fail "base64 is required"
sha256_bin=$(command -v sha256sum) || fail "sha256sum is required"

audit_root=$(mktemp -d)
cleanup() {
	rm -rf -- "${audit_root:?}"
}
trap cleanup EXIT

api_get() {
	local endpoint=$1
	local output=$2
	if ! "$gh_bin" api \
		--hostname github.com \
		--method GET \
		-H 'Accept: application/vnd.github+json' \
		-H 'X-GitHub-Api-Version: 2022-11-28' \
		"$endpoint" >"$output"; then
		fail "GitHub API request failed: $endpoint"
	fi
	[[ -s $output ]] || fail "GitHub API returned an empty response: $endpoint"
	# jq normally accepts a stream of top-level JSON values and reports the
	# status of the last one. REST responses must contain exactly one value, or
	# a later valid object could conceal an earlier malformed schema result.
	if ! "$jq_bin" -e -s 'length == 1' "$output" >/dev/null 2>&1; then
		fail "GitHub API must return exactly one JSON value: $endpoint"
	fi
}

# Exact lower-scope lookups close the pagination race inherent in proving a
# name absent from a mutable collection. Only an explicit, well-formed GitHub
# 404 proves absence; authentication, permission, transport, and schema errors
# all fail the audit.
api_expect_absent() {
	local endpoint=$1
	local label=$2
	local permission_endpoint=$3
	local array_key=$4
	local permission_file=$audit_root/permission-$label.json
	local response=$audit_root/exact-response
	local error_file=$audit_root/exact-error
	local body=$audit_root/exact-body
	local status http_codes

	# Prove that the token can enumerate this scope before accepting an exact
	# 404. Otherwise GitHub may intentionally conceal an existing variable from
	# a token that lacks Actions-variable permission.
	api_get "$permission_endpoint?per_page=1&page=1" "$permission_file"
	if ! "$jq_bin" -e --arg key "$array_key" '
		type == "object" and
		(.total_count | type == "number" and . >= 0 and floor == .) and
		(has($key)) and (.[$key] | type == "array" and length <= 1)
	' "$permission_file" >/dev/null; then
		fail "$label variable permission probe schema is invalid"
	fi

	set +e
	"$gh_bin" api \
		--include \
		--hostname github.com \
		--method GET \
		-H 'Accept: application/vnd.github+json' \
		-H 'X-GitHub-Api-Version: 2022-11-28' \
		"$endpoint" >"$response" 2>"$error_file"
	status=$?
	set -e
	if ((status == 0)); then
		fail "$label must not define $gate_variable or $policy_variable"
	fi
	http_codes=$(tr -d '\r' <"$response" | awk '$1 ~ /^HTTP\/[0-9.]+$/ && $2 ~ /^[0-9][0-9][0-9]$/ { print $2 }')
	[[ $http_codes == 404 ]] || fail "could not prove $label variable absent: $endpoint"
	tr -d '\r' <"$response" | awk '
		$1 ~ /^HTTP\/[0-9.]+$/ && $2 ~ /^[0-9][0-9][0-9]$/ { in_headers = 1; body = 0; next }
		in_headers && $0 == "" { in_headers = 0; body = 1; next }
		body { print }
	' >"$body"
	if ! "$jq_bin" -e -s '
		length == 1 and (.[0] | type == "object" and
		.message == "Not Found" and .status == "404")
	' "$body" >/dev/null 2>&1; then
		fail "GitHub absence response schema is invalid: $endpoint"
	fi
}

fetch_collection() {
	local endpoint=$1
	local array_key=$2
	local output=$3
	local page=1
	local expected_total=
	local collected=0
	local page_file=$audit_root/page.json
	local next_file=$audit_root/collection-next.json
	local page_total page_count

	printf '[]\n' >"$output"
	while :; do
		api_get "$endpoint?per_page=100&page=$page" "$page_file"
		if ! "$jq_bin" -e --arg key "$array_key" '
			type == "object" and
			(.total_count | type == "number" and . >= 0 and floor == .) and
			(has($key)) and (.[$key] | type == "array" and length <= 100)
		' "$page_file" >/dev/null; then
			fail "GitHub API collection schema is invalid: $endpoint page $page"
		fi
		page_total=$($jq_bin -r '.total_count' "$page_file")
		page_count=$($jq_bin -r --arg key "$array_key" '.[$key] | length' "$page_file")
		if [[ -z $expected_total ]]; then
			expected_total=$page_total
		elif ((page_total != expected_total)); then
			fail "GitHub API collection changed during audit: $endpoint"
		fi
		collected=$((collected + page_count))
		((collected <= expected_total)) ||
			fail "GitHub API collection exceeds total_count: $endpoint"
		"$jq_bin" -s --arg key "$array_key" '.[0] + .[1][$key]' \
			"$output" "$page_file" >"$next_file"
		mv "$next_file" "$output"
		if ((collected == expected_total)); then
			break
		fi
		((page_count > 0)) || fail "GitHub API collection ended before total_count: $endpoint"
		page=$((page + 1))
	done
}

repo_file=$audit_root/repository.json
api_get "repos/$requested_repo" "$repo_file"
if ! "$jq_bin" -e '
	type == "object" and
	(.full_name | type == "string") and
	(.default_branch | type == "string") and
	(.owner | type == "object") and
	(.owner.login | type == "string") and
	(.owner.type == "User" or .owner.type == "Organization")
' "$repo_file" >/dev/null; then
	fail "repository owner schema or account type is invalid"
fi
canonical_repo=$($jq_bin -r '.full_name' "$repo_file")
owner_login=$($jq_bin -r '.owner.login' "$repo_file")
owner_type=$($jq_bin -r '.owner.type' "$repo_file")
validate_repo_name "$canonical_repo" || fail "GitHub API returned an invalid repository name"
[[ $($jq_bin -r '.default_branch' "$repo_file") == main ]] ||
	fail "repository default branch must be main"
if [[ ${canonical_repo,,} != ${requested_repo,,} ]]; then
	fail "GitHub API repository identity mismatch: got $canonical_repo, want $requested_repo"
fi
canonical_owner=${canonical_repo%%/*}
if [[ ${canonical_owner,,} != ${owner_login,,} ]]; then
	fail "GitHub API repository owner identity is inconsistent"
fi

repository_runners=$audit_root/repository-runners.json
fetch_collection "repos/$canonical_repo/actions/runners" runners "$repository_runners"
if ! "$jq_bin" -e 'type == "array" and length == 0' "$repository_runners" >/dev/null; then
	fail "repository must have zero self-hosted runners; the benchmark controller runs locally"
fi

environment_file=$audit_root/environment.json
api_get "repos/$canonical_repo/environments/$environment_name" "$environment_file"
if ! "$jq_bin" -e --arg name "$environment_name" '
	type == "object" and
	.name == $name and
	(.deployment_branch_policy | type == "object") and
	.deployment_branch_policy.protected_branches == false and
	.deployment_branch_policy.custom_branch_policies == true
' "$environment_file" >/dev/null; then
	fail "$environment_name must use only custom deployment branch policies"
fi

gate_variable_file=$audit_root/gate-variable.json
policy_variable_file=$audit_root/policy-variable.json
api_get \
	"repos/$canonical_repo/environments/$environment_name/variables/$gate_variable" \
	"$gate_variable_file"
api_get \
	"repos/$canonical_repo/environments/$environment_name/variables/$policy_variable" \
	"$policy_variable_file"
if ! "$jq_bin" -e --arg name "$gate_variable" '
	type == "object" and .name == $name and (.value | type == "string")
' "$gate_variable_file" >/dev/null; then
	fail "$gate_variable exact lookup schema is invalid"
fi
if ! "$jq_bin" -e --arg name "$policy_variable" '
	type == "object" and .name == $name and (.value | type == "string")
' "$policy_variable_file" >/dev/null; then
	fail "$policy_variable exact lookup schema is invalid"
fi
gate_source_sha=$($jq_bin -r '.value' "$gate_variable_file")
policy_sha256=$($jq_bin -r '.value' "$policy_variable_file")
[[ $gate_source_sha =~ ^[0-9a-f]{40}$ ]] ||
	fail "$gate_variable must be a 40-character lowercase commit SHA"
[[ $policy_sha256 =~ ^[0-9a-f]{64}$ ]] ||
	fail "$policy_variable must be a 64-character lowercase SHA-256 digest"

api_expect_absent \
	"repos/$canonical_repo/actions/variables/$gate_variable" \
	"repository" \
	"repos/$canonical_repo/actions/variables" \
	"variables"
api_expect_absent \
	"repos/$canonical_repo/actions/variables/$policy_variable" \
	"repository" \
	"repos/$canonical_repo/actions/variables" \
	"variables"

if [[ $owner_type == Organization ]]; then
	api_expect_absent \
		"orgs/$owner_login/actions/variables/$gate_variable" \
		"organization" \
		"orgs/$owner_login/actions/variables" \
		"variables"
	api_expect_absent \
		"orgs/$owner_login/actions/variables/$policy_variable" \
		"organization" \
		"orgs/$owner_login/actions/variables" \
		"variables"
fi

branch_policies=$audit_root/branch-policies.json
fetch_collection \
	"repos/$canonical_repo/environments/$environment_name/deployment-branch-policies" \
	branch_policies "$branch_policies"
if ! "$jq_bin" -e '
	type == "array" and length == 1 and
	.[0].name == "main" and .[0].type == "branch" and
	(.[0].id | type == "number" and floor == .) and
	(.[0].node_id | type == "string")
' "$branch_policies" >/dev/null; then
	fail "$environment_name must have exactly one deployment policy for the main branch"
fi

content_file=$audit_root/policy-content.json
commit_file=$audit_root/gate-commit.json
api_get "repos/$canonical_repo/git/commits/$gate_source_sha" "$commit_file"
if ! "$jq_bin" -e --arg sha "$gate_source_sha" '
	type == "object" and .sha == $sha and
	(.tree | type == "object") and
	(.tree.sha | type == "string" and test("^[0-9a-f]{40}$"))
' "$commit_file" >/dev/null; then
	fail "$gate_variable does not identify the exact pinned Git commit"
fi
api_get \
	"repos/$canonical_repo/contents/$policy_path?ref=$gate_source_sha" \
	"$content_file"
if ! "$jq_bin" -e --arg path "$policy_path" '
	type == "object" and
	.type == "file" and .path == $path and .encoding == "base64" and
	(.content | type == "string" and length > 0) and
	(.size | type == "number" and . >= 0 and floor == .) and
	(.sha | type == "string" and test("^[0-9a-f]{40}$"))
' "$content_file" >/dev/null; then
	fail "pinned benchmark policy content schema is invalid"
fi
decoded_policy=$audit_root/benchmark-policy.json
if ! "$jq_bin" -jr '.content' "$content_file" | "$base64_bin" --decode >"$decoded_policy"; then
	fail "pinned benchmark policy content is not valid base64"
fi
expected_policy_size=$($jq_bin -r '.size' "$content_file")
actual_policy_size=$(wc -c <"$decoded_policy") ||
	fail "could not measure the pinned benchmark policy"
if ((actual_policy_size != expected_policy_size)); then
	fail "pinned benchmark policy size is $actual_policy_size, want $expected_policy_size"
fi
actual_policy_sha256=$($sha256_bin "$decoded_policy") ||
	fail "could not hash the pinned benchmark policy"
actual_policy_sha256=${actual_policy_sha256%% *}
if [[ $actual_policy_sha256 != "$policy_sha256" ]]; then
	fail "pinned $policy_path digest is $actual_policy_sha256, want $policy_sha256"
fi

if [[ $output_format == json ]]; then
	"$jq_bin" -cn \
		--arg repo "$canonical_repo" \
		--arg environment "$environment_name" \
		--arg gate "$gate_source_sha" \
		--arg policy "$policy_sha256" \
		'{repo:$repo,environment:$environment,gate_source_sha:$gate,policy_sha256:$policy}'
else
	echo "benchmark gate configuration audit passed: $canonical_repo/$environment_name"
	echo "$gate_variable=$gate_source_sha"
	echo "$policy_variable=$policy_sha256"
fi
