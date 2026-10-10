//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// riverSettles holds every request in w that accepted an operation of kind
// until River's worker has settled it, so the worker, not the request,
// records the outcome. settled counts the operations it held.
func (w *world) riverSettles(kind string) (settled *atomic.Int32, remove func()) {
	settled = &atomic.Int32{}
	status := func(id uuid.UUID) string {
		var s string
		if err := w.pool.QueryRow(context.Background(), w.q(`SELECT status FROM billing.provider_intents WHERE id = $1`), id).Scan(&s); err != nil {
			return ""
		}
		return s
	}
	remove = failpoint.Set(func(_ context.Context, s failpoint.Site) error {
		if s.Point != failpoint.BeforeInline || s.Kind != kind || status(s.Operation) == "" {
			return nil
		}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if st := status(s.Operation); st == "succeeded" || st == "failed_terminal" {
				settled.Add(1)
				return nil
			}
			w.settleQuiet()
		}
		return errors.New("River's worker never settled the operation")
	})
	return settled, remove
}

// River's worker, not the paying request, records a declined new card's
// decline: the card the purchase saved is removed from OpenRails and its
// vault deleted at NMI, once, for an enrollment and for a one-time sale.
func TestWorkerDeclineDiscardsTheNewCard(t *testing.T) {
	for name, purchase := range map[string]struct {
		kind  string
		price func(*world) string
	}{
		"enrollment": {subscriptions.TypeInitialMembership, func(w *world) string { return w.membership("content:members", 9_990_000).ID.String() }},
		"one-time sale": {payments.TypeNMISale, func(w *world) string {
			product, err := w.client[embedded].CreateProduct(w.t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
			require.NoError(w.t, err)
			price, err := w.client[embedded].CreatePrice(w.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
			require.NoError(w.t, err)
			return price.ID.String()
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: purchase.price(w)}
			vaults := w.vaultCount()
			settled, remove := w.riverSettles(purchase.kind)
			defer remove()

			paid, err := h.pay("pay-declined", w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"}))
			require.Equal(t, int32(1), settled.Load(), "River's worker recorded the decline")
			require.Equal(t, "insufficient_funds", declinedPay(t, paid, err).Reason)
			w.settle()
			require.Empty(t, h.subscriptions())
			require.Empty(t, h.methods(), "the declined card is not kept")
			require.Equal(t, vaults, w.vaultCount(), "its vault is removed at NMI")
			require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1, "once")

			remove()
			session, err := h.pay("pay-retry", w.nmi.Tokenize(visa))
			require.NoError(t, err)
			require.Equal(t, "succeeded", session.Status)
			require.Len(t, h.methods(), 1)
		})
	}
}
