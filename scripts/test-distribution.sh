#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
root_version=v2.99.0-batch1d.0
cli_version=v2.99.0-batch1d.0
xk6_version=v0.99.0-batch1d.0
root_module=github.com/alkaid/pitaya/v2
cli_module=github.com/alkaid/pitaya/pitaya-cli/v2
xk6_module=github.com/alkaid/pitaya/xk6-pitaya

tmpdir=$(mktemp -d)
image="pitaya-xk6-distribution:batch1d-$$"
cleanup() {
	docker image rm -f "$image" >/dev/null 2>&1 || true
	if [[ -d $tmpdir/modcache ]]; then
		GOWORK=off GOMODCACHE="$tmpdir/modcache" go clean -modcache >/dev/null 2>&1 || \
			chmod -R u+w "$tmpdir/modcache"
	fi
	rm -rf "$tmpdir"
}
trap cleanup EXIT

proxy_dir=$tmpdir/proxy
mkdir -p "$proxy_dir"
(
	cd "$repo_root/tools"
	GOWORK=off go run ./cmd/localmodproxy \
		-repo "$repo_root" \
		-out "$proxy_dir" \
		-root-version "$root_version" \
		-cli-version "$cli_version" \
		-xk6-version "$xk6_version"
)

export GOWORK=off
export GOPROXY="file://$proxy_dir|https://goproxy.cn|https://proxy.golang.org|direct"
export GOSUMDB=sum.golang.org
export GONOSUMDB="github.com/alkaid/pitaya,github.com/alkaid/pitaya/*"
export GOMODCACHE=$tmpdir/modcache
export GOCACHE=$tmpdir/gocache
export GOBIN=$tmpdir/bin
mkdir -p "$GOMODCACHE" "$GOCACHE" "$GOBIN"

verify_proxy_graph() {
	local module=$1
	local version=$2
	local name=$3
	local graph_dir=$tmpdir/graph-$name
	mkdir -p "$graph_dir"
	(
		cd "$graph_dir"
		go mod init "example.com/distribution-$name"
		go mod edit -go=1.26.5
		go mod edit -require="$module@$version"
		go mod download "$module@$version"
		"$repo_root/scripts/check-module-graph.sh" --no-replace "$graph_dir"
	)
}

verify_proxy_graph "$root_module" "$root_version" root
verify_proxy_graph "$cli_module" "$cli_version" cli
verify_proxy_graph "$xk6_module" "$xk6_version" xk6

go install "$cli_module@$cli_version"
"$GOBIN/pitaya-cli" --help >/dev/null
"$GOBIN/pitaya-cli" --filename /dev/null
go version -m "$GOBIN/pitaya-cli" >"$tmpdir/cli-buildinfo"
awk -v path="$cli_module" -v version="$cli_version" \
	'$1 == "mod" && $2 == path && $3 == version { found = 1 } END { exit !found }' \
	"$tmpdir/cli-buildinfo"
awk -v path="$root_module" -v version="$root_version" \
	'$1 == "dep" && $2 == path && $3 == version { found = 1 } END { exit !found }' \
	"$tmpdir/cli-buildinfo"
if rg -n '=>|github.com/topfreegames/pitaya/v2' "$tmpdir/cli-buildinfo"; then
	echo "installed CLI contains a replacement or upstream Pitaya" >&2
	exit 1
fi

go run go.k6.io/xk6/cmd/xk6@v1.4.7 build v1.8.0 \
	--build-flags=-trimpath \
	--build-flags="-ldflags=-s -w" \
	--build-flags=-buildvcs=false \
	--with "$xk6_module@$xk6_version" \
	--output "$tmpdir/k6"

"$tmpdir/k6" version >"$tmpdir/k6-version"
rg -q -F "$xk6_module $xk6_version" "$tmpdir/k6-version"
rg -q -F 'k6/x/pitaya' "$tmpdir/k6-version"

cat >"$tmpdir/smoke.js" <<'EOF'
import pitaya from 'k6/x/pitaya';
import { check } from 'k6';

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: { checks: ['rate==1'] },
};

export default function () {
  const client = new pitaya.Client({ requestTimeoutMs: 100 });
  check(client, { 'pitaya Client export constructs': (value) => value !== null });
}
EOF

"$tmpdir/k6" inspect "$tmpdir/smoke.js" >/dev/null
K6_NO_USAGE_REPORT=true "$tmpdir/k6" run --quiet "$tmpdir/smoke.js"

expected_revision=$("$repo_root/scripts/build-xk6-image.sh" --print-revision)
"$repo_root/scripts/build-xk6-image.sh" "$image"
actual_revision=$(docker image inspect \
	--format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$image")
if [[ $actual_revision != "$expected_revision" ]]; then
	echo "Docker image revision $actual_revision does not match source $expected_revision" >&2
	exit 1
fi
docker run --rm "$image" version >"$tmpdir/docker-k6-version"
rg -q -F 'k6/x/pitaya' "$tmpdir/docker-k6-version"
docker run --rm -v "$tmpdir/smoke.js:/scripts/smoke.js:ro" \
	-e K6_NO_USAGE_REPORT=true "$image" run --quiet /scripts/smoke.js

echo "versioned CLI, xk6, and Docker distribution verification passed"
