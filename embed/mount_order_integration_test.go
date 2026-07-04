//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestMountedHandlerResolvesMerchantBoundAfterMount pins the #744 fix: the
// embedded HTTP surface (pkg/embedded.MountHandler -> embedhttp.NewHTTPHandler)
// resolves Runtime.ConfiguredMerchant() PER REQUEST rather than baking a
// snapshot into the middleware chain at mount time. Before the fix, mounting
// the handler BEFORE UpsertMerchantConfig bound the merchant pinned the
// surface to the zero merchant FOREVER (mount-time bake), even though the SDK
// (embed/transport.go's live read) worked fine — an order-sensitive,
// unenforced split brain. This test drives that exact order: mount, THEN
// bind, THEN request.
func TestMountedHandlerResolvesMerchantBoundAfterMount(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	slug := fmt.Sprintf("mount-order-%d", time.Now().UnixNano())
	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// Mount BEFORE any merchant is bound: Runtime.ConfiguredMerchant() is zero
	// at this instant. RouteSetCheckout requires an Authenticator (unused by
	// the public GET /products route below; billingauth.Optional tolerates
	// its failure and proceeds unauthenticated).
	noAuth := billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	})
	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:     []embedded.RouteSet{embedded.RouteSetCheckout},
		Authenticator: noAuth,
	})
	require.NoError(t, err)

	// Bind the merchant AFTER the handler is already mounted and could be
	// serving traffic — the exact order the #744 bug broke.
	_, err = rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)

	// Empty MountOptions.MountPrefix means incoming paths already arrive
	// canonical ("/v1/..."); combinedMount adds the "/billing" segment the
	// mux registered under before dispatching.
	req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Before the fix this 500s: ResolveMerchantHTTP pinned the zero merchant
	// into the middleware chain at mount time, so MerchantDBConnMW's
	// merchant.Require fails on every request forever, regardless of any
	// later bind. After the fix, ConfiguredMerchant() is resolved live on
	// every request and reflects the bind — an ordinary (empty) product list.
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
