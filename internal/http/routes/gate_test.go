package routes

import (
	"context"
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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/customerscope"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// openTiers are the tiers no Auth runs on: their credential is the request
// itself (a capability id or a provider signature) or there is none.
var openTiers = []Tier{AuthPublic, AuthSessionID, AuthProvider, AuthCheckoutSession}

// openRoutes is every route no Auth gates. A route that joins it is a
// reviewed change.
var openRoutes = []string{
	"GET /health/live", "GET /health/ready", "GET /metrics", "GET /v1/config",
	"GET /v1/captcha/client.js", "GET /v1/captcha/status",
	"GET /v1/products", "GET /v1/solana/tokens",
	"GET /v1/checkout-attempts/{id}/solana-pay", "POST /v1/checkout-attempts/{id}/solana-pay",
	"GET /v1/checkout-sessions/{id}", "POST /v1/checkout-sessions/{id}/pay",
	"POST /v1/webhooks/{rail}/{account_id}",
}

// sensitiveRoutes are the routes that also ask Auth.Sensitive of a user in
// person: those that move money or remove access. A change is reviewed.
var sensitiveRoutes = []string{
	"DELETE /v1/merchant/alert-webhooks/{id}",
	"DELETE /v1/merchant/api-keys/{id}",
	"DELETE /v1/merchant/catalog/meters/{key}/rate-card",
	"DELETE /v1/merchant/customers/{customer_id}/payment-methods/{id}",
	"DELETE /v1/merchant/customers/{customer_id}/product-access/{id}",
	"DELETE /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}",
	"DELETE /v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}",
	"DELETE /v1/merchant/federated-grants/{id}",
	"DELETE /v1/merchant/team/invites/{id}",
	"DELETE /v1/merchant/team/{user_id}",
	"GET /v1/merchant/api-keys",
	"GET /v1/merchant/billing-archive",
	"PATCH /v1/merchant/catalog/prices/{id}",
	"PATCH /v1/merchant/catalog/products/{id}",
	"PATCH /v1/merchant/customers/settings",
	"PATCH /v1/merchant/psps/{id}",
	"PATCH /v1/merchant/team/{user_id}",
	"POST /v1/merchant/admissions",
	"POST /v1/merchant/admissions/extend",
	"POST /v1/merchant/admissions/release",
	"POST /v1/merchant/admissions/{request_id}/capture",
	"POST /v1/merchant/alert-webhooks",
	"POST /v1/merchant/api-host/verify",
	"POST /v1/merchant/api-keys",
	"POST /v1/merchant/billing-archive",
	"POST /v1/merchant/billing-import",
	"POST /v1/merchant/catalog/applications",
	"POST /v1/merchant/catalog/drift/refresh",
	"POST /v1/merchant/catalog/entitlement-replacements",
	"POST /v1/merchant/catalog/prices",
	"POST /v1/merchant/catalog/product-archives",
	"POST /v1/merchant/catalog/products",
	"POST /v1/merchant/checkout-sessions",
	"POST /v1/merchant/configuration/applications",
	"POST /v1/merchant/credit-grants",
	"POST /v1/merchant/customers/ensure",
	"POST /v1/merchant/customers/{customer_id}/credit-grants/{id}/revoke",
	"POST /v1/merchant/customers/{customer_id}/payments/off-channel",
	"POST /v1/merchant/federated-grants",
	"POST /v1/merchant/findings/{id}/resolve",
	"POST /v1/merchant/invoices/{id}/payments",
	"POST /v1/merchant/invoices/{id}/retry-collection",
	"POST /v1/merchant/invoices/{id}/uncollectible",
	"POST /v1/merchant/invoices/{id}/void",
	"POST /v1/merchant/payments/{id}/refunds",
	"POST /v1/merchant/plan-migrations",
	"POST /v1/merchant/product-access",
	"POST /v1/merchant/provider-operations",
	"POST /v1/merchant/provider-operations/{operation_id}/close",
	"POST /v1/merchant/provider-operations/{operation_id}/extend",
	"POST /v1/merchant/provider-operations/{operation_id}/observations",
	"POST /v1/merchant/provider-operations/{operation_id}/refusal",
	"POST /v1/merchant/provider-operations/{operation_id}/release",
	"POST /v1/merchant/provider-operations/{operation_id}/resolution",
	"POST /v1/merchant/psps",
	"POST /v1/merchant/psps/refresh",
	"POST /v1/merchant/psps/{id}/archive",
	"POST /v1/merchant/reprice-batches",
	"POST /v1/merchant/reprice-batches/{id}/cancel",
	"POST /v1/merchant/reprices/{id}/cancel",
	"POST /v1/merchant/subscriptions/{id}/cancel",
	"POST /v1/merchant/subscriptions/{id}/change-tier",
	"POST /v1/merchant/subscriptions/{id}/change-tier/preview",
	"POST /v1/merchant/subscriptions/{id}/resume",
	"POST /v1/merchant/team/invites",
	"POST /v1/merchant/usage-events",
	"POST /v1/merchant/wasted-spend",
	"PUT /v1/merchant/alert-webhooks/{id}/url",
	"PUT /v1/merchant/api-host",
	"PUT /v1/merchant/catalog/meters/{key}",
	"PUT /v1/merchant/catalog/meters/{key}/rate-card",
	"PUT /v1/merchant/catalog/products/by-key/{product_key}",
	"PUT /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}",
	"PUT /v1/merchant/customers/{customer_id}/spend-delegations",
	"PUT /v1/merchant/name",
	"PUT /v1/merchant/subscriptions/{id}/payment-method",
}

// Every catalog route declares exactly one tier of the closed set, and what
// that tier checks: a staff route its guard groups, a control-plane route one
// exact permission, a customer route none, and no customer path names a
// customer.
func TestEveryRouteDeclaresOneTier(t *testing.T) {
	tiers := []Tier{AuthPublic, AuthCheckoutSession, AuthSessionID, AuthUser, AuthCustomer, AuthMerchant, AuthOperator, AuthProvider}
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
		if r.Staff() {
			require.Equal(t, AuthMerchant, r.Auth, key)
			require.Empty(t, r.Perm, "%s: a staff route's permission is the host's guard", key)
			require.NotEmpty(t, r.Name, key)
			require.Empty(t, names[r.Name], "%s: %s already names %s", key, r.Name, names[r.Name])
			names[r.Name] = key
			require.NotEmpty(t, r.Resources, key)
			for _, res := range r.Resources {
				require.Contains(t, Resources, res, key)
			}
			if r.Group == MerchantConfig {
				require.Equal(t, LevelAdmin, r.Level, "%s: every configuration route is MerchantConfig's", key)
			} else {
				require.Contains(t, []Level{LevelRead, LevelWrite}, r.Level, key)
			}
		} else {
			require.Empty(t, r.Name, key)
			require.Empty(t, r.Level, key)
			require.Empty(t, r.Resources, key)
		}
		switch r.Auth {
		case AuthMerchant:
			if !r.Staff() {
				require.NotEmpty(t, r.Perm, key)
				require.NotContains(t, r.Perm, "*", "%s: a permission is exact, never a glob", key)
			}
		case AuthCustomer:
			require.Empty(t, r.Perm, key)
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
	for _, g := range GuardNames() {
		require.NotEmpty(t, g.Routes, "%s covers no route", g.Name)
	}
}

// staffGuards guards each staff route with a permission naming its level.
var staffGuards = map[GuardKey]string{StaffReadsKey: "staff:read", StaffWritesKey: "staff:write", MerchantConfigKey: "staff:admin"}

// levelGuard is staffGuards resolved over every staff route.
func levelGuard(t *testing.T) func(Route) string {
	t.Helper()
	var all []Route
	for _, r := range Catalog() {
		if r.Staff() {
			all = append(all, r)
		}
	}
	guard, err := ResolveGuards(all, staffGuards)
	require.NoError(t, err)
	return guard
}

// recordingAuth records which middleware ran, in order, and admits who.
type recordingAuth struct {
	mu    sync.Mutex
	calls []string
	who   billingauth.Identity
}

type admittedKey struct{}

func (a *recordingAuth) record(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			a.calls = append(a.calls, name)
			a.mu.Unlock()
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), admittedKey{}, true)))
		})
	}
}
func (a *recordingAuth) Required() func(http.Handler) http.Handler { return a.record("Required") }
func (a *recordingAuth) RequirePermission(p string) func(http.Handler) http.Handler {
	return a.record("RequirePermission:" + p)
}
func (a *recordingAuth) Sensitive() func(http.Handler) http.Handler { return a.record("Sensitive") }
func (a *recordingAuth) Identity(ctx context.Context) (billingauth.Identity, bool) {
	return a.who, ctx.Value(admittedKey{}) != nil
}
func (a *recordingAuth) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.calls
	a.calls = nil
	return out
}

// gatedRuntime is a database-free runtime bound to merchantA with catalog
// edits open.
func gatedRuntime(t *testing.T) *app.Runtime {
	open := &catalogpolicy.Exposure{}
	require.NoError(t, open.Decide(true))
	rt := &app.Runtime{Config: &config.Config{}, CatalogEdits: open}
	rt.SetConfiguredMerchant(merchantA)
	return rt
}

// everyGatedSurface mounts every group an embedded host can publish, gated
// by a.
func everyGatedSurface(t *testing.T, a billingauth.Auth) (*router.Table, *app.Runtime) {
	rt := gatedRuntime(t)
	providers := routesurface.AllProviderRoutes()
	table := &router.Table{}
	RegisterUserRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, ProviderRoutes: &providers})
	RegisterMerchantRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, Guard: levelGuard(t)}, Merchant, MerchantConfig)
	RegisterSelfServiceRoutes(router.NewMux(table, "/v1/me", rt), rt, CustomerMount{Auth: a, Providers: providers})
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

// Mount stacks exactly the tier's middleware on each route, in AuthKit's
// order: Required on a customer route; RequirePermission for the route's
// guard and, for one that moves money or removes access by a user in person,
// Sensitive on a staff route; nothing on an open route.
func TestMountComposesTierMiddleware(t *testing.T) {
	for _, who := range []billingauth.Identity{authtest.User(userA), authtest.Application(userA)} {
		rec := &recordingAuth{who: who}
		table, _ := everyGatedSurface(t, rec)
		h := table.Handler()
		mounted := map[string]bool{}
		for _, key := range routeKeys(table) {
			mounted[key] = true
		}
		checked, sensitive := 0, 0
		guard := levelGuard(t)
		for _, r := range Catalog() {
			if !mounted[r.Key()] || r.Group == ControlPlane || r.Group == Platform {
				continue
			}
			var want []string
			switch r.Auth {
			case AuthCustomer:
				want = []string{"Required"}
			case AuthMerchant:
				want = []string{"RequirePermission:" + guard(r)}
				if Sensitive(r) && billingauth.Interactive(who) {
					want = append(want, "Sensitive")
					sensitive++
				}
			}
			req := httptest.NewRequest(r.Method, filled(r.Path), strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer any")
			serveSafely(h, req)
			require.Equal(t, want, rec.take(), "%s as %s", r.Key(), who.SubjectKind)
			checked++
		}
		require.Greater(t, checked, 180)
		if billingauth.Interactive(who) {
			require.Greater(t, sensitive, 20)
		}
	}
}

// A pass-through Auth, whose middleware checks nothing and admits no identity,
// gets no route past OpenRails' own check: every gated route is refused
// before its handler.
func TestPassThroughAuthIsRefusedEverywhere(t *testing.T) {
	table, _ := everyGatedSurface(t, authtest.PassThrough{})
	h := table.Handler()
	gated := 0
	for _, r := range Catalog() {
		if r.Auth != AuthCustomer && r.Auth != AuthMerchant || !r.Staff() && r.Group != Customer {
			continue
		}
		req := httptest.NewRequest(r.Method, filled(r.Path), strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer forged")
		code := serveSafely(h, req)
		if r.CatalogWrite || code == http.StatusNotFound {
			// Unmounted for this configuration (a feature it lacks).
			if code == http.StatusNotFound {
				continue
			}
		}
		require.Equal(t, http.StatusUnauthorized, code, r.Key())
		gated++
	}
	require.Greater(t, gated, 170)
}

// Every gated handler re-checks the verdict the gate bound: reached without
// it, by any path, it answers 401 and does not run.
func TestHandlersRecheckTheirVerdict(t *testing.T) {
	rt := gatedRuntime(t)
	env := newEnv(rt, Options{Auth: authtest.Deny{}, Guard: levelGuard(t)})
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
				billingauth.Staff{Identity: authtest.User(userA), Route: "GET /v1/merchant/other", Merchant: merchantA}),
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
		a := &recordingAuth{who: who}
		table := &router.Table{}
		var seen customerscope.Scope
		mux := router.NewMux(table, "/v1/me", rt)
		env := newEnv(rt, Options{})
		env.Customers = a
		route, _ := Lookup(GET, "/v1/me/balance")
		mux.Handle(GET, "/balance", func(r *httprequest.Request) {
			seen, _ = r.CustomerScope()
			r.NoContent()
		}, env.gates(route)...)
		req := httptest.NewRequest(GET, "/v1/me/balance", nil)
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

// The staff gate refuses what the host's middleware admitted without an
// identity and anything at another merchant.
func TestStaffGate(t *testing.T) {
	rt := gatedRuntime(t)
	run := func(a billingauth.Auth, key string, header map[string]string) (int, billingauth.Staff) {
		method, path, _ := strings.Cut(key, " ")
		route, ok := Lookup(method, path)
		require.True(t, ok, key)
		env := newEnv(rt, Options{Auth: a, Guard: levelGuard(t)})
		table := &router.Table{}
		var seen billingauth.Staff
		router.NewMux(table, "", rt).Handle(route.Method, route.Path, func(r *httprequest.Request) {
			seen, _ = r.Staff()
			r.NoContent()
		}, env.gates(route)...)
		req := httptest.NewRequest(route.Method, filled(route.Path), strings.NewReader("{}"))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		return serveSafely(table.Handler(), req), seen
	}
	person := authtest.User(userA)
	service := authtest.Application("svc_1")
	personalKey := person
	personalKey.Credential = billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "pk_1"}

	code, staff := run(&recordingAuth{who: person}, "POST /v1/merchant/payments/{id}/refunds", nil)
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, billingauth.Staff{Identity: person, Route: "POST /v1/merchant/payments/{id}/refunds", Merchant: merchantA}, staff)

	// A checkout session is an ordinary staff write: whoever creates it, its
	// customer pays it, and a saved card needs that customer's own proof.
	for _, who := range []billingauth.Identity{person, service, personalKey} {
		code, staff = run(&recordingAuth{who: who}, "POST /v1/merchant/checkout-sessions", nil)
		require.Equal(t, http.StatusNoContent, code, who.Credential.Kind)
		require.Equal(t, "POST /v1/merchant/checkout-sessions", staff.Route)
	}

	for name, tc := range map[string]struct {
		auth   billingauth.Auth
		header map[string]string
		status int
	}{
		"admitted without an identity":   {authtest.PassThrough{}, nil, http.StatusUnauthorized},
		"an unknown subject kind":        {&recordingAuth{who: withKind(person, "")}, nil, http.StatusForbidden},
		"no invoker":                     {&recordingAuth{who: withInvoker(person, billingauth.Invoker{})}, nil, http.StatusUnauthorized},
		"another merchant's selector":    {&recordingAuth{who: person}, map[string]string{merchant.SelectorHeader: "id:" + merchantB.String()}, http.StatusConflict},
		"a refusing RequirePermission":   {&refusingPermission{recordingAuth{who: person}}, nil, http.StatusForbidden},
		"a panicking Identity":           {panickingIdentity{}, nil, http.StatusUnauthorized},
		"a refusing Sensitive (step-up)": {&staleAuth{recordingAuth{who: person}}, nil, http.StatusForbidden},
	} {
		code, _ := run(tc.auth, "POST /v1/merchant/payments/{id}/refunds", tc.header)
		require.Equal(t, tc.status, code, name)
	}
	code, _ = run(&staleAuth{recordingAuth{who: service}}, "POST /v1/merchant/payments/{id}/refunds", nil)
	require.Equal(t, http.StatusNoContent, code, "automation has no sign-in to renew")
	code, _ = run(&staleAuth{recordingAuth{who: personalKey}}, "POST /v1/merchant/payments/{id}/refunds", nil)
	require.Equal(t, http.StatusNoContent, code, "a user's API key automates the account")
	unbound := gatedRuntime(t)
	unbound.SetConfiguredMerchant(billing.MerchantID{})
	env := newEnv(unbound, Options{Auth: &recordingAuth{who: person}, Guard: levelGuard(t)})
	route, _ := Lookup(GET, "/v1/merchant/payments")
	table := &router.Table{}
	router.NewMux(table, "", unbound).Handle(route.Method, route.Path, func(r *httprequest.Request) { r.NoContent() }, env.gates(route)...)
	require.Equal(t, http.StatusForbidden, serveSafely(table.Handler(), httptest.NewRequest(GET, "/v1/merchant/payments", nil)), "no configured merchant, no merchant API")
}

type refusingPermission struct{ recordingAuth }

func (*refusingPermission) RequirePermission(string) func(http.Handler) http.Handler {
	fake := &authtest.Fake{}
	token := fake.Person(userA)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+token)
			fake.RequirePermission("anything")(next).ServeHTTP(w, r)
		})
	}
}

type staleAuth struct{ recordingAuth }

func (*staleAuth) Sensitive() func(http.Handler) http.Handler {
	return (&authtest.Fake{}).Sensitive()
}

type panickingIdentity struct{ authtest.PassThrough }

func (panickingIdentity) Identity(context.Context) (billingauth.Identity, bool) {
	panic("broken provider")
}

// Mount refuses a route the Auth gives no middleware for: nothing is
// mounted open.
func TestMountFailsClosed(t *testing.T) {
	rt := gatedRuntime(t)
	for name, mount := range map[string]func(){
		"merchant without Auth": func() {
			RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Guard: levelGuard(t)}, Merchant)
		},
		"merchant with a typed nil Auth": func() {
			RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: (*authtest.Fake)(nil), Guard: levelGuard(t)}, Merchant)
		},
		"a staff route without a guard": func() {
			RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: authtest.Deny{}}, Merchant)
		},
		"a guard naming no permission": func() {
			RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: authtest.Deny{}, Guard: func(Route) string { return " " }}, MerchantConfig)
		},
		"customers without Auth": func() {
			RegisterSelfServiceRoutes(router.NewMux(&router.Table{}, "/v1/me", rt), rt, CustomerMount{})
		},
		"a nil RequirePermission": func() {
			RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: nilPermission{}, Guard: levelGuard(t)}, Merchant)
		},
	} {
		require.PanicsWithError(t, mountErrorFor(t, mount), mount, name)
	}
}

type nilPermission struct{ authtest.Deny }

func (nilPermission) RequirePermission(string) func(http.Handler) http.Handler { return nil }

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
// a presented credential the Auth admits binds that customer; a refused or
// absent one leaves the capability alone.
func TestCheckoutViewer(t *testing.T) {
	route, _ := Lookup(GET, "/v1/checkout-sessions/{id}")
	run := func(a billingauth.Auth, authorization string) (bool, customerscope.Scope) {
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
			case "customerscope.Bind", "billingauth.BindIdentity", "billingauth.BindStaff", "billingauth.BindMerchant":
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

var _ = merchanttarget.FromContext
