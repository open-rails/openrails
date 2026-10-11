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

// guardedHost is an AuthKit host that declares OpenRails' permissions from
// billing.Permissions, each on its persona: its root roles hold the root:
// names, as the README declares them, and a merchant's own staff the
// merchant: ones in that merchant's group. Its backend holds a root API key
// with every root: name.
type guardedHost struct {
	ak                        *authkit.Client
	perms                     map[billing.Permission]iam.Perm
	merchant                  iam.Persona
	roles                     map[string]iam.Role
	customer, reader, support string // access tokens
	owner, gone               string
	app                       string // the backend's API key, holding every root: permission
}

func newGuardedHost(t *testing.T, f *fixture) *guardedHost {
	return newGuardedHostOn(t, f.pool)
}

// newGuardedHostOn is a guarded host whose AuthKit uses pool.
func newGuardedHostOn(t *testing.T, pool *pgxpool.Pool) *guardedHost {
	t.Helper()
	rbac := authkit.NewRoles(authkit.APIKeys)
	merchant := rbac.Persona(billing.MerchantGroupPersona)
	h := &guardedHost{perms: map[billing.Permission]iam.Perm{}, merchant: merchant.Persona}
	var everyRoot []iam.Grant
	for _, p := range billing.Permissions() {
		if p.Persona() == billing.MerchantGroupPersona {
			h.perms[p] = merchant.Permission(p.Resource(), p.Action())
			continue
		}
		h.perms[p] = rbac.Root.Permission(p.Resource(), p.Action())
		everyRoot = append(everyRoot, h.perms[p])
	}
	h.roles = map[string]iam.Role{
		"reader":  rbac.Root.Role("reader", h.perms[billing.RootBillingRead]),
		"support": rbac.Root.Role("support", h.perms[billing.RootBillingRead], h.perms[billing.RootBillingManage]),
		"admin":   rbac.Root.Role("billing-admin", everyRoot...),
		// A merchant's own staff, in its group.
		"merchant-support": merchant.Role("support", h.perms[billing.MerchantBillingRead], h.perms[billing.MerchantBillingManage]),
	}
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = pool }),
		authtest.WithConfig(func(c *authkit.Config) { c.Roles = rbac }))
	h.ak = as.Client
	h.customer, h.reader, h.support, h.owner = h.token(t, ""), h.token(t, "reader"), h.token(t, "support"), h.token(t, "admin")
	gone := authtest.NewUser(t, h.ak)
	authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(gone.ID), h.roles["support"])
	h.gone = authtest.SignIn(t, h.ak, gone).AccessToken
	_, err := h.ak.RevokeAccountSessions(t.Context(), iam.UserIdentity(gone.ID), gone.ID)
	require.NoError(t, err)
	key, err := h.ak.CreateAPIKey(t.Context(), iam.SystemIdentity(), iam.RootGroup(), iam.NewAPIKey{Name: "billing-backend", Role: h.roles["admin"]})
	require.NoError(t, err)
	h.app = key.Secret
	return h
}

// merchantStaff signs in a new user holding the merchant support role in
// mid's own group, whose id is the merchant's.
func (h *guardedHost) merchantStaff(t *testing.T, mid billing.MerchantID) string {
	t.Helper()
	group, err := h.ak.CreateGroup(t.Context(), iam.NewGroup{ID: mid.String(), Persona: h.merchant})
	require.NoError(t, err)
	u := authtest.NewUser(t, h.ak)
	authtest.GrantRole(t, h.ak, iam.GroupByID(group.ID), iam.UserSubject(u.ID), h.roles["merchant-support"])
	return authtest.SignIn(t, h.ak, u).AccessToken
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

// routes turns groups on, guarded by AuthKit's Authenticator alone.
func (h *guardedHost) routes(groups openrails.RouteGroups) openrails.Routes {
	return openrails.Routes{Auth: h.ak.Authenticator(), Prefix: "/billing", RouteGroups: groups}
}

// mount serves bill's routes with groups on.
func (h *guardedHost) mount(t *testing.T, bill *openrails.Engine, groups openrails.RouteGroups) (http.Handler, error) {
	t.Helper()
	mux := http.NewServeMux()
	return mux, openrailshttp.Mount(mux, bill, h.routes(groups))
}

func bearerRequest(token string) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/billing/v1/admin/payments", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
}

// AuthKit's Authenticator passes the conformance kit with the host's Routes:
// staff holding each of the route groups' root: permissions exactly, in
// exactly the root group (its RootScope), a stale sign-in, the backend's API
// key, and staff signed out last.
func TestAuthKitPassesCheckAuth(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	staff := authtest.NewUser(t, h.ak)
	authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(staff.ID), h.roles["admin"])
	openrailstest.CheckAuth(t, h.routes(openrails.RouteGroups{Admin: true, Programmatic: true}), helpersauthtest.Cases{
		Staff:       bearerRequest(authtest.SignIn(t, h.ak, staff).AccessToken),
		User:        bearerRequest(h.customer),
		Holders:     map[string]func() *http.Request{string(billing.RootBillingRead): bearerRequest(h.reader)},
		Stale:       bearerRequest(authtest.StaleSession(t, h.ak, h.owner)),
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

// Each admin route checks its permission by its level, each catalog and
// merchant-config route its group's: the root: name (root:billing:read) in
// AuthKit's root group or the merchant: name (merchant:billing:read) in the
// merchant's own. A group that is off is not mounted. A write that moves
// money still needs a recent sign-in, and the machine checkout-attempt
// routes are gone.
func TestRouteGroupPermissions(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	bill, client := f.runtime(t, "groups-"+uuid.NewString()[:8])
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
		mux, err := h.mount(t, bill, adminGroups)
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
		unmounted(t, mux, http.MethodPost, "/billing/v1/admin/checkout-sessions")
	})

	// A merchant's own staff hold the merchant: names in its group: they act
	// at that merchant and nowhere else, and a root holder at both.
	t.Run("a merchant's own staff", func(t *testing.T) {
		other, _ := f.runtime(t, "other-"+uuid.NewString()[:8])
		staff := h.merchantStaff(t, client.MerchantID())
		here, err := h.mount(t, bill, adminGroups)
		require.NoError(t, err)
		there, err := h.mount(t, other, adminGroups)
		require.NoError(t, err)
		passes(t, here, staff, http.MethodGet, "/billing/v1/admin/payments")
		passes(t, here, staff, http.MethodPost, payment)
		forbidden(t, there, staff, http.MethodGet, "/billing/v1/admin/payments")
		forbidden(t, there, staff, http.MethodPost, payment)
		passes(t, there, h.reader, http.MethodGet, "/billing/v1/admin/payments")

		access := func(mux http.Handler, token string) billing.AdminAccess {
			return decode[billing.AdminAccess](t, call(t, mux, token, http.MethodGet, "/billing/v1/admin/access", "", nil))
		}
		require.Equal(t, billing.AdminAccess{Admin: billing.AccessUpdate, ScopeID: client.MerchantID().String()}, access(here, staff))
		require.Equal(t, access(here, staff), access(here, h.support), "the access read answers the same levels for either name")
		require.Equal(t, billing.AccessNone, access(there, staff).Admin)
	})

	t.Run("merchant configuration", func(t *testing.T) {
		config, _ := f.runtime(t, "config-"+uuid.NewString()[:8])
		mux, err := h.mount(t, config, openrails.RouteGroups{Admin: true, MerchantConfig: true})
		require.NoError(t, err)
		forbidden(t, mux, h.support, http.MethodGet, "/billing/v1/admin/psps")
		passes(t, mux, h.owner, http.MethodGet, "/billing/v1/admin/psps")
		passes(t, mux, h.support, http.MethodGet, "/billing/v1/admin/payments")
		unmounted(t, mux, http.MethodPost, "/billing/v1/admin/catalog/products")
	})

	t.Run("the catalog alone", func(t *testing.T) {
		catalog, _ := f.runtime(t, "catalog-"+uuid.NewString()[:8])
		mux, err := h.mount(t, catalog, openrails.RouteGroups{Catalog: true})
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
	bill, _ := f.runtime(t, "refusals-"+uuid.NewString()[:8])
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, h.routes(openrails.RouteGroups{Admin: true, Programmatic: true})))
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

// Mount fails closed: any group without Auth, and a route neither of whose
// permissions the host's AuthKit declares, which is named. A host that
// declares only the root: names mounts every group.
func TestMountFailsClosed(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	bill, _ := f.runtime(t, "groups-"+uuid.NewString()[:8])
	require.ErrorContains(t, openrailshttp.Mount(http.NewServeMux(), bill, openrails.Routes{RouteGroups: adminGroups}), "Routes.Auth is required")

	// A host whose roles declare root's billing permissions alone.
	rbac := authkit.NewRoles()
	for _, p := range []billing.Permission{billing.RootBillingRead, billing.RootBillingManage} {
		rbac.Root.Permission(p.Resource(), p.Action())
	}
	bare := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) { c.Roles = rbac })).Client.Authenticator()
	require.NoError(t, openrailshttp.Mount(http.NewServeMux(), bill, openrails.Routes{Auth: bare, Prefix: "/a", RouteGroups: adminGroups}))
	for name, tc := range map[string]struct {
		groups openrails.RouteGroups
		err    string
	}{
		"a staff group it declares nothing for": {openrails.RouteGroups{Admin: true, Metrics: true}, `knows none of its permissions ["merchant:metrics:read" "root:metrics:read"]`},
		"the programmatic routes":               {openrails.RouteGroups{Programmatic: true}, "/v1/app/"},
	} {
		err := openrailshttp.Mount(http.NewServeMux(), bill, openrails.Routes{Auth: bare, Prefix: "/b", RouteGroups: tc.groups})
		require.ErrorContains(t, err, tc.err, name)
		require.ErrorContains(t, err, "knows none of its permissions", name)
	}

	every := openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}
	require.NoError(t, openrailshttp.Mount(http.NewServeMux(), bill, openrails.Routes{Auth: h.ak.Authenticator(), Prefix: "/c", RouteGroups: every}))
}
