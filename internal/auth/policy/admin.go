package policy

import (
	"context"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/uptrace/bun"
)

// PermOperatorAdmin is the broad OpenRails operator/admin capability evaluated
// live against the operator tenant for admin routes (#224). It mirrors
// controlplane.PermAdmin ("openrails:admin") but lives here so the gin-free route
// registrars (internal/http/routes) and the embedded core need not import
// internal/controlplane — and through it AuthKit (#284). The control-plane
// catalog references this same value (see internal/controlplane/catalog.go).
const PermOperatorAdmin = "openrails:admin"

// OperatorPermissionChecker is the live AuthKit effective-permission check the
// control plane provides (#224). It evaluates whether a user holds a permission
// in the operator tenant at request time (not from stale JWT claims). The
// controlplane.ControlPlane satisfies this interface.
type OperatorPermissionChecker interface {
	HasOperatorPermission(ctx context.Context, orgSlug, userID, perm string) (bool, error)
}

// PlatformSuperadminChecker is the live cross-tenant platform-superadmin check
// (issue #226). It evaluates whether a user holds openrails:platform:superadmin
// in the SEPARATE platform tenant at request time. The controlplane.ControlPlane
// satisfies this interface. It is DISTINCT from OperatorPermissionChecker:
// the platform check ignores the caller's claimed org and always evaluates
// against the configured platform tenant, so a tenant operator admin (whose admin
// permission lives in a tenant operator tenant) can never pass it.
type PlatformSuperadminChecker interface {
	HasPlatformSuperadmin(ctx context.Context, userID string) (bool, error)
}

// IsOperatorAdmin reports whether the given UserContext is an OpenRails billing
// admin. HARDCUT (#221/#224): admin authority is the operator-tenant permission
// model ONLY. The legacy global-admin fallback (the live `admin` role in
// profiles.user_roles / profiles.roles) has been REMOVED — there is no DB-role
// path.
//
// A caller is an admin iff uc.Tenant equals the configured operator tenant slug AND
// uc.TenantRoles contains one of cfg.Auth.EffectiveOperatorTenantAdminRoles
// (case-insensitive). When no operator tenant is configured there is no admin by
// this check (deployments must configure an operator tenant).
//
// `db` is unused and retained only for call-site compatibility; pass nil.
func IsOperatorAdmin(ctx context.Context, cfg *config.Config, db bun.IDB, uc authprovider.UserContext) (bool, error) {
	if cfg == nil || !cfg.Auth.OperatorTenantEnabled() {
		return false, nil
	}
	operatorSlug := cfg.Auth.EffectiveOperatorTenantSlug()
	if !strings.EqualFold(strings.TrimSpace(uc.Tenant), operatorSlug) {
		return false, nil
	}
	adminRoles := cfg.Auth.EffectiveOperatorTenantAdminRoles()
	return uc.HasAnyTenantRole(adminRoles...), nil
}
