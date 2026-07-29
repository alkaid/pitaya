#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
compose_file=$repo_root/examples/testing/docker-compose.yml
project_name="pitaya-e2e-${GITHUB_RUN_ID:-local}-$$"
tmpdir=$(mktemp -d)
server_binary=$tmpdir/server
compose=(docker compose --project-name "$project_name" --file "$compose_file")

cleanup() {
	local status=$?
	trap - EXIT
	if ((status != 0)); then
		"${compose[@]}" ps --all || true
		"${compose[@]}" logs --no-color --tail=200 || true
	fi
	"${compose[@]}" down --volumes --remove-orphans || true
	rm -rf "$tmpdir"
	exit "$status"
}
trap cleanup EXIT

(
	cd "$repo_root"
	GOWORK=off go build -trimpath -buildvcs=false -o "$server_binary" ./examples/testing
)

"${compose[@]}" up --detach --wait --wait-timeout 60

published_port() {
	local service=$1
	local container_port=$2
	local address
	address=$("${compose[@]}" port "$service" "$container_port")
	printf '%s\n' "${address##*:}"
}

nats_port=$(published_port nats 4222)
etcd_port=$(published_port etcd 2379)
redis_port=$(published_port redis 6379)

(
	cd "$repo_root"
	PITAYA_TEST_SERVER_BINARY="$server_binary" \
	PITAYA_TEST_NATS_URL="nats://127.0.0.1:$nats_port" \
	PITAYA_TEST_ETCD_URL="127.0.0.1:$etcd_port" \
	PITAYA_TEST_REDIS_URL="127.0.0.1:$redis_port" \
	GOWORK=off go test -tags=e2e ./e2e -count=1 -timeout=10m
)
