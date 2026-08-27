#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
allowlist=$repo_root/scripts/verify-tools.allowlist
today=$(date -u +%F)
declare -A allowed=()
declare -A used=()

while IFS='|' read -r expires owner file reference reason; do
	[[ -z $expires || $expires = \#* ]] && continue
	if [[ ! $expires =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ || $expires < $today ]]; then
		echo "expired or invalid tools allowlist entry: $expires|$file|$reference" >&2
		exit 1
	fi
	if [[ -z $owner ]]; then
		echo "tools allowlist entry has no owner: $file|$reference" >&2
		exit 1
	fi
	if [[ -z $reason ]]; then
		echo "tools allowlist entry has no rationale: $file|$reference" >&2
		exit 1
	fi
	allowed["$file|$reference"]=$expires
done <"$allowlist"

is_allowed() {
	local file=$1
	local reference=$2
	local key="$file|$reference"
	if [[ -n ${allowed[$key]:-} ]]; then
		used[$key]=1
		return 0
	fi
	return 1
}

while IFS=: read -r file line content; do
	use=$(sed -E 's/.*uses:[[:space:]]*([^[:space:]#]+).*/\1/' <<<"$content")
	[[ $use = ./* ]] && continue
	ref=${use##*@}
	if [[ ! $ref =~ ^[0-9a-f]{40}$ ]]; then
		echo "$file:$line uses a mutable action reference: $use" >&2
		exit 1
	fi
done < <(cd "$repo_root" && rg -n --no-heading '^[[:space:]]*uses:[[:space:]]*' .github/workflows)

while IFS=: read -r file line content; do
	file=${file#./}
	if [[ $content =~ FROM[[:space:]]+([^[:space:]#]+) ]]; then
		reference=${BASH_REMATCH[1]}
	elif [[ $content =~ image:[[:space:]]*([^[:space:]#]+) ]]; then
		reference=${BASH_REMATCH[1]}
	else
		continue
	fi
	if [[ $reference =~ @sha256:[0-9a-f]{64}$ ]]; then
		continue
	fi
	if ! is_allowed "$file" "$reference"; then
		echo "$file:$line uses an image without an immutable digest: $reference" >&2
		exit 1
	fi
done < <(
	cd "$repo_root"
	rg -n --no-heading '^[[:space:]]*(FROM|image:|-[[:space:]]+image:)[[:space:]]*' \
		--glob 'Dockerfile*' --glob '*.yaml' --glob '*.yml' .
)

for key in "${!allowed[@]}"; do
	if [[ -z ${used[$key]:-} ]]; then
		echo "unused tools allowlist entry: $key" >&2
		exit 1
	fi
done

if (
	cd "$repo_root"
	rg -n '(@latest|:latest)' --glob 'Dockerfile*' --glob '*.yaml' --glob '*.yml' \
		--glob '*.sh' --glob 'Makefile' --glob '!scripts/verify-tools.sh' \
		--glob '!scripts/verify-tools.allowlist' .
); then
	echo "floating tool or image version found" >&2
	exit 1
fi

go_version_at_least() {
	local actual=${1#go}
	local minimum=$2
	local actual_major actual_minor actual_patch
	local minimum_major minimum_minor minimum_patch

	if [[ ! $actual =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
		return 2
	fi
	actual_major=${BASH_REMATCH[1]}
	actual_minor=${BASH_REMATCH[2]}
	actual_patch=${BASH_REMATCH[3]}

	if [[ ! $minimum =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
		return 2
	fi
	minimum_major=${BASH_REMATCH[1]}
	minimum_minor=${BASH_REMATCH[2]}
	minimum_patch=${BASH_REMATCH[3]}

	if ((actual_major != minimum_major)); then
		((actual_major > minimum_major))
		return
	fi
	if ((actual_minor != minimum_minor)); then
		((actual_minor > minimum_minor))
		return
	fi
	((actual_patch >= minimum_patch))
}

required_go=$(awk '$1 == "go" { print $2; exit }' "$repo_root/go.mod")
actual_go=$(go env GOVERSION)
if go_version_at_least "$actual_go" "$required_go"; then
	:
else
	version_status=$?
	if ((version_status == 2)); then
		echo "unable to compare Go versions: minimum $required_go, found $actual_go" >&2
	else
		echo "Go $required_go or newer is required (found ${actual_go#go})" >&2
	fi
	exit 1
fi

mock_version=$(cd "$repo_root" && GOWORK=off go list -m -f '{{.Version}}' go.uber.org/mock)
if [[ $mock_version != v0.6.0 ]]; then
	echo "unexpected mockgen module version: $mock_version" >&2
	exit 1
fi

vuln_version=$(cd "$repo_root/tools" && GOWORK=off go list -m -f '{{.Version}}' golang.org/x/vuln)
if [[ $vuln_version != v1.6.0 ]]; then
	echo "unexpected govulncheck module version: $vuln_version" >&2
	exit 1
fi

perf_version=$(cd "$repo_root/tools" && GOWORK=off go list -m -f '{{.Version}}' golang.org/x/perf)
if [[ $perf_version != v0.0.0-20260709024250-82a0b07e230d ]]; then
	echo "unexpected benchmark parser/statistics module version: $perf_version" >&2
	exit 1
fi

(
	cd "$repo_root/tools"
	GOWORK=off go mod verify
	GOWORK=off go test ./...
)

# Exercise lock, publication, regression, and cleanup semantics without Docker.
"$repo_root/scripts/test-benchmark-gate-nats-orchestration.sh"

# Keep the fixed-host controller and the live-config auditor's fail-closed
# behavior executable without contacting GitHub or running Docker.
"$repo_root/scripts/test-audit-benchmark-gate-config.sh"
"$repo_root/scripts/test-benchmark-gate-controller.sh"

echo "tool, action, and image pin verification passed"
