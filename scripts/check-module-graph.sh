#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
reject_replacements=false
if [[ ${1:-} == --no-replace ]]; then
	reject_replacements=true
	shift
fi
module_dir=${1:-.}

required_modules=(
	github.com/alkaid/pitaya/v2
	github.com/alkaid/ants/v2
	github.com/alkaid/viper
	github.com/alkaid/crypt
)

forbidden_modules=(
	github.com/topfreegames/pitaya
	github.com/topfreegames/pitaya/v2
	github.com/panjf2000/ants/v2
	github.com/spf13/viper
	github.com/sagikazarmark/crypt
)

contains_module() {
	local graph_file=$1
	local module=$2
	awk -v module="$module" '$1 == module { found = 1 } END { exit !found }' "$graph_file"
}

check_graph() {
	local graph_file=$1
	local module
	if $reject_replacements && awk '$2 == "=>" { found = 1 } END { exit !found }' "$graph_file"; then
		echo "module graph contains a replacement" >&2
		return 1
	fi
	for module in "${required_modules[@]}"; do
		if ! contains_module "$graph_file" "$module"; then
			echo "required fork module missing: $module" >&2
			return 1
		fi
	done
	for module in "${forbidden_modules[@]}"; do
		if contains_module "$graph_file" "$module"; then
			echo "forbidden upstream module selected: $module" >&2
			return 1
		fi
	done
}

self_test() {
	local tmpdir=$1
	local index=0
	local module
	printf '%s\n' "${required_modules[@]}" >"$tmpdir/positive"
	check_graph "$tmpdir/positive"
	for module in "${forbidden_modules[@]}"; do
		cp "$tmpdir/positive" "$tmpdir/negative-$index"
		printf '%s\n' "$module" >>"$tmpdir/negative-$index"
		if check_graph "$tmpdir/negative-$index" >/dev/null 2>&1; then
			echo "module graph negative self-test unexpectedly passed: $module" >&2
			return 1
		fi
		((index += 1))
	done
	if $reject_replacements; then
		cp "$tmpdir/positive" "$tmpdir/replacement"
		printf '%s => %s\n' "${required_modules[0]}" ../ >>"$tmpdir/replacement"
		if check_graph "$tmpdir/replacement" >/dev/null 2>&1; then
			echo "module graph replacement self-test unexpectedly passed" >&2
			return 1
		fi
	fi
}

if [[ $module_dir = /* ]]; then
	module_path=$module_dir
else
	module_path=$repo_root/$module_dir
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
self_test "$tmpdir"

(
	cd "$module_path"
	go list -m -f '{{if .Replace}}{{.Path}} => {{.Replace.Path}}{{else}}{{.Path}}{{end}}' all
) >"$tmpdir/graph"

check_graph "$tmpdir/graph"
echo "module identity verification passed: ${module_path#"$repo_root"/}"
