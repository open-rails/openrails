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
)

// StandaloneAuth is the standalone server's Auth for the admin API:
// merchant API keys, trusted issuers' access tokens (#1140) and the control
// plane's own user sessions, through the same route gate an embedded host's
// middleware goes through. Its RequirePermission resolves the merchant the
// credential acts on.
type StandaloneAuth struct {
	// Issuer is the control plane's issuer: its users' and API keys'.
	Issuer                    string
	Authenticator             billingauth.Authenticator
	ResourceTokenResolver     ResourceTokenResolver
	AdminPermissionChecker    authpolicy.AdminPermissionChecker
	ServiceCredentialResolver ServiceCredentialResolver
}

var _ billingauth.Auth = (*StandaloneAuth)(nil)

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

// ResourceCustomerResolver verifies a trusted issuer's access token granted
// openrails:self and resolves the customer it acts as (#1140).
type ResourceCustomerResolver interface {
	ResolveResourceCustomer(r *http.Request) (*credential.ResolvedDelegated, error)
}

// StaffPrincipal is a standalone staff credential as RequirePermission
// resolved it: the merchant it acts on and its grant set, which the control
// plane's own handlers read (no-escalation checks).
type StaffPrincipal struct {
	MerchantID   billing.MerchantID
	MerchantSlug string
	Issuer       string
	ID           string
	Credential   string
	Machine      bool
	// Permissions is a machine or access token's grant set; empty for a user
	// session, whose authority is checked live.
	Permissions []string
}

// standaloneCredential is what Required verified.
type standaloneCredential struct {
	identity billingauth.Identity
	apiKey   *credential.ResolvedServiceCredential
	resource *credential.ResolvedResourceAccess
	user     *billingauth.UserContext
}

type standaloneCredentialKey struct{}
type standalonePrincipalKey struct{}

// StandalonePrincipal is the staff credential RequirePermission resolved for
// r's route.
func StandalonePrincipal(r *http.Request) (StaffPrincipal, bool) {
	if r == nil {
		return StaffPrincipal{}, false
	}
	p, ok := r.Context().Value(standalonePrincipalKey{}).(StaffPrincipal)
	return p, ok && p.ID != ""
}

// Required admits a merchant API key, a trusted issuer's access token
// granted openrails:merchant, or a control-plane user session.
func (a *StandaloneAuth) Required() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cred, err := a.authenticate(r)
			if err != nil {
				refuseWith(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), standaloneCredentialKey{}, cred)))
		})
	}
}

// RequirePermission authenticates the request (unless Required already
// did) and admits a credential holding perm on the merchant it resolves to,
// binding that merchant.
func (a *StandaloneAuth) RequirePermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cred, ok := r.Context().Value(standaloneCredentialKey{}).(standaloneCredential)
			if !ok {
				var err error
				if cred, err = a.authenticate(r); err != nil {
					refuseWith(w, r, err)
					return
				}
				r = r.WithContext(context.WithValue(r.Context(), standaloneCredentialKey{}, cred))
			}
			p, err := a.authorize(r.Context(), r, cred, perm)
			if err != nil {
				refuseWith(w, r, err)
				return
			}
			if prior, ok := StandalonePrincipal(r); ok && prior.MerchantID != p.MerchantID {
				refuseWith(w, r, billingauth.Refusal(billing.CodeMerchantContextMismatch))
				return
			}
			ctx := context.WithValue(r.Context(), standalonePrincipalKey{}, p)
			// A selector already resolved to this merchant (a former name
			// included) stays as the request named it.
			if resolved, ok := merchanttarget.FromContext(ctx); !ok || resolved.MerchantID != p.MerchantID {
				ctx = merchanttarget.WithResolved(ctx, billingauth.Target{MerchantID: p.MerchantID, MerchantSlug: p.MerchantSlug})
			}
			next.ServeHTTP(w, r.WithContext(merchant.WithID(ctx, p.MerchantID)))
		})
	}
}

// Sensitive admits a control-plane user whose sign-in AuthKit finds recent,
// and a trusted issuer's user whose token's auth_time is recent. An API key
// carries no sign-in.
func (a *StandaloneAuth) Sensitive() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cred, ok := r.Context().Value(standaloneCredentialKey{}).(standaloneCredential)
			var err error
			switch {
			case !ok:
				err = billingauth.ErrUnauthenticated
			case cred.resource != nil && a.ResourceTokenResolver != nil:
				err = a.ResourceTokenResolver.RequireRecentResourceSignIn(r)
			case cred.resource != nil:
				err = billingauth.Refusal(billing.CodeStepUpUnavailable)
			case cred.user == nil:
			case a.AdminPermissionChecker == nil:
				err = billingauth.ErrRecentSignInUnavailable
			default:
				err = a.AdminPermissionChecker.CheckRecentSignIn(r.Context(), r)
			}
			if err != nil {
				refuseWith(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Identity is who Required or RequirePermission admitted.
func (a *StandaloneAuth) Identity(ctx context.Context) (billingauth.Identity, bool) {
	cred, ok := ctx.Value(standaloneCredentialKey{}).(standaloneCredential)
	return cred.identity, ok && cred.identity.Subject != ""
}

func (a *StandaloneAuth) authenticate(r *http.Request) (standaloneCredential, error) {
	if credential.LooksLikeResourceToken(authorizationToken(r.Header.Get("Authorization"))) {
		if a.ResourceTokenResolver == nil {
			return standaloneCredential{}, billingauth.Refusal(billing.CodeAccessTokenIssuerUnknown)
		}
		resolved, err := a.ResourceTokenResolver.ResolveResourceToken(r)
		if err != nil {
			return standaloneCredential{}, credential.ResourceTokenRefusal(err)
		}
		// sub is the issuer's user; a client acting for itself (sub =
		// client_id) is a service.
		id, kind := resolved.Subject, billingauth.SubjectUser
		if resolved.Machine {
			kind = billingauth.SubjectApplication
			if resolved.ClientID != "" {
				id = resolved.ClientID
			}
		}
		if strings.TrimSpace(id) == "" {
			return standaloneCredential{}, billingauth.Refusal(billing.CodeAccessTokenInvalid)
		}
		return standaloneCredential{resource: resolved, identity: billingauth.Identity{
			Issuer: resolved.Issuer, Subject: id, SubjectKind: kind, Invoker: billingauth.Invoker{Issuer: resolved.Issuer, ID: id},
			Credential: billingauth.Credential{Kind: billingauth.CredentialAccessToken, ID: resolved.SessionID},
			Email:      resolved.Email, EmailVerified: resolved.EmailVerified, Username: resolved.Username,
		}}, nil
	}
	if resolved, err, handled := a.resolveAPIKey(r.Context(), r); handled {
		switch {
		case errors.Is(err, credential.ErrServiceCredentialMerchantUnresolved):
			return standaloneCredential{}, billingauth.Refusal(billing.CodeServiceCredentialMerchantUnresolved)
		case errors.Is(err, credential.ErrServiceCredentialScopeDenied):
			return standaloneCredential{}, billingauth.Refusal(billing.CodeServiceCredentialResourceScopeDenied)
		case errors.Is(err, credential.ErrServiceCredentialHostMismatch):
			return standaloneCredential{}, billingauth.Refusal(billing.CodeHostMerchantMismatch)
		case err != nil, resolved == nil, resolved != nil && strings.TrimSpace(resolved.KeyID) == "":
			return standaloneCredential{}, billingauth.Refusal(billing.CodeServiceCredentialInvalid)
		}
		issuer := resolved.Issuer
		if issuer == "" {
			issuer = a.Issuer
		}
		// The key belongs to its merchant's group: that service is who acts.
		return standaloneCredential{apiKey: resolved, identity: billingauth.Identity{
			Issuer: issuer, Subject: resolved.OwnerGroupID, SubjectKind: billingauth.SubjectApplication, Invoker: billingauth.Invoker{Issuer: issuer, ID: resolved.OwnerGroupID},
			Credential: billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: resolved.KeyID},
		}}, nil
	}
	if a.Authenticator == nil {
		return standaloneCredential{}, billingauth.ErrUnauthenticated
	}
	uc, err := a.Authenticator.Authenticate(r.Context(), r)
	if err != nil {
		return standaloneCredential{}, billingauth.Unauthenticated(err)
	}
	if verr := uc.ValidateSubject(); verr != nil {
		return standaloneCredential{}, billingauth.Refusal(billing.CodeAuthenticationRequired, verr.Error())
	}
	return standaloneCredential{user: &uc, identity: billingauth.Identity{
		Issuer: a.Issuer, Subject: uc.UserID, SubjectKind: billingauth.SubjectUser, Invoker: billingauth.Invoker{Issuer: a.Issuer, ID: uc.UserID},
		Credential: billingauth.Credential{Kind: billingauth.CredentialSession, ID: uc.SessionID},
		Email:      uc.Email, EmailVerified: uc.EmailVerified, Username: uc.Username,
	}}, nil
}

func (a *StandaloneAuth) authorize(ctx context.Context, req *http.Request, cred standaloneCredential, perm string) (StaffPrincipal, error) {
	c := cred.identity
	machine := c.SubjectKind == billingauth.SubjectApplication
	switch {
	case cred.resource != nil:
		resolved := cred.resource
		if !resolved.HasPermission(perm) {
			return StaffPrincipal{}, billingauth.Refusal(billing.CodePermissionRequired)
		}
		if hostMID, ok := merchant.HostMerchant(ctx); ok && hostMID != resolved.MerchantID {
			return StaffPrincipal{}, billingauth.Refusal(billing.CodeHostMerchantMismatch)
		}
		return StaffPrincipal{MerchantID: resolved.MerchantID, MerchantSlug: resolved.MerchantSlug, Issuer: c.Issuer, ID: c.Subject, Credential: string(billingauth.CredentialAccessToken), Machine: machine, Permissions: resolved.Permissions}, nil
	case cred.apiKey != nil:
		resolved := cred.apiKey
		if !resolved.HasPermission(perm) {
			return StaffPrincipal{}, billingauth.Refusal(billing.CodePermissionRequired)
		}
		return StaffPrincipal{MerchantID: resolved.MerchantID, MerchantSlug: resolved.MerchantSlug, Issuer: c.Issuer, ID: c.Credential.ID, Credential: string(billingauth.CredentialAPIKey), Machine: true, Permissions: resolved.Permissions}, nil
	case cred.user == nil:
		return StaffPrincipal{}, billingauth.ErrUnauthenticated
	}
	if a.AdminPermissionChecker == nil {
		return StaffPrincipal{}, billingauth.Refusal(billing.CodeAuthorizationUnavailable)
	}
	ref := strings.TrimSpace(cred.user.Merchant)
	if ref == "" {
		// The selector is an untrusted hint: membership is checked against the
		// merchant it names, and the resolved merchant must agree below.
		if target, ok := merchanttarget.FromContext(ctx); ok {
			ref = target.MerchantSlug
		} else if selector, _, err := merchant.ParseSelector(req.Header); err == nil {
			ref = selector.Slug
		}
	}
	membershipMID, canonical, err := a.AdminPermissionChecker.ResolveAuthorizedMerchant(ctx, req, ref, perm)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRevoked), errors.Is(err, billingauth.ErrUnauthenticated):
			return StaffPrincipal{}, billingauth.Unauthenticated(err)
		case errors.Is(err, billing.ErrPermissionRequired):
			return StaffPrincipal{}, billingauth.Refusal(billing.CodePermissionRequired)
		case errors.Is(err, billing.ErrMerchantUnresolved), errors.Is(err, credential.ErrMerchantAmbiguous):
			return StaffPrincipal{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
		default:
			return StaffPrincipal{}, billingauth.Refusal(billing.CodeAuthorizationUnavailable)
		}
	}
	if membershipMID.IsZero() {
		return StaffPrincipal{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	// #766: membership was checked on the user's merchant; a Host-pinned
	// request must be that merchant's.
	if hostMID, ok := merchant.HostMerchant(ctx); ok && hostMID != membershipMID {
		return StaffPrincipal{}, billingauth.Refusal(billing.CodeHostMerchantMismatch)
	}
	if mid, ok := merchant.FromContext(ctx); ok && mid != membershipMID {
		return StaffPrincipal{}, billingauth.Refusal(billing.CodeMerchantContextMismatch)
	}
	return StaffPrincipal{MerchantID: membershipMID, MerchantSlug: canonical, Issuer: c.Issuer, ID: c.Subject, Credential: string(billingauth.CredentialSession)}, nil
}

// StandaloneCustomers is the standalone server's Auth for /v1/me: a trusted
// issuer's DPoP-bound access token granted openrails:self is its user's own
// billing, at the merchant the request names or the issuer's only one,
// which its Required binds.
type StandaloneCustomers struct{ Resolver ResourceCustomerResolver }

var _ billingauth.Auth = StandaloneCustomers{}

type standaloneCustomerKey struct{}

// Required admits a customer's access token.
func (a StandaloneCustomers) Required() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a.Resolver == nil {
				refuseWith(w, r, billingauth.Refusal(billing.CodeAuthenticationUnavailable))
				return
			}
			token := authorizationToken(r.Header.Get("Authorization"))
			if token == "" {
				refuseWith(w, r, billingauth.Refusal(billing.CodeAuthenticationRequired, "access token required"))
				return
			}
			if !credential.LooksLikeResourceToken(token) {
				refuseWith(w, r, billingauth.Refusal(billing.CodeAccessTokenInvalid))
				return
			}
			resolved, err := a.Resolver.ResolveResourceCustomer(r)
			if err != nil {
				refuseWith(w, r, credential.ResourceTokenRefusal(err))
				return
			}
			// The issuer's own user, acting on their billing.
			c := billingauth.Identity{
				Issuer: resolved.Issuer, Subject: resolved.CustomerID.String(), SubjectKind: billingauth.SubjectUser,
				Invoker:    billingauth.Invoker{Issuer: resolved.Issuer, ID: resolved.CustomerID.String()},
				Credential: billingauth.Credential{Kind: billingauth.CredentialAccessToken},
				Email:      resolved.Email, EmailVerified: resolved.EmailVerified, Username: resolved.Username,
			}
			ctx := context.WithValue(r.Context(), standaloneCustomerKey{}, c)
			ctx = merchanttarget.WithResolved(ctx, billingauth.Target{MerchantID: resolved.MerchantID, MerchantSlug: resolved.MerchantSlug})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequirePermission admits nobody: a customer's token holds no merchant
// permission.
func (StandaloneCustomers) RequirePermission(string) func(http.Handler) http.Handler {
	return refuseAll
}

// Sensitive admits nobody.
func (StandaloneCustomers) Sensitive() func(http.Handler) http.Handler { return refuseAll }

// Identity is the customer Required admitted.
func (StandaloneCustomers) Identity(ctx context.Context) (billingauth.Identity, bool) {
	c, ok := ctx.Value(standaloneCustomerKey{}).(billingauth.Identity)
	return c, ok && c.Subject != ""
}

func refuseAll(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { refuseWith(w, r, billingauth.ErrForbidden) })
}

// resolveAPIKey resolves an API key; handled is false for any other
// credential.
func (a *StandaloneAuth) resolveAPIKey(ctx context.Context, r *http.Request) (*credential.ResolvedServiceCredential, error, bool) {
	resolver := a.ServiceCredentialResolver
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
