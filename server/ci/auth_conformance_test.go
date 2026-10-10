//go:build e2e && integration

package ci_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	helpersauthtest "github.com/open-rails/helpers/auth/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
)

// guardedHost is an AuthKit host whose root roles hold its own staff
// permissions, as the README declares them, beside a stricter one for the
// merchant's configuration, and whose backend holds a root API key with the
// programmatic ones too.
type guardedHost struct {
	ak                        *authkit.Client
	scope                     openrails.Scope
	read, update, admin       iam.Perm
	entitlements, events      iam.Perm
	roles                     map[string]iam.Role
	customer, reader, support string // access tokens
	owner, gone               string
	app                       string // the backend's API key, holding every permission
}

func newGuardedHost(t *testing.T, f *fixture) *guardedHost {
	return newGuardedHostOn(t, f.pool)
}

// newGuardedHostOn is a guarded host whose AuthKit uses pool.
func newGuardedHostOn(t *testing.T, pool *pgxpool.Pool) *guardedHost {
	t.Helper()
	rbac := authkit.NewRoles(authkit.APIKeys)
	h := &guardedHost{
		read:         rbac.Root.Permission("billing", "read"),
		update:       rbac.Root.Permission("billing", "manage"),
		admin:        rbac.Root.Permission("config", "manage"),
		entitlements: rbac.Root.Permission("entitlements", "read"),
		events:       rbac.Root.Permission("events", "read"),
	}
	h.roles = map[string]iam.Role{
		"reader":  rbac.Root.Role("reader", h.read),
		"support": rbac.Root.Role("support", h.read, h.update),
		"admin":   rbac.Root.Role("billing-admin", h.read, h.update, h.admin, h.entitlements, h.events),
	}
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = pool }),
		authtest.WithConfig(func(c *authkit.Config) { c.Roles = rbac }))
	h.ak = as.Client
	scope, err := h.ak.Scope(t.Context(), iam.RootGroup())
	require.NoError(t, err)
	h.scope = scope
	h.customer, h.reader, h.support, h.owner = h.token(t, ""), h.token(t, "reader"), h.token(t, "support"), h.token(t, "admin")
	gone := authtest.NewUser(t, h.ak)
	authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(gone.ID), h.roles["support"])
	h.gone = authtest.SignIn(t, h.ak, gone).AccessToken
	_, err = h.ak.RevokeAccountSessions(t.Context(), iam.UserIdentity(gone.ID), gone.ID)
	require.NoError(t, err)
	key, err := h.ak.CreateAPIKey(t.Context(), iam.SystemIdentity(), iam.RootGroup(), iam.NewAPIKey{Name: "billing-backend", Role: h.roles["admin"]})
	require.NoError(t, err)
	h.app = key.Secret
	return h
}

// token signs in a new user holding role in the root group ("" holds none).
func (h *guardedHost) token(t *testing.T, role string) string {
	t.Helper()
	u := authtest.NewUser(t, h.ak)
	if role != "" {
		authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(u.ID), h.roles[role])
	}
	return authtest.SignIn(t, h.ak, u).AccessToken
}

// routes mounts perms, each one's route group on, guarded by AuthKit's
// Authenticator in the root group's scope.
func (h *guardedHost) routes(perms openrails.Permissions, programmatic bool) openrails.Routes {
	groups := openrails.RouteGroups{Admin: perms.AdminRead != nil, Catalog: perms.Catalog != nil, MerchantConfig: perms.MerchantConfig != nil, Metrics: perms.Metrics != nil, Programmatic: programmatic}
	routes := openrails.Routes{Auth: h.ak.Authenticator(), Prefix: "/billing", RouteGroups: groups, Permissions: perms}
	if perms != (openrails.Permissions{}) {
		routes.Scope = h.scope
	}
	return routes
}

// mount serves client's routes with perms.
func (h *guardedHost) mount(t *testing.T, client *openrails.Client, perms openrails.Permissions) (http.Handler, error) {
	t.Helper()
	mux := http.NewServeMux()
	return mux, openrailshttp.Mount(mux, client, h.routes(perms, false))
}

func bearerRequest(token string) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/billing/v1/admin/payments", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
}

// AuthKit's Authenticator passes the conformance kit with the host's Routes:
// staff holding each permission exactly, in exactly the root group, a stale
// sign-in, the backend's API key, and staff signed out last.
func TestAuthKitPassesCheckAuth(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	staff := authtest.NewUser(t, h.ak)
	authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(staff.ID), h.roles["support"])
	openrailstest.CheckAuth(t, h.routes(openrails.Permissions{AdminRead: h.read, AdminUpdate: h.update}, true), helpersauthtest.Cases{
		Staff:       bearerRequest(authtest.SignIn(t, h.ak, staff).AccessToken),
		User:        bearerRequest(h.customer),
		Holders:     map[string]func() *http.Request{h.read.String(): bearerRequest(h.reader)},
		Stale:       bearerRequest(authtest.StaleSession(t, h.ak, h.support)),
		Application: bearerRequest(h.app),
		Refused:     map[string]func() *http.Request{"forged": bearerRequest(h.customer + "x"), "signed out": bearerRequest(h.gone)},
		Revoke: func() {
			_, err := h.ak.RevokeAccountSessions(context.Background(), iam.UserIdentity(staff.ID), staff.ID)
			require.NoError(t, err)
		},
	})
}

// answer is a response's status, code and challenge.
type answer struct {
	status          int
	body, challenge string
}

func ask(t *testing.T, mux http.Handler, token, method, path string, body any) answer {
	t.Helper()
	w := call(t, mux, token, method, path, "", body)
	return answer{w.Code, w.Body.String(), w.Header().Get("WWW-Authenticate")}
}

// Each admin route checks the host's AdminRead or AdminUpdate by its level,
// each catalog route Catalog, each merchant-config route MerchantConfig; a
// group that is off is not mounted. A write that moves money still needs a
// recent sign-in, and the machine checkout-attempt routes are gone.
func TestRouteGroupPermissions(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	client := f.runtime(t, "groups-"+uuid.NewString()[:8])
	payment := "/billing/v1/admin/payments/" + billing.PaymentID(uuid.New()).String() + "/refunds"
	subscription := "/billing/v1/admin/subscriptions/" + billing.SubscriptionID(uuid.New()).String() + "/cancel"
	// passes is a request the gate admitted: the handler answers, here that
	// the payment or subscription does not exist.
	passes := func(t *testing.T, mux http.Handler, token, method, path string) {
		t.Helper()
		got := ask(t, mux, token, method, path, map[string]any{})
		require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed}, got.status, "%s %s: %s", method, path, got.body)
		require.NotContains(t, got.body, "page not found", "%s %s is mounted", method, path)
	}
	// OpenRails answers every refusal: permission_required, or a step-up.
	forbidden := func(t *testing.T, mux http.Handler, token, method, path string) {
		t.Helper()
		got := ask(t, mux, token, method, path, map[string]any{})
		require.Equal(t, http.StatusForbidden, got.status, "%s %s: %s", method, path, got.body)
		require.Contains(t, got.body, `"code":"permission_required"`)
	}
	unmounted := func(t *testing.T, mux http.Handler, method, path string) {
		t.Helper()
		got := ask(t, mux, h.owner, method, path, map[string]any{})
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, got.status, "%s %s is not mounted: %s", method, path, got.body)
	}

	t.Run("admin reads and writes", func(t *testing.T) {
		mux, err := h.mount(t, client, openrails.Permissions{AdminRead: h.read, AdminUpdate: h.update})
		require.NoError(t, err)
		forbidden(t, mux, h.customer, http.MethodGet, "/billing/v1/admin/payments")
		passes(t, mux, h.reader, http.MethodGet, "/billing/v1/admin/payments")
		forbidden(t, mux, h.reader, http.MethodPost, payment)
		passes(t, mux, h.support, http.MethodPost, payment)
		passes(t, mux, h.support, http.MethodPost, subscription)
		require.Equal(t, http.StatusOK, ask(t, mux, h.customer, http.MethodGet, "/billing/v1/me/payment-methods", nil).status, "a customer needs no permission")
		unmounted(t, mux, http.MethodGet, "/billing/v1/admin/psps")

		stale := authtest.StaleSession(t, h.ak, h.support)
		passes(t, mux, stale, http.MethodGet, "/billing/v1/admin/payments")
		got := ask(t, mux, stale, http.MethodPost, payment, map[string]any{})
		require.Equal(t, http.StatusUnauthorized, got.status, got.body)
		require.Contains(t, got.body, `"code":"step_up_required"`)

		for _, route := range []struct{ method, path string }{
			{http.MethodPost, "/billing/v1/admin/checkout-attempts"},
			{http.MethodGet, "/billing/v1/admin/checkout-attempts/" + billing.CheckoutAttemptID(uuid.New()).String()},
			{http.MethodPost, "/billing/v1/admin/checkout-attempts/" + billing.CheckoutAttemptID(uuid.New()).String() + "/confirm"},
		} {
			unmounted(t, mux, route.method, route.path)
		}
		// Staff may hand a customer a checkout session; only the customer
		// pays it with a saved card.
		mint := map[string]any{"customer": map[string]any{"id": uuid.NewString()}, "price_id": billing.PriceID(uuid.New())}
		w := call(t, mux, h.reader, http.MethodPost, "/billing/v1/admin/checkout-sessions", "", mint)
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		w = call(t, mux, h.support, http.MethodPost, "/billing/v1/admin/checkout-sessions", "", mint)
		require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, w.Code, w.Body.String())
	})

	t.Run("reads alone mount no write", func(t *testing.T) {
		mux, err := h.mount(t, client, openrails.Permissions{AdminRead: h.read})
		require.NoError(t, err)
		passes(t, mux, h.reader, http.MethodGet, "/billing/v1/admin/payments")
		passes(t, mux, h.reader, http.MethodPost, "/billing/v1/admin/subscriptions/"+billing.SubscriptionID(uuid.New()).String()+"/change/preview")
		unmounted(t, mux, http.MethodPost, payment)
		unmounted(t, mux, http.MethodPost, subscription)
	})

	t.Run("merchant configuration", func(t *testing.T) {
		mux, err := h.mount(t, f.runtime(t, "config-"+uuid.NewString()[:8]), openrails.Permissions{AdminRead: h.read, AdminUpdate: h.update, MerchantConfig: h.admin})
		require.NoError(t, err)
		forbidden(t, mux, h.support, http.MethodGet, "/billing/v1/admin/psps")
		passes(t, mux, h.owner, http.MethodGet, "/billing/v1/admin/psps")
		passes(t, mux, h.support, http.MethodGet, "/billing/v1/admin/payments")
		unmounted(t, mux, http.MethodPost, "/billing/v1/admin/catalog/products")
	})

	t.Run("the catalog alone", func(t *testing.T) {
		mux, err := h.mount(t, f.runtime(t, "catalog-"+uuid.NewString()[:8]), openrails.Permissions{Catalog: h.admin})
		require.NoError(t, err)
		forbidden(t, mux, h.support, http.MethodPost, "/billing/v1/admin/catalog/products")
		forbidden(t, mux, h.reader, http.MethodGet, "/billing/v1/admin/catalog/products")
		passes(t, mux, h.owner, http.MethodPost, "/billing/v1/admin/catalog/products")
		passes(t, mux, h.owner, http.MethodGet, "/billing/v1/admin/catalog/products")
		unmounted(t, mux, http.MethodGet, "/billing/v1/admin/psps")
		unmounted(t, mux, http.MethodGet, "/billing/v1/admin/payments")
	})
}

// Through a real mount guarded by AuthKit's Authenticator, OpenRails answers
// every refusal itself: 401 with its challenge (none presented, forged,
// signed out), 403 for a permission not held, RFC 9470's 401 step-up for a
// stale person on an operation that moves money, which the backend's API key
// passes on its permission alone, and 503 while AuthKit cannot reach its
// database. /v1/app takes the API key, an application, and refuses a person.
func TestAuthKitRefusals(t *testing.T) {
	f := newFixture(t)
	var down atomic.Bool
	cfg, err := pgxpool.ParseConfig(strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN")))
	require.NoError(t, err)
	cfg.PrepareConn = func(context.Context, *pgx.Conn) (bool, error) {
		if down.Load() {
			return true, errors.New("e2e: AuthKit's database is down")
		}
		return true, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	h := newGuardedHostOn(t, pool)
	client := f.runtime(t, "refusals-"+uuid.NewString()[:8])
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, client, h.routes(openrails.Permissions{AdminRead: h.read, AdminUpdate: h.update, Entitlements: h.entitlements, Events: h.events}, true)))
	refund := "/billing/v1/admin/payments/" + billing.PaymentID(uuid.New()).String() + "/refunds"

	anonymous := httptest.NewRecorder()
	mux.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/billing/v1/admin/payments", nil))
	require.Equal(t, http.StatusUnauthorized, anonymous.Code, anonymous.Body.String())
	require.Equal(t, "Bearer", anonymous.Header().Get("WWW-Authenticate"))
	require.Contains(t, anonymous.Body.String(), `"code":"authentication_required"`)

	for name, tc := range map[string]struct {
		token, method, path string
		status              int
		code, challenge     string
	}{
		"forged":                {h.customer + "x", http.MethodGet, "/billing/v1/admin/payments", 401, "authentication_required", `Bearer error="invalid_token"`},
		"signed out":            {h.gone, http.MethodGet, "/billing/v1/admin/payments", 401, "credential_revoked", `Bearer error="invalid_token"`},
		"a permission not held": {h.customer, http.MethodGet, "/billing/v1/admin/payments", 403, "permission_required", ""},
		"a stale person":        {authtest.StaleSession(t, h.ak, h.support), http.MethodPost, refund, 401, "step_up_required", `Bearer error="insufficient_user_authentication", max_age="900"`},
		"a person on /v1/app":   {h.owner, http.MethodGet, "/billing/v1/app/host-events", 403, "application_required", ""},
		"a customer on /v1/app": {h.customer, http.MethodPost, "/billing/v1/app/entitlements/check", 403, "application_required", ""},
	} {
		got := ask(t, mux, tc.token, tc.method, tc.path, map[string]any{})
		require.Equal(t, tc.status, got.status, "%s: %s", name, got.body)
		require.Contains(t, got.body, `"code":"`+tc.code+`"`, name)
		require.Equal(t, tc.challenge, got.challenge, name)
	}
	stale := ask(t, mux, authtest.StaleSession(t, h.ak, h.support), http.MethodPost, refund, map[string]any{})
	require.Contains(t, stale.body, `"step_up_methods":[`, "AuthKit's challenge reaches its client: %s", stale.body)

	for _, path := range []string{refund, "/billing/v1/app/host-events"} {
		method := http.MethodPost
		if strings.HasSuffix(path, "host-events") {
			method = http.MethodGet
		}
		got := ask(t, mux, h.app, method, path, map[string]any{})
		require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, got.status, "the backend's API key reaches %s: %s", path, got.body)
	}

	down.Store(true)
	got := ask(t, mux, h.support, http.MethodGet, "/billing/v1/admin/payments", nil)
	down.Store(false)
	require.Equal(t, http.StatusServiceUnavailable, got.status, got.body)
	require.Contains(t, got.body, `"code":"authentication_unavailable"`)
	require.Empty(t, got.challenge)
	require.Equal(t, http.StatusOK, ask(t, mux, h.support, http.MethodGet, "/billing/v1/admin/payments", nil).status, "back once AuthKit is")
}

// Mount fails closed: a group on without its permission, a permission for a
// group that is off or AuthKit does not know, a permission without Scope or
// a Scope without one, and any group without Auth.
func TestMountFailsClosed(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	client := f.runtime(t, "groups-"+uuid.NewString()[:8])
	ak := h.ak.Authenticator()
	for name, tc := range map[string]struct {
		routes openrails.Routes
		err    string
	}{
		"admin without its read":             {openrails.Routes{Auth: ak, Scope: h.scope, RouteGroups: adminGroups, Permissions: openrails.Permissions{AdminUpdate: h.update}}, "RouteGroups.Admin is on without Permissions.AdminRead"},
		"an empty read":                      {openrails.Routes{Auth: ak, Scope: h.scope, RouteGroups: adminGroups, Permissions: openrails.Permissions{AdminRead: iam.Perm{}}}, "RouteGroups.Admin is on without Permissions.AdminRead"},
		"a group that is off":                {openrails.Routes{Auth: ak, Permissions: openrails.Permissions{Catalog: h.admin}}, "Permissions.Catalog is given, but RouteGroups.Catalog is off"},
		"no Auth":                            {openrails.Routes{Scope: h.scope, RouteGroups: adminGroups, Permissions: openrails.Permissions{AdminRead: h.read}}, "Routes.Auth is required"},
		"no Scope":                           {openrails.Routes{Auth: ak, RouteGroups: adminGroups, Permissions: openrails.Permissions{AdminRead: h.read}}, "without Routes.Scope"},
		"a Scope, no permission":             {openrails.Routes{Auth: ak, Scope: h.scope}, "Routes.Scope is given, but no permission is"},
		"programmatic off":                   {openrails.Routes{Auth: ak, Scope: h.scope, Permissions: openrails.Permissions{Events: h.events}}, "Permissions.Events is given, but RouteGroups.Programmatic is off"},
		"an unknown permission":              {openrails.Routes{Auth: ak, Scope: h.scope, RouteGroups: adminGroups, Permissions: openrails.Permissions{AdminRead: perm("root:billing:raed")}}, `does not know the permission "root:billing:raed"`},
		"an unknown programmatic permission": {openrails.Routes{Auth: ak, Scope: h.scope, RouteGroups: openrails.RouteGroups{Programmatic: true}, Permissions: openrails.Permissions{Usage: perm("root:usage:manage")}}, `does not know the permission "root:usage:manage"`},
	} {
		err := openrailshttp.Mount(http.NewServeMux(), client, tc.routes)
		require.ErrorContains(t, err, tc.err, name)
	}
}
