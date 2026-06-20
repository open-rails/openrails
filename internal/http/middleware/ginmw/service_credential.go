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

// Gin context keys for a resolved merchant-scoped service credential.
const (
	// ServiceCredentialContextKey holds the *controlplane.ResolvedServiceCredential for the request.
	ServiceCredentialContextKey = "openrails.service_credential"
	// ServiceCredentialOwnerOrgSlugContextKey holds the owning AuthKit org slug.
	ServiceCredentialOwnerOrgSlugContextKey = "openrails.service_credential_authkit_org_slug"
)

// ServiceCredentialResolver validates a presented OpenRails-issued API key against live AuthKit +
// merchant-directory state. The control plane implements it; tests can inject a
// fake. A nil resolver is a wiring bug (#469: the standalone always has a
// control plane) and the middleware fails closed.
type ServiceCredentialResolver interface {
	// LooksLikeAPIKey reports whether token carries this deployment's API-key marker.
	LooksLikeAPIKey(token string) bool
	// ResolveAPIKey validates the API key and resolves its merchant + permissions.
	ResolveAPIKey(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

// ServiceJWTResolver validates first-party OIDC service JWTs from registered
// merchant issuers. Implemented by the control plane when service-JWT grants are
// available.
type ServiceJWTResolver interface {
	ResolveServiceJWT(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

// RemoteApplicationResolver validates a remote application access token
// (#76/#484): a remote_application acting AS ITSELF, granted STORED
// merchant-role/permissions — a SECOND programmatic credential type alongside
// API keys. The control plane implements it. ResolveRemoteApplication returns
// the SAME *ResolvedServiceCredential shape, so the existing #481 role-based
// merchant authz runs unchanged; controlplane.ErrNotRemoteApplicationToken
// signals a JWT that verified but is not a remote application access token
// (fall through / reject).
type RemoteApplicationResolver interface {
	ResolveRemoteApplication(ctx context.Context, token string) (*controlplane.ResolvedServiceCredential, error)
}

// ServiceCredentialRequired authenticates a merchant-scoped public route with a
// programmatic service credential. Shared-secret OpenRails API keys use the
// `openrails_st_...` wire shape; JWKS-backed remote applications and service
// JWTs are separate credential types that resolve to the same authorization
// result.
//
// On success it pins the resolved merchant onto the request context (overriding the
// default configured-merchant resolution) and records the API key's permissions.
// Expired, revoked, unknown, or cross-merchant/unmapped API keys are rejected.
//
// resolver must be non-nil; routes are only mounted with this middleware when the
// control plane is configured.
func ServiceCredentialRequired(resolver ServiceCredentialResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolver == nil {
			response.InternalError(c, "API key authentication not configured")
			c.Abort()
			return
		}

		token := bearerToken(c)
		if token == "" {
			response.UnauthorizedWithMessage(c, "API key bearer token required")
			c.Abort()
			return
		}
		resolved, credentialType, err := resolveServiceCredential(c.Request.Context(), resolver, token)
		if err != nil {
			switch {
			case errors.Is(err, authcore.ErrAccessTokenExpired):
				response.UnauthorizedWithMessage(c, "service_credential_expired")
			case errors.Is(err, authcore.ErrAccessTokenRevoked):
				response.UnauthorizedWithMessage(c, "service_credential_revoked")
			case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved):
				// Owning AuthKit org maps to no active OpenRails merchant.
				response.ForbiddenWithMessage(c, "service_credential_merchant_unresolved")
			case errors.Is(err, controlplane.ErrServiceCredentialScopeDenied):
				response.ForbiddenWithMessage(c, "service_credential_resource_scope_denied")
			case errors.Is(err, authcore.ErrInvalidAccessToken):
				response.UnauthorizedWithMessage(c, "service_credential_invalid")
			case errors.Is(err, authcore.ErrInvalidServiceJWT):
				response.UnauthorizedWithMessage(c, "service_jwt_invalid")
			default:
				log.WithError(err).Warn("API key resolution failed")
				response.UnauthorizedWithMessage(c, "service_credential_invalid")
			}
			c.Abort()
			return
		}

		// Pin the resolved merchant for merchant-owned DB access (issue #223). This
		// OVERRIDES the default configured-merchant resolution from ResolveMerchant.
		ctx := merchant.WithID(c.Request.Context(), resolved.MerchantID)
		c.Request = c.Request.WithContext(ctx)
		c.Set("openrails.merchant_id", resolved.MerchantID)
		c.Set(ServiceCredentialContextKey, resolved)
		c.Set(ServiceCredentialOwnerOrgSlugContextKey, resolved.OwnerOrgSlug)
		c.Set(PrincipalContextKey, principalFromServiceCredential(resolved, credentialType))

		c.Next()
	}
}

func resolveServiceCredential(ctx context.Context, resolver ServiceCredentialResolver, token string) (*controlplane.ResolvedServiceCredential, CredentialType, error) {
	// Shared-secret API key: this deployment's brand prefix marks it.
	if resolver.LooksLikeAPIKey(token) {
		resolved, err := resolver.ResolveAPIKey(ctx, token)
		return resolved, CredentialAPIKey, err
	}
	// Programmatic JWT credential (#484): a remote application access token is a
	// second accepted credential type alongside API keys. Try remote-application
	// verification first for a JWT-shaped bearer; a JWT that verifies but is not a
	// remote application access token falls through to the service-JWT path.
	if raResolver, ok := resolver.(RemoteApplicationResolver); ok && controlplane.LooksLikeJWT(token) {
		resolved, err := raResolver.ResolveRemoteApplication(ctx, token)
		if err == nil {
			return resolved, CredentialRemoteApplication, nil
		}
		if !errors.Is(err, controlplane.ErrNotRemoteApplicationToken) {
			return nil, "", err
		}
		// Not a remote application access token: fall through to service-JWT.
	}
	if jwtResolver, ok := resolver.(ServiceJWTResolver); ok {
		resolved, err := jwtResolver.ResolveServiceJWT(ctx, token)
		return resolved, CredentialServiceJWT, err
	}
	return nil, "", authcore.ErrInvalidAccessToken
}

// RequireServiceCredentialCustomerScope gates a route on the resolved
// credential's payer resource scope. It must run after ServiceCredentialRequired;
// merchant-wide credentials pass, payer-scoped credentials pass only for their
// exact merchant subject id.
func RequireServiceCredentialCustomerScope(c *gin.Context, payer uuid.UUID) bool {
	resolved, ok := ServiceCredentialFromGin(c)
	if !ok || resolved == nil {
		response.UnauthorizedWithMessage(c, "API key required")
		c.Abort()
		return false
	}
	if !resolved.AllowsCustomer(payer) {
		response.ForbiddenWithMessage(c, "service_credential_customer_scope_denied")
		c.Abort()
		return false
	}
	return true
}

// ServiceCredentialFromGin returns the resolved service credential attached to
// the request, if any.
func ServiceCredentialFromGin(c *gin.Context) (*controlplane.ResolvedServiceCredential, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.Get(ServiceCredentialContextKey)
	if !ok {
		return nil, false
	}
	resolved, ok := v.(*controlplane.ResolvedServiceCredential)
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
