package controlplane

import (
	"context"
	"errors"
	"strings"
)

// ErrNoControlPlane is returned by authority checks invoked on a nil control
// plane (an embedded host that never attached one), so callers fail closed
// explicitly rather than silently allowing or denying.
var ErrNoControlPlane = errors.New("controlplane: not configured")

// HasAdminPermission reports whether the given user holds perm in the caller's
// OWN AuthKit org according to LIVE AuthKit effective-permission state (not
// stale JWT claims). This is the #312 deploy-authority primitive: admin writes
// are gated by the openrails:admin permission held in the caller's tenant + the
// OpenRails permission catalog, evaluated at request time — NOT by membership in
// a separate "operator" AuthKit org.
//
// tenantSlug is the caller's tenant. An empty slug yields no authority (there is
// no operator-tenant fallback): the caller must present a tenant context.
func (c *ControlPlane) HasAdminPermission(ctx context.Context, tenantSlug, userID, perm string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	slug := strings.ToLower(strings.TrimSpace(tenantSlug))
	if slug == "" {
		return false, nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	return c.Core().HasPermission(ctx, slug, userID, strings.TrimSpace(perm))
}

// IsAdmin reports whether the user holds the broad openrails:admin permission in
// their own AuthKit org via live AuthKit state.
func (c *ControlPlane) IsAdmin(ctx context.Context, tenantSlug, userID string) (bool, error) {
	return c.HasAdminPermission(ctx, tenantSlug, userID, PermAdmin)
}

// PlatformOrgSlug returns the configured managed-hosting platform-superadmin org
// slug (issue #226), or "" when no platform org is configured.
func (c *ControlPlane) PlatformOrgSlug() string {
	if c == nil || c.cfg == nil || c.cfg.Auth == nil || c.cfg.Auth.ControlPlane == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.cfg.Auth.ControlPlane.PlatformOrgSlug))
}

// HasPlatformSuperadmin reports whether userID holds PermPlatformSuperadmin in
// the platform org (issue #226) according to LIVE AuthKit effective-permission
// state — NOT in any org operator org. This is the cross-tenant authority
// primitive: it is what gates the /v1/platform/* surface.
//
// It ALWAYS evaluates against the configured platform org slug, ignoring the
// caller's claimed org. This is what makes a org operator admin (who holds
// openrails:admin in a org operator org but is NOT a member of the platform
// tenant) fail the gate: their permission lives in the wrong tenant. When no platform
// tenant is configured it returns false (the platform surface is disabled).
func (c *ControlPlane) HasPlatformSuperadmin(ctx context.Context, userID string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	slug := c.PlatformOrgSlug()
	if slug == "" {
		return false, nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	return c.Core().HasPermission(ctx, slug, userID, PermPlatformSuperadmin)
}
