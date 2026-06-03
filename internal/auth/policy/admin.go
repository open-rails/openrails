package policy

import (
	"context"
	"strings"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	log "github.com/sirupsen/logrus"
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

// PlatformSuperadminRequired gates a cross-tenant /v1/platform/* route on the
// LIVE openrails:platform:superadmin permission held in the platform org
// (issue #226). The caller must be authenticated (else 401). On denial it
// returns 403 "platform_superadmin_required". When checker is nil this is a
// configuration error and fails closed with 500.
//
// A tenant operator admin is denied here: their authority is the per-tenant
// openrails:admin permission in a tenant operator org, which is a different org
// and a different permission than the one this gate checks.
func PlatformSuperadminRequired(checker PlatformSuperadminChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		uc, ok := ginauth.UserContextFromGin(c)
		if !ok || uc.UserID == "" {
			response.UnauthorizedWithMessage(c, "authentication required")
			c.Abort()
			return
		}
		if checker == nil {
			log.Error("platform superadmin middleware misconfigured: nil checker")
			response.InternalError(c, "authorization unavailable")
			c.Abort()
			return
		}
		allowed, err := checker.HasPlatformSuperadmin(c.Request.Context(), uc.UserID)
		if err != nil {
			log.WithError(err).Error("failed to evaluate platform superadmin permission")
			response.InternalError(c, "failed to check permission")
			c.Abort()
			return
		}
		if !allowed {
			log.WithFields(log.Fields{"user_id": uc.UserID, "caller_org": uc.Org}).
				Warn("platform superadmin denied")
			response.ForbiddenWithMessage(c, "platform_superadmin_required")
			c.Abort()
			return
		}
		c.Next()
	}
}

// OperatorPermissionRequired gates a route on a LIVE OpenRails permission held
// in the operator org (#224 admin authority). It is the forward replacement for
// the global-admin fallback: admin authority comes from the operator org +
// permission catalog, evaluated live, rather than from a global `admin` role or
// trusting JWT role claims.
//
// The caller must be authenticated (else 401). The checker resolves the operator
// org from config when orgSlug is empty. On denial it returns 403
// "operator_permission_required". When checker is nil this middleware is a
// configuration error and fails closed with 500.
func OperatorPermissionRequired(checker OperatorPermissionChecker, perm string) gin.HandlerFunc {
	return func(c *gin.Context) {
		uc, ok := ginauth.UserContextFromGin(c)
		if !ok || uc.UserID == "" {
			response.UnauthorizedWithMessage(c, "authentication required")
			c.Abort()
			return
		}
		if checker == nil {
			log.Error("operator permission middleware misconfigured: nil checker")
			response.InternalError(c, "authorization unavailable")
			c.Abort()
			return
		}
		allowed, err := checker.HasOperatorPermission(c.Request.Context(), uc.Org, uc.UserID, perm)
		if err != nil {
			log.WithError(err).Error("failed to evaluate operator permission")
			response.InternalError(c, "failed to check permission")
			c.Abort()
			return
		}
		if !allowed {
			log.WithFields(log.Fields{"user_id": uc.UserID, "permission": perm}).
				Warn("operator permission denied")
			response.ForbiddenWithMessage(c, "operator_permission_required")
			c.Abort()
			return
		}
		c.Next()
	}
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

// OperatorAdminRequired ensures the caller is an admin under the operator-org
// permission model. HARDCUT (#221/#224): the legacy global-`admin`-role fallback
// against profiles.user_roles has been REMOVED. Authority is the operator org +
// its admin-equivalent roles ONLY.
//
// Gates all /admin/* routes:
//
//   - When the operator org is configured: caller must have UserContext.Org
//     equal to the configured slug AND hold one of the configured
//     admin-equivalent roles in UserContext.OrgRoles. Returns 403
//     "operator_org_required" / "operator_org_mismatch" /
//     "operator_org_role_required" depending on which condition failed.
//
//   - When the operator org is NOT configured: fails closed with 403
//     "admin_required" — there is no DB-role fallback.
//
// An unauthenticated caller gets 401 "authentication required". `db` is unused
// and retained only for call-site compatibility; pass nil.
func OperatorAdminRequired(cfg *config.Config, db bun.IDB) gin.HandlerFunc {
	return func(c *gin.Context) {
		uc, ok := ginauth.UserContextFromGin(c)
		if !ok || uc.UserID == "" {
			response.UnauthorizedWithMessage(c, "authentication required")
			c.Abort()
			return
		}

		if cfg == nil || !cfg.Auth.OperatorOrgEnabled() {
			log.WithField("user_id", uc.UserID).
				Warn("admin access denied: operator org not configured; global-admin fallback removed (#221/#224)")
			response.ForbiddenWithMessage(c, "admin_required")
			c.Abort()
			return
		}

		operatorSlug := strings.TrimSpace(cfg.Auth.OperatorOrgSlug)
		if strings.TrimSpace(uc.Org) == "" {
			log.WithField("user_id", uc.UserID).Warn("operator admin denied: org claim missing")
			response.ForbiddenWithMessage(c, "operator_org_required")
			c.Abort()
			return
		}
		if !strings.EqualFold(strings.TrimSpace(uc.Org), operatorSlug) {
			log.WithFields(log.Fields{
				"user_id":      uc.UserID,
				"caller_org":   uc.Org,
				"operator_org": operatorSlug,
			}).Warn("operator admin denied: org mismatch")
			response.ForbiddenWithMessage(c, "operator_org_mismatch")
			c.Abort()
			return
		}
		adminRoles := cfg.Auth.EffectiveOperatorOrgAdminRoles()
		if !uc.HasAnyOrgRole(adminRoles...) {
			log.WithFields(log.Fields{
				"user_id":    uc.UserID,
				"org":        uc.Org,
				"want_roles": adminRoles,
			}).Warn("operator admin denied: required org role missing")
			response.ForbiddenWithMessage(c, "operator_org_role_required")
			c.Abort()
			return
		}
		c.Next()
	}
}
