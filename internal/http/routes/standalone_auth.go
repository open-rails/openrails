package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// StandaloneAuth is the standalone server's Authenticator for the admin API
// and the programmatic routes: merchant API keys, trusted issuers' access
// tokens (#1140) and the control plane's own users' sessions, through the
// same route gate as an embedded host's. A key or a token names its
// merchant; a session's is the one ResolveMerchant finds.
type StandaloneAuth struct {
	// Issuer is the control plane's issuer: its users', API keys' and
	// merchant groups'.
	Issuer string
	// Sessions says who the control plane's own users are (AuthKit's
	// Authenticator); it admits users only here.
	Sessions                  billingauth.Authenticator
	ResourceTokenResolver     ResourceTokenResolver
	ServiceCredentialResolver ServiceCredentialResolver
	Directory                 StaffDirectory
	// Now is the clock a token's sign-in age is read on; nil is time.Now.
	Now func() time.Time
}

var _ billingauth.Authenticator = (*StandaloneAuth)(nil)

type ServiceCredentialResolver interface {
	LooksLikeAPIKey(token string) bool
	ResolveAPIKey(ctx context.Context, token string) (*credential.ResolvedServiceCredential, error)
}

// ResourceTokenResolver verifies a trusted issuer's RFC 9068 access token
// and resolves its merchant and permissions (#1140).
type ResourceTokenResolver interface {
	ResolveResourceToken(r *http.Request) (*credential.ResolvedResourceAccess, error)
}

// ResourceCustomerResolver verifies a trusted issuer's access token granted
// openrails:self and resolves the customer it acts as (#1140).
type ResourceCustomerResolver interface {
	ResolveResourceCustomer(r *http.Request) (*credential.ResolvedDelegated, error)
}

// StaffDirectory is the control plane's merchants as the staff gate reads
// them.
type StaffDirectory interface {
	// UserMerchant is the live merchant a control-plane user acts on: the
	// one ref names (a current or former name), else the only one they hold
	// a role in. billing.ErrMerchantUnresolved, credential.ErrMerchantAmbiguous.
	UserMerchant(ctx context.Context, userID, ref string) (billing.MerchantID, string, error)
	// MerchantGroup is the permission group whose members administer mid.
	MerchantGroup(ctx context.Context, mid billing.MerchantID) (string, error)
}

// Authenticate admits a merchant API key, a trusted issuer's access token
// granted openrails:merchant, or a control-plane user's session.
func (a *StandaloneAuth) Authenticate(r *http.Request) (billingauth.Verified, error) {
	if r == nil {
		return nil, billingauth.ErrUnauthenticated
	}
	if credential.LooksLikeResourceToken(authorizationToken(r.Header.Get("Authorization"))) {
		return a.resourceToken(r)
	}
	if v, err, handled := a.apiKey(r); handled {
		return v, err
	}
	if isNil(a.Sessions) {
		return nil, billingauth.ErrUnauthenticated
	}
	v, err := a.Sessions.Authenticate(r)
	if err != nil {
		return nil, err
	}
	if id := v.Identity(); id.SubjectKind != billingauth.SubjectUser || !id.SelfInvoked() {
		return nil, billingauth.Refusal(billing.CodeAuthenticationRequired, "the control plane's sessions are its users'")
	}
	return v, nil
}

func (a *StandaloneAuth) resourceToken(r *http.Request) (billingauth.Verified, error) {
	if a.ResourceTokenResolver == nil {
		return nil, billingauth.Refusal(billing.CodeAccessTokenIssuerUnknown)
	}
	resolved, err := a.ResourceTokenResolver.ResolveResourceToken(r)
	if err != nil {
		return nil, credential.ResourceTokenRefusal(err)
	}
	// sub is the issuer's user; a client acting for itself (sub =
	// client_id) is an application.
	id, kind := resolved.Subject, billingauth.SubjectUser
	if resolved.Machine {
		kind = billingauth.SubjectApplication
		if resolved.ClientID != "" {
			id = resolved.ClientID
		}
	}
	if strings.TrimSpace(id) == "" || resolved.MerchantID.IsZero() {
		return nil, billingauth.Refusal(billing.CodeAccessTokenInvalid)
	}
	return tokenVerified{
		MerchantBinding: billingauth.BindsMerchant(billingauth.Target{MerchantID: resolved.MerchantID, MerchantSlug: resolved.MerchantSlug}),
		auth:            a,
		token:           resolved,
		id: billingauth.Identity{
			Issuer: resolved.Issuer, Subject: id, SubjectKind: kind, Invoker: billingauth.Invoker{Issuer: resolved.Issuer, ID: id},
			Credential: billingauth.Credential{Kind: billingauth.CredentialAccessToken, ID: resolved.SessionID},
			Email:      resolved.Email, EmailVerified: resolved.EmailVerified, Username: resolved.Username,
		},
	}, nil
}

// apiKey admits a merchant API key; handled is false for any other
// credential.
func (a *StandaloneAuth) apiKey(r *http.Request) (billingauth.Verified, error, bool) {
	resolver := a.ServiceCredentialResolver
	if resolver == nil {
		return nil, nil, false
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" || !resolver.LooksLikeAPIKey(token) {
		return nil, nil, false
	}
	resolved, err := resolver.ResolveAPIKey(r.Context(), token)
	switch {
	case errors.Is(err, credential.ErrServiceCredentialMerchantUnresolved):
		return nil, billingauth.Refusal(billing.CodeServiceCredentialMerchantUnresolved), true
	case errors.Is(err, credential.ErrServiceCredentialScopeDenied):
		return nil, billingauth.Refusal(billing.CodeServiceCredentialResourceScopeDenied), true
	case errors.Is(err, credential.ErrServiceCredentialHostMismatch):
		return nil, billingauth.Refusal(billing.CodeHostMerchantMismatch), true
	case err != nil, resolved == nil, resolved != nil && (strings.TrimSpace(resolved.KeyID) == "" || resolved.MerchantID.IsZero()):
		return nil, billingauth.Refusal(billing.CodeServiceCredentialInvalid), true
	}
	issuer := resolved.Issuer
	if issuer == "" {
		issuer = a.Issuer
	}
	// The key belongs to its merchant's group: that application is who acts.
	return keyVerified{
		MerchantBinding: billingauth.BindsMerchant(billingauth.Target{MerchantID: resolved.MerchantID, MerchantSlug: resolved.MerchantSlug}),
		scope:           billingauth.Scope{Authority: a.Issuer, ID: resolved.OwnerGroupID},
		key:             resolved,
		id: billingauth.Identity{
			Issuer: issuer, Subject: resolved.OwnerGroupID, SubjectKind: billingauth.SubjectApplication, Invoker: billingauth.Invoker{Issuer: issuer, ID: resolved.OwnerGroupID},
			Credential: billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: resolved.KeyID},
		},
	}, nil, true
}

// Scope is where mid's staff hold their permissions: its permission group,
// in the control plane's issuer.
func (a *StandaloneAuth) Scope(ctx context.Context, mid billing.MerchantID) (billingauth.Scope, error) {
	if a.Directory == nil {
		return billingauth.Scope{}, billingauth.Refusal(billing.CodeAuthorizationUnavailable)
	}
	group, err := a.Directory.MerchantGroup(ctx, mid)
	switch {
	case errors.Is(err, billing.ErrMerchantUnresolved):
		return billingauth.Scope{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	case err != nil:
		return billingauth.Scope{}, errors.Join(auth.ErrUnavailable, err)
	case strings.TrimSpace(group) == "":
		return billingauth.Scope{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	return billingauth.Scope{Authority: a.Issuer, ID: group}, nil
}

// ResolveMerchant is the merchant a control-plane user's session acts on:
// the one the request selects, else the only one they hold a role in. The
// gate's Can then checks their permission there, live.
func (a *StandaloneAuth) ResolveMerchant(r *http.Request, v billingauth.Verified) (billingauth.Target, error) {
	id := v.Identity()
	if id.SubjectKind != billingauth.SubjectUser || a.Directory == nil {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	ctx := r.Context()
	// The selector is an untrusted hint: Can checks the merchant it names.
	ref := ""
	if target, ok := merchanttarget.FromContext(ctx); ok {
		ref = target.MerchantSlug
	} else if selector, _, err := merchant.ParseSelector(r.Header); err == nil {
		ref = selector.Slug
	}
	mid, slug, err := a.Directory.UserMerchant(ctx, id.Subject, strings.TrimSpace(ref))
	switch {
	case errors.Is(err, billing.ErrMerchantUnresolved), errors.Is(err, credential.ErrMerchantAmbiguous):
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	case err != nil:
		return billingauth.Target{}, errors.Join(billingauth.Refusal(billing.CodeAuthorizationUnavailable), err)
	case mid.IsZero():
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	if bound, ok := merchant.FromContext(ctx); ok && !bound.IsZero() && bound != mid {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantContextMismatch)
	}
	return billingauth.Target{MerchantID: mid, MerchantSlug: slug}, nil
}

func (a *StandaloneAuth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// exactPermission is a permission, never a pattern.
func exactPermission(p string) bool {
	return p != "" && strings.TrimSpace(p) == p && !strings.Contains(p, "*")
}

// keyVerified is a merchant API key: its role's permissions, in its group.
type keyVerified struct {
	billingauth.MerchantBinding
	id    billingauth.Identity
	scope billingauth.Scope
	key   *credential.ResolvedServiceCredential
}

func (v keyVerified) Identity() billingauth.Identity { return v.id }

func (v keyVerified) Can(_ context.Context, scope billingauth.Scope, permission string) (bool, error) {
	return exactPermission(permission) && scope.ID != "" && scope == v.scope && v.key.HasPermission(permission), nil
}

// CheckRecentSignIn: an API key has no sign-in of its own.
func (keyVerified) CheckRecentSignIn(context.Context) error { return auth.ErrForbidden }

// tokenVerified is a trusted issuer's access token: its grant, at the
// merchant it acts for.
type tokenVerified struct {
	billingauth.MerchantBinding
	id    billingauth.Identity
	auth  *StandaloneAuth
	token *credential.ResolvedResourceAccess
}

func (v tokenVerified) Identity() billingauth.Identity { return v.id }

func (v tokenVerified) Can(ctx context.Context, scope billingauth.Scope, permission string) (bool, error) {
	if !exactPermission(permission) || scope.ID == "" || scope.Authority != v.auth.Issuer || !v.token.HasPermission(permission) {
		return false, nil
	}
	own, err := v.auth.Scope(ctx, v.token.MerchantID)
	if err != nil {
		return false, err
	}
	return scope == own, nil
}

// CheckRecentSignIn reads the token's auth_time; a client acting for itself
// has no sign-in of its own.
func (v tokenVerified) CheckRecentSignIn(context.Context) error {
	return v.token.CheckRecentSignIn(v.auth.now())
}

// StandaloneCustomers is the standalone server's Authenticator for /v1/me: a
// trusted issuer's DPoP-bound access token granted openrails:self is its
// user's own billing, at the merchant the request names or the issuer's only
// one, which the credential names.
type StandaloneCustomers struct{ Resolver ResourceCustomerResolver }

var _ billingauth.Authenticator = StandaloneCustomers{}

// Authenticate admits a customer's access token.
func (a StandaloneCustomers) Authenticate(r *http.Request) (billingauth.Verified, error) {
	if a.Resolver == nil {
		return nil, billingauth.Refusal(billing.CodeAuthenticationUnavailable)
	}
	if r == nil {
		return nil, billingauth.ErrUnauthenticated
	}
	token := authorizationToken(r.Header.Get("Authorization"))
	switch {
	case token == "":
		return nil, billingauth.Refusal(billing.CodeAuthenticationRequired, "access token required")
	case !credential.LooksLikeResourceToken(token):
		return nil, billingauth.Refusal(billing.CodeAccessTokenInvalid)
	}
	resolved, err := a.Resolver.ResolveResourceCustomer(r)
	if err != nil {
		return nil, credential.ResourceTokenRefusal(err)
	}
	if resolved.MerchantID.IsZero() {
		return nil, billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	// The issuer's own user, acting on their billing.
	subject := resolved.CustomerID.String()
	return customerVerified{
		MerchantBinding: billingauth.BindsMerchant(billingauth.Target{MerchantID: resolved.MerchantID, MerchantSlug: resolved.MerchantSlug}),
		id: billingauth.Identity{
			Issuer: resolved.Issuer, Subject: subject, SubjectKind: billingauth.SubjectUser,
			Invoker:    billingauth.Invoker{Issuer: resolved.Issuer, ID: subject},
			Credential: billingauth.Credential{Kind: billingauth.CredentialAccessToken},
			Email:      resolved.Email, EmailVerified: resolved.EmailVerified, Username: resolved.Username,
		},
	}, nil
}

// customerVerified is a customer's token: identity only, holding nothing.
type customerVerified struct {
	billingauth.MerchantBinding
	id billingauth.Identity
}

func (v customerVerified) Identity() billingauth.Identity { return v.id }

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
