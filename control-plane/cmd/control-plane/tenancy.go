package main

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/tenancy"
)

// tenancyConfig is docs/prd/multitenancy.md's deployment input: the tenant
// set (AC-K3) and the audience a tenant token must be minted for (AC-K1).
type tenancyConfig struct {
	tenants  *tenancy.Config
	audience string
}

func loadTenancyConfig() (tenancyConfig, error) {
	tenants, err := tenancy.Parse(os.Getenv("SESSION_PLATFORM_TENANTS"))
	if err != nil {
		return tenancyConfig{}, fmt.Errorf("SESSION_PLATFORM_TENANTS: %w", err)
	}
	return tenancyConfig{tenants: tenants, audience: env("TENANT_TOKEN_AUDIENCE", tenancy.DefaultAudience)}, nil
}

func (c tenancyConfig) apiOption(client kubernetes.Interface) api.Option {
	return api.WithTenancy(tenancy.NewTokenReviewResolver(client, c.audience, c.tenants), c.tenants)
}
