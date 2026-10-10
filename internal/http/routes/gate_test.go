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
	auth "github.com/open-rails/helpers/auth"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/customerscope"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchant"
)

// openTiers are the tiers no Auth runs on: their credential is the request
// itself (a capability id or a provider signature) or there is none.
var openTiers = []Tier{AuthPublic, AuthSessionID, AuthProvider, AuthCheckoutSession}

// openRoutes is every route no Auth gates. A route that joins it is a
// reviewed change.
var openRoutes = []string{
	"GET /health/live", "GET /health/ready", "GET /v1/config",
	"GET /v1/captcha/client.js", "GET /v1/captcha/status",
	"GET /v1/catalog/products", "GET /v1/solana/tokens",
	"GET /v1/checkout-attempts/{id}/solana-pay", "POST /v1/checkout-attempts/{id}/solana-pay",
	"GET /v1/checkout-sessions/{id}", "POST /v1/checkout-sessions/{id}/pay",
	"POST /v1/webhooks/{rail}/{account_id}",
}

// sensitiveRoutes are the routes that also ask a person for a recent sign-in:
// those that move money or remove access. A change is reviewed.
var sensitiveRoutes = []string{
	"DELETE /v1/admin/alert-webhooks/{id}",
	"DELETE /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}",
	"DELETE /v1/admin/provisioning-tokens/{id}",
	"GET /v1/admin/billing-archive",
	"PATCH /v1/admin/alert-webhooks/{id}",
	"PATCH /v1/admin/catalog/prices/{id}",
	"PATCH /v1/admin/catalog/products/{id}",
	"PATCH /v1/admin/configuration",
	"PATCH /v1/admin/customers/{customer_id}",
	"PATCH /v1/admin/psps/{id}",
	"POST /v1/admin/alert-webhooks",
	"POST /v1/admin/billing-archive",
	"POST /v1/admin/billing-import",
	"POST /v1/admin/catalog/applications",
	"POST /v1/admin/catalog/prices",
	"POST /v1/admin/catalog/product-archives",
	"POST /v1/admin/catalog/products",
	"POST /v1/admin/checkout-sessions",
	"POST /v1/admin/credit-grants",
	"POST /v1/admin/credit-grants/{id}/revoke",
	"POST /v1/admin/findings/{id}/resolve",
	"POST /v1/admin/invoices/{id}/mark-uncollectible",
	"POST /v1/admin/invoices/{id}/retry-collection",
	"POST /v1/admin/invoices/{id}/void",
	"POST /v1/admin/payments",
	"POST /v1/admin/payments/{id}/refunds",
	"POST /v1/admin/price-migrations",
	"POST /v1/admin/price-migrations/{id}/cancel",
	"POST /v1/admin/product-access",
	"POST /v1/admin/product-access/{id}/revoke",
	"POST /v1/admin/provider-operations/{operation_id}/close",
	"POST /v1/admin/provisioning-tokens",
	"POST /v1/admin/psps",
	"POST /v1/admin/psps/refresh",
	"POST /v1/admin/subscriptions/{id}/cancel",
	"POST /v1/admin/subscriptions/{id}/change",
	"POST /v1/admin/subscriptions/{id}/resume",
	"PUT /v1/admin/catalog/meters/{key}",
	"PUT /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}",
	"PUT /v1/admin/subscriptions/{id}/payment-method",
}

// Every catalog route declares exactly one tier of the closed set, and what
// that tier checks: a staff route its group's permission, a customer route
// none, and no customer path names a customer.
func TestEveryRouteDeclaresOneTier(t *testing.T) {
	tiers := []Tier{AuthPublic, AuthCheckoutSession, AuthSessionID, AuthCustomer, AuthMerchant, AuthSignedIn, AuthApplication, AuthProvider, AuthProvisioning}
	var open, sensitive []string
	names := map[string]string{}
	for _, r := range Catalog() {
		key := r.Key()
		require.Contains(t, tiers, r.Auth, "%s declares no known tier", key)
		if slices.Contains(openTiers, r.Auth) {
			open = append(open, key)
		}
		if Sensitive(r) {
			sensitive = append(sensitive, key)
		}
		require.False(t, r.Sensitive && r.Auth != AuthMerchant, "%s: only a merchant route steps up", key)
		if r.ClientMethod() {
			require.NotEmpty(t, r.Name, "%s: a Client method calls it", key)
			require.Empty(t, names[r.Name], "%s: %s already names %s", key, r.Name, names[r.Name])
			names[r.Name] = key
		} else {
			require.Empty(t, r.Name, key)
		}
		if r.Staff() {
			require.Equal(t, AuthMerchant, r.Auth, key)
			if r.Group == Admin || r.Group == CatalogAdmin {
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
			require.False(t, Sensitive(r), "%s: customer self-service never steps up", key)
			for _, param := range []string{"{customer_id}", "{subject}", "{user_id}"} {
				require.NotContains(t, r.Path, param, "%s: a customer route takes its customer from the gate only", key)
			}
		}
	}
	sort.Strings(open)
	want := slices.Clone(openRoutes)
	sort.Strings(want)
	require.Equal(t, want, open, "the routes no Auth gates")
	sort.Strings(sensitive)
	require.Equal(t, sensitiveRoutes, sensitive, "the routes that step up")
}

// staffPermissions gives each group and programmatic task a permission
// naming it.
var staffPermissions = Permissions{
	AdminRead: "root:billing:read", AdminUpdate: "root:billing:manage", Catalog: "root:catalog:manage", MerchantConfig: "root:config:manage", Metrics: "root:metrics:read",
	Entitlements: "root:entitlements:read", Offers: "root:catalog:read", Usage: "root:usage:manage", Costs: "root:costs:manage", Events: "root:events:read",
}

// testScope is where recordingAuth's subjects hold their permissions.
var testScope = FixedScope(authtest.Scope)

// recordingAuth says every request is who, records each call the gate
// makes, and grants what allowed names (every permission when nil) in
// authtest.Scope.
type recordingAuth struct {
	mu      sync.Mutex
	calls   []string
	who     billingauth.Identity
	allowed map[string]bool
	// authErr, canErr and signInErr are the answers of Authenticate, Can and
	// CheckRecentSignIn; noSignIn drops CheckRecentSignIn.
	authErr, canErr, signInErr error
	noSignIn                   bool
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
	return scope == authtest.Scope && (v.a.allowed == nil || v.a.allowed[perm]), nil
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

// gatedRuntime is a database-free runtime bound to merchantA.
func gatedRuntime(t *testing.T) *app.Runtime {
	t.Helper()
	rt := &app.Runtime{Config: &config.Config{Vault: &config.VaultConfig{KVMount: "kv"}}}
	rt.SetConfiguredMerchant(merchantA)
	return rt
}

// everyGatedSurface mounts every group an embedded host can publish, its
// subjects saying who they are through a.
func everyGatedSurface(t *testing.T, a billingauth.Authenticator) (*router.Table, *app.Runtime) {
	rt := gatedRuntime(t)
	providers := routesurface.AllProviderRoutes()
	table := &router.Table{}
	RegisterUserRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, ProviderRoutes: &providers})
	RegisterStaffRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, Scope: testScope, Permissions: staffPermissions})
	RegisterAppRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, Scope: testScope, Permissions: staffPermissions})
	RegisterCustomerRoutes(router.NewMux(table, "/v1/me", rt), rt, CustomerMount{Auth: a, Providers: providers})
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
// request is on every gated route; a staff route's permission in the
// mount's Scope, and an application's on a programmatic route; a person's
// recent sign-in on a route that moves money or removes access, never an
// application's; the access read each mounted group's permission; nothing
// on an open route.
func TestGatesAskTheVerified(t *testing.T) {
	for _, who := range []billingauth.Identity{authtest.User(userA), authtest.Application(userA)} {
		rec := &recordingAuth{who: who}
		table, _ := everyGatedSurface(t, rec)
		h := table.Handler()
		mounted := map[string]bool{}
		for _, key := range routeKeys(table) {
			mounted[key] = true
		}
		checked, sensitive := 0, 0
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
					want = append(want, "Can:"+staffPermissions.For(r))
				}
			case AuthSignedIn:
				want = []string{"Authenticate"}
				for _, perm := range []string{staffPermissions.AdminRead, staffPermissions.AdminUpdate, staffPermissions.Catalog, staffPermissions.MerchantConfig, staffPermissions.Metrics} {
					want = append(want, "Can:"+perm)
				}
			case AuthMerchant:
				want = []string{"Authenticate", "Can:" + staffPermissions.For(r)}
				if Sensitive(r) && who.SubjectKind == billingauth.SubjectUser {
					want = append(want, "CheckRecentSignIn")
					sensitive++
				}
			}
			req := httptest.NewRequest(r.Method, filled(r.Path), strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer any")
			serveSafely(h, req)
			require.Equal(t, want, rec.take(), "%s as %s", r.Key(), who.SubjectKind)
			checked++
		}
		require.NotZero(t, checked)
		if who.SubjectKind == billingauth.SubjectUser {
			require.NotZero(t, sensitive)
		}
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
	env := newEnv(rt, Options{Auth: authtest.Deny{}, Scope: testScope, Permissions: staffPermissions})
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
// as the customer the request acts for, at the mount's merchant; a service
// or a non-UUID subject is no customer.
func TestCustomerGate(t *testing.T) {
	rt := gatedRuntime(t)
	run := func(who billingauth.Identity, header map[string]string) (int, customerscope.Scope) {
		table := &router.Table{}
		var seen customerscope.Scope
		mux := router.NewMux(table, "/v1/me", rt)
		env := newEnv(rt, Options{})
		env.Customers = &recordingAuth{who: who}
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
		"another merchant's selector":    {user, map[string]string{merchant.SelectorHeader: "id:" + merchantB.String()}, http.StatusConflict},
	} {
		code, _ := run(tc.who, tc.header)
		require.Equal(t, tc.status, code, name)
	}
}

// refusal is what a gate answered.
type refusal struct {
	status    int
	code      string
	challenge string
	metadata  map[string]any
	staff     billingauth.Staff
}

// runRoute serves key behind its gates, with a and the test scope.
func runRoute(t *testing.T, rt *app.Runtime, a billingauth.Authenticator, key string, header map[string]string) refusal {
	t.Helper()
	method, path, _ := strings.Cut(key, " ")
	route, ok := Lookup(method, path)
	require.True(t, ok, key)
	env := newEnv(rt, Options{Auth: a, Scope: testScope, Permissions: staffPermissions})
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
	out.status, out.challenge = rec.Code, rec.Header().Get("WWW-Authenticate")
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
// permission in the mount's scope, at the mount's merchant; a person on an
// operation that moves money also proves a recent sign-in, whatever
// credential it presents. An application passes on its permission alone.
func TestStaffGate(t *testing.T) {
	rt := gatedRuntime(t)
	person := authtest.User(userA)
	service := authtest.Application("svc_1")
	personalKey := person
	personalKey.Credential = billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "pk_1"}

	got := runRoute(t, rt, &recordingAuth{who: person}, refund, nil)
	require.Equal(t, http.StatusNoContent, got.status, got.code)
	require.Equal(t, billingauth.Staff{Identity: person, Route: refund, Merchant: merchantA}, got.staff)

	// A checkout session is an ordinary staff write: whoever creates it, its
	// customer pays it, and a saved card needs that customer's own proof.
	for _, who := range []billingauth.Identity{person, service, personalKey} {
		got = runRoute(t, rt, &recordingAuth{who: who}, "POST /v1/admin/checkout-sessions", nil)
		require.Equal(t, http.StatusNoContent, got.status, who.Credential.Kind)
		require.Equal(t, "POST /v1/admin/checkout-sessions", got.staff.Route)
	}

	stepUp := &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute, Metadata: map[string]any{"step_up_methods": []any{"password"}}}
	revoked := errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)
	for name, tc := range map[string]struct {
		auth      billingauth.Authenticator
		header    map[string]string
		status    int
		code      string
		challenge string
	}{
		"no credential":                       {authtest.Deny{}, map[string]string{"Authorization": ""}, 401, "authentication_required", "Bearer"},
		"a refused credential":                {authtest.Deny{}, nil, 401, "authentication_required", `Bearer error="invalid_token"`},
		"an expired credential":               {&recordingAuth{authErr: errors.Join(auth.ErrUnauthenticated, auth.ErrExpired)}, nil, 401, "credential_expired", `Bearer error="invalid_token"`},
		"a revoked credential":                {&recordingAuth{authErr: revoked}, nil, 401, "credential_revoked", `Bearer error="invalid_token"`},
		"a missing sender proof":              {&recordingAuth{authErr: errors.Join(auth.ErrUnauthenticated, auth.ErrSenderProofRequired)}, nil, 401, "sender_proof_required", `DPoP error="invalid_dpop_proof"`},
		"a provider's own challenge":          {&recordingAuth{authErr: &auth.Challenge{Err: auth.ErrSenderProofRequired, Header: http.Header{"Www-Authenticate": {`DPoP error="use_dpop_nonce"`}}}}, nil, 401, "sender_proof_required", `DPoP error="use_dpop_nonce"`},
		"a credential refused here":           {&recordingAuth{authErr: auth.ErrForbidden}, nil, 403, "permission_required", ""},
		"auth unavailable":                    {&recordingAuth{authErr: auth.ErrUnavailable}, nil, 503, "authentication_unavailable", ""},
		"an unclassified error":               {&recordingAuth{authErr: errors.New("boom")}, nil, 503, "authentication_unavailable", ""},
		"a panicking Authenticate":            {panicking{}, nil, 503, "authentication_unavailable", ""},
		"a panicking Identity":                {panicking{identity: true}, nil, 503, "authentication_unavailable", ""},
		"neither a Verified nor an error":     {nothing{}, nil, 503, "authentication_unavailable", ""},
		"an unknown subject kind":             {&recordingAuth{who: withKind(person, "")}, nil, 403, "permission_required", ""},
		"no invoker":                          {&recordingAuth{who: withInvoker(person, billingauth.Invoker{})}, nil, 401, "authentication_required", `Bearer error="invalid_token"`},
		"another merchant's selector":         {&recordingAuth{who: person}, map[string]string{merchant.SelectorHeader: "id:" + merchantB.String()}, 409, "merchant_binding_mismatch", ""},
		"a permission not held":               {&recordingAuth{who: person, allowed: map[string]bool{}}, nil, 403, "permission_required", ""},
		"a Can whose sign-in was revoked":     {&recordingAuth{who: person, canErr: revoked}, nil, 401, "credential_revoked", `Bearer error="invalid_token"`},
		"a Can that could not run":            {&recordingAuth{who: person, canErr: errors.New("db")}, nil, 503, "authorization_unavailable", ""},
		"a Can that refuses with an error":    {&recordingAuth{who: person, canErr: auth.ErrForbidden}, nil, 503, "authorization_unavailable", ""},
		"a stale sign-in":                     {&recordingAuth{who: person, signInErr: stepUp}, nil, 401, "step_up_required", `Bearer error="insufficient_user_authentication", max_age="900"`},
		"no sign-in of its own":               {&recordingAuth{who: person, signInErr: auth.ErrForbidden}, nil, 403, "step_up_unavailable", ""},
		"a person who cannot prove a sign-in": {&recordingAuth{who: person, noSignIn: true}, nil, 403, "step_up_unavailable", ""},
		"a person's API key":                  {&recordingAuth{who: personalKey, noSignIn: true}, nil, 403, "step_up_unavailable", ""},
		"a sign-in revoked since":             {&recordingAuth{who: person, signInErr: revoked}, nil, 401, "credential_revoked", `Bearer error="invalid_token"`},
		"a sign-in check that could not run":  {&recordingAuth{who: person, signInErr: auth.ErrUnavailable}, nil, 503, "authentication_unavailable", ""},
	} {
		got := runRoute(t, rt, tc.auth, refund, tc.header)
		require.Equal(t, tc.status, got.status, "%s: %s", name, got.code)
		require.Equal(t, tc.code, got.code, name)
		require.Equal(t, tc.challenge, got.challenge, name)
	}
	got = runRoute(t, rt, &recordingAuth{who: person, signInErr: stepUp}, refund, nil)
	require.Equal(t, map[string]any{"step_up_methods": []any{"password"}}, got.metadata, "the provider's challenge reaches its client")
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
	p := staffPermissions
	got = runRoute(t, rt, &recordingAuth{who: service, allowed: map[string]bool{p.Usage: true, p.Costs: true, p.Entitlements: true}}, "GET /v1/app/host-events", nil)
	require.Equal(t, http.StatusForbidden, got.status, "every permission but Events")
	require.Equal(t, "permission_required", got.code)
	got = runRoute(t, rt, &recordingAuth{who: service, allowed: map[string]bool{p.Events: true}}, "GET /v1/app/host-events", nil)
	require.Equal(t, http.StatusNoContent, got.status, "Events alone")
	require.Equal(t, "GET /v1/app/host-events", got.staff.Route)
	// Its writes run once per Idempotency-Key.
	got = runRoute(t, rt, &recordingAuth{who: service}, "POST /v1/app/host-events/acknowledge", nil)
	require.Equal(t, http.StatusBadRequest, got.status, "a programmatic write without an Idempotency-Key")

	unbound := gatedRuntime(t)
	unbound.SetConfiguredMerchant(billing.MerchantID{})
	got = runRoute(t, unbound, &recordingAuth{who: person}, "GET /v1/admin/payments", nil)
	require.Equal(t, http.StatusForbidden, got.status, "no configured merchant, no admin API")
}

// panicking is a broken provider.
type panicking struct{ identity bool }

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

// Mount refuses a route it cannot gate: nothing is mounted open.
func TestMountFailsClosed(t *testing.T) {
	rt := gatedRuntime(t)
	for name, mount := range map[string]func(){
		"admin without Auth": func() {
			RegisterStaffRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Scope: testScope, Permissions: staffPermissions})
		},
		"admin with a typed nil Auth": func() {
			RegisterStaffRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: (*authtest.Fake)(nil), Scope: testScope, Permissions: staffPermissions})
		},
		"admin without Scope": func() {
			RegisterStaffRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: authtest.Deny{}, Permissions: staffPermissions})
		},
		"a blank permission": func() {
			RegisterStaffRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: authtest.Deny{}, Scope: testScope, Permissions: Permissions{MerchantConfig: " "}})
		},
		"customers without Auth": func() {
			RegisterCustomerRoutes(router.NewMux(&router.Table{}, "/v1/me", rt), rt, CustomerMount{})
		},
		"programmatic without Auth": func() {
			RegisterAppRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Scope: testScope, Permissions: staffPermissions})
		},
		"programmatic without Scope": func() {
			RegisterAppRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: authtest.Deny{}, Permissions: Permissions{Usage: "root:usage:manage"}})
		},
		"a blank programmatic permission": func() {
			RegisterAppRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: authtest.Deny{}, Scope: testScope, Permissions: Permissions{Events: " "}})
		},
	} {
		require.PanicsWithError(t, mountErrorFor(t, mount), mount, name)
	}
	// Without a permission, no staff route is mounted at all.
	none := &router.Table{}
	RegisterStaffRoutes(router.NewMux(none, "/v1", rt), rt, Options{Auth: authtest.Deny{}})
	require.Empty(t, none.Entries)
	// A programmatic route mounts only with its permission; SCIM names none.
	app := &router.Table{}
	RegisterAppRoutes(router.NewMux(app, "/v1", rt), rt, Options{Auth: authtest.Deny{}, Scope: testScope, Permissions: Permissions{Usage: "root:usage:manage"}})
	for _, r := range Catalog() {
		if r.Group != App {
			continue
		}
		mounted := slices.ContainsFunc(app.Entries, func(e router.Entry) bool { return e.Method == r.Method && e.Path == r.Path })
		require.Equal(t, r.Permission == NeedUsage || r.Auth == AuthProvisioning, mounted, r.Key())
	}
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

// The checkout viewer shows saved cards only to the session's own customer:
// a presented credential the Authenticator admits as a person in person
// binds that customer; a refused or absent one, or an application, leaves
// the capability alone.
func TestCheckoutViewer(t *testing.T) {
	route, _ := Lookup(GET, "/v1/checkout-sessions/{id}")
	run := func(a billingauth.Authenticator, authorization string) (bool, customerscope.Scope) {
		env := newEnv(gatedRuntime(t), Options{Auth: a})
		var scope customerscope.Scope
		var bound, reached bool
		h := env.checkoutViewer(route)(func(r *httprequest.Request) {
			reached = true
			scope, bound = r.CustomerScope()
		})
		req := httptest.NewRequest(GET, "/v1/checkout-sessions/ocs_x", nil)
		req = req.WithContext(merchant.WithID(req.Context(), merchantA))
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		h(httprequest.NewHTTP(httptest.NewRecorder(), req, nil))
		require.True(t, reached, "the capability is never refused here")
		return bound, scope
	}
	fake := &authtest.Fake{}
	bound, scope := run(fake, "Bearer "+fake.Person(userB))
	require.True(t, bound)
	require.Equal(t, userB, scope.Customer().String())
	for name, authorization := range map[string]string{"anonymous": "", "refused": "Bearer forged", "a service": "Bearer " + fake.Machine("svc_1")} {
		bound, _ = run(fake, authorization)
		require.False(t, bound, name)
	}
	bound, _ = run(nil, "Bearer "+fake.Person(userB))
	require.False(t, bound, "a mount without Auth shows no saved cards")
	bound, _ = run(panicking{}, "Bearer any")
	require.False(t, bound, "a panicking provider shows no saved cards")
}

// Only the route gate makes a customer scope or binds a verdict.
func TestBindersHaveOneSite(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	calls := map[string][]string{}
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
			case "customerscope.Bind", "billingauth.BindIdentity", "billingauth.BindStaff", "billingauth.BindMerchant", "billingauth.BindVerified":
				rel, _ := filepath.Rel(root, path)
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
		"billingauth.BindMerchant": {gate},
		"billingauth.BindVerified": {gate},
	}, calls)
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
// caller holds: none, read or update for customer support, and whether the
// catalog, merchant config and metrics are theirs.
func TestAccessAnswersWhatTheCallerHolds(t *testing.T) {
	rt := gatedRuntime(t)
	read := func(perms Permissions, allowed ...string) billing.AdminAccess {
		gate := &recordingAuth{who: authtest.User(userA), allowed: map[string]bool{}}
		for _, perm := range allowed {
			gate.allowed[perm] = true
		}
		table := &router.Table{}
		RegisterStaffRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: gate, Scope: testScope, Permissions: perms})
		rec := do(table.Handler(), http.MethodGet, "/v1/admin/access", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var access billing.AdminAccess
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &access))
		return access
	}
	p := staffPermissions
	none := billing.AdminAccess{Admin: billing.AccessNone}
	require.Equal(t, none, read(p), "a caller holding nothing")
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessRead}, read(p, p.AdminRead))
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessUpdate, Metrics: true}, read(p, p.AdminRead, p.AdminUpdate, p.Metrics))
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessNone, Catalog: true, MerchantConfig: true}, read(p, p.Catalog, p.MerchantConfig))
	require.Equal(t, none, read(Permissions{MerchantConfig: p.MerchantConfig}, p.AdminRead, p.Metrics), "a group that is off is no one's")
	require.Equal(t, none, read(Permissions{AdminRead: p.AdminRead}, p.AdminUpdate), "an update without the read is nothing")
}
