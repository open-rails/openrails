//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestMountHandlerRouteSelection proves the neutral MountHandler front door
// (#623, gin-free since #670): route-group selection, MountPrefix stripping,
// customer gating, ActiveRouteSets, and the GET /v1/capabilities discovery
// endpoint.
func TestMountHandlerRouteSelection(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	dbtest.EnsureTestMerchant(ctx, t, h.sharedPool())

	subject := uuid.New()
	delegated := billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{
			MerchantID:   dbtest.TestMerchantID.String(),
			MerchantSlug: dbtest.TestMerchantSlug,
			SubjectID:    subject.String(),
			Issuer:       "embedded-host",
			Permissions:  []string{permissions.MerchantAll},
		}, nil
	})
	// Non-nil so checkout passes the auth boundary; its behavior is irrelevant to
	// the routes this test exercises (capabilities is public; /v1/me uses delegated).
	authn := billingauth.AuthenticatorFunc(func(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	})

	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{
			Config: &config.Config{
				Env:      "dev",
				TestMode: config.CredentialPostureSandbox,
				DB:       &config.DBConfig{URL: h.DSN},
			},
			Redis: h.Redis,
			River: embedded.RiverManagedByOpenRails(),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// Case 1: customer omitted -> /v1/me is not mounted, capabilities.customer=false.
	h1, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		MountPrefix:    "/billing",
		RouteSets:      []embedded.RouteSet{embedded.RouteSetCheckout, embedded.RouteSetWebhooks},
		Authenticator:  authn,
		ProviderRoutes: &embedded.ProviderRoutes{},
	})
	require.NoError(t, err)
	require.ElementsMatch(t,
		[]embedded.RouteSet{embedded.RouteSetCheckout, embedded.RouteSetWebhooks},
		rt.ActiveRouteSets())

	w := doMounted(h1, http.MethodGet, "/billing/v1/me/balance?currency=USD", nil)
	require.Equal(t, http.StatusNotFound, w.Code, "customer omitted -> /v1/me must 404")

	caps1 := getCapabilities(t, h1)
	require.True(t, caps1.RouteGroups["checkout"])
	require.False(t, caps1.RouteGroups["webhooks"])
	require.False(t, caps1.RouteGroups["customer"])
	require.False(t, caps1.RouteGroups["payment_providers"])
	require.False(t, caps1.Routes["billing_portal"])
	require.False(t, caps1.Routes["solana"])
	require.False(t, caps1.Routes["webhooks"])

	// Case 2: customer included -> /v1/me mounted, capabilities.customer=true.
	h2, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		MountPrefix:            "/billing",
		RouteSets:              []embedded.RouteSet{embedded.RouteSetCheckout, embedded.RouteSetCustomer, embedded.RouteSetWebhooks},
		Authenticator:          authn,
		DelegatedAuthenticator: delegated,
		ProviderRoutes:         &embedded.ProviderRoutes{},
	})
	require.NoError(t, err)
	require.Contains(t, rt.ActiveRouteSets(), embedded.RouteSetCustomer)

	caps2 := getCapabilities(t, h2)
	require.True(t, caps2.RouteGroups["customer"], "customer selected -> advertised true even though the user handler strips it")

	w = doMounted(h2, http.MethodGet, "/billing/v1/me/balance?currency=USD", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func getCapabilities(t *testing.T, h http.Handler) struct {
	RouteGroups map[string]bool `json:"route_groups"`
	Routes      map[string]bool `json:"routes"`
} {
	t.Helper()
	w := doMounted(h, http.MethodGet, "/billing/v1/capabilities", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var caps struct {
		RouteGroups map[string]bool `json:"route_groups"`
		Routes      map[string]bool `json:"routes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &caps), w.Body.String())
	return caps
}
