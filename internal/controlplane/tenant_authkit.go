package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"
)

// EnsureAuthKitTenant idempotently ensures an AuthKit tenant for orgSlug exists and is
// seeded with the OpenRails operator role + full permission catalog, returning
// the org id. It is the per-tenant analogue of Bootstrap's default-tenant org
// step (issue #225): the tenancy lifecycle service calls this to create/link a
// new tenant's operator tenant through in-process AuthKit CORE calls — never raw
// AuthKit SQL or a private HTTP route.
//
// Re-running with the same slug returns the existing org id without creating a
// duplicate (CreateTenant is only attempted when ResolveTenantBySlug misses; DefineRole
// upserts and SetRolePermissions replaces), so it is safe in the idempotent
// Provision path. It satisfies tenancy.TenantProvisioner.
func (c *ControlPlane) EnsureAuthKitTenant(ctx context.Context, orgSlug string) (string, error) {
	if c == nil {
		return "", errors.New("controlplane: nil control plane")
	}
	core := c.Core()
	if core == nil {
		return "", errors.New("controlplane: core service unavailable")
	}
	slug := strings.ToLower(strings.TrimSpace(orgSlug))
	if slug == "" {
		return "", errors.New("controlplane: tenant org slug is required")
	}

	org, err := core.ResolveTenantBySlug(ctx, slug)
	if err != nil {
		if !errors.Is(err, authcore.ErrTenantNotFound) {
			return "", fmt.Errorf("controlplane: resolve tenant org %q: %w", slug, err)
		}
		created, cerr := core.CreateTenant(ctx, slug)
		if cerr != nil {
			return "", fmt.Errorf("controlplane: create tenant org %q: %w", slug, cerr)
		}
		org = created
		log.WithField("tenant_org", slug).Info("controlplane: created tenant operator tenant")
	}

	if err := core.DefineRole(ctx, slug, OperatorRole); err != nil {
		return "", fmt.Errorf("controlplane: define operator role on tenant org: %w", err)
	}
	if err := core.SetRolePermissions(ctx, slug, OperatorRole, OperatorRolePermissions()); err != nil {
		return "", fmt.Errorf("controlplane: seed operator role permissions on tenant org: %w", err)
	}
	return org.ID, nil
}
