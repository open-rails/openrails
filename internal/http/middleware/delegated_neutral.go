package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/authkit"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Framework-neutral delegated-identity middleware for the self-service
// (/v1/me/*) and customer-treasury (/v1/customers/:customer_id/*) surfaces.
// Ported from the retired gin middleware (#670); the context payload contract
// (request keys below) is unchanged, so handlers work identically.

// Request keys for resolved bearer identity.
const (
	// PrincipalContextKey holds the resolved bearer *Principal.
	PrincipalContextKey = "openrails.principal"
	// DelegatedContextKey holds the *controlplane.ResolvedDelegated.
	DelegatedContextKey = "openrails.delegated"
	// ServiceCredentialContextKey holds the *controlplane.ResolvedServiceCredential
	// for the request (pinned by the route gate; read by handlers).
	ServiceCredentialContextKey = "openrails.service_credential"
)

// CredentialType identifies the bearer credential profile that authenticated
// the request. Webhooks are intentionally outside this model.
type CredentialType string

const (
	CredentialAPIKey            CredentialType = "api_key"
	CredentialRemoteApplication CredentialType = "remote_application"
	CredentialServiceJWT        CredentialType = "service_jwt"
	CredentialDelegatedUser     CredentialType = "delegated_user"
	CredentialHostDelegatedUser CredentialType = "host_delegated_user"
	CredentialUserSession       CredentialType = "user_session"
)

// Principal is the common bearer-auth result used by route permission gates.
type Principal struct {
	MerchantID     merchant.ID
	MerchantSlug   string
	MerchantSource string
	CredentialType CredentialType
	Subject        string

	can func(context.Context, string) bool
}

// Can reports whether the resolved principal has perm.
func (p *Principal) Can(ctx context.Context, perm string) bool {
	if p == nil || p.can == nil {
		return false
	}
	return p.can(ctx, strings.TrimSpace(perm))
}

// DelegatedResolver validates a presented browser-direct delegated access token
// against live AuthKit + merchant-directory state. The control plane implements
// it; tests can inject a fake.
type DelegatedResolver interface {
	ResolveDelegated(ctx context.Context, token string, origin string) (*controlplane.ResolvedDelegated, error)
}

// DelegatedFromRequest returns the resolved delegated token attached to the
// request, if any.
func DelegatedFromRequest(r *request.Request) (*controlplane.ResolvedDelegated, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Get(DelegatedContextKey)
	if !ok {
		return nil, false
	}
	resolved, ok := v.(*controlplane.ResolvedDelegated)
	return resolved, ok && resolved != nil
}

// PrincipalFromRequest returns the bearer principal attached to the request.
func PrincipalFromRequest(r *request.Request) (*Principal, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Get(PrincipalContextKey)
	if !ok {
		return nil, false
	}
	p, ok := v.(*Principal)
	return p, ok && p != nil
}

// DelegatedSelfRequired authenticates a merchant-scoped self-service route with
// a browser-direct DELEGATED ACCESS TOKEN (#222 browser tier). On success it
// pins the resolved merchant, binds the acting user (the token's
// delegated_sub), and records the delegated state + principal for permission
// gates. Fail-closed on missing/expired/revoked/cross-merchant tokens.
func DelegatedSelfRequired(resolver DelegatedResolver) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			if resolver == nil {
				r.AbortJSON(http.StatusInternalServerError, "delegated authentication not configured")
				return
			}
			token := requestBearerToken(r)
			if token == "" {
				r.AbortJSON(http.StatusUnauthorized, "delegated bearer token required")
				return
			}
			resolved, err := resolver.ResolveDelegated(r.Request.Context(), token, r.Header("Origin"))
			if err != nil {
				switch {
				case errors.Is(err, authkit.ErrAccessTokenExpired):
					r.AbortJSON(http.StatusUnauthorized, "delegated_token_expired")
				case errors.Is(err, authkit.ErrAccessTokenRevoked):
					r.AbortJSON(http.StatusUnauthorized, "delegated_token_revoked")
				case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved),
					errors.Is(err, controlplane.ErrDelegatedIssuerUnknown):
					r.AbortJSON(http.StatusForbidden, "delegated_merchant_unresolved")
				case errors.Is(err, controlplane.ErrDelegatedOriginNotAllowed):
					r.AbortJSON(http.StatusForbidden, "delegated_origin_not_allowed")
				case errors.Is(err, controlplane.ErrDelegatedNotConfigured):
					r.AbortJSON(http.StatusInternalServerError, "delegated authentication not configured")
				default:
					log.WithError(err).Warn("delegated token resolution failed")
					r.AbortJSON(http.StatusUnauthorized, "delegated_token_invalid")
				}
				return
			}
			bindDelegated(r, resolved, CredentialDelegatedUser)
			next(r)
		}
	}
}

// DelegatedPrincipalRequired authenticates a self-service route with a
// HOST-SUPPLIED billingauth.DelegatedAuthenticator (#339): the host verifies
// its own credential and returns the explicitly mapped principal. Produces the
// EXACT SAME context payload as DelegatedSelfRequired. Explicit mapping, no
// fallbacks: an empty/unparseable merchant or empty subject is rejected.
func DelegatedPrincipalRequired(authn billingauth.DelegatedAuthenticator) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			if authn == nil {
				r.AbortJSON(http.StatusInternalServerError, "delegated authentication not configured")
				return
			}
			principal, err := authn.AuthenticateDelegated(r.Request.Context(), r.Request)
			if err != nil {
				r.AbortJSON(http.StatusUnauthorized, billingauth.UnauthenticatedMessage(err))
				return
			}
			resolved, verr := controlplane.ResolvedDelegatedFromHostPrincipal(principal)
			if verr != nil {
				r.AbortJSON(http.StatusUnauthorized, "delegated_principal_invalid")
				return
			}
			bindDelegated(r, resolved, CredentialHostDelegatedUser)
			next(r)
		}
	}
}

// bindDelegated pins the resolved merchant (#223), binds the acting user, and
// records the delegated state + principal for the permission gates.
func bindDelegated(r *request.Request, resolved *controlplane.ResolvedDelegated, typ CredentialType) {
	ctx := merchant.WithID(r.Request.Context(), resolved.MerchantID)
	r.Request = r.Request.WithContext(ctx)
	r.SetUserContext(billingauth.UserContext{
		UserID:        resolved.DelegatedSubject,
		Email:         resolved.Email,
		EmailVerified: resolved.EmailVerified,
		Username:      resolved.Username,
		Merchant:      resolved.Merchant,
	})
	r.Set("openrails.merchant_id", resolved.MerchantID)
	r.Set(DelegatedContextKey, resolved)
	r.Set(PrincipalContextKey, principalFromDelegated(resolved, typ))
}

func principalFromDelegated(resolved *controlplane.ResolvedDelegated, typ CredentialType) *Principal {
	if resolved == nil {
		return nil
	}
	return &Principal{
		MerchantID:     resolved.MerchantID,
		MerchantSlug:   resolved.MerchantSlug,
		MerchantSource: "delegated_issuer",
		CredentialType: typ,
		Subject:        strings.TrimSpace(resolved.DelegatedSubject),
		can: func(_ context.Context, perm string) bool {
			// #564: resolved.Permissions is already claim ∩ signer authority.
			return resolved.HasPermission(perm)
		},
	}
}

// RequirePermission gates a route on a permission held by the resolved bearer
// principal. Must run after the group's authentication middleware.
func RequirePermission(perm string) router.Middleware {
	perm = strings.TrimSpace(perm)
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			principal, ok := PrincipalFromRequest(r)
			if !ok {
				r.AbortJSON(http.StatusUnauthorized, "bearer principal required")
				return
			}
			if !principal.Can(r.Request.Context(), perm) {
				r.AbortJSON(http.StatusForbidden, "permission_required")
				return
			}
			next(r)
		}
	}
}

// CustomerIDMatchesDelegated reports whether the :customer_id path segment
// names the same customer as the resolved delegated principal — by slug,
// merchant string, or merchant id.
func CustomerIDMatchesDelegated(customerID string, resolved *controlplane.ResolvedDelegated) bool {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" || resolved == nil {
		return false
	}
	candidates := []string{resolved.Merchant, resolved.MerchantSlug}
	if !resolved.MerchantID.IsZero() {
		candidates = append(candidates, resolved.MerchantID.String())
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == customerID {
			return true
		}
	}
	return false
}

// CustomerScopeRequired gates the customer-as-payer treasury surface
// (/v1/customers/:customer_id/*, #567). Runs AFTER the delegated auth
// middleware: confirms :customer_id matches the resolved principal's customer
// and REBINDS the acting payer subject to the customer's payable subject so
// the shared /v1/me money handlers operate on the customer balance. Never
// touches permissions; the pinned merchant is unchanged.
func CustomerScopeRequired() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			resolved, ok := DelegatedFromRequest(r)
			if !ok {
				r.AbortJSON(http.StatusUnauthorized, "delegated principal required")
				return
			}
			if resolved.MerchantID.IsZero() {
				r.AbortJSON(http.StatusUnauthorized, "delegated principal invalid")
				return
			}
			if !CustomerIDMatchesDelegated(r.Param("customer_id"), resolved) {
				r.AbortJSON(http.StatusForbidden, "customer_scope_mismatch")
				return
			}
			r.SetUserContext(billingauth.UserContext{
				UserID:   resolved.MerchantID.UUID().String(),
				Username: resolved.Merchant,
				Merchant: resolved.Merchant,
			})
			next(r)
		}
	}
}

// CustomerGroupEnsurer materializes the AuthKit customer permission-group; the
// control plane implements it.
type CustomerGroupEnsurer interface {
	EnsureCustomerPermissionGroup(ctx context.Context, customerID, ownerSubject string) (string, error)
}

// EnsureCustomerPermissionGroup materializes the AuthKit customer group on the
// first customer-owned write that manages spend delegation/credentials.
func EnsureCustomerPermissionGroup(cp CustomerGroupEnsurer) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			resolved, ok := DelegatedFromRequest(r)
			if !ok || resolved == nil {
				r.AbortJSON(http.StatusUnauthorized, "delegated principal required")
				return
			}
			if cp == nil {
				r.AbortJSON(http.StatusInternalServerError, "customer control plane unavailable")
				return
			}
			if _, err := cp.EnsureCustomerPermissionGroup(r.Request.Context(), r.Param("customer_id"), resolved.DelegatedSubject); err != nil {
				r.AbortJSON(http.StatusInternalServerError, "customer permission-group ensure failed")
				return
			}
			next(r)
		}
	}
}

func requestBearerToken(r *request.Request) string {
	h := strings.TrimSpace(r.Header("Authorization"))
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
