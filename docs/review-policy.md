# PR 수동 승인 정책

> 수동 승인 케이스 · 위험 표면 · 의도적 제외 · 미탐 원장의 SSOT. 2026-11-04 최초 등재 (reconciler 모델 `tbm_session-platform-pr-manual-approval`, 템플릿 `templates/pr-manual-approval.tbm.md`).
> 판정기(`scripts/review-policy/judge.py`)는 이 문서의 케이스 ID 를 판정 근거 라벨로 쓰고 케이스 본문을 되풀이하지 않는다.

이 레포의 PR 수동 승인은 **하나의 프로세스**다. 판정기 하나가 PR 이 아래 케이스 어디에도 닿지 않았다고 확인될 때만 head 커밋에 `review/manual-approval` = success 를 붙이고, 기본 브랜치 ruleset 이 그 status 를 required 로 요구한다. 닿았을 가능성이 있으면 판정기는 아무 status 도 붙이지 않는다 — status 의 **부재**가 곧 「사람이 본다」이고, 판정기가 죽거나 판정이 불가능해도(정책 파싱 실패 · 정책과 판정기의 케이스 ID 불일치 · 빈 변경 목록 · files API 상한) 같은 결과가 된다. 사람의 승인은 PR 을 검토한 사람이 **같은 context 로 직접 success 를 붙이는 것**(수동 status) 하나뿐이다.

처음 등재한 케이스는 제품 문서와 이 프로세스 자신뿐이다. 코드·배포의 위험(마이그레이션·인증·매니페스트 등)을 케이스로 더할지는 사람의 결정이고, 더하면 아래 표·surface·anchors 에 같은 형식으로 행을 늘린다.

## 프로세스

| 항목 | 값 |
|---|---|
| status context | `review/manual-approval` |
| 판정기 | `scripts/review-policy/judge.py` (`--policy docs/review-policy.md`, 표준 입력 = 변경 경로 목록 · 개명은 옛 경로와 새 경로 둘 다; 종료 코드 0 = 케이스 미접촉, 1 = 접촉(닿은 케이스 ID 와 경로 출력), 2 = 판정 불가) |
| 워크플로 | `.github/workflows/review-policy.yml` — `pull_request_target`, **base 브랜치를 체크아웃**해 base 쪽 판정기로 돌고, PR 의 변경 경로는 `pulls/{n}/files` API 로 받아 판정기의 입력(데이터)으로만 쓴다. PR 코드는 체크아웃도 실행도 하지 않는다. 종료 코드 0 일 때만 status 를 붙인다 |
| 게이트 | 현재 기본 브랜치에 ruleset 이 없다. 이 문서와 워크플로가 main 에 착지한 뒤 저장소 소유자가 기본 브랜치 ruleset 을 만들고 required status check 에 `review/manual-approval`(integration 고정 없음)을 넣는다 — 먼저 넣으면 워크플로가 없는 동안 모든 PR 이 막힌다. 넣은 뒤 이 칸에 ruleset 이름·id 와 날짜를 적는다. 확인: `gh api "repos/dlddu/session-platform/rules/branches/main" --jq '.[] \| select(.type=="required_status_checks") \| .parameters.required_status_checks[] \| "\(.context) integration=\(.integration_id // "any")"'` → `review/manual-approval integration=any` 가 있어야 하고 `review/` 계열 다른 context 는 없어야 한다 |
| 사람 승인 | 수동 status. 승인할 수 있는 사람: 저장소 소유자(`dlddu`). PR 을 검토한 뒤 head 커밋에 붙인다: `gh api -X POST "repos/dlddu/session-platform/statuses/<head sha>" -f state=success -f context=review/manual-approval -f description="수동 승인 — <승인자>" -f target_url=<PR URL>`. 사람의 지시로 도는 승인 전용 도구: reconciler `reconciler-manual-approval` 스킬(사람 세션 전용) → homelab-k3s-mcp `github_commit_status_create` — 붙일 수 있는 context 는 서버 env `GITHUB_COMMIT_STATUS_CONTEXT_PREFIXES` = `review/` 로 제한된다(`dlddu/homelab-k3s-mcp` `k8s/deployment.yaml`). 이 레포에서 그 네임스페이스의 status 는 `review/manual-approval` 하나다. ruleset bypass · 관리자 머지 · required check 해제는 승인 경로가 아니다. 승인 뒤 새 푸시로 head 가 바뀌면 승인은 무효가 되고 판정기가 다시 판정한다 |
| 마지막 재검토 | 2026-11-04 |
| 다음 재검토 | 2027-02-02 |

재검토 때는 날짜만 옮기지 않는다: 아래 `surface` 블록이 여전히 위험을 다 담는지(제품 문서가 새 위치에 생기지 않았는지), ruleset 의 bypass 목록이 비어 있는지(admin 권한 필요)를 사람이 보고 결과를 이 절에 한 줄로 남긴다.

## 수동 승인 케이스

| ID | 대상 | 이유 |
|---|---|---|
| MA1 | 제품 문서 — 가치·PRD·테스트·여정: `docs/values.md` · `docs/prd/` · `docs/test/` · `docs/user-journeys/` 의 **모든** 변경(개명은 옛 경로와 새 경로 둘 다 판정 입력이 된다). 집계(`docs/doc-tracker/`)·목업은 대상이 아니다 | 제품이 무엇을 왜 하는가의 결정이고, 소유자는 사용자다(`docs/values.md` 「제품 소유자」). 정합성 루프(reconciler docs-impl·scenario-e2e 등)가 어긋남을 「문서를 구현에 맞추는」 방향으로 닫으면 제품 결정이 사람 눈 없이 바뀐다 — 2026-11-04 점검에서 이 레포는 승인 게이트가 없어 #72(PRD 2건 AC 개정) 등 제품 문서 PR 가 사람 승인 없이 머지된 것이 확인됐다. 테스트 문서는 AC 의 검증 기준을 정하므로 함께 건다 |
| MA2 | 판정기·워크플로·정책 자신 — `scripts/review-policy/` · `.github/workflows/review-policy.yml` · `docs/review-policy.md` | 워크플로가 `pull_request_target` 이라 PR 은 **base 쪽 판정기**로 판정된다 — PR 이 규칙을 고쳐 자기를 통과시킬 수는 없지만, 그 변경 자체(케이스를 줄이거나 자기 경로를 빼는 편집)는 옛 판정기가 보지 못한 채 다음 PR 부터 효력을 가진다. 그래서 이 경로의 변경은 사람 눈에 올린다 |

## 위험 표면

케이스의 위험이 레포에 나타나는 모양. reconciler as-is 지문이 이 블록에 걸리는 (파일, 토큰) 집합을 잰다 — 제품 결정을 담는 머리(AC · 제품 가치 · 테스트 시나리오 · 여정)가 새 파일에 나타나면 지문이 움직여 이 정책이 다시 판정된다. 걸리는 파일은 전부 위 케이스에 덮이거나 아래 「의도적 제외」에 있어야 한다. 집계(`docs/doc-tracker/`)·목업 HTML 은 결정을 옮겨 적을 뿐이라 표면에서 뺀다.

```surface
scope: docs
exclude: ^docs/(doc-tracker|journeys|mockups)/|\.html$
ere: ^### AC[-.0-9A-Z]*|^## 제품 가치|^### 시나리오|^## 테스트 시나리오|^## 검증 대상 AC
```

## 앵커

케이스가 이름으로 지목하는 경로. 하나라도 사라지면 그 케이스는 죽은 케이스다 — 판정기와 이 문서를 새 위치로 옮기거나, 위험 자체가 사라졌으면 케이스를 지운다.

```anchors
docs/values.md
docs/prd/*.md
docs/test/*.md
docs/user-journeys/*.md
scripts/review-policy/judge.py
.github/workflows/review-policy.yml
docs/review-policy.md
```

## 의도적 제외

판정기가 보지 않지만 위 케이스의 위험에 닿을 수 있는 변경. 재개 조건이 참이 되면 그 행을 지우고 케이스를 더한다. 제외를 연장하려면 새 재개 조건을 쓴다. 처음 등재 시점에는 없다.

| 대상 | 사유 | 재개 조건 | 소관 |
|---|---|---|---|

## 미탐 원장

`review/manual-approval` 이 자동으로 붙어 머지된 PR 에서, 사람 승인이 필요했던 변경을 뒤늦게 발견하면 한 행을 더한다. 보강 케이스 ID 는 케이스 표와 판정기에 실재해야 한다. 비어 있는 것이 정상이다.

| PR | 놓친 것 | 보강 케이스 ID | 기록일 |
|---|---|---|---|
