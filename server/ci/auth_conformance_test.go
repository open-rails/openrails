//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
)

// guardedHost is an AuthKit host whose root roles hold its own staff
// permissions, as the README declares them, beside a stricter one for the
// merchant's configuration.
type guardedHost struct {
	ak                        *authkit.Client
	read, update, admin       iam.Perm
	customer, reader, support string // access tokens
	owner, gone               string
}

func newGuardedHost(t *testing.T, f *fixture) *guardedHost {
	t.Helper()
	rbac := authkit.NewRoles()
	h := &guardedHost{
		read:   rbac.Root.Permission("customers", "read"),
		update: rbac.Root.Permission("customers", "update"),
		admin:  rbac.Root.Permission("billing", "admin"),
	}
	roles := map[string]iam.Role{
		"reader":  rbac.Root.Role("reader", h.read),
		"support": rbac.Root.Role("support", h.read, h.update),
		"admin":   rbac.Root.Role("billing-admin", h.read, h.update, h.admin),
	}
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) { c.Roles = rbac })) // a root: permission needs no Config.Merchant
	h.ak = as.Client
	token := func(role string) string {
		u := authtest.NewUser(t, h.ak)
		if role != "" {
			authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(u.ID), roles[role])
		}
		return authtest.SignIn(t, h.ak, u).AccessToken
	}
	h.customer, h.reader, h.support, h.owner = token(""), token("reader"), token("support"), token("admin")
	gone := authtest.NewUser(t, h.ak)
	authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(gone.ID), roles["support"])
	h.gone = authtest.SignIn(t, h.ak, gone).AccessToken
	_, err := h.ak.RevokeAccountSessions(t.Context(), iam.UserIdentity(gone.ID), gone.ID)
	require.NoError(t, err)
	return h
}

// mount serves client's routes with perms.
func (h *guardedHost) mount(t *testing.T, client *openrails.Client, perms openrails.Permissions) (http.Handler, error) {
	t.Helper()
	mux := http.NewServeMux()
	err := openrailshttp.Mount(mux, client, openrails.Routes{Auth: h.ak, Prefix: "/billing", Permissions: perms})
	return mux, err
}

// AuthKit's own Client is the Auth: it passes the conformance kit with the
// host's permissions, holding each exactly.
func TestAuthKitPassesCheckAuth(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	request := func(token string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/billing/v1/admin/payments", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}
	}
	openrailstest.CheckAuth(t, h.ak, openrailstest.AuthCases{
		Permissions: openrails.Permissions{AdminRead: h.read, AdminWrite: h.update},
		Customer:    request(h.customer),
		Staff:       request(h.support),
		Holders:     map[string]func() *http.Request{h.read.String(): request(h.reader)},
		Refused:     map[string]func() *http.Request{"forged": request(h.customer + "x"), "signed out": request(h.gone)},
		StaleStaff:  request(authtest.StaleSession(t, h.ak, h.support)),
	})
}

// Each admin route checks the host's AdminRead or AdminWrite by its level,
// each merchant-config route MerchantConfig; a bundle without its permission
// is not mounted. A write that moves money still needs a recent sign-in, and
// the machine checkout-attempt routes are gone.
func TestBundlePermissions(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	client := f.runtime(t, "bundles-"+uuid.NewString()[:8])
	payment := "/billing/v1/admin/payments/" + billing.PaymentID(uuid.New()).String() + "/refunds"
	subscription := "/billing/v1/admin/subscriptions/" + billing.SubscriptionID(uuid.New()).String() + "/cancel"
	serve := func(mux http.Handler, token, method, path string) (int, string) {
		w := call(t, mux, token, method, path, "", map[string]any{})
		return w.Code, w.Body.String()
	}
	// passes is a request the gate admitted: the handler answers, here that
	// the payment or subscription does not exist.
	passes := func(t *testing.T, mux http.Handler, token, method, path string) {
		t.Helper()
		code, body := serve(mux, token, method, path)
		require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed}, code, "%s %s: %s", method, path, body)
		require.NotContains(t, body, "page not found", "%s %s is mounted", method, path)
	}
	// AuthKit answers its own refusal: forbidden, or step_up_required.
	const forbidden = `"code":"forbidden"`
	refused := func(t *testing.T, mux http.Handler, token, method, path, want string) {
		t.Helper()
		code, body := serve(mux, token, method, path)
		require.Equal(t, http.StatusForbidden, code, "%s %s: %s", method, path, body)
		require.Contains(t, body, want)
	}
	unmounted := func(t *testing.T, mux http.Handler, method, path string) {
		t.Helper()
		code, body := serve(mux, h.owner, method, path)
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, code, "%s %s is not mounted: %s", method, path, body)
	}

	t.Run("admin reads and writes", func(t *testing.T) {
		mux, err := h.mount(t, client, openrails.Permissions{AdminRead: h.read, AdminWrite: h.update})
		require.NoError(t, err)
		refused(t, mux, h.customer, http.MethodGet, "/billing/v1/admin/payments", forbidden)
		passes(t, mux, h.reader, http.MethodGet, "/billing/v1/admin/payments")
		refused(t, mux, h.reader, http.MethodPost, payment, forbidden)
		passes(t, mux, h.support, http.MethodPost, payment)
		passes(t, mux, h.support, http.MethodPost, subscription)
		code, _ := serve(mux, h.customer, http.MethodGet, "/billing/v1/me/payment-methods")
		require.Equal(t, http.StatusOK, code, "a customer needs no permission")
		unmounted(t, mux, http.MethodGet, "/billing/v1/admin/psps")

		stale := authtest.StaleSession(t, h.ak, h.support)
		passes(t, mux, stale, http.MethodGet, "/billing/v1/admin/payments")
		refused(t, mux, stale, http.MethodPost, payment, `"code":"step_up_required"`)

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
		passes(t, mux, h.reader, http.MethodPost, "/billing/v1/admin/tiers/lookup")
		unmounted(t, mux, http.MethodPost, payment)
		unmounted(t, mux, http.MethodPost, subscription)
	})

	t.Run("merchant configuration", func(t *testing.T) {
		mux, err := h.mount(t, f.runtime(t, "config-"+uuid.NewString()[:8]), openrails.Permissions{AdminRead: h.read, AdminWrite: h.update, MerchantConfig: h.admin})
		require.NoError(t, err)
		refused(t, mux, h.support, http.MethodGet, "/billing/v1/admin/psps", forbidden)
		passes(t, mux, h.owner, http.MethodGet, "/billing/v1/admin/psps")
		passes(t, mux, h.support, http.MethodGet, "/billing/v1/admin/payments")
		unmounted(t, mux, http.MethodPost, "/billing/v1/admin/catalog/products")
	})

	t.Run("catalog edits", func(t *testing.T) {
		mux, err := h.mount(t, f.runtime(t, "catalog-"+uuid.NewString()[:8]), openrails.Permissions{AdminRead: h.read, CatalogWrite: h.admin})
		require.NoError(t, err)
		refused(t, mux, h.support, http.MethodPost, "/billing/v1/admin/catalog/products", forbidden)
		passes(t, mux, h.owner, http.MethodPost, "/billing/v1/admin/catalog/products")
		passes(t, mux, h.reader, http.MethodGet, "/billing/v1/admin/catalog/products")
		unmounted(t, mux, http.MethodGet, "/billing/v1/admin/psps")
		unmounted(t, mux, http.MethodPost, payment)
	})
}

// Mount fails closed: AdminWrite without AdminRead, and any bundle without
// Auth.
func TestMountRefusesWritesWithoutReads(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	client := f.runtime(t, "bundles-"+uuid.NewString()[:8])
	for name, perms := range map[string]openrails.Permissions{
		"writes alone":          {AdminWrite: h.update},
		"an empty read":         {AdminRead: iam.Perm{}, AdminWrite: h.update},
		"configuration, writes": {AdminWrite: h.update, MerchantConfig: h.admin},
	} {
		_, err := h.mount(t, client, perms)
		require.ErrorContains(t, err, "AdminWrite needs AdminRead", name)
	}
	_, err := h.mount(t, client, openrails.Permissions{CatalogWrite: h.admin})
	require.ErrorContains(t, err, "CatalogWrite needs AdminRead")
	err = openrailshttp.Mount(http.NewServeMux(), client, openrails.Routes{Prefix: "/billing", Permissions: openrails.Permissions{AdminRead: h.read}})
	require.ErrorContains(t, err, "Routes.Auth is required")
}
