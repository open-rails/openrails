//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/pricing"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestStandaloneMerchantMeteringRoutesHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"metering-admin-"+uuid.NewString(),
		[]string{
			controlplane.PermMerchantCatalogRead,
			controlplane.PermMerchantCatalogUpdate,
			controlplane.PermMerchantCustomerSettingsRead,
			controlplane.PermMerchantCustomerSettingsUpdate,
		},
	)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	productID := createMeteringProduct(t, surface.BaseURL, token, "metered-product-"+suffix)
	meterKey := "api.requests-" + suffix
	meterURL := surface.BaseURL + "/v1/merchant/catalog/meters/" + meterKey
	rateCardURL := meterURL + "/rate-card"

	status, body := requestJSON(t, http.MethodPut, meterURL, token, map[string]any{
		"aggregation": 1,
	})
	require.Equal(t, http.StatusBadRequest, status, string(body))

	meterRequest := map[string]any{
		"event_type":     "api.request",
		"value_property": "units",
		"aggregation":    pricing.AggregationSum,
		"unit":           "request",
		"group_by": map[string]string{
			"region": "metadata.region",
		},
	}
	status, body = requestJSON(t, http.MethodPut, meterURL, token, meterRequest)
	require.Equal(t, http.StatusOK, status, string(body))
	var created struct {
		billingservice.UsageMeterDTO
		ConfigurationSource string `json:"configuration_source"`
		WritesAllowed       bool   `json:"writes_allowed"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	require.Equal(t, strings.ReplaceAll(meterKey, ".", "-"), created.Key)
	require.Equal(t, config.MerchantSourceAPI, created.ConfigurationSource)
	require.True(t, created.WritesAllowed)

	status, body = requestJSON(t, http.MethodPut, meterURL, token, meterRequest)
	require.Equal(t, http.StatusOK, status, string(body))
	var repeated billingservice.UsageMeterDTO
	require.NoError(t, json.Unmarshal(body, &repeated))
	require.Equal(t, created.UpdatedAt, repeated.UpdatedAt, "idempotent meter put must not rewrite the row")

	status, body = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/meters?limit=1&offset=0", token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var page struct {
		Items  []json.RawMessage `json:"items"`
		Total  int64             `json:"total"`
		Limit  int               `json:"limit"`
		Offset int               `json:"offset"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	require.NotEmpty(t, page.Items)
	require.Positive(t, page.Total)
	require.Equal(t, 1, page.Limit)
	require.Zero(t, page.Offset)

	status, body = requestJSON(t, http.MethodGet, meterURL, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), `"configuration_source":"api"`)

	status, body = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/meters/missing-"+suffix, token, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	requireAPIErrorCode(t, body, "usage_meter_not_found")

	rateCard := map[string]any{
		"product_id": productID,
		"filter": map[string][]string{
			"region": {"eu"},
		},
		"price": map[string]any{
			"model":    pricing.ModelPerUnit,
			"currency": "USD",
			"per_unit": map[string]any{"unit_amount": 25},
		},
	}
	missingProduct := cloneJSONMap(t, rateCard)
	missingProduct["product_id"] = uuid.New()
	status, body = requestJSON(t, http.MethodPut, rateCardURL, token, missingProduct)
	require.Equal(t, http.StatusNotFound, status, string(body))
	requireAPIErrorCode(t, body, "rate_card_product_not_found")

	status, body = requestJSON(t, http.MethodPut, rateCardURL, token, rateCard)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), productID.String())
	status, body = requestJSON(t, http.MethodPut, rateCardURL, token, rateCard)
	require.Equal(t, http.StatusOK, status, string(body))

	overridesURL := meterURL + "/overrides"
	status, body = requestJSON(t, http.MethodGet, overridesURL, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), `"items":[]`)

	customerID := uuid.New()
	_, err := h.MerchantPool(dbtest.TestMerchantID.UUID()).Exec(ctx, `
INSERT INTO openrails.customers (id, merchant_id, subject)
VALUES ($1, $2, $3)`, customerID, dbtest.TestMerchantID.UUID(), "metering-customer-"+suffix)
	require.NoError(t, err)
	overrideURL := surface.BaseURL + "/v1/merchant/customers/" + customerID.String() +
		"/rate-overrides/" + created.Key
	status, body = requestJSON(t, http.MethodPut, overrideURL, token, map[string]any{
		"price": map[string]any{
			"model":    pricing.ModelPerUnit,
			"currency": "USD",
			"per_unit": map[string]any{"unit_amount": 20},
		},
	})
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = requestJSON(t, http.MethodGet, overridesURL+"?limit=10", token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), customerID.String())

	status, body = requestJSON(t, http.MethodDelete, rateCardURL, token, nil)
	require.Equal(t, http.StatusConflict, status, string(body))
	requireAPIErrorCode(t, body, "rate_card_has_overrides")

	_, err = h.MerchantPool(dbtest.TestMerchantID.UUID()).Exec(ctx, `
INSERT INTO openrails.usage_events
    (merchant_id, customer_id, invoker_id, currency, resource, event_type,
     amount, source, source_id, occurred_at)
VALUES ($1, $2, 'metering-test', 'USD', 'api', 'api.request',
        0, 'test', $3, $4)`,
		dbtest.TestMerchantID.UUID(),
		customerID,
		uuid.NewString(),
		time.Now().UTC(),
	)
	require.NoError(t, err)
	changedMeter := cloneJSONMap(t, meterRequest)
	changedMeter["unit"] = "token"
	status, body = requestJSON(t, http.MethodPut, meterURL, token, changedMeter)
	require.Equal(t, http.StatusConflict, status, string(body))
	requireAPIErrorCode(t, body, "meter_in_use")

	status, body = requestJSON(t, http.MethodDelete, overrideURL, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestJSON(t, http.MethodDelete, rateCardURL, token, nil)
	require.Equal(t, http.StatusNoContent, status, string(body))
	status, body = requestJSON(t, http.MethodDelete, rateCardURL, token, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	requireAPIErrorCode(t, body, "default_rate_card_not_found")
}

func createMeteringProduct(t *testing.T, baseURL, token, key string) uuid.UUID {
	t.Helper()
	status, body := requestJSON(t, http.MethodPost, baseURL+"/v1/merchant/catalog/products", token, map[string]any{
		"key":          key,
		"display_name": "Metered Product",
	})
	require.Equal(t, http.StatusCreated, status, string(body))
	var product struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &product))
	require.NotEqual(t, uuid.Nil, product.ID)
	return product.ID
}

func requireAPIErrorCode(t *testing.T, body []byte, code string) {
	t.Helper()
	var response api.ErrorResponse
	require.NoError(t, json.Unmarshal(body, &response))
	require.Equal(t, code, response.Error.Code)
}

func cloneJSONMap(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(source)
	require.NoError(t, err)
	var clone map[string]any
	require.NoError(t, json.Unmarshal(raw, &clone))
	return clone
}
