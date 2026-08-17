//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/pricing"
)

// TestMerchantMeteringRouteToInvoiceAcceptance proves the complete #805
// operator path: merchant HTTP owns the product, meter, default and negotiated
// contracts; the host reports zero-amount idempotent usage over HTTP; and the
// rating sweep produces stable, itemized invoices from those declarations.
func TestMerchantMeteringRouteToInvoiceAcceptance(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	standalone := h.StartStandalone("usd")
	standaloneToken := standalone.MintAPIKey(
		dbtest.TestMerchantSlug,
		"metering-acceptance-"+uuid.NewString(),
		[]string{
			controlplane.PermMerchantCatalogRead,
			controlplane.PermMerchantCatalogUpdate,
			controlplane.PermMerchantCustomerSettingsRead,
			controlplane.PermMerchantCustomerSettingsUpdate,
			controlplane.PermMerchantAdmissionsCreate,
		},
	)
	meterKey := proveMerchantMeteringRouteToInvoice(t, ctx, h, standalone, standaloneToken)

	// Embedded manifest-owned deployments expose the same reads, but their
	// catalog remains declaration-owned and therefore refuses runtime writes.
	embedded := h.StartEmbeddedHost("usd")
	meterURL := embedded.BaseURL + "/v1/merchant/catalog/meters/" + meterKey
	status, body := requestJSON(t, http.MethodGet, meterURL, embedded.Token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), `"configuration_source":"manifest"`)
	require.Contains(t, string(body), `"writes_allowed":false`)

	status, body = requestJSON(t, http.MethodPut, meterURL, embedded.Token, map[string]any{
		"aggregation": pricing.AggregationCount,
	})
	require.Equal(t, http.StatusMethodNotAllowed, status, string(body))
	requireAPIErrorCode(t, body, "manifest_driven")
}

func proveMerchantMeteringRouteToInvoice(
	t *testing.T,
	ctx context.Context,
	h *Harness,
	surface *Surface,
	token string,
) string {
	t.Helper()
	mctx := dbtest.WithTestMerchant(ctx)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	productID := createMeteringProduct(t, surface.BaseURL, token, "acceptance-product-"+suffix)
	meterKey := "acceptance-units-" + suffix
	eventType := "acceptance.usage." + suffix
	meterURL := surface.BaseURL + "/v1/merchant/catalog/meters/" + meterKey
	rateURL := meterURL + "/rate-card"

	status, body := requestJSON(t, http.MethodPut, meterURL, token, map[string]any{
		"event_type":     eventType,
		"value_property": "units",
		"aggregation":    pricing.AggregationSum,
		"unit":           "unit",
		"group_by": map[string]string{
			"region": "metadata.region",
		},
	})
	require.Equal(t, http.StatusOK, status, string(body))

	defaultCard := map[string]any{
		"product_id": productID,
		"filter": map[string][]string{
			"region": {"eu"},
		},
		"price": map[string]any{
			"model":    pricing.ModelPerUnit,
			"currency": "USD",
			"per_unit": map[string]any{"unit_amount": 100_000, "divide_by": 1},
		},
	}
	status, body = requestJSON(t, http.MethodPut, rateURL, token, defaultCard)
	require.Equal(t, http.StatusOK, status, string(body))

	other := surface.ProvisionOwnedMerchant("metering-isolation-" + suffix)
	status, body = requestJSON(t, http.MethodGet, meterURL, other.APIKey, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	requireAPIErrorCode(t, body, "usage_meter_not_found")

	merchantID := dbtest.TestMerchantID.UUID()
	pool := h.MerchantPool(merchantID)
	defaultCustomer := uuid.New()
	negotiatedCustomer := uuid.New()
	for _, customerID := range []uuid.UUID{defaultCustomer, negotiatedCustomer} {
		_, err := pool.Exec(ctx, `
INSERT INTO openrails.customers (id, merchant_id, subject)
VALUES ($1, $2, $3)`, customerID, merchantID, "metering-acceptance-"+customerID.String())
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, customerID := range []uuid.UUID{defaultCustomer, negotiatedCustomer} {
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", customerID)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", customerID)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", customerID)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", customerID)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", customerID)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.customers WHERE id = $1", customerID)
		}
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	svc := money.NewMoneyService(h.MerchantDB(merchantID))
	arrears := money.BillingModeArrears
	for _, customerID := range []uuid.UUID{defaultCustomer, negotiatedCustomer} {
		_, err := svc.UpsertAccountSettings(
			mctx,
			identity.CustomerID(customerID),
			money.DefaultCurrency,
			money.AccountSettingsInput{BillingMode: &arrears},
		)
		require.NoError(t, err)
	}

	overrideURL := surface.BaseURL + "/v1/merchant/customers/" +
		negotiatedCustomer.String() + "/rate-overrides/" + meterKey
	status, body = requestJSON(t, http.MethodPut, overrideURL, token, map[string]any{
		"price": map[string]any{
			"model":    pricing.ModelPerUnit,
			"currency": "USD",
			"per_unit": map[string]any{"unit_amount": 40_000, "divide_by": 1},
		},
		"allowance": map[string]any{"included": 10},
	})
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = requestJSON(t, http.MethodGet, meterURL+"/overrides", token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), negotiatedCustomer.String())
	status, body = requestJSON(t, http.MethodDelete, rateURL, token, nil)
	require.Equal(t, http.StatusConflict, status, string(body))
	requireAPIErrorCode(t, body, "rate_card_has_overrides")

	firstOccurred := time.Now().UTC().Truncate(time.Second)
	for _, customerID := range []uuid.UUID{defaultCustomer, negotiatedCustomer} {
		usage := usageReport(customerID, eventType, "period-one-"+customerID.String(), firstOccurred, 25)
		status, body = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/usage/report", token, usage)
		require.Equal(t, http.StatusOK, status, string(body))
		status, body = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/usage/report", token, usage)
		require.Equal(t, http.StatusOK, status, "identical usage retry must be idempotent: "+string(body))
	}

	from, to := firstOccurred.Add(-time.Hour), firstOccurred.Add(time.Hour)
	defaultInvoice, err := svc.FinalizeInvoice(mctx, identity.CustomerID(defaultCustomer), "USD", from, to)
	require.NoError(t, err)
	negotiatedInvoice, err := svc.FinalizeInvoice(mctx, identity.CustomerID(negotiatedCustomer), "USD", from, to)
	require.NoError(t, err)
	require.Equal(t, int64(2_500_000), defaultInvoice.AmountDue)
	require.Equal(t, int64(600_000), negotiatedInvoice.AmountDue)
	requireMeteredLine(t, defaultInvoice.LineItems, meterKey, 2_500_000)
	requireMeteredLine(t, negotiatedInvoice.LineItems, meterKey, 600_000)

	updatedDefault := cloneJSONMap(t, defaultCard)
	updatedDefault["price"] = map[string]any{
		"model":    pricing.ModelPerUnit,
		"currency": "USD",
		"per_unit": map[string]any{"unit_amount": 200_000, "divide_by": 1},
	}
	status, body = requestJSON(t, http.MethodPut, rateURL, token, updatedDefault)
	require.Equal(t, http.StatusOK, status, string(body))
	var persistedAmount int64
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT amount_due FROM openrails.invoices WHERE id = $1",
		negotiatedInvoice.ID,
	).Scan(&persistedAmount))
	require.Equal(t, int64(600_000), persistedAmount, "replacing a card must not rewrite finalized history")

	status, body = requestJSON(t, http.MethodDelete, overrideURL, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	secondOccurred := firstOccurred.Add(3 * time.Hour)
	status, body = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/usage/report", token,
		usageReport(negotiatedCustomer, eventType, "period-two", secondOccurred, 25))
	require.Equal(t, http.StatusOK, status, string(body))
	restoredInvoice, err := svc.FinalizeInvoice(
		mctx,
		identity.CustomerID(negotiatedCustomer),
		"USD",
		secondOccurred.Add(-time.Hour),
		secondOccurred.Add(time.Hour),
	)
	require.NoError(t, err)
	require.Equal(t, int64(5_000_000), restoredInvoice.AmountDue)
	requireMeteredLine(t, restoredInvoice.LineItems, meterKey, 5_000_000)

	return meterKey
}

func usageReport(customerID uuid.UUID, eventType, sourceID string, occurredAt time.Time, units int64) map[string]any {
	return map[string]any{
		"customer_id":      customerID.String(),
		"currency":         "USD",
		"event_type":       eventType,
		"dimensions":       map[string]int64{"units": units},
		"metadata":         map[string]any{"region": "eu"},
		"amount":           0,
		"source":           "metering-acceptance",
		"source_id":        sourceID,
		"occurred_at_unix": occurredAt.Unix(),
	}
}

func requireMeteredLine(t *testing.T, lines []models.InvoiceLineItem, meterKey string, amount int64) {
	t.Helper()
	for _, line := range lines {
		if line.EventType == "metered:"+meterKey {
			require.Equal(t, amount, line.Amount)
			return
		}
	}
	require.Fail(t, "metered invoice line missing", meterKey)
}
