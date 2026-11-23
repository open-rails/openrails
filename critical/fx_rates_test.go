//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/fxfake"
	"github.com/stretchr/testify/require"
)

// The FX source is the external boundary: a euro price paid in USDC is
// priced through the rate it publishes.
func TestFXRates(t *testing.T) {
	t.Run("OR-J37-S10/a-published-rate-far-from-the-held-one-prices-nothing", func(t *testing.T) {
		rates := fxfake.New()
		t.Cleanup(rates.Close)
		s := newSolanaWorld(t, false, solanaFixtureOptions{fx: rates.Transport()})
		buy := func(key string) (int, []byte) {
			product, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Euro purchase", Ownership: catalog.OwnershipConsumable, Entitlements: []string{key + ":paid"}})
			require.NoError(t, err)
			price, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "EUR", SellVia: []catalog.SellVia{catalog.SellViaSolana}})
			require.NoError(t, err)
			return s.request(t, http.MethodPost, "/v1/me/orders", s.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Payment: &billing.OrderPaymentParams{TokenSymbol: "USDC"}}, map[string]string{"Idempotency-Key": key})
		}

		rates.Date("eur", s.clock.Now())
		rates.Rate(fxfake.Primary, "eur", "usd", 1.08)
		status, raw := buy("euro-first")
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.NotNil(t, order.Payment.NextAction)
		require.Equal(t, "10.800000", order.Payment.NextAction.Payload["token_amount"])

		// Three days on the held rate is stale, and the primary publishes a
		// euro at a ten-thousandth of a dollar.
		s.clock.Advance(72 * time.Hour)
		rates.Date("eur", s.clock.Now())
		rates.Rate(fxfake.Primary, "eur", "usd", 0.0001)
		status, raw = buy("euro-second")
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"invalid_param"`)
		require.Contains(t, string(raw), "held back")
		require.Equal(t, 2, rates.Requests(fxfake.Primary, "eur"), "the new publication was read and refused")
	})
}
