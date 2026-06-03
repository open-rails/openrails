package ginmw

import (
	"context"
	"errors"
	"strings"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Gin context keys for a resolved OpenRails-issued tenant OAT (issue #222).
const (
	// OATContextKey holds the *controlplane.ResolvedOAT for the request.
	OATContextKey = "billing.oat"
	// OATOrgSlugContextKey holds the owning AuthKit org slug.
	OATOrgSlugContextKey = "billing.oat_org_slug"
)

// OATResolver validates a presented OpenRails-issued OAT against live AuthKit +
// tenant-directory state. The control plane implements it; tests can inject a
// fake. nil resolver => the deployment is not OAT-capable (verifier-only mode).
type OATResolver interface {
	// LooksLikeOAT reports whether token carries this deployment's OAT marker.
	LooksLikeOAT(token string) bool
	// ResolveOAT validates the OAT and resolves its tenant + permissions.
	ResolveOAT(ctx context.Context, token string) (*controlplane.ResolvedOAT, error)
}

// OATRequired authenticates a tenant-scoped public route with an OpenRails-issued
// tenant OAT (issue #222). It REPLACES the retired private/mTLS/api-key service
// surface: machine/server callers (Doujins/Hentai0/Tensorhub reserving credits,
// reading entitlements, etc.) present `Authorization: Bearer <openrails_oat_...>`
// against public tenant routes.
//
// On success it pins the resolved tenant onto the request context (overriding the
// default single-tenant resolution) and records the OAT's permissions for
// downstream RequireOATPermission gates. Expired, revoked, unknown, or
// cross-tenant/unmapped OATs are rejected.
//
// resolver must be non-nil; routes are only mounted with this middleware when the
// control plane is configured.
func OATRequired(resolver OATResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolver == nil {
			response.InternalError(c, "oat authentication not configured")
			c.Abort()
			return
		}

		token := bearerToken(c)
		if token == "" {
			response.UnauthorizedWithMessage(c, "oat bearer token required")
			c.Abort()
			return
		}
		if !resolver.LooksLikeOAT(token) {
			// Not an OAT (e.g. a JWT or delegated token). These public service
			// routes accept only OpenRails-issued OATs; reject other credentials.
			response.UnauthorizedWithMessage(c, "openrails-issued oat required")
			c.Abort()
			return
		}

		resolved, err := resolver.ResolveOAT(c.Request.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, authcore.ErrAccessTokenExpired):
				response.UnauthorizedWithMessage(c, "oat_expired")
			case errors.Is(err, authcore.ErrAccessTokenRevoked):
				response.UnauthorizedWithMessage(c, "oat_revoked")
			case errors.Is(err, controlplane.ErrOATTenantUnresolved):
				// Owning org maps to no active tenant (cross-tenant / unmapped).
				response.ForbiddenWithMessage(c, "oat_tenant_unresolved")
			case errors.Is(err, authcore.ErrInvalidAccessToken):
				response.UnauthorizedWithMessage(c, "oat_invalid")
			default:
				log.WithError(err).Warn("oat resolution failed")
				response.UnauthorizedWithMessage(c, "oat_invalid")
			}
			c.Abort()
			return
		}

		// Pin the resolved tenant for tenant-owned DB access (issue #223). This
		// OVERRIDES the default single-tenant resolution from ResolveTenant.
		ctx := tenant.WithID(c.Request.Context(), resolved.TenantID)
		c.Request = c.Request.WithContext(ctx)
		c.Set("billing.tenant_id", resolved.TenantID)
		c.Set(OATContextKey, resolved)
		c.Set(OATOrgSlugContextKey, resolved.OrgSlug)

		c.Next()
	}
}

// RequireOATPermission gates a route on a specific OpenRails permission held by
// the authenticated OAT (issue #222). Must run AFTER OATRequired. PermAdmin
// satisfies any permission (handled by ResolvedOAT.HasPermission).
func RequireOATPermission(perm string) gin.HandlerFunc {
	perm = strings.TrimSpace(perm)
	return func(c *gin.Context) {
		value, ok := c.Get(OATContextKey)
		if !ok {
			response.UnauthorizedWithMessage(c, "oat required")
			c.Abort()
			return
		}
		resolved, ok := value.(*controlplane.ResolvedOAT)
		if !ok || resolved == nil {
			response.InternalError(c, "oat state invalid")
			c.Abort()
			return
		}
		if !resolved.HasPermission(perm) {
			response.ForbiddenWithMessage(c, "oat_permission_required")
			c.Abort()
			return
		}
		c.Next()
	}
}

// OATFromGin returns the resolved OAT attached to the request, if any.
func OATFromGin(c *gin.Context) (*controlplane.ResolvedOAT, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.Get(OATContextKey)
	if !ok {
		return nil, false
	}
	resolved, ok := v.(*controlplane.ResolvedOAT)
	return resolved, ok && resolved != nil
}

func bearerToken(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	h := strings.TrimSpace(c.GetHeader("Authorization"))
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
