package tenancy_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/tenancy"
)

const sample = `{"tenants":[
 {"name":"pocket-aide","serviceAccounts":["pocket-aide/pocket-aide"],"workloadTypes":["codex","claude-code"]},
 {"name":"lab","serviceAccounts":["lab/runner","lab/ci"],"workloadTypes":["shell"]}
]}`

func TestParse(t *testing.T) {
	cfg, err := tenancy.Parse(sample)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := len(cfg.Tenants()); n != 2 {
		t.Fatalf("tenants = %d", n)
	}
	got, ok := cfg.ForServiceAccount("lab", "ci")
	if !ok || got.Name != "lab" || !got.Allows(session.WorkloadTypeShell) || got.Allows(session.WorkloadTypeCodex) {
		t.Fatalf("lab/ci = %+v, %v", got, ok)
	}
	if _, ok := cfg.ForServiceAccount("lab", "other"); ok {
		t.Fatal("unlisted ServiceAccount resolved to a tenant")
	}
	if t2, ok := cfg.Lookup("pocket-aide"); !ok || !t2.Allows(session.WorkloadTypeCodex) {
		t.Fatalf("lookup pocket-aide = %+v, %v", t2, ok)
	}
}

func TestParseWildcardGrantsEveryType(t *testing.T) {
	cfg, err := tenancy.Parse(`{"tenants":[{"name":"lab","serviceAccounts":["lab/a","other/b"],"workloadTypes":["*"]}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lab, _ := cfg.Lookup("lab")
	for _, w := range []session.WorkloadType{session.WorkloadTypeShell, session.WorkloadTypeClaudeCode, session.WorkloadTypeMiniMaxCode, session.WorkloadTypeCodex, session.WorkloadTypeApprovalGated} {
		if !lab.Allows(w) {
			t.Errorf("wildcard tenant refuses %q", w)
		}
	}
	if got, ok := cfg.ForServiceAccount("other", "b"); !ok || got != lab {
		t.Errorf("second account of the tenant = %+v, %v", got, ok)
	}
}

func TestParseEmptyMeansNoTenants(t *testing.T) {
	for _, raw := range []string{"", "  \n"} {
		cfg, err := tenancy.Parse(raw)
		if err != nil || len(cfg.Tenants()) != 0 {
			t.Fatalf("Parse(%q) = %v, %v", raw, cfg.Tenants(), err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":           `{`,
		"unknown field":      `{"tenants":[],"extra":1}`,
		"bad name":           `{"tenants":[{"name":"Bad_Name","serviceAccounts":["a/b"],"workloadTypes":[]}]}`,
		"duplicate name":     `{"tenants":[{"name":"a","serviceAccounts":["a/b"]},{"name":"a","serviceAccounts":["a/c"]}]}`,
		"no accounts":        `{"tenants":[{"name":"a","serviceAccounts":[]}]}`,
		"bad account":        `{"tenants":[{"name":"a","serviceAccounts":["just-a-name"]}]}`,
		"shared account":     `{"tenants":[{"name":"a","serviceAccounts":["n/sa"]},{"name":"b","serviceAccounts":["n/sa"]}]}`,
		"unknown type":       `{"tenants":[{"name":"a","serviceAccounts":["n/sa"],"workloadTypes":["vim"]}]}`,
		"duplicate type":     `{"tenants":[{"name":"a","serviceAccounts":["n/sa"],"workloadTypes":["shell","shell"]}]}`,
		"empty type literal": `{"tenants":[{"name":"a","serviceAccounts":["n/sa"],"workloadTypes":[""]}]}`,
		"wildcard with type": `{"tenants":[{"name":"a","serviceAccounts":["n/sa"],"workloadTypes":["*","shell"]}]}`,
		"wildcard twice":     `{"tenants":[{"name":"a","serviceAccounts":["n/sa"],"workloadTypes":["*","*"]}]}`,
	} {
		if _, err := tenancy.Parse(raw); err == nil {
			t.Errorf("%s: Parse accepted %s", name, raw)
		}
	}
}

func TestCallerSees(t *testing.T) {
	cfg, _ := tenancy.Parse(sample)
	lab, _ := cfg.Lookup("lab")
	if !tenancy.OperatorCaller.Sees("") || !tenancy.OperatorCaller.Sees("lab") {
		t.Error("operator must see every session")
	}
	c := tenancy.Caller{Tenant: lab}
	if !c.Sees("lab") || c.Sees("pocket-aide") || c.Sees("") {
		t.Error("a tenant sees exactly its own sessions")
	}
}

func reviewClient(status authv1.TokenReviewStatus, seen *authv1.TokenReviewSpec) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		if seen != nil {
			*seen = review.Spec
		}
		review.Status = status
		return true, review, nil
	})
	return cs
}

func request(header string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

func TestResolverWithoutTokenIsOperator(t *testing.T) {
	cfg, _ := tenancy.Parse(sample)
	resolver := tenancy.NewTokenReviewResolver(reviewClient(authv1.TokenReviewStatus{}, nil), "", cfg)
	caller, err := resolver.Resolve(request(""))
	if err != nil || !caller.Operator {
		t.Fatalf("caller = %+v, %v; want the operator", caller, err)
	}
}

func TestResolverMapsServiceAccountToTenant(t *testing.T) {
	cfg, _ := tenancy.Parse(sample)
	var spec authv1.TokenReviewSpec
	cs := reviewClient(authv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{tenancy.DefaultAudience},
		User:          authv1.UserInfo{Username: "system:serviceaccount:pocket-aide:pocket-aide"},
	}, &spec)
	caller, err := tenancy.NewTokenReviewResolver(cs, "", cfg).Resolve(request("Bearer tok"))
	if err != nil || caller.Operator || caller.Tenant == nil || caller.Tenant.Name != "pocket-aide" {
		t.Fatalf("caller = %+v, %v", caller, err)
	}
	if spec.Token != "tok" || len(spec.Audiences) != 1 || spec.Audiences[0] != tenancy.DefaultAudience {
		t.Fatalf("review spec = %+v, want the token for audience %q", spec, tenancy.DefaultAudience)
	}
}

func TestResolverRefusals(t *testing.T) {
	cfg, _ := tenancy.Parse(sample)
	sa := authv1.UserInfo{Username: "system:serviceaccount:lab:runner"}
	cases := []struct {
		name   string
		header string
		status authv1.TokenReviewStatus
		want   error
	}{
		{"not bearer", "Basic abc", authv1.TokenReviewStatus{}, session.ErrUnauthenticated},
		{"empty bearer", "Bearer  ", authv1.TokenReviewStatus{}, session.ErrUnauthenticated},
		{"unauthenticated", "Bearer t", authv1.TokenReviewStatus{Authenticated: false}, session.ErrUnauthenticated},
		{"wrong audience", "Bearer t", authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"https://kubernetes.default.svc"}, User: sa}, session.ErrUnauthenticated},
		{"not a service account", "Bearer t", authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"session-platform"}, User: authv1.UserInfo{Username: "alice"}}, session.ErrUnauthenticated},
		{"unmapped account", "Bearer t", authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"session-platform"}, User: authv1.UserInfo{Username: "system:serviceaccount:lab:other"}}, session.ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := tenancy.NewTokenReviewResolver(reviewClient(tc.status, nil), "", cfg)
			caller, err := resolver.Resolve(request(tc.header))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if caller.Operator {
				t.Fatal("a refused bearer request must never become the operator")
			}
		})
	}
}

func TestResolverEmptyHeaderIsNotOperator(t *testing.T) {
	cfg, _ := tenancy.Parse(sample)
	r := request("")
	r.Header["Authorization"] = []string{""}
	if _, err := tenancy.NewTokenReviewResolver(reviewClient(authv1.TokenReviewStatus{}, nil), "", cfg).Resolve(r); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("err = %v: a present-but-empty Authorization header must not fall back to the operator", err)
	}
}
