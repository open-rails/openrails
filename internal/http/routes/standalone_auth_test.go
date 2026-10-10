package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/credential"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
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

// groups are the merchants' permission groups.
var groups = map[billing.MerchantID]string{merchantA: "group_1", merchantB: "group_2"}

// directory resolves merchants by name, and a user's only one.
type directory struct {
	named    map[string]billing.MerchantID
	inferred string
	err      error
	refs     []string
}

func (d *directory) UserMerchant(_ context.Context, _ string, ref string) (billing.MerchantID, string, error) {
	d.refs = append(d.refs, ref)
	if d.err != nil {
		return billing.MerchantID{}, "", d.err
	}
	if ref == "" {
		if ref = d.inferred; ref == "" {
			return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
		}
	}
	id, ok := d.named[ref]
	if !ok || id.IsZero() {
		return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
	}
	return id, ref, nil
}

func (d *directory) MerchantGroup(_ context.Context, mid billing.MerchantID) (string, error) {
	if g, ok := groups[mid]; ok {
		return g, nil
	}
	return "", billing.ErrMerchantUnresolved
}

// sessions is the control plane's users: who, holding granted in each group.
type sessions struct {
	who     billingauth.Identity
	err     error
	granted map[string][]string
}

func (s sessions) Authenticate(*http.Request) (billingauth.Verified, error) {
	if s.err != nil {
		return nil, s.err
	}
	return session(s), nil
}

type session sessions

func (v session) Identity() billingauth.Identity { return v.who }

func (v session) Can(_ context.Context, scope billingauth.Scope, perm string) (bool, error) {
	if scope.Authority != "cp" {
		return false, nil
	}
	for _, p := range v.granted[scope.ID] {
		if p == perm {
			return true, nil
		}
	}
	return false, nil
}

func (session) CheckRecentSignIn(context.Context) error { return nil }

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

type tokenResolver struct {
	token *credential.ResolvedResourceAccess
	err   error
}

func (r tokenResolver) ResolveResourceToken(*http.Request) (*credential.ResolvedResourceAccess, error) {
	return r.token, r.err
}

// served is what the gate answered, and what the handler behind it saw.
type served struct {
	status   int
	code     string
	staff    billingauth.Staff
	merchant billing.MerchantID
}

// runStaff serves key behind the standalone server's gate.
func runStaff(t *testing.T, a *StandaloneAuth, key string, header map[string]string, ctx func(context.Context) context.Context) served {
	t.Helper()
	method, path, _ := strings.Cut(key, " ")
	route, ok := Lookup(method, path)
	require.True(t, ok, key)
	rt := &app.Runtime{Config: &config.Config{}}
	env := newEnv(rt, Options{Auth: a, Scope: a.Scope, ResolveMerchant: a.ResolveMerchant, Permissions: Permissions{AdminRead: staffperm.Read, AdminUpdate: staffperm.Write, MerchantConfig: staffperm.Admin}})
	table := &router.Table{}
	var out served
	router.NewMux(table, "", rt).Handle(route.Method, route.Path, func(r *httprequest.Request) {
		out.staff, _ = r.Staff()
		out.merchant, _ = merchant.FromContext(r.Request.Context())
		r.NoContent()
	}, env.gates(route)...)
	r := httptest.NewRequest(route.Method, filled(route.Path), strings.NewReader("{}"))
	for k, v := range header {
		r.Header.Set(k, v)
	}
	if ctx != nil {
		r = r.WithContext(ctx(r.Context()))
	}
	rec := httptest.NewRecorder()
	table.Handler().ServeHTTP(rec, r)
	out.status = rec.Code
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	out.code = body.Error.Code
	return out
}

const configuration = "GET /v1/admin/configuration"

// Every standalone credential kind reaches a merchant route only through its
// own verified merchant and an explicit permission there; failures keep
// distinct, stable codes.
func TestStandaloneAuthEachCredentialKind(t *testing.T) {
	admin := staffperm.Admin
	user := authtest.User(userA)
	user.Issuer, user.Invoker.Issuer = "cp", "cp"
	member := sessions{who: user, granted: map[string][]string{"group_1": {admin}}}
	dir := func() *directory {
		return &directory{named: map[string]billing.MerchantID{"a": merchantA}, inferred: "a"}
	}
	token := &credential.ResolvedResourceAccess{Issuer: "https://issuer.example", Subject: userB, MerchantID: merchantA, Permissions: []string{admin}}
	for _, tc := range []struct {
		name    string
		auth    *StandaloneAuth
		header  map[string]string
		status  int
		code    string
		subject string
		kind    billingauth.SubjectKind
	}{
		{name: "api key", auth: &StandaloneAuth{Issuer: "cp", ServiceCredentialResolver: credResolver{key: serviceCredential(admin)}, Directory: dir()}, header: bearer("sk_1"), status: 204, subject: "group_1", kind: billingauth.SubjectApplication},
		{name: "api key glob", auth: &StandaloneAuth{Issuer: "cp", ServiceCredentialResolver: credResolver{key: serviceCredential("merchant:*")}, Directory: dir()}, header: bearer("sk_1"), status: 204, subject: "group_1", kind: billingauth.SubjectApplication},
		{name: "api key lacking permission", auth: &StandaloneAuth{Issuer: "cp", ServiceCredentialResolver: credResolver{key: serviceCredential("root:*")}, Directory: dir()}, header: bearer("sk_1"), status: 403, code: "permission_required"},
		{name: "api key resolved to nothing", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{}}, header: bearer("sk_1"), status: 401, code: "service_credential_invalid"},
		{name: "api key without an id", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{key: &credential.ResolvedServiceCredential{MerchantID: merchantA, Permissions: []string{admin}}}}, header: bearer("sk_1"), status: 401, code: "service_credential_invalid"},
		{name: "api key scope denied", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialScopeDenied}}, header: bearer("sk_1"), status: 403, code: "service_credential_resource_scope_denied"},
		{name: "api key merchant unresolved", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialMerchantUnresolved}}, header: bearer("sk_1"), status: 403, code: "service_credential_merchant_unresolved"},
		{name: "api key for another host", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: credential.ErrServiceCredentialHostMismatch}}, header: bearer("sk_1"), status: 403, code: "host_merchant_mismatch"},
		{name: "api key invalid", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{keyErr: errors.New("bad key")}}, header: bearer("sk_1"), status: 401, code: "service_credential_invalid"},
		{name: "access token", auth: &StandaloneAuth{Issuer: "cp", ResourceTokenResolver: tokenResolver{token: token}, Directory: dir()}, header: bearer("eyJ0eXAiOiJhdCtqd3QifQ.e30.sig"), status: 204, subject: userB, kind: billingauth.SubjectUser},
		{name: "access token refused", auth: &StandaloneAuth{Issuer: "cp", ResourceTokenResolver: tokenResolver{err: credential.ErrResourceTokenIssuerUnknown}, Directory: dir()}, header: bearer("eyJ0eXAiOiJhdCtqd3QifQ.e30.sig"), status: 401, code: "access_token_issuer_unknown"},
		{name: "a JWT that is no access token is a user session", auth: &StandaloneAuth{ServiceCredentialResolver: credResolver{key: serviceCredential(admin)}, Sessions: sessions{err: auth.ErrUnauthenticated}}, header: bearer("a.b.c"), status: 401, code: "authentication_required"},
		{name: "no credential path", auth: &StandaloneAuth{}, status: 401, code: "authentication_required"},
		{name: "an application's session", auth: &StandaloneAuth{Issuer: "cp", Sessions: sessions{who: authtest.Application("app_1")}, Directory: dir()}, status: 401, code: "authentication_required"},
		{name: "user without a directory", auth: &StandaloneAuth{Issuer: "cp", Sessions: member}, status: 403, code: "merchant_unresolved"},
		{name: "membership lookup failure", auth: &StandaloneAuth{Issuer: "cp", Sessions: member, Directory: &directory{err: errors.New("db")}}, status: 503, code: "authorization_unavailable"},
		{name: "revoked session", auth: &StandaloneAuth{Issuer: "cp", Sessions: sessions{err: errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)}, Directory: dir()}, status: 401, code: "credential_revoked"},
		{name: "ambiguous membership", auth: &StandaloneAuth{Issuer: "cp", Sessions: member, Directory: &directory{err: credential.ErrMerchantAmbiguous}}, status: 403, code: "merchant_unresolved"},
		{name: "user without the permission", auth: &StandaloneAuth{Issuer: "cp", Sessions: sessions{who: user}, Directory: dir()}, status: 403, code: "permission_required"},
		{name: "user session", auth: &StandaloneAuth{Issuer: "cp", Sessions: member, Directory: dir()}, status: 204, subject: userA, kind: billingauth.SubjectUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runStaff(t, tc.auth, configuration, tc.header, nil)
			require.Equal(t, tc.status, got.status, got.code)
			if tc.code != "" {
				require.Equal(t, tc.code, got.code)
			}
			if tc.status == 204 {
				require.Equal(t, tc.subject, got.staff.Subject)
				require.Equal(t, tc.kind, got.staff.SubjectKind)
				require.True(t, billingauth.SelfActing(got.staff.Identity), "a standalone subject acts itself")
				require.Equal(t, merchantA, got.staff.Merchant)
				require.Equal(t, merchantA, got.merchant, "the gate binds the credential's merchant")
			}
		})
	}
}

// A key or token holds exactly its permissions, in exactly its merchant's
// scope, and proves no sign-in a key does not have.
func TestStandaloneCredentialsHoldExactly(t *testing.T) {
	a := &StandaloneAuth{Issuer: "cp", ServiceCredentialResolver: credResolver{key: serviceCredential("merchant:*", staffperm.Read)}, Directory: &directory{}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer sk_1")
	v, err := a.Authenticate(r)
	require.NoError(t, err)
	own := billingauth.Scope{Authority: "cp", ID: "group_1"}
	for _, tc := range []struct {
		scope billingauth.Scope
		perm  string
		want  bool
	}{
		{own, staffperm.Read, true},
		{own, "", false},
		{own, "*", false},
		{own, "merchant:*", false},
		{billingauth.Scope{Authority: "cp", ID: "group_2"}, staffperm.Read, false},
		{billingauth.Scope{Authority: "other", ID: "group_1"}, staffperm.Read, false},
		{billingauth.Scope{}, staffperm.Read, false},
	} {
		ok, err := v.(auth.PermissionChecker).Can(t.Context(), tc.scope, tc.perm)
		require.NoError(t, err)
		require.Equal(t, tc.want, ok, "%+v %q", tc.scope, tc.perm)
	}
	require.ErrorIs(t, v.(auth.RecentSignInChecker).CheckRecentSignIn(t.Context()), auth.ErrForbidden, "a key has no sign-in")

	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	signedIn := func(at time.Time, machine bool) error {
		token := &credential.ResolvedResourceAccess{Issuer: "https://issuer.example", Subject: userB, ClientID: "client", Machine: machine, MerchantID: merchantA, Permissions: []string{staffperm.Write}, AuthTime: at}
		a := &StandaloneAuth{Issuer: "cp", ResourceTokenResolver: tokenResolver{token: token}, Directory: &directory{}, Now: func() time.Time { return now }}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer eyJ0eXAiOiJhdCtqd3QifQ.e30.sig")
		v, err := a.Authenticate(r)
		require.NoError(t, err)
		ok, err := v.(auth.PermissionChecker).Can(t.Context(), own, staffperm.Write)
		require.NoError(t, err)
		require.True(t, ok)
		return v.(auth.RecentSignInChecker).CheckRecentSignIn(t.Context())
	}
	require.NoError(t, signedIn(now.Add(-time.Minute), false))
	stale := signedIn(now.Add(-time.Hour), false)
	require.ErrorIs(t, stale, auth.ErrStepUpRequired)
	var challenge *auth.Challenge
	require.ErrorAs(t, stale, &challenge)
	require.Equal(t, credential.FederatedSignInWindow, challenge.MaxAge)
	require.Equal(t, 0, challenge.Metadata["max_age"], "the client re-authorizes at its issuer")
	require.ErrorIs(t, signedIn(time.Time{}, false), auth.ErrStepUpRequired, "a token that does not say is stale")
	require.ErrorIs(t, signedIn(now, true), auth.ErrForbidden, "a client acting for itself has no sign-in")
}

// A user session's merchant comes from an explicit selector or their only
// membership, is authorized by their permission there, and must agree with
// any Host or context pin.
func TestStandaloneUserSessionMerchantSelection(t *testing.T) {
	user := authtest.User(userA)
	user.Issuer, user.Invoker.Issuer = "cp", "cp"
	for _, tc := range []struct {
		name, selector string
		dir            directory
		granted        map[string][]string
		ctx            func(context.Context) context.Context
		status         int
		code           string
		merchant       billing.MerchantID
		asked          string
	}{
		{name: "explicit selector", selector: " b ", dir: directory{named: map[string]billing.MerchantID{"b": merchantB}}, granted: map[string][]string{"group_2": {staffperm.Admin}}, status: 204, merchant: merchantB, asked: "b"},
		{name: "single membership inferred", dir: directory{named: map[string]billing.MerchantID{"a": merchantA}, inferred: "a"}, granted: map[string][]string{"group_1": {staffperm.Admin}}, status: 204, merchant: merchantA, asked: ""},
		{name: "no selector and no single membership", status: 403, code: "merchant_unresolved", asked: ""},
		{name: "selected merchant without live permission", selector: "b", dir: directory{named: map[string]billing.MerchantID{"b": merchantB}}, granted: map[string][]string{"group_1": {staffperm.Admin}}, status: 403, code: "permission_required", asked: "b"},
		{name: "selected merchant that does not resolve", selector: "b", dir: directory{named: map[string]billing.MerchantID{"b": {}}}, status: 403, code: "merchant_unresolved", asked: "b"},
		{name: "must match the Host merchant", selector: "a", dir: directory{named: map[string]billing.MerchantID{"a": merchantA}}, granted: map[string][]string{"group_1": {staffperm.Admin}},
			ctx: func(ctx context.Context) context.Context { return merchant.WithHostMerchant(ctx, merchantB) }, status: 403, code: "host_merchant_mismatch", asked: "a"},
		{name: "must match the pinned merchant", selector: "a", dir: directory{named: map[string]billing.MerchantID{"a": merchantA}}, granted: map[string][]string{"group_1": {staffperm.Admin}},
			ctx: func(ctx context.Context) context.Context { return merchant.WithID(ctx, merchantB) }, status: 403, code: "merchant_context_mismatch", asked: "a"},
		{name: "agreeing pins", selector: "a", dir: directory{named: map[string]billing.MerchantID{"a": merchantA}}, granted: map[string][]string{"group_1": {staffperm.Admin}},
			ctx: func(ctx context.Context) context.Context {
				return merchant.WithHostMerchant(merchant.WithID(ctx, merchantA), merchantA)
			}, status: 204, merchant: merchantA, asked: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir
			a := &StandaloneAuth{Issuer: "cp", Sessions: sessions{who: user, granted: tc.granted}, Directory: &dir}
			header := map[string]string{}
			if tc.selector != "" {
				header[merchant.SelectorHeader] = tc.selector
			}
			got := runStaff(t, a, configuration, header, tc.ctx)
			require.Equal(t, tc.status, got.status, got.code)
			if tc.code != "" {
				require.Equal(t, tc.code, got.code)
			}
			require.Equal(t, []string{tc.asked}, dir.refs)
			if tc.status == 204 {
				require.Equal(t, tc.merchant, got.staff.Merchant)
				require.Equal(t, userA, got.staff.Subject)
			}
		})
	}
}

// The in-process host principal holds every permission at its own merchant
// only.
func TestHostAuth(t *testing.T) {
	serve := func(ctx func(context.Context) context.Context) served {
		t.Helper()
		route, _ := Lookup(GET, "/v1/admin/configuration")
		rt := &app.Runtime{Config: &config.Config{}}
		env := newEnv(rt, HostOptions())
		table := &router.Table{}
		var out served
		router.NewMux(table, "", rt).Handle(route.Method, route.Path, func(r *httprequest.Request) {
			out.staff, _ = r.Staff()
			out.merchant, _ = merchant.FromContext(r.Request.Context())
			r.NoContent()
		}, env.gates(route)...)
		r := httptest.NewRequest(route.Method, route.Path, nil)
		r = r.WithContext(ctx(r.Context()))
		rec := httptest.NewRecorder()
		table.Handler().ServeHTTP(rec, r)
		out.status = rec.Code
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		out.code = body.Error.Code
		return out
	}
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
			got := serve(func(ctx context.Context) context.Context {
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
				require.Equal(t, "svc", got.staff.Subject)
				require.Equal(t, billingauth.SubjectApplication, got.staff.SubjectKind)
			}
		})
	}
	got := serve(func(ctx context.Context) context.Context {
		ctx = merchanttarget.WithResolved(ctx, billingauth.Target{MerchantID: merchantB})
		return requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{MerchantID: merchantA})
	})
	require.Equal(t, http.StatusConflict, got.status, "a selector for another merchant never re-scopes the host")

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(requestauth.WithHostPrincipal(r.Context(), &requestauth.HostPrincipal{MerchantID: merchantA}))
	v, err := HostAuth{}.Authenticate(r)
	require.NoError(t, err)
	can := v.(auth.PermissionChecker)
	ok, _ := can.Can(t.Context(), billingauth.HostScope(merchantA), "host")
	require.True(t, ok)
	ok, _ = can.Can(t.Context(), billingauth.HostScope(merchantB), "host")
	require.False(t, ok, "only at its own merchant")
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
	run := func(a billingauth.Authenticator, authorization string) (int, customerscopeView) {
		rt := &app.Runtime{Config: &config.Config{}}
		var seen customerscopeView
		route, _ := Lookup(GET, "/v1/me")
		env := customerEnv(rt, CustomerMount{Auth: a, ResolveMerchant: CredentialOnly})
		h := &router.Table{}
		router.NewMux(h, "", rt).Handle(GET, "/v1/me", func(r *httprequest.Request) {
			scope, _ := r.CustomerScope()
			seen = customerscopeView{customer: scope.Customer().String(), merchant: scope.Merchant()}
			r.NoContent()
		}, env.gates(route)...)
		req := httptest.NewRequest(GET, "/v1/me", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		return rec.Code, seen
	}
	code, seen := run(StandaloneCustomers{Resolver: customerResolver{resolved: resolved}}, "DPoP "+resourceToken)
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, customerscopeView{customer: userA, merchant: merchantA}, seen)

	for name, tc := range map[string]struct {
		auth          billingauth.Authenticator
		authorization string
		status        int
	}{
		"no resolver":         {StandaloneCustomers{}, "DPoP " + resourceToken, 503},
		"no token":            {StandaloneCustomers{Resolver: customerResolver{resolved: resolved}}, "", 401},
		"not an access token": {StandaloneCustomers{Resolver: customerResolver{resolved: resolved}}, "Bearer sk_1", 401},
		"refused token":       {StandaloneCustomers{Resolver: customerResolver{err: credential.ChallengeError{Code: billing.CodeSenderProofRequired, Err: credential.ErrResourceTokenInvalid}}}, "Bearer " + resourceToken, 401},
	} {
		code, _ := run(tc.auth, tc.authorization)
		require.Equal(t, tc.status, code, name)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "DPoP "+resourceToken)
	v, err := StandaloneCustomers{Resolver: customerResolver{resolved: resolved}}.Authenticate(r)
	require.NoError(t, err)
	_, holds := v.(auth.PermissionChecker)
	require.False(t, holds, "a customer token holds no merchant permission")
}

type customerscopeView struct {
	customer string
	merchant billing.MerchantID
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}
