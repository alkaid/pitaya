#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workspace=$repo_root/go.work
module_dirs=(. pitaya-cli xk6-pitaya)
upstream_identity_pattern='(github\.com/topfreegames/|git@github\.com:topfreegames/)pitaya(/v2)?(\.git)?([\x27/@[:space:]"(),:;]|$)'
manifests=(
	go.mod
	go.sum
	go.work
	go.work.sum
	pitaya-cli/go.mod
	pitaya-cli/go.sum
	xk6-pitaya/go.mod
	xk6-pitaya/go.sum
)

snapshot_manifests() {
	local base=$1
	local output=$2
	local manifest
	: >"$output"
	for manifest in "${manifests[@]}"; do
		if [[ -f $base/$manifest ]]; then
			sha256sum "$base/$manifest" | sed "s#  $base/#  #" >>"$output"
		else
			printf 'MISSING  %s\n' "$manifest" >>"$output"
		fi
	done
}

scan_upstream_identities() {
	rg --hidden --no-ignore -n -i "$upstream_identity_pattern" "$1"
}

is_identity_candidate() {
	local file=$1
	local base=${file##*/}
	case $base in
		go.mod | go.sum | go.work | go.work.sum) return 0 ;;
	esac
	[[ $file == *.go || ${base,,} == *dockerfile* ]]
}

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

for upstream_sample in \
	'github.com/topfreegames/pitaya' \
	'github.com/topfreegames/pitaya/component' \
	'github.com/topfreegames/pitaya/v2' \
	'github.com/topfreegames/pitaya/v2/component' \
	'github.com/topfreegames/pitaya@v2.11.24' \
	'https://github.com/topfreegames/pitaya.git' \
	'git@github.com:topfreegames/pitaya.git' \
	"RUN git clone 'https://github.com/topfreegames/pitaya' /src" \
	'HTTPS://GITHUB.COM/TopFreeGames/Pitaya/v2/component'; do
	if ! rg -q -i "$upstream_identity_pattern" <<<"$upstream_sample"; then
		echo "source identity detector missed: $upstream_sample" >&2
		exit 1
	fi
done

for identity_candidate in \
	go.mod go.sum go.work go.work.sum main.go Dockerfile Dockerfile.release dev.Dockerfile; do
	if ! is_identity_candidate "$identity_candidate"; then
		echo "source identity candidate selector missed: $identity_candidate" >&2
		exit 1
	fi
done
for allowed_neighbor in \
	'github.com/alkaid/pitaya/v2/component' \
	'github.com/topfreegames/pitaya-cli' \
	'github.com/topfreegames/pitaya-protos' \
	'github.com/topfreegames/pitayadmin'; do
	if rg -q -i "$upstream_identity_pattern" <<<"$allowed_neighbor"; then
		echo "source identity detector rejected a neighboring module: $allowed_neighbor" >&2
		exit 1
	fi
done

stager=$tmpdir/stagecontext
(
	cd "$repo_root/tools"
	GOWORK=off go build -o "$stager" ./cmd/stagecontext
)

stage_source_files() {
	local repo=$1
	local destination=$2
	"$stager" \
		-repo "$repo" \
		-destination "$destination" \
		-files-from-stdin \
		-require-all
}

symlink_probe=$tmpdir/source-symlink-probe
mkdir -p "$symlink_probe/repo"
printf '%s\n' 'github.com/topfreegames/pitaya/v2' >"$symlink_probe/repo/payload.txt"
ln -s payload.txt "$symlink_probe/repo/Dockerfile.probe"
if printf 'Dockerfile.probe\0' | stage_source_files "$symlink_probe/repo" "$symlink_probe/staged" >/dev/null 2>&1; then
	echo "source identity staging accepted a symlink candidate" >&2
	exit 1
fi

hidden_probe=$tmpdir/hidden-probe/.hidden
mkdir -p "$hidden_probe"
printf '%s\n' 'import "github.com/topfreegames/pitaya/v2/component"' >"$hidden_probe/probe.go"
if ! scan_upstream_identities "$tmpdir/hidden-probe" >/dev/null; then
	echo "source identity scan skipped a hidden path" >&2
	exit 1
fi

source_root=$tmpdir/source-identity
source_files=$tmpdir/source-files
source_file_count=0
: >"$source_files"
while IFS= read -r -d '' file; do
	if is_identity_candidate "$file"; then
		printf '%s\0' "$file" >>"$source_files"
		((source_file_count += 1))
	fi
done < <(git -C "$repo_root" ls-files -z --cached --others --exclude-standard)

((source_file_count > 0)) || {
	echo "source identity scan has no input files" >&2
	exit 1
}
stage_source_files "$repo_root" "$source_root" <"$source_files"
[[ -d $source_root ]] || {
	echo "source identity staging did not create its destination" >&2
	exit 1
}

source_matches=$tmpdir/source-identity-matches
if scan_upstream_identities "$source_root" >"$source_matches"; then
	cat "$source_matches" >&2
	echo "source, module manifest, or Dockerfile contains an upstream Pitaya identity" >&2
	exit 1
else
	status=$?
	if [[ $status -ne 1 ]]; then
		echo "source identity scan failed with status $status" >&2
		exit 1
	fi
fi

stage_manifests() {
	local destination=$1
	local manifest
	for manifest in "${manifests[@]}"; do
		printf '%s\0' "$manifest"
	done | "$stager" \
		-repo "$repo_root" \
		-destination "$destination" \
		-files-from-stdin \
		-require-all
}

original_root=$tmpdir/original
sync_root=$tmpdir/sync
stage_manifests "$original_root"
stage_manifests "$sync_root"
snapshot_manifests "$original_root" "$tmpdir/before"

while IFS= read -r -d '' file; do
	[[ $file == *.go ]] || continue
	[[ $file == tools/* ]] && continue
	printf '%s\0' "$file"
done < <(git -C "$repo_root" ls-files -z --cached --others --exclude-standard) | \
	"$stager" -repo "$repo_root" -destination "$sync_root" -files-from-stdin

(
	cd "$sync_root"
	GOWORK="$sync_root/go.work" go work sync
)

snapshot_manifests "$sync_root" "$tmpdir/synced"
if ! cmp -s "$tmpdir/before" "$tmpdir/synced"; then
	echo "go work sync would change module or workspace manifests:" >&2
	diff -u "$tmpdir/before" "$tmpdir/synced" >&2 || true
	for manifest in "${manifests[@]}"; do
		if ! cmp -s "$original_root/$manifest" "$sync_root/$manifest"; then
			diff -u "$original_root/$manifest" "$sync_root/$manifest" >&2 || true
		fi
	done
	exit 1
fi

for module_dir in "${module_dirs[@]}"; do
	module_path=$repo_root/$module_dir
	echo "workspace verification: $module_dir"
	(
		cd "$module_path"
		GOFLAGS=-mod=readonly GOWORK="$workspace" go mod download
		GOWORK="$workspace" go mod verify
		GOFLAGS=-mod=readonly GOWORK="$workspace" go test ./... -count=1
	)
done

for module_dir in "${module_dirs[@]}"; do
	module_path=$repo_root/$module_dir
	echo "standalone verification: $module_dir"
	(
		cd "$module_path"
		GOWORK=off go mod tidy -diff
		GOFLAGS=-mod=readonly GOWORK=off go mod download
		GOWORK=off go mod verify
		GOFLAGS=-mod=readonly GOWORK=off go test ./... -count=1
		GOFLAGS=-mod=readonly GOWORK=off "$repo_root/scripts/check-module-graph.sh" --no-replace "$module_path"
	)
done

after_root=$tmpdir/after-root
stage_manifests "$after_root"
snapshot_manifests "$after_root" "$tmpdir/after"
if ! cmp -s "$tmpdir/before" "$tmpdir/after"; then
	echo "module verification changed module or workspace manifests:" >&2
	diff -u "$tmpdir/before" "$tmpdir/after" >&2 || true
	exit 1
fi

echo "workspace and standalone module verification passed"
