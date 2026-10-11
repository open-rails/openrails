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

// Each deposit is its own merchant order, its chosen amount frozen when the
// order is created: a pay naming another total is refused, a replayed pay
// charges once, and every deposit mints its own lot.
func TestCreditDepositOrders(t *testing.T) {
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "api-credit", DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "deposit", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)
	buyer := w.newCustomer()
	method := buyer.saveCard("nmi", visa)

	for _, tp := range []topology{embedded, remote} {
		for _, amount := range []*int64{nil, new(int64(0)), new(int64(999_999)), new(int64(500_010_000))} {
			_, err := buyer.sell(tp, order{price: price.ID, amount: amount})
			require.Error(t, err, tp)
		}
	}
	require.Empty(t, w.nmi.Sales(), "invalid selections cannot reach a provider")
	for i, amount := range []int64{100_000_000, 75_000_000} {
		session, err := buyer.sell([]topology{embedded, remote}[i], order{price: price.ID, amount: &amount})
		require.NoError(t, err)
		require.Equal(t, amount, session.order.Total)
		status, out := buyer.call(http.MethodPost, "/orders/"+session.order.ID.String()+"/pay", "other-"+uuid.NewString(),
			map[string]any{"payment": map[string]any{"payment_method_id": method}, "expected_total": "1000000"})
		require.Equal(t, http.StatusConflict, status, "a pay cannot change the frozen amount: %v", out)
		require.Equal(t, billing.CodeOrderTotalChanged, errorCode(out))
		paid, err := session.buy(buyer, order{method: method})
		require.NoError(t, err)
		require.Equal(t, "succeeded", paid.Status, "%+v", paid.Order)
		replay, err := session.buy(buyer, order{method: method})
		require.NoError(t, err)
		require.Equal(t, paid.PaymentID, replay.PaymentID)
		w.settle()
		require.Equal(t, []string{"100.00", "75.00"}[i], w.nmi.LastSale().Amount)
	}
	require.Len(t, w.nmi.Sales(), 2, "separate deposits are repeat-buyable and retries do not charge twice")
	grants, err := client.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: buyer.cid()})
	require.NoError(t, err)
	require.Len(t, grants.Items, 2)
	for _, grant := range grants.Items {
		require.NotNil(t, grant.ExpiresAt)
		require.WithinDuration(t, w.clock.Now().Add(365*24*time.Hour), *grant.ExpiresAt, time.Second)
	}
	current, err := client.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Zero(t, current.UnitAmount, "each deposit must leave the shared offer unchanged")
	require.Zero(t, current.Version, "deposits are not catalog price versions")
}
