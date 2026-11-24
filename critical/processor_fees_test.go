//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// TestProcessorFees is the general ledger's processor fees: Stripe's, read
// from the balance transactions of the charges, refunds and disputes recorded.
func TestProcessorFees(t *testing.T) {
	t.Run("OR-J40-S40/a-stripe-charge-fee-and-a-dispute-fee-post-once-exactly", func(t *testing.T) {
		m := newDisputedMembership(t)
		w, mid := m.a.world, m.a.alpha.MerchantID().UUID()
		fleet := quietFleet(t, w)

		// The $10.00 charge queued its fee read; the billing pass posts
		// Stripe's 2.9% + 30¢, 59¢: 590000 micros, out of the PSP's cash.
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{0, 590_000}, feeTotal(t, w, mid, "processor_fees", "USD"))
		require.Equal(t, [2]int64{590_000, 0}, feeTotal(t, w, mid, "cash_clearing", "USD"))
		require.Equal(t, 1, feeTransfers(t, w, mid, "processor_fee"))
		require.Zero(t, queuedFeeReads(t, w, mid))

		// Its balance transaction read again posts nothing new.
		_, err := w.pool.Exec(t.Context(), "INSERT INTO "+booksTable(w, "stripe_fee_reads")+" (merchant_id, psp_id, ref, kind, due_at)"+
			" SELECT merchant_id, psp_id, transaction_id, 'payment', now() FROM "+booksTable(w, "payments")+" WHERE merchant_id=$1 AND id=$2", mid, m.payment.UUID())
		require.NoError(t, err)
		require.False(t, runPass(t, fleet))
		require.Equal(t, 1, feeTransfers(t, w, mid, "processor_fee"))
		require.Equal(t, [2]int64{0, 590_000}, feeTotal(t, w, mid, "processor_fees", "USD"))

		// A dispute withdraws the charge and Stripe's $15.00 dispute fee: the
		// fee posts once, though the dispute is read again as it changes.
		m.notice(t, "evt_fee_dispute", "charge.dispute.created", m.p.core.ChargeDispute("dp_fees", m.charge, "needs_response", "fraudulent"))
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{0, 15_000_000}, feeTotal(t, w, mid, "chargeback_fees", "USD"))
		m.notice(t, "evt_fee_dispute_review", "charge.dispute.updated", m.p.core.ChargeDispute("dp_fees", m.charge, "under_review", "fraudulent"))
		require.False(t, runPass(t, fleet))
		require.Equal(t, 2, m.p.core.Reads("/v1/disputes/dp_fees"), "a dispute whose status changes is read again")
		require.Equal(t, 1, feeTransfers(t, w, mid, "chargeback_fee"))
		require.Equal(t, [2]int64{0, 15_000_000}, feeTotal(t, w, mid, "chargeback_fees", "USD"))

		// A notice that changes nothing keeps a failing read's count; the
		// dispute won reads it afresh, and Stripe's returned fee posts back.
		m.p.core.Intercept(func(r *http.Request) bool {
			return r.Method == http.MethodGet && r.URL.Path == "/v1/disputes/dp_fees"
		}, unavailable)
		m.notice(t, "evt_fee_dispute_same", "charge.dispute.updated", m.p.core.ChargeDispute("dp_fees", m.charge, "under_review", "fraudulent"))
		require.False(t, runPass(t, fleet))
		attempts, _ := feeRead(t, w, mid, "dispute")
		require.Equal(t, 1, attempts)
		m.notice(t, "evt_fee_dispute_same_again", "charge.dispute.updated", m.p.core.ChargeDispute("dp_fees", m.charge, "under_review", "fraudulent"))
		attempts, _ = feeRead(t, w, mid, "dispute")
		require.Equal(t, 1, attempts, "a notice that changes no status resets no failures")
		m.p.core.ClearIntercepts()
		m.notice(t, "evt_fee_dispute_won", "charge.dispute.closed", m.p.core.ChargeDispute("dp_fees", m.charge, "won", "fraudulent"))
		attempts, _ = feeRead(t, w, mid, "dispute")
		require.Zero(t, attempts)
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{15_000_000, 15_000_000}, feeTotal(t, w, mid, "chargeback_fees", "USD"))
		require.Equal(t, 2, feeTransfers(t, w, mid, "chargeback_fee"))
		require.Zero(t, queuedFeeReads(t, w, mid))

		// A charge in euros settles in the account's dollars: its fee posts in
		// dollars, on the settled amount (20.00 EUR at 1.1 is 22.00 USD; 2.9%
		// + 30¢ is 94¢), and the euro books take none. A read Stripe fails
		// waits, and the pass and the charge stand; read again, it posts.
		m.p.core.SetSettlement("usd", 1.1)
		price := mrrPlan(t, m.a.alpha, "fees-eur", billing.CreatePriceParams{UnitAmount: 20_000_000, Currency: "EUR", BillingIntervalMonths: ptr(1)})
		buyMRRPlan(t, m.a, m.p, price, 20_000_000, "fees-eur")
		m.p.core.Intercept(func(r *http.Request) bool {
			return r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "balance_transaction")
		}, unavailable)
		require.False(t, runPass(t, fleet), "a fee read that fails never fails the pass")
		require.Equal(t, [2]int64{0, 590_000}, feeTotal(t, w, mid, "processor_fees", "USD"))
		attempts, _ = feeRead(t, w, mid, "payment")
		require.Equal(t, 1, attempts)
		m.p.core.ClearIntercepts()
		_, err = w.pool.Exec(t.Context(), "UPDATE "+booksTable(w, "stripe_fee_reads")+" SET due_at=now() WHERE merchant_id=$1", mid)
		require.NoError(t, err)
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{0, 590_000 + 940_000}, feeTotal(t, w, mid, "processor_fees", "USD"))
		require.Equal(t, [2]int64{0, 0}, feeTotal(t, w, mid, "processor_fees", "EUR"))
		require.Equal(t, 2, feeTransfers(t, w, mid, "processor_fee"))
		require.Zero(t, queuedFeeReads(t, w, mid))
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+mid.String()), "Ledger integrity OK")
	})
	t.Run("OR-J40-S41/a-prevention-refund-reads-through-its-dispute", func(t *testing.T) {
		m := newDisputedMembership(t)
		w, mid := m.a.world, m.a.alpha.MerchantID().UUID()
		fleet := quietFleet(t, w)
		require.False(t, runPass(t, fleet))
		m.p.core.ChargeDispute("dp_rdr_fees", m.charge, "lost", "subscription_canceled")

		// The customer held, the RDR resolution's refund books only after
		// the billing pass has read its dispute.
		hold, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		defer hold.Rollback(context.Background()) //nolint:errcheck
		_, err = hold.Exec(t.Context(), "SELECT 1 FROM "+booksTable(w, "customers")+" WHERE merchant_id=$1 AND id=$2 FOR NO KEY UPDATE", mid, m.sub.CustomerID.UUID())
		require.NoError(t, err)
		body := stripeNotice(t, "evt_rdr_fees", "charge.dispute.created", m.dispute("dp_rdr_fees", "lost", "resolution", "subscription_canceled"))
		delivered := deliverAsync(m.a, body)
		require.Eventually(t, func() bool {
			var n int
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "disputes")+" WHERE merchant_id=$1 AND provider_ref='dp_rdr_fees'", mid).Scan(&n))
			return n == 1
		}, 20*time.Second, 50*time.Millisecond)
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{0, 15_000_000}, feeTotal(t, w, mid, "chargeback_fees", "USD"))
		require.Zero(t, queuedFeeReads(t, w, mid))
		require.NoError(t, hold.Commit(t.Context()))
		require.NoError(t, <-delivered)
		reversals := m.reversals(t)
		require.Len(t, reversals, 1)
		require.Equal(t, "dp_rdr_fees", reversals[0].TransactionID)
		require.Zero(t, queuedFeeReads(t, w, mid), "a prevention refund reads through its dispute, never as a refund")

		// The case changes later: its next read posts the returned fee.
		m.p.core.ChargeDispute("dp_rdr_fees", m.charge, "won", "subscription_canceled")
		m.notice(t, "evt_rdr_fees_won", "charge.dispute.updated", m.dispute("dp_rdr_fees", "won", "resolution", "subscription_canceled"))
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{15_000_000, 15_000_000}, feeTotal(t, w, mid, "chargeback_fees", "USD"))
		require.Equal(t, 2, feeTransfers(t, w, mid, "chargeback_fee"))
		require.Zero(t, m.p.core.Reads("/v1/refunds/dp_rdr_fees"))
		require.Zero(t, queuedFeeReads(t, w, mid))
	})
}

// unavailable answers an intercepted Stripe request 503.
func unavailable(r *http.Request, _ func() *http.Response) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
}

// deliverAsync posts a signed Stripe notice from another goroutine and
// reports how it was answered.
func deliverAsync(a *authorityWorld, body []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, a.server.URL+"/v1/webhooks/stripe/acct_e2e", bytes.NewReader(body))
		if err != nil {
			done <- err
			return
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Stripe-Signature", stripeSignature(body, "whsec_critical_fixture"))
		response, err := a.server.Client().Do(r)
		if err != nil {
			done <- err
			return
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK {
			done <- fmt.Errorf("notice answered %d: %s", response.StatusCode, raw)
			return
		}
		done <- nil
	}()
	return done
}

// feeTotal is one general-ledger account type's credits and debits in a
// currency, every PSP's together.
func feeTotal(t *testing.T, w *world, merchant uuid.UUID, accountType, currency string) [2]int64 {
	t.Helper()
	var out [2]int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(credits_posted), 0)::bigint, COALESCE(sum(debits_posted), 0)::bigint FROM "+booksTable(w, "ledger_type_totals")+
		" WHERE merchant_id=$1 AND book='general' AND account_type=$2 AND currency=$3", merchant, accountType, currency).Scan(&out[0], &out[1]))
	return out
}

// feeTransfers counts the general ledger's transfers of one type.
func feeTransfers(t *testing.T, w *world, merchant uuid.UUID, transferType string) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='general' AND transfer_type=$2",
		merchant, transferType).Scan(&n))
	return n
}

// queuedFeeReads counts the merchant's fee reads still queued.
func queuedFeeReads(t *testing.T, w *world, merchant uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "stripe_fee_reads"}.Sanitize()+" WHERE merchant_id=$1", merchant).Scan(&n))
	return n
}

// feeRead is the merchant's one queued fee read of a kind: its failures, and
// whether it waits for its finding.
func feeRead(t *testing.T, w *world, merchant uuid.UUID, kind string) (int, bool) {
	t.Helper()
	var attempts int
	var parked bool
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT attempts, due_at = 'infinity' FROM "+booksTable(w, "stripe_fee_reads")+" WHERE merchant_id=$1 AND kind=$2", merchant, kind).Scan(&attempts, &parked))
	return attempts, parked
}
