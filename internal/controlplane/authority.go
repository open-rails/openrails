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
// operator org according to LIVE AuthKit effective-permission state (not stale
// JWT claims). This is the #224 admin-authority primitive: admin writes are
// gated by the operator org + the OpenRails permission catalog, evaluated at
// request time.
//
// orgSlug should be the operator org for the resolved tenant; when empty it
// falls back to auth.operator_org_slug.
func (c *ControlPlane) HasOperatorPermission(ctx context.Context, orgSlug, userID, perm string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	slug := strings.ToLower(strings.TrimSpace(orgSlug))
	if slug == "" && c.cfg != nil && c.cfg.Auth != nil {
		slug = strings.ToLower(strings.TrimSpace(c.cfg.Auth.OperatorOrgSlug))
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
// permission in the operator org via live AuthKit state.
func (c *ControlPlane) IsOperatorAdmin(ctx context.Context, orgSlug, userID string) (bool, error) {
	return c.HasOperatorPermission(ctx, orgSlug, userID, PermAdmin)
}
