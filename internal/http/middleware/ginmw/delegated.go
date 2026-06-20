package ginmw

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	authcore "github.com/open-rails/authkit/core"
	"github.com/open-rails/openrails/internal/http/response"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Gin context keys for a resolved browser-direct delegated access token (#222
// browser tier).
const (
	// DelegatedContextKey holds the *controlplane.ResolvedDelegated for the request.
	DelegatedContextKey = "openrails.delegated"
)

// DelegatedResolver validates a presented browser-direct delegated access token
// against live AuthKit + merchant-directory state. The control plane implements
// it; tests can inject a fake. A nil resolver is a wiring bug (#469: the
// standalone always has a control plane) and the middleware fails closed.
type DelegatedResolver interface {
	// ResolveDelegated verifies the delegated access token (aud=openrails, no sub)
	// and resolves its merchant + acting user (delegated_sub).
	ResolveDelegated(ctx context.Context, token string, origin string) (*controlplane.ResolvedDelegated, error)
}

// DelegatedSelfRequired authenticates a merchant-scoped self-service route with a
// browser-direct DELEGATED ACCESS TOKEN (issue #222 browser-tier foundation; the
// backend prerequisite for browser-direct self-service consumers.
//
// A merchant's host frontend mints a short-lived delegated access token for the
// logged-in end-user (canonical claims minted by AuthKit: `aud` includes
// `openrails`, `merchant`, `delegated_sub`)
// and the browser presents it as `Authorization: Bearer <jwt>` directly to
// OpenRails. This lets a browser self-serve its OWN billing without routing
// through the host backend, while OpenRails remains the authorization boundary.
//
// On success it:
//   - pins the resolved merchant onto the request context (overriding the default
//     configured-merchant resolution) so all merchant-owned DB access is merchant-scoped,
//   - sets the acting user (the token's `delegated_sub`) as the request's
//     user context, so the existing user-facing `me` handlers naturally scope
//     every read/write to that user — never another user's data,
//   - records any browser-safe org permissions for admin route gates.
//
// Rejected (fail-closed): missing/expired/revoked tokens, normal-`sub` (non-
// delegated) tokens, wrong audience/issuer/signature, tokens carrying any
// platform/unknown permission, and cross-merchant / unmapped tokens.
func DelegatedSelfRequired(resolver DelegatedResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolver == nil {
			response.InternalError(c, "delegated authentication not configured")
			c.Abort()
			return
		}

		token := bearerToken(c)
		if token == "" {
			response.UnauthorizedWithMessage(c, "delegated bearer token required")
			c.Abort()
			return
		}

		resolved, err := resolver.ResolveDelegated(c.Request.Context(), token, c.Request.Header.Get("Origin"))
		if err != nil {
			switch {
			case errors.Is(err, authcore.ErrAccessTokenExpired):
				response.UnauthorizedWithMessage(c, "delegated_token_expired")
			case errors.Is(err, authcore.ErrAccessTokenRevoked):
				response.UnauthorizedWithMessage(c, "delegated_token_revoked")
			case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved),
				errors.Is(err, controlplane.ErrDelegatedIssuerUnknown):
				// Token's merchant/issuer maps to no active merchant (cross-merchant /
				// unmapped / unregistered or disabled federated issuer).
				response.ForbiddenWithMessage(c, "delegated_merchant_unresolved")
			case errors.Is(err, controlplane.ErrDelegatedOriginNotAllowed):
				response.ForbiddenWithMessage(c, "delegated_origin_not_allowed")
			case errors.Is(err, controlplane.ErrDelegatedNotConfigured):
				response.InternalError(c, "delegated authentication not configured")
			default:
				log.WithError(err).Warn("delegated token resolution failed")
				response.UnauthorizedWithMessage(c, "delegated_token_invalid")
			}
			c.Abort()
			return
		}

		// Pin the resolved merchant for merchant-owned DB access (#223). This
		// OVERRIDES the default configured-merchant resolution from ResolveMerchant.
		ctx := merchant.WithID(c.Request.Context(), resolved.MerchantID)

		// Bind the acting user so the reused user-facing handlers scope to the
		// delegated subject. We populate BOTH the gin value and the request
		// context the existing authprovider helpers read.
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

		c.Next()
	}
}

// DelegatedPrincipalRequired authenticates a merchant-scoped self-service route
// with a HOST-SUPPLIED billingauth.DelegatedAuthenticator (issue #339): when
// OpenRails runs as a SUBSYSTEM of a host application, the host verifies its
// own credential and returns the explicitly mapped principal — one system,
// one credential. It is the host-pluggable counterpart of
// DelegatedSelfRequired and produces the EXACT SAME context payload (pinned
// merchant, acting user, resolved-delegated record), so every downstream
// handler and admin RequirePermission gate works unchanged.
//
// EXPLICIT MAPPING — NO FALLBACKS: the principal must carry the resolved
// merchant id and subject. A principal with an empty/unparseable merchant, an
// empty subject, or any permission outside the delegated browser admin catalog
// is rejected (401, fail closed), so a host can never smuggle a
// service/operator/platform grant onto the browser surface. Permissions are
// optional for self-service routes.
func DelegatedPrincipalRequired(authn billingauth.DelegatedAuthenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		if authn == nil {
			response.InternalError(c, "delegated authentication not configured")
			c.Abort()
			return
		}

		principal, err := authn.AuthenticateDelegated(c.Request.Context(), c.Request)
		if err != nil {
			response.UnauthorizedWithMessage(c, billingauth.UnauthenticatedMessage(err))
			c.Abort()
			return
		}
		resolved, verr := resolvedFromHostPrincipal(principal)
		if verr != nil {
			response.UnauthorizedWithMessage(c, "delegated_principal_invalid")
			c.Abort()
			return
		}

		// Identical context payload to DelegatedSelfRequired: pin the resolved
		// merchant (#223), bind the acting user, and record the delegated state for
		// the admin route permission gates.
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
		c.Set(PrincipalContextKey, principalFromDelegated(resolved, CredentialHostDelegatedUser))

		c.Next()
	}
}

// resolvedFromHostPrincipal validates a host-supplied principal and converts
// it to the *controlplane.ResolvedDelegated shape the delegated context
// carries. Validation is strict and fail-closed: explicit merchant + subject and
// every supplied permission inside the delegated browser catalog.
func resolvedFromHostPrincipal(p *billingauth.DelegatedPrincipal) (*controlplane.ResolvedDelegated, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	tenantID, err := merchant.ParseID(strings.TrimSpace(p.MerchantID))
	if err != nil || tenantID.IsZero() {
		return nil, billingauth.ErrDelegatedPrincipalInvalid
	}
	subject := strings.TrimSpace(p.SubjectID)
	customerID := identity.CustomerIDFromString(subject)
	if customerID.IsZero() {
		return nil, billingauth.ErrDelegatedPrincipalInvalid
	}
	perms := make([]string, 0, len(p.Permissions))
	for _, perm := range p.Permissions {
		perm = strings.TrimSpace(perm)
		if !controlplane.IsDelegatedPermission(perm) {
			return nil, billingauth.ErrDelegatedPrincipalInvalid
		}
		perms = append(perms, perm)
	}

	return &controlplane.ResolvedDelegated{
		Merchant:         strings.TrimSpace(p.MerchantSlug),
		MerchantID:       tenantID,
		MerchantSlug:     strings.TrimSpace(p.MerchantSlug),
		CustomerID:       customerID.UUID(),
		DelegatedSubject: subject,
		Issuer:           strings.TrimSpace(p.Issuer),
		Permissions:      perms,
		Email:            p.Email,
		EmailVerified:    p.EmailVerified,
		Username:         p.Username,
	}, nil
}

// DelegatedFromGin returns the resolved delegated token attached to the request,
// if any.
func DelegatedFromGin(c *gin.Context) (*controlplane.ResolvedDelegated, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.Get(DelegatedContextKey)
	if !ok {
		return nil, false
	}
	resolved, ok := v.(*controlplane.ResolvedDelegated)
	return resolved, ok && resolved != nil
}
