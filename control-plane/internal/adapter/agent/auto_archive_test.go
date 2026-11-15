package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
)

func TestHTTPClientAutoArchiveTargetRoundTrip(t *testing.T) {
	var seen []string
	var body struct {
		URL       string            `json:"url"`
		ExpiresAt string            `json:"expiresAt"`
		Headers   map[string]string `json:"headers"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPut {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-a", Namespace: "sessions"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host},
	})
	c := agent.NewHTTPClient(cs, "sessions", agent.WithPort(port))

	expires := time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	if err := c.SetAutoArchiveTarget(context.Background(), "sess-a", "https://b.s3/k?sig=1", expires,
		map[string]string{"X-Amz-Meta-Writer": "pod:sess-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := c.ClearAutoArchiveTarget(context.Background(), "sess-a"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	want := []string{"PUT " + agent.AutoArchiveTargetPath, "DELETE " + agent.AutoArchiveTargetPath}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	if body.URL != "https://b.s3/k?sig=1" || body.ExpiresAt != "2026-11-15T11:00:00Z" || body.Headers["X-Amz-Meta-Writer"] != "pod:sess-a" {
		t.Fatalf("pushed body = %v", body)
	}
}
