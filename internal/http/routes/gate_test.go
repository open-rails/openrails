package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	auth "github.com/open-rails/helpers/auth"

	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/credential"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

var (
	merchantA = merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	merchantB = merchant.ID(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
)

func userAuth(uc billingauth.UserContext, err error) billingauth.Authenticator {
	return billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) { return uc, err })
}

// membership grants the permission on the listed merchant slugs; a zero ID is
// a granted but unresolvable merchant.
type membership struct {
	granted  map[string]merchant.ID
	inferred string
	err      error
	refs     []string
}

func (m *membership) ResolveAuthorizedMerchant(_ context.Context, ref, _, _ string) (merchant.ID, string, error) {
	m.refs = append(m.refs, ref)
	if m.err != nil {
		return merchant.ID{}, "", m.err
	}
	if ref == "" {
		if ref = m.inferred; ref == "" {
			return merchant.ID{}, "", authpolicy.ErrMerchantUnresolved
		}
	}
	id, ok := m.granted[ref]
	if !ok {
		return merchant.ID{}, "", authpolicy.ErrPermissionRequired
	}
	return id, ref, nil
}

type credResolver struct {
	key, remote, jwt          *credential.ResolvedServiceCredential
	keyErr, remoteErr, jwtErr error
}

func (credResolver) LooksLikeAPIKey(token string) bool { return strings.HasPrefix(token, "sk_") }
func (c credResolver) ResolveAPIKey(context.Context, string) (*credential.ResolvedServiceCredential, error) {
	return c.key, c.keyErr
}
func (c credResolver) ResolveRemoteApplication(context.Context, string) (*credential.ResolvedServiceCredential, error) {
	if c.remote == nil && c.remoteErr == nil {
		return nil, credential.ErrNotRemoteApplicationToken
	}
	return c.remote, c.remoteErr
}
func (c credResolver) ResolveServiceJWT(context.Context, string) (*credential.ResolvedServiceCredential, error) {
	if c.jwt == nil && c.jwtErr == nil {
		return nil, errors.New("access_token_wrong_typ")
	}
	return c.jwt, c.jwtErr
}

type delegatedResolver struct {
	resolved *credential.ResolvedDelegated
	err      error
}

func (d delegatedResolver) ResolveDelegated(*http.Request) (*credential.ResolvedDelegated, error) {
	return d.resolved, d.err
}

func hostDelegated(p *billingauth.DelegatedPrincipal, err error) billingauth.DelegatedAuthenticator {
	return billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) { return p, err })
}

func service(perms ...string) *credential.ResolvedServiceCredential {
	return &credential.ResolvedServiceCredential{MerchantID: merchantA, Permissions: perms}
}

// Every credential kind reaches a merchant route only through its own verified
// merchant and an explicit permission; failures keep distinct, stable codes.
func TestGateAuthorizesEachCredentialKind(t *testing.T) {
	read := permissions.MerchantSettingsRead
	delegatedOK := delegatedResolver{resolved: &credential.ResolvedDelegated{MerchantID: merchantA, Merchant: "a", DelegatedSubject: userA, Permissions: []string{read}}}
	type want struct {
		status   int
		message  string
		merchant merchant.ID
		subject  string
		userID   string
		userSlug string
	}
	for _, tc := range []struct {
		name   string
		opts   GateOptions
		host   *requestauth.HostPrincipal
		header map[string]string
		want   want
	}{
		{name: "host principal without merchant", host: &requestauth.HostPrincipal{Permissions: []string{"merchant:*"}}, want: want{status: 401, message: "host_principal_invalid"}},
		{name: "host principal lacking permission", host: &requestauth.HostPrincipal{MerchantID: merchantA, Permissions: []string{permissions.MerchantCatalogRead}}, want: want{status: 403, message: "permission_required"}},
		{name: "host principal wins over any header", host: &requestauth.HostPrincipal{MerchantID: merchantA, Subject: "svc", Permissions: []string{"merchant:*"}},
			opts: GateOptions{ServiceCredentialResolver: credResolver{keyErr: errors.New("never consulted")}}, header: map[string]string{"Authorization": "Bearer sk_1"}, want: want{merchant: merchantA, subject: "svc"}},

		{name: "api key", opts: GateOptions{ServiceCredentialResolver: credResolver{key: service(read)}}, header: bearer("sk_1"), want: want{merchant: merchantA}},
		{name: "api key glob", opts: GateOptions{ServiceCredentialResolver: credResolver{key: service("merchant:*")}}, header: bearer("sk_1"), want: want{merchant: merchantA}},
		{name: "api key lacking permission", opts: GateOptions{ServiceCredentialResolver: credResolver{key: service(permissions.CustomerAll)}}, header: bearer("sk_1"), want: want{status: 403, message: "permission_required"}},
		{name: "api key resolved to nothing", opts: GateOptions{ServiceCredentialResolver: credResolver{}}, header: bearer("sk_1"), want: want{status: 401, message: "service_credential_invalid"}},
		{name: "api key scope denied", opts: GateOptions{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialScopeDenied}}, header: bearer("sk_1"), want: want{status: 403, message: "service_credential_resource_scope_denied"}},
		{name: "api key merchant unresolved", opts: GateOptions{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialMerchantUnresolved}}, header: bearer("sk_1"), want: want{status: 403, message: "service_credential_merchant_unresolved"}},
		{name: "api key for another host", opts: GateOptions{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialHostMismatch}}, header: bearer("sk_1"), want: want{status: 403, message: "host_merchant_mismatch"}},
		{name: "api key invalid", opts: GateOptions{ServiceCredentialResolver: credResolver{keyErr: errors.New("bad key")}}, header: bearer("sk_1"), want: want{status: 401, message: "service_credential_invalid"}},

		{name: "remote application", opts: GateOptions{ServiceCredentialResolver: credResolver{remote: service(read)}}, header: bearer("a.b.c"), want: want{merchant: merchantA}},
		{name: "rejected remote application without user fallback", opts: GateOptions{ServiceCredentialResolver: credResolver{remoteErr: credential.ErrDelegatedInvalid}}, header: bearer("a.b.c"), want: want{status: 401, message: "service_credential_invalid"}},
		{name: "rejected remote application falls through to user session", opts: GateOptions{ServiceCredentialResolver: credResolver{remoteErr: credential.ErrDelegatedInvalid}, Authenticator: userAuth(billingauth.UserContext{}, billingauth.ErrUnauthenticated)}, header: bearer("a.b.c"), want: want{status: 401, message: "authentication required"}},
		{name: "verified service jwt scope denial never falls through", opts: GateOptions{ServiceCredentialResolver: credResolver{jwtErr: credential.ErrServiceCredentialScopeDenied}, DelegatedResolver: delegatedOK}, header: bearer("a.b.c"), want: want{status: 403, message: "service_credential_resource_scope_denied"}},
		{name: "service jwt from another host's issuer", opts: GateOptions{ServiceCredentialResolver: credResolver{jwtErr: credential.ErrDelegatedIssuerUnknown}, DelegatedResolver: delegatedOK}, header: bearer("a.b.c"), want: want{status: 403, message: "host_merchant_mismatch"}},
		{name: "wrong-typ service jwt reaches delegated resolver", opts: GateOptions{ServiceCredentialResolver: credResolver{}, DelegatedResolver: delegatedOK}, header: bearer("a.b.c"), want: want{merchant: merchantA, subject: userA, userID: userA, userSlug: "a"}},

		{name: "delegated token", opts: GateOptions{DelegatedResolver: delegatedOK}, header: map[string]string{"Authorization": "DPoP a.b.c"}, want: want{merchant: merchantA, subject: userA, userID: userA, userSlug: "a"}},
		{name: "delegated token lacking permission", opts: GateOptions{DelegatedResolver: delegatedResolver{resolved: &credential.ResolvedDelegated{MerchantID: merchantA, DelegatedSubject: userA, Permissions: []string{permissions.CustomerSpendDelegationsRead}}}}, header: bearer("a.b.c"), want: want{status: 403, message: "permission_required"}},
		{name: "delegated verification unavailable", opts: GateOptions{DelegatedResolver: delegatedResolver{err: credential.ErrDelegatedUnavailable}}, header: bearer("a.b.c"), want: want{status: 503, message: "delegated_verification_unavailable"}},
		{name: "delegated token missing sender proof", opts: GateOptions{DelegatedResolver: delegatedResolver{err: auth.ErrSenderProofRequired}}, header: bearer("a.b.c"), want: want{status: 401, message: "sender_proof_required"}},
		{name: "invalid delegated token", opts: GateOptions{DelegatedResolver: delegatedResolver{err: credential.ErrDelegatedInvalid}}, header: bearer("a.b.c"), want: want{status: 401, message: "delegated_token_invalid"}},
		{name: "opaque token skips delegated resolver", opts: GateOptions{DelegatedResolver: delegatedOK}, header: bearer("opaque"), want: want{status: 401, message: "bearer principal required"}},

		{name: "host delegated principal", opts: GateOptions{DelegatedAuthenticator: hostDelegated(&billingauth.DelegatedPrincipal{MerchantID: merchantA.String(), SubjectID: userA, Permissions: []string{read}}, nil)}, want: want{merchant: merchantA, subject: userA, userID: userA}},
		{name: "host delegated principal lacking permission", opts: GateOptions{DelegatedAuthenticator: hostDelegated(&billingauth.DelegatedPrincipal{MerchantID: merchantA.String(), SubjectID: userA, Permissions: []string{permissions.CustomerSpendDelegationsRead}}, nil)}, want: want{status: 403, message: "permission_required"}},
		{name: "host delegated principal with opaque subject", opts: GateOptions{DelegatedAuthenticator: hostDelegated(&billingauth.DelegatedPrincipal{MerchantID: merchantA.String(), SubjectID: "user-1", Permissions: []string{read}}, nil)}, want: want{status: 401, message: "delegated_principal_invalid"}},
		{name: "host delegated rejection", opts: GateOptions{DelegatedAuthenticator: hostDelegated(nil, billingauth.ErrUnauthenticated)}, want: want{status: 401, message: "authentication required"}},

		{name: "no credential path", want: want{status: 401, message: "bearer principal required"}},
		{name: "user with opaque subject", opts: GateOptions{Authenticator: userAuth(billingauth.UserContext{UserID: "user-1"}, nil), AdminPermissionChecker: &membership{}}, want: want{status: 401}},
		{name: "user without membership checker", opts: GateOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil)}, want: want{status: 500, message: "authorization unavailable"}},
		{name: "membership lookup failure", opts: GateOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), AdminPermissionChecker: &membership{err: errors.New("db")}}, want: want{status: 500, message: "failed to check permission"}},
		{name: "ambiguous membership", opts: GateOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), AdminPermissionChecker: &membership{err: credential.ErrMerchantAmbiguous}}, want: want{status: 403, message: "merchant_unresolved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/merchant/settings", nil)
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			ctx := r.Context()
			if tc.host != nil {
				ctx = requestauth.WithHostPrincipal(ctx, tc.host)
			}
			principal, err := NewGate(tc.opts).Authorize(ctx, r, read)
			assertGate(t, principal, err, tc.want.status, tc.want.message)
			if tc.want.status == 0 {
				require.Equal(t, tc.want.merchant, principal.MerchantID)
				require.Equal(t, tc.want.subject, principal.Subject)
				require.Equal(t, tc.want.userID, principal.UserContext.UserID)
				require.Equal(t, tc.want.userSlug, principal.UserContext.Merchant)
			}
		})
	}
}

// A user session's merchant comes from its token or an explicit selector, is
// authorized by live membership, and must agree with any Host or context pin.
func TestGateUserSessionMerchantSelection(t *testing.T) {
	for _, tc := range []struct {
		name, tokenMerchant, selector string
		checker                       membership
		ctx                           func(context.Context) context.Context
		status                        int
		message                       string
		merchant                      merchant.ID
		asked, slug                   string
	}{
		{name: "explicit selector", selector: " b ", checker: membership{granted: map[string]merchant.ID{"b": merchantB}}, merchant: merchantB, asked: "b", slug: "b"},
		{name: "single membership inferred", checker: membership{granted: map[string]merchant.ID{"a": merchantA}, inferred: "a"}, merchant: merchantA, asked: "", slug: "a"},
		{name: "no selector and no single membership", checker: membership{}, status: 403, message: "merchant_unresolved", asked: ""},
		{name: "selected merchant without live permission", selector: "b", checker: membership{granted: map[string]merchant.ID{"a": merchantA}}, status: 403, message: "permission_required", asked: "b"},
		{name: "granted merchant that does not resolve", selector: "b", checker: membership{granted: map[string]merchant.ID{"b": {}}}, status: 403, message: "merchant_unresolved", asked: "b"},
		{name: "token merchant cannot be overridden", tokenMerchant: "a", selector: "b", checker: membership{granted: map[string]merchant.ID{"a": merchantA, "b": merchantB}}, merchant: merchantA, asked: "a", slug: "a"},
		{name: "must match the Host merchant", selector: "a", checker: membership{granted: map[string]merchant.ID{"a": merchantA}},
			ctx: func(ctx context.Context) context.Context { return merchant.WithHostMerchant(ctx, merchantB) }, status: 403, message: "host_merchant_mismatch", asked: "a"},
		{name: "must match the pinned merchant", selector: "a", checker: membership{granted: map[string]merchant.ID{"a": merchantA}},
			ctx: func(ctx context.Context) context.Context { return merchant.WithID(ctx, merchantB) }, status: 403, message: "merchant_context_mismatch", asked: "a"},
		{name: "agreeing pins", selector: "a", checker: membership{granted: map[string]merchant.ID{"a": merchantA}},
			ctx: func(ctx context.Context) context.Context {
				return merchant.WithHostMerchant(merchant.WithID(ctx, merchantA), merchantA)
			}, merchant: merchantA, asked: "a", slug: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := tc.checker
			gate := NewGate(GateOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA, Merchant: tc.tokenMerchant}, nil), AdminPermissionChecker: &checker})
			r := httptest.NewRequest(http.MethodGet, "/v1/merchant/settings", nil)
			if tc.selector != "" {
				r.Header.Set(billingauth.MerchantSelectorHeader, tc.selector)
			}
			ctx := r.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}
			principal, err := gate.Authorize(ctx, r, permissions.MerchantSettingsRead)
			assertGate(t, principal, err, tc.status, tc.message)
			require.Equal(t, []string{tc.asked}, checker.refs)
			if tc.status == 0 {
				require.Equal(t, tc.merchant, principal.MerchantID)
				require.Equal(t, userA, principal.Subject)
				require.Equal(t, tc.slug, principal.UserContext.Merchant)
			}
		})
	}

	checker := &membership{granted: map[string]merchant.ID{"a": merchantA}, inferred: "a"}
	principal, err := NewGate(GateOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), AdminPermissionChecker: checker}).Authorize(t.Context(), nil, permissions.MerchantSettingsRead)
	require.NoError(t, err, "a nil request still infers from membership")
	require.Equal(t, merchantA, principal.MerchantID)
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func assertGate(t *testing.T, principal billingauth.Principal, err error, status int, message string) {
	t.Helper()
	if status == 0 {
		require.NoError(t, err)
		return
	}
	var gateErr billingauth.GateError
	require.ErrorAs(t, err, &gateErr)
	require.Equal(t, status, gateErr.Status, gateErr.Message)
	if message != "" {
		require.Equal(t, message, gateErr.Message)
	}
	require.Zero(t, principal)
}

type gateFunc func(context.Context, *http.Request, string) (billingauth.Principal, error)

func (f gateFunc) Authorize(ctx context.Context, r *http.Request, perm string) (billingauth.Principal, error) {
	return f(ctx, r, perm)
}

// The permission middleware turns the gate's answer into the request's only
// merchant and principal, and refuses before the handler on any failure.
func TestMerchantPermissionMiddleware(t *testing.T) {
	type seen struct {
		merchant  merchant.ID
		user      string
		principal any
	}
	run := func(gate billingauth.Gate, header map[string]string, ctx context.Context) (*httptest.ResponseRecorder, *seen) {
		var got *seen
		mw := Options{Gate: gate}.RequireMerchantPermission(permissions.MerchantPaymentsRead)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/merchant/payments", nil).WithContext(ctx)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		mw(func(req *httprequest.Request) {
			got = &seen{}
			got.merchant, _ = merchant.FromContext(req.Request.Context())
			if uc, ok := req.UserContext(); ok {
				got.user = uc.UserID
			}
			got.principal, _ = req.Get(httphandlers.MerchantRoutePrincipalContextKey)
			req.Status(http.StatusNoContent)
		})(httprequest.NewHTTP(rec, r, nil))
		return rec, got
	}
	allow := billingauth.Principal{MerchantID: merchantA, Subject: userA, UserContext: billingauth.UserContext{UserID: userA}}
	allowGate := gateFunc(func(_ context.Context, _ *http.Request, perm string) (billingauth.Principal, error) {
		require.Equal(t, permissions.MerchantPaymentsRead, perm)
		return allow, nil
	})

	rec, got := run(allowGate, nil, t.Context())
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, merchantA, got.merchant)
	require.Equal(t, userA, got.user)
	require.Equal(t, allow, got.principal)

	for _, tc := range []struct {
		name   string
		gate   billingauth.Gate
		header map[string]string
		ctx    context.Context
		status int
	}{
		{"no gate", nil, nil, t.Context(), 500},
		{"gate refusal", gateFunc(func(context.Context, *http.Request, string) (billingauth.Principal, error) {
			return billingauth.Principal{}, billingauth.GateError{Status: 403, Message: "permission_required"}
		}), nil, t.Context(), 403},
		{"gate failure is not a refusal", gateFunc(func(context.Context, *http.Request, string) (billingauth.Principal, error) {
			return billingauth.Principal{}, errors.New("db down")
		}), nil, t.Context(), 500},
		{"binding header for another merchant", allowGate, map[string]string{merchant.BindingHeader: merchantB.String()}, t.Context(), 409},
		{"malformed binding header", allowGate, map[string]string{merchant.BindingHeader: "nope"}, t.Context(), 400},
		{"slug selector without a resolved target", allowGate, map[string]string{merchant.SlugHeader: "a"}, t.Context(), 409},
		{"configured merchant differs", allowGate, nil, merchant.WithID(t.Context(), merchantB), 409},
	} {
		rec, got := run(tc.gate, tc.header, tc.ctx)
		require.Equal(t, tc.status, rec.Code, tc.name)
		require.Nil(t, got, "%s reached the handler", tc.name)
	}

	rec, _ = run(gateFunc(func(context.Context, *http.Request, string) (billingauth.Principal, error) {
		return billingauth.Principal{}, billingauth.GateError{Status: 401, Message: "sender_proof_required"}
	}), nil, t.Context())
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Header().Get("WWW-Authenticate"), "DPoP")
}

// Required auth pins a UUID subject for the handler (surviving request
// reassignment); optional auth never refuses.
func TestUserAuthMiddleware(t *testing.T) {
	for _, tc := range []struct {
		name           string
		authn          billingauth.Authenticator
		seenAs         string
		requiredStatus int
	}{
		{"valid", userAuth(billingauth.UserContext{UserID: userA}, nil), userA, http.StatusNoContent},
		{"rejected", userAuth(billingauth.UserContext{}, billingauth.ErrUnauthenticated), "", http.StatusUnauthorized},
		{"opaque subject", userAuth(billingauth.UserContext{UserID: "42"}, nil), "", http.StatusUnauthorized},
		{"auth disabled", nil, "", http.StatusInternalServerError},
	} {
		opts := Options{Authenticator: tc.authn}
		for _, mw := range []struct {
			name   string
			mw     router.Middleware
			status int
		}{
			{"required", opts.requiredMW(), tc.requiredStatus},
			{"optional", opts.optionalMW(), http.StatusNoContent},
		} {
			user := "unset"
			rec := httptest.NewRecorder()
			mw.mw(func(r *httprequest.Request) {
				r.Request = r.Request.WithContext(context.Background())
				uc, _ := r.UserContext()
				user = uc.UserID
				r.Status(http.StatusNoContent)
			})(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me/balance", nil), nil))
			require.Equal(t, mw.status, rec.Code, "%s/%s", tc.name, mw.name)
			if rec.Code == http.StatusNoContent {
				require.Equal(t, tc.seenAs, user, "%s/%s", tc.name, mw.name)
			}
		}
	}
}
