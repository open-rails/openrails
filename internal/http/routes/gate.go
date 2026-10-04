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
	AdminPermissionChecker    authpolicy.AdminPermissionChecker
	ServiceCredentialResolver ServiceCredentialResolver
	DelegatedResolver         DelegatedResolver
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

// DelegatedResolver validates a browser-direct delegated access token and
// resolves its merchant + acting user (#259/#555).
type DelegatedResolver interface {
	ResolveDelegated(r *http.Request) (*credential.ResolvedDelegated, error)
}

type serviceJWTResolver interface {
	ResolveServiceJWT(ctx context.Context, token string) (*credential.ResolvedServiceCredential, error)
}

type remoteApplicationResolver interface {
	ResolveRemoteApplication(ctx context.Context, token string) (*credential.ResolvedServiceCredential, error)
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
	if resolved, err, handled := g.resolveServiceCredential(ctx, req, g.Authenticator != nil); handled {
		if err != nil {
			switch {
			case errors.Is(err, credential.ErrServiceCredentialMerchantUnresolved):
				return billingauth.Principal{}, billingauth.Refusal(billing.CodeServiceCredentialMerchantUnresolved)
			case errors.Is(err, credential.ErrServiceCredentialScopeDenied):
				return billingauth.Principal{}, billingauth.Refusal(billing.CodeServiceCredentialResourceScopeDenied)
			case errors.Is(err, credential.ErrDelegatedIssuerUnknown), errors.Is(err, credential.ErrServiceCredentialHostMismatch):
				// The API key or issuer resolves to a different Host merchant.
				// Issuer resolution also uses its sentinel for unregistered/disabled issuers.
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
	if g.DelegatedResolver != nil && req != nil {
		if token := authorizationToken(req.Header.Get("Authorization")); credential.LooksLikeJWT(token) {
			resolved, err := g.DelegatedResolver.ResolveDelegated(req)
			if err != nil {
				if errors.Is(err, credential.ErrDelegatedUnavailable) {
					return billingauth.Principal{}, billingauth.Refusal(billing.CodeDelegatedVerificationUnavailable)
				}
				if errors.Is(err, auth.ErrSenderProofRequired) {
					return billingauth.Principal{}, billingauth.Refusal(billing.CodeSenderProofRequired)
				}
				if g.Authenticator == nil || !errors.Is(err, credential.ErrDelegatedInvalid) {
					return billingauth.Principal{}, billingauth.Refusal(billing.CodeDelegatedTokenInvalid)
				}
			} else {
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
		}
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

// RequireRecentSignIn implements billingauth.Gate with the control plane's
// AuthKit Sensitive check.
func (g legacyGate) RequireRecentSignIn(ctx context.Context, req *http.Request, p billingauth.Principal) error {
	var check func(context.Context) error
	if g.AdminPermissionChecker != nil {
		check = func(ctx context.Context) error { return g.AdminPermissionChecker.CheckRecentSignIn(ctx, req) }
	}
	return billingauth.RequireRecentSignIn(ctx, p, check)
}

func (g legacyGate) resolveServiceCredential(ctx context.Context, r *http.Request, allowJWTFallthrough bool) (*credential.ResolvedServiceCredential, error, bool) {
	resolver := g.ServiceCredentialResolver
	if resolver == nil || r == nil {
		return nil, nil, false
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return nil, nil, false
	}
	if resolver.LooksLikeAPIKey(token) {
		resolved, err := resolver.ResolveAPIKey(ctx, token)
		if err != nil {
			return nil, err, true
		}
		return resolved, nil, true
	}
	if !credential.LooksLikeJWT(token) {
		return nil, nil, false
	}
	if raResolver, ok := resolver.(remoteApplicationResolver); ok {
		resolved, err := raResolver.ResolveRemoteApplication(ctx, token)
		if err == nil {
			return resolved, nil, true
		}
		if allowJWTFallthrough && errors.Is(err, credential.ErrDelegatedInvalid) {
			return nil, nil, false
		}
		if !errors.Is(err, credential.ErrNotRemoteApplicationToken) {
			return nil, err, true
		}
	}
	if jwtResolver, ok := resolver.(serviceJWTResolver); ok {
		resolved, err := jwtResolver.ResolveServiceJWT(ctx, token)
		if err == nil {
			return resolved, nil, true
		}
		// A VERIFIED service JWT that is definitively rejected (cross-merchant
		// resource scope, its issuer owns no merchant, or — #734 — its issuer's
		// merchant disagrees with the request's Host-pinned merchant) must surface
		// as 403 — not fall through to the delegated/user-session paths, which
		// would mislabel it 401 access_token_wrong_typ. A wrong-typ (not-a-service-JWT)
		// error still falls through so delegated/user tokens reach their own resolvers.
		if errors.Is(err, credential.ErrServiceCredentialScopeDenied) ||
			errors.Is(err, credential.ErrServiceCredentialMerchantUnresolved) ||
			errors.Is(err, credential.ErrDelegatedIssuerUnknown) {
			return nil, err, true
		}
	}
	return nil, nil, false
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
