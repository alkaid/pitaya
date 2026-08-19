#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
artifact_dir=$repo_root/build/verification/logrus
mkdir -p "$artifact_dir"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

run_rg_scan() {
	local output=$1
	shift
	if "$@" >"$output"; then
		return 0
	else
		local status=$?
		[[ $status -eq 1 ]] && return 0
		return "$status"
	fi
}

if run_rg_scan "$tmpdir/error-probe" rg -n 'probe' "$tmpdir/missing" >/dev/null 2>&1; then
	echo "Logrus scan error self-test unexpectedly passed" >&2
	exit 1
fi

if [[ -n $(find "$repo_root/logger/logrus" "$repo_root/logger/test" -type f -print -quit 2>/dev/null) ]]; then
	echo "removed first-party Logrus packages must not be restored" >&2
	exit 1
fi

source_hits=$tmpdir/source-hits
if (
	cd "$repo_root"
	run_rg_scan "$source_hits" rg --hidden -n -i \
		'github.com/sirupsen/logrus|github.com/alkaid/pitaya/v2/logger/(logrus|test)' \
		--iglob '*.go' --iglob '*.sh' --iglob '*.yaml' --iglob '*.yml' \
		--iglob '*dockerfile*' --iglob '!scripts/verify-logrus.sh' .
); then
	:
else
	status=$?
	echo "first-party Logrus source scan failed with status $status" >&2
	exit "$status"
fi
if [[ -s $source_hits ]]; then
	cat "$source_hits" >&2
	echo "first-party Logrus references remain" >&2
	exit 1
fi

doc_hits=$tmpdir/doc-hits
if (
	cd "$repo_root"
	run_rg_scan "$doc_hits" rg --hidden --with-filename --no-line-number -i \
		'logrus|logger/test' --iglob '*.md' --iglob '*.rst' .
); then
	:
else
	status=$?
	echo "first-party Logrus documentation scan failed with status $status" >&2
	exit "$status"
fi

expected_doc_hits=$tmpdir/expected-doc-hits
actual_doc_hits=$tmpdir/actual-doc-hits
printf '%s\n' \
	'./docs/logging.md:## Migrate from the first-party Logrus packages' \
	'./docs/logging.md:The deprecated `github.com/alkaid/pitaya/v2/logger/logrus` adapter and `github.com/alkaid/pitaya/v2/logger/test` helper have been removed. There is no compatibility shim and no general `pitaya.SetLogger` replacement.' \
	'./docs/logging.md:The client no longer accepts a Logrus level. Construct it with the current API and provide an explicit disconnect reason:' \
	'./docs/logging.md:`worker.SetLogger` remains a separate legacy integration that accepts `logger/interfaces.Logger`. Applications that still call it must own that adapter while migrating; Pitaya no longer provides a Logrus implementation for it.' \
	'./docs/migration-v2-ak.md:### 已删除的 Logrus 包' \
	'./docs/migration-v2-ak.md:`RegisterRPCJob` 与 `Stop` 现在串行。`Stop` 已开始或完成、且 Worker 尚未通过 `Start` 或 `StartWithError` 再次启动时，注册返回新增的 `constants.ErrWorkerStopped`。需要在同一进程重新使用 Worker 的代码必须先重新启动，再重新注册 RPC job；不能依赖上一个生命周期保留的 manager。`Worker.SetLogger` 仍保留，并通过稳定 facade 并发更新，不要求恢复已删除的第一方 Logrus adapter。' \
	'./docs/migration-v2-ak.md:服务端日志改用当前基于 Zap 的 `logger` API。client 包仍保留独立的 logger hook，但它不能替代服务端全局日志配置；需要继续使用 Logrus 的应用应自行维护 adapter。' \
	'./docs/migration-v2-ak.md:第一方 `logger/logrus` 适配器和 `logger/test` 测试辅助包已删除，不提供兼容层。' \
	'./docs/testing.md:| `logrus-verify` | Removed first-party Logrus packages do not reappear |' | \
	LC_ALL=C sort >"$expected_doc_hits"
LC_ALL=C sort "$doc_hits" >"$actual_doc_hits"
if ! cmp -s "$expected_doc_hits" "$actual_doc_hits"; then
	diff -u "$expected_doc_hits" "$actual_doc_hits" >&2 || true
	echo "Logrus documentation references differ from the reviewed set" >&2
	exit 1
fi

rg -q -F 'github.com/alkaid/pitaya/v2/logger/logrus' "$repo_root/docs/logging.md" || {
	echo "logging migration no longer documents the removed adapter" >&2
	exit 1
}
rg -q -F '`logger/logrus` 适配器' "$repo_root/docs/migration-v2-ak.md" || {
	echo "fork migration no longer documents the removed adapter" >&2
	exit 1
}
rg -q -F '`logrus-verify`' "$repo_root/docs/testing.md" || {
	echo "testing guide no longer documents the Logrus verification target" >&2
	exit 1
}

direct_logrus=$(
	cd "$repo_root"
	GOWORK=off go list -m -f '{{if and (eq .Path "github.com/sirupsen/logrus") (not .Indirect)}}{{.Path}}{{end}}' all
)
if [[ -n $direct_logrus ]]; then
	echo "root module directly requires github.com/sirupsen/logrus" >&2
	exit 1
fi

for module_dir in . pitaya-cli xk6-pitaya; do
	artifact_name=${module_dir//\//-}
	artifact_name=${artifact_name//./root}
	(
		cd "$repo_root/$module_dir"
		GOWORK=off go mod why -m github.com/sirupsen/logrus
	) >"$artifact_dir/$artifact_name.txt"
done

echo "first-party Logrus verification passed; dependency evidence: $artifact_dir"
