//go:build e2e

// 검증 시나리오: drop-file-share.md#시나리오 1
//
// docs/prd/drop-file-share.md AC-I1~I3 를 배포된 SUT 에서: 에이전트가 공유 볼륨에 둔 파일이
// 실물 drop(deploy/e2e-drop.yaml)으로 올라가고, 같은 보관함이 다시 공유 볼륨으로 내려온다.
// 도구 호출은 공급자 대역의 e2e-tool 지시자로 실 CLI 가 세션 MCP 에 낸다(f3 과 같은 진입).
package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	dropInClusterURL = "http://drop.drop.svc.cluster.local"
	dropUploadName   = "mcp__session-platform-session-mcp__drop_upload"
	dropDownloadName = "mcp__session-platform-session-mcp__drop_download"
	// CLI 기동 + MCP 왕복 + drop 의 TokenReview. 승인을 기다리지 않으므로 f3 보다 짧아도 되지만
	// 첫 invocation 의 CLI 기동이 러너에 따라 길다.
	dropToolBudget = 150 * time.Second
)

func TestDropRoundTrip_FilesLeaveAndReturnThroughTheSharedVolume(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; the file is placed and read back through exec")
	}
	ns := sessionNamespace()
	status, s := createTyped(t, map[string]any{"name": uniqueName(t), "workloadType": "approval-gated"})
	if status != http.StatusCreated {
		t.Fatalf("create approval-gated session: status=%d", status)
	}
	t.Cleanup(func() {
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil); resp.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", s.ID, resp.StatusCode, raw)
		}
	})
	helpers := helperPodsFor(t, cs, ns, s.ID)
	if len(helpers) != 1 {
		t.Fatalf("session %s has %d helper pods, want 1", s.ID, len(helpers))
	}
	helper := helpers[0].Name
	shared := strings.TrimSpace(f4Sh(t, cs, cfg, ns, s.Pod, workloadContainer, `printf %s "${SESSION_SHARED_DIR-}"`))
	if shared == "" {
		t.Fatal("the workload container has no SESSION_SHARED_DIR; the kind overlay names a ReadWriteMany class, so this is a wiring fault")
	}

	// 에이전트 쪽에서 파일을 둔다 — 도구 인자에는 이 경로만 실린다(AC-I1).
	marker := "drop-e2e-contents-" + s.ID
	f4Sh(t, cs, cfg, ns, s.Pod, workloadContainer,
		`set -eu; mkdir -p "$1/out"; printf '%s' "$2" > "$1/out/hello.txt"`, shared, marker)

	id := "sp-" + s.ID[:16]
	password := "pw-" + s.ID[16:28]
	upload, _ := json.Marshal(map[string]any{"paths": []string{"out/hello.txt"}, "id": id, "password": password, "days": 1})
	writeShell(t, s.ID, "e2e-tool:"+dropUploadName+":"+string(upload))

	// 보관함은 drop 자신에게 묻는다 — 받는 사람이 쓰는 것과 같은 받기 API 로, 세션 MCP 를 거치지 않고.
	listing := dropEventually(t, func() (string, bool) {
		out := f4Sh(t, cs, cfg, ns, helper, sessionMCPContainer,
			`curl -sS --max-time 20 -w '\n%{http_code}' -u "$2:$3" -H 'Accept: application/json' "$1/get/$2"`,
			dropInClusterURL, id, password)
		i := strings.LastIndex(out, "\n")
		if i < 0 {
			return out, false
		}
		return out[:i], strings.TrimSpace(out[i+1:]) == "200"
	}, "drop never held the uploaded drop "+id)
	var got struct {
		Files []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(listing), &got); err != nil {
		t.Fatalf("decode drop's listing: %v raw=%s", err, listing)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "hello.txt" || got.Files[0].Size != int64(len(marker)) {
		t.Fatalf("drop lists %+v, want hello.txt of %d bytes", got.Files, len(marker))
	}
	if content := f4Sh(t, cs, cfg, ns, helper, sessionMCPContainer,
		`curl -sS --fail --max-time 20 -u "$2:$3" "$1/get/$2/f/0"`, dropInClusterURL, id, password); content != marker {
		t.Fatalf("drop serves %q, want the bytes the agent wrote", content)
	}

	download, _ := json.Marshal(map[string]any{"id": id, "password": password, "dest": "in"})
	writeShell(t, s.ID, "e2e-tool:"+dropDownloadName+":"+string(download))
	dropEventually(t, func() (string, bool) {
		out := f4Sh(t, cs, cfg, ns, s.Pod, workloadContainer, `cat "$1/in/hello.txt" 2>/dev/null || true`, shared)
		return out, out == marker
	}, "the downloaded file never reached the workload's shared volume")

	// AC-I3: 승인을 기다린 적이 없다. 두 호출 모두 대기 마커 없이 끝났다.
	if buf := readShellAt(t, s.ID, 0).Payload; strings.Contains(buf, "[session-platform: awaiting approval") {
		t.Fatalf("a drop tool waited on the approval gate: %q", buf)
	}
}

func dropEventually(t *testing.T, probe func() (string, bool), why string) string {
	t.Helper()
	deadline := time.Now().Add(dropToolBudget)
	for {
		out, ok := probe()
		if ok {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s (last probe: %q)", why, dropToolBudget, out)
		}
		time.Sleep(2 * time.Second)
	}
}
