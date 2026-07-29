#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
artifact_dir=$repo_root/build/verification/logrus
mkdir -p "$artifact_dir"

if [[ -n $(find "$repo_root/logger/logrus" "$repo_root/logger/test" -type f -print -quit 2>/dev/null) ]]; then
	echo "removed first-party Logrus packages must not be restored" >&2
	exit 1
fi

mapfile -t source_hits < <(
	cd "$repo_root"
	rg -n -i 'github.com/sirupsen/logrus|github.com/alkaid/pitaya/v2/logger/(logrus|test)' \
		--glob '*.go' --glob '*.sh' --glob '*.yaml' --glob '*.yml' \
		--glob 'Dockerfile*' --glob '!scripts/verify-logrus.sh' . || true
)
if ((${#source_hits[@]})); then
	printf 'first-party Logrus references remain:\n%s\n' "${source_hits[*]}" >&2
	exit 1
fi

mapfile -t doc_hits < <(
	cd "$repo_root"
	rg -n -i 'logrus|logger/test' --glob '*.md' --glob '!docs/logging.md' . || true
)
if ((${#doc_hits[@]})); then
	printf 'undocumented Logrus references remain:\n%s\n' "${doc_hits[*]}" >&2
	exit 1
fi

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
