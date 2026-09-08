#!/usr/bin/env bash
# check-scenario-mapping.sh — enforce the 테스트 시나리오 ↔ e2e 1:1 mapping mechanically.
#
# The rules it checks live in docs/test/e2e.md ("시나리오 ↔ e2e 매핑 규칙"):
#   1. scenario → file: every scenario that is on none of the three registration
#      tables (exceptions, pending implementation, authoring backlog) is declared
#      by exactly one matching-unit file.
#   2. file → scenario: every matching-unit file declares exactly one existing
#      scenario (or `없음` for a registered smoke/infra file).
#   3. non-scenario files must be registered in docs/test/e2e.md.
#   4. the three registration tables may only name scenarios that exist, none may
#      name a scenario that also has a declaring file, and no scenario may sit in
#      two of them at once.
#   5. no declaration may reference a scenario that does not exist.
#   6. the registry's mapping rows, non-scenario rows and totals must match
#      reality.
#   7. a scenario that is implemented and not excepted but has no file yet goes
#      on the authoring backlog with its suite, its prerequisite and its evidence.
#   8. no unclassified gap: a scenario with neither a file nor a row in one of
#      the three tables fails the run. A gap is a judgement someone has to write
#      down, not a counter that may quietly grow — 2026-09-08 축 교체 left 16 of
#      them standing under a green run, which is what this rule exists to stop.
#      Escape hatch for a new scenario: one row in 「저작 대기」 saying which suite
#      will own it and what has to land first.
#
# Matching units (everything else — the shared harness, web/e2e/journeys/**,
# integration/unit/envtest suites — is deliberately outside):
#   - control-plane/test/e2e_*_test.go      (Go API e2e, build tag `e2e`)
#   - web/e2e/*.spec.ts                     (Playwright, top level only)
#
# Sources of truth: the `### 시나리오 N: …` headings of docs/test/*.md (the
# registry docs/test/e2e.md itself excluded) for the scenario set, the
# `// 검증 시나리오:` header of each matching file for the mapping, docs/test/e2e.md
# for the registry (exceptions, pending, backlog, non-scenario files, totals).
#
# Scenario identifier = `<문서 파일명>#시나리오 <N>` where `<N>` is the heading's own
# number token — including sub-numbers such as `2-1`, which really occur
# (approval-gated-workload.md, architecture.md). Copy it from the heading; do not
# renumber.
#
# Runs anywhere — no cluster, no toolchain beyond coreutils.
set -euo pipefail
export LC_ALL=C

cd "$(dirname "$0")/../.."
REGISTRY=docs/test/e2e.md

fail=0
err() {
	printf 'FAIL: %s\n' "$1" >&2
	fail=1
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# ------------------------------------------------- scenario set (docs/test/*.md)
: >"$tmp/scenarios"
for doc in docs/test/*.md; do
	[ -e "$doc" ] || continue
	[ "$doc" = "$REGISTRY" ] && continue
	base=$(basename "$doc")
	# `### 시나리오 <N>: …` — <N> is `2` or `2-1`. The colon is required so prose
	# mentioning a scenario in a heading cannot enter the set.
	grep -oE '^### 시나리오 [0-9]+(-[0-9]+)?:' "$doc" |
		sed -E "s|^### 시나리오 ([0-9]+(-[0-9]+)?):|$base#시나리오 \1|" >>"$tmp/scenarios"
done
sort -u "$tmp/scenarios" -o "$tmp/scenarios"
sc_total=$(grep -c . "$tmp/scenarios" || true)
if [ "$sc_total" -eq 0 ]; then
	err "docs/test 에서 시나리오 헤딩(### 시나리오 N:)을 하나도 찾지 못했다"
	exit 1
fi

# ------------------------------------------------- declarations (matching units)
: >"$tmp/decl"   # "<path>\t<scenario id>"
: >"$tmp/nonsc"  # "<path>"
matching=0
for f in control-plane/test/e2e_*_test.go web/e2e/*.spec.ts; do
	[ -e "$f" ] || continue
	matching=$((matching + 1))
	n=$(grep -cE '^//[[:space:]]*검증 시나리오:' "$f" || true)
	if [ "$n" -eq 0 ]; then
		err "$f: '// 검증 시나리오:' 선언이 없다 (매칭 단위는 정확히 1개를 선언해야 한다)"
		continue
	fi
	if [ "$n" -gt 1 ]; then
		err "$f: '// 검증 시나리오:' 선언이 $n개다 (파일당 정확히 1개, 규칙 2)"
		continue
	fi
	value=$(grep -m1 -E '^//[[:space:]]*검증 시나리오:' "$f" | sed -E 's|^//[[:space:]]*검증 시나리오:[[:space:]]*||' | sed -E 's/[[:space:]]+$//')
	case "$value" in
	없음*)
		printf '%s\n' "$f" >>"$tmp/nonsc"
		;;
	*'#시나리오 '*)
		if ! grep -qxF "$value" "$tmp/scenarios"; then
			err "$f: 존재하지 않는 시나리오 '$value' 를 선언한다 (규칙 5 참조 무결성)"
			continue
		fi
		printf '%s\t%s\n' "$f" "$value" >>"$tmp/decl"
		;;
	*)
		err "$f: 선언값 '$value' 를 해석할 수 없다 (<문서 파일명>#시나리오 <N> 또는 '없음 (…)')"
		;;
	esac
done

# rule 1: no scenario declared by two files
{ cut -f2 "$tmp/decl" | sort | uniq -d || true; } >"$tmp/dups"
while read -r sc; do
	[ -n "$sc" ] || continue
	err "'$sc' 를 선언한 파일이 여러 개다: $(awk -F'\t' -v s="$sc" '$2==s{printf "%s ", $1}' "$tmp/decl")(규칙 1)"
done <"$tmp/dups"

cut -f2 "$tmp/decl" | sort -u >"$tmp/declared"

# ------------------------------------------------------- registry blocks (docs)
block() { # block <marker> -> the lines between <!-- marker:begin --> / :end
	awk -v m="$1" '
		$0 ~ "<!-- " m ":begin -->" { on = 1; next }
		$0 ~ "<!-- " m ":end -->"   { on = 0 }
		on
	' "$REGISTRY"
}
for m in scenario-mapping scenario-exceptions scenario-pending scenario-backlog scenario-nonscenario scenario-summary; do
	if ! grep -q "<!-- $m:begin -->" "$REGISTRY" || ! grep -q "<!-- $m:end -->" "$REGISTRY"; then
		err "$REGISTRY 에 <!-- $m:begin --> / <!-- $m:end --> 블록이 없다 (규칙 6)"
	fi
done
[ "$fail" -eq 0 ] || exit 1

# first backticked cell of each table row -> one id/path per line. `|| true`
# everywhere a table may legitimately be empty (구현 대기 표) — a matchless grep
# returns 1 and `set -e` would kill the run before any FAIL is printed.
first_cell() { { grep -E '^\| *`[^`]+`' || true; } | sed -E 's/^\| *`([^`]+)`.*/\1/'; }

# the three registration tables: registered scenarios must exist and must not
# also be declared by a file.
for kind in exceptions pending backlog; do
	block "scenario-$kind" | first_cell | sort -u >"$tmp/$kind"
	while read -r sc; do
		[ -n "$sc" ] || continue
		grep -qxF "$sc" "$tmp/scenarios" || err "$kind 표의 '$sc' 는 docs/test 에 없는 시나리오다 (규칙 5)"
		if grep -qxF "$sc" "$tmp/declared"; then
			err "'$sc' 가 $kind 로 등재됐는데 선언 파일도 있다 — 둘 중 하나만 (규칙 4)"
		fi
	done <"$tmp/$kind"
done
# …and no scenario may sit in two of them. The three answer different questions
# (영구 면제 / 구현이 없다 / 파일이 없다) and a scenario has one answer.
overlap() { # overlap <a> <b> <말>
	comm -12 "$tmp/$1" "$tmp/$2" >"$tmp/both"
	while read -r sc; do
		[ -n "$sc" ] || continue
		err "'$sc' 가 $3에 모두 있다 — 한 시나리오의 판정은 하나다 (규칙 4·6)"
	done <"$tmp/both"
}
overlap exceptions pending "예외 목록과 구현 대기 표"
overlap exceptions backlog "예외 목록과 저작 대기 표"
overlap pending backlog "구현 대기 표와 저작 대기 표"

# mapping rows: "| `<scenario>` | `<path>` | …"  ->  "<path>\t<scenario>"
{ block scenario-mapping | grep -E '^\| *`[^`]+` *\| *`[^`]+`' || true; } |
	sed -E 's/^\| *`([^`]+)` *\| *`([^`]+)`.*/\2\t\1/' | sort >"$tmp/doc_map"
sort "$tmp/decl" >"$tmp/real_map"
if ! diff -u "$tmp/doc_map" "$tmp/real_map" >"$tmp/map_diff"; then
	err "$REGISTRY 의 매핑 표가 실제 선언과 다르다 (규칙 6):"
	sed 's/^/       /' "$tmp/map_diff" >&2
fi

# non-scenario rows
block scenario-nonscenario | first_cell | sort >"$tmp/doc_nonsc"
sort "$tmp/nonsc" >"$tmp/real_nonsc"
if ! diff -u "$tmp/doc_nonsc" "$tmp/real_nonsc" >"$tmp/nonsc_diff"; then
	err "$REGISTRY 의 비-시나리오 등재가 실제와 다르다 (규칙 3·6):"
	sed 's/^/       /' "$tmp/nonsc_diff" >&2
fi

# ------------------------------------------------------------------- aggregate
exc_total=$(grep -c . "$tmp/exceptions" || true)
pend_total=$(grep -c . "$tmp/pending" || true)
back_total=$(grep -c . "$tmp/backlog" || true)
decl_total=$(grep -c . "$tmp/declared" || true)
sort -u "$tmp/declared" "$tmp/exceptions" "$tmp/pending" "$tmp/backlog" >"$tmp/covered"
comm -23 "$tmp/scenarios" "$tmp/covered" >"$tmp/gaps"
gap_total=$(grep -c . "$tmp/gaps" || true)
# ` · ` joined — scenario ids contain spaces, so a space-joined list is ambiguous.
gap_list=$(paste -sd'\n' "$tmp/gaps" | sed -e ':a' -e 'N' -e '$!ba' -e 's/\n/ · /g')

summary=$(block scenario-summary)
check_num() { # check_num <label> <expected>
	got=$(printf '%s\n' "$summary" | sed -nE "s/^- $1: *([0-9]+).*/\1/p" | head -1)
	if [ -z "$got" ]; then
		err "$REGISTRY 집계에 '- $1: <숫자>' 줄이 없다 (규칙 6)"
	elif [ "$got" != "$2" ]; then
		err "$REGISTRY 집계 '$1' = $got, 실제 = $2 (규칙 6)"
	fi
}
check_num "시나리오 총계" "$sc_total"
check_num "예외" "$exc_total"
check_num "구현 대기" "$pend_total"
check_num "저작 대기" "$back_total"
check_num "시나리오 매칭 파일" "$decl_total"
check_num "공백" "$gap_total"

doc_gaps=$(printf '%s\n' "$summary" | sed -nE 's/^- 공백: *[0-9]+ *— *(.*)$/\1/p' | head -1 | sed -E 's/ +$//')
if [ "$gap_total" -gt 0 ] && [ "$doc_gaps" != "$gap_list" ]; then
	err "$REGISTRY 집계의 공백 목록 '$doc_gaps' ≠ 실제 '$gap_list' (규칙 6)"
fi

# 규칙 8 — 미분류 공백은 실패다. 파일도 없고 세 표 어디에도 없는 시나리오는 "아직 판정하지
# 않았다"는 뜻이고, 그것이 초록 아래 조용히 쌓이는 것을 막는 것이 이 검사다. 새 시나리오를
# 추가하는 PR 이 붉어지면 고치는 법은 한 줄이다 — 「저작 대기」에 스위트와 선행을 적어 넣는 것.
if [ "$gap_total" -gt 0 ]; then
	err "미분류 공백 $gap_total개 — 파일도 없고 예외·구현 대기·저작 대기 어디에도 없다 (규칙 8): $gap_list"
fi

# 불변식 — (시나리오 − 예외 − 구현 대기 − 저작 대기) = 매칭 파일 + 공백. 규칙 8 이 공백을 0 으로
# 묶으므로 실질은 좌변 = 매칭 파일이고, 저작 대기가 0 이 되는 날 완전 1:1 이 된다. 이 줄이
# 어긋나면 위의 개별 검사 중 하나가 이미 붉다.
if [ $((sc_total - exc_total - pend_total - back_total)) -ne $((decl_total + gap_total)) ]; then
	err "불변식 위반: (시나리오 $sc_total − 예외 $exc_total − 구현 대기 $pend_total − 저작 대기 $back_total) ≠ 매칭 파일 $decl_total + 공백 $gap_total"
fi

# --------------------------------------------------------------------- verdict
if [ "$fail" -ne 0 ]; then
	printf '\n시나리오 ↔ e2e 1:1 매핑 위반이 있다. 위 항목을 고치거나 docs/test/e2e.md 등재를 갱신할 것.\n' >&2
	exit 1
fi

printf '시나리오 ↔ e2e 1:1 OK — 시나리오 %s개 = 매칭 파일 %s + 예외 %s + 구현 대기 %s + 저작 대기 %s + 공백 %s (매칭 단위 %s개, 비-시나리오 %s개)\n' \
	"$sc_total" "$decl_total" "$exc_total" "$pend_total" "$back_total" "$gap_total" "$matching" "$(grep -c . "$tmp/nonsc" || true)"
# 규칙 8 이 위에서 공백을 0 으로 묶으므로 여기 도달하면 남은 잔여는 저작 대기뿐이다. 그 목록을
# 찍어 두면 다음 슬라이스가 무엇을 집어야 하는지 CI 로그에서 바로 보인다.
[ "$back_total" -eq 0 ] || printf '저작 대기(판정은 끝났고 전용 파일만 없는 시나리오): %s\n' \
	"$(paste -sd'\n' "$tmp/backlog" | sed -e ':a' -e 'N' -e '$!ba' -e 's/\n/ · /g')"
