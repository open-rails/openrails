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
// stale JWT claims). Merchant-local authority is `org:` authority in the
// caller's merchant org.
//
// orgSlug is the caller's org. An empty slug yields no authority (there is
// no operator-org fallback): the caller must present an org context.
func (c *ControlPlane) HasAdminPermission(ctx context.Context, orgSlug, userID, perm string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	slug := strings.ToLower(strings.TrimSpace(orgSlug))
	if slug == "" {
		return false, nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	return c.Core().HasPermission(ctx, slug, userID, strings.TrimSpace(perm))
}

// IsAdmin reports whether the user holds a broad merchant-owner grant in their
// own AuthKit org via live AuthKit state.
func (c *ControlPlane) IsAdmin(ctx context.Context, orgSlug, userID string) (bool, error) {
	perms, err := c.Core().EffectivePermissions(ctx, orgSlug, userID)
	if err != nil {
		return false, err
	}
	for _, p := range perms {
		if strings.HasPrefix(p, "org:") {
			return true, nil
		}
	}
	return false, nil
}

// PlatformOrgSlug returns "" because OpenRails no longer supports a config-seeded
// platform-superadmin org. Cross-merchant SaaS administration belongs in the
// private openrails-saas layer, not the source-available self-hosted control plane.
func (c *ControlPlane) PlatformOrgSlug() string {
	return ""
}

// HasPlatformSuperadmin reports whether userID holds the concrete platform
// permission used by the OpenRails platform directory routes according to
// AuthKit's platform RBAC layer.
func (c *ControlPlane) HasPlatformSuperadmin(ctx context.Context, userID string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	return c.Core().HasPlatformPermission(ctx, userID, PermPlatformSuperadmin)
}
