//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// A chosen deposit is frozen before handing checkout to another app. Paying
// and replaying use the existing card-sale path and mint one lot per purchase.
func TestHostedCreditDepositSnapshot(t *testing.T) {
	app, pay := hostedHosts(t, nil)
	client := app.client[embedded]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "api-credit", DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "deposit", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)
	buyer := app.newCustomer()
	method := buyer.saveCard("nmi", visa)

	for _, tp := range []topology{embedded, remote} {
		for _, amount := range []*int64{nil, new(int64(0)), new(int64(999_999)), new(int64(500_010_000)), new(int64(100_000_001))} {
			_, err := app.client[tp].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{Customer: buyer.identity(), PriceID: price.ID, Amount: amount})
			require.ErrorIs(t, err, billing.ErrInvalid, tp)
		}
	}
	require.Empty(t, pay.nmi.Sales(), "invalid selections cannot reach a provider")
	for i, amount := range []int64{100_000_000, 75_000_000} {
		link, err := app.client[[]topology{embedded, remote}[i]].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{Customer: buyer.identity(), PriceID: price.ID, Amount: &amount})
		require.NoError(t, err)
		session := hostedSession{w: pay, id: link.ID}
		require.Equal(t, []string{"100000000", "75000000"}[i], session.read()["due_today"])
		body := map[string]any{"option_id": session.option("nmi"), "payment_method_id": method}
		changed := map[string]any{"option_id": session.option("nmi"), "payment_method_id": method, "amount": "1000000"}
		status, _ := session.pay(changed)
		require.Equal(t, http.StatusBadRequest, status, "the payment form cannot change the minted amount")
		status, paid := session.pay(body)
		require.Equal(t, http.StatusOK, status, "%v", paid)
		require.Equal(t, "succeeded", paid["status"])
		status, replay := session.pay(body)
		require.Equal(t, http.StatusOK, status, "%v", replay)
		require.Equal(t, paid["payment_id"], replay["payment_id"])
		pay.settle()
		require.Equal(t, []string{"100.00", "75.00"}[i], pay.nmi.LastSale().Amount)
	}
	require.Len(t, pay.nmi.Sales(), 2, "separate deposits are repeat-buyable and retries do not charge twice")
	grants, err := client.ListCreditGrants(t.Context(), buyer.cid(), billing.CreditGrantListParams{})
	require.NoError(t, err)
	require.Len(t, grants.Items, 2)
	for _, grant := range grants.Items {
		require.NotNil(t, grant.ExpiresAt)
		require.WithinDuration(t, app.clock.Now().Add(365*24*time.Hour), *grant.ExpiresAt, time.Second)
	}
	current, err := client.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Zero(t, current.UnitAmount, "each deposit must leave the shared offer unchanged")
	require.Zero(t, current.Revision, "deposits are not catalog price revisions")
}

// The selected amount participates in the durable buyer/request identity, so
// a retried pay action cannot silently become a different-sized deposit.
func TestCreditDepositAttemptIdempotency(t *testing.T) {
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "deposit-replay", DisplayName: "API deposit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "deposit", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)
	for _, tp := range []topology{embedded, remote} {
		buyer := w.newCustomer()
		method := buyer.saveCard("nmi", visa)
		request := billing.CreateCheckoutAttemptParams{Customer: buyer.identity(), PriceID: price.ID, Amount: new(int64(100_000_000)), IdempotencyKey: "chosen-deposit", PaymentOptions: billing.CheckoutPaymentOptions{PSP: "nmi", PaymentMethodID: pmid(method)}}
		result, err := w.client[tp].CreateCheckoutAttempt(t.Context(), request)
		require.NoError(t, err, tp)
		require.Equal(t, billing.CheckoutAttemptSucceeded, result.Status)
		require.Equal(t, int64(100_000_000), *result.Amount)
		replay, err := w.client[tp].CreateCheckoutAttempt(t.Context(), request)
		require.NoError(t, err, tp)
		require.Equal(t, result.ID, replay.ID)
		request.Amount = new(int64(200_000_000))
		_, err = w.client[tp].CreateCheckoutAttempt(t.Context(), request)
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused, tp)
		request.Amount = nil
		_, err = w.client[tp].CreateCheckoutAttempt(t.Context(), request)
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused, tp)
	}
	w.settle()
	require.Len(t, w.nmi.Sales(), 2, "only the two distinct buyers' original deposits were charged")
}
