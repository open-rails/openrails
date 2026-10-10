package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/internal/staffperm"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

var (
	merchantA = billing.MerchantID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	merchantB = billing.MerchantID(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
)

func userAuth(uc billingauth.UserContext, err error) billingauth.Authenticator {
	return billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) { return uc, err })
}

// membership grants the permission on the listed merchant slugs; a zero ID is
// a granted but unresolvable merchant.
type membership struct {
	granted  map[string]billing.MerchantID
	inferred string
	err      error
	refs     []string
}

func (m *membership) ResolveAuthorizedMerchant(_ context.Context, _ *http.Request, ref, _ string) (billing.MerchantID, string, error) {
	m.refs = append(m.refs, ref)
	if m.err != nil {
		return billing.MerchantID{}, "", m.err
	}
	if ref == "" {
		if ref = m.inferred; ref == "" {
			return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
		}
	}
	id, ok := m.granted[ref]
	if !ok {
		return billing.MerchantID{}, "", billing.ErrPermissionRequired
	}
	return id, ref, nil
}

func (m *membership) CheckRecentSignIn(context.Context, *http.Request) error { return nil }

type credResolver struct {
	key    *credential.ResolvedServiceCredential
	keyErr error
}

func (credResolver) LooksLikeAPIKey(token string) bool { return strings.HasPrefix(token, "sk_") }
func (c credResolver) ResolveAPIKey(context.Context, string) (*credential.ResolvedServiceCredential, error) {
	return c.key, c.keyErr
}

func serviceCredential(perms ...string) *credential.ResolvedServiceCredential {
	return &credential.ResolvedServiceCredential{KeyID: "key_1", OwnerGroupID: "group_1", MerchantID: merchantA, Permissions: perms}
}

// chainResult is what the handler behind an Auth's Required and
// RequirePermission saw.
type chainResult struct {
	status    int
	code      string
	who       billingauth.Identity
	principal StaffPrincipal
	merchant  billing.MerchantID
}

func runStaff(t *testing.T, a billingauth.Auth, perm string, header map[string]string, ctx func(context.Context) context.Context) chainResult {
	t.Helper()
	var out chainResult
	h := a.Required()(a.RequirePermission(perm)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out.who, _ = a.Identity(r.Context())
		out.principal, _ = StandalonePrincipal(r)
		out.merchant, _ = merchant.FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})))
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/configuration", nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	if ctx != nil {
		r = r.WithContext(ctx(r.Context()))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	out.status = rec.Code
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	out.code = body.Error.Code
	return out
}

// Every standalone credential kind reaches a merchant route only through its
// own verified merchant and an explicit permission; failures keep distinct,
// stable codes.
func TestStandaloneAuthEachCredentialKind(t *testing.T) {
	read := staffperm.Admin
	for _, tc := range []struct {
		name    string
		auth    *StandaloneAuth
		header  map[string]string
		status  int
		code    string
		subject string
		kind    billingauth.SubjectKind
	}{
		{name: "api key", auth: &StandaloneAuth{Issuer: "cp", ServiceCredentialResolver: credResolver{key: serviceCredential(read)}}, header: bearer("sk_1"), status: 204, subject: "group_1", kind: billingauth.SubjectApplication},
		{name: "api key glob", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{key: serviceCredential("merchant:*")}}, header: bearer("sk_1"), status: 204, subject: "group_1", kind: billingauth.SubjectApplication},
		{name: "api key lacking permission", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{key: serviceCredential("root:*")}}, header: bearer("sk_1"), status: 403, code: "permission_required"},
		{name: "api key resolved to nothing", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{}}, header: bearer("sk_1"), status: 401, code: "service_credential_invalid"},
		{name: "api key without an id", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{key: &credential.ResolvedServiceCredential{MerchantID: merchantA, Permissions: []string{read}}}}, header: bearer("sk_1"), status: 401, code: "service_credential_invalid"},
		{name: "api key scope denied", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialScopeDenied}}, header: bearer("sk_1"), status: 403, code: "service_credential_resource_scope_denied"},
		{name: "api key merchant unresolved", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialMerchantUnresolved}}, header: bearer("sk_1"), status: 403, code: "service_credential_merchant_unresolved"},
		{name: "api key for another host", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialHostMismatch}}, header: bearer("sk_1"), status: 403, code: "host_merchant_mismatch"},
		{name: "api key invalid", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: errors.New("bad key")}}, header: bearer("sk_1"), status: 401, code: "service_credential_invalid"},
		{name: "a JWT that is no access token is a user session", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{key: serviceCredential(read)}, Authenticator: userAuth(billingauth.UserContext{}, billingauth.ErrUnauthenticated)}, header: bearer("a.b.c"), status: 401, code: "authentication_required"},
		{name: "no credential path", auth: &StandaloneAuth{}, status: 401, code: "authentication_required"},
		{name: "user with opaque subject", auth: &StandaloneAuth{Authenticator: userAuth(billingauth.UserContext{UserID: "user-1"}, nil), AdminPermissionChecker: &membership{}}, status: 401, code: "authentication_required"},
		{name: "user without membership checker", auth: &StandaloneAuth{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil)}, status: 503, code: "authorization_unavailable"},
		{name: "membership lookup failure", auth: &StandaloneAuth{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), AdminPermissionChecker: &membership{err: errors.New("db")}}, status: 503, code: "authorization_unavailable"},
		{name: "revoked session", auth: &StandaloneAuth{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), AdminPermissionChecker: &membership{err: errors.Join(errors.New("session_revoked"), auth.ErrRevoked)}}, status: 401, code: "credential_revoked"},
		{name: "ambiguous membership", auth: &StandaloneAuth{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), AdminPermissionChecker: &membership{err: credential.ErrMerchantAmbiguous}}, status: 403, code: "merchant_unresolved"},
		{name: "user session", auth: &StandaloneAuth{Issuer: "cp", Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), AdminPermissionChecker: &membership{granted: map[string]billing.MerchantID{"a": merchantA}, inferred: "a"}}, status: 204, subject: userA, kind: billingauth.SubjectUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runStaff(t, tc.auth, read, tc.header, nil)
			require.Equal(t, tc.status, got.status, got.code)
			if tc.code != "" {
				require.Equal(t, tc.code, got.code)
			}
			if tc.status == 204 {
				require.Equal(t, tc.subject, got.who.Subject)
				require.Equal(t, tc.kind, got.who.SubjectKind)
				require.True(t, billingauth.SelfActing(got.who), "a standalone subject acts itself")
				require.Equal(t, merchantA, got.principal.MerchantID)
				require.Equal(t, merchantA, got.merchant, "RequirePermission binds the credential's merchant")
			}
		})
	}
}

// A user session's merchant comes from its token or an explicit selector, is
// authorized by live membership, and must agree with any Host or context pin.
func TestStandaloneUserSessionMerchantSelection(t *testing.T) {
	for _, tc := range []struct {
		name, tokenMerchant, selector string
		checker                       membership
		ctx                           func(context.Context) context.Context
		status                        int
		code                          string
		merchant                      billing.MerchantID
		asked, slug                   string
	}{
		{name: "explicit selector", selector: " b ", checker: membership{granted: map[string]billing.MerchantID{"b": merchantB}}, status: 204, merchant: merchantB, asked: "b", slug: "b"},
		{name: "single membership inferred", checker: membership{granted: map[string]billing.MerchantID{"a": merchantA}, inferred: "a"}, status: 204, merchant: merchantA, asked: "", slug: "a"},
		{name: "no selector and no single membership", checker: membership{}, status: 403, code: "merchant_unresolved", asked: ""},
		{name: "selected merchant without live permission", selector: "b", checker: membership{granted: map[string]billing.MerchantID{"a": merchantA}}, status: 403, code: "permission_required", asked: "b"},
		{name: "granted merchant that does not resolve", selector: "b", checker: membership{granted: map[string]billing.MerchantID{"b": {}}}, status: 403, code: "merchant_unresolved", asked: "b"},
		{name: "token merchant cannot be overridden", tokenMerchant: "a", selector: "b", checker: membership{granted: map[string]billing.MerchantID{"a": merchantA, "b": merchantB}}, status: 204, merchant: merchantA, asked: "a", slug: "a"},
		{name: "must match the Host merchant", selector: "a", checker: membership{granted: map[string]billing.MerchantID{"a": merchantA}},
			ctx: func(ctx context.Context) context.Context { return merchant.WithHostMerchant(ctx, merchantB) }, status: 403, code: "host_merchant_mismatch", asked: "a"},
		{name: "must match the pinned merchant", selector: "a", checker: membership{granted: map[string]billing.MerchantID{"a": merchantA}},
			ctx: func(ctx context.Context) context.Context { return merchant.WithID(ctx, merchantB) }, status: 403, code: "merchant_context_mismatch", asked: "a"},
		{name: "agreeing pins", selector: "a", checker: membership{granted: map[string]billing.MerchantID{"a": merchantA}},
			ctx: func(ctx context.Context) context.Context {
				return merchant.WithHostMerchant(merchant.WithID(ctx, merchantA), merchantA)
			}, status: 204, merchant: merchantA, asked: "a", slug: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := tc.checker
			a := &StandaloneAuth{Authenticator: userAuth(billingauth.UserContext{UserID: userA, Merchant: tc.tokenMerchant}, nil), AdminPermissionChecker: &checker}
			header := map[string]string{}
			if tc.selector != "" {
				header[merchant.SelectorHeader] = tc.selector
			}
			got := runStaff(t, a, staffperm.Admin, header, tc.ctx)
			require.Equal(t, tc.status, got.status, got.code)
			if tc.code != "" {
				require.Equal(t, tc.code, got.code)
			}
			require.Equal(t, []string{tc.asked}, checker.refs)
			if tc.status == 204 {
				require.Equal(t, tc.merchant, got.principal.MerchantID)
				require.Equal(t, userA, got.principal.ID)
				require.Equal(t, tc.slug, got.principal.MerchantSlug)
			}
		})
	}
}

// The in-process host principal is bounded by its grants and its merchant.
func TestHostAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		host   *requestauth.HostPrincipal
		status int
		code   string
	}{
		{name: "no host principal", status: 401, code: "authentication_required"},
		{name: "host principal without merchant", host: &requestauth.HostPrincipal{}, status: 401, code: "host_principal_invalid"},
		{name: "host principal", host: &requestauth.HostPrincipal{MerchantID: merchantA, Subject: "svc"}, status: 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runStaff(t, HostAuth{}, staffperm.Admin, nil, func(ctx context.Context) context.Context {
				if tc.host == nil {
					return ctx
				}
				return requestauth.WithHostPrincipal(ctx, tc.host)
			})
			require.Equal(t, tc.status, got.status, got.code)
			if tc.code != "" {
				require.Equal(t, tc.code, got.code)
			}
			if tc.status == 204 {
				require.Equal(t, merchantA, got.merchant)
				require.Equal(t, "svc", got.who.Subject)
				require.Equal(t, billingauth.SubjectApplication, got.who.SubjectKind)
			}
		})
	}
	got := runStaff(t, HostAuth{}, staffperm.Admin, nil, func(ctx context.Context) context.Context {
		ctx = merchanttarget.WithResolved(ctx, billingauth.Target{MerchantID: merchantB})
		return requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{MerchantID: merchantA})
	})
	require.Equal(t, http.StatusConflict, got.status, "a selector for another merchant never re-scopes the host")
}

type customerResolver struct {
	resolved *credential.ResolvedDelegated
	err      error
}

func (c customerResolver) ResolveResourceCustomer(*http.Request) (*credential.ResolvedDelegated, error) {
	return c.resolved, c.err
}

// A trusted issuer's openrails:self token is its user's own billing, at the
// merchant it resolves to; it holds no merchant permission.
func TestStandaloneCustomers(t *testing.T) {
	const resourceToken = "eyJ0eXAiOiJhdCtqd3QifQ.e30.sig"
	resolved := &credential.ResolvedDelegated{CustomerID: uuid.MustParse(userA), MerchantID: merchantA, MerchantSlug: "a", Issuer: "https://issuer.example", CredentialClass: billingauth.CredentialClassUserSession}
	run := func(a billingauth.Auth, authorization string) (int, billingauth.Identity, billingauth.Target) {
		var who billingauth.Identity
		var target billingauth.Target
		h := a.Required()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			who, _ = a.Identity(r.Context())
			target, _ = merchanttarget.FromContext(r.Context())
			w.WriteHeader(http.StatusNoContent)
		}))
		r := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, who, target
	}
	code, who, target := run(StandaloneCustomers{Resolver: customerResolver{resolved: resolved}}, "DPoP "+resourceToken)
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, userA, who.Subject)
	require.Equal(t, billingauth.SubjectUser, who.SubjectKind)
	require.True(t, billingauth.SelfActing(who))
	require.Equal(t, merchantA, target.MerchantID)

	for name, tc := range map[string]struct {
		auth          billingauth.Auth
		authorization string
		status        int
	}{
		"no resolver":         {StandaloneCustomers{}, "DPoP " + resourceToken, 503},
		"no token":            {StandaloneCustomers{Resolver: customerResolver{resolved: resolved}}, "", 401},
		"not an access token": {StandaloneCustomers{Resolver: customerResolver{resolved: resolved}}, "Bearer sk_1", 401},
		"refused token":       {StandaloneCustomers{Resolver: customerResolver{err: credential.ChallengeError{Code: billing.CodeSenderProofRequired, Err: credential.ErrResourceTokenInvalid}}}, "Bearer " + resourceToken, 401},
	} {
		code, _, _ := run(tc.auth, tc.authorization)
		require.Equal(t, tc.status, code, name)
	}
	refused := httptest.NewRecorder()
	StandaloneCustomers{}.RequirePermission(staffperm.Read)(http.NotFoundHandler()).ServeHTTP(refused, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusForbidden, refused.Code, "a customer token holds no merchant permission")
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}
