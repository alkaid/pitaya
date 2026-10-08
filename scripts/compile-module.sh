#!/usr/bin/env bash
# Compile each package without running TestMain or starting test services.
set -euo pipefail
cd "$(dirname "$0")/.."
cd "${1:-.}"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
go list ./... > "$tmpdir/packages"
while IFS= read -r package; do
    go test -mod=readonly -c -o "$tmpdir/package.test" "$package"
done < "$tmpdir/packages"
