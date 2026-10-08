#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ $(protoc --version) != "libprotoc 36.2" ]]; then
    echo "protoc 36.2 is required; see docs/development.md" >&2
    exit 1
fi
repo_root=$PWD
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
for tool in protoc-gen-go protoc-gen-go-grpc protoc-gen-go-errors; do
    printf '#!/usr/bin/env bash\nexec env GOWORK=off go tool -modfile=%q %q "$@"\n' "$repo_root/tools/go.mod" "$tool" > "$tmpdir/$tool"
    chmod +x "$tmpdir/$tool"
done
export PATH="$tmpdir:$PATH"
if [[ ${1:-} == demo ]]; then
    for dir in examples/demo/protos examples/testing/protos examples/demo/cluster_protobuf/protos examples/demo/worker/protos examples/demo/cluster_ak/protos; do
        protoc -I "$dir" "$dir"/*.proto --go_out="$dir" --go_opt=paths=source_relative
    done
else
    protoc -I . -I pitaya-protos/lib/alkaid/goerrors pitaya-protos/*.proto --go_out=. --go-grpc_out=. --go-errors_out=.
    protoc -I pitaya-protos/test pitaya-protos/test/*.proto --go_out=protos/test --go_opt=paths=source_relative
    protoc -I benchmark/testdata benchmark/testdata/test.proto --go_out=benchmark/testdata --go_opt=paths=source_relative
fi
