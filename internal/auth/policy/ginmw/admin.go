// Package ginmw holds the gin middleware variants of the admin authorization
// gates. It is imported only by the standalone (gin) HTTP path. The gin-free
// policy core (IsLiveAdmin + the router.Middleware *MW helpers) lives in
// internal/auth/policy, so that gin-free importers (ultimately pkg/embedded) do
// not transitively pull in github.com/gin-gonic/gin (#285).
package ginmw

import (
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/internal/http/response"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
)

// PlatformSuperadminRequired gates a cross-tenant /v1/platform/* route on the
// LIVE openrails:platform:superadmin permission held in the platform tenant
// (issue #226). The caller must be authenticated (else 401). On denial it
// returns 403 "platform_superadmin_required". When checker is nil this is a
// configuration error and fails closed with 500.
//
// A tenant operator admin is denied here: their authority is the per-tenant
// openrails:admin permission in a tenant operator tenant, which is a different org
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
			log.WithFields(log.Fields{"user_id": uc.UserID, "caller_org": uc.Tenant}).
				Warn("platform superadmin denied")
			response.ForbiddenWithMessage(c, "platform_superadmin_required")
			c.Abort()
			return
		}
		c.Next()
	}
}

// AdminPermissionRequired gates a route on a LIVE OpenRails permission held in
// the CALLER'S OWN tenant (#312 deploy authority). HARDCUT: admin authority is
// the openrails:admin permission evaluated live against the caller's tenant +
// the OpenRails permission catalog — NOT membership in a separate "operator"
// AuthKit tenant, and NOT a JWT role claim. A deployment-minted admin service
// token reaches the equivalent service routes via RequireServiceTokenPermission.
//
// The caller must be authenticated (else 401). On denial it returns 403
// "admin_permission_required". When checker is nil this middleware is a
// configuration error and fails closed with 500 — a deployment that mounts admin
// routes must wire the live permission checker (the control plane).
func AdminPermissionRequired(checker policy.AdminPermissionChecker, perm string) gin.HandlerFunc {
	return func(c *gin.Context) {
		uc, ok := ginauth.UserContextFromGin(c)
		if !ok || uc.UserID == "" {
			response.UnauthorizedWithMessage(c, "authentication required")
			c.Abort()
			return
		}
		if checker == nil {
			log.Error("admin permission middleware misconfigured: nil checker")
			response.InternalError(c, "authorization unavailable")
			c.Abort()
			return
		}
		allowed, err := checker.HasAdminPermission(c.Request.Context(), uc.Tenant, uc.UserID, perm)
		if err != nil {
			log.WithError(err).Error("failed to evaluate admin permission")
			response.InternalError(c, "failed to check permission")
			c.Abort()
			return
		}
		if !allowed {
			log.WithFields(log.Fields{"user_id": uc.UserID, "permission": perm}).
				Warn("admin permission denied")
			response.ForbiddenWithMessage(c, "admin_permission_required")
			c.Abort()
			return
		}
		c.Next()
	}
}
