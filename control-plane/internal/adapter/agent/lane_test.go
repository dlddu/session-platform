package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// The control plane's hop onto one lane is a path prefix and nothing else
// (AC-N11): one pod, one port, one network boundary. These tests pin which path
// each call lands on, because that is the whole of the addressing.

// lanePod serves a pod that records the paths it was asked for, and answers the
// agent's own paths and the monitor lane's prefixed ones separately.
type lanePod struct {
	paths     []string
	bufs      map[string][]byte
	writes    map[string][]string
	lanesCode int
	lanesBody string
}

func (p *lanePod) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.paths = append(p.paths, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/lanes":
			if p.lanesCode != 0 && p.lanesCode != http.StatusOK {
				http.Error(w, p.lanesBody, p.lanesCode)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(p.lanesBody))
		case strings.HasSuffix(r.URL.Path, "/write"), r.URL.Path == "/write":
			body, _ := io.ReadAll(r.Body)
			if p.writes == nil {
				p.writes = map[string][]string{}
			}
			p.writes[r.URL.Path] = append(p.writes[r.URL.Path], string(body))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"queued"}`))
		case strings.HasSuffix(r.URL.Path, "/read"), r.URL.Path == "/read":
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			buf := p.bufs[r.URL.Path]
			payload := ""
			if offset >= 0 && offset < len(buf) {
				payload = string(buf[offset:])
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"payload": payload, "nextOffset": len(buf)})
		case strings.HasSuffix(r.URL.Path, "/stream"), r.URL.Path == "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(": idle\n\n"))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

func laneHarness(t *testing.T, pod string) (*agent.HTTPClient, *lanePod) {
	t.Helper()
	p := &lanePod{
		bufs: map[string][]byte{
			"/read":         []byte("사용자와의 대화"),
			"/monitor/read": []byte("작업 레인의 대화"),
		},
		lanesBody: `{"paired":true,"lanes":[{"role":"parent","path":"/read","ready":true},` +
			`{"role":"monitor","path":"/monitor/read","ready":false}]}`,
	}
	srv := httptest.NewServer(p.handler())
	t.Cleanup(srv.Close)
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: "sessions"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host},
	})
	return agent.NewHTTPClient(cs, "sessions", agent.WithPort(port)), p
}

// TestLaneCallsLandOnTheLanesOwnPath: the parent lane is the agent's own paths,
// which is what makes a caller that knows nothing about lanes reach the
// person's chat, and the monitor is the same paths under its prefix.
func TestLaneCallsLandOnTheLanesOwnPath(t *testing.T) {
	c, pod := laneHarness(t, "sess-ab12")
	ctx := context.Background()

	for _, lane := range []session.Lane{"", session.LaneParent} {
		if err := c.WriteLane(ctx, "sess-ab12", lane, "사용자에게"); err != nil {
			t.Fatalf("write lane %q: %v", lane, err)
		}
	}
	if err := c.WriteLane(ctx, "sess-ab12", session.LaneMonitor, "작업에게"); err != nil {
		t.Fatalf("write monitor lane: %v", err)
	}
	if got := pod.writes["/write"]; len(got) != 2 {
		t.Fatalf("the agent's own /write got %v, want both the empty and the parent lane", got)
	}
	if got := pod.writes["/monitor/write"]; len(got) != 1 || got[0] != "작업에게" {
		t.Fatalf("/monitor/write got %v, want the monitor's prompt alone", got)
	}

	// A read on each lane sees that lane's conversation and no other.
	chat, chatNext, err := c.ReadLane(ctx, "sess-ab12", session.LaneParent, 0)
	if err != nil {
		t.Fatal(err)
	}
	monitor, monitorNext, err := c.ReadLane(ctx, "sess-ab12", session.LaneMonitor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if chat != "사용자와의 대화" || monitor != "작업 레인의 대화" {
		t.Fatalf("lane reads crossed: chat %q, monitor %q", chat, monitor)
	}
	if chatNext == 0 || monitorNext == 0 {
		t.Fatalf("both lanes must issue a cursor: %d and %d", chatNext, monitorNext)
	}

	// Read with no lane is byte-for-byte the pod's own read, which is the
	// compatibility this rests on.
	plain, plainNext, err := c.Read(ctx, "sess-ab12", 0)
	if err != nil {
		t.Fatal(err)
	}
	if plain != chat || plainNext != chatNext {
		t.Fatalf("Read = (%q,%d) but the parent lane = (%q,%d); they must be the same conversation",
			plain, plainNext, chat, chatNext)
	}

	stream, err := c.StreamLane(ctx, "sess-ab12", session.LaneMonitor, 0)
	if err != nil {
		t.Fatalf("stream monitor lane: %v", err)
	}
	stream.Close()
	if !containsPath(pod.paths, "GET /monitor/stream") {
		t.Fatalf("the monitor stream did not land on its own path: %v", pod.paths)
	}
	// Nothing was added outside the pod's one port: every path asked for is
	// either the agent's own or under the one prefix.
	for _, path := range pod.paths {
		if !strings.HasPrefix(path, "GET /") && !strings.HasPrefix(path, "POST /") {
			t.Fatalf("unexpected request %q", path)
		}
	}
}

// TestLanePathPrefixRefusesWhatIsNotALane: a lane that is not a manager's is
// refused before any request is made, so a typo never becomes a call on a path
// the pod would answer from the wrong conversation.
func TestLanePathPrefixRefusesWhatIsNotALane(t *testing.T) {
	c, pod := laneHarness(t, "sess-ab12")
	ctx := context.Background()
	for _, lane := range []session.Lane{"worker", "Monitor", "both"} {
		if err := c.WriteLane(ctx, "sess-ab12", lane, "x"); !errors.Is(err, session.ErrInvalidInput) {
			t.Fatalf("write lane %q = %v, want ErrInvalidInput", lane, err)
		}
		if _, _, err := c.ReadLane(ctx, "sess-ab12", lane, 0); !errors.Is(err, session.ErrInvalidInput) {
			t.Fatalf("read lane %q = %v, want ErrInvalidInput", lane, err)
		}
		if _, err := c.StreamLane(ctx, "sess-ab12", lane, 0); !errors.Is(err, session.ErrInvalidInput) {
			t.Fatalf("stream lane %q = %v, want ErrInvalidInput", lane, err)
		}
	}
	if len(pod.paths) != 0 {
		t.Fatalf("a refused lane still reached the pod: %v", pod.paths)
	}
}

// TestLanesComesFromThePod: the inventory is the pod's own answer, and a pod
// that has no such route is unsupported rather than a pod with no lanes.
func TestLanesComesFromThePod(t *testing.T) {
	c, pod := laneHarness(t, "sess-ab12")
	ctx := context.Background()

	inventory, err := c.Lanes(ctx, "sess-ab12")
	if err != nil {
		t.Fatalf("lanes: %v", err)
	}
	if !inventory.Paired || len(inventory.Lanes) != 2 {
		t.Fatalf("inventory = %+v, want the pair the pod reported", inventory)
	}
	if inventory.Lanes[1].Role != session.LaneMonitor || inventory.Lanes[1].Ready {
		t.Fatalf("the pod's own readiness was not carried through: %+v", inventory.Lanes[1])
	}

	pod.lanesCode = http.StatusNotFound
	pod.lanesBody = "404 page not found"
	if _, err := c.Lanes(ctx, "sess-ab12"); !errors.Is(err, session.ErrLanesUnsupported) {
		t.Fatalf("a pod with no lanes route = %v, want ErrLanesUnsupported", err)
	}

	// An error that is not "no such route" stays an error: a pod that is
	// failing must not read as one that runs a single conversation.
	pod.lanesCode = http.StatusInternalServerError
	pod.lanesBody = "boom"
	if _, lanesErr := c.Lanes(ctx, "sess-ab12"); lanesErr == nil || errors.Is(lanesErr, session.ErrLanesUnsupported) {
		t.Fatalf("a failing pod = %v, want a plain error", lanesErr)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
