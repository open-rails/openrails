package routes

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	auth "github.com/open-rails/authkit/auth"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/customerscope"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchant"
)

// openTiers are the tiers no Auth runs on: their credential is the request
// itself (a provider signature) or there is none.
var openTiers = []Tier{AuthPublic, AuthProvider}

// openRoutes is every route no Auth gates. A route that joins it is a
// reviewed change.
var openRoutes = []string{
	"GET /health/live", "GET /health/ready", "GET /v1/config",
	"GET /v1/orders/{id}/solana-pay", "POST /v1/orders/{id}/solana-pay",
	"POST /v1/webhooks/{rail}/{account_id}",
}

// Every catalog route declares exactly one tier of the closed set, and what
// that tier checks: a staff route its group's permission, a customer route
// none, and no customer path names a customer.
func TestEveryRouteDeclaresOneTier(t *testing.T) {
	tiers := []Tier{AuthPublic, AuthCustomer, AuthMerchant, AuthSignedIn, AuthApplication, AuthProvider, AuthProvisioning, AuthNetwork}
	var open []string
	names := map[string]string{}
	for _, r := range Catalog() {
		key := r.Key()
		require.Contains(t, tiers, r.Auth, "%s declares no known tier", key)
		if slices.Contains(openTiers, r.Auth) {
			open = append(open, key)
		}
		require.False(t, r.Auth == AuthMerchant && r.Sensitive, "%s: admin operations never ask for a recent sign-in", key)
		require.False(t, r.Sensitive && r.Auth != AuthNetwork, "%s: only a network route steps up", key)
		if r.ClientMethod() {
			require.NotEmpty(t, r.Name, "%s: a Client method calls it", key)
			require.Empty(t, names[r.Name], "%s: %s already names %s", key, r.Name, names[r.Name])
			names[r.Name] = key
		} else {
			require.Empty(t, r.Name, key)
		}
		if r.Staff() {
			require.Equal(t, AuthMerchant, r.Auth, key)
			if r.Group == Admin || r.Group == CatalogAdmin || r.Group == MerchantConfig {
				require.Contains(t, []Level{LevelRead, LevelUpdate}, r.Level, key)
			} else {
				require.Empty(t, r.Level, "%s: a configuration or metrics route has its group's one permission", key)
			}
		} else if r.Group == App && r.Level != "" {
			require.Equal(t, LevelRead, r.Level, "%s: only a POST that reads declares a level", key)
			require.Equal(t, POST, r.Method, key)
		} else {
			require.Empty(t, r.Level, key)
		}
		switch r.Auth {
		case AuthCustomer:
			require.Equal(t, Customer, r.Group, key)
			require.False(t, r.Sensitive, "%s: customer self-service never steps up", key)
			for _, param := range []string{"{customer_id}", "{subject}", "{user_id}"} {
				require.NotContains(t, r.Path, param, "%s: a customer route takes its customer from the gate only", key)
			}
		case AuthNetwork:
			require.Equal(t, Network, r.Group, key)
			for _, param := range []string{"{customer_id}", "{subject}", "{user_id}"} {
				require.NotContains(t, r.Path, param, "%s: a network route takes its customer from the account's link only", key)
			}
		}
	}
	sort.Strings(open)
	want := slices.Clone(openRoutes)
	sort.Strings(want)
	require.Equal(t, want, open, "the routes no Auth gates")
}

// everyGroup turns on every staff group and the programmatic routes.
var everyGroup = config.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}

// recordingAuth says every request is who, records each call the gate
// makes, and grants what allowed names (every permission when nil) in its
// root scope, authtest.Root, and what at names in merchantA's scope.
type recordingAuth struct {
	mu      sync.Mutex
	calls   []string
	who     billingauth.Identity
	allowed map[billing.Permission]bool
	at      map[billing.Permission]bool
	// authErr, canErr and signInErr are the answers of Authenticate, Can and
	// CheckRecentSignIn; noSignIn drops CheckRecentSignIn.
	authErr, canErr, signInErr error
	noSignIn                   bool
}

func (*recordingAuth) RootScope(context.Context) (billingauth.Scope, error) {
	return authtest.Root, nil
}

func (a *recordingAuth) record(call string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
}

func (a *recordingAuth) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.calls
	a.calls = nil
	return out
}

func (a *recordingAuth) Authenticate(*http.Request) (billingauth.Verified, error) {
	a.record("Authenticate")
	if a.authErr != nil {
		return nil, a.authErr
	}
	if a.noSignIn {
		return identityOnly{recorded{a}}, nil
	}
	return recorded{a}, nil
}

type recorded struct{ a *recordingAuth }

func (v recorded) Identity() billingauth.Identity { return v.a.who }

func (v recorded) Can(_ context.Context, scope billingauth.Scope, perm string) (bool, error) {
	v.a.record("Can:" + perm)
	if v.a.canErr != nil {
		return false, v.a.canErr
	}
	switch scope {
	case authtest.Root:
		return v.a.allowed == nil || v.a.allowed[billing.Permission(perm)], nil
	case authtest.MerchantScope(merchantA):
		return v.a.at[billing.Permission(perm)], nil
	}
	return false, nil
}

// asks are the Can calls a gate makes for pair of a caller holding its
// root: name alone: the merchant: name first.
func asks(pair Pair) []string {
	return []string{"Can:" + string(pair.Merchant), "Can:" + string(pair.Root)}
}

func (v recorded) CheckRecentSignIn(context.Context) error {
	v.a.record("CheckRecentSignIn")
	return v.a.signInErr
}

// identityOnly holds permissions but cannot prove a sign-in.
type identityOnly struct{ r recorded }

func (v identityOnly) Identity() billingauth.Identity { return v.r.Identity() }

func (v identityOnly) Can(ctx context.Context, scope billingauth.Scope, perm string) (bool, error) {
	return v.r.Can(ctx, scope, perm)
}

// gatedRuntime is a database-free runtime that declares merchantA, with the
// fakes' issuer as its own: their credentials, bound to no scope, act there.
func gatedRuntime(t *testing.T) *app.Runtime {
	t.Helper()
	return runtimeDeclaring(merchantA)
}

// runtimeDeclaring is a database-free runtime whose one declared merchant
// the fakes' issuer binds; the zero id declares none.
func runtimeDeclaring(mid billing.MerchantID) *app.Runtime {
	rt := &app.Runtime{Config: &config.Config{Vault: &config.VaultConfig{KVMount: "kv"}}}
	if !mid.IsZero() {
		rt.DeclareMerchants([]billing.MerchantID{mid}, map[string]billing.MerchantID{authtest.Root.Authority: mid})
	}
	return rt
}

// live is the tests' directory: merchantA and merchantB are live.
func live(_ context.Context, id billing.MerchantID) (bool, error) {
	return id == merchantA || id == merchantB, nil
}

// everyGatedSurface mounts every group an embedded host can publish, its
// subjects saying who they are through a.
func everyGatedSurface(t *testing.T, a billingauth.Authenticator) (*router.Table, *app.Runtime) {
	rt := gatedRuntime(t)
	providers := routesurface.AllProviderRoutes()
	table := &router.Table{}
	RegisterUserRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, ProviderRoutes: &providers, live: live})
	RegisterStaffRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, Groups: everyGroup, live: live})
	RegisterAppRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, Groups: everyGroup, live: live})
	RegisterCustomerRoutes(router.NewMux(table, "/v1/me", rt), rt, CustomerMount{Auth: a, Providers: providers, live: live})
	RegisterWebhookRoutes(router.NewMux(table, "/v1/webhooks", rt), rt)
	return table, rt
}

func filled(path string) string { return wildcard.ReplaceAllString(path, "x") }

// serveSafely serves a request; a handler panicking on the bare runtime past
// every gate answers handlerReached.
func serveSafely(h http.Handler, r *http.Request) (code int) {
	defer func() {
		if recover() != nil {
			code = handlerReached
		}
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

// Each tier asks the host's auth exactly what it needs, once: who the
// request is on every gated route; a staff route's permission, its
// merchant: name at the merchant and then its root: name in the root scope,
// and an application's on a programmatic route. Admin routes never ask
// for a recent sign-in; the access read asks each mounted group's permission;
// an open route asks nothing.
func TestGatesAskTheVerified(t *testing.T) {
	for _, who := range []billingauth.Identity{authtest.User(userA), authtest.Application(userA)} {
		rec := &recordingAuth{who: who}
		table, _ := everyGatedSurface(t, rec)
		h := table.Handler()
		mounted := map[string]bool{}
		for _, key := range routeKeys(table) {
			mounted[key] = true
		}
		checked := 0
		for _, r := range Catalog() {
			if !mounted[r.Key()] {
				continue
			}
			var want []string
			switch r.Auth {
			case AuthCustomer, AuthProvisioning:
				want = []string{"Authenticate"}
			case AuthApplication:
				want = []string{"Authenticate"}
				if who.SubjectKind == billingauth.SubjectApplication {
					want = append(want, asks(r.Needs())...)
				}
			case AuthSignedIn:
				want = []string{"Authenticate"}
				for _, pair := range []Pair{needAdminRead, needAdminUpdate, {billing.MerchantCatalogRead, billing.RootCatalogRead}, needCatalog, needMerchantConfigRead, needMerchantConfig, needMetrics} {
					want = append(want, asks(pair)...)
				}
				if who.SubjectKind == billingauth.SubjectApplication {
					env := newEnv(gatedRuntime(t), Options{Groups: everyGroup})
					for _, pair := range env.applicationPermissions {
						want = append(want, asks(pair)...)
					}
				}
			case AuthMerchant:
				want = append([]string{"Authenticate"}, asks(r.Needs())...)
				// Clearing an incident also needs root authority, asked only
				// of an interactive human.
				if r.Name == "ClearDestructiveHold" && billingauth.Interactive(who) {
					want = append(want, "Can:"+string(billing.RootAdminRateLimitsUnlock))
				}
			}
			req := httptest.NewRequest(r.Method, filled(r.Path), strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer any")
			serveSafely(h, req)
			require.Equal(t, want, rec.take(), "%s as %s", r.Key(), who.SubjectKind)
			checked++
		}
		require.NotZero(t, checked)
	}
}

// An Authenticator that admits a request without saying who it is gets no
// route past OpenRails' own check: every gated route is refused before its
// handler.
func TestPassThroughAuthIsRefusedEverywhere(t *testing.T) {
	table, _ := everyGatedSurface(t, authtest.PassThrough{})
	h := table.Handler()
	gated := 0
	for _, r := range Catalog() {
		guarded := r.Auth == AuthCustomer || r.Auth == AuthMerchant && r.Staff() || r.Auth == AuthApplication || r.Auth == AuthProvisioning || r.Auth == AuthSignedIn
		if !guarded {
			continue
		}
		req := httptest.NewRequest(r.Method, filled(r.Path), strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer forged")
		code := serveSafely(h, req)
		if code == http.StatusNotFound {
			continue // unmounted for this configuration
		}
		require.Equal(t, http.StatusUnauthorized, code, r.Key())
		gated++
	}
	require.NotZero(t, gated)
}

// Every gated handler re-checks the verdict the gate bound: reached without
// it, by any path, it answers 401 and does not run.
func TestHandlersRecheckTheirVerdict(t *testing.T) {
	rt := gatedRuntime(t)
	env := newEnv(rt, Options{Auth: authtest.Deny{}, Groups: everyGroup})
	env.Customers = authtest.Deny{}
	for _, r := range Catalog() {
		if r.Auth != AuthCustomer && r.Auth != AuthMerchant {
			continue
		}
		h := env.Guarded(r)
		if h == nil {
			continue
		}
		for _, ctx := range []context.Context{
			context.Background(),
			merchant.WithID(context.Background(), merchantA),
			// The other tier's verdict is not this one's.
			billingauth.BindStaff(customerscope.Bind(merchant.WithID(context.Background(), merchantA), merchantA, billing.CustomerID(uuid.MustParse(userA)), userA, true),
				billingauth.Staff{Identity: authtest.User(userA), Route: "GET /v1/admin/other", Merchant: merchantA}),
		} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(r.Method, filled(r.Path), strings.NewReader("{}")).WithContext(ctx)
			code := func() (code int) {
				defer func() {
					if recover() != nil {
						code = handlerReached
					}
				}()
				h(httprequest.NewHTTP(w, req, rt))
				return w.Code
			}()
			require.Equal(t, http.StatusUnauthorized, code, r.Key())
		}
	}
}

// The customer gate admits a user, whatever credential they signed in with,
// as the customer the request acts for, at the merchant its credential is
// bound to; a service or a non-UUID subject is no customer.
func TestCustomerGate(t *testing.T) {
	rt := gatedRuntime(t)
	run := func(who billingauth.Identity, header map[string]string) (int, customerscope.Scope) {
		table := &router.Table{}
		var seen customerscope.Scope
		mux := router.NewMux(table, "/v1/me", rt)
		env := newEnv(rt, Options{live: live})
		env.Customers = &recordingAuth{who: who}
		env.admit = admitAll
		route, _ := Lookup(GET, "/v1/me")
		mux.Handle(GET, "", func(r *httprequest.Request) {
			seen, _ = r.CustomerScope()
			r.NoContent()
		}, env.gates(route)...)
		req := httptest.NewRequest(GET, "/v1/me", nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		return serveSafely(table.Handler(), req), seen
	}
	user := authtest.User(userA)
	code, scope := run(user, nil)
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, userA, scope.Customer().String())
	require.Equal(t, merchantA, scope.Merchant())

	device := user
	device.Credential = billingauth.Credential{Kind: billingauth.CredentialDeviceKey, ID: "dk_1"}
	foreign := authtest.Application(userB)
	foreign.Invoker = billingauth.Invoker{Issuer: "https://cozy.example", ID: "u_42"}
	code, scope = run(device, nil)
	require.Equal(t, http.StatusNoContent, code, "a device key is its user")
	require.Equal(t, userA, scope.Customer().String())

	for name, tc := range map[string]struct {
		who    billingauth.Identity
		header map[string]string
		status int
	}{
		"an application acting itself":   {authtest.Application(userA), nil, http.StatusForbidden},
		"an invoker for another subject": {foreign, nil, http.StatusForbidden},
		"no subject kind":                {withKind(user, ""), nil, http.StatusForbidden},
		"no invoker":                     {withInvoker(user, billingauth.Invoker{}), nil, http.StatusUnauthorized},
		"an opaque subject":              {authtest.User("user-1"), nil, http.StatusUnauthorized},
		"a non-canonical UUID":           {authtest.User(strings.ToUpper(merchantB.String())), nil, http.StatusUnauthorized},
	} {
		code, _ := run(tc.who, tc.header)
		require.Equal(t, tc.status, code, name)
	}
	// Nothing in the request names the merchant: the retired selector header
	// naming another one changes nothing.
	code, scope = run(user, map[string]string{retiredSelector: "id:" + merchantB.String()})
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, merchantA, scope.Merchant())
}

// retiredSelector is the request header that once named a merchant.
const retiredSelector = "OpenRails" + "-Merchant"

// refusal is what a gate answered.
type refusal struct {
	status    int
	code      string
	challenge string
	metadata  map[string]any
	staff     billingauth.Staff
	// bound is the merchant the response says the request acted on.
	bound string
}

// runRoute serves key behind its gates, with a.
func runRoute(t *testing.T, rt *app.Runtime, a billingauth.Authenticator, key string, header map[string]string) refusal {
	t.Helper()
	method, path, _ := strings.Cut(key, " ")
	route, ok := Lookup(method, path)
	require.True(t, ok, key)
	env := newEnv(rt, Options{Auth: a, Groups: everyGroup, live: live})
	table := &router.Table{}
	var out refusal
	router.NewMux(table, "", rt).Handle(route.Method, route.Path, func(r *httprequest.Request) {
		out.staff, _ = r.Staff()
		r.NoContent()
	}, env.gates(route)...)
	req := httptest.NewRequest(route.Method, filled(route.Path), strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer any")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	table.Handler().ServeHTTP(rec, req)
	out.status, out.challenge, out.bound = rec.Code, rec.Header().Get("WWW-Authenticate"), rec.Header().Get(merchant.BoundHeader)
	var body struct {
		Error struct {
			Code     string         `json:"code"`
			Metadata map[string]any `json:"metadata"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	out.code, out.metadata = body.Error.Code, body.Error.Metadata
	return out
}

const refund = "POST /v1/admin/payments/{id}/refunds"

// The staff gate admits a person or an application holding the route's
// permission at the merchant its credential is bound to. Both pass on
// that permission without a recent sign-in, including a person's API key.
func TestStaffGate(t *testing.T) {
	rt := gatedRuntime(t)
	person := authtest.User(userA)
	service := authtest.Application("svc_1")
	personalKey := person
	personalKey.Credential = billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "pk_1"}

	got := runRoute(t, rt, &recordingAuth{who: person}, refund, nil)
	require.Equal(t, http.StatusNoContent, got.status, got.code)
	require.Equal(t, billingauth.Staff{Identity: person, Route: refund, Merchant: merchantA}, got.staff)
	require.Equal(t, merchantA.String(), got.bound, "the response names the merchant it acted on")

	stepUp := &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute, Metadata: map[string]any{"step_up_methods": []any{"password"}}}
	revoked := errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)
	// A provider's sender-proof refusal is a 401 like any other; its
	// challenge passes through unread.
	senderProof := errors.Join(auth.ErrUnauthenticated, auth.ErrSenderProofRequired)
	presented := httptest.NewRequest(http.MethodGet, "/", nil)
	presented.Header.Set("Authorization", "Bearer x")
	for name, tc := range map[string]struct {
		auth      billingauth.Authenticator
		header    map[string]string
		status    int
		code      string
		challenge string
	}{
		"no credential":                              {authtest.Deny{}, map[string]string{"Authorization": ""}, 401, "authentication_required", "Bearer"},
		"a refused credential":                       {authtest.Deny{}, nil, 401, "authentication_required", `Bearer error="invalid_token"`},
		"an expired credential":                      {&recordingAuth{authErr: errors.Join(auth.ErrUnauthenticated, auth.ErrExpired)}, nil, 401, "credential_expired", `Bearer error="invalid_token"`},
		"a revoked credential":                       {&recordingAuth{authErr: revoked}, nil, 401, "credential_revoked", `Bearer error="invalid_token"`},
		"a missing sender proof":                     {&recordingAuth{authErr: senderProof}, nil, 401, "authentication_required", auth.Refuse(presented, senderProof).Header.Get("WWW-Authenticate")},
		"a provider's own challenge":                 {&recordingAuth{authErr: &auth.Challenge{Err: auth.ErrSenderProofRequired, Header: http.Header{"Www-Authenticate": {`Proof error="use_nonce"`}, "Proof-Nonce": {"n1"}}}}, nil, 401, "authentication_required", `Proof error="use_nonce"`},
		"a credential refused here":                  {&recordingAuth{authErr: auth.ErrForbidden}, nil, 403, "permission_required", ""},
		"auth unavailable":                           {&recordingAuth{authErr: auth.ErrUnavailable}, nil, 503, "authentication_unavailable", ""},
		"an unclassified error":                      {&recordingAuth{authErr: errors.New("boom")}, nil, 503, "authentication_unavailable", ""},
		"a panicking Authenticate":                   {panicking{}, nil, 503, "authentication_unavailable", ""},
		"a panicking Identity":                       {panicking{identity: true}, nil, 503, "authentication_unavailable", ""},
		"neither a Verified nor an error":            {nothing{}, nil, 503, "authentication_unavailable", ""},
		"an unknown subject kind":                    {&recordingAuth{who: withKind(person, "")}, nil, 403, "permission_required", ""},
		"no invoker":                                 {&recordingAuth{who: withInvoker(person, billingauth.Invoker{})}, nil, 401, "authentication_required", `Bearer error="invalid_token"`},
		"a merchant that is not live":                {boundAuth{who: person, scope: authtest.MerchantScope(billing.MerchantID(uuid.New()))}, nil, 403, "merchant_unresolved", ""},
		"a permission not held":                      {&recordingAuth{who: person, allowed: map[billing.Permission]bool{}}, nil, 403, "permission_required", ""},
		"a root scope that could not be read":        {rootAnswering{&recordingAuth{who: person}, func() (billingauth.Scope, error) { return billingauth.Scope{}, auth.ErrUnavailable }}, nil, 503, "authorization_unavailable", ""},
		"a root scope that names none":               {rootAnswering{&recordingAuth{who: person}, func() (billingauth.Scope, error) { return billingauth.Scope{Authority: "test"}, nil }}, nil, 503, "authorization_unavailable", ""},
		"a panicking RootScope":                      {rootAnswering{&recordingAuth{who: person}, func() (billingauth.Scope, error) { panic("broken provider") }}, nil, 503, "authorization_unavailable", ""},
		"a Can whose sign-in was revoked":            {&recordingAuth{who: person, canErr: revoked}, nil, 401, "credential_revoked", `Bearer error="invalid_token"`},
		"a Can that could not run":                   {&recordingAuth{who: person, canErr: errors.New("db")}, nil, 503, "authorization_unavailable", ""},
		"a Can that refuses with an error":           {&recordingAuth{who: person, canErr: auth.ErrForbidden}, nil, 503, "authorization_unavailable", ""},
		"a stale sign-in":                            {&recordingAuth{who: person, signInErr: stepUp}, nil, 204, "", ""},
		"no sign-in of its own":                      {&recordingAuth{who: person, signInErr: auth.ErrForbidden}, nil, 204, "", ""},
		"a person who cannot prove a sign-in":        {&recordingAuth{who: person, noSignIn: true}, nil, 204, "", ""},
		"a person's API key":                         {&recordingAuth{who: personalKey, noSignIn: true}, nil, 204, "", ""},
		"an unused sign-in check reports revocation": {&recordingAuth{who: person, signInErr: revoked}, nil, 204, "", ""},
		"a sign-in check that could not run":         {&recordingAuth{who: person, signInErr: auth.ErrUnavailable}, nil, 204, "", ""},
	} {
		got := runRoute(t, rt, tc.auth, refund, tc.header)
		require.Equal(t, tc.status, got.status, "%s: %s", name, got.code)
		require.Equal(t, tc.code, got.code, name)
		require.Equal(t, tc.challenge, got.challenge, name)
	}
	got = runRoute(t, rt, &recordingAuth{who: person, signInErr: stepUp}, refund, nil)
	require.Equal(t, http.StatusNoContent, got.status, "a stale staff sign-in passes on its permission")
	require.Empty(t, got.metadata)
	got = runRoute(t, rt, &recordingAuth{who: service, signInErr: stepUp, noSignIn: true}, refund, nil)
	require.Equal(t, http.StatusNoContent, got.status, "an application has no sign-in to renew")
	got = runRoute(t, rt, &recordingAuth{who: person, signInErr: stepUp}, "GET /v1/admin/payments", nil)
	require.Equal(t, http.StatusNoContent, got.status, "a read asks no sign-in")

	// A programmatic route takes only an application holding its
	// permission: a person is refused, whatever their roles grant or their
	// credential is.
	for _, who := range []billingauth.Identity{person, personalKey} {
		got = runRoute(t, rt, &recordingAuth{who: who}, "GET /v1/app/host-events", nil)
		require.Equal(t, http.StatusForbidden, got.status, who.Credential.Kind)
		require.Equal(t, "application_required", got.code)
	}
	got = runRoute(t, rt, &recordingAuth{who: service, allowed: map[billing.Permission]bool{billing.RootUsageManage: true, billing.RootEntitlementsRead: true}}, "GET /v1/app/host-events", nil)
	require.Equal(t, http.StatusForbidden, got.status, "every permission but events")
	require.Equal(t, "permission_required", got.code)
	got = runRoute(t, rt, &recordingAuth{who: service, allowed: map[billing.Permission]bool{billing.RootEventsRead: true}}, "GET /v1/app/host-events", nil)
	require.Equal(t, http.StatusNoContent, got.status, "events alone")
	require.Equal(t, "GET /v1/app/host-events", got.staff.Route)
	// Naturally identified writes need no redundant HTTP retry key.
	got = runRoute(t, rt, &recordingAuth{who: service}, "POST /v1/app/host-events/acknowledge", nil)
	require.Equal(t, http.StatusNoContent, got.status, "acknowledging named events needs no Idempotency-Key")

	// The retired selector header names nothing.
	got = runRoute(t, rt, &recordingAuth{who: person}, "GET /v1/admin/payments", map[string]string{retiredSelector: "id:" + merchantB.String()})
	require.Equal(t, http.StatusNoContent, got.status)
	require.Equal(t, merchantA, got.staff.Merchant)

	// No deployment default fills in: where no merchant declares the
	// credential's issuer, a credential bound to no scope reaches no merchant.
	got = runRoute(t, runtimeDeclaring(billing.MerchantID{}), &recordingAuth{who: person}, "GET /v1/admin/payments", nil)
	require.Equal(t, http.StatusForbidden, got.status)
	require.Equal(t, "merchant_unresolved", got.code)
	require.Empty(t, got.bound)
}

// rootAnswering is an Authenticator whose RootScope answers as scope does.
type rootAnswering struct {
	*recordingAuth
	scope func() (billingauth.Scope, error)
}

func (a rootAnswering) RootScope(context.Context) (billingauth.Scope, error) { return a.scope() }

// rootless is an Authenticator that is no authkit/auth Root.
type rootless struct{ a billingauth.Authenticator }

func (r rootless) Authenticate(req *http.Request) (billingauth.Verified, error) {
	return r.a.Authenticate(req)
}

// catalogued knows only its own permissions (authkit/auth PermissionCatalog).
type catalogued struct {
	*recordingAuth
	known []billing.Permission
}

func (c catalogued) KnownPermission(p string) bool {
	return slices.Contains(c.known, billing.Permission(p))
}

// panicking is a broken provider.
type panicking struct{ identity bool }

func (panicking) RootScope(context.Context) (billingauth.Scope, error) { return authtest.Root, nil }

func (p panicking) Authenticate(*http.Request) (billingauth.Verified, error) {
	if p.identity {
		return p, nil
	}
	panic("broken provider")
}

func (panicking) Identity() billingauth.Identity { panic("broken provider") }

// nothing answers neither a Verified nor an error.
type nothing struct{}

func (nothing) Authenticate(*http.Request) (billingauth.Verified, error) { return nil, nil }
func (nothing) RootScope(context.Context) (billingauth.Scope, error)     { return authtest.Root, nil }

// Mount refuses a route it cannot gate: nothing is mounted open. A staff or
// programmatic route needs an Auth with a root scope, one that knows a
// permission of the route.
func TestMountFailsClosed(t *testing.T) {
	rt := gatedRuntime(t)
	staff := func(a billingauth.Authenticator, groups config.RouteGroups) func() {
		return func() {
			RegisterStaffRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: a, Groups: groups})
		}
	}
	programs := func(a billingauth.Authenticator) func() {
		return func() {
			RegisterAppRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: a, Groups: everyGroup})
		}
	}
	who := &recordingAuth{who: authtest.User(userA)}
	for name, mount := range map[string]func(){
		"admin without Auth":                               staff(nil, everyGroup),
		"admin with a typed nil Auth":                      staff((*authtest.Fake)(nil), everyGroup),
		"admin without a root scope":                       staff(rootless{who}, everyGroup),
		"a group whose permissions the Auth does not know": staff(catalogued{who, []billing.Permission{billing.RootCustomerRead, billing.RootCustomerManage}}, config.RouteGroups{Admin: true, Metrics: true}),
		"customers without Auth": func() {
			RegisterCustomerRoutes(router.NewMux(&router.Table{}, "/v1/me", rt), rt, CustomerMount{})
		},
		"network accounts without a root scope": func() {
			env := newEnv(rt, Options{})
			env.Customers = rootless{who}
			route, _ := Lookup(GET, "/v1/network/links")
			env.networkGates(route)
		},
		"programmatic without Auth":                        programs(nil),
		"programmatic without a root scope":                programs(rootless{who}),
		"a programmatic permission the Auth does not know": programs(catalogued{who, []billing.Permission{billing.RootUsageManage}}),
	} {
		require.PanicsWithError(t, mountErrorFor(t, mount), mount, name)
	}
	require.Contains(t, mountErrorFor(t, staff(catalogued{who, []billing.Permission{billing.RootCustomerRead, billing.RootCustomerManage}}, config.RouteGroups{Admin: true, Metrics: true})),
		`knows none of its permissions ["merchant:metrics:read" "root:metrics:read"]`)

	// Without a group on, no staff or programmatic route is mounted at all;
	// customers need no root scope where their issuer binds the merchant.
	none := &router.Table{}
	RegisterStaffRoutes(router.NewMux(none, "/v1", rt), rt, Options{Auth: rootless{who}})
	RegisterAppRoutes(router.NewMux(none, "/v1", rt), rt, Options{Auth: rootless{who}, Groups: config.RouteGroups{Admin: true}})
	require.Empty(t, none.Entries)
	RegisterCustomerRoutes(router.NewMux(none, "/v1/me", rt), rt, CustomerMount{Auth: rootless{who}})
	require.NotEmpty(t, none.Entries)

	// A group that is on mounts all its routes: the admin group its reads
	// and its changes, the programmatic one every /v1/app route its feature
	// mounts, SCIM among them. One known name of each pair is enough.
	merchantOnly := catalogued{who, nil}
	for _, perm := range billing.Permissions() {
		if perm.Persona() == billing.MerchantGroupPersona {
			merchantOnly.known = append(merchantOnly.known, perm)
		}
	}
	on := &router.Table{}
	RegisterStaffRoutes(router.NewMux(on, "/v1", rt), rt, Options{Auth: merchantOnly, Groups: config.RouteGroups{Admin: true}})
	RegisterAppRoutes(router.NewMux(on, "/v1", rt), rt, Options{Auth: merchantOnly, Groups: config.RouteGroups{Programmatic: true}})
	env := newEnv(rt, Options{Groups: everyGroup})
	for _, r := range Catalog() {
		mounted := slices.ContainsFunc(on.Entries, func(e router.Entry) bool { return e.Method == r.Method && e.Path == r.Path })
		require.Equal(t, (r.Group == Admin || r.Group == App || r.Group == Access) && env.mounts(r), mounted, r.Key())
	}
}

// A route's permission is a pair: its merchant: name held in the scope of
// the merchant the request acts on, or its root: name held in the root
// scope. Neither admits anywhere else, and a route about no one merchant
// asks the root: name alone.
func TestGateAsksThePair(t *testing.T) {
	rt := gatedRuntime(t)
	person := authtest.User(userA)
	const payments = "GET /v1/admin/payments"
	holding := func(root, at map[billing.Permission]bool) *recordingAuth {
		if root == nil {
			root = map[billing.Permission]bool{}
		}
		return &recordingAuth{who: person, allowed: root, at: at}
	}
	held := func(perm billing.Permission) map[billing.Permission]bool {
		return map[billing.Permission]bool{perm: true}
	}
	for name, tc := range map[string]struct {
		auth   *recordingAuth
		route  string
		status int
	}{
		"the merchant: name at the merchant":      {holding(nil, held(billing.MerchantCustomerRead)), payments, http.StatusNoContent},
		"the root: name in the root scope":        {holding(held(billing.RootCustomerRead), nil), payments, http.StatusNoContent},
		"the merchant: name in the root scope":    {holding(held(billing.MerchantCustomerRead), nil), payments, http.StatusForbidden},
		"the root: name at the merchant":          {holding(nil, held(billing.RootCustomerRead)), payments, http.StatusForbidden},
		"neither":                                 {holding(nil, nil), payments, http.StatusForbidden},
		"the read's name on a change":             {holding(nil, held(billing.MerchantCustomerRead)), refund, http.StatusForbidden},
		"another group's name":                    {holding(held(billing.RootMetricsRead), held(billing.MerchantCatalogManage)), payments, http.StatusForbidden},
		"the change's merchant: name on a change": {holding(nil, held(billing.MerchantCustomerManage)), refund, http.StatusNoContent},
	} {
		got := runRoute(t, rt, tc.auth, tc.route, nil)
		require.Equal(t, tc.status, got.status, "%s: %s", name, got.code)
		if tc.status == http.StatusForbidden {
			require.Equal(t, "permission_required", got.code, name)
		}
	}

	// Retired billing permission names grant no alias to the customer gates.
	for _, old := range []struct {
		merchant, root billing.Permission
		route          string
	}{
		{"merchant:billing:read", "root:billing:read", payments},
		{"merchant:billing:manage", "root:billing:manage", refund},
	} {
		got := runRoute(t, rt, holding(held(old.root), held(old.merchant)), old.route, nil)
		require.Equal(t, http.StatusForbidden, got.status, "retired permissions %s and %s", old.merchant, old.root)
		require.Equal(t, "permission_required", got.code)
	}

	// The merchant: name admits only at its own merchant: recordingAuth
	// holds it at merchantA, and its issuer binds merchantB.
	elsewhere := runtimeDeclaring(merchantB)
	require.Equal(t, http.StatusForbidden, runRoute(t, elsewhere, holding(nil, held(billing.MerchantCustomerRead)), payments, nil).status)
	require.Equal(t, http.StatusNoContent, runRoute(t, elsewhere, holding(held(billing.RootCustomerRead), nil), payments, nil).status, "the root: name reaches every merchant")

	// A name the Auth does not know is never asked.
	known := catalogued{holding(held(billing.RootCustomerRead), nil), []billing.Permission{billing.RootCustomerRead}}
	require.Equal(t, http.StatusNoContent, runRoute(t, rt, known, payments, nil).status)
	require.Equal(t, []string{"Authenticate", "Can:" + string(billing.RootCustomerRead)}, known.take())

	// A route about no one merchant names its root: permission alone.
	rootOnly := func(a *recordingAuth) int {
		route, _ := Lookup(GET, "/v1/admin/payments")
		env, table := newEnv(rt, Options{Auth: a, Groups: everyGroup, live: live}), &router.Table{}
		router.NewMux(table, "", rt).Handle(route.Method, route.Path, func(r *httprequest.Request) { r.NoContent() }, verify(route, a), env.staffCheck(route, a, Pair{Root: billing.RootCustomerRead}))
		return serveSafely(table.Handler(), httptest.NewRequest(route.Method, route.Path, nil))
	}
	require.Equal(t, http.StatusNoContent, rootOnly(holding(held(billing.RootCustomerRead), nil)))
	merchantHolder := holding(nil, map[billing.Permission]bool{billing.MerchantCustomerRead: true, billing.RootCustomerRead: true})
	require.Equal(t, http.StatusForbidden, rootOnly(merchantHolder), "held at one merchant, it reaches no root-only route")
	require.Equal(t, []string{"Authenticate", "Can:" + string(billing.RootCustomerRead)}, merchantHolder.take(), "only the root scope is asked")
}

func mountErrorFor(t *testing.T, mount func()) (msg string) {
	t.Helper()
	defer func() {
		v := recover()
		err, ok := v.(MountError)
		require.True(t, ok, "%v", v)
		msg = err.Error()
	}()
	mount()
	return ""
}

// Only the route gate makes a customer scope or binds a verdict, and every
// gated tier binds its merchant through boundMerchant alone.
func TestBindersHaveOneSite(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	calls := map[string][]string{}
	binders := map[string][]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == ".git" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == "boundMerchant" && onEnv(fn) {
				binders["defined"] = append(binders["defined"], filepath.ToSlash(rel))
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "boundMerchant" || sel.Sel.Name == "bound") {
					if env, ok := sel.X.(*ast.Ident); ok && env.Name == "e" && onEnv(fn) {
						binders[sel.Sel.Name] = append(binders[sel.Sel.Name], fn.Name.Name)
					}
				}
				return true
			})
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			name := pkg.Name + "." + sel.Sel.Name
			switch name {
			case "customerscope.Bind", "billingauth.BindIdentity", "billingauth.BindStaff", "billingauth.BindVerified":
				calls[name] = append(calls[name], filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	}))
	gate := "internal/http/routes/gate.go"
	require.Equal(t, map[string][]string{
		"customerscope.Bind":       {gate},
		"billingauth.BindIdentity": {gate},
		"billingauth.BindStaff":    {gate},
		"billingauth.BindVerified": {gate},
	}, calls)
	for _, callers := range binders {
		slices.Sort(callers)
	}
	require.Equal(t, map[string][]string{
		"defined":       {gate},
		"boundMerchant": {"bound", "provisioningGate"},
		"bound":         {"appCheck", "customerCheck", "signedInGates", "staffCheck"},
	}, binders)
}

// onEnv is a method of the route gate's *Env.
func onEnv(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	name, ok := star.X.(*ast.Ident)
	return ok && name.Name == "Env"
}

func withKind(id billingauth.Identity, kind billingauth.SubjectKind) billingauth.Identity {
	id.SubjectKind = kind
	return id
}

func withInvoker(id billingauth.Identity, invoker billingauth.Invoker) billingauth.Identity {
	id.Invoker = invoker
	return id
}

// The access read answers, for any signed-in caller, each mounted group the
// caller holds, at the merchant or in the root scope: none, read or update
// for customer support, and whether the catalog, merchant config and metrics
// are theirs; and the merchant's own scope.
func TestAccessAnswersWhatTheCallerHolds(t *testing.T) {
	rt := gatedRuntime(t)
	read := func(groups config.RouteGroups, gate *recordingAuth) billing.AdminAccess {
		gate.who = authtest.User(userA)
		table := &router.Table{}
		RegisterStaffRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: gate, Groups: groups, live: live})
		rec := do(table.Handler(), http.MethodGet, "/v1/admin/access", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var access billing.AdminAccess
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &access))
		require.Equal(t, merchantA.String(), access.ScopeID)
		access.ScopeID = ""
		require.Empty(t, access.ApplicationPermissions)
		access.ApplicationPermissions = nil
		if access.Catalog == billing.AccessNone {
			access.Catalog = ""
		}
		if access.MerchantConfig == billing.AccessNone {
			access.MerchantConfig = ""
		}
		return access
	}
	set := func(perms ...billing.Permission) map[billing.Permission]bool {
		out := map[billing.Permission]bool{}
		for _, perm := range perms {
			out[perm] = true
		}
		return out
	}
	root := func(perms ...billing.Permission) *recordingAuth { return &recordingAuth{allowed: set(perms...)} }
	at := func(perms ...billing.Permission) *recordingAuth {
		return &recordingAuth{allowed: set(), at: set(perms...)}
	}
	staff := authtest.Groups()
	none := billing.AdminAccess{Admin: billing.AccessNone}
	require.Equal(t, none, read(staff, root()), "a caller holding nothing")
	for _, tc := range []struct {
		want           billing.AdminAccess
		inRoot, atShop []billing.Permission
	}{
		{billing.AdminAccess{Admin: billing.AccessNone, Catalog: billing.AccessRead, MerchantConfig: billing.AccessRead}, []billing.Permission{billing.RootCatalogRead, billing.RootConfigRead}, []billing.Permission{billing.MerchantCatalogRead, billing.MerchantConfigRead}},
		{billing.AdminAccess{Admin: billing.AccessRead}, []billing.Permission{billing.RootCustomerRead}, []billing.Permission{billing.MerchantCustomerRead}},
		{billing.AdminAccess{Admin: billing.AccessUpdate, Metrics: true},
			[]billing.Permission{billing.RootCustomerRead, billing.RootCustomerManage, billing.RootMetricsRead},
			[]billing.Permission{billing.MerchantCustomerRead, billing.MerchantCustomerManage, billing.MerchantMetricsRead}},
		{billing.AdminAccess{Admin: billing.AccessNone, Catalog: billing.AccessUpdate, MerchantConfig: billing.AccessUpdate},
			[]billing.Permission{billing.RootCatalogRead, billing.RootCatalogManage, billing.RootConfigRead, billing.RootConfigManage},
			[]billing.Permission{billing.MerchantCatalogRead, billing.MerchantCatalogManage, billing.MerchantConfigRead, billing.MerchantConfigManage}},
	} {
		require.Equal(t, tc.want, read(staff, root(tc.inRoot...)))
		require.Equal(t, tc.want, read(staff, at(tc.atShop...)), "the merchant: names answer the same levels")
	}
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessUpdate}, read(staff, &recordingAuth{allowed: set(billing.RootCustomerManage), at: set(billing.MerchantCustomerRead)}), "a name from each scope")
	require.Equal(t, none, read(config.RouteGroups{MerchantConfig: true}, root(billing.RootCustomerRead, billing.RootMetricsRead)), "a group that is off is no one's")
	require.Equal(t, none, read(config.RouteGroups{Admin: true}, root(billing.RootCustomerManage)), "an update without the read is nothing")
	require.Equal(t, none, read(staff, root(billing.MerchantCustomerRead)), "a merchant: name held in the root scope")

	// Assistants require an enabled provider, consent, a mounted area, and its permission.
	rt.Config.LLM = &config.LLMConfig{APIKey: "test-key", CatalogCopilotEnabled: true, CatalogDraftingEnabled: true}
	catalog := root(billing.RootCatalogRead, billing.RootCatalogManage)
	require.Equal(t, billing.AdminAssistants{}, read(staff, root(billing.RootMetricsRead)).Assistants, "metrics permission grants no catalog assistant")
	require.Equal(t, billing.AdminAssistants{CatalogCopilot: true, CatalogDrafting: true}, read(staff, catalog).Assistants)
	require.Equal(t, billing.AdminAssistants{CatalogCopilot: true, CatalogDrafting: true}, read(staff, root(billing.RootCatalogRead)).Assistants, "the assistant is a catalog read; applying a draft still requires manage")
	require.Equal(t, billing.AdminAssistants{}, read(staff, root(billing.RootCatalogManage)).Assistants, "manage without read does not open the catalog")
	require.Equal(t, billing.AdminAssistants{}, read(staff, root()).Assistants, "no access to catalog")
	require.Equal(t, billing.AdminAssistants{}, read(config.RouteGroups{Admin: true}, catalog).Assistants, "catalog not mounted")
	rt.Config.LLM = &config.LLMConfig{APIKey: "test-key", CatalogDraftingEnabled: true}
	require.Equal(t, billing.AdminAssistants{}, read(staff, catalog).Assistants, "drafting requires copilot consent too")
	rt.Config.LLM = &config.LLMConfig{CatalogCopilotEnabled: true, CatalogDraftingEnabled: true}
	require.Equal(t, billing.AdminAssistants{}, read(staff, catalog).Assistants, "provider key missing")
}

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

var (
	merchantA = billing.MerchantID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	merchantB = billing.MerchantID(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
)

// admitAll admits every customer, as a database would a new one.
func admitAll(context.Context, billing.MerchantID, billing.CustomerID, string, bool) error {
	return nil
}

// boundAuth admits who, bound to scope (authkit/auth Bound), holding every
// permission there. scopes, when not nil, are the scopes its token was
// granted, and client the OAuth client it was issued to.
type boundAuth struct {
	who      billingauth.Identity
	scope    billingauth.Scope
	scopes   []string
	client   string
	asserted *bool
}

func (a boundAuth) Authenticate(*http.Request) (billingauth.Verified, error) {
	switch {
	case a.asserted != nil:
		return assertedAuth{a}, nil
	case a.scopes != nil:
		return scopedAuth{a}, nil
	case a.client != "":
		return clientAuth{a}, nil
	}
	return a, nil
}
func (a boundAuth) RootScope(context.Context) (billingauth.Scope, error) { return authtest.Root, nil }
func (a boundAuth) Identity() billingauth.Identity                       { return a.who }
func (a boundAuth) BoundScope() billingauth.Scope                        { return a.scope }
func (a boundAuth) Can(_ context.Context, scope billingauth.Scope, _ string) (bool, error) {
	return scope == a.scope, nil
}

// scopedAuth is a boundAuth whose token says what scopes it was granted.
type scopedAuth struct{ boundAuth }

func (a scopedAuth) Scopes() []string { return a.scopes }

// clientAuth is a boundAuth whose token names the client it was issued to.
type clientAuth struct{ boundAuth }

func (a clientAuth) ClientID() string { return a.client }

// assertedAuth exposes the verified assertion-grant capability.
type assertedAuth struct{ boundAuth }

func (a assertedAuth) Asserted() bool { return *a.asserted }

// A credential acts on the one merchant it is bound to. A bound scope is a
// live merchant's in the root authority or the credential reaches no
// merchant: it never falls through to the issuer binding. Bound to none, the
// merchant that declares its issuer is the one; otherwise none is.
func TestBoundMerchant(t *testing.T) {
	rt := gatedRuntime(t)
	app, person := authtest.Application("app"), authtest.User(userA)
	gone := billing.MerchantID(uuid.New())
	const findings = "GET /v1/merchant/findings"
	for name, tc := range map[string]struct {
		auth     billingauth.Authenticator
		merchant billing.MerchantID
	}{
		"its issuer's merchant, bound to no scope": {&recordingAuth{who: person}, merchantA},
		"the scope of the merchant":                {boundAuth{who: app, scope: authtest.MerchantScope(merchantA)}, merchantA},
		"the scope of another merchant":            {boundAuth{who: app, scope: authtest.MerchantScope(merchantB)}, merchantB},
		"the root scope":                           {boundAuth{who: app, scope: authtest.Root}, billing.MerchantID{}},
		"a scope of no merchant":                   {boundAuth{who: app, scope: billingauth.Scope{Authority: authtest.Root.Authority, ID: "staff"}}, billing.MerchantID{}},
		"the scope of a merchant that is not live": {boundAuth{who: app, scope: authtest.MerchantScope(gone)}, billing.MerchantID{}},
		"another authority's scope":                {boundAuth{who: app, scope: billingauth.Scope{Authority: "https://other.example", ID: merchantA.String()}}, billing.MerchantID{}},
		"a merchant id that is not canonical":      {boundAuth{who: app, scope: billingauth.Scope{Authority: authtest.Root.Authority, ID: strings.ToUpper(merchantA.String())}}, billing.MerchantID{}},
	} {
		got := runRoute(t, rt, tc.auth, "GET /v1/admin/access", nil)
		if tc.merchant.IsZero() {
			require.Equal(t, http.StatusForbidden, got.status, name)
			require.Equal(t, "merchant_unresolved", got.code, name)
			require.Empty(t, got.bound, name)
			continue
		}
		require.Equal(t, http.StatusNoContent, got.status, "%s: %s", name, got.code)
		require.Equal(t, tc.merchant, got.staff.Merchant, name)
		require.Equal(t, tc.merchant.String(), got.bound, name)
	}
	// The permission is asked at the bound merchant: a key of merchantB holds
	// nothing at merchantA, and no request reaches it there.
	require.Equal(t, merchantB, runRoute(t, rt, boundAuth{who: app, scope: authtest.MerchantScope(merchantB)}, findings, nil).staff.Merchant)

	// A lookup that fails is an outage, never a merchant.
	down := newEnv(rt, Options{Auth: boundAuth{who: app, scope: authtest.MerchantScope(merchantA)}, live: func(context.Context, billing.MerchantID) (bool, error) {
		return false, errors.New("directory down")
	}})
	_, err := down.boundMerchant(context.Background(), down.Auth, boundAuth{who: app, scope: authtest.MerchantScope(merchantA)}, app)
	require.ErrorIs(t, err, auth.ErrUnavailable)

	// An OpenRails credential of its own is bound to its row's merchant.
	env := newEnv(runtimeDeclaring(billing.MerchantID{}), Options{live: live})
	token := provisioningToken{billingauth.BindsMerchant(billingauth.Target{MerchantID: merchantB})}
	target, err := env.boundMerchant(context.Background(), nil, token, token.Identity())
	require.NoError(t, err)
	require.Equal(t, merchantB, target.MerchantID)
	token = provisioningToken{billingauth.BindsMerchant(billingauth.Target{MerchantID: gone})}
	_, err = env.boundMerchant(context.Background(), nil, token, token.Identity())
	require.ErrorContains(t, err, "bound to no merchant")
}

// A customer is its issuer's subject (OIDC Core §5.7). The merchant's own
// (issuer ""): the user of the issuer the merchant declares, an account of
// the root authority bound to it, and the customer its backend names in an
// assertion. Another issuer's user is pinned to that issuer, and never
// reaches a customer that is not its own.
func TestCustomerIssuer(t *testing.T) {
	rt := gatedRuntime(t)
	here := authtest.MerchantScope(merchantA)
	foreign := authtest.User(userA)
	foreign.Issuer, foreign.Invoker.Issuer = "https://issuer.example", "https://issuer.example"
	asserted, notAsserted := true, false
	native := authtest.User(userA)

	var issuer string
	var account bool
	recordIssuer := func(_ context.Context, _ billing.MerchantID, _ billing.CustomerID, iss string, acct bool) error {
		issuer, account = iss, acct
		return nil
	}
	customer := func(a billingauth.Authenticator, admit func(context.Context, billing.MerchantID, billing.CustomerID, string, bool) error) refusal {
		table := &router.Table{}
		env := newEnv(rt, Options{live: live})
		env.Customers, env.admit = a, admit
		route, _ := Lookup(GET, "/v1/me")
		router.NewMux(table, "/v1/me", rt).Handle(GET, "", func(r *httprequest.Request) { r.NoContent() }, env.gates(route)...)
		rec := httptest.NewRecorder()
		table.Handler().ServeHTTP(rec, httptest.NewRequest(GET, "/v1/me", nil))
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return refusal{status: rec.Code, code: body.Error.Code, bound: rec.Header().Get(merchant.BoundHeader)}
	}
	for name, tc := range map[string]struct {
		auth    billingauth.Authenticator
		issuer  string
		account bool
	}{
		"its issuer's user, bound to no scope":      {&recordingAuth{who: native}, "", true},
		"the root authority's account, bound":       {boundAuth{who: native, scope: here}, "", true},
		"that account, granted the customer scope":  {boundAuth{who: native, scope: here, scopes: []string{"openid", billing.ScopeSelf}}, "", true},
		"the customer the merchant's backend names": {boundAuth{who: foreign, scope: here, asserted: &asserted}, "", false},
		"a matching client is not an assertion":     {boundAuth{who: foreign, scope: here, client: foreign.Issuer}, foreign.Issuer, false},
		"an explicitly unasserted credential":       {boundAuth{who: foreign, scope: here, asserted: &notAsserted}, foreign.Issuer, false},
		"another issuer's user":                     {boundAuth{who: foreign, scope: here}, foreign.Issuer, false},
		"another issuer's user through a client":    {boundAuth{who: foreign, scope: here, client: "admin-ui"}, foreign.Issuer, false},
	} {
		issuer, account = "unset", false
		got := customer(tc.auth, recordIssuer)
		require.Equal(t, http.StatusNoContent, got.status, "%s: %s", name, got.code)
		require.Equal(t, tc.issuer, issuer, name)
		require.Equal(t, tc.account, account, "%s: the deployment's own account", name)
		require.Equal(t, merchantA.String(), got.bound, name)
	}
	for name, tc := range map[string]struct {
		auth  billingauth.Authenticator
		admit func(context.Context, billing.MerchantID, billing.CustomerID, string, bool) error
		code  string
	}{
		"a console's token, not granted the customer scope": {boundAuth{who: native, scope: here, scopes: []string{billing.ScopeMerchant}}, recordIssuer, "permission_required"},
		"a token granted no scope":                          {boundAuth{who: native, scope: here, scopes: []string{}}, recordIssuer, "permission_required"},
		"another issuer's customer": {boundAuth{who: foreign, scope: here}, func(context.Context, billing.MerchantID, billing.CustomerID, string, bool) error {
			return db.ErrCustomerIssuerMismatch
		}, "customer_issuer_mismatch"},
		"bound to the root scope":         {boundAuth{who: foreign, scope: authtest.Root}, recordIssuer, "merchant_unresolved"},
		"bound, without a root authority": {rootless{boundAuth{who: foreign, scope: here}}, recordIssuer, "merchant_unresolved"},
	} {
		got := customer(tc.auth, tc.admit)
		require.Equal(t, http.StatusForbidden, got.status, name)
		require.Equal(t, tc.code, got.code, name)
	}
}

// Application discovery works without any console areas and never attributes
// application-only authority to a human carrying the same permission.
func TestAccessProgrammaticOnly(t *testing.T) {
	rt := gatedRuntime(t)
	for _, who := range []billingauth.Identity{authtest.Application("backend"), authtest.User(userA)} {
		for _, groups := range []config.RouteGroups{{Programmatic: true}, {Admin: true}} {
			auth := &recordingAuth{who: who, allowed: map[billing.Permission]bool{}, at: map[billing.Permission]bool{billing.MerchantUsageRead: true, billing.MerchantOrdersManage: true, billing.MerchantCustomersRead: true}}
			table := &router.Table{}
			RegisterStaffRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: auth, Groups: groups, live: live})
			w := do(table.Handler(), http.MethodGet, "/v1/admin/access", nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var access billing.AdminAccess
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &access))
			require.Equal(t, billing.AccessNone, access.Admin)
			require.Equal(t, billing.AccessNone, access.Catalog)
			if groups.Programmatic && who.SubjectKind == billingauth.SubjectApplication {
				require.Equal(t, []billing.Permission{billing.MerchantOrdersManage, billing.MerchantUsageRead}, access.ApplicationPermissions, "network customer lookup is not mounted")
			} else {
				require.Empty(t, access.ApplicationPermissions)
			}
		}
	}
}

func TestReadPermissionsCannotWrite(t *testing.T) {
	for _, tc := range []struct {
		permission  billing.Permission
		read, write string
	}{
		{billing.MerchantConfigRead, "GET /v1/merchant/configuration", "PATCH /v1/merchant/configuration"},
		{billing.MerchantUsageRead, "GET /v1/app/running-charges", "POST /v1/app/running-charges"},
	} {
		t.Run(string(tc.permission), func(t *testing.T) {
			auth := &recordingAuth{who: authtest.Application("reader"), allowed: map[billing.Permission]bool{}, at: map[billing.Permission]bool{tc.permission: true}}
			rt := gatedRuntime(t)
			env := newEnv(rt, Options{Auth: auth, Groups: everyGroup, live: live})
			for _, key := range []string{tc.read, tc.write} {
				method, path, _ := strings.Cut(key, " ")
				route, ok := Lookup(method, path)
				require.True(t, ok)
				table := &router.Table{}
				router.NewMux(table, "", rt).Handle(method, path, func(r *httprequest.Request) { r.SuccessJSON(map[string]bool{"reached": true}) }, env.gates(route)...)
				w := do(table.Handler(), method, path, nil)
				if key == tc.read {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				} else {
					require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				}
			}
		})
	}
}
