package controlplane

import (
	"context"
	"errors"
	"strings"
)

// ErrNoControlPlane is returned by authority checks when the control plane is
// not configured (verifier-only mode), so callers can fall back to the legacy
// JWT/role policy explicitly rather than silently allowing or denying.
var ErrNoControlPlane = errors.New("controlplane: not configured")

// HasOperatorPermission reports whether the given user holds perm in the
// operator tenant according to LIVE AuthKit effective-permission state (not stale
// JWT claims). This is the #224 admin-authority primitive: admin writes are
// gated by the operator tenant + the OpenRails permission catalog, evaluated at
// request time.
//
// orgSlug should be the operator tenant for the resolved tenant; when empty it
// falls back to auth.operator_tenant_slug.
func (c *ControlPlane) HasOperatorPermission(ctx context.Context, orgSlug, userID, perm string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	slug := strings.ToLower(strings.TrimSpace(orgSlug))
	if slug == "" && c.cfg != nil && c.cfg.Auth != nil {
		slug = strings.ToLower(strings.TrimSpace(c.cfg.Auth.OperatorTenantSlug))
	}
	if slug == "" {
		slug = "operator"
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	return c.Core().HasPermission(ctx, slug, userID, strings.TrimSpace(perm))
}

// IsOperatorAdmin reports whether the user holds the broad openrails:admin
// permission in the operator tenant via live AuthKit state.
func (c *ControlPlane) IsOperatorAdmin(ctx context.Context, orgSlug, userID string) (bool, error) {
	return c.HasOperatorPermission(ctx, orgSlug, userID, PermAdmin)
}

// PlatformTenantSlug returns the configured managed-hosting platform-superadmin org
// slug (issue #226), or "" when no platform tenant is configured.
func (c *ControlPlane) PlatformTenantSlug() string {
	if c == nil || c.cfg == nil || c.cfg.Auth == nil || c.cfg.Auth.ControlPlane == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.cfg.Auth.ControlPlane.PlatformTenantSlug))
}

// HasPlatformSuperadmin reports whether userID holds PermPlatformSuperadmin in
// the PLATFORM org (issue #226) according to LIVE AuthKit effective-permission
// state — NOT in any tenant operator tenant. This is the cross-tenant authority
// primitive: it is what gates the /v1/platform/* surface and break-glass.
//
// It ALWAYS evaluates against the configured platform tenant slug, ignoring the
// caller's claimed org. This is what makes a tenant operator admin (who holds
// openrails:admin in a tenant operator tenant but is NOT a member of the platform
// org) fail the gate: their permission lives in the wrong org. When no platform
// org is configured it returns false (the platform surface is disabled).
func (c *ControlPlane) HasPlatformSuperadmin(ctx context.Context, userID string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	slug := c.PlatformTenantSlug()
	if slug == "" {
		return false, nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	return c.Core().HasPermission(ctx, slug, userID, PermPlatformSuperadmin)
}
