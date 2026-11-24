//go:build integration

package critical_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// TestRevenueReversals is journey J41's reversals: refunds, chargebacks and
// won disputes on the general ledger.
func TestRevenueReversals(t *testing.T) {
	t.Run("OR-J41-S18/a-partial-refund-while-service-runs-is-a-price-reduction-with-catch-up", func(t *testing.T) {
		start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)
		a, provider := newPaymentWorld(t, atClock(start))
		price := calendarPrice(t, a, "monthly", 10_000_000, 1)
		order := buy(t, a, provider, "monthly-plan", []billing.OrderLineParams{{PriceID: price}}, 10_000_000)
		line, payment := order.Lines[0].ID.UUID(), *order.Payment.PaymentID
		sched := scheduleOfLine(t, a.world, line)

		// Six and a half days in, 4.00 is refunded and the access kept. The
		// month's 2.166666 earned at 10.00 is recognised first; at 6.00 it
		// earned 1.30, so 0.866666 is the refund's contra and the rest leaves
		// deferred revenue.
		refundedAt := start.Add(156 * time.Hour)
		a.clock.Advance(refundedAt.Sub(a.clock.Now()))
		refund, err := a.alpha.RefundPayment(t.Context(), "partial-refund", payment, billing.RefundPaymentParams{Amount: 4_000_000, Reason: "Partial refund"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, refund.Status)
		r := refund.ID.UUID().String()
		require.Equal(t, []glRow{
			{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: r, Debit: "refunds", Credit: "cash_clearing", Amount: 866_666, At: refundedAt, OrderLine: &line, Schedule: &sched.ID},
			{Type: "refund_deferred", Operation: "refund", Source: "payment", SourceID: r, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 3_133_334, At: refundedAt, OrderLine: &line, Schedule: &sched.ID},
		}, glFor(t, a.world, "payment", r))
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "subscription_revenue", Funded: 6_000_000, Recognized: 1_300_000, Starts: start, Ends: end, Through: refundedAt, Next: &end},
			scheduleOfLine(t, a.world, line).terms())

		// The period's end earns the rest of the lower price.
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, end.Add(time.Hour))
		require.Equal(t, []glRow{
			recognized(sched.ID, line, "recognition", refundedAt, refundedAt, 2_166_666),
			recognized(sched.ID, line, "recognition", end, end.Add(-time.Microsecond), 4_700_000),
		}, recognitions(t, a.world, sched.ID))
		require.Zero(t, unappliedCash(t, a.world, "USD"))
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S19/a-full-refund-that-revokes-access-takes-unrecognised-revenue-first", func(t *testing.T) {
		start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		pass := oneOffPrice(t, a, "pass", 9_000_000, ptr(720))
		order := buy(t, a, provider, "pass", []billing.OrderLineParams{{PriceID: pass}}, 9_000_000)
		line := order.Lines[0].ID.UUID()
		sched := scheduleOfLine(t, a.world, line)

		// Five days in, refunded in full with the access revoked: the access
		// ends first, so its five days are recognised, then the refund takes
		// the 7.50 still deferred and reverses the 1.50 earned.
		refundedAt := start.Add(120 * time.Hour)
		a.clock.Advance(refundedAt.Sub(a.clock.Now()))
		refund, err := a.alpha.RefundPayment(t.Context(), "full-refund", *order.Payment.PaymentID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "Full refund"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, refund.Status)
		r := refund.ID.UUID().String()
		require.Equal(t, []glRow{
			{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: r, Debit: "refunds", Credit: "cash_clearing", Amount: 1_500_000, At: refundedAt, OrderLine: &line, Schedule: &sched.ID},
			{Type: "refund_deferred", Operation: "refund", Source: "payment", SourceID: r, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 7_500_000, At: refundedAt, OrderLine: &line, Schedule: &sched.ID},
		}, glFor(t, a.world, "payment", r))
		require.Equal(t, []glRow{recognized(sched.ID, line, "recognition", refundedAt, refundedAt.Add(-time.Microsecond), 1_500_000)}, recognitions(t, a.world, sched.ID))
		done := scheduleOfLine(t, a.world, line)
		require.Equal(t, &refundedAt, done.EndedAt)
		require.Zero(t, done.Funded)
		require.Zero(t, done.Recognized)
		require.Nil(t, done.Next, "nothing is left to forfeit")
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S20/a-chargeback-reduces-its-schedule-and-the-won-dispute-restores-it", func(t *testing.T) {
		start := time.Date(2026, time.May, 10, 10, 0, 0, 0, time.UTC)
		m := newDisputedMembership(t, atClock(start))
		a := m.a
		schedules := subscriptionSchedules(t, a.world, m.sub.ID.UUID())
		require.Len(t, schedules, 1)
		sched := schedules[0]
		line := sched.OrderLine
		var psp uuid.UUID
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT psp_id FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), m.payment.UUID()).Scan(&psp))

		// Six hours into the day's membership the charge is disputed: a
		// quarter of it was earned, so 2.50 is the chargeback's contra and
		// 7.50 leaves deferred revenue. The membership ends there.
		disputedAt := start.Add(6 * time.Hour)
		a.clock.Advance(disputedAt.Sub(a.clock.Now()))
		d := m.dispute("dp_books", "needs_response", "chargeback", "fraudulent")
		m.notice(t, "evt_books_created", "charge.dispute.created", d)
		reversals := m.reversals(t)
		require.Len(t, reversals, 1)
		c := reversals[0].ID.UUID().String()
		require.Equal(t, []glRow{
			{Type: "chargeback_recognized", Operation: "chargeback", Source: "payment", SourceID: c, Debit: "chargebacks", Credit: "cash_clearing", Amount: 2_500_000, At: disputedAt, OrderLine: &line, Schedule: &sched.ID},
			{Type: "chargeback_deferred", Operation: "chargeback", Source: "payment", SourceID: c, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 7_500_000, At: disputedAt, OrderLine: &line, Schedule: &sched.ID},
		}, glFor(t, a.world, "payment", c))
		charged := scheduleOfLine(t, a.world, line)
		require.Zero(t, charged.Funded)
		require.Equal(t, &disputedAt, charged.EndedAt)

		// Won: what the chargeback took comes back, pro rata. The membership
		// stays ended, so the restored deferral is forfeited at once.
		d["status"] = "won"
		m.notice(t, "evt_books_won", "charge.dispute.closed", d)
		reversals = m.reversals(t)
		require.Len(t, reversals, 2)
		var won string
		for _, rv := range reversals {
			if rv.Amount > 0 {
				won = rv.ID.UUID().String()
			}
		}
		require.NotEmpty(t, won)
		require.Equal(t, []glRow{
			{Type: "dispute_won", Operation: "dispute_won", Source: "payment", SourceID: won, Debit: "cash_clearing", DebitPSP: &psp, Credit: "chargebacks", Amount: 2_500_000, At: disputedAt, OrderLine: &line, Schedule: &sched.ID},
			{Type: "dispute_won", Operation: "dispute_won_deferred", Source: "payment", SourceID: won, Debit: "cash_clearing", DebitPSP: &psp, Credit: "deferred_revenue", Amount: 7_500_000, At: disputedAt, OrderLine: &line, Schedule: &sched.ID},
			{Type: "revenue_recognized", Operation: "dispute_won", Source: "payment", SourceID: won, Debit: "deferred_revenue", Credit: "subscription_revenue", Amount: 7_500_000, At: disputedAt, OrderLine: &line, Schedule: &sched.ID},
		}, glFor(t, a.world, "payment", won))
		restored := scheduleOfLine(t, a.world, line)
		require.EqualValues(t, 10_000_000, restored.Funded)
		require.EqualValues(t, 10_000_000, restored.Recognized)
		require.Nil(t, restored.Next)
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S21/a-writer-that-skips-the-poster-commits-and-ledger-audit-raises-a-finding", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		sale := oneOffPrice(t, a, "sale", 10_000_000, nil)
		order := buy(t, a, provider, "sale", []billing.OrderLineParams{{PriceID: sale}}, 10_000_000)
		payment := order.Payment.PaymentID.UUID()
		audit := "id:" + a.alpha.MerchantID().String()
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// A refund row written past the poster commits: the books never
		// refuse. ledger-audit names it and raises a finding.
		skipped := uuid.New()
		_, err := a.pool.Exec(t.Context(), "INSERT INTO "+booksTable(a.world, "payments")+
			" (merchant_id, id, customer_id, order_id, channel, rail, psp_id, transaction_id, amount, list_amount, currency, status, purchased_at, refunded_payment_id, reversal_kind, refund_reason, money_movement)"+
			" SELECT merchant_id, $3, customer_id, order_id, channel, rail, psp_id, 're_skipped', -3000000, list_amount, currency, 'succeeded', now(), id, 'refund', 'requested_by_customer', 'rail'"+
			" FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), payment, skipped)
		require.NoError(t, err)
		output, err := hostCLIResult(t, a.world, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "unposted refund: payment="+skipped.String())
		found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.entry.missing"})
		require.NoError(t, err)
		require.Len(t, found.Items, 1)
		require.Equal(t, "payment:"+skipped.String(), found.Items[0].SubjectKey)
	})

	t.Run("OR-J41-S22/a-duplicate-captures-refund-returns-it-from-unapplied-cash", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		price, product := catalogLine(t, a, "sale", 10_000_000, nil)
		customer := uuid.MustParse(a.alice.ID)
		psp := worldStripePSP(t, a)
		now := a.clock.Now().UTC().Truncate(time.Microsecond)
		bill := func(transaction string) observed.Bill {
			return observed.Bill{PSPID: psp, CustomerID: customer, Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "provider_purchase", SourceKey: "twice-paid",
				Currency: "USD", Total: 10_000_000, Lines: []observed.Line{{PriceID: &price, ProductID: &product, Description: "Sale", Quantity: ptr(1), UnitAmount: 10_000_000, Amount: 10_000_000}},
				Payment: &models.Payment{CustomerID: customer, Rail: models.RailStripe, TransactionID: transaction, Amount: 10_000_000, ListAmount: 10_000_000, Currency: "USD",
					Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
				Settled: true, SettlementSource: "cash"}
		}
		require.True(t, recordBill(t, a.world, bill("ch_twice_first"), now).Completed)
		again := recordBill(t, a.world, bill("ch_twice_second"), now)
		require.True(t, again.PaymentCreated)
		require.EqualValues(t, 10_000_000, unappliedCash(t, a.world, "USD"))

		// The provider refunds the second capture: its money leaves from
		// unapplied cash, where it waited.
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := db.WithPSPID(merchant.WithID(t.Context(), a.alpha.MerchantID()), psp)
		refund, err := payments.NewPaymentService(database, a.clock).RefundObservedWithReason(ctx, again.Payment.ID, "re_twice_second", 10_000_000, billing.RefundDuplicate, now)
		require.NoError(t, err)
		require.Equal(t, []glRow{{Type: "cash_returned", Operation: "refund", Source: "payment", SourceID: refund.ID.String(), Debit: "unapplied_cash", Credit: "cash_clearing", Amount: 10_000_000, At: now}},
			glFor(t, a.world, "payment", refund.ID.String()))
		require.Zero(t, unappliedCash(t, a.world, "USD"))
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S23/a-refund-of-a-payment-from-before-the-books-is-never-refused", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		customer := uuid.MustParse(a.alice.ID)
		psp := worldStripePSP(t, a)
		now := a.clock.Now().UTC().Truncate(time.Microsecond)
		// A charge from before the books: no posting holds its money.
		paid := uuid.New()
		_, err := a.pool.Exec(t.Context(), "INSERT INTO "+booksTable(a.world, "customers")+" (merchant_id, id) VALUES ($1, $2) ON CONFLICT DO NOTHING", a.alpha.MerchantID().UUID(), customer)
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "INSERT INTO "+booksTable(a.world, "payments")+
			" (merchant_id, id, customer_id, channel, rail, psp_id, transaction_id, amount, list_amount, currency, status, purchased_at, money_movement)"+
			" VALUES ($1, $2, $3, 'rail', 'stripe', $4, 'ch_before_books', 10000000, 10000000, 'USD', 'succeeded', $5, 'rail')", a.alpha.MerchantID().UUID(), paid, customer, psp, now.Add(-24*time.Hour))
		require.NoError(t, err)

		// Its refund posts: the money leaves unapplied cash, which goes below
		// zero, and that is a finding, not a refusal.
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := db.WithPSPID(merchant.WithID(t.Context(), a.alpha.MerchantID()), psp)
		refund, err := payments.NewPaymentService(database, a.clock).RefundObservedWithReason(ctx, paid, "re_before_books", 4_000_000, billing.RefundRequestedByCustomer, now)
		require.NoError(t, err)
		require.Equal(t, []glRow{{Type: "cash_returned", Operation: "refund", Source: "payment", SourceID: refund.ID.String(), Debit: "unapplied_cash", Credit: "cash_clearing", Amount: 4_000_000, At: now}},
			glFor(t, a.world, "payment", refund.ID.String()))
		require.EqualValues(t, -4_000_000, unappliedCash(t, a.world, "USD"))
		breaches, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.sign_breach"})
		require.NoError(t, err)
		require.Len(t, breaches.Items, 1)
		require.Equal(t, "unapplied_cash/USD", breaches.Items[0].SubjectKey)
	})
}

// worldStripePSP is the payment world's Stripe PSP.
func worldStripePSP(t *testing.T, a *authorityWorld) uuid.UUID {
	t.Helper()
	var psp uuid.UUID
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(a.world, "psps")+" WHERE merchant_id=$1 AND rail='stripe'", a.alpha.MerchantID().UUID()).Scan(&psp))
	return psp
}
