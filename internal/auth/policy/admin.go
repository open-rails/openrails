package policy

import (
	"context"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/uptrace/bun"
)

// OperatorPermissionChecker is the live AuthKit effective-permission check the
// control plane provides (#224). It evaluates whether a user holds a permission
// in the operator org at request time (not from stale JWT claims). The
// controlplane.ControlPlane satisfies this interface.
type OperatorPermissionChecker interface {
	HasOperatorPermission(ctx context.Context, orgSlug, userID, perm string) (bool, error)
}

// PlatformSuperadminChecker is the live cross-tenant platform-superadmin check
// (issue #226). It evaluates whether a user holds openrails:platform:superadmin
// in the SEPARATE platform org at request time. The controlplane.ControlPlane
// satisfies this interface. It is DISTINCT from OperatorPermissionChecker:
// the platform check ignores the caller's claimed org and always evaluates
// against the configured platform org, so a tenant operator admin (whose admin
// permission lives in a tenant operator org) can never pass it.
type PlatformSuperadminChecker interface {
	HasPlatformSuperadmin(ctx context.Context, userID string) (bool, error)
}

// IsOperatorAdmin reports whether the given UserContext is an OpenRails billing
// admin. HARDCUT (#221/#224): admin authority is the operator-org permission
// model ONLY. The legacy global-admin fallback (the live `admin` role in
// profiles.user_roles / profiles.roles) has been REMOVED — there is no DB-role
// path.
//
// A caller is an admin iff uc.Org equals the configured operator org slug AND
// uc.OrgRoles contains one of cfg.Auth.EffectiveOperatorOrgAdminRoles
// (case-insensitive). When no operator org is configured there is no admin by
// this check (deployments must configure an operator org).
//
// `db` is unused and retained only for call-site compatibility; pass nil.
func IsOperatorAdmin(ctx context.Context, cfg *config.Config, db bun.IDB, uc authprovider.UserContext) (bool, error) {
	if cfg == nil || !cfg.Auth.OperatorOrgEnabled() {
		return false, nil
	}
	operatorSlug := strings.TrimSpace(cfg.Auth.OperatorOrgSlug)
	if !strings.EqualFold(strings.TrimSpace(uc.Org), operatorSlug) {
		return false, nil
	}
	adminRoles := cfg.Auth.EffectiveOperatorOrgAdminRoles()
	return uc.HasAnyOrgRole(adminRoles...), nil
}
