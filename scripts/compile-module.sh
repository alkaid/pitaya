#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
module_dir=${1:-.}

if [[ $module_dir = /* ]]; then
	module_path=$module_dir
else
	module_path=$repo_root/$module_dir
fi

if [[ ! -f $module_path/go.mod ]]; then
	echo "module not found: $module_path" >&2
	exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

(
	cd "$module_path"
	go list ./... >"$tmpdir/packages"
	while IFS= read -r package; do
		go test -c -o "$tmpdir/package.test" "$package"
	done <"$tmpdir/packages"
)

echo "compile-only verification passed: ${module_path#"$repo_root"/}"
