#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output_dir=${PITAYA_VULN_OUTPUT_DIR:-$repo_root/build/govulncheck}
tool_mod=$repo_root/tools/go.mod
modules=(
	"root:."
	"pitaya-cli:pitaya-cli"
	"xk6-pitaya:xk6-pitaya"
)
status=0
checker_dir=$(mktemp -d)
trap 'rm -rf "$checker_dir"' EXIT

GOWORK=off go -C "$repo_root/tools" build -o "$checker_dir/vulnreport" ./cmd/vulnreport

mkdir -p "$output_dir"

for module in "${modules[@]}"; do
	name=${module%%:*}
	directory=${module#*:}
	output=$output_dir/$name.json
	echo "scanning module: $name"
	attempt=1
	scan_status=0
	while ((attempt <= 3)); do
		scan_status=0
		(
			cd "$repo_root"
			GOWORK=off go tool -modfile="$tool_mod" govulncheck \
				-C "$directory" -format=json ./...
		) >"$output" || scan_status=$?
		if ((scan_status != 1)); then
			break
		fi
		if ((attempt < 3)); then
			echo "govulncheck failed for $name; retrying ($attempt/3)" >&2
		fi
		attempt=$((attempt + 1))
	done
	if ((scan_status != 0 && scan_status != 3)); then
		status=1
	fi
	if [[ ! -s $output ]]; then
		echo "govulncheck produced no report for $name" >&2
		status=1
		continue
	fi
	report_status=0
	"$checker_dir/vulnreport" "$output" || report_status=$?
	if ((report_status == 3)); then
		if ((status == 0)); then
			status=3
		fi
	elif ((report_status != 0)); then
		status=1
	fi
done

echo "govulncheck JSON reports: $output_dir"
exit "$status"
