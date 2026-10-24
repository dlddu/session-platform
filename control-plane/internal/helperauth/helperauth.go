// Package helperauth tells the control plane which session's helper pod is
// calling its internal repo access API (docs/prd/repo-access.md, AC-H3). The
// caller presents a projected service-account token minted for this audience
// only; TokenReview proves it, and the pod it is bound to names the session.
package helperauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	podNameExtra = "authentication.kubernetes.io/pod-name"
	podUIDExtra  = "authentication.kubernetes.io/pod-uid"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type Verifier struct {
	client         kubernetes.Interface
	namespace      string
	serviceAccount string
	audience       string
	sessionLabel   string
	roleLabel      string
	helperRole     string
}

// New builds a verifier for tokens of serviceAccount in namespace, minted for
// audience, whose bound pod carries sessionLabel and roleLabel=helperRole.
func New(client kubernetes.Interface, namespace, serviceAccount, audience, sessionLabel, roleLabel, helperRole string) *Verifier {
	return &Verifier{
		client: client, namespace: namespace, serviceAccount: serviceAccount, audience: audience,
		sessionLabel: sessionLabel, roleLabel: roleLabel, helperRole: helperRole,
	}
}

// SessionID returns the session whose helper pod holds the request's bearer
// token. Any doubt is ErrUnauthenticated; only infrastructure faults are other
// errors.
func (v *Verifier) SessionID(r *http.Request) (string, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", ErrUnauthenticated
	}
	ctx := r.Context()
	review, err := v.client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{v.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	st := review.Status
	if !st.Authenticated || !slices.Contains(st.Audiences, v.audience) {
		return "", ErrUnauthenticated
	}
	if st.User.Username != "system:serviceaccount:"+v.namespace+":"+v.serviceAccount {
		return "", ErrUnauthenticated
	}
	podName, podUID := first(st.User.Extra[podNameExtra]), first(st.User.Extra[podUIDExtra])
	if podName == "" || podUID == "" {
		return "", ErrUnauthenticated
	}
	return v.sessionOfPod(ctx, podName, podUID)
}

// sessionOfPod rejects a token whose pod has since been replaced under the
// same name — the UID is what the token was bound to.
func (v *Verifier) sessionOfPod(ctx context.Context, name, uid string) (string, error) {
	pod, err := v.client.CoreV1().Pods(v.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", ErrUnauthenticated
	}
	if string(pod.UID) != uid || pod.Labels[v.roleLabel] != v.helperRole {
		return "", ErrUnauthenticated
	}
	id := pod.Labels[v.sessionLabel]
	if id == "" {
		return "", ErrUnauthenticated
	}
	return id, nil
}

func first(vs authv1.ExtraValue) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}
