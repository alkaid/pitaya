#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
allowlist=$repo_root/scripts/verify-docs.allowlist
ledger=$repo_root/docs/upstream-ledger.md
declare -A allowed=()
declare -A expected=()
declare -A observed=()
declare -A used=()
upstream_reference_pattern='((https?://)?github\.com/topfreegames/|git@github\.com:topfreegames/)[[:alnum:]._/@?%:=+#&~-]+'
ledger_base=5230ff3dc79b461f55b6193bb9d0ae5c3d13869f
ledger_tip=45717a2f25acc3419b9f94ef1f9fbc4c14d13e7c
ledger_digest=f9412cd3993e95b9a8caf1701204befe880e7936fd5bdd746bbd40f35b843461
ledger_reference_count=0

fail() {
	echo "docs verification failed: $*" >&2
	exit 1
}

trim() {
	local value=$1
	value=${value#"${value%%[![:space:]]*}"}
	value=${value%"${value##*[![:space:]]}"}
	printf '%s' "$value"
}

scan_doc_lines() {
	local pattern=$1
	shift
	rg --with-filename -n -i "$pattern" -- "$@"
}

scan_doc_references() {
	local pattern=$1
	shift
	rg --with-filename --null -n -i -o "$pattern" -- "$@"
}

decode_docs() {
	local source_root=$1
	local destination=$2
	shift 2
	python3 - "$source_root" "$destination" "$@" <<'PY'
import html
from pathlib import Path
import sys

source_root = Path(sys.argv[1]).resolve(strict=True)
destination = Path(sys.argv[2])
for name in sys.argv[3:]:
    relative = Path(name)
    if relative.is_absolute() or ".." in relative.parts:
        raise ValueError(f"documentation path escapes repository: {name!r}")
    source = (source_root / relative).resolve(strict=True)
    source.relative_to(source_root)
    target = destination / relative
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(html.unescape(source.read_text(encoding="utf-8")), encoding="utf-8")
PY
}

is_allowed_upstream_reference() {
	local file=$1
	local line=$2
	local reference=$3
	local line_text=$4
	local key="$file|$reference"
	if [[ $reference != https://github.com/topfreegames/* ]]; then
		return 1
	fi
	if [[ $file == docs/upstream-ledger.md && $reference =~ ^https://github\.com/topfreegames/pitaya/commit/[0-9a-f]{40}$ ]]; then
		[[ $line_text == '| ['* ]] || return 1
		((ledger_reference_count += 1))
		return 0
	fi
	if [[ -n ${allowed[$key]:-} ]]; then
		[[ ${line_text,,} == *upstream* ]] || fail "$file:$line allowlisted URL lacks upstream context: $reference"
		used[$key]=1
		observed[$key]=$(( ${observed[$key]:-0} + 1 ))
		return 0
	fi
	return 1
}

while IFS='|' read -r file url count reason extra; do
	[[ -z $file || $file = \#* ]] && continue
	[[ -z ${extra:-} ]] || fail "invalid allowlist row: $file|$url|$count|$reason|$extra"
	[[ $url == https://github.com/topfreegames/* ]] || fail "non-upstream allowlist URL: $url"
	[[ $count =~ ^[1-9][0-9]*$ ]] || fail "invalid allowlist count: $file|$url|$count"
	[[ ${reason,,} == *upstream* ]] || fail "allowlist reason must say upstream: $file|$url"
	key="$file|$url"
	[[ -z ${allowed[$key]:-} ]] || fail "duplicate allowlist entry: $key"
	allowed[$key]=$reason
	expected[$key]=$count
done <"$allowlist"

for forbidden_reference in \
	'github.com/topfreegames/pitaya/component' \
	'github.com/topfreegames/pitaya/v2/component' \
	'github.com/topfreegames/pitaya@v2.11.24' \
	'https://github.com/topfreegames/pitaya.git' \
	'git@github.com:topfreegames/pitaya.git' \
	'http://github.com/topfreegames/pitaya' \
	'HTTPS://GITHUB.COM/TopFreeGames/Pitaya'; do
	detected=
	if detected=$(rg -i -o "$upstream_reference_pattern" <<<"$forbidden_reference"); then
		:
	else
		status=$?
		[[ $status -eq 1 ]] || fail "upstream reference detector errored for: $forbidden_reference"
	fi
	[[ -n ${detected:-} ]] || fail "upstream reference detector missed: $forbidden_reference"
	if is_allowed_upstream_reference docs/not-allowed.md 1 "$detected" 'upstream negative self-test'; then
		fail "upstream reference policy unexpectedly allowed: $forbidden_reference"
	fi
done

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

option_probe=$tmpdir/option-probe
mkdir -p "$option_probe"
printf '%s\n' 'doc filename option boundary sentinel' >"$option_probe/--pre=cat"
for scanner in scan_doc_lines scan_doc_references; do
	if ! (
		cd "$option_probe"
		"$scanner" 'doc filename option boundary sentinel' '--pre=cat' </dev/null >/dev/null
	); then
		fail "$scanner treated a documentation filename as an rg option"
	fi
done

doc_files=()
while IFS= read -r -d '' file; do
	[[ -f $repo_root/$file ]] || continue
	case ${file,,} in
		*.md | *.rst) doc_files+=("$file") ;;
	esac
done < <(git -C "$repo_root" ls-files -z --cached --others --exclude-standard)

((${#doc_files[@]} > 0)) || fail "no Markdown or RST files found"

entity_probe=$tmpdir/entity-probe
mkdir -p "$entity_probe/source"
printf '%s\n' \
	'[upstream](https://github&#46;com/topfreegames/pitaya)' \
	'[mail](mailto:security&#64;tfgco.com)' >"$entity_probe/source/probe.md"
decode_docs "$entity_probe/source" "$entity_probe/decoded" probe.md
rg -q -i "$upstream_reference_pattern" "$entity_probe/decoded/probe.md" || \
	fail "HTML entity decoding did not expose an upstream URL"
rg -q -i 'security@tfgco\.com' "$entity_probe/decoded/probe.md" || \
	fail "HTML entity decoding did not expose the former security address"

decoded_docs=$tmpdir/decoded-docs
decode_docs "$repo_root" "$decoded_docs" "${doc_files[@]}"

security_matches=$tmpdir/former-security-addresses
if (
	cd "$decoded_docs"
	scan_doc_lines 'security@tfgco\.com' "${doc_files[@]}"
) >"$security_matches"; then
	cat "$security_matches" >&2
	fail "documentation contains the former upstream security address"
else
	status=$?
	[[ $status -eq 1 ]] || fail "former security address scan failed with status $status"
fi

reference_matches=$tmpdir/upstream-references
if (
	cd "$decoded_docs"
	scan_doc_references "$upstream_reference_pattern" "${doc_files[@]}"
) >"$reference_matches"; then
	:
else
	status=$?
	[[ $status -eq 1 ]] || fail "upstream reference scan failed with status $status"
fi

while IFS= read -r -d '' file; do
	IFS=: read -r line reference || fail "malformed upstream reference record for: $file"
	[[ $line =~ ^[1-9][0-9]*$ ]] || fail "invalid upstream reference line for $file: $line"
	line_text=$(sed -n "${line}p" "$decoded_docs/$file")
	if ! is_allowed_upstream_reference "$file" "$line" "$reference" "$line_text"; then
		fail "$file:$line contains a forbidden or unallowlisted upstream reference: $reference"
	fi
done <"$reference_matches"

for key in "${!allowed[@]}"; do
	[[ -n ${used[$key]:-} ]] || fail "unused upstream URL allowlist entry: $key"
	[[ ${observed[$key]:-0} -eq ${expected[$key]} ]] || fail "upstream URL count mismatch for $key: found ${observed[$key]:-0}, expected ${expected[$key]}"
done

rg -q -F 'github.com/alkaid/pitaya/v2' "$repo_root/README.md" || fail "README lacks fork module identity"
rg -q -F 'Go 1.26.5' "$repo_root/README.md" || fail "README lacks exact Go baseline"
rg -q -F 'v2.11.1-ak-35' "$repo_root/README.md" || fail "README lacks verified release pin"
rg -q -F 'https://github.com/alkaid/pitaya/security/advisories/new' "$repo_root/README.md" || fail "README lacks private vulnerability reporting URL"

[[ -f $ledger ]] || fail "missing upstream ledger"
ledger_count=0
ledger_shas=$tmpdir/ledger-shas
: >"$ledger_shas"
declare -A ledger_seen=()
while IFS= read -r row; do
	[[ $row == '| ['* ]] || continue
	IFS='|' read -r _ commit_cell date category priority status assessment trailing <<<"$row"
	commit_cell=$(trim "$commit_cell")
	date=$(trim "$date")
	category=$(trim "$category")
	priority=$(trim "$priority")
	status=$(trim "$status")
	assessment=$(trim "$assessment")
	trailing=$(trim "$trailing")
	[[ -z $trailing ]] || fail "ledger row has extra columns: $row"
	if [[ ! $commit_cell =~ ^\[([0-9a-f]{40})\]\(https://github\.com/topfreegames/pitaya/commit/([0-9a-f]{40})\)$ ]]; then
		fail "malformed ledger commit cell: $commit_cell"
	fi
	sha=${BASH_REMATCH[1]}
	[[ $sha == "${BASH_REMATCH[2]}" ]] || fail "ledger link SHA mismatch: $commit_cell"
	[[ -z ${ledger_seen[$sha]:-} ]] || fail "duplicate ledger SHA: $sha"
	ledger_seen[$sha]=1
	[[ $date =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || fail "invalid ledger date for $sha: $date"
	[[ $category =~ ^(security|correctness|dependency|test|docs|feature|build/ci|maintenance|observability)$ ]] || fail "invalid ledger category for $sha: $category"
	[[ $priority =~ ^P[0-3]$ ]] || fail "invalid ledger priority for $sha: $priority"
	[[ $status =~ ^(unreviewed|reviewed-pending|blocked|in-progress|ported|cherry-picked|already-equivalent|rejected|covered-by-parents)$ ]] || fail "invalid ledger status for $sha: $status"
	case $status in
		already-equivalent|ported|cherry-picked)
			[[ $assessment == *fork-ref:* && $assessment == *verify:* ]] || fail "$status row lacks fork-ref/verify evidence: $sha"
			;;
		covered-by-parents)
			[[ $assessment == *parents:* ]] || fail "merge row lacks parents: $sha"
			;;
		blocked)
			[[ $assessment == *blocked-by:* ]] || fail "blocked row lacks blocker: $sha"
			;;
		reviewed-pending)
			[[ $assessment == *required:* ]] || fail "reviewed-pending row lacks required action: $sha"
			;;
		rejected)
			[[ $assessment == *reason:* ]] || fail "rejected row lacks reason: $sha"
			;;
	esac
	printf '%s\n' "$sha" >>"$ledger_shas"
	((ledger_count += 1))
done <"$ledger"

[[ $ledger_count -eq 70 ]] || fail "ledger contains $ledger_count commits; expected 70"
[[ $ledger_reference_count -eq $ledger_count ]] || fail "ledger contains $ledger_reference_count commit links for $ledger_count rows"
rg -q -F "$ledger_base" "$ledger" || fail "ledger lacks pinned merge base"
rg -q -F "$ledger_tip" "$ledger" || fail "ledger lacks pinned upstream tip"

actual_digest=$(sha256sum "$ledger_shas")
actual_digest=${actual_digest%% *}
[[ $actual_digest == "$ledger_digest" ]] || fail "ordered ledger SHA digest is $actual_digest; expected $ledger_digest"

if git -C "$repo_root" cat-file -e "$ledger_base^{commit}" 2>/dev/null && \
	git -C "$repo_root" cat-file -e "$ledger_tip^{commit}" 2>/dev/null; then
	live_shas=$tmpdir/live-shas
	git -C "$repo_root" rev-list --reverse "$ledger_base..$ledger_tip" >"$live_shas"
	if ! cmp -s "$ledger_shas" "$live_shas"; then
		diff -u "$live_shas" "$ledger_shas" >&2 || true
		fail "ledger does not match the pinned upstream commit range"
	fi
fi

echo "fork documentation verification passed"
