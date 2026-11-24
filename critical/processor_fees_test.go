//go:build integration

package critical_test

import (
	"net/http"
	"strings"
	"testing"

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
		require.Equal(t, 1, feeTransfers(t, w, mid, "chargeback_fee"))
		require.Equal(t, [2]int64{0, 15_000_000}, feeTotal(t, w, mid, "chargeback_fees", "USD"))

		// A charge in euros settles in the account's dollars: its fee posts in
		// dollars, on the settled amount (20.00 EUR at 1.1 is 22.00 USD; 2.9%
		// + 30¢ is 94¢), and the euro books take none. A read Stripe fails
		// waits, and the pass and the charge stand; read again, it posts.
		m.p.core.SetSettlement("usd", 1.1)
		price := mrrPlan(t, m.a.alpha, "fees-eur", billing.CreatePriceParams{UnitAmount: 20_000_000, Currency: "EUR", BillingIntervalMonths: ptr(1)})
		buyMRRPlan(t, m.a, m.p, price, 20_000_000, "fees-eur")
		m.p.core.Intercept(func(r *http.Request) bool {
			return r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "balance_transaction")
		}, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
		})
		require.False(t, runPass(t, fleet), "a fee read that fails never fails the pass")
		require.Equal(t, [2]int64{0, 590_000}, feeTotal(t, w, mid, "processor_fees", "USD"))
		var attempts int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT attempts FROM "+booksTable(w, "stripe_fee_reads")+" WHERE merchant_id=$1 AND kind='payment'", mid).Scan(&attempts))
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
