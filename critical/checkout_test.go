//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestCheckout(t *testing.T) {
	t.Run("OR-J09-S01/real-charge-order-access-and-committed-event", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "download", DisplayName: "Download", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"download:owned"}})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "download", UnitAmount: 9_990_000, Currency: "USD"})
		require.NoError(t, err)
		before, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
		require.NoError(t, err)
		token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(3)}}, ExpectedTotal: ptr(int64(29_970_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: token}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "purchase-once"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.Equal(t, billing.OrderComplete, order.Status)
		require.Equal(t, billing.OrderPaymentSucceeded, order.Payment.Status)
		require.EqualValues(t, 29_970_000, order.Total)
		require.Equal(t, "USD", order.Currency)
		require.NotNil(t, order.Payment.PaymentID)
		money, err := a.alpha.GetPayment(t.Context(), *order.Payment.PaymentID)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, money.Status)
		require.EqualValues(t, 29_970_000, money.Amount)
		require.Equal(t, billing.PaymentCharge, money.Kind)
		require.Equal(t, order.ID, *money.OrderID)
		effects := provider.effects(t)
		require.Len(t, effects, 1)
		require.Equal(t, "charge", effects[0].Kind)
		require.EqualValues(t, 2997, effects[0].Amount)
		require.Equal(t, "usd", effects[0].Currency)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"download:owned", "another:unowned"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["download:owned"])
		require.False(t, access.Entitlements["another:unowned"])
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{After: before.NextCursor, Limit: 100})
		require.NoError(t, err)
		settled, completed := 0, 0
		for _, event := range events.Events {
			require.Equal(t, a.alpha.MerchantID(), event.MerchantID)
			if event.Type == "payment.settled" {
				settled++
				require.Contains(t, string(event.Data), money.ID.String())
			}
			if event.Type == "order.completed" {
				completed++
				require.Contains(t, string(event.Data), order.ID.String())
			}
		}
		require.Equal(t, 1, settled)
		require.Equal(t, 1, completed)
		require.Empty(t, provider.core.Unexpected(), "every provider request must be explicitly modeled")
	})
}
