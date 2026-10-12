//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// cartPrices are the price ids of a cart's lines, in order.
func cartPrices(call orderCall) []string {
	var out []string
	for _, l := range orderLines(call) {
		out = append(out, l["price_id"].(string))
	}
	return out
}

// The cart is one per customer of a merchant: two host processes of the
// merchant read and change the same cart, priced live on every read, and an
// order made from it takes its lines in the transaction that creates it.
func TestCartSharedAcrossHosts(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	ctx := t.Context()
	a, b := w.server.URL, w.startReplica().server.URL
	client := w.client[embedded]
	book := w.lifetime("cart:book", 5_000_000)
	course := w.lifetime("cart:course", 7_000_000)
	annual, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: course.ProductID, Key: "course-annual-" + uuid.NewString()[:8], UnitAmount: 9_000_000, Currency: "USD"})
	require.NoError(t, err)
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "cart-credit-" + uuid.NewString()[:8], DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	deposit, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)
	c := w.newCustomer()
	put := func(base string, price billing.PriceID, body map[string]any) orderCall {
		t.Helper()
		return c.orderAt(base, http.MethodPut, "/cart/lines/"+price.String(), "", body)
	}

	empty := c.orderAt(b, http.MethodGet, "/cart", "", nil)
	require.Equal(t, http.StatusOK, empty.status, "%v", empty.body)
	require.Empty(t, empty.body["lines"])
	require.Nil(t, empty.body["last_changed_at"])
	require.Equal(t, micros(0), empty.body["total"])

	require.Equal(t, http.StatusOK, put(a, book.ID, map[string]any{}).status)
	added := put(b, course.ID, map[string]any{})
	require.Equal(t, http.StatusOK, added.status, "%v", added.body)
	require.Equal(t, []string{book.ID.String(), course.ID.String()}, cartPrices(added), "host B adds to the cart host A started")
	require.Equal(t, micros(12_000_000), added.body["total"])
	read := c.orderAt(a, http.MethodGet, "/cart", "", nil)
	require.Equal(t, cartPrices(added), cartPrices(read), "host A reads what host B added")
	expires, err := time.Parse(time.RFC3339, read.body["expires_at"].(string))
	require.NoError(t, err)
	require.WithinDuration(t, w.clock.Now().Add(30*24*time.Hour), expires, time.Minute)

	switched := put(a, annual.ID, map[string]any{})
	require.Equal(t, []string{book.ID.String(), annual.ID.String()}, cartPrices(switched), "another price of the course replaces its line")
	require.Equal(t, micros(14_000_000), switched.body["total"])
	chosen := put(b, deposit.ID, map[string]any{"amount": "25000000"})
	require.Equal(t, micros(25_000_000), orderLines(chosen)[2]["amount"])
	require.Equal(t, micros(39_000_000), chosen.body["total"])
	refused := put(b, deposit.ID, map[string]any{"amount": "999999"})
	require.Equal(t, "amount_invalid", orderLines(refused)[2]["refusal"].(map[string]any)["code"], "a line out of bounds is kept, refused")
	require.Equal(t, http.StatusNotFound, put(a, billing.PriceID("test:"+uuid.NewString()+":v1"), map[string]any{}).status)
	require.Equal(t, http.StatusBadRequest, put(a, book.ID, map[string]any{"quantity": -1}).status)
	removed := put(a, deposit.ID, map[string]any{"quantity": 0})
	require.Equal(t, []string{book.ID.String(), annual.ID.String()}, cartPrices(removed))

	preview := c.orderAt(b, http.MethodPost, "/orders/preview", "", map[string]any{"cart": true})
	require.Equal(t, http.StatusOK, preview.status, "%v", preview.body)
	require.Equal(t, micros(14_000_000), preview.body["total"])
	both := c.orderAt(b, http.MethodPost, "/orders/preview", "", map[string]any{"cart": true, "lines": []any{line(book, 0)}})
	require.Equal(t, "invalid_param", orderError(both))

	// A changed total refuses the order and leaves the cart as it was.
	method := c.saveCard("nmi", visa)
	stale := c.order(http.MethodPost, "/orders", "cart-"+uuid.NewString(), map[string]any{"cart": true, "expected_total": micros(12_000_000), "payment": map[string]any{"payment_method_id": method}})
	require.Equal(t, "order_total_changed", orderError(stale), "%v", stale.body)
	require.Len(t, orderLines(c.orderAt(a, http.MethodGet, "/cart", "", nil)), 2)

	bought := c.orderAt(b, http.MethodPost, "/orders", "cart-"+uuid.NewString(), map[string]any{"cart": true, "expected_total": micros(14_000_000), "payment": map[string]any{"payment_method_id": method}})
	require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
	require.Equal(t, "complete", bought.body["status"], "%v", bought.body)
	require.Equal(t, []string{book.ID.String(), annual.ID.String()}, cartPrices(bought))
	require.True(t, c.entitled("cart:book"))
	require.True(t, c.entitled("cart:course"))
	after := c.orderAt(a, http.MethodGet, "/cart", "", nil)
	require.Empty(t, after.body["lines"], "the order took the cart's lines")
	again := c.order(http.MethodPost, "/orders", "cart-"+uuid.NewString(), map[string]any{"cart": true})
	require.Equal(t, "invalid_param", orderError(again), "an empty cart buys nothing")

	// Emptied on request, and expired 30 days after its last change.
	require.Equal(t, http.StatusOK, put(a, deposit.ID, map[string]any{"amount": "5000000"}).status)
	cleared := c.orderAt(b, http.MethodDelete, "/cart", "", nil)
	require.Equal(t, http.StatusNoContent, cleared.status)
	require.Empty(t, c.orderAt(a, http.MethodGet, "/cart", "", nil).body["lines"])
	require.Equal(t, http.StatusOK, put(a, deposit.ID, map[string]any{"amount": "5000000"}).status)
	w.clock.Advance(29 * 24 * time.Hour)
	require.Len(t, orderLines(c.orderAt(b, http.MethodGet, "/cart", "", nil)), 1)
	w.clock.Advance(2 * 24 * time.Hour)
	require.Empty(t, c.orderAt(b, http.MethodGet, "/cart", "", nil).body["lines"], "an unchanged cart expires")
}
