# PR 수동 승인 정책

> 수동 승인 케이스 · 위험 표면 · 의도적 제외 · 미탐 원장의 SSOT. 모델 `tbm_session-platform-pr-manual-approval`, 템플릿 reconciler `templates/pr-manual-approval.tbm.md`.
> 판정기(`scripts/review-policy/judge.py`)는 이 문서의 케이스 ID 를 판정 근거 라벨로 쓰고 케이스 본문을 되풀이하지 않는다.

이 레포의 PR 수동 승인은 **하나의 프로세스**다. 판정기 하나가 PR 이 아래 케이스 어디에도 닿지 않았다고 확인될 때만 head 커밋에 `review/manual-approval` = success 를 붙이고, 기본 브랜치 ruleset 이 그 status 를 required 로 요구한다. 닿았을 가능성이 있으면 판정기는 아무 status 도 붙이지 않는다 — status 의 **부재**가 곧 「사람이 본다」이고, 판정기가 죽거나 판정이 불가능해도(정책 파싱 실패 · 정책과 판정기의 케이스 ID 불일치 · 빈 변경 목록 · files API 상한) 같은 결과가 된다. 사람의 승인은 PR 을 검토한 사람이 **같은 context 로 직접 success 를 붙이는 것**(수동 status) 하나뿐이다.

케이스는 저장소 소유자가 2026-11-04 정했다. 2026-11-04 오전 등재(MA1·MA2) 뒤 같은 날 운영 매니페스트·저장 형식·인증·샌드박스 케이스(MA3~MA5)를 더했다. CI 워크플로 전반은 대상이 아니다(게이트 자신의 워크플로만 MA2). 케이스를 더하거나 줄이는 것은 사람의 결정이고, 바꾸면 아래 표·surface·anchors 와 판정기를 같은 PR 에서 함께 고친다.

## 프로세스

| 항목 | 값 |
|---|---|
| status context | `review/manual-approval` |
| 판정기 | `scripts/review-policy/judge.py` (`--policy docs/review-policy.md`, 표준 입력 = 변경 경로 목록 · 개명은 옛 경로와 새 경로 둘 다; 종료 코드 0 = 케이스 미접촉, 1 = 접촉(닿은 케이스 ID 와 경로 출력), 2 = 판정 불가). 케이스마다 대상 경로와 제외 패턴(테스트 등)을 갖는다 |
| 워크플로 | `.github/workflows/review-policy.yml` — `pull_request_target`, **base 브랜치를 체크아웃**해 base 쪽 판정기로 돌고, PR 의 변경 경로는 `pulls/{n}/files` API 로 받아 판정기의 입력(데이터)으로만 쓴다. PR 코드는 체크아웃도 실행도 하지 않는다. 종료 코드 0 일 때만 status 를 붙인다 |
| 게이트 | 기본 브랜치 ruleset `default`(id 26521755, active, bypass 0)의 required status check — `review/manual-approval`(integration 고정 없음) 하나. 게이트 PR #193 착지 뒤 2026-11-04 에 저장소 소유자가 ruleset 을 만들어 넣었다(먼저 넣으면 워크플로가 없는 동안 모든 PR 이 막힌다). 확인: `gh api "repos/dlddu/session-platform/rules/branches/main" --jq '.[] \| select(.type=="required_status_checks") \| .parameters.required_status_checks[] \| "\(.context) integration=\(.integration_id // "any")"'` → `review/manual-approval integration=any` 가 있어야 하고 `review/` 계열 다른 context 는 없어야 한다 |
| 사람 승인 | 수동 status. 승인할 수 있는 사람: 저장소 소유자(`dlddu`). PR 을 검토한 뒤 head 커밋에 붙인다: `gh api -X POST "repos/dlddu/session-platform/statuses/<head sha>" -f state=success -f context=review/manual-approval -f description="수동 승인 — <승인자>" -f target_url=<PR URL>`. 사람의 지시로 도는 승인 전용 도구: reconciler `reconciler-manual-approval` 스킬(사람 세션 전용) → homelab-k3s-mcp `github_commit_status_create` — 붙일 수 있는 context 는 서버 env `GITHUB_COMMIT_STATUS_CONTEXT_PREFIXES` = `review/` 로 제한된다(`dlddu/homelab-k3s-mcp` `k8s/deployment.yaml`). 이 레포에서 그 네임스페이스의 status 는 `review/manual-approval` 하나다. ruleset bypass · 관리자 머지 · required check 해제는 승인 경로가 아니다. 승인 뒤 새 푸시로 head 가 바뀌면 승인은 무효가 되고 판정기가 다시 판정한다 |
| 마지막 재검토 | 2026-11-04 |
| 다음 재검토 | 2027-02-02 |

재검토 때는 날짜만 옮기지 않는다: 아래 `surface` 블록이 여전히 위험을 다 담는지(위험이 새 위치·새 기술로 나타나지 않았는지), ruleset 의 bypass 목록이 비어 있는지(admin 권한 필요)를 사람이 보고 결과를 이 절에 한 줄로 남긴다.

## 수동 승인 케이스

| ID | 대상 | 이유 |
|---|---|---|
| MA1 | 제품 문서 — 가치·PRD·테스트·여정: `docs/values.md` · `docs/prd/` · `docs/test/` · `docs/user-journeys/` 의 **모든** 변경(개명은 옛 경로와 새 경로 둘 다 판정 입력이 된다) | 제품이 무엇을 왜 하는가의 결정이고, 소유자는 사용자다(`docs/values.md` 「제품 소유자」). 정합성 루프(reconciler docs-impl·scenario-e2e 등)가 어긋남을 「문서를 구현에 맞추는」 방향으로 닫으면 제품 결정이 사람 눈 없이 바뀐다. 테스트 문서는 AC 의 검증 기준을 정하므로 함께 건다. 집계(doc-tracker)·목업은 결정을 옮겨 적을 뿐이라 대상이 아니다 |
| MA2 | 판정기·워크플로·정책 자신 — `scripts/review-policy/` · `.github/workflows/review-policy.yml` · `docs/review-policy.md` | 워크플로가 `pull_request_target` 이라 PR 은 **base 쪽 판정기**로 판정된다 — PR 이 규칙을 고쳐 자기를 통과시킬 수는 없지만, 그 변경 자체(케이스를 줄이거나 자기 경로를 빼는 편집)는 옛 판정기가 보지 못한 채 다음 PR 부터 효력을 가진다. 그래서 이 경로의 변경은 사람 눈에 올린다 |
| MA3 | 운영 매니페스트·RBAC·네트워크 정책 — `k8s/` 아래 모든 변경(`k8s/*.example.yaml` 제외) | Flux 가 main 의 `k8s/` 를 그대로 운영에 적용한다(`k8s/kustomization.yaml`). control-plane Role 의 파드·ConfigMap·PVC 생성/삭제 권한(`rbac.yaml`), data-plane 의 `view` 바인딩, 인증 없는 control-plane API 를 지키는 유일한 선인 ingress NetworkPolicy 와 셸 워크로드 egress 차단(`network-policy.yaml`), SQLite 단일 writer 전제인 `replicas: 1`, 비밀·S3 역할 참조가 여기에 있다. `*.example.yaml` 은 kustomization 리소스가 아니다 |
| MA4 | 저장 형식 — 레포 접근 허용목록 SQLite(`control-plane/internal/repoaccess/`) · 세션 ConfigMap 저장(`control-plane/internal/adapter/configmap/`, `control-plane/internal/session/session.go`) · S3 체크포인트·아카이브(`control-plane/internal/adapter/checkpointstore/`, `control-plane/internal/adapter/archive/`) · 데이터 플레인 상태 아카이브(`data-plane/cmd/agent/state_archive.go`, `data-plane/cmd/agent/checkpoint.go`). 테스트(`*_test.go`, `envtest/`)는 대상이 아니다 | 이미 쓰인 데이터를 읽어야 하는 형식이다 — 허용목록 SQLite 는 마이그레이션 장치 없이 `CREATE TABLE IF NOT EXISTS` 로 만들어지고, 세션 JSON 태그·tar 아카이브 레이아웃·S3 키 접두사가 바뀌면 기존 세션과 아카이브를 복원할 수 없게 된다. 단위 테스트는 「새로 쓰고 새로 읽는」 왕복만 보므로 옛 데이터와의 호환 파괴를 잡지 못한다 |
| MA5 | 인증·샌드박스·승인 게이트웨이 — 헬퍼 인증(`control-plane/internal/helperauth/`) · 레포 접근 API(`control-plane/cmd/control-plane/repo_access.go`, `control-plane/internal/api/repo_access.go`) · 자격증명 프록시와 K3s MCP 거부목록·git push·비공개 레포 접근 토큰(`data-plane/cmd/agent/credential_proxy*.go`, `k3s_mcp_proxy.go`, `k3s_mcp_tool_deny.go`, `session_mcp_git_push.go`, `session_mcp_repo_access.go`, `minimax_marketplace.go`) · 토큰 발급 진입점(`data-plane/entrypoint.sh`) · 워크로드 파드 보안·네트워크 격리(`control-plane/internal/adapter/k8s/`) · 승인 게이트웨이(`data-plane/cmd/agent/approval_gateway.go`, `approval_wait.go`, `session_mcp_tools.go`) · 두 이미지(`control-plane/Dockerfile`, `data-plane/Dockerfile`). 테스트(`*_test.go`)는 대상이 아니다 | 에이전트 워크로드가 무엇에 닿을 수 있는가를 정하는 코드다 — TokenReview 로 헬퍼 파드의 세션을 식별하고, 프록시가 공급자 토큰을 응답에서 지우며, 거부목록이 minimax-code 제한의 유일한 집행 지점이고, 파드 securityContext·세션별 NetworkPolicy 가 사용자 코드를 클러스터에서 떼어 놓으며, 모든 외부 도구가 승인 게이트웨이를 거친다. 느슨해져도 테스트는 「통과하지만 덜 막는」 변경을 잡지 못한다. `control-plane/internal/adapter/k8s/` 는 변경이 잦아 승인 대기가 늘어나는 것을 비용으로 받아들인다 |

## 위험 표면

케이스의 위험이 레포에 나타나는 모양. reconciler as-is 지문이 이 블록에 걸리는 (파일, 토큰) 집합을 잰다 — 위험 신호가 새 파일에 나타나면 지문이 움직여 이 정책이 다시 판정된다. 걸리는 파일은 전부 위 케이스에 덮이거나 아래 「의도적 제외」에 있어야 한다. 테스트·집계·목업은 운영 경로가 아니라 표면에서 뺀다.

```surface
scope: docs k8s control-plane data-plane
exclude: ^docs/(doc-tracker|journeys|mockups)/|\.html$|_test\.go$|^control-plane/test/|/envtest/|^k8s/[^/]*\.example\.yaml$
ere: ^### ([A-Z]+-)?AC[-.0-9A-Z]*|^## 제품 가치|^## Acceptance Criteria|^### 시나리오|^## 테스트 시나리오|^## 검증 대상 AC
ere: ^kind: (Deployment|Role|ClusterRole|RoleBinding|ClusterRoleBinding|NetworkPolicy|ServiceAccount|PersistentVolumeClaim)$
ere: CREATE TABLE|ALTER TABLE|tar\.NewReader
ere: TokenReviews\(\)|deniedTools|x-access-token|AllowPrivilegeEscalation
```

## 앵커

케이스가 이름으로 지목하는 경로. 하나라도 사라지면 그 케이스는 죽은 케이스다 — 판정기와 이 문서를 새 위치로 옮기거나, 위험 자체가 사라졌으면 케이스를 지운다.

```anchors
docs/values.md
docs/prd/*
docs/test/*
docs/user-journeys/*
scripts/review-policy/*
.github/workflows/review-policy.yml
docs/review-policy.md
k8s/*
control-plane/internal/repoaccess/*
control-plane/internal/adapter/configmap/*
control-plane/internal/adapter/checkpointstore/*
control-plane/internal/adapter/archive/*
control-plane/internal/session/session.go
data-plane/cmd/agent/state_archive.go
data-plane/cmd/agent/checkpoint.go
control-plane/internal/helperauth/*
control-plane/cmd/control-plane/repo_access.go
control-plane/internal/api/repo_access.go
data-plane/cmd/agent/credential_proxy*.go
data-plane/cmd/agent/k3s_mcp_proxy.go
data-plane/cmd/agent/k3s_mcp_tool_deny.go
data-plane/cmd/agent/session_mcp_git_push.go
data-plane/cmd/agent/session_mcp_repo_access.go
data-plane/cmd/agent/minimax_marketplace.go
data-plane/entrypoint.sh
control-plane/internal/adapter/k8s/*
data-plane/cmd/agent/approval_gateway.go
data-plane/cmd/agent/approval_wait.go
data-plane/cmd/agent/session_mcp_tools.go
data-plane/Dockerfile
control-plane/Dockerfile
```

## 의도적 제외

판정기가 보지 않지만 위 케이스의 위험에 닿을 수 있는 변경. 재개 조건이 참이 되면 그 행을 지우고 케이스를 더한다. 제외를 연장하려면 새 재개 조건을 쓴다.

| 대상 | 사유 | 재개 조건 | 소관 |
|---|---|---|---|

## 미탐 원장

`review/manual-approval` 이 자동으로 붙어 머지된 PR 에서, 사람 승인이 필요했던 변경을 뒤늦게 발견하면 한 행을 더한다. 보강 케이스 ID 는 케이스 표와 판정기에 실재해야 한다. 비어 있는 것이 정상이다.

| PR | 놓친 것 | 보강 케이스 ID | 기록일 |
|---|---|---|---|
