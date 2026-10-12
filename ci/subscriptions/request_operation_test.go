//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// The request that accepts an operation runs its first attempt: River's wake
// waits out the request's hold instead of racing it, so the customer's answer
// is that attempt's outcome, never an unsent "processing".
func TestRequestRunsTheOperationItAccepted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	product, err := w.client[embedded].CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	price, err := w.client[embedded].CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID.String()}

	var stalled, taken atomic.Bool
	defer failpoint.Set(func(_ context.Context, s failpoint.Site) error {
		if s.Point != failpoint.BeforeInline || s.Kind != payments.TypeNMISale {
			return nil
		}
		// The request stalls with River's wake already due and promoted.
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			w.settleQuiet()
			var status string
			if err := w.pool.QueryRow(context.Background(), w.q(`SELECT status FROM billing.provider_intents WHERE id = $1`), s.Operation).Scan(&status); err != nil || status != "pending" {
				taken.Store(true)
				break
			}
		}
		stalled.Store(true)
		return nil
	})()

	paid, err := h.pay("pay", w.nmi.Tokenize(visa))
	require.NoError(t, err)
	require.True(t, stalled.Load())
	require.False(t, taken.Load(), "River's worker left the operation to the request that accepted it")
	require.Equal(t, "succeeded", paid.Status, "the request's own attempt answers: %+v", paid.Order)
}
