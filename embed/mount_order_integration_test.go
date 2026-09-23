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
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// Constructor declarations bind the merchant before the first mounted request.
func TestMountedHandlerUsesConstructorMerchant(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	slug := fmt.Sprintf("mount-order-%d", time.Now().UnixNano())
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dsn}, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), Merchant: &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{DisplayName: slug}}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	noAuth := billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	})
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Checkout: true}, Authenticator: noAuth})
	require.NoError(t, err)

	// The host registers these routes at /v1 with no path rewrite.
	req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
