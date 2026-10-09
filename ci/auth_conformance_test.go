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

// perm is a test host's own permission.
type perm string

func (p perm) String() string { return string(p) }

// staffGuards guard every staff route with one permission per level group.
var staffGuards = openrails.Guards{openrails.StaffReads: perm("host:billing:read"), openrails.StaffWrites: perm("host:billing:write"), openrails.MerchantConfig: perm("host:billing:admin")}

// readsAndWrites guard Routes.Merchant alone.
var readsAndWrites = openrails.Guards{openrails.StaffReads: staffGuards[openrails.StaffReads], openrails.StaffWrites: staffGuards[openrails.StaffWrites]}

// guardedHost is an AuthKit host whose root roles hold its own staff
// permissions, as the README declares them, beside a stricter refund
// permission and one for a single route.
type guardedHost struct {
	ak                                          *authkit.Client
	read, update, refunds, refundOne, admin     iam.Perm
	customer, reader, support, refunder, single string // access tokens
	owner, gone                                 string
}

func newGuardedHost(t *testing.T, f *fixture) *guardedHost {
	t.Helper()
	rbac := authkit.NewRoles()
	h := &guardedHost{
		read:      rbac.Root.Permission("customers", "read"),
		update:    rbac.Root.Permission("customers", "update"),
		refunds:   rbac.Root.Permission("refunds", "create"),
		refundOne: rbac.Root.Permission("payments", "refund"),
		admin:     rbac.Root.Permission("billing", "admin"),
	}
	roles := map[string]iam.Role{
		"reader":   rbac.Root.Role("reader", h.read),
		"support":  rbac.Root.Role("support", h.read, h.update),
		"refunder": rbac.Root.Role("refunder", h.read, h.refunds),
		"single":   rbac.Root.Role("single", h.read, h.refundOne),
		"admin":    rbac.Root.Role("billing-admin", h.read, h.update, h.refunds, h.refundOne, h.admin),
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
	h.customer, h.reader, h.support, h.refunder, h.single, h.owner = token(""), token("reader"), token("support"), token("refunder"), token("single"), token("admin")
	gone := authtest.NewUser(t, h.ak)
	authtest.GrantRole(t, h.ak, iam.RootGroup(), iam.UserSubject(gone.ID), roles["support"])
	h.gone = authtest.SignIn(t, h.ak, gone).AccessToken
	_, err := h.ak.RevokeAccountSessions(t.Context(), iam.UserIdentity(gone.ID), gone.ID)
	require.NoError(t, err)
	return h
}

// mount serves client's routes guarded by guards.
func (h *guardedHost) mount(t *testing.T, client *openrails.Client, config bool, guards openrails.Guards) (http.Handler, error) {
	t.Helper()
	mux := http.NewServeMux()
	err := openrailshttp.Mount(mux, client, openrails.Routes{Auth: h.ak, Prefix: "/billing", Merchant: true, MerchantConfig: config, Customers: openrails.CustomerSelfService, Guards: guards})
	return mux, err
}

// AuthKit's own Client is the Auth: it passes the conformance kit with the
// host's permissions as guards, holding each guard's permission exactly.
func TestAuthKitPassesCheckAuth(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	request := func(token string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/payments", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}
	}
	openrailstest.CheckAuth(t, h.ak, openrailstest.AuthCases{
		Guards:     openrails.Guards{openrails.StaffReads: h.read, openrails.StaffWrites: h.update},
		Customer:   request(h.customer),
		Staff:      request(h.support),
		Holders:    map[openrails.RouteSet]func() *http.Request{openrails.StaffReads: request(h.reader)},
		Refused:    map[string]func() *http.Request{"forged": request(h.customer + "x"), "signed out": request(h.gone)},
		StaleStaff: request(authtest.StaleSession(t, h.ak, h.support)),
	})
}

// Each staff route checks the host's permission for its most specific guard:
// the level groups, a resource group over its level, one route over both. A
// write that moves money still needs a recent sign-in, and the machine
// checkout-attempt routes are gone.
func TestGuardsResolvePerTier(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	client := f.runtime(t, "guards-"+uuid.NewString()[:8])
	payment := "/billing/v1/merchant/payments/" + billing.PaymentID(uuid.New()).String() + "/refunds"
	subscription := "/billing/v1/merchant/subscriptions/" + billing.SubscriptionID(uuid.New()).String() + "/cancel"
	serve := func(mux http.Handler, token, method, path string) (int, string) {
		w := call(t, mux, token, method, path, "", map[string]any{})
		return w.Code, w.Body.String()
	}
	// passes is a request the gate admitted: the handler answers, here that
	// the payment or subscription does not exist.
	passes := func(t *testing.T, mux http.Handler, token, method, path string) {
		t.Helper()
		code, body := serve(mux, token, method, path)
		require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, code, "%s %s: %s", method, path, body)
	}
	// AuthKit answers its own refusal: forbidden, or step_up_required.
	const forbidden = `"code":"forbidden"`
	refused := func(t *testing.T, mux http.Handler, token, method, path, want string) {
		t.Helper()
		code, body := serve(mux, token, method, path)
		require.Equal(t, http.StatusForbidden, code, "%s %s: %s", method, path, body)
		require.Contains(t, body, want)
	}

	t.Run("level groups", func(t *testing.T) {
		mux, err := h.mount(t, client, false, openrails.Guards{openrails.StaffReads: h.read, openrails.StaffWrites: h.update})
		require.NoError(t, err)
		refused(t, mux, h.customer, http.MethodGet, "/billing/v1/merchant/payments", forbidden)
		passes(t, mux, h.reader, http.MethodGet, "/billing/v1/merchant/payments")
		refused(t, mux, h.reader, http.MethodPost, payment, forbidden)
		passes(t, mux, h.support, http.MethodPost, payment)
		passes(t, mux, h.support, http.MethodPost, subscription)
		code, _ := serve(mux, h.customer, http.MethodGet, "/billing/v1/me/payment-methods")
		require.Equal(t, http.StatusOK, code, "a customer needs no permission")

		stale := authtest.StaleSession(t, h.ak, h.support)
		passes(t, mux, stale, http.MethodGet, "/billing/v1/merchant/payments")
		refused(t, mux, stale, http.MethodPost, payment, `"code":"step_up_required"`)

		for _, route := range []struct{ method, path string }{
			{http.MethodPost, "/billing/v1/merchant/checkout-attempts"},
			{http.MethodGet, "/billing/v1/merchant/checkout-attempts/" + billing.CheckoutAttemptID(uuid.New()).String()},
			{http.MethodPost, "/billing/v1/merchant/checkout-attempts/" + billing.CheckoutAttemptID(uuid.New()).String() + "/confirm"},
		} {
			code, body := serve(mux, h.owner, route.method, route.path)
			require.Equal(t, http.StatusNotFound, code, "%s %s is not mounted: %s", route.method, route.path, body)
		}
		// Staff may hand a customer a checkout session; only the customer
		// pays it with a saved card.
		mint := map[string]any{"customer": map[string]any{"id": uuid.NewString()}, "price_id": billing.PriceID(uuid.New())}
		w := call(t, mux, h.reader, http.MethodPost, "/billing/v1/merchant/checkout-sessions", "", mint)
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		w = call(t, mux, h.support, http.MethodPost, "/billing/v1/merchant/checkout-sessions", "", mint)
		require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, w.Code, w.Body.String())
	})

	t.Run("a resource group overrides its level", func(t *testing.T) {
		mux, err := h.mount(t, client, false, openrails.Guards{openrails.StaffReads: h.read, openrails.StaffWrites: h.update, openrails.Refunds: h.refunds})
		require.NoError(t, err)
		refused(t, mux, h.support, http.MethodPost, payment, forbidden)
		passes(t, mux, h.refunder, http.MethodPost, payment)
		passes(t, mux, h.support, http.MethodPost, subscription)
		refused(t, mux, h.refunder, http.MethodPost, subscription, forbidden)
	})

	t.Run("a route overrides its resource and level", func(t *testing.T) {
		mux, err := h.mount(t, client, false, openrails.Guards{openrails.StaffReads: h.read, openrails.StaffWrites: h.update, openrails.Refunds: h.refunds, openrails.RefundPayment: h.refundOne})
		require.NoError(t, err)
		refused(t, mux, h.refunder, http.MethodPost, payment, forbidden)
		refused(t, mux, h.support, http.MethodPost, payment, forbidden)
		passes(t, mux, h.single, http.MethodPost, payment)
		passes(t, mux, h.support, http.MethodPost, subscription)
	})
}

// Mount fails closed: a mounted staff route no guard covers, two guards of
// one tier covering a route, a guard for routes not mounted, and the
// configuration routes without their guard.
func TestMountRefusesIncompleteGuards(t *testing.T) {
	f := newFixture(t)
	h := newGuardedHost(t, f)
	client := f.runtime(t, "guards-"+uuid.NewString()[:8])
	levels := openrails.Guards{openrails.StaffReads: h.read, openrails.StaffWrites: h.update}
	with := func(extra openrails.Guards) openrails.Guards {
		out := openrails.Guards{}
		for k, v := range levels {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for name, tc := range map[string]struct {
		config bool
		guards openrails.Guards
		want   string
	}{
		"writes uncovered":             {false, openrails.Guards{openrails.StaffReads: h.read}, "guards none of"},
		"no guards":                    {false, nil, "guards none of"},
		"configuration uncovered":      {true, levels, "guards none of"},
		"a guard for unmounted routes": {false, with(openrails.Guards{openrails.MerchantConfig: h.admin}), "openrails.MerchantConfig covers no mounted route"},
		"two resources of one route":   {true, with(openrails.Guards{openrails.MerchantConfig: h.admin, openrails.Catalog: h.admin, openrails.Refunds: h.refunds}), "both guard ArchiveProduct"},
		"an unknown route set":         {false, with(openrails.Guards{openrails.RouteSet("route:Nope"): h.read}), "is not a RouteSet"},
		"an empty permission":          {false, with(openrails.Guards{openrails.Refunds: iam.Perm{}}), "empty permission"},
	} {
		_, err := h.mount(t, client, tc.config, tc.guards)
		require.ErrorContains(t, err, tc.want, name)
	}
	// Naming the route itself settles the conflict.
	_, err := h.mount(t, client, true, with(openrails.Guards{openrails.MerchantConfig: h.admin, openrails.Catalog: h.admin, openrails.Refunds: h.refunds, openrails.ArchiveProduct: h.admin}))
	require.NoError(t, err)
}
