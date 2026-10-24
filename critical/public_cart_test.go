//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestPublicCart(t *testing.T) {
	t.Run("OR-J08-S01/cart-preview-and-consumption-have-exact-totals", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, price := publicSale(t, a, "cart-total", 9_990_000, nil)
		status, raw := a.request(t, "PUT", "/v1/me/cart/lines/"+price.ID.String(), a.aliceToken, billing.SetCartLineParams{Quantity: ptr(3)})
		require.Equal(t, http.StatusOK, status, string(raw))
		var cart billing.Cart
		require.NoError(t, json.Unmarshal(raw, &cart))
		require.EqualValues(t, 29_970_000, cart.Total)
		require.Len(t, cart.Lines, 1)
		require.Equal(t, price.ID, cart.Lines[0].PriceID)
		require.Equal(t, 3, *cart.Lines[0].Quantity)
		require.NotNil(t, cart.ExpiresAt)
		require.Equal(t, 30*24*time.Hour, cart.ExpiresAt.Sub(*cart.LastChangedAt))
		status, raw = a.request(t, "POST", "/v1/me/orders/preview", a.aliceToken, billing.PreviewMyOrderParams{Cart: true})
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview billing.OrderPreview
		require.NoError(t, json.Unmarshal(raw, &preview))
		require.EqualValues(t, 29_970_000, preview.Total)
		status, raw = a.request(t, "POST", "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Cart: true})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.Equal(t, billing.OrderOpen, order.Status)
		require.EqualValues(t, 29_970_000, order.Total)
		status, raw = a.request(t, "GET", "/v1/me/cart", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &cart))
		require.Empty(t, cart.Lines)
		require.Zero(t, cart.Total)
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J08-S02/application-order-never-charges-the-saved-card", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, price := publicSale(t, a, "saved-card", 9_990_000, nil)
		paid := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "save-card")
		methods, err := a.alphaRemote.ListPaymentMethods(t.Context(), paid.CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, methods.Items, 1)
		input := billing.CreateOrderParams{CustomerID: paid.CustomerID, Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(2)}}, IdempotencyKey: "unpaid-application"}
		unpaid, err := a.alphaRemote.CreateOrder(t.Context(), input)
		require.NoError(t, err)
		require.Equal(t, billing.OrderOpen, unpaid.Status)
		require.EqualValues(t, 19_980_000, unpaid.Total)
		require.Nil(t, unpaid.Payment.PaymentID)
		replay, err := a.alphaRemote.CreateOrder(t.Context(), input)
		require.NoError(t, err)
		require.Equal(t, unpaid.ID, replay.ID)
		listed, err := a.alphaRemote.ListOrders(t.Context(), billing.OrderListParams{CustomerID: paid.CustomerID, Status: billing.OrderOpen})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		require.Equal(t, unpaid.ID, listed.Items[0].ID)
		canceled, err := a.alphaRemote.CancelOrder(t.Context(), unpaid.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderCanceled, canceled.Status)
		require.Len(t, p.effects(t), 1, "only the explicit original purchase charged")
	})
	t.Run("OR-J08-S03/unavailable-quantity-and-stale-total-cannot-charge", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, price := publicSale(t, a, "refused-cart", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		preview, err := a.alphaRemote.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(-1)}}})
		require.ErrorIs(t, err, billing.ErrInvalid)
		var badQuantity *billing.StatusError
		require.ErrorAs(t, err, &badQuantity)
		require.Equal(t, "invalid_param", badQuantity.Code)
		require.Nil(t, preview)
		input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(1_000_000)), Payment: &billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, raw := a.request(t, "POST", "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "stale-total"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"order_total_changed"`)
		_, err = a.alphaRemote.UpdatePrice(t.Context(), price.ID, billing.UpdatePriceParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		preview, err = a.alphaRemote.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}})
		require.NoError(t, err)
		require.Len(t, preview.Lines, 1)
		require.NotNil(t, preview.Lines[0].Refusal)
		require.Equal(t, "unavailable", preview.Lines[0].Refusal.Code)
		orders, err := a.alphaRemote.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, orders.Items)
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J08-S04/cancel-and-expiry-preserve-settled-order", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := startFleet(t, a.world)
		_, price := publicSale(t, a, "order-expiry", 9_990_000, nil)
		paid := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "settled-before-expiry")
		_, err := a.alphaRemote.CancelOrder(t.Context(), paid.ID)
		require.Error(t, err)
		expires := a.clock.Now().Add(time.Hour)
		pending, err := a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: paid.CustomerID, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpiresAt: &expires, IdempotencyKey: "will-expire"})
		require.NoError(t, err)
		a.clock.Advance(time.Hour)
		status, raw := a.request(t, "POST", "/v1/me/orders/"+pending.ID.String()+"/pay", a.aliceToken, billing.PayOrderParams{ExpectedTotal: 9_990_000, Payment: billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": "expired-payment"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"order_not_payable"`)
		runRegistered(t, fleet, "openrails.order_expiry", 1)
		expired, err := a.alphaRemote.GetOrder(t.Context(), pending.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderExpired, expired.Status)
		_, err = a.alphaRemote.CancelOrder(t.Context(), pending.ID)
		require.Error(t, err)
		a.clock.Advance(31 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.order_expiry", 1)
		kept, err := a.alphaRemote.GetOrder(t.Context(), paid.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderComplete, kept.Status)
		require.Equal(t, paid.Payment.PaymentID, kept.Payment.PaymentID)
		require.Len(t, p.effects(t), 1)
	})
}
