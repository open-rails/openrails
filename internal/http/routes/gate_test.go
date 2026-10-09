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
	"GET /health/live", "GET /health/ready", "GET /metrics", "GET /v1/capabilities",
	"GET /v1/captcha/client.js", "GET /v1/captcha/status",
	"GET /v1/checkout-config", "GET /v1/currencies", "GET /v1/prices", "GET /v1/products", "GET /v1/solana/tokens",
	"GET /v1/checkout-attempts/{id}/solana-pay", "POST /v1/checkout-attempts/{id}/solana-pay",
	"GET /v1/checkout-sessions/{id}", "POST /v1/checkout-sessions/{id}/pay",
	"POST /v1/webhooks/{rail}/{account_id}",
}

// Every catalog route declares exactly one tier of the closed set, and the
// permission that tier checks: a merchant route one exact merchant
// permission, a customer route none, and no customer path names a customer.
func TestEveryRouteDeclaresOneTier(t *testing.T) {
	tiers := []Tier{AuthPublic, AuthCheckoutSession, AuthSessionID, AuthUser, AuthCustomer, AuthMerchant, AuthOperator, AuthProvider}
	permissions := Permissions()
	var open []string
	for _, r := range Catalog() {
		key := r.Key()
		require.Contains(t, tiers, r.Auth, "%s declares no known tier", key)
		if slices.Contains(openTiers, r.Auth) {
			open = append(open, key)
		}
		switch r.Auth {
		case AuthMerchant:
			require.NotContains(t, r.Perm, "*", "%s: a permission is exact, never a glob", key)
			if r.Group == Merchant {
				require.Contains(t, permissions, r.Perm, key)
			}
			require.Equal(t, billing.RequiresRecentSignIn(r.Perm) || r.Also != "" && billing.RequiresRecentSignIn(r.Also), Sensitive(r), key)
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
	for _, p := range permissions {
		require.True(t, strings.HasPrefix(p, "merchant:") && !strings.Contains(p, "*"), p)
	}
	require.Contains(t, permissions, billing.MerchantAccessGrantPermanent, "handlers' further asks are permissions too")
	for _, builtin := range []string{billing.MerchantMembersRead, billing.MerchantMembersManage, billing.MerchantCredentialsManage} {
		require.NotContains(t, permissions, builtin, "AuthKit registers its own built-ins; the merchant API never asks them")
	}
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
	RegisterMerchantRoutes(router.NewMux(table, "/v1", rt), rt, Options{Auth: a, CatalogWrites: true})
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
// permission (and Also) and, for one that moves money or removes access by a
// user in person, Sensitive on a merchant route; nothing on an open route.
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
		for _, r := range Catalog() {
			if !mounted[r.Key()] || r.Group == ControlPlane || r.Group == Platform {
				continue
			}
			var want []string
			switch r.Auth {
			case AuthCustomer:
				want = []string{"Required"}
			case AuthMerchant:
				want = []string{"RequirePermission:" + r.Perm}
				if r.Also != "" {
					want = append(want, "RequirePermission:"+r.Also)
				}
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
		if r.Auth != AuthCustomer && r.Auth != AuthMerchant || r.Group != Merchant && r.Group != Customer {
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
	env := newEnv(rt, Options{Auth: authtest.Deny{}, CatalogWrites: true})
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
				billingauth.Staff{Identity: authtest.User(userA), Permission: "merchant:other:read", Merchant: merchantA}),
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
// identity, a person on a machine-only route, and anything at another merchant.
func TestStaffGate(t *testing.T) {
	rt := gatedRuntime(t)
	run := func(a billingauth.Auth, key string, header map[string]string) (int, billingauth.Staff) {
		method, path, _ := strings.Cut(key, " ")
		route, ok := Lookup(method, path)
		require.True(t, ok, key)
		env := newEnv(rt, Options{Auth: a})
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
	require.Equal(t, billingauth.Staff{Identity: person, Permission: billing.MerchantPaymentsRefund, Merchant: merchantA}, staff)

	for _, key := range []string{"POST /v1/merchant/checkout-sessions", "POST /v1/merchant/checkout-attempts", "POST /v1/merchant/checkout-attempts/{id}/confirm"} {
		code, _ = run(&recordingAuth{who: person}, key, nil)
		require.Equal(t, http.StatusForbidden, code, "%s: a person never creates a checkout for someone else", key)
		code, staff = run(&recordingAuth{who: service}, key, nil)
		require.Equal(t, http.StatusNoContent, code, key)
		require.Equal(t, "k_svc_1", staff.Credential.ID)
		code, _ = run(&recordingAuth{who: personalKey}, key, nil)
		require.Equal(t, http.StatusNoContent, code, "%s: a user's API key automates the account", key)
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
	env := newEnv(unbound, Options{Auth: &recordingAuth{who: person}})
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
		"merchant without Auth": func() { RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{}) },
		"merchant with a typed nil Auth": func() {
			RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: (*authtest.Fake)(nil)})
		},
		"customers without Auth": func() {
			RegisterSelfServiceRoutes(router.NewMux(&router.Table{}, "/v1/me", rt), rt, CustomerMount{})
		},
		"a nil RequirePermission": func() {
			RegisterMerchantRoutes(router.NewMux(&router.Table{}, "/v1", rt), rt, Options{Auth: nilPermission{}})
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
			case "customerscope.Bind", "billingauth.BindIdentity", "billingauth.BindStaff":
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
