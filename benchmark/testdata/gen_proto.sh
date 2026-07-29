#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
protoc_gen_go=$(GOWORK=off go tool -modfile="$repo_root/tools/go.mod" -n protoc-gen-go)
protoc_bin=${PROTOC:-protoc}
protoc_version=$("$protoc_bin" --version 2>/dev/null) || {
	echo "protoc 35.1 is required" >&2
	exit 1
}
if [ "$protoc_version" != "libprotoc 35.1" ]; then
	echo "protoc 35.1 is required (found $protoc_version)" >&2
	exit 1
fi

cd "$script_dir"
"$protoc_bin" --plugin="protoc-gen-go=$protoc_gen_go" --go_out=. --go_opt=paths=source_relative ./*.proto
