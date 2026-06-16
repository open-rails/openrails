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
	"github.com/open-rails/openrails/pkg/merchant"
)

// Gin context keys for a resolved OpenRails-issued merchant service token (issue #222).
const (
	// ServiceTokenContextKey holds the *controlplane.ResolvedServiceToken for the request.
	ServiceTokenContextKey = "openrails.service_token"
	// ServiceTokenOwnerOrgSlugContextKey holds the owning AuthKit org slug.
	ServiceTokenOwnerOrgSlugContextKey = "openrails.service_token_authkit_org_slug"
)

// ServiceTokenResolver validates a presented OpenRails-issued service token against live AuthKit +
// merchant-directory state. The control plane implements it; tests can inject a
// fake. A nil resolver is a wiring bug (#469: the standalone always has a
// control plane) and the middleware fails closed.
type ServiceTokenResolver interface {
	// LooksLikeServiceToken reports whether token carries this deployment's service token marker.
	LooksLikeServiceToken(token string) bool
	// ResolveServiceToken validates the service token and resolves its merchant + permissions.
	ResolveServiceToken(ctx context.Context, token string) (*controlplane.ResolvedServiceToken, error)
}

// ServiceJWTResolver validates first-party OIDC service JWTs from registered
// merchant issuers. Implemented by the control plane when service-JWT grants are
// available.
type ServiceJWTResolver interface {
	ResolveServiceJWT(ctx context.Context, token string) (*controlplane.ResolvedServiceToken, error)
}

// RemoteApplicationResolver validates a JWKS-principal SELF-token (#76/#484): a
// remote_application acting AS ITSELF, granted STORED merchant-role/permissions —
// a SECOND programmatic credential type alongside service tokens. The control
// plane implements it. ResolveRemoteApplication returns the SAME
// *ResolvedServiceToken shape, so the existing #481 role-based merchant authz
// runs unchanged; controlplane.ErrNotRemoteApplicationToken signals a JWT that
// verified but is not a remote_application self-token (fall through / reject).
type RemoteApplicationResolver interface {
	ResolveRemoteApplication(ctx context.Context, token string) (*controlplane.ResolvedServiceToken, error)
}

// ServiceTokenRequired authenticates a merchant-scoped public route with an OpenRails-issued
// merchant service token (issue #222). It REPLACES the retired private/mTLS/api-key service
// surface: machine/server callers (HostApps/Tensorhub reserving credits,
// reading entitlements, etc.) present `Authorization: Bearer <openrails_st_...>`
// against public merchant routes.
//
// On success it pins the resolved merchant onto the request context (overriding the
// default single-merchant resolution) and records the service token's permissions for
// downstream RequireServiceTokenPermission gates. Expired, revoked, unknown, or
// cross-merchant/unmapped service tokens are rejected.
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
		resolved, err := resolveServiceCredential(c.Request.Context(), resolver, token)
		if err != nil {
			switch {
			case errors.Is(err, authcore.ErrAccessTokenExpired):
				response.UnauthorizedWithMessage(c, "service_token_expired")
			case errors.Is(err, authcore.ErrAccessTokenRevoked):
				response.UnauthorizedWithMessage(c, "service_token_revoked")
			case errors.Is(err, controlplane.ErrServiceTokenMerchantUnresolved):
				// Owning AuthKit org maps to no active OpenRails merchant.
				response.ForbiddenWithMessage(c, "service_token_merchant_unresolved")
			case errors.Is(err, controlplane.ErrServiceTokenScopeDenied):
				response.ForbiddenWithMessage(c, "service_token_resource_scope_denied")
			case errors.Is(err, authcore.ErrInvalidAccessToken):
				response.UnauthorizedWithMessage(c, "service_token_invalid")
			case errors.Is(err, authcore.ErrInvalidServiceJWT):
				response.UnauthorizedWithMessage(c, "service_jwt_invalid")
			default:
				log.WithError(err).Warn("service token resolution failed")
				response.UnauthorizedWithMessage(c, "service_token_invalid")
			}
			c.Abort()
			return
		}

		// Pin the resolved merchant for merchant-owned DB access (issue #223). This
		// OVERRIDES the default single-merchant resolution from ResolveMerchant.
		ctx := merchant.WithID(c.Request.Context(), resolved.MerchantID)
		c.Request = c.Request.WithContext(ctx)
		c.Set("openrails.merchant_id", resolved.MerchantID)
		c.Set(ServiceTokenContextKey, resolved)
		c.Set(ServiceTokenOwnerOrgSlugContextKey, resolved.OwnerOrgSlug)

		c.Next()
	}
}

func resolveServiceCredential(ctx context.Context, resolver ServiceTokenResolver, token string) (*controlplane.ResolvedServiceToken, error) {
	// Shared-secret service token: this deployment's brand prefix marks it.
	if resolver.LooksLikeServiceToken(token) {
		return resolver.ResolveServiceToken(ctx, token)
	}
	// Programmatic JWT credential (#484): a JWKS-principal SELF-token is a second
	// accepted credential type alongside service tokens. Try remote-application
	// verification first for a JWT-shaped bearer; a JWT that verifies but is not a
	// remote_application self-token falls through to the service-JWT path.
	if raResolver, ok := resolver.(RemoteApplicationResolver); ok && controlplane.LooksLikeJWT(token) {
		resolved, err := raResolver.ResolveRemoteApplication(ctx, token)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, controlplane.ErrNotRemoteApplicationToken) {
			return nil, err
		}
		// Not a remote_application self-token: fall through to service-JWT.
	}
	if jwtResolver, ok := resolver.(ServiceJWTResolver); ok {
		return jwtResolver.ResolveServiceJWT(ctx, token)
	}
	return nil, authcore.ErrInvalidAccessToken
}

// RequireServiceTokenCustomerScope gates a route on the resolved service token's payer resource
// scope. It must run after ServiceTokenRequired; merchant-wide service tokens pass, payer-scoped service tokens
// pass only for their exact merchant subject id.
func RequireServiceTokenCustomerScope(c *gin.Context, payer uuid.UUID) bool {
	resolved, ok := ServiceTokenFromGin(c)
	if !ok || resolved == nil {
		response.UnauthorizedWithMessage(c, "service token required")
		c.Abort()
		return false
	}
	if !resolved.AllowsCustomer(payer) {
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
