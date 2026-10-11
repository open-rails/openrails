//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/engine"
)

// publicConfig reads the deployment's GET /v1/config, as any browser does.
func publicConfig(t *testing.T, bill *openrails.Engine) billing.PublicConfig {
	t.Helper()
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: authtest.Deny{}}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cfg billing.PublicConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	return cfg
}

// configReader is the customer whose browser reads the merchant's setup.
const configReader = "00000000-0000-4000-8000-0000000c0f19"

// customerConfig reads GET /v1/me/config as one of bill's merchant's
// customers, as a signed-in browser does.
func customerConfig(t *testing.T, bill *openrails.Engine) billing.CustomerConfig {
	t.Helper()
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))
	req := httptest.NewRequest(http.MethodGet, "/v1/me/config", nil)
	// A host's token for the customer, bound to the merchant's group as a
	// group-bound mint is.
	mid := engine.Graph(bill).Runtime.DeclaredMerchants()[0]
	req.Header.Set("Authorization", "Bearer "+auth.Issue(authtest.Grant{Identity: authtest.User(configReader), Bound: authtest.MerchantScope(mid)}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cfg billing.CustomerConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	return cfg
}

// revokeAccess takes a product-access window back.
func revokeAccess(client *openrails.Client, ctx context.Context, id billing.ProductAccessID) error {
	return client.RevokeProductAccess(ctx, id, billing.RevokeProductAccessParams{Reason: "test"})
}
