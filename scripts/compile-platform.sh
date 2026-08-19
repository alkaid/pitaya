#!/usr/bin/env bash
set -euo pipefail

target_goos="${1:-}"
target_goarch="${2:-amd64}"

case "$target_goos" in
  darwin|windows) ;;
  *)
    echo "TARGET_GOOS must be darwin or windows" >&2
    exit 2
    ;;
esac

case "$target_goarch" in
  amd64|arm64) ;;
  *)
    echo "TARGET_GOARCH must be amd64 or arm64" >&2
    exit 2
    ;;
esac

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
runner="$(command -v true)"

for module in . pitaya-cli xk6-pitaya; do
  (
    cd "$repo_root/$module"
    GOWORK=off \
      CGO_ENABLED=0 \
      GOOS="$target_goos" \
      GOARCH="$target_goarch" \
      go test -exec="$runner" -run '^$' ./...
  )
done
