//go:build e2e

// 검증 시나리오: approval-gated-workload.md#시나리오 6
//
// AC-F5 (docs/prd/approval-gated-workload.md), asserted on the deployed SUT.
//
// **이 파일은 매칭 단위가 아니라 「현행 대체 검증」이다.** 2026-09-12 개정으로 매칭 공간이
// web/e2e 최상위 spec 단독이 됐고, control-plane/test 의 Go 파일은 유예된 시나리오를 대신
// 검증하는 자리로 남았다. 위의 `검증 시나리오:` 선언은 그래서 매핑 선언이 아니라 대체 검증
// 선언이며, docs/test/e2e.md 유예 표의 같은 행이 「현행 대체 검증」 칸에 이 경로를 적어야
// 게이트가 초록이다(scripts/e2e/check-scenario-mapping.sh 가 diff 로 대조한다). 이 행의 해제는
// Playwright 최상위 spec 착지 또는 규칙 4 예외 전환이고, 어느 쪽이든 이 파일의 단언이 그
// 판정의 재료다.
//
// 이 파일이 배타적으로 사는 것: **공유 볼륨이 전달 통로다.** 헬퍼 파드의 MCP 컨테이너가
// 승인된 응답을 떨구는 그 자리에 남긴 파일을 워크로드 파드의 **에이전트가 자기 도구로**
// 읽어 같은 볼륨에 되쓰고, 그 바이트를 헬퍼가 다시 읽는다 — 동결을 거치지 않은 다음 실행이
// 그 파일을 본다는 시나리오 6 (b) 이고, 왕복의 양 끝이 서로 다른 파드다. 그 왕복 내내
// 난스가 **세션 출력 버퍼에는 한 번도 나타나지 않는다**는 음성 단언이 「파일 전달이 도구
// 응답 본문이 아니라 볼륨으로 이뤄진다」를 공허하지 않게 만든다 — 대조군은 같은 버퍼에
// 실제로 들어 있는 대역의 상수 응답이다. (e) 는 워크로드 쪽에서 산다.
//
// **마운트 층은 다시 사지 않는다.** 클레임이 ReadWriteMany 로 bind 되는 것, 워크로드와
// 헬퍼가 같은 경로에 마운트하는 것, credential-proxy 에는 마운트되지 않는 것, 세션마다 다른
// 클레임인 것, 클레임이 세션과 함께 회수되는 것은 control-plane/test/shared_volume_test.go
// 가 이미 배포 SUT 에서 산다. 여기서 두 경로가 같은지 확인하는 것은 그 사실을 되사는 것이
// 아니라 아래 왕복의 **전제**를 실측으로 세우는 것이다 — 프롬프트가 실어 나르는 경로가 두
// 컨테이너에서 같은 문자열이어야 왕복 자체가 성립한다.
//
// What this file deliberately does NOT assert, and why:
//
//   - (a) 앞머리의 「**승인된 외부 호출로** 공유 볼륨에 파일을 만들게 함」. 승인 왕복 자체는
//     e2e_approval_gated_3_approval_path_test.go 가 사지만, spill 을 만드는 호출은 이 SUT 에서
//     일어나지 않는다. 스킴은 더 이상 벽이 아니다 — 2026-09-12 에 validateFetchTarget 이 평문
//     http 도 받게 됐다. 남은 벽은 **대상**이다: spillBody 는 응답이 maxInlineBodyBytes(100,000)
//     를 넘을 때만 돌고, 이 배포에서 GET 에 답하는 인클러스터 origin 은 세 대역의 /healthz 와
//     게이트웨이의 작은 JSON 뿐이라 그 선을 넘는 본문이 없다. 그런 origin 을 세우는 것은 대역
//     확장이고 소관은 tbm_session-platform-e2e-mock-policy(차단 요인 FETCH-ORIGIN)다. 그래서
//     여기서 spill 을 만드는 것은 fetch 가 아니라 이 테스트이고, 그 파일을 **제품이 쓰는 자리와
//     같은 자리**(session_mcp_files.go 의 spillSubdir·spillFileSuffix)에 두는 것까지가 살 수
//     있는 전부다. 볼륨이 그 바이트를 실어 나른다는 것은 그 위에서 온전히 관측된다.
//   - (c) 복원 후 잔존과 (d) 동결 전·후 커서 두 갈래. 이 타입은 POST /snapshot 이 503
//     `checkpoint strategy is disabled` 라 **동결 자체가 없다**(internal/service 의
//     checkpointerFor). 선행은 AC-F5 의 아카이브 전략이고 소관은
//     tbm_session-platform-docs-impl 이다 — e2e_f4_helper_pod_test.go 가 같은 벽 앞에서
//     자기 두 갈래를 등재해 둔 그 선행이다.
//
// 세 갈래 모두 docs/test/e2e.md § "남은 미검증 분기 (공백은 아님)" 에 등재했다.
package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// data-plane/cmd/agent/session_mcp_files.go 의 spillSubdir·spillFileSuffix,
	// session_mcp_tools.go 의 requestIDPrefix. 제품 상수를 import 하지 않고 옮겨
	// 적는 것은 이 스위트의 관례다(e2e_f1 의 이유): 테스트가 제품 값을 읽으면 둘이
	// 함께 바뀌어도 초록이라, 자리가 옮겨진 것을 아무도 모른다.
	f5SpillSubdir     = "web_fetch_get"
	f5SpillSuffix     = ".body"
	f5RequestIDPrefix = "req-"
	// 대역이 지시자 없는 턴에 내는 상수 (deploy/e2e-anthropic-fake.yaml). 도구
	// 결과를 받은 다음 턴에는 지시자가 실린 user turn 이 더 이상 마지막이 아니므로
	// 대역은 이 상수로 답한다 — 그래서 이 문자열이 버퍼에 오면 도구 왕복이 끝난
	// 것이고, 동시에 아래 음성 단언의 대조군이 된다.
	f5ProviderReply = "session-platform-e2e-provider-ok"
	// 한 번의 에이전트 왕복 예산: CLI 기동 + 도구 실행 + 대역 두 턴. 승인 게이트를
	// 지나지 않으므로 f3 의 마커 예산보다 짧게 잡아도 되지만, 파드가 막 뜬 직후의
	// 첫 invocation 이라 여유를 둔다.
	f5InvocationBudget = 120 * time.Second
)

// f5SharedDir 은 컨테이너가 **실제로 받은** SESSION_SHARED_DIR 이다. pod spec 이 아니라
// 돌고 있는 프로세스의 환경에서 읽는 이유는 shared_volume_test.go 의 같은 프로브가 적는
// 것과 같다 — 주입은 됐는데 이미지가 무시하는 경우를 spec 검사는 통과시킨다.
func f5SharedDir(t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod, container string) string {
	t.Helper()
	got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, pod, container, `printf %s "${SESSION_SHARED_DIR-}"`))
	if got == "" {
		t.Fatalf("%s/%s [%s] was not told SESSION_SHARED_DIR — a volume nothing knows about carries nothing (AC-F5)",
			ns, pod, container)
	}
	return got
}

// f5EventuallyBuffer 은 세션 버퍼가 want 를 담을 때까지 offset 0 을 다시 읽는다.
// eventuallyShellRead 의 30초 예산은 쉘 한 줄에 맞춰져 있어 CLI 기동 + 도구 실행 한
// 왕복에는 모자란다.
func f5EventuallyBuffer(t *testing.T, id, want string) string {
	t.Helper()
	deadline := time.Now().Add(f5InvocationBudget)
	for {
		r := readShellAt(t, id, 0)
		if strings.Contains(r.Payload, want) {
			return r.Payload
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session buffer never carried %q within %s — the invocation did not complete; buffer=%q",
				want, f5InvocationBudget, r.Payload)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func f5Delete(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", id, resp.StatusCode, raw)
		}
	})
}

// 시나리오 6 (a)(b): 볼륨에만 있는 바이트가 다음 실행에서 에이전트를 거쳐 되돌아온다.
//
// 왕복은 한 세션 안의 한 방향이 아니라 **파드를 두 번 건넌다**: MCP 컨테이너(헬퍼 파드)가
// 쓰고 → 에이전트(워크로드 파드)가 읽어 되쓰고 → 다시 MCP 컨테이너가 읽는다. 두 번째
// 건너감이 없으면 「워크로드가 그 파일을 보았다」를 테스트 프로세스의 exec 로만 사게 되고,
// 그것은 시나리오가 말하는 「다음 프롬프트로 그 파일을 읽게 함」이 아니다.
func TestApprovalGatedSharedVolume_TheVolumeCarriesTheFileToTheAgentWithoutAFreeze(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; one filesystem under two pods is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	f5Delete(t, s.ID)
	helper := f4TheHelperPod(t, cs, ns, s)
	workload := getPodEventually(t, cs, ns, s.Pod)

	helperDir := f5SharedDir(t, cs, cfg, ns, helper.Name, sessionMCPContainer)
	workloadDir := f5SharedDir(t, cs, cfg, ns, workload.Name, workloadContainer)
	if helperDir != workloadDir {
		t.Fatalf("the pair was told different shared directories — MCP %q, workload %q; "+
			"the prompt below carries a path, so the round trip needs one string (AC-F5)", helperDir, workloadDir)
	}

	// requestID 는 spillBody 가 파일 이름에 쓰는 것과 같은 모양이고, 난스는 이 실행에만
	// 있는 바이트다 — 아래 음성 단언이 그 유일성 위에 선다.
	requestID := f5RequestIDPrefix + "e2e-" + s.ID
	spillPath := fmt.Sprintf("%s/%s/%s%s", helperDir, f5SpillSubdir, requestID, f5SpillSuffix)
	readBackPath := fmt.Sprintf("%s/%s/%s.readback", helperDir, f5SpillSubdir, requestID)
	nonce := fmt.Sprintf("f5-nonce-%s-%d", s.ID, time.Now().UnixNano())

	// 승인된 응답이 볼륨에 도착하는 자리를 그대로 쓴다. 이 파일을 fetch 가 아니라
	// 테스트가 만드는 이유는 헤더의 FETCH-ORIGIN 항목에 있다.
	f4Sh(t, cs, cfg, ns, helper.Name, sessionMCPContainer,
		`set -eu; mkdir -p "$(dirname "$2")"; printf '%s' "$1" > "$2"`, nonce, spillPath)

	// 동결을 건너뛴 **다음 실행**이다: 세션은 계속 active 이고 그 사이 스냅샷도 재시작도
	// 없다. 에이전트에게 도구 하나를 시키는 채널은 대역의 지시자뿐이고(대역이 그 이름의
	// tool_use 블록을 낸다), Bash 는 플랫폼이 관리 설정으로 허용한 도구 목록에 있다.
	prompt := fmt.Sprintf(`e2e-tool:Bash:{"command":"cat %s > %s"}`, spillPath, readBackPath)
	writeShell(t, s.ID, prompt)
	buf := f5EventuallyBuffer(t, s.ID, f5ProviderReply)

	// 되쓴 바이트를 **헬퍼 파드에서** 읽는다. 워크로드에서 읽으면 자기가 쓴 것을 자기가
	// 보는 것이라 볼륨이 공유라는 사실을 사지 못한다.
	got := f4Sh(t, cs, cfg, ns, helper.Name, sessionMCPContainer, `set -eu; cat "$1"`, readBackPath)
	if got != nonce {
		t.Fatalf("the MCP container reads %q back at %s, want the %d bytes it wrote — "+
			"the agent's tool call did not carry them across the volume (AC-F5)", got, readBackPath, len(nonce))
	}

	// 음성: 그 바이트는 세션 출력 어디에도 없다. 공허하지 않은 이유는 같은 버퍼가 대역의
	// 상수 응답을 실제로 담고 있다는 것 — 버퍼가 비어 있어서 못 찾은 것이 아니다.
	if !strings.Contains(buf, f5ProviderReply) {
		t.Fatalf("the buffer does not carry the provider reply, so the negative below would be vacuous: %q", buf)
	}
	if strings.Contains(buf, nonce) {
		t.Fatalf("the session buffer carries the file's bytes: %q — AC-F5 hands files over the volume, "+
			"not through the response", buf)
	}
	final := readShellAt(t, s.ID, 0)
	if strings.Contains(final.Payload, nonce) {
		t.Fatalf("the session buffer carries the file's bytes after the invocation settled: %q", final.Payload)
	}
}

// 시나리오 6 (e): 다른 세션의 워크로드 파드는 이 볼륨을 마운트하지 못한다.
//
// shared_volume_test.go 는 같은 격리를 **헬퍼** 쪽에서 산다(각 세션의 MCP 컨테이너가 서로의
// 파일을 못 본다). 시나리오가 지목하는 것은 워크로드 파드이고, 그쪽은 마운트 경로가 같은
// 문자열이라 「같은 경로 = 같은 볼륨」이라는 착시가 가능한 유일한 자리다 — 그래서 양성
// 대조군을 먼저 통과시킨 뒤에 음성을 잰다.
func TestApprovalGatedSharedVolume_AnotherSessionsWorkloadDoesNotSeeIt(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; volume privacy is a claim about deployed pods")
	}
	ns := sessionNamespace()

	first, second := f4Create(t), f4Create(t)
	f5Delete(t, first.ID)
	f5Delete(t, second.ID)

	helperOne := f4TheHelperPod(t, cs, ns, first)
	workloadOne := getPodEventually(t, cs, ns, first.Pod)
	workloadTwo := getPodEventually(t, cs, ns, second.Pod)

	dirOne := f5SharedDir(t, cs, cfg, ns, helperOne.Name, sessionMCPContainer)
	dirTwo := f5SharedDir(t, cs, cfg, ns, workloadTwo.Name, workloadContainer)

	requestID := f5RequestIDPrefix + "e2e-" + first.ID
	nonce := fmt.Sprintf("f5-private-%s-%d", first.ID, time.Now().UnixNano())
	relative := fmt.Sprintf("%s/%s%s", f5SpillSubdir, requestID, f5SpillSuffix)
	f4Sh(t, cs, cfg, ns, helperOne.Name, sessionMCPContainer,
		`set -eu; mkdir -p "$(dirname "$2")"; printf '%s' "$1" > "$2"`, nonce, dirOne+"/"+relative)

	// 양성 대조군: 이 세션의 워크로드는 본다. 이것이 실패하면 아래 음성은 「경로가 틀렸다」와
	// 구별되지 않는다.
	mine := f4Sh(t, cs, cfg, ns, workloadOne.Name, workloadContainer,
		`if [ -e "$1" ]; then cat "$1"; else printf absent; fi`,
		f5SharedDir(t, cs, cfg, ns, workloadOne.Name, workloadContainer)+"/"+relative)
	if mine != nonce {
		t.Fatalf("the session's own workload reads %q at its shared dir, want the bytes its MCP container wrote (AC-F5)", mine)
	}

	other := f4Sh(t, cs, cfg, ns, workloadTwo.Name, workloadContainer,
		`if [ -e "$1" ]; then cat "$1"; else printf absent; fi`, dirTwo+"/"+relative)
	if other != "absent" {
		t.Fatalf("session %s's workload reads %q at the same relative path under its own shared dir — "+
			"the two sessions are looking at one volume (AC-F5, AC-A2)", second.ID, other)
	}
}
