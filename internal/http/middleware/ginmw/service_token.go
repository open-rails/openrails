package ginmw

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/open-rails/openrails/internal/http/response"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Gin context keys for a resolved OpenRails-issued tenant service token (issue #222).
const (
	// ServiceTokenContextKey holds the *controlplane.ResolvedServiceToken for the request.
	ServiceTokenContextKey = "openrails.service_token"
	// ServiceTokenAuthKitTenantSlugContextKey holds the owning AuthKit tenant slug.
	ServiceTokenAuthKitTenantSlugContextKey = "openrails.service_token_authkit_tenant_slug"
)

// ServiceTokenResolver validates a presented OpenRails-issued service token against live AuthKit +
// tenant-directory state. The control plane implements it; tests can inject a
// fake. nil resolver => the deployment is not service token-capable (verifier-only mode).
type ServiceTokenResolver interface {
	// LooksLikeServiceToken reports whether token carries this deployment's service token marker.
	LooksLikeServiceToken(token string) bool
	// ResolveServiceToken validates the service token and resolves its tenant + permissions.
	ResolveServiceToken(ctx context.Context, token string) (*controlplane.ResolvedServiceToken, error)
}

// ServiceTokenRequired authenticates a tenant-scoped public route with an OpenRails-issued
// tenant service token (issue #222). It REPLACES the retired private/mTLS/api-key service
// surface: machine/server callers (HostApps/Tensorhub reserving credits,
// reading entitlements, etc.) present `Authorization: Bearer <openrails_st_...>`
// against public tenant routes.
//
// On success it pins the resolved tenant onto the request context (overriding the
// default single-tenant resolution) and records the service token's permissions for
// downstream RequireServiceTokenPermission gates. Expired, revoked, unknown, or
// cross-tenant/unmapped service tokens are rejected.
//
// resolver must be non-nil; routes are only mounted with this middleware when the
// control plane is configured.
func ServiceTokenRequired(resolver ServiceTokenResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolver == nil {
			response.InternalError(c, "service token authentication not configured")
			c.Abort()
			return
		}

		token := bearerToken(c)
		if token == "" {
			response.UnauthorizedWithMessage(c, "service token bearer token required")
			c.Abort()
			return
		}
		if !resolver.LooksLikeServiceToken(token) {
			// Not a service token (e.g. a JWT or delegated token). These public service
			// routes accept only OpenRails-issued service tokens; reject other credentials.
			response.UnauthorizedWithMessage(c, "openrails-issued service token required")
			c.Abort()
			return
		}

		resolved, err := resolver.ResolveServiceToken(c.Request.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, authcore.ErrAccessTokenExpired):
				response.UnauthorizedWithMessage(c, "service_token_expired")
			case errors.Is(err, authcore.ErrAccessTokenRevoked):
				response.UnauthorizedWithMessage(c, "service_token_revoked")
			case errors.Is(err, controlplane.ErrServiceTokenTenantUnresolved):
				// Owning org maps to no active tenant (cross-tenant / unmapped).
				response.ForbiddenWithMessage(c, "service_token_tenant_unresolved")
			case errors.Is(err, controlplane.ErrServiceTokenScopeDenied):
				response.ForbiddenWithMessage(c, "service_token_resource_scope_denied")
			case errors.Is(err, authcore.ErrInvalidAccessToken):
				response.UnauthorizedWithMessage(c, "service_token_invalid")
			default:
				log.WithError(err).Warn("service token resolution failed")
				response.UnauthorizedWithMessage(c, "service_token_invalid")
			}
			c.Abort()
			return
		}

		// Pin the resolved tenant for tenant-owned DB access (issue #223). This
		// OVERRIDES the default single-tenant resolution from ResolveTenant.
		ctx := tenant.WithID(c.Request.Context(), resolved.TenantID)
		c.Request = c.Request.WithContext(ctx)
		c.Set("billing.tenant_id", resolved.TenantID)
		c.Set(ServiceTokenContextKey, resolved)
		c.Set(ServiceTokenAuthKitTenantSlugContextKey, resolved.AuthKitTenantSlug)

		c.Next()
	}
}

// RequireServiceTokenTenantSubjectScope gates a route on the resolved service token's payer resource
// scope. It must run after ServiceTokenRequired; tenant-wide service tokens pass, payer-scoped service tokens
// pass only for their exact tenant subject id.
func RequireServiceTokenTenantSubjectScope(c *gin.Context, payer uuid.UUID) bool {
	resolved, ok := ServiceTokenFromGin(c)
	if !ok || resolved == nil {
		response.UnauthorizedWithMessage(c, "service token required")
		c.Abort()
		return false
	}
	if !resolved.AllowsTenantSubject(payer) {
		response.ForbiddenWithMessage(c, "service_token_tenant_subject_scope_denied")
		c.Abort()
		return false
	}
	return true
}

// RequireServiceTokenPermission gates a route on a specific OpenRails permission held by
// the authenticated service token (issue #222). Must run AFTER ServiceTokenRequired. PermAdmin
// satisfies any permission (handled by ResolvedServiceToken.HasPermission).
func RequireServiceTokenPermission(perm string) gin.HandlerFunc {
	perm = strings.TrimSpace(perm)
	return func(c *gin.Context) {
		value, ok := c.Get(ServiceTokenContextKey)
		if !ok {
			response.UnauthorizedWithMessage(c, "service token required")
			c.Abort()
			return
		}
		resolved, ok := value.(*controlplane.ResolvedServiceToken)
		if !ok || resolved == nil {
			response.InternalError(c, "service token state invalid")
			c.Abort()
			return
		}
		if !resolved.HasPermission(perm) {
			response.ForbiddenWithMessage(c, "service_token_permission_required")
			c.Abort()
			return
		}
		c.Next()
	}
}

// ServiceTokenFromGin returns the resolved service token attached to the request, if any.
func ServiceTokenFromGin(c *gin.Context) (*controlplane.ResolvedServiceToken, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.Get(ServiceTokenContextKey)
	if !ok {
		return nil, false
	}
	resolved, ok := v.(*controlplane.ResolvedServiceToken)
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
