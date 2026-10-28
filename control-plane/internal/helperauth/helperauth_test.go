package helperauth

import (
	"errors"
	"net/http/httptest"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	ns  = "session-platform"
	aud = "session-platform"
)

func helperPod(name, uid, session, role string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, UID: types.UID(uid),
		Labels: map[string]string{"session-id": session, "role": role},
	}}
}

// reviewing makes the fake answer TokenReview from a table keyed by token.
func reviewing(c *fake.Clientset, answers map[string]authv1.TokenReviewStatus) {
	c.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		st, ok := answers[tr.Spec.Token]
		if !ok || len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != aud {
			st = authv1.TokenReviewStatus{}
		}
		return true, &authv1.TokenReview{Status: st}, nil
	})
}

func status(user, podName, podUID string, audiences ...string) authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     audiences,
		User: authv1.UserInfo{Username: user, Extra: map[string]authv1.ExtraValue{
			podNameExtra: {podName}, podUIDExtra: {podUID},
		}},
	}
}

func TestSessionIDFromHelperToken(t *testing.T) {
	c := fake.NewSimpleClientset(
		helperPod("sess-a-helper", "uid-a", "sess-a", "helper"),
		helperPod("sess-b", "uid-b", "sess-b", "workload"),
	)
	sa := "system:serviceaccount:" + ns + ":session-mcp"
	reviewing(c, map[string]authv1.TokenReviewStatus{
		"good":         status(sa, "sess-a-helper", "uid-a", aud),
		"wrong-aud":    status(sa, "sess-a-helper", "uid-a", "pocket-aide"),
		"other-sa":     status("system:serviceaccount:"+ns+":default", "sess-a-helper", "uid-a", aud),
		"stale-uid":    status(sa, "sess-a-helper", "uid-old", aud),
		"workload-pod": status(sa, "sess-b", "uid-b", aud),
		"no-pod":       status(sa, "", "", aud),
	})
	v := New(c, ns, "session-mcp", aud, "session-id", "role", "helper")

	call := func(header string) (string, error) {
		r := httptest.NewRequest("GET", "/x", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		return v.SessionID(r)
	}
	if id, err := call("Bearer good"); err != nil || id != "sess-a" {
		t.Fatalf("good token = %q, %v", id, err)
	}
	for _, h := range []string{"", "Basic good", "Bearer ", "Bearer unknown", "Bearer wrong-aud", "Bearer other-sa", "Bearer stale-uid", "Bearer workload-pod", "Bearer no-pod"} {
		if _, err := call(h); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: err = %v, want ErrUnauthenticated", h, err)
		}
	}
}
