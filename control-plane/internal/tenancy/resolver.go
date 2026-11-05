package tenancy

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// DefaultAudience is the audience a tenant's projected token must be minted
// for. A token minted for the API server (the default audience) is refused,
// so a workload's ambient ServiceAccount token never doubles as a tenant
// credential (AC-K1).
const DefaultAudience = "session-platform"

const serviceAccountPrefix = "system:serviceaccount:"

// Resolver turns a request into its Caller (AC-K1, AC-K4).
type Resolver interface {
	Resolve(r *http.Request) (Caller, error)
}

// TokenReviewResolver proves bearer tokens with the TokenReview API.
type TokenReviewResolver struct {
	client   kubernetes.Interface
	audience string
	config   *Config
}

// NewTokenReviewResolver builds a resolver over cfg. An empty audience means
// DefaultAudience.
func NewTokenReviewResolver(client kubernetes.Interface, audience string, cfg *Config) *TokenReviewResolver {
	if strings.TrimSpace(audience) == "" {
		audience = DefaultAudience
	}
	return &TokenReviewResolver{client: client, audience: audience, config: cfg}
}

// Resolve returns OperatorCaller for a request without an Authorization
// header. A present header must be a bearer token TokenReview proves for this
// audience and whose ServiceAccount a tenant claims; anything less is
// session.ErrUnauthenticated or session.ErrForbidden, never the operator.
func (v *TokenReviewResolver) Resolve(r *http.Request) (Caller, error) {
	header, present := r.Header["Authorization"]
	if !present {
		return OperatorCaller, nil
	}
	token, ok := strings.CutPrefix(strings.Join(header, ""), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return Caller{}, session.ErrUnauthenticated
	}
	namespace, name, err := v.review(r.Context(), token)
	if err != nil {
		return Caller{}, err
	}
	tenant, ok := v.config.ForServiceAccount(namespace, name)
	if !ok {
		return Caller{}, fmt.Errorf("%w: %s/%s: %w", session.ErrForbidden, namespace, name, ErrNoTenant)
	}
	return Caller{Tenant: tenant}, nil
}

func (v *TokenReviewResolver) review(ctx context.Context, token string) (string, string, error) {
	review, err := v.client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{v.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", "", fmt.Errorf("token review: %w", err)
	}
	st := review.Status
	if !st.Authenticated || !slices.Contains(st.Audiences, v.audience) {
		return "", "", session.ErrUnauthenticated
	}
	rest, ok := strings.CutPrefix(st.User.Username, serviceAccountPrefix)
	if !ok {
		return "", "", session.ErrUnauthenticated
	}
	namespace, name, ok := strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" || strings.Contains(name, ":") {
		return "", "", session.ErrUnauthenticated
	}
	return namespace, name, nil
}

// StaticResolver resolves every request to one caller. Tests and deployments
// without tenancy use it.
type StaticResolver Caller

// Resolve implements Resolver.
func (s StaticResolver) Resolve(*http.Request) (Caller, error) { return Caller(s), nil }
