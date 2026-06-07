package policy

import (
	"context"
	"strings"

	"github.com/open-rails/openrails/pkg/authprovider"
)

// PermAdmin is the broad OpenRails admin capability evaluated live against the
// caller's OWN tenant for admin routes (#312). It mirrors controlplane.PermAdmin
// ("openrails:admin") but lives here so the gin-free route registrars
// (internal/http/routes) and the embedded core need not import
// internal/controlplane — and through it AuthKit (#284). The control-plane
// catalog references this same value (see internal/controlplane/catalog.go).
const PermAdmin = "openrails:admin"

// AdminPermissionChecker is the live AuthKit effective-permission check the
// control plane provides. HARDCUT (#312): admin authority is the openrails:admin
// permission held in the CALLER'S OWN tenant (or carried on a deployment-minted
// admin service token), evaluated at request time — NOT membership in a separate
// "operator" AuthKit tenant. The controlplane.ControlPlane satisfies this
// interface.
type AdminPermissionChecker interface {
	HasAdminPermission(ctx context.Context, tenantSlug, userID, perm string) (bool, error)
}

// PlatformSuperadminChecker is the live cross-tenant platform-superadmin check
// (issue #226). It evaluates whether a user holds openrails:platform:superadmin
// in the SEPARATE platform tenant at request time. The controlplane.ControlPlane
// satisfies this interface. It is DISTINCT from AdminPermissionChecker: the
// platform check ignores the caller's claimed tenant and always evaluates against
// the configured platform tenant, so a tenant admin (whose admin permission lives
// in their own tenant) can never pass it.
type PlatformSuperadminChecker interface {
	HasPlatformSuperadmin(ctx context.Context, userID string) (bool, error)
}

// IsLiveAdmin reports whether the given UserContext holds the live openrails:admin
// permission in its OWN tenant (#312). It is the soft-authorization helper used
// by mixed public/admin read endpoints (e.g. showing inactive catalog rows to
// admins). When checker is nil (verifier-only mode) or the caller carries no
// tenant context, it returns false — there is no operator-tenant or DB-role
// fallback.
func IsLiveAdmin(ctx context.Context, checker AdminPermissionChecker, uc authprovider.UserContext) (bool, error) {
	if checker == nil || strings.TrimSpace(uc.UserID) == "" || strings.TrimSpace(uc.Tenant) == "" {
		return false, nil
	}
	return checker.HasAdminPermission(ctx, uc.Tenant, uc.UserID, PermAdmin)
}
