#!/usr/bin/env bash
# Check each nested module against its published dependencies, then verify the
# current root using a temporary replacement without changing the manifest.
set -euo pipefail
cd "$(dirname "$0")/.."
repo_root=$PWD
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
export GOWORK=off
for module in pitaya-cli xk6-pitaya; do
    (
        cd "$module"
        go test -mod=readonly ./...
        go vet -mod=readonly ./...

        modfile="$tmpdir/$module.mod"
        cp go.mod "$modfile"
        cp go.sum "$tmpdir/$module.sum"
        go mod edit -modfile="$modfile" -replace="github.com/alkaid/pitaya/v2=$repo_root"
        go mod tidy -modfile="$modfile"
        go test -mod=readonly -modfile="$modfile" ./...
        go vet -mod=readonly -modfile="$modfile" ./...
    )
done
(cd tools && go mod tidy -diff && go build -mod=readonly tool)
