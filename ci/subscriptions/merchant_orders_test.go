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

// A merchant order: the merchant's backend creates an unpaid order for one
// customer, in process and through NewRemote alike; the customer pays it on
// the merchant's site with their own credential. Staff read it and never pay.
func TestMerchantOrderPaidByCustomer(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	ctx := t.Context()
	life := w.lifetime("orders:merchant", 15_000_000)
	for _, tp := range []topology{embedded, remote} {
		c := w.newCustomer()
		client := w.client[tp]
		lines := []billing.OrderLineParams{{PriceID: life.ID}}
		preview, err := client.PreviewOrder(ctx, billing.PreviewOrderParams{CustomerID: c.cid(), Lines: lines})
		require.NoError(t, err, tp)
		require.Equal(t, int64(15_000_000), preview.Total)
		require.NotEmpty(t, preview.PaymentOptions)

		key := "merchant-" + uuid.NewString()
		order, err := client.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: c.cid(), Lines: lines, IdempotencyKey: key})
		require.NoError(t, err, tp)
		require.Equal(t, billing.OrderOriginMerchant, order.Origin)
		require.Equal(t, billing.OrderOpen, order.Status)
		require.Equal(t, billing.OrderPaymentRequiresPaymentMethod, order.Payment.Status)
		require.WithinDuration(t, w.clock.Now().Add(30*24*time.Hour), order.ExpiresAt, time.Minute, "a merchant order lasts 30 days")
		again, err := client.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: c.cid(), Lines: lines, IdempotencyKey: key})
		require.NoError(t, err)
		require.Equal(t, order.ID, again.ID, "the key answers the same order")
		_, err = client.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: c.cid(), Lines: []billing.OrderLineParams{{PriceID: life.ID, Quantity: new(1)}}, IdempotencyKey: key})
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		_, err = client.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: c.cid(), Lines: lines, IdempotencyKey: "late-" + uuid.NewString(), ExpiresAt: new(w.clock.Now().Add(121 * 24 * time.Hour))})
		requireCode(t, err, http.StatusBadRequest, billing.CodeInvalidParam)

		other := w.newCustomer()
		require.Equal(t, http.StatusNotFound, other.order(http.MethodGet, "/orders/"+order.ID.String(), "", nil).status, "another customer's order does not exist")
		read := c.order(http.MethodGet, "/orders/"+order.ID.String(), "", nil)
		require.Equal(t, "merchant", read.body["origin"])
		require.NotEmpty(t, read.body["payment_options"])
		paid := c.order(http.MethodPost, "/orders/"+order.ID.String()+"/pay", "pay-"+uuid.NewString(),
			map[string]any{"payment": map[string]any{"token": w.nmi.Tokenize(visa), "psp_id": w.psp["nmi"]}, "expected_total": micros(15_000_000)})
		require.Equal(t, http.StatusOK, paid.status, "%v", paid.body)
		require.Equal(t, "complete", paid.body["status"], "%v", paid.body)
		require.True(t, c.entitled("orders:merchant"))

		staff, err := client.GetOrder(ctx, order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderComplete, staff.Status)
		require.Nil(t, staff.Payment.NextAction, "staff read; only the customer pays")
		status, _ := w.merchantJSON(w.auth.token(t, "staff"), http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay",
			map[string]any{"payment": map[string]any{"token": w.nmi.Tokenize(visa), "psp_id": w.psp["nmi"]}, "expected_total": micros(15_000_000)})
		require.NotEqual(t, http.StatusOK, status, "staff never pay")
		_, err = client.CancelOrder(ctx, order.ID)
		requireCode(t, err, http.StatusConflict, billing.CodeOrderNotCancelable)
	}
}

// The merchant withdraws an unpaid order it created; it takes no payment
// after. A customer's own order is not the merchant's to withdraw.
func TestMerchantOrderCancel(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	ctx := t.Context()
	life := w.lifetime("orders:withdrawn", 5_000_000)
	c := w.newCustomer()
	order, err := w.client[remote].CreateOrder(ctx, billing.CreateOrderParams{CustomerID: c.cid(), Lines: []billing.OrderLineParams{{PriceID: life.ID}}, IdempotencyKey: "w-" + uuid.NewString()})
	require.NoError(t, err)
	held := c.order(http.MethodPost, "/orders", "mine-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}})
	require.Equal(t, "already_owned", orderError(held), "the merchant's unpaid order holds the product")

	canceled, err := w.client[remote].CancelOrder(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, billing.OrderCanceled, canceled.Status)
	refused := c.order(http.MethodPost, "/orders/"+order.ID.String()+"/pay", "pay-"+uuid.NewString(),
		map[string]any{"payment": map[string]any{"payment_method_id": c.saveCard("nmi", visa)}, "expected_total": micros(5_000_000)})
	require.Equal(t, "order_not_payable", orderError(refused))

	own := c.order(http.MethodPost, "/orders", "own-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}})
	require.Equal(t, http.StatusCreated, own.status, "the claim was released: %v", own.body)
	ownID, err := billing.ParseOrderID(own.body["id"].(string))
	require.NoError(t, err)
	_, err = w.client[embedded].CancelOrder(ctx, ownID)
	requireCode(t, err, http.StatusConflict, billing.CodeOrderNotCancelable)
}

// A deposit's amount is the customer's choice within the price's bounds:
// the customer's own order and a merchant order alike; a fixed price takes
// none.
func TestOrderLineChosenAmount(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	ctx := t.Context()
	client := w.client[embedded]
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "deposit-" + uuid.NewString()[:8], DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	deposit, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)
	life := w.lifetime("orders:fixed", 2_000_000)
	c := w.newCustomer()

	refusal := func(lines []any) string {
		preview := c.order(http.MethodPost, "/orders/preview", "", map[string]any{"lines": lines})
		require.Equal(t, http.StatusOK, preview.status, "%v", preview.body)
		if r, ok := orderLines(preview)[0]["refusal"].(map[string]any); ok {
			return r["code"].(string)
		}
		return ""
	}
	require.Equal(t, "amount_required", refusal([]any{map[string]any{"price_id": deposit.ID.String()}}))
	require.Equal(t, "amount_invalid", refusal([]any{map[string]any{"price_id": deposit.ID.String(), "amount": "999999"}}))
	require.Equal(t, "amount_not_allowed", refusal([]any{map[string]any{"price_id": life.ID.String(), "amount": "2000000"}}))
	require.Equal(t, "", refusal([]any{map[string]any{"price_id": deposit.ID.String(), "amount": "25000000"}}))

	bought := c.order(http.MethodPost, "/orders", "deposit-"+uuid.NewString(), map[string]any{"lines": []any{map[string]any{"price_id": deposit.ID.String(), "amount": "25000000"}},
		"expected_total": micros(25_000_000), "payment": map[string]any{"payment_method_id": c.saveCard("nmi", visa)}})
	require.Equal(t, "complete", bought.body["status"], "%v", bought.body)
	require.Equal(t, micros(25_000_000), orderLines(bought)[0]["amount"])

	order, err := w.client[remote].CreateOrder(ctx, billing.CreateOrderParams{CustomerID: c.cid(), Lines: []billing.OrderLineParams{{PriceID: deposit.ID, Amount: new(int64(40_000_000))}}, IdempotencyKey: "m-" + uuid.NewString()})
	require.NoError(t, err)
	require.Equal(t, int64(40_000_000), order.Total)
	grants, err := client.ListCreditGrants(ctx, billing.CreditGrantListParams{CustomerID: c.cid()})
	require.NoError(t, err)
	require.Len(t, grants.Items, 1, "the paid deposit granted its credit")
	require.Equal(t, int64(25_000_000), grants.Items[0].Amount)
	current, err := client.GetPrice(ctx, deposit.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Zero(t, current.UnitAmount, "a chosen amount never changes the price")
}
