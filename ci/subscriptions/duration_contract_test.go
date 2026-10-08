//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestCatalogDurationHumanAPIContract(t *testing.T) {
	w := newWorld(t)
	key := "durations-" + uuid.NewString()[:8]
	apply := func(price map[string]any, create bool) billing.CatalogApplicationReceipt {
		t.Helper()
		product := map[string]any{"key": key, "prices": []any{price}}
		if create {
			product["display_name"] = "Duration contract"
		}
		var receipt billing.CatalogApplicationReceipt
		status := w.staffCall(http.MethodPost, "/v1/merchant/catalog/applications", map[string]any{
			"schema_version": 1, "products": []any{product},
		}, &receipt)
		require.Equal(t, http.StatusOK, status, "%+v", receipt)
		return receipt
	}
	read := func(priceKey string, access, interval *int) *billing.Price {
		t.Helper()
		price, err := w.client[remote].GetPriceByKey(t.Context(), key, priceKey)
		require.NoError(t, err)
		require.Equal(t, access, price.AccessDurationHours)
		require.Equal(t, interval, price.BillingIntervalHours)
		return price
	}
	initial := apply(map[string]any{
		"key": "mixed", "currency": "USD", "unit_amount": "10000000",
		"access_duration": "3 days", "billing_interval": "30 days",
	}, true)
	require.False(t, initial.Replayed)
	original := read("mixed", new(72), new(720))
	for _, terms := range []map[string]any{
		{"access_duration": "72 hours", "billing_interval": "720 hours"},
		{"access_duration_hours": 72, "billing_interval_hours": 720},
	} {
		terms["key"], terms["currency"], terms["unit_amount"] = "mixed", "USD", "10000000"
		replay := apply(terms, true)
		require.True(t, replay.Replayed, "equivalent durations must address the same catalog application")
		require.Equal(t, initial.ApplicationID, replay.ApplicationID)
		require.Equal(t, original.ID, read("mixed", new(72), new(720)).ID)
	}
	// The YAML HTTP entry point shares the normalized JSON replay identity.
	yaml := fmt.Sprintf("schema_version: 1\nproducts:\n- key: %s\n  display_name: Duration contract\n  prices:\n  - key: mixed\n    currency: USD\n    unit_amount: 10000000\n    access_duration: 72 hours\n    billing_interval: 30 days\n", key)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+mountPrefix+"/v1/merchant/catalog/applications", strings.NewReader(yaml))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/yaml")
	request.Header.Set("Authorization", "Bearer "+w.auth.token(t, "staff"))
	request.Header.Set("OpenRails-Merchant", w.slug)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var yamlReplay billing.CatalogApplicationReceipt
	require.NoError(t, json.NewDecoder(response.Body).Decode(&yamlReplay))
	require.True(t, yamlReplay.Replayed)
	require.Equal(t, initial.ApplicationID, yamlReplay.ApplicationID)

	apply(map[string]any{"key": "mixed", "unit_amount": "12000000"}, false)
	repriced := read("mixed", new(72), new(720))
	require.NotEqual(t, original.ID, repriced.ID)
	require.EqualValues(t, 12_000_000, repriced.UnitAmount)
	apply(map[string]any{"key": "mixed", "access_duration": nil}, false)
	read("mixed", nil, new(720))
	apply(map[string]any{"key": "mixed", "billing_interval": nil}, false)
	read("mixed", nil, nil)

	apply(map[string]any{"key": "finite", "currency": "USD", "unit_amount": "1000000", "access_duration": "3 days"}, false)
	read("finite", new(72), nil)
	apply(map[string]any{"key": "recurring", "currency": "USD", "unit_amount": "2000000", "billing_interval": "30 days"}, false)
	read("recurring", nil, new(720))
	apply(map[string]any{"key": "trial", "currency": "USD", "unit_amount": "3000000", "billing_interval": "30 days", "trial_unit_amount": "0", "trial_duration": "1 day"}, false)
	trial := read("trial", nil, new(720))
	require.Equal(t, new(24), trial.TrialDurationHours)
	require.Equal(t, new(int64(0)), trial.TrialUnitAmount)
}

func TestCatalogDurationCreatePriceAPIContract(t *testing.T) {
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			client := w.client[tp]
			product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "price-duration-" + string(tp), DisplayName: "Independent durations"})
			require.NoError(t, err)
			for _, tc := range []struct {
				key      string
				access   *int
				interval *int
			}{
				{"permanent", nil, nil},
				{"finite", new(72), nil},
				{"recurring-permanent", nil, new(720)},
				{"recurring-finite", new(72), new(720)},
			} {
				price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{
					ProductID: product.ID, Key: tc.key, Currency: "USD", UnitAmount: 10_000_000,
					AccessDurationHours: tc.access, BillingIntervalHours: tc.interval,
				})
				require.NoError(t, err, tc.key)
				read, err := client.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
				require.NoError(t, err)
				require.Equal(t, tc.access, read.AccessDurationHours, tc.key)
				require.Equal(t, tc.interval, read.BillingIntervalHours, tc.key)
				var wire map[string]any
				require.Equal(t, http.StatusOK, w.staffCall(http.MethodGet, "/v1/merchant/catalog/prices/"+price.ID.String(), nil, &wire))
				require.NotContains(t, wire, "auto_renew", "renewal choice must not be a catalog property")
			}
		})
	}
}

func TestCatalogDurationInvalidAPIContract(t *testing.T) {
	w := newWorld(t)
	product, err := w.client[remote].CreateProduct(t.Context(), billing.CreateProductParams{Key: "duration-validation", DisplayName: "Validation"})
	require.NoError(t, err)
	revision, err := w.client[remote].GetCatalogRevision(t.Context())
	require.NoError(t, err)
	for _, value := range []bool{true, false} {
		status, body := w.staffJSON(http.MethodPost, "/v1/merchant/catalog/prices", map[string]any{
			"product_id": product.ID.String(), "key": "invalid", "currency": "USD", "unit_amount": "1000000", "auto_renew": value,
		})
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
	}
	for _, invalid := range []map[string]any{
		{"access_duration": "0 hours"},
		{"billing_interval_hours": 0},
		{"access_duration": "3 days", "access_duration_hours": 72},
		{"billing_interval": nil, "billing_interval_hours": nil},
		{"auto_renew": false},
	} {
		invalid["key"], invalid["currency"], invalid["unit_amount"] = "invalid", "USD", "1000000"
		status, body := w.staffJSON(http.MethodPost, "/v1/merchant/catalog/applications", map[string]any{
			"schema_version": 1, "products": []any{map[string]any{"key": product.Key, "prices": []any{invalid}}},
		})
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
	}
	after, err := w.client[remote].GetCatalogRevision(t.Context())
	require.NoError(t, err)
	require.Equal(t, revision.Revision, after.Revision, "invalid duration declarations cannot mutate the catalog")
}

func TestCheckoutOrderRenewalAPIContract(t *testing.T) {
	w := newWorld(t)
	product, err := w.client[remote].CreateProduct(t.Context(), billing.CreateProductParams{
		Key: "renewal-choice", DisplayName: "Order renewal", EntitlementsSpec: map[string]*int{"content:renewal-choice": nil},
	})
	require.NoError(t, err)
	price, err := w.client[remote].CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID: product.ID, Key: "monthly", Currency: "USD", UnitAmount: 10_000_000,
		BillingIntervalHours: new(720), AccessDurationHours: new(72),
	})
	require.NoError(t, err)
	customer := w.newCustomer()
	for _, choice := range []*bool{nil, new(true), new(false)} {
		wantRenew := choice == nil || *choice
		for _, tp := range []topology{embedded, remote} {
			link, err := w.client[tp].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{
				Customer: customer.identity(), PriceID: price.ID, AutoRenew: choice,
			})
			require.NoError(t, err)
			session := hostedSession{w: w, id: link.ID}
			plan := session.read()["plan"].(map[string]any)
			require.Equal(t, wantRenew, plan["auto_renew"])
			require.EqualValues(t, 720, plan["billing_interval_hours"])
			require.EqualValues(t, 72, plan["access_duration_hours"])
		}
		body := map[string]any{"price_id": price.ID.String()}
		if choice != nil {
			body["auto_renew"] = *choice
		}
		session := customer.session(body)
		require.Equal(t, wantRenew, session.read()["plan"].(map[string]any)["auto_renew"])
	}
}
