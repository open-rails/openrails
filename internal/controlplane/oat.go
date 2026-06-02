package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/pkg/tenant"
)

// ResolvedOAT is the result of validating an OpenRails-issued tenant OAT against
// live AuthKit + tenant-directory state (issue #222). It carries everything route
// authorization needs: the owning AuthKit org, the resolved OpenRails tenant, and
// the OAT's granted OpenRails permission strings.
type ResolvedOAT struct {
	// OrgSlug is the AuthKit org that owns the OAT.
	OrgSlug string
	// TenantID is the OpenRails tenant (#223) the owning org administers.
	TenantID tenant.ID
	// TenantSlug is the resolved tenant's slug.
	TenantSlug string
	// Permissions is the OAT's granted OpenRails permission catalog entries
	// (colon-form, e.g. "openrails:credits:write").
	Permissions []string
}

// HasPermission reports whether the resolved OAT grants perm. The broad
// PermAdmin grant satisfies any permission check.
func (r *ResolvedOAT) HasPermission(perm string) bool {
	perm = strings.TrimSpace(perm)
	for _, p := range r.Permissions {
		if p == perm || p == PermAdmin {
			return true
		}
	}
	return false
}

// TokenPrefix returns the configured OAT brand prefix (e.g. "openrails"), used to
// recognize and parse presented OATs. Empty -> bare "oat_" marker.
func (c *ControlPlane) TokenPrefix() string {
	if c == nil || c.cfg == nil || c.cfg.Auth == nil || c.cfg.Auth.ControlPlane == nil {
		return ""
	}
	return strings.TrimSpace(c.cfg.Auth.ControlPlane.TokenPrefix)
}

// LooksLikeOAT reports whether token carries this deployment's OAT marker. Used
// by the OAT middleware to route a Bearer credential to OAT validation rather
// than JWT verification.
func (c *ControlPlane) LooksLikeOAT(token string) bool {
	return authcore.HasOATPrefix(c.TokenPrefix(), strings.TrimSpace(token))
}

// ResolveOAT validates a presented OAT string end-to-end:
//
//   - parses the <prefix>_oat_<key_id>_<secret> shape,
//   - resolves key id + secret hash via AuthKit core (owning org, permissions,
//     expiry, revocation, org-deleted) — returns authcore.ErrAccessTokenExpired /
//     ErrAccessTokenRevoked / ErrInvalidAccessToken on those conditions,
//   - maps the owning AuthKit org slug -> OpenRails tenant (#223) via the
//     billing.tenants directory (authkit_org_slug).
//
// A cross-tenant or unknown org (no tenant directory row) yields
// ErrOATTenantUnresolved so the caller can reject the request.
func (c *ControlPlane) ResolveOAT(ctx context.Context, token string) (*ResolvedOAT, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	keyID, secret, ok := authcore.ParseOAT(c.TokenPrefix(), strings.TrimSpace(token))
	if !ok {
		return nil, authcore.ErrInvalidAccessToken
	}

	orgSlug, perms, err := c.Core().ResolveOrgAccessToken(ctx, keyID, secret)
	if err != nil {
		return nil, err
	}

	tid, tslug, err := c.tenantForOrgSlug(ctx, orgSlug)
	if err != nil {
		return nil, err
	}

	return &ResolvedOAT{
		OrgSlug:     orgSlug,
		TenantID:    tid,
		TenantSlug:  tslug,
		Permissions: perms,
	}, nil
}

// ErrOATTenantUnresolved indicates the OAT's owning AuthKit org is not mapped to
// any OpenRails tenant (no billing.tenants row, or the tenant is suspended/
// deleted). Treated as an authorization failure: the OAT cannot act on any
// tenant surface.
var ErrOATTenantUnresolved = errors.New("controlplane: oat owning org maps to no active tenant")

// tenantForOrgSlug maps an AuthKit org slug to its OpenRails tenant via the
// billing.tenants directory. The operator org administers the default tenant;
// future per-tenant orgs map to their own tenant rows. Suspended/deleted tenants
// are rejected.
func (c *ControlPlane) tenantForOrgSlug(ctx context.Context, orgSlug string) (tenant.ID, string, error) {
	orgSlug = strings.ToLower(strings.TrimSpace(orgSlug))
	if orgSlug == "" {
		return tenant.ID{}, "", ErrOATTenantUnresolved
	}
	if c.pool == nil {
		return tenant.ID{}, "", errors.New("controlplane: pgx pool unavailable for tenant resolution")
	}

	var (
		idStr  string
		slug   string
		status string
	)
	err := c.pool.QueryRow(ctx, `
		SELECT id::text, slug, status
		  FROM billing.tenants
		 WHERE lower(authkit_org_slug) = $1
		   AND deleted_at IS NULL
		 LIMIT 1
	`, orgSlug).Scan(&idStr, &slug, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tenant.ID{}, "", ErrOATTenantUnresolved
		}
		return tenant.ID{}, "", err
	}
	if status != "active" {
		return tenant.ID{}, "", ErrOATTenantUnresolved
	}

	tid, err := tenant.ParseID(idStr)
	if err != nil {
		return tenant.ID{}, "", err
	}
	return tid, slug, nil
}
