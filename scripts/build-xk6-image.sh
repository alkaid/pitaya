#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

source_revision() {
	local head diff_hash
	head=$(git -C "$repo_root" rev-parse HEAD)
	if git -C "$repo_root" diff --quiet HEAD --; then
		printf '%s\n' "$head"
		return
	fi
	diff_hash=$(git -C "$repo_root" diff --binary HEAD -- | sha256sum)
	diff_hash=${diff_hash%% *}
	printf '%s-dirty.%s\n' "$head" "${diff_hash:0:12}"
}

if [[ ${1:-} == --print-revision ]]; then
	source_revision
	exit 0
fi

image=${1:?usage: build-xk6-image.sh IMAGE}
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
context=$tmpdir/context
mkdir -p "$context"

stager=$tmpdir/stagecontext
(
	cd "$repo_root/tools"
	GOWORK=off go build -o "$stager" ./cmd/stagecontext
)
"$stager" -repo "$repo_root" -destination "$context"

while IFS= read -r -d '' file; do
	case $file in
		*_test.go)
			;;
		acceptor/*.go | client/*.go | co/*.go | config/*.go | conn/*.go | \
		constants/*.go | context/*.go | interfaces/*.go | logger/*.go | \
		metrics/*.go | networkentity/*.go | protos/*.go | serialize/*.go | \
		session/*.go | util/*.go | xk6-pitaya/*.go)
			echo "untracked production source is not in the Docker context: $file" >&2
			exit 1
		;;
	esac
done < <(git -C "$repo_root" ls-files -z --others --exclude-standard)

revision=$(source_revision)
printf '%s\n' "$revision" >"$context/.pitaya-docker-context"

docker build \
	--build-arg "PITAYA_REVISION=$revision" \
	--file "$repo_root/xk6-pitaya/Dockerfile" \
	--tag "$image" \
	"$context"
