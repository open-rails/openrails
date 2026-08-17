package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
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
	// Invoker is the host-owned spend principal this credential acts as under
	// Subject's account (or#930). Non-empty = INVOKER-SCOPED: it spends the
	// payer's money without being the payer.
	Invoker string

	can func(context.Context, string) bool
}

// InvokerScoped reports whether this principal acts as a delegated INVOKER on
// someone else's payer account, rather than as the payer itself.
func (p *Principal) InvokerScoped() bool {
	return p != nil && strings.TrimSpace(p.Invoker) != ""
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
		Invoker:        strings.TrimSpace(resolved.Invoker),
		can: func(_ context.Context, perm string) bool {
			// #564: resolved.Permissions is already claim ∩ signer authority.
			return resolved.HasPermission(perm)
		},
	}
}

// PayerScopedRequired refuses an INVOKER-SCOPED principal (or#930): a credential
// that spends a payer's money without being the payer. Its bound subject names
// an account it does not own, so everything the self-service and treasury
// surfaces answer — balance, transactions, invoices, subscriptions, payment
// methods, checkout, the payer's own delegation policy — is somebody else's.
//
// This is the guard that makes the narrow credential class SAFE to mint: a host
// can map an end user onto the payer org's account for the one read that is
// genuinely the end user's own (its spend windows) without opening the payer's
// whole surface to it. Fail closed — an unauthenticated request is refused here
// too, so mounting this without an auth middleware cannot silently pass.
func PayerScopedRequired() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			principal, ok := PrincipalFromRequest(r)
			if !ok {
				r.AbortJSON(http.StatusUnauthorized, "bearer principal required")
				return
			}
			if principal.InvokerScoped() {
				r.AbortJSON(http.StatusForbidden, "invoker_scoped_principal")
				return
			}
			next(r)
		}
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

// TreasuryPayerContextKey holds the *TreasuryPayer bound by
// CustomerScopeRequired.
const TreasuryPayerContextKey = "openrails.treasury_payer"

// TreasuryPayer is the payer identity the customer-treasury surface
// (/v1/customers/:customer_id/*) operates on, resolved and bound by
// CustomerScopeRequired (or#916). Handlers consume this binding instead of
// re-deriving a payer from merchant coordinates.
type TreasuryPayer struct {
	// Subject is the acting subject (delegated_sub), recorded for audit.
	Subject string
	// CustomerID is the payable subject every treasury read/write is keyed on.
	CustomerID identity.CustomerID
	// MerchantPayer marks the merchant-as-payer binding: the merchant's own
	// treasury account, granted only to merchant-admin principals.
	MerchantPayer bool
}

// TreasuryPayerFromRequest returns the payer bound by CustomerScopeRequired.
func TreasuryPayerFromRequest(r *request.Request) (*TreasuryPayer, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Get(TreasuryPayerContextKey)
	if !ok {
		return nil, false
	}
	p, ok := v.(*TreasuryPayer)
	return p, ok && p != nil
}

// ResolveTreasuryPayer matches the :customer_id path scope against the
// resolved principal and returns the typed payer binding (or#916):
//
//   - SUBJECT payer: :customer_id names the principal's OWN payable subject
//     (its delegated_sub or durable customer id). This is exactly the /v1/me
//     binding — strictly more gated, since the treasury routes' customer:*
//     RequirePermission rides on top.
//   - MERCHANT payer: :customer_id names the pinned merchant's own
//     coordinates (slug / id) — the merchant org's treasury account. Only a
//     merchant-admin principal (one granted the full `merchant:*` authority)
//     may bind it: every delegated principal of a merchant carries those
//     coordinates, so matching them alone let ANY subject holding customer:*
//     grants act on the merchant's balance (the pre-or#916 hazard).
//
// No match — including merchant coordinates presented without merchant-admin
// authority — fails closed.
func ResolveTreasuryPayer(customerID string, resolved *controlplane.ResolvedDelegated) (*TreasuryPayer, bool) {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" || resolved == nil || resolved.MerchantID.IsZero() {
		return nil, false
	}
	subject := strings.TrimSpace(resolved.DelegatedSubject)
	if (subject != "" && customerID == subject) ||
		(resolved.CustomerID != uuid.Nil && customerID == resolved.CustomerID.String()) {
		payer := identity.CustomerID(resolved.CustomerID)
		if payer.IsZero() {
			// Host principals carry the subject verbatim; a resolver that did
			// not materialize a durable customer id still keys on the subject
			// when it is a payable (uuid) subject.
			payer = identity.CustomerIDFromString(subject)
		}
		if payer.IsZero() {
			return nil, false
		}
		return &TreasuryPayer{Subject: subject, CustomerID: payer}, true
	}
	for _, coordinate := range []string{resolved.Merchant, resolved.MerchantSlug, resolved.MerchantID.String()} {
		if strings.TrimSpace(coordinate) != customerID {
			continue
		}
		if !resolved.HasPermission(permissions.MerchantAll) {
			return nil, false
		}
		return &TreasuryPayer{
			Subject:       subject,
			CustomerID:    identity.CustomerID(resolved.MerchantID.UUID()),
			MerchantPayer: true,
		}, true
	}
	return nil, false
}

// CustomerScopeRequired gates the customer-as-payer treasury surface
// (/v1/customers/:customer_id/*, #567). Runs AFTER the delegated auth
// middleware: resolves the typed treasury payer from the :customer_id scope
// (or#916) and REBINDS the acting payer subject to that payable subject so
// the shared /v1/me money handlers operate on the bound balance. Never
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
			payer, ok := ResolveTreasuryPayer(r.Param("customer_id"), resolved)
			if !ok {
				r.AbortJSON(http.StatusForbidden, "customer_scope_mismatch")
				return
			}
			user := billingauth.UserContext{
				UserID:   payer.CustomerID.UUID().String(),
				Merchant: resolved.Merchant,
			}
			if payer.MerchantPayer {
				user.Username = resolved.Merchant
			} else {
				user.Email = resolved.Email
				user.EmailVerified = resolved.EmailVerified
				user.Username = resolved.Username
			}
			r.Set(TreasuryPayerContextKey, payer)
			r.SetUserContext(user)
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
