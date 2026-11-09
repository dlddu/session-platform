// Package tenancy decides which tenant a control-plane request speaks for
// (docs/prd/multitenancy.md, AC-K1~K5). A tenant is identified by the
// Kubernetes ServiceAccount whose projected token the caller presents; a
// request with no bearer token is the operator, who sees every tenant.
package tenancy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Tenant is one configured tenant: the ServiceAccounts that speak for it and
// the workload types its sessions may run (AC-K3).
type Tenant struct {
	Name            string                 `json:"name"`
	ServiceAccounts []string               `json:"serviceAccounts"`
	WorkloadTypes   []session.WorkloadType `json:"workloadTypes"`
}

// AnyWorkloadType, as a tenant's only workload type, grants every type the
// deployment offers — including types added after the tenant was configured.
const AnyWorkloadType session.WorkloadType = "*"

// Allows reports whether the tenant may create sessions of type w.
func (t *Tenant) Allows(w session.WorkloadType) bool {
	return slices.Contains(t.WorkloadTypes, AnyWorkloadType) || slices.Contains(t.WorkloadTypes, w)
}

// Config is the validated tenant set, indexed both ways.
type Config struct {
	tenants   []*Tenant
	byName    map[string]*Tenant
	byAccount map[string]*Tenant
}

type configFile struct {
	Tenants []Tenant `json:"tenants"`
}

var (
	tenantNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// accountPattern is "<namespace>/<serviceaccount>", both DNS-1123 names.
	accountPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// Parse reads the SESSION_PLATFORM_TENANTS document. Blank means no tenants:
// every bearer caller is then refused and the operator view is unchanged.
// Anything malformed is a startup error rather than a silently wider grant.
func Parse(raw string) (*Config, error) {
	cfg := &Config{byName: map[string]*Tenant{}, byAccount: map[string]*Tenant{}}
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file configFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("tenant config: %w", err)
	}
	for i := range file.Tenants {
		t := file.Tenants[i]
		if !tenantNamePattern.MatchString(t.Name) {
			return nil, fmt.Errorf("tenant config: tenant name %q is not a DNS label", t.Name)
		}
		if _, dup := cfg.byName[t.Name]; dup {
			return nil, fmt.Errorf("tenant config: duplicate tenant %q", t.Name)
		}
		if len(t.ServiceAccounts) == 0 {
			return nil, fmt.Errorf("tenant config: tenant %q names no serviceAccounts", t.Name)
		}
		for _, account := range t.ServiceAccounts {
			if !accountPattern.MatchString(account) {
				return nil, fmt.Errorf("tenant config: tenant %q: %q is not <namespace>/<serviceaccount>", t.Name, account)
			}
			if owner, dup := cfg.byAccount[account]; dup {
				return nil, fmt.Errorf("tenant config: serviceAccount %q is claimed by both %q and %q", account, owner.Name, t.Name)
			}
			cfg.byAccount[account] = &file.Tenants[i]
		}
		if slices.Contains(t.WorkloadTypes, AnyWorkloadType) && len(t.WorkloadTypes) != 1 {
			return nil, fmt.Errorf("tenant config: tenant %q: %q must be the only workload type", t.Name, AnyWorkloadType)
		}
		seen := map[session.WorkloadType]bool{}
		for _, w := range t.WorkloadTypes {
			if w == AnyWorkloadType {
				continue
			}
			if w == "" || !w.Valid() {
				return nil, fmt.Errorf("tenant config: tenant %q: unknown workload type %q", t.Name, w)
			}
			if seen[w] {
				return nil, fmt.Errorf("tenant config: tenant %q lists workload type %q twice", t.Name, w)
			}
			seen[w] = true
		}
		cfg.byName[t.Name] = &file.Tenants[i]
		cfg.tenants = append(cfg.tenants, &file.Tenants[i])
	}
	return cfg, nil
}

// Tenants returns the tenants in configuration order.
func (c *Config) Tenants() []*Tenant {
	if c == nil {
		return nil
	}
	return slices.Clone(c.tenants)
}

// Lookup returns the tenant named name.
func (c *Config) Lookup(name string) (*Tenant, bool) {
	if c == nil {
		return nil, false
	}
	t, ok := c.byName[name]
	return t, ok
}

// ForServiceAccount returns the tenant that namespace/name speaks for.
func (c *Config) ForServiceAccount(namespace, name string) (*Tenant, bool) {
	if c == nil {
		return nil, false
	}
	t, ok := c.byAccount[namespace+"/"+name]
	return t, ok
}

// Caller is who a request speaks for. Exactly one of Operator and Tenant holds.
type Caller struct {
	Operator bool
	Tenant   *Tenant
}

// OperatorCaller is the caller of a request without a bearer token (AC-K4).
var OperatorCaller = Caller{Operator: true}

// Sees reports whether the caller may see a session owned by tenant (AC-K2):
// the operator sees every session, a tenant only its own.
func (c Caller) Sees(tenant string) bool {
	return c.Operator || (c.Tenant != nil && c.Tenant.Name == tenant)
}

// ErrNoTenant reports a proven ServiceAccount that no tenant claims.
var ErrNoTenant = errors.New("service account belongs to no tenant")
