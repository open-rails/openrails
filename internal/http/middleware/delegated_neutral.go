package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
)

// Framework-neutral delegated-identity middleware for the self-service
// (/v1/me/*) surface.
// Ported from the retired gin middleware (#670); the context payload contract
// (request keys below) is unchanged, so handlers work identically.

// Request keys for resolved bearer identity.
const (
	// PrincipalContextKey holds the resolved bearer *Principal.
	PrincipalContextKey = "openrails.principal"
	// DelegatedContextKey holds the *credential.ResolvedDelegated.
	DelegatedContextKey = "openrails.delegated"
	// ServiceCredentialContextKey holds the *credential.ResolvedServiceCredential
	// for the request (pinned by the route gate; read by handlers).
	ServiceCredentialContextKey = "openrails.service_credential"
	// ResourceUserContextKey holds the *credential.ResourceUser a trusted
	// issuer's token resolved to on a signed-in user's own routes.
	ResourceUserContextKey = "openrails.resource_user"
)

// ResourceUserFromRequest returns the trusted issuer's principal the user
// tier resolved, if any.
func ResourceUserFromRequest(r *request.Request) (*credential.ResourceUser, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Get(ResourceUserContextKey)
	if !ok {
		return nil, false
	}
	user, ok := v.(*credential.ResourceUser)
	return user, ok && user != nil
}

// CredentialType identifies the bearer credential profile that authenticated
// the request. Webhooks are intentionally outside this model.
type CredentialType string

const (
	CredentialDelegatedUser     CredentialType = "delegated_user"
	CredentialHostDelegatedUser CredentialType = "host_delegated_user"
	CredentialUserSession       CredentialType = "user_session"
)

// Principal is the common bearer-auth result used by route permission gates.
type Principal struct {
	CredentialClass      billingauth.CredentialClass
	MerchantID           billing.MerchantID
	MerchantSlug         string
	MerchantConfigSource string
	CredentialType       CredentialType
	Subject              string
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

// ResourceCustomerResolver verifies a trusted issuer's access token granted
// openrails:self and resolves the customer it acts as (#1140). The control
// plane implements it; tests can inject a fake.
type ResourceCustomerResolver interface {
	ResolveResourceCustomer(r *http.Request) (*credential.ResolvedDelegated, error)
}

// DelegatedFromRequest returns the resolved delegated token attached to the
// request, if any.
func DelegatedFromRequest(r *request.Request) (*credential.ResolvedDelegated, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Get(DelegatedContextKey)
	if !ok {
		return nil, false
	}
	resolved, ok := v.(*credential.ResolvedDelegated)
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

// ResourceCustomerRequired authenticates the self-service surface with a
// trusted issuer's DPoP-bound access token granted openrails:self: the
// token's user is the customer of the merchant the request names, or of the
// issuer's only one. Every other credential is refused.
func ResourceCustomerRequired(resolver ResourceCustomerResolver) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			if resolver == nil {
				r.AbortCode(billing.CodeInternalError, "customer authentication not configured")
				return
			}
			token := requestAuthorizationToken(r.Request)
			if token == "" {
				r.AbortCode(billing.CodeAuthenticationRequired, "access token required")
				return
			}
			if !credential.LooksLikeResourceToken(token) {
				r.AbortGate(billingauth.Refusal(billing.CodeAccessTokenInvalid))
				return
			}
			resolved, err := resolver.ResolveResourceCustomer(r.Request)
			if err != nil {
				r.AbortGate(credential.ResourceTokenRefusal(err))
				return
			}
			if !bindDelegated(r, resolved, CredentialDelegatedUser, resolved.CredentialClass) {
				return
			}
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
				r.AbortCode(billing.CodeInternalError, "delegated authentication not configured")
				return
			}
			principal, err := authn.AuthenticateDelegated(r.Request.Context(), r.Request)
			if err != nil {
				var gate billingauth.GateError
				if errors.As(err, &gate) && gate.Status >= 400 && gate.Status <= 599 {
					r.AbortGate(gate)
					return
				}
				r.AbortGate(billingauth.Unauthenticated(err))
				return
			}
			resolved, verr := credential.ResolvedDelegatedFromHostPrincipal(principal)
			if verr != nil {
				r.AbortCode(billing.CodeDelegatedPrincipalInvalid, "")
				return
			}
			if !bindDelegated(r, resolved, CredentialHostDelegatedUser, principal.CredentialClass) {
				return
			}
			next(r)
		}
	}
}

// bindDelegated pins the resolved merchant (#223), binds the acting user, and
// records the delegated state + principal for the permission gates.
func bindDelegated(r *request.Request, resolved *credential.ResolvedDelegated, typ CredentialType, class billingauth.CredentialClass) bool {
	if !EnforceMerchantBinding(r, resolved.MerchantID) {
		return false
	}
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
	r.Set(PrincipalContextKey, principalFromDelegated(resolved, typ, class))
	return true
}

func principalFromDelegated(resolved *credential.ResolvedDelegated, typ CredentialType, class billingauth.CredentialClass) *Principal {
	if resolved == nil {
		return nil
	}
	return &Principal{
		MerchantID:           resolved.MerchantID,
		MerchantSlug:         resolved.MerchantSlug,
		MerchantConfigSource: "delegated_issuer",
		CredentialType:       typ,
		CredentialClass:      class,
		Subject:              strings.TrimSpace(resolved.DelegatedSubject),
		Invoker:              strings.TrimSpace(resolved.Invoker),
		can: func(_ context.Context, perm string) bool {
			// #564: resolved.Permissions is already claim ∩ signer authority.
			return resolved.HasPermission(perm)
		},
	}
}

// PayerScopedRequired refuses an INVOKER-SCOPED principal (or#930): a credential
// that spends a payer's money without being the payer. Its bound subject names
// an account it does not own, so everything the self-service surface
// answers — balance, transactions, invoices, subscriptions, payment
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
				r.AbortCode(billing.CodeAuthenticationRequired, "bearer principal required")
				return
			}
			if principal.InvokerScoped() {
				r.AbortCode(billing.CodeInvokerScopedPrincipal, "")
				return
			}
			next(r)
		}
	}
}

func requestAuthorizationToken(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 2 && (strings.EqualFold(fields[0], "Bearer") || strings.EqualFold(fields[0], "DPoP")) {
		return fields[1]
	}
	return ""
}
