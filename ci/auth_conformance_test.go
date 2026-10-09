//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
)

// readmeAuth is the README's AuthKit adapter (examples/embedded billingAuth).
type readmeAuth struct{ ak *authkit.Client }

func (a readmeAuth) Required() func(http.Handler) http.Handler { return verify.RequireSession(a.ak) }

func (a readmeAuth) RequirePermission(p string) func(http.Handler) http.Handler {
	perm, err := a.ak.Permission(p)
	if err != nil {
		panic(err)
	}
	return verify.RequirePermissionOn(a.ak, iam.RootGroup(), perm)
}

func (a readmeAuth) Sensitive() func(http.Handler) http.Handler { return verify.Sensitive(a.ak) }

func (a readmeAuth) Identity(ctx context.Context) (openrails.Identity, bool) {
	cl, ok := verify.ClaimsFromContext(ctx)
	if !ok || cl.Kind != iam.ActorUser || cl.UserID == "" {
		return openrails.Identity{}, false
	}
	credential := openrails.Credential{Kind: openrails.CredentialSession, ID: cl.SessionID}
	if cl.DeviceKeyID != "" {
		credential = openrails.Credential{Kind: openrails.CredentialDeviceKey, ID: cl.DeviceKeyID}
	}
	return openrails.Identity{
		Issuer: cl.Issuer, Subject: cl.UserID, SubjectKind: openrails.SubjectUser,
		Invoker: openrails.Invoker{Issuer: cl.Issuer, ID: cl.UserID}, Credential: credential,
		Email: cl.Email, Username: cl.Username, EmailVerified: cl.EmailVerified,
	}, true
}

// The README's adapter over a real AuthKit passes the conformance kit, and
// guards a mounted engine: a customer reaches only their own billing, staff
// the merchant API, and nobody signed in creates a checkout.
func TestAuthKitAdapterGuardsTheMount(t *testing.T) {
	f := newFixture(t)
	rbac := authkit.NewRoles()
	merchant := rbac.Persona("merchant")
	for _, p := range append(openrails.Permissions(), openrails.MachinePermissions()...) {
		parts := strings.SplitN(p, ":", 3)
		merchant.Permission(parts[1], parts[2])
	}
	// The README grants staff only the permissions a person may hold; this
	// owner holds merchant:*, the machine-only ones included.
	staffRole := rbac.Root.Role("billing-staff", merchant.All())
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) { c.Roles = rbac }))
	ak := as.Client
	auth := readmeAuth{ak}

	customer, staff, gone := authtest.NewUser(t, ak), authtest.NewUser(t, ak), authtest.NewUser(t, ak)
	authtest.GrantRole(t, ak, iam.RootGroup(), iam.UserSubject(staff.ID), staffRole)
	customerToken := authtest.SignIn(t, ak, customer).AccessToken
	staffToken := authtest.SignIn(t, ak, staff).AccessToken
	goneToken := authtest.SignIn(t, ak, gone).AccessToken
	_, err := ak.RevokeAccountSessions(t.Context(), iam.UserActor(gone.ID), gone.ID)
	require.NoError(t, err)
	request := func(token string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/payments", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}
	}
	openrailstest.CheckAuth(t, auth, openrailstest.AuthCases{
		Permission: billing.MerchantPaymentsRefund,
		Customer:   request(customerToken),
		Staff:      request(staffToken),
		Refused:    map[string]func() *http.Request{"forged": request(customerToken + "x"), "signed out": request(goneToken)},
	})

	client := f.runtime(t, "authkit-"+uuid.NewString()[:8])
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, client, openrails.Routes{Auth: auth, Prefix: "/billing", Merchant: true, Customers: openrails.CustomerSelfService}))
	serve := func(token, method, path string) int {
		return call(t, mux, token, method, "/billing"+path, "", nil).Code
	}
	require.Equal(t, http.StatusOK, serve(customerToken, http.MethodGet, "/v1/me/payment-methods"))
	require.Equal(t, http.StatusForbidden, serve(customerToken, http.MethodGet, "/v1/merchant/payments"))
	require.Equal(t, http.StatusOK, serve(staffToken, http.MethodGet, "/v1/merchant/payments"))
	require.Equal(t, http.StatusUnauthorized, serve(goneToken, http.MethodGet, "/v1/me/payment-methods"))
	w := call(t, mux, staffToken, http.MethodPost, "/billing/v1/merchant/checkout-sessions", "", map[string]any{"customer": map[string]any{"id": customer.ID}, "price_id": billing.PriceID(uuid.New())})
	require.Equal(t, http.StatusForbidden, w.Code, "merchant:* never lets a person start a purchase: %s", w.Body.String())
	require.Contains(t, w.Body.String(), billing.CodePermissionRequired)
}
