//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	embgin "github.com/open-rails/openrails/pkg/embedded/gin"
)

// TestRegisterAPIEndToEnd proves the AuthKit-style RegisterAPI front door (#623):
// route-group selection, prefix inference from the gin group, customer gating,
// ActiveRouteSets, and the GET /v1/capabilities discovery endpoint.
func TestRegisterAPIEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
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
				Env: "dev",
				DB:  &config.DBConfig{URL: h.DSN},
			},
			Redis: h.Redis,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// Case 1: customer omitted -> /v1/me is not mounted, capabilities.customer=false.
	eng1 := gin.New()
	grp1 := eng1.Group("/billing")
	require.NoError(t, embgin.RegisterAPI(grp1, rt.Embedded(),
		embgin.WithGroups(embedded.RouteSetCheckout, embedded.RouteSetWebhooks),
		embgin.WithAuthenticator(authn),
		embgin.WithProviderRoutes(embgin.ProviderRoutes{}),
	))
	require.ElementsMatch(t,
		[]embedded.RouteSet{embedded.RouteSetCheckout, embedded.RouteSetWebhooks},
		rt.ActiveRouteSets())

	w := doMounted(eng1, http.MethodGet, "/billing/v1/me/balance?currency=USD", nil)
	require.Equal(t, http.StatusNotFound, w.Code, "customer omitted -> /v1/me must 404")

	caps1 := getCapabilities(t, eng1)
	require.True(t, caps1.RouteGroups["checkout"])
	require.False(t, caps1.RouteGroups["webhooks"])
	require.False(t, caps1.RouteGroups["customer"])
	require.False(t, caps1.RouteGroups["payment_providers"])
	require.False(t, caps1.Routes["billing_portal"])
	require.False(t, caps1.Routes["solana"])
	require.False(t, caps1.Routes["webhooks"])

	// Case 2: customer included -> /v1/me mounted, capabilities.customer=true.
	eng2 := gin.New()
	grp2 := eng2.Group("/billing")
	require.NoError(t, embgin.RegisterAPI(grp2, rt.Embedded(),
		embgin.WithGroups(embedded.RouteSetCheckout, embedded.RouteSetCustomer, embedded.RouteSetWebhooks),
		embgin.WithAuthenticator(authn),
		embgin.WithDelegatedAuthenticator(delegated),
		embgin.WithProviderRoutes(embgin.ProviderRoutes{}),
	))
	require.Contains(t, rt.ActiveRouteSets(), embedded.RouteSetCustomer)

	caps2 := getCapabilities(t, eng2)
	require.True(t, caps2.RouteGroups["customer"], "customer selected -> advertised true even though the user handler strips it")

	w = doMounted(eng2, http.MethodGet, "/billing/v1/me/balance?currency=USD", nil)
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
