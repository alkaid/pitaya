#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workspace=$repo_root/go.work
module_dirs=(. pitaya-cli xk6-pitaya)
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

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

stager=$tmpdir/stagecontext
(
	cd "$repo_root/tools"
	GOWORK=off go build -o "$stager" ./cmd/stagecontext
)

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
