package ginmw

import (
	"errors"

	"github.com/gin-gonic/gin"
	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/response"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantPrincipalRequired authenticates a `/v1/merchant/*` route with ANY
// credential that resolves to the AuthKit org OWNING the merchant (#555). It
// REPLACES the per-credential-type middleware split (ServiceCredentialRequired /
// DelegatedSelfRequired): an OpenRails API key, a remote-application or service
// JWT, a browser-direct delegated access token, and a logged-in user session all
// normalize into ONE Principal + a pinned merchant, so a single downstream
// RequirePermission(merchant:...) gate authorizes every caller type.
//
// Resolution precedence by token shape (fail-closed — the merchant is always
// pinned from the verified credential, never the URL):
//
//   - API-key-shaped bearer -> service credential ONLY. A brand-prefixed key is
//     never a JWT, so a failure is reported as-is with no fall-through.
//   - JWT-shaped bearer      -> try the programmatic path (remote-application then
//     service JWT); a JWT that is not a programmatic token falls through to the
//     delegated browser token, since a delegated access token is also a JWT.
//   - no bearer              -> the live AuthKit user session (cookie/session),
//     authorized by live org permissions.
//
// serviceResolver and delegatedResolver are the control plane in standalone mode
// (always present, #469). userChecker may be nil when no interactive user surface
// is mounted; the no-bearer branch then rejects.
func MerchantPrincipalRequired(serviceResolver ServiceCredentialResolver, delegatedResolver DelegatedResolver, userChecker AdminPermissionChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)

		if token != "" {
			// Brand-prefixed shared-secret API key: definitively a service credential.
			if serviceResolver != nil && serviceResolver.LooksLikeAPIKey(token) {
				if resolved, credType, err := resolveServiceCredential(c.Request.Context(), serviceResolver, token); err != nil {
					abortServiceCredentialError(c, err)
				} else {
					bindServiceCredentialPrincipal(c, resolved, credType)
					c.Next()
				}
				return
			}

			// JWT-shaped bearer: programmatic (remote-app / service JWT) first, then
			// the delegated browser token (also a JWT) before rejecting.
			if controlplane.LooksLikeJWT(token) {
				if serviceResolver != nil {
					if resolved, credType, err := resolveServiceCredential(c.Request.Context(), serviceResolver, token); err == nil {
						bindServiceCredentialPrincipal(c, resolved, credType)
						c.Next()
						return
					}
				}
				if delegatedResolver != nil {
					if resolved, err := delegatedResolver.ResolveDelegated(c.Request.Context(), token, c.Request.Header.Get("Origin")); err != nil {
						abortDelegatedError(c, err)
					} else {
						bindDelegatedPrincipal(c, resolved)
						c.Next()
					}
					return
				}
			}

			response.UnauthorizedWithMessage(c, "merchant_credential_invalid")
			c.Abort()
			return
		}

		// No bearer token: fall back to the live interactive user session.
		if userChecker != nil {
			if uc, ok := ginauth.UserContextFromGin(c); ok && uc.UserID != "" {
				c.Set(PrincipalContextKey, userSessionPrincipal(uc.UserID, uc.Org, userChecker))
				c.Next()
				return
			}
		}
		response.UnauthorizedWithMessage(c, "merchant authentication required")
		c.Abort()
	}
}

// bindServiceCredentialPrincipal pins the resolved merchant + records the service
// credential and its Principal on the request, identically to
// ServiceCredentialRequired.
func bindServiceCredentialPrincipal(c *gin.Context, resolved *controlplane.ResolvedServiceCredential, credType CredentialType) {
	ctx := merchant.WithID(c.Request.Context(), resolved.MerchantID)
	c.Request = c.Request.WithContext(ctx)
	c.Set("openrails.merchant_id", resolved.MerchantID)
	c.Set(ServiceCredentialContextKey, resolved)
	c.Set(ServiceCredentialOwnerOrgSlugContextKey, resolved.OwnerOrgSlug)
	c.Set(PrincipalContextKey, principalFromServiceCredential(resolved, credType))
}

// bindDelegatedPrincipal pins the resolved merchant, binds the acting delegated
// user, and records the Principal, identically to DelegatedSelfRequired.
func bindDelegatedPrincipal(c *gin.Context, resolved *controlplane.ResolvedDelegated) {
	ctx := merchant.WithID(c.Request.Context(), resolved.MerchantID)
	uc := authprovider.UserContext{
		UserID:        resolved.DelegatedSubject,
		Email:         resolved.Email,
		EmailVerified: resolved.EmailVerified,
		Username:      resolved.Username,
		Org:           resolved.Merchant,
	}
	ctx = authprovider.SetUserContext(ctx, uc)
	c.Request = c.Request.WithContext(ctx)
	c.Set("openrails.user_context", uc)
	c.Set("openrails.merchant_id", resolved.MerchantID)
	c.Set(DelegatedContextKey, resolved)
	c.Set(PrincipalContextKey, principalFromDelegated(resolved, CredentialDelegatedUser))
}

func abortServiceCredentialError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, authcore.ErrAccessTokenExpired):
		response.UnauthorizedWithMessage(c, "service_credential_expired")
	case errors.Is(err, authcore.ErrAccessTokenRevoked):
		response.UnauthorizedWithMessage(c, "service_credential_revoked")
	case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved):
		response.ForbiddenWithMessage(c, "service_credential_merchant_unresolved")
	case errors.Is(err, controlplane.ErrServiceCredentialScopeDenied):
		response.ForbiddenWithMessage(c, "service_credential_resource_scope_denied")
	case errors.Is(err, authcore.ErrInvalidServiceJWT):
		response.UnauthorizedWithMessage(c, "service_jwt_invalid")
	case errors.Is(err, authcore.ErrInvalidAccessToken):
		response.UnauthorizedWithMessage(c, "merchant_credential_invalid")
	default:
		log.WithError(err).Warn("merchant API key resolution failed")
		response.UnauthorizedWithMessage(c, "merchant_credential_invalid")
	}
	c.Abort()
}

func abortDelegatedError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, authcore.ErrAccessTokenExpired):
		response.UnauthorizedWithMessage(c, "delegated_token_expired")
	case errors.Is(err, authcore.ErrAccessTokenRevoked):
		response.UnauthorizedWithMessage(c, "delegated_token_revoked")
	case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved),
		errors.Is(err, controlplane.ErrDelegatedIssuerUnknown):
		response.ForbiddenWithMessage(c, "delegated_merchant_unresolved")
	case errors.Is(err, controlplane.ErrDelegatedOriginNotAllowed):
		response.ForbiddenWithMessage(c, "delegated_origin_not_allowed")
	case errors.Is(err, controlplane.ErrDelegatedNotConfigured):
		response.InternalError(c, "delegated authentication not configured")
	default:
		log.WithError(err).Warn("merchant delegated token resolution failed")
		response.UnauthorizedWithMessage(c, "merchant_credential_invalid")
	}
	c.Abort()
}
