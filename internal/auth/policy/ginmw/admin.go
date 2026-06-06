// Package ginmw holds the gin middleware variants of the admin/operator
// authorization gates. It is imported only by the standalone (gin) HTTP path.
// The gin-free policy core (IsOperatorAdmin + the router.Middleware *MW helpers)
// lives in internal/auth/policy, so that gin-free importers (ultimately
// pkg/embedded) do not transitively pull in github.com/gin-gonic/gin (#285).
package ginmw

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/internal/http/response"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
)

// PlatformSuperadminRequired gates a cross-tenant /v1/platform/* route on the
// LIVE openrails:platform:superadmin permission held in the platform org
// (issue #226). The caller must be authenticated (else 401). On denial it
// returns 403 "platform_superadmin_required". When checker is nil this is a
// configuration error and fails closed with 500.
//
// A tenant operator admin is denied here: their authority is the per-tenant
// openrails:admin permission in a tenant operator org, which is a different org
// and a different permission than the one this gate checks.
func PlatformSuperadminRequired(checker policy.PlatformSuperadminChecker) gin.HandlerFunc {
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
func OperatorPermissionRequired(checker policy.OperatorPermissionChecker, perm string) gin.HandlerFunc {
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

// OperatorAdminRequired ensures the caller is an admin under the operator-org
// permission model. HARDCUT (#221/#224): the legacy global-`admin`-role fallback
// against profiles.user_roles has been REMOVED. Authority is the operator org +
// its admin-equivalent roles ONLY.
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
