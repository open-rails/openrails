//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func applyHumanAmountYAML(t *testing.T, w *world, document string) (int, billing.CatalogApplicationReceipt) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+mountPrefix+"/v1/merchant/catalog/applications", strings.NewReader(document))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/yaml")
	request.Header.Set("Authorization", "Bearer "+w.auth.token(t, "staff"))
	request.Header.Set("OpenRails-Merchant", w.slug)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	var receipt billing.CatalogApplicationReceipt
	if response.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(response.Body).Decode(&receipt))
	}
	return response.StatusCode, receipt
}

func TestHumanAmountCatalogUsesExactCurrencyNativeUnits(t *testing.T) {
	w := newWorld(t)
	for _, tc := range []struct {
		key, amount, currency string
		native                int64
	}{
		{"dollars", "9.99 USD", "USD", 9_990_000},
		{"euro", "9.99 eur", "EUR", 9_990_000},
		{"yen", "10 JPY", "JPY", 100_000},
		{"sol", "1 SOL", "SOL", 1_000_000_000},
		{"usdc", "10 USDC", "USDC", 10_000_000},
		{"micro-dollar", "0.000001 USD", "USD", 1},
		{"native-yen", "0.0001 JPY", "JPY", 1},
		{"lamport", "0.000000001 SOL", "SOL", 1},
		{"native-usdc", "0.000001 USDC", "USDC", 1},
		{"zero-sol", "0 SOL", "SOL", 0},
		{"zero-usd", "0 USD", "USD", 0},
		{"max-dollar", "9223372036854.775807 USD", "USD", math.MaxInt64},
		{"max-sol", "9223372036.854775807 SOL", "SOL", math.MaxInt64},
		{"max-usdc", "9223372036854.775807 USDC", "USDC", math.MaxInt64},
	} {
		t.Run(tc.key, func(t *testing.T) {
			key := "human-amount-" + tc.key
			document := fmt.Sprintf("schema_version: 1\nproducts:\n  %s:\n    display_name: Human money\n    prices:\n      buy:\n        amount: %s\n", key, tc.amount)
			status, original := applyHumanAmountYAML(t, w, document)
			require.Equal(t, http.StatusOK, status)
			require.False(t, original.Replayed)
			price, err := w.client[remote].GetPriceByKey(t.Context(), key, "buy")
			require.NoError(t, err)
			require.Equal(t, tc.currency, price.Currency)
			require.Equal(t, tc.native, price.UnitAmount)
			var replay billing.CatalogApplicationReceipt
			status = w.staffCall(http.MethodPost, "/v1/merchant/catalog/applications", map[string]any{
				"schema_version": 1,
				"products": map[string]any{key: map[string]any{"display_name": "Human money", "prices": map[string]any{"buy": map[string]any{
					"currency": tc.currency, "unit_amount": strconv.FormatInt(tc.native, 10),
				}}}},
			}, &replay)
			require.Equal(t, http.StatusOK, status)
			require.True(t, replay.Replayed, "human amounts and native integers share one catalog identity")
			require.Equal(t, original.ApplicationID, replay.ApplicationID)
			read, err := w.client[embedded].GetPriceByKey(t.Context(), key, "buy")
			require.NoError(t, err)
			require.Equal(t, price.ID, read.ID)
		})
	}
}

func TestHumanAmountInvalidInputCannotMutateCatalog(t *testing.T) {
	w := newWorld(t)
	before, err := w.client[remote].GetCatalogRevision(t.Context())
	require.NoError(t, err)
	for _, amount := range []string{
		"9.99", "1 UNKNOWN", "1 USDD", "-1 USD", "1e3 USD", "1/2 USD", "NaN USD", "Inf USD",
		"0.0000001 USD", "0.0000000001 SOL", "0.0000001 USDC", "0.00001 JPY",
		"9223372036854.775808 USD", "9223372036.854775808 SOL", "9223372036854.775808 USDC",
	} {
		document := fmt.Sprintf("schema_version: 1\nproducts:\n  rejected-money:\n    display_name: Reject\n    prices:\n      buy:\n        amount: %q\n", amount)
		status, _ := applyHumanAmountYAML(t, w, document)
		require.Equal(t, http.StatusBadRequest, status, amount)
	}
	for _, literal := range []string{"null", "9.99", "true", "[]", "{currency: USD, value: 9.99}"} {
		document := fmt.Sprintf("schema_version: 1\nproducts:\n  rejected-money:\n    display_name: Reject\n    prices:\n      buy:\n        amount: %s\n", literal)
		status, _ := applyHumanAmountYAML(t, w, document)
		require.Equal(t, http.StatusBadRequest, status, literal)
	}
	for _, extra := range []string{"currency: USD", "currency: null", "unit_amount: 9990000", "unit_amount: null"} {
		document := fmt.Sprintf("schema_version: 1\nproducts:\n  rejected-money:\n    display_name: Reject\n    prices:\n      buy:\n        amount: 9.99 USD\n        %s\n", extra)
		status, _ := applyHumanAmountYAML(t, w, document)
		require.Equal(t, http.StatusBadRequest, status, extra)
	}
	after, err := w.client[embedded].GetCatalogRevision(t.Context())
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
}
