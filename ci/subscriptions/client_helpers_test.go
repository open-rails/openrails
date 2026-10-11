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
)

// publicConfig reads bill's public GET /v1/config, as a browser does.
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

// revokeAccess takes a product-access window back.
func revokeAccess(client *openrails.Client, ctx context.Context, id billing.ProductAccessID) error {
	return client.RevokeProductAccess(ctx, id, billing.RevokeProductAccessParams{Reason: "test"})
}
