//go:build integration

package integrationharness

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/catalog"
)

func TestCatalogPublishRefusesAmbiguousPriceKeys(t *testing.T) {
	h := New(t, t.Context())
	surface := h.StartStandalone("USD")
	owned := surface.ProvisionOwnedMerchant("catalog-collision-" + uuid.NewString()[:8])
	token := surface.MintAPIKey(owned.MerchantSlug, "catalog", []string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate})
	m := catalog.Manifest{Version: catalog.SupportedVersion, Products: []catalog.Product{{Key: "ambiguous", DisplayName: "Ambiguous", Prices: []catalog.Price{
		{Currency: "USD", UnitAmount: 9_990_000, Duration: "30d", AutoRenew: true},
		{Currency: "USD", UnitAmount: 4_990_000, Duration: "30d", AutoRenew: true},
	}}}}
	status, raw := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{"catalog": m, "insert": true})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	var response struct {
		Error openrails.ErrorDetails `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, "invalid_param", response.Error.Code)
	require.NotNil(t, response.Error.Param)
	require.Equal(t, "key", *response.Error.Param)
	require.Contains(t, response.Error.Message, "ambiguous-monthly")
	require.Contains(t, response.Error.Message, "disambiguate")
	client := surface.Client(openrails.WithAPIKey(token), openrails.WithMerchantID(owned.MerchantID))
	products, err := client.ListProducts(t.Context(), openrails.ProductFilter{})
	require.NoError(t, err)
	require.Empty(t, products.Items, "bad input cannot install a partial catalog")
}
