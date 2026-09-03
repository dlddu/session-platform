#!/usr/bin/env bash
# check-ac-mapping.sh — enforce the AC ↔ e2e 1:1 mapping mechanically.
#
# The rules it checks live in docs/test/e2e.md ("AC ↔ e2e 매핑 규칙"):
#   1. AC → file: every AC that is not on the exception list is declared by
#      exactly one matching-unit file.
#   2. file → AC: every matching-unit file declares exactly one AC (or `없음`
#      for a registered smoke/infra file).
#   3. non-AC files must be registered in docs/test/e2e.md.
#   4. the exception list may only name ACs that exist in docs/prd.
#   5. no declaration may reference an AC that does not exist.
#   6. the registry's mapping rows, non-AC rows and totals must match reality.
#
# Matching units (everything else — the shared harness, web/e2e/journeys/**,
# integration/unit/envtest suites — is deliberately outside):
#   - control-plane/test/e2e_*_test.go      (Go API e2e, build tag `e2e`)
#   - web/e2e/*.spec.ts                     (Playwright, top level only)
#
# Sources of truth: docs/prd/*.md `### AC-...` headings for the AC set, the
# `// 검증 AC:` header of each matching file for the mapping, docs/test/e2e.md
# for the registry (exceptions, non-AC files, totals).
#
# Runs anywhere — no cluster, no toolchain beyond coreutils.
set -euo pipefail

cd "$(dirname "$0")/../.."
REGISTRY=docs/test/e2e.md

fail=0
err() {
	printf 'FAIL: %s\n' "$1" >&2
	fail=1
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# ---------------------------------------------------------------- AC set (PRD)
grep -rhoE '^### (AC-[A-Z][0-9]+):' docs/prd/*.md | sed -E 's/^### (AC-[A-Z][0-9]+):.*/\1/' | sort -u >"$tmp/acs"
ac_total=$(wc -l <"$tmp/acs" | tr -d ' ')
if [ "$ac_total" -eq 0 ]; then
	err "docs/prd에서 AC 헤딩(### AC-...)을 하나도 찾지 못했다"
	exit 1
fi

# ------------------------------------------------- declarations (matching units)
: >"$tmp/decl"    # "<AC> <path>"
: >"$tmp/nonac"   # "<path>"
matching=0
for f in control-plane/test/e2e_*_test.go web/e2e/*.spec.ts; do
	[ -e "$f" ] || continue
	matching=$((matching + 1))
	n=$(grep -cE '^//[[:space:]]*검증 AC:' "$f" || true)
	if [ "$n" -eq 0 ]; then
		err "$f: '// 검증 AC:' 선언이 없다 (매칭 단위는 정확히 1개를 선언해야 한다)"
		continue
	fi
	if [ "$n" -gt 1 ]; then
		err "$f: '// 검증 AC:' 선언이 $n개다 (파일당 정확히 1개, 규칙 2)"
		continue
	fi
	value=$(grep -m1 -E '^//[[:space:]]*검증 AC:' "$f" | sed -E 's|^//[[:space:]]*검증 AC:[[:space:]]*||')
	case "$value" in
	없음*)
		printf '%s\n' "$f" >>"$tmp/nonac"
		;;
	AC-*)
		ac=$(printf '%s' "$value" | grep -oE '^AC-[A-Z][0-9]+')
		rest=$(printf '%s' "$value" | sed -E 's/^AC-[A-Z][0-9]+[[:space:]]*//')
		if [ -n "$rest" ]; then
			err "$f: 선언이 '$value' — AC 식별자 하나만 적는다 (규칙 2)"
			continue
		fi
		if ! grep -qx "$ac" "$tmp/acs"; then
			err "$f: 존재하지 않는 $ac 를 선언한다 (규칙 5 참조 무결성)"
			continue
		fi
		printf '%s %s\n' "$ac" "$f" >>"$tmp/decl"
		;;
	*)
		err "$f: 선언값 '$value' 를 해석할 수 없다 (AC-<계열><번호> 또는 '없음 (…)')"
		;;
	esac
done

# rule 1: no AC declared by two files
awk '{print $1}' "$tmp/decl" | sort | uniq -d >"$tmp/dups"
while read -r ac; do
	[ -n "$ac" ] || continue
	err "$ac 를 선언한 파일이 여러 개다: $(awk -v a="$ac" '$1==a{printf "%s ", $2}' "$tmp/decl")(규칙 1)"
done <"$tmp/dups"

awk '{print $1}' "$tmp/decl" | sort -u >"$tmp/declared"

# ------------------------------------------------------- registry blocks (docs)
block() { # block <marker> -> the lines between <!-- marker:begin --> / :end
	awk -v m="$1" '
		$0 ~ "<!-- " m ":begin -->" { on = 1; next }
		$0 ~ "<!-- " m ":end -->"   { on = 0 }
		on
	' "$REGISTRY"
}
for m in ac-mapping ac-exceptions ac-nonac ac-summary; do
	if ! grep -q "<!-- $m:begin -->" "$REGISTRY" || ! grep -q "<!-- $m:end -->" "$REGISTRY"; then
		err "$REGISTRY 에 <!-- $m:begin --> / <!-- $m:end --> 블록이 없다 (규칙 6)"
	fi
done
[ "$fail" -eq 0 ] || exit 1

# exceptions: first cell of each table row
block ac-exceptions | grep -E '^\| *AC-' | sed -E 's/^\| *(AC-[A-Z][0-9]+).*/\1/' | sort -u >"$tmp/exceptions"
while read -r ac; do
	[ -n "$ac" ] || continue
	grep -qx "$ac" "$tmp/acs" || err "예외 목록의 $ac 는 docs/prd에 없는 AC다 (규칙 5)"
	grep -qx "$ac" "$tmp/declared" && err "$ac 는 예외로 등재됐는데 선언 파일도 있다 — 둘 중 하나만 (규칙 4)"
done <"$tmp/exceptions"

# mapping rows: "| AC-X | path |"
block ac-mapping | grep -E '^\| *AC-' |
	sed -E 's/^\| *(AC-[A-Z][0-9]+) *\| *`?([^`|]+)`? *\|.*/\1 \2/' | sed -E 's/ +$//' | sort >"$tmp/doc_map"
sort "$tmp/decl" >"$tmp/real_map"
if ! diff -u "$tmp/doc_map" "$tmp/real_map" >"$tmp/map_diff"; then
	err "$REGISTRY 의 매핑 표가 실제 선언과 다르다 (규칙 6):"
	sed 's/^/       /' "$tmp/map_diff" >&2
fi

# non-AC rows
block ac-nonac | grep -E '^\| *`?(control-plane|web)/' |
	sed -E 's/^\| *`?([^`|]+)`? *\|.*/\1/' | sed -E 's/ +$//' | sort >"$tmp/doc_nonac"
sort "$tmp/nonac" >"$tmp/real_nonac"
if ! diff -u "$tmp/doc_nonac" "$tmp/real_nonac" >"$tmp/nonac_diff"; then
	err "$REGISTRY 의 비-AC 등재가 실제와 다르다 (규칙 3·6):"
	sed 's/^/       /' "$tmp/nonac_diff" >&2
fi

# ------------------------------------------------------------------- aggregate
exc_total=$(grep -c . "$tmp/exceptions" || true)
decl_total=$(grep -c . "$tmp/declared" || true)
comm -23 "$tmp/acs" <(sort -u "$tmp/declared" "$tmp/exceptions") >"$tmp/gaps"
gap_total=$(grep -c . "$tmp/gaps" || true)
gap_list=$(tr '\n' ' ' <"$tmp/gaps" | sed -E 's/ +$//')

summary=$(block ac-summary)
check_num() { # check_num <label> <expected>
	got=$(printf '%s\n' "$summary" | sed -nE "s/^- $1: *([0-9]+).*/\1/p" | head -1)
	if [ -z "$got" ]; then
		err "$REGISTRY 집계에 '- $1: <숫자>' 줄이 없다 (규칙 6)"
	elif [ "$got" != "$2" ]; then
		err "$REGISTRY 집계 '$1' = $got, 실제 = $2 (규칙 6)"
	fi
}
check_num "AC 총계" "$ac_total"
check_num "예외" "$exc_total"
check_num "AC 매칭 파일" "$decl_total"
check_num "공백" "$gap_total"

doc_gaps=$(printf '%s\n' "$summary" | sed -nE 's/^- 공백: *[0-9]+ *— *(.*)$/\1/p' | head -1 | sed -E 's/ +$//')
if [ "$gap_total" -gt 0 ] && [ "$doc_gaps" != "$gap_list" ]; then
	err "$REGISTRY 집계의 공백 목록 '$doc_gaps' ≠ 실제 '$gap_list' (규칙 6)"
fi

# --------------------------------------------------------------------- verdict
if [ "$fail" -ne 0 ]; then
	printf '\nAC ↔ e2e 1:1 매핑 위반이 있다. 위 항목을 고치거나 docs/test/e2e.md 등재를 갱신할 것.\n' >&2
	exit 1
fi

printf 'AC ↔ e2e 1:1 OK — AC %s개 = 매칭 파일 %s + 예외 %s + 공백 %s (매칭 단위 %s개, 비-AC %s개)\n' \
	"$ac_total" "$decl_total" "$exc_total" "$gap_total" "$matching" "$(grep -c . "$tmp/nonac" || true)"
[ "$gap_total" -eq 0 ] || printf '공백(전용 파일도 예외 등재도 없는 AC): %s\n' "$gap_list"
