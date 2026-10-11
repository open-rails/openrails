//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// An order whose Solana quote expired unpaid still awaits its wallet, and a
// transfer landing within the grace credits it: the buyer is never told to
// pay again for money that may already be on its way.
func TestSolanaPayAttemptProcessingUntilReferenceCloses(t *testing.T) {
	p := newSolanaPay(t)
	buyer := p.w.newCustomer()
	req := p.checkout(buyer)
	p.w.clock.Advance(16 * time.Minute)
	read := buyer.order(http.MethodGet, "/orders/"+req.order.String(), "", nil)
	require.Equal(t, "requires_action", paymentOf(read)["status"], "the quote expired but its reference still credits")
	require.Equal(t, "requires_action", p.w.attemptStatus(req.id))
	p.pay(req, req.amount)
	p.eventually(func() bool { return p.payments(req) == 1 }, "the transfer is credited")
	p.eventually(func() bool { return p.status(req) == "succeeded" }, "the attempt succeeds")
}

// A Solana subscribe awaiting its wallet holds the customer's slot: card
// enrollment of the same plan is refused before any charge, and a first pull
// that lands for a slot another membership took is queued for refund.
func TestSolanaSubscribeHoldsSlotBeforeFirstPull(t *testing.T) {
	s := openSolanaShop(t)
	t.Run("card enrollment waits", func(t *testing.T) {
		b := s.buyer(t, false)
		c := s.checkout(t, b, b.wallet.PublicKey())
		charges := len(s.w.nmi.Ledger(""))
		paid, err := b.checkout(embedded, order{price: pid(s.price), rail: "nmi", method: b.saveCard("nmi", visa), successURL: "https://e2e.test/return"})
		require.NoError(t, err)
		requireSlotHeld(t, paid)
		require.Len(t, s.w.nmi.Ledger(""), charges, "nothing is charged by card")
		sig := s.land(t, signAs(t, c.bundle, b.wallet), s.w.clock.Now())
		done, err := b.confirm(c, sig)
		require.NoError(t, err)
		require.Equal(t, "succeeded", done.Status)
	})
	t.Run("orphaned first pull is queued for refund", func(t *testing.T) {
		b := s.buyer(t, false)
		c := s.checkout(t, b, b.wallet.PublicKey())
		// Another writer (an import, a restored book) gave the customer this
		// plan meanwhile.
		_, err := s.w.pool.Exec(t.Context(), s.w.q(`INSERT INTO billing.subscriptions
			(price_id, product_id, status, rail, collection_policy, rail_subscription_id, started_at, merchant_id, customer_id, psp_id, current_period_starts_at, current_period_ends_at)
			SELECT p.id, p.product_id, 'active', 'stripe', 'provider', 'sub_other_writer', now(), p.merchant_id, $2, psp.id, now(), now() + interval '30 days'
			FROM billing.prices p JOIN billing.psps psp ON psp.merchant_id = p.merchant_id AND psp.rail = 'stripe' WHERE p.id = $1 LIMIT 1`),
			pid(s.price).UUID(), uuid.MustParse(b.id))
		require.NoError(t, err)
		sig := s.land(t, signAs(t, c.bundle, b.wallet), s.w.clock.Now())
		_, err = b.confirm(c, sig)
		require.Error(t, err, "no membership is created for a taken slot")
		var repairs int
		require.NoError(t, s.w.pool.QueryRow(t.Context(), s.w.q(`SELECT count(*) FROM billing.reconciliation_findings WHERE finding_type = 'consistency.ledger.unbooked' AND evidence->>'transaction_id' = $1`), sig).Scan(&repairs))
		require.Equal(t, 1, repairs, "the landed first pull is queued for refund")
	})
}
