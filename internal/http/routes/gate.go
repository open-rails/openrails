package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
)

type GateOptions struct {
	Authenticator             billingauth.Authenticator
	ResourceTokenResolver     ResourceTokenResolver
	AdminPermissionChecker    authpolicy.AdminPermissionChecker
	ServiceCredentialResolver ServiceCredentialResolver
	DelegatedAuthenticator    billingauth.DelegatedAuthenticator
}

func NewGate(opts GateOptions) billingauth.Gate {
	return legacyGate(opts)
}

type legacyGate GateOptions

type ServiceCredentialResolver interface {
	LooksLikeAPIKey(token string) bool
	ResolveAPIKey(ctx context.Context, token string) (*credential.ResolvedServiceCredential, error)
}

// ResourceTokenResolver verifies a trusted issuer's RFC 9068 access token
// and resolves its merchant and permissions (#1140).
type ResourceTokenResolver interface {
	ResolveResourceToken(r *http.Request) (*credential.ResolvedResourceAccess, error)
	// RequireRecentResourceSignIn is nil when r's token says its user signed
	// in at the issuer recently enough for a sensitive operation.
	RequireRecentResourceSignIn(r *http.Request) error
}

// ResourceUserResolver verifies a trusted issuer's access token on a
// signed-in user's own routes and resolves the merchants it may act on.
type ResourceUserResolver interface {
	ResolveResourceUser(r *http.Request) (*credential.ResourceUser, error)
}

func (g legacyGate) Authorize(ctx context.Context, req *http.Request, perm string) (billingauth.Principal, error) {
	// #685: in-process host principal, attached to the request CONTEXT by the
	// embed SDK's in-process transport. Trusted precisely because context values
	// cannot arrive on a network request (no header is consulted); gated on
	// permissions like every other credential.
	if hp, ok := requestauth.HostPrincipalFromContext(ctx); ok {
		if hp.MerchantID.IsZero() {
			return billingauth.Principal{}, billingauth.Refusal(billing.CodeHostPrincipalInvalid)
		}
		resolved := &credential.ResolvedServiceCredential{
			OwnerGroupRef: "in-process-host",
			MerchantID:    hp.MerchantID,
			MerchantSlug:  hp.MerchantSlug,
			Permissions:   hp.Permissions,
		}
		if !resolved.HasPermission(perm) {
			return billingauth.Principal{}, billingauth.Refusal(billing.CodePermissionRequired)
		}
		return billingauth.Principal{MerchantID: hp.MerchantID, Kind: billingauth.Machine, Subject: hp.Subject, Permissions: resolved.Permissions}, nil
	}
	if req != nil && credential.LooksLikeResourceToken(authorizationToken(req.Header.Get("Authorization"))) {
		return g.authorizeResourceToken(req, perm)
	}
	if resolved, err, handled := g.resolveAPIKey(ctx, req); handled {
		if err != nil {
			switch {
			case errors.Is(err, credential.ErrServiceCredentialMerchantUnresolved):
				return billingauth.Principal{}, billingauth.Refusal(billing.CodeServiceCredentialMerchantUnresolved)
			case errors.Is(err, credential.ErrServiceCredentialScopeDenied):
				return billingauth.Principal{}, billingauth.Refusal(billing.CodeServiceCredentialResourceScopeDenied)
			case errors.Is(err, credential.ErrServiceCredentialHostMismatch):
				// The API key resolves to a different Host merchant.
				return billingauth.Principal{}, billingauth.Refusal(billing.CodeHostMerchantMismatch)
			default:
				return billingauth.Principal{}, billingauth.Refusal(billing.CodeServiceCredentialInvalid)
			}
		}
		if resolved == nil {
			return billingauth.Principal{}, billingauth.Refusal(billing.CodeServiceCredentialInvalid)
		}
		if !resolved.HasPermission(perm) {
			return billingauth.Principal{}, billingauth.Refusal(billing.CodePermissionRequired)
		}
		return billingauth.Principal{MerchantID: resolved.MerchantID, Kind: billingauth.Machine, Permissions: resolved.Permissions}, nil
	}
	if g.DelegatedAuthenticator != nil && req != nil {
		principal, err := g.DelegatedAuthenticator.AuthenticateDelegated(ctx, req)
		if err != nil {
			return billingauth.Principal{}, billingauth.Unauthenticated(err)
		}
		resolved, verr := credential.ResolvedDelegatedFromHostPrincipal(principal)
		if verr != nil {
			return billingauth.Principal{}, billingauth.Refusal(billing.CodeDelegatedPrincipalInvalid)
		}
		if !resolved.HasPermission(perm) {
			return billingauth.Principal{}, billingauth.Refusal(billing.CodePermissionRequired)
		}
		return billingauth.Principal{
			MerchantID: resolved.MerchantID,
			Kind:       billingauth.Delegated,
			Subject:    resolved.DelegatedSubject,
			UserContext: billingauth.UserContext{
				UserID:        resolved.DelegatedSubject,
				Email:         resolved.Email,
				EmailVerified: resolved.EmailVerified,
				Username:      resolved.Username,
				Merchant:      resolved.Merchant,
			},
			Permissions: resolved.Permissions,
		}, nil
	}
	if g.Authenticator == nil {
		return billingauth.Principal{}, billingauth.Refusal(billing.CodeAuthenticationRequired, "bearer principal required")
	}
	uc, err := g.Authenticator.Authenticate(ctx, req)
	if err != nil {
		return billingauth.Principal{}, billingauth.Unauthenticated(err)
	}
	if verr := uc.ValidateSubject(); verr != nil {
		return billingauth.Principal{}, billingauth.Refusal(billing.CodeAuthenticationRequired, verr.Error())
	}
	if g.AdminPermissionChecker == nil {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusInternalServerError, Code: billing.CodeInternalError, Message: "authorization unavailable"}
	}
	if strings.TrimSpace(uc.Merchant) == "" {
		// The selector is an untrusted hint: membership is checked against the
		// merchant it names, and the resolved merchant must agree below.
		if target, ok := merchanttarget.FromContext(ctx); ok {
			uc.Merchant = target.MerchantSlug
		} else if req != nil {
			if selector, _, err := merchant.ParseSelector(req.Header); err == nil {
				uc.Merchant = selector.Slug
			}
		}
	}
	membershipMID, canonical, err := g.AdminPermissionChecker.ResolveAuthorizedMerchant(ctx, req, uc.Merchant, perm)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRevoked), errors.Is(err, billingauth.ErrUnauthenticated):
			return billingauth.Principal{}, credentialFailure(err)
		case errors.Is(err, billing.ErrPermissionRequired):
			return billingauth.Principal{}, billingauth.Refusal(billing.CodePermissionRequired)
		case errors.Is(err, billing.ErrMerchantUnresolved), errors.Is(err, credential.ErrMerchantAmbiguous):
			return billingauth.Principal{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
		default:
			return billingauth.Principal{}, billingauth.GateError{Status: http.StatusInternalServerError, Code: billing.CodeInternalError, Message: "failed to check permission"}
		}
	}
	if membershipMID.IsZero() {
		return billingauth.Principal{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	uc.Merchant = canonical
	mid, ok := merchant.FromContext(ctx)
	if !ok {
		mid = membershipMID
	}
	// #766: uc.Merchant was resolved from the USER'S group membership, but mid
	// may instead be the Host-pinned merchant (merchant.WithHostMerchant, set by
	// ResolveMerchantFromHostHTTP alongside merchant.WithID — #734). Without this
	// assertion a user with permission on merchant A, whose request lands on
	// merchant B's Host, would get a Principal scoped to B on authority checked
	// against A. Mirrors merchantForIssuer's identical Host-pin check
	// (internal/controlplane/issuer_registry.go) for the service-JWT/API-key/
	// delegated paths; HostMerchant (not the plain FromContext merchant) is the
	// right signal because it is a no-op unless a Host resolver actually ran, so
	// single-merchant self-hosters are unaffected.
	if hostMID, ok := merchant.HostMerchant(ctx); ok {
		if hostMID != membershipMID {
			return billingauth.Principal{}, billingauth.Refusal(billing.CodeHostMerchantMismatch)
		}
	}
	if mid != membershipMID {
		return billingauth.Principal{}, billingauth.Refusal(billing.CodeMerchantContextMismatch)
	}
	return billingauth.Principal{MerchantID: mid, Kind: billingauth.User, Subject: uc.UserID, UserContext: uc}, nil
}

// authorizeResourceToken gates an RFC 9068 access token: only its own
// verifier sees it, and it never falls through to another credential kind.
func (g legacyGate) authorizeResourceToken(req *http.Request, perm string) (billingauth.Principal, error) {
	if g.ResourceTokenResolver == nil {
		return billingauth.Principal{}, billingauth.Refusal(billing.CodeAccessTokenIssuerUnknown)
	}
	resolved, err := g.ResourceTokenResolver.ResolveResourceToken(req)
	if err != nil {
		return billingauth.Principal{}, credential.ResourceTokenRefusal(err)
	}
	if !resolved.HasPermission(perm) {
		return billingauth.Principal{}, billingauth.Refusal(billing.CodePermissionRequired)
	}
	if hostMID, ok := merchant.HostMerchant(req.Context()); ok && hostMID != resolved.MerchantID {
		return billingauth.Principal{}, billingauth.Refusal(billing.CodeHostMerchantMismatch)
	}
	principal := billingauth.Principal{MerchantID: resolved.MerchantID, Kind: billingauth.Delegated, Subject: resolved.Subject, Permissions: resolved.Permissions}
	if resolved.Machine {
		principal.Kind = billingauth.Machine
		return principal, nil
	}
	principal.UserContext = billingauth.UserContext{
		UserID: resolved.Subject, Email: resolved.Email, EmailVerified: resolved.EmailVerified,
		Username: resolved.Username, Merchant: resolved.MerchantSlug,
	}
	return principal, nil
}

// RequireRecentSignIn implements billingauth.Gate with the control plane's
// AuthKit Sensitive check.
func (g legacyGate) RequireRecentSignIn(ctx context.Context, req *http.Request, p billingauth.Principal) error {
	if req != nil && p.Kind == billingauth.Delegated && g.ResourceTokenResolver != nil &&
		credential.LooksLikeResourceToken(authorizationToken(req.Header.Get("Authorization"))) {
		return g.ResourceTokenResolver.RequireRecentResourceSignIn(req)
	}
	var check func(context.Context) error
	if g.AdminPermissionChecker != nil {
		check = func(ctx context.Context) error { return g.AdminPermissionChecker.CheckRecentSignIn(ctx, req) }
	}
	return billingauth.RequireRecentSignIn(ctx, p, check)
}

// resolveAPIKey resolves an API key; handled is false for any other
// credential.
func (g legacyGate) resolveAPIKey(ctx context.Context, r *http.Request) (*credential.ResolvedServiceCredential, error, bool) {
	resolver := g.ServiceCredentialResolver
	if resolver == nil || r == nil {
		return nil, nil, false
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" || !resolver.LooksLikeAPIKey(token) {
		return nil, nil, false
	}
	resolved, err := resolver.ResolveAPIKey(ctx, token)
	return resolved, err, true
}

// credentialFailure is the 401 for a credential a live check refused.
func credentialFailure(err error) billingauth.GateError {
	return billingauth.Unauthenticated(err)
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func authorizationToken(header string) string {
	fields := strings.Fields(header)
	if len(fields) == 2 && (strings.EqualFold(fields[0], "Bearer") || strings.EqualFold(fields[0], "DPoP")) {
		return fields[1]
	}
	return ""
}
