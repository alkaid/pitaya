#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

(
	cd "$tmpdir"
	GOWORK=off go mod init example.com/pitaya-consumer
	GOWORK=off go mod edit -go=1.26.5
	GOWORK=off go mod edit -require=github.com/alkaid/pitaya/v2@v2.11.1-ak-35
	GOWORK=off go mod edit -replace=github.com/alkaid/pitaya/v2="$repo_root"
)

cat >"$tmpdir/consumer_test.go" <<'EOF'
package consumer_test

import (
	pitaya "github.com/alkaid/pitaya/v2"
	"github.com/alkaid/pitaya/v2/client"
	"github.com/alkaid/pitaya/v2/config"
)

var (
	_ *pitaya.App
	_ client.PitayaClient
	_ *config.Config
)
EOF

(
	cd "$tmpdir"
	GOWORK=off go mod tidy
	GOWORK=off go test ./... -count=1
	GOWORK=off "$repo_root/scripts/check-module-graph.sh" "$tmpdir"
)

echo "downstream consumer verification passed"
