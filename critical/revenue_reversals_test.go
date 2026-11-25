//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices/observed"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/open-rails/openrails/internal/stripemock"
)

// TestRevenueReversals is journey J41's reversals: refunds, chargebacks and
// won disputes on the general ledger.
func TestRevenueReversals(t *testing.T) {
	t.Run("OR-J41-S18/a-partial-refund-while-service-runs-is-a-price-reduction-with-catch-up", func(t *testing.T) {
		start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)
		a, provider := newPaymentWorld(t, atClock(start))
		price := calendarPrice(t, a, "monthly", 10_000_000, 1)
		invoice := buy(t, a, provider, "monthly-plan", []billing.InvoiceLineParams{{PriceID: price}}, 10_000_000)
		line, payment := invoice.Lines[0].ID.UUID(), *invoice.Payment.PaymentID
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
			{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: r, Debit: "refunds", Credit: "cash_clearing", Amount: 866_666, At: refundedAt, InvoiceLine: &line, Schedule: &sched.ID},
			{Type: "refund_deferred", Operation: "refund", Source: "payment", SourceID: r, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 3_133_334, At: refundedAt, InvoiceLine: &line, Schedule: &sched.ID},
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
		invoice := buy(t, a, provider, "pass", []billing.InvoiceLineParams{{PriceID: pass}}, 9_000_000)
		line := invoice.Lines[0].ID.UUID()
		sched := scheduleOfLine(t, a.world, line)

		// Five days in, refunded in full with the access revoked: the access
		// ends first, so its five days are recognised, then the refund takes
		// the 7.50 still deferred and reverses the 1.50 earned.
		refundedAt := start.Add(120 * time.Hour)
		a.clock.Advance(refundedAt.Sub(a.clock.Now()))
		refund, err := a.alpha.RefundPayment(t.Context(), "full-refund", *invoice.Payment.PaymentID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "Full refund"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, refund.Status)
		r := refund.ID.UUID().String()
		require.Equal(t, []glRow{
			{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: r, Debit: "refunds", Credit: "cash_clearing", Amount: 1_500_000, At: refundedAt, InvoiceLine: &line, Schedule: &sched.ID},
			{Type: "refund_deferred", Operation: "refund", Source: "payment", SourceID: r, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 7_500_000, At: refundedAt, InvoiceLine: &line, Schedule: &sched.ID},
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
		line := sched.InvoiceLine
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
			{Type: "chargeback_recognized", Operation: "chargeback", Source: "payment", SourceID: c, Debit: "chargebacks", Credit: "cash_clearing", Amount: 2_500_000, At: disputedAt, InvoiceLine: &line, Schedule: &sched.ID},
			{Type: "chargeback_deferred", Operation: "chargeback", Source: "payment", SourceID: c, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 7_500_000, At: disputedAt, InvoiceLine: &line, Schedule: &sched.ID},
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
			{Type: "dispute_won", Operation: "dispute_won", Source: "payment", SourceID: won, Debit: "cash_clearing", DebitPSP: &psp, Credit: "chargebacks", Amount: 2_500_000, At: disputedAt, InvoiceLine: &line, Schedule: &sched.ID},
			{Type: "dispute_won", Operation: "dispute_won_deferred", Source: "payment", SourceID: won, Debit: "cash_clearing", DebitPSP: &psp, Credit: "deferred_revenue", Amount: 7_500_000, At: disputedAt, InvoiceLine: &line, Schedule: &sched.ID},
			{Type: "revenue_recognized", Operation: "dispute_won", Source: "payment", SourceID: won, Debit: "deferred_revenue", Credit: "subscription_revenue", Amount: 7_500_000, At: disputedAt, InvoiceLine: &line, Schedule: &sched.ID},
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
		invoice := buy(t, a, provider, "sale", []billing.InvoiceLineParams{{PriceID: sale}}, 10_000_000)
		payment := invoice.Payment.PaymentID.UUID()
		audit := "id:" + a.alpha.MerchantID().String()
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// A refund row written past the poster commits: the books never
		// refuse. ledger-audit names it and raises a finding.
		// Stored by the payment writer's own clock, as the books' start is.
		skipped := uuid.New()
		_, err := a.pool.Exec(t.Context(), "INSERT INTO "+booksTable(a.world, "payments")+
			" (merchant_id, id, customer_id, invoice_id, channel, rail, psp_id, transaction_id, amount, list_amount, currency, status, purchased_at, created_at, refunded_payment_id, reversal_kind, refund_reason, money_movement)"+
			" SELECT merchant_id, $3, customer_id, invoice_id, channel, rail, psp_id, 're_skipped', -3000000, list_amount, currency, 'succeeded', $4, $4, id, 'refund', 'requested_by_customer', 'rail'"+
			" FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), payment, skipped, a.clock.Now())
		require.NoError(t, err)
		output, err := hostCLIResult(t, a.world, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "unposted refund: payment="+skipped.String())
		found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.entry.missing"})
		require.NoError(t, err)
		require.Len(t, found.Items, 1)
		require.Equal(t, "payment:"+skipped.String(), found.Items[0].SubjectKey)

		// Posted at last, it passes, and its finding closes.
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.PostReversal(ctx, database.NewWithPgxTx(tx), skipped, a.clock.Now())
		}))
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", audit), "Ledger integrity OK")
		found, err = a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.entry.missing", Status: "fixed"})
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
		database := configuredDB(t, a.world)
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
		// A charge from before the books, for its invoice: no posting holds its
		// money.
		paid, yesterday := uuid.New(), now.Add(-24*time.Hour)
		invoice, _ := rawInvoice(t, a.world, customer, "USD", &yesterday, rawLine{amount: 10_000_000})
		_, err := a.pool.Exec(t.Context(), "INSERT INTO "+booksTable(a.world, "payments")+
			" (merchant_id, id, customer_id, invoice_id, channel, rail, psp_id, transaction_id, amount, list_amount, currency, status, purchased_at, money_movement)"+
			" VALUES ($1, $2, $3, $4, 'rail', 'stripe', $5, 'ch_before_books', 10000000, 10000000, 'USD', 'succeeded', $6, 'rail')", a.alpha.MerchantID().UUID(), paid, customer, invoice, psp, yesterday)
		require.NoError(t, err)

		// Its refund posts: the money leaves unapplied cash, which goes below
		// zero, and that is a finding, not a refusal.
		database := configuredDB(t, a.world)
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

	t.Run("OR-J41-S24/a-usage-invoices-refund-is-contra-and-its-chargeback-reopens-the-owed", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, false)
		oneUsage(t, a.alpha, usageInput(customer, "refunded-work", 3))
		a.clock.Advance(time.Second)
		fleet := startFleet(t, a.world)
		runUsageDue(t, fleet)
		require.Eventually(t, func() bool { return oneUsageInvoice(t, a, customer).Status == billing.InvoiceComplete }, 15*time.Second, 20*time.Millisecond)
		invoice := oneUsageInvoice(t, a, customer)
		var paid uuid.UUID
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND invoice_id=$2",
			a.alpha.MerchantID().UUID(), invoice.ID.UUID()).Scan(&paid))
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		service := payments.NewPaymentService(configuredDB(t, a.world), a.clock)
		now := a.clock.Now().UTC().Truncate(time.Microsecond)
		// Its cash settled the usage receivable: none waits unapplied.
		require.Zero(t, unappliedCash(t, a.world, "USD"))

		// The merchant refunds 1.00 of the delivered usage: nothing owed
		// reopens, and nothing is left unapplied, so it is contra revenue.
		refund, err := service.RecordObservedRefund(ctx, paid, "usage-refund", 1_000_000, payments.ReversalRefund, now, nil)
		require.NoError(t, err)
		require.Equal(t, []glRow{{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: refund.ID.String(), Debit: "refunds", Credit: "cash_clearing", Amount: 1_000_000, At: now}},
			glFor(t, a.world, "payment", refund.ID.String()))

		// The rest is charged back: the owed amount stands again, receivable
		// once more, with no contra.
		chargeback, err := service.Refund(ctx, paid, "usage-chargeback", 29_000_000, payments.ReversalChargeback)
		require.NoError(t, err)
		require.Equal(t, []glRow{{Type: "usage_reopened", Operation: "chargeback", Source: "payment", SourceID: chargeback.ID.String(), Debit: "usage_receivable", Credit: "cash_clearing", Amount: 29_000_000, At: now}},
			glFor(t, a.world, "payment", chargeback.ID.String()))
		require.Empty(t, glQuery(t, a.world, "t.transfer_type IN ('chargeback_recognized', 'chargeback_deferred')"))
		require.Zero(t, unappliedCash(t, a.world, "USD"))
		funds, err := a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.EqualValues(t, 29_000_000, funds.OwedAmount)
	})

	t.Run("OR-J41-S25/refunds-in-pieces-each-reduce-the-price-from-where-the-last-left-it", func(t *testing.T) {
		start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)
		a, provider := newPaymentWorld(t, atClock(start))
		price := calendarPrice(t, a, "monthly", 10_000_000, 1)
		invoice := buy(t, a, provider, "monthly-plan", []billing.InvoiceLineParams{{PriceID: price}}, 10_000_000)
		line, payment := invoice.Lines[0].ID.UUID(), *invoice.Payment.PaymentID
		sched := scheduleOfLine(t, a.world, line)
		refund := func(key string, at time.Time, amount int64) string {
			t.Helper()
			a.clock.Advance(at.Sub(a.clock.Now()))
			r, err := a.alpha.RefundPayment(t.Context(), key, payment, billing.RefundPaymentParams{Amount: amount, Reason: "Partial refund"})
			require.NoError(t, err)
			return r.ID.UUID().String()
		}
		first, second := start.Add(156*time.Hour), start.Add(240*time.Hour)
		refund("first-piece", first, 4_000_000)
		// Ten days in, at 6.00 the month earned 2.00: 0.70 more is recognised,
		// then the price falls to 4.00, which earned 1.333333.
		r := refund("second-piece", second, 2_000_000)
		require.Equal(t, []glRow{
			{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: r, Debit: "refunds", Credit: "cash_clearing", Amount: 666_667, At: second, InvoiceLine: &line, Schedule: &sched.ID},
			{Type: "refund_deferred", Operation: "refund", Source: "payment", SourceID: r, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 1_333_333, At: second, InvoiceLine: &line, Schedule: &sched.ID},
		}, glFor(t, a.world, "payment", r))
		require.Equal(t, []glRow{
			recognized(sched.ID, line, "recognition", first, first, 2_166_666),
			recognized(sched.ID, line, "recognition", second, second, 700_000),
		}, recognitions(t, a.world, sched.ID))
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "subscription_revenue", Funded: 4_000_000, Recognized: 1_333_333, Starts: start, Ends: end, Through: second, Next: &end},
			scheduleOfLine(t, a.world, line).terms())
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S26/a-refund-of-a-two-line-bill-takes-each-line-pro-rata", func(t *testing.T) {
		start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		plan := calendarPrice(t, a, "monthly", 10_000_000, 1)
		kit := oneOffPrice(t, a, "kit", 20_000_000, nil)
		invoice := buy(t, a, provider, "two-lines", []billing.InvoiceLineParams{{PriceID: plan}, {PriceID: kit}}, 30_000_000)
		planLine, kitLine := invoice.Lines[0].ID.UUID(), invoice.Lines[1].ID.UUID()
		planSched, kitSched := scheduleOfLine(t, a.world, planLine), scheduleOfLine(t, a.world, kitLine)

		// 6.00 of 30.00: 2.00 from the plan, a price reduction six and a half
		// days in; 4.00 from the kit, delivered, all contra.
		at := start.Add(156 * time.Hour)
		a.clock.Advance(at.Sub(a.clock.Now()))
		refund, err := a.alpha.RefundPayment(t.Context(), "two-lines", *invoice.Payment.PaymentID, billing.RefundPaymentParams{Amount: 6_000_000, Reason: "Partial refund"})
		require.NoError(t, err)
		r := refund.ID.UUID().String()
		require.ElementsMatch(t, []glRow{
			{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: r, Debit: "refunds", Credit: "cash_clearing", Amount: 433_333, At: at, InvoiceLine: &planLine, Schedule: &planSched.ID},
			{Type: "refund_deferred", Operation: "refund", Source: "payment", SourceID: r, Debit: "deferred_revenue", Credit: "cash_clearing", Amount: 1_566_667, At: at, InvoiceLine: &planLine, Schedule: &planSched.ID},
			{Type: "refund_recognized", Operation: "refund", Source: "payment", SourceID: r, Debit: "refunds", Credit: "cash_clearing", Amount: 4_000_000, At: at, InvoiceLine: &kitLine, Schedule: &kitSched.ID},
		}, glFor(t, a.world, "payment", r))
		require.EqualValues(t, 8_000_000, scheduleOfLine(t, a.world, planLine).Funded)
		require.EqualValues(t, 16_000_000, scheduleOfLine(t, a.world, kitLine).Funded)
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S27/a-mixed-credit-invoices-reversals-move-each-dollar-once", func(t *testing.T) {
		m := newMixedSale(t)
		a := m.a
		payment := m.buy(t, "mixed-books")
		lot := paymentLot(t, a.alpha, m.customer, payment)

		// 40.00 refunded: the lot's 10.00 first, by its poster, then 30.00 of
		// the delivered toolkit, all contra.
		refund, err := a.alpha.RefundPayment(t.Context(), "mixed-books-refund", payment, billing.RefundPaymentParams{Amount: 40_000_000, Reason: "Partial refund"})
		require.NoError(t, err)
		r := refund.ID.UUID().String()
		require.EqualValues(t, 40_000_000, cashMoved(t, a.world, r), "each dollar once")
		require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "refund_deferred"))
		legs := glFor(t, a.world, "payment", r)
		require.Len(t, legs, 1)
		require.Equal(t, "refund_recognized", legs[0].Type)
		require.EqualValues(t, 30_000_000, legs[0].Amount)

		// The other 60.00 is charged back and won: neither the lot nor the
		// toolkit gives or gets back more than it took.
		charge, intent := m.charge(t, payment)
		dispute := stripemock.Object{"id": "dp_mixed_books", "object": "dispute", "amount": 6_000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
		m.notice(t, "evt_mixed_books_dispute", "charge.dispute.created", dispute)
		dispute["status"] = "won"
		m.notice(t, "evt_mixed_books_won", "charge.dispute.closed", dispute)
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 3)
		for _, rv := range read.Refunds {
			if rv.ID == refund.ID {
				continue
			}
			moved := cashMoved(t, a.world, rv.ID.UUID().String())
			if rv.Amount < 0 {
				require.EqualValues(t, -rv.Amount, moved, "the chargeback, once")
			} else {
				require.EqualValues(t, rv.Amount, moved, "the recovery, once")
			}
		}
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S28/an-end-undone-after-a-refund-reinstates-no-forfeit-it-never-made", func(t *testing.T) {
		start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		pass := oneOffPrice(t, a, "pass", 9_000_000, ptr(720))
		invoice := buy(t, a, provider, "pass", []billing.InvoiceLineParams{{PriceID: pass}}, 9_000_000)
		line, access := invoice.Lines[0].ID.UUID(), *invoice.Lines[0].ProductAccessID
		sched := scheduleOfLine(t, a.world, line)

		// Revoked five days in, then 3.00 refunded: its five days are
		// recognised, and the refund takes 3.00 of what is still deferred.
		revoked := start.Add(120 * time.Hour)
		a.clock.Advance(revoked.Sub(a.clock.Now()))
		require.NoError(t, a.alpha.RevokeProductAccess(t.Context(), access, billing.RevokeProductAccessParams{Reason: "Ended by the merchant"}))
		_, err := a.alpha.RefundPayment(t.Context(), "after-revoke", *invoice.Payment.PaymentID, billing.RefundPaymentParams{Amount: 3_000_000, Reason: "Partial refund"})
		require.NoError(t, err)
		reduced := scheduleOfLine(t, a.world, line)
		require.EqualValues(t, 6_000_000, reduced.Funded)
		require.EqualValues(t, 1_500_000, reduced.Recognized)

		// The revocation undone: nothing was forfeited, so nothing is
		// reinstated, though 1.50 is more than 6.00 earned by then.
		undone := revoked.Add(24 * time.Hour)
		a.clock.Advance(undone.Sub(a.clock.Now()))
		_, err = a.pool.Exec(t.Context(), "UPDATE "+booksTable(a.world, "product_access")+" SET revoked_at=NULL, revoke_reason=NULL WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), access.UUID())
		require.NoError(t, err)
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, undone.Add(time.Hour))
		end := start.Add(720 * time.Hour)
		passAt(t, a, fleet, end.Add(time.Hour))
		require.Equal(t, []glRow{
			recognized(sched.ID, line, "recognition", revoked, revoked.Add(-time.Microsecond), 1_500_000),
			recognized(sched.ID, line, "recognition", end, end.Add(-time.Microsecond), 4_500_000),
		}, recognitions(t, a.world, sched.ID))
		require.Zero(t, scheduleOfLine(t, a.world, line).Reinstatements)
	})

	t.Run("OR-J41-S29/a-refund-the-rail-confirms-later-is-dated-when-it-settled", func(t *testing.T) {
		// Begun an hour ago: it settles twenty minutes on, never in the future.
		m := newMixedSale(t, func(w *world) { w.clock.Advance(-time.Hour) })
		a, provider := m.a, m.p
		payment := m.buy(t, "settled-later")

		// The rail's answer is lost: the refund waits, reserved, and neither
		// the lot's poster nor the payment's posts anything.
		provider.mu.Lock()
		provider.lostRefundResponse = true
		provider.mu.Unlock()
		_, _ = a.alpha.RefundPayment(t.Context(), "later", payment, billing.RefundPaymentParams{Amount: 40_000_000, Reason: "Partial refund"})
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 1)
		refund := read.Refunds[0].ID.UUID().String()
		require.Empty(t, glQuery(t, a.world, "t.source_id=$2", refund))

		// Twenty minutes later the retry learns it settled: the lot's legs and
		// the toolkit's are all dated then.
		provider.mu.Lock()
		provider.lostRefundResponse = false
		provider.mu.Unlock()
		a.clock.Advance(20 * time.Minute)
		settled := a.clock.Now().UTC().Truncate(time.Microsecond)
		fleet := startFleet(t, a.world)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if job.State != rivertype.JobStateCompleted && job.State != rivertype.JobStateRunning {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		require.Eventually(t, func() bool {
			read, err := a.alpha.GetPayment(t.Context(), payment)
			return err == nil && read.AmountRefunded == 40_000_000
		}, 20*time.Second, 20*time.Millisecond)
		lot := glQuery(t, a.world, "t.source_id=$2 AND t.operation='purchased_credit_refund'", refund)
		require.NotEmpty(t, lot)
		own := glFor(t, a.world, "payment", refund)
		require.Len(t, own, 1)
		require.EqualValues(t, 30_000_000, own[0].Amount)
		for _, leg := range append(lot, own...) {
			require.Equal(t, settled, leg.At, "%+v", leg)
		}
		require.EqualValues(t, 40_000_000, cashMoved(t, a.world, refund))
	})

	t.Run("OR-J41-S30/a-timed-line-refunded-before-its-access-is-not-unscheduled", func(t *testing.T) {
		start := time.Date(2026, time.July, 6, 9, 0, 0, 0, time.UTC)
		a, _ := newPaymentWorld(t, atClock(start))
		pass, product := catalogLine(t, a, "pass", 9_000_000, ptr(720))
		customer := uuid.MustParse(a.alice.ID)
		psp := worldStripePSP(t, a)
		now := a.clock.Now().UTC().Truncate(time.Microsecond)
		paid := recordBill(t, a.world, observed.Bill{PSPID: psp, CustomerID: customer, Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "provider_purchase", SourceKey: "refunded-pass",
			Currency: "USD", Total: 9_000_000, Lines: []observed.Line{{PriceID: &pass, ProductID: &product, Description: "Pass", Quantity: ptr(1), UnitAmount: 9_000_000, Amount: 9_000_000, AccessDurationHours: ptr(720)}},
			Payment: &models.Payment{CustomerID: customer, Rail: models.RailStripe, TransactionID: "ch_refunded_pass", Amount: 9_000_000, ListAmount: 9_000_000, Currency: "USD",
				Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
			Settled: true, SettlementSource: "cash"}, now)
		require.True(t, paid.Completed)

		// Refunded whole before its access was granted: nothing is left for
		// the line, so it is not missing a schedule.
		database := configuredDB(t, a.world)
		ctx := db.WithPSPID(merchant.WithID(t.Context(), a.alpha.MerchantID()), psp)
		_, err := payments.NewPaymentService(database, a.clock).RefundObservedWithReason(ctx, paid.Payment.ID, "re_refunded_pass", 9_000_000, billing.RefundRequestedByCustomer, now)
		require.NoError(t, err)
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.ApplyInvoice(ctx, database.NewWithPgxTx(tx), paid.Invoice.ID, nil)
		}))
		found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.line.unapplied"})
		require.NoError(t, err)
		require.Empty(t, found.Items)
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S31/a-reversals-share-of-a-credit-lot-is-decided-when-it-settles", func(t *testing.T) {
		// $10 charged back takes the whole credit line; $90 refunded then
		// finds none and refunds the toolkit. The won dispute restores the
		// credit to the customer, and no later pass hands it to the refund.
		t.Run("card-credits", func(t *testing.T) {
			m := newMixedSale(t)
			a := m.a
			payment := m.buy(t, "decided-share")
			charge, intent := m.charge(t, payment)
			dispute := stripemock.Object{"id": "dp_decided_share", "object": "dispute", "amount": 1000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
			m.notice(t, "evt_decided_share_dispute", "charge.dispute.created", dispute)
			refund, err := a.alpha.RefundPayment(t.Context(), "decided-share-refund", payment, billing.RefundPaymentParams{Amount: 90_000_000, Reason: "Return the toolkit"})
			require.NoError(t, err)
			r := refund.ID.UUID()
			require.Equal(t, [2]int64{0, 0}, lotShare(t, a.world, r), "it took none of the lot")
			require.EqualValues(t, 90_000_000, cashMoved(t, a.world, r.String()))

			dispute["status"] = "won"
			m.notice(t, "evt_decided_share_won", "charge.dispute.closed", dispute)
			balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
			require.False(t, applyLotReversal(t, a.world, payment.UUID(), r), "the next pass moves nothing")
			balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
			require.EqualValues(t, 90_000_000, cashMoved(t, a.world, r.String()), "90 moved for 90")
			booksAudited(t, a.world)
		})
		// The same in a merchant's own unit: 400 GOLD bought for $20 with a
		// $30 item, all checked out together.
		t.Run("merchant-unit", func(t *testing.T) {
			s, p := unitPaymentWorld(t, 0)
			a := s.a
			gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"})
			item, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "item", DisplayName: "Item", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"item:owned"}})
			require.NoError(t, err)
			itemPrice, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: item.ID, Key: "item", UnitAmount: 30_000_000, Currency: "USD"})
			require.NoError(t, err)
			m := &mixedSale{a: a, p: p, customer: s.alice, lines: []billing.InvoiceLineParams{{PriceID: gold["usd"]}, {PriceID: itemPrice.ID}}}
			input := billing.CreateMyInvoiceParams{Lines: m.lines, ExpectedTotal: ptr(int64(50_000_000)), Reusable: ptr(true),
				Payment: &billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
			status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "gold-and-item"})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var invoice billing.Invoice
			require.NoError(t, json.Unmarshal(raw, &invoice))
			require.Equal(t, billing.InvoiceComplete, invoice.Status)
			payment := *invoice.Payment.PaymentID
			unitBalance(t, a.alpha, s.alice, "GOLD", 400, 0, 400)

			// R1, a $20 chargeback, takes the pack's line: 400 GOLD, $20.
			charge, intent := m.charge(t, payment)
			dispute := stripemock.Object{"id": "dp_gold_share", "object": "dispute", "amount": 2000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
			m.notice(t, "evt_gold_share_dispute", "charge.dispute.created", dispute)
			unitBalance(t, a.alpha, s.alice, "GOLD", 0, 0, 0)
			read, err := a.alpha.GetPayment(t.Context(), payment)
			require.NoError(t, err)
			require.Len(t, read.Refunds, 1)
			require.Equal(t, [2]int64{20_000_000, 400}, lotShare(t, a.world, read.Refunds[0].ID.UUID()))

			// R2, $30 refunded, finds the line returned: it takes none of the
			// lot and is booked against the item.
			refund, err := a.alpha.RefundPayment(t.Context(), "gold-item-refund", payment, billing.RefundPaymentParams{Amount: 30_000_000, Reason: "Return the item"})
			require.NoError(t, err)
			r := refund.ID.UUID()
			require.Equal(t, [2]int64{0, 0}, lotShare(t, a.world, r))
			require.EqualValues(t, 30_000_000, cashMoved(t, a.world, r.String()))

			// R1 won: its 400 GOLD and $20 come back, and stay.
			dispute["status"] = "won"
			m.notice(t, "evt_gold_share_won", "charge.dispute.closed", dispute)
			unitBalance(t, a.alpha, s.alice, "GOLD", 400, 0, 400)
			require.False(t, applyLotReversal(t, a.world, payment.UUID(), r), "the next pass moves nothing")
			unitBalance(t, a.alpha, s.alice, "GOLD", 400, 0, 400)
			require.Zero(t, systemBalance(t, a, "GOLD", ledger.UnitsRefunded), "400 refunded, 400 restored, none to R2")
			require.EqualValues(t, 30_000_000, cashMoved(t, a.world, r.String()), "30 moved for 30")
			booksAudited(t, a.world)
		})
	})

	t.Run("OR-J41-S32/a-reversal-posted-for-other-cash-than-its-amount-fails-ledger-audit", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		sale := oneOffPrice(t, a, "sale", 10_000_000, nil)
		invoice := buy(t, a, provider, "sale", []billing.InvoiceLineParams{{PriceID: sale}}, 10_000_000)
		refund, err := a.alpha.RefundPayment(t.Context(), "misbalanced", *invoice.Payment.PaymentID, billing.RefundPaymentParams{Amount: 3_000_000, Reason: "Partial refund"})
		require.NoError(t, err)
		audit := "id:" + a.alpha.MerchantID().String()
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// A second split of the same refund, as a poster that revisits it
		// would post: it commits, and ledger-audit names it.
		r := refund.ID.UUID()
		customer, psp := uuid.MustParse(a.alice.ID), worldStripePSP(t, a)
		require.NoError(t, booksTx(t, a.world, func(ctx context.Context, q *gen.Queries) error {
			_, _, err := books.New(q, a.alpha.MerchantID().UUID()).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.Refunds}, Credit: books.Ref{Type: books.CashClearing, PSP: &psp},
				Amount: 1_000_000, Currency: "USD", Type: books.RefundRecognized,
				Coord:       books.Coord{Operation: books.OpRefund, Source: "payment", SourceID: r.String()},
				EffectiveAt: a.clock.Now(), Customer: &customer,
			})
			return err
		}))
		output, err := hostCLIResult(t, a.world, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "misbalanced refund: payment="+r.String())
		found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.reversal.misbalanced"})
		require.NoError(t, err)
		require.Len(t, found.Items, 1)
		require.Equal(t, "payment:"+r.String(), found.Items[0].SubjectKey)
	})

	t.Run("OR-J41-S33/a-usage-payment-starts-the-books", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, false)
		oneUsage(t, a.alpha, usageInput(customer, "first-receipt", 3))
		a.clock.Advance(time.Second)
		fleet := startFleet(t, a.world)
		runUsageDue(t, fleet)
		require.Eventually(t, func() bool { return oneUsageInvoice(t, a, customer).Status == billing.InvoiceComplete }, 15*time.Second, 20*time.Millisecond)
		invoice := oneUsageInvoice(t, a, customer)
		var paid uuid.UUID
		var started time.Time
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND invoice_id=$2",
			a.alpha.MerchantID().UUID(), invoice.ID.UUID()).Scan(&paid))
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT started_at FROM "+booksTable(a.world, "books_starts")+" WHERE merchant_id=$1",
			a.alpha.MerchantID().UUID()).Scan(&started), "the usage payment, the merchant's first receipt, started the books")
		audit := "id:" + a.alpha.MerchantID().String()

		// So a reversal of it stored past the poster is named.
		skipped := uuid.New()
		_, err := a.pool.Exec(t.Context(), "INSERT INTO "+booksTable(a.world, "payments")+
			" (merchant_id, id, customer_id, invoice_id, channel, rail, psp_id, transaction_id, amount, list_amount, currency, status, purchased_at, created_at, refunded_payment_id, reversal_kind, money_movement)"+
			" SELECT merchant_id, $3, customer_id, invoice_id, channel, rail, psp_id, 'cb_skipped', -1000000, list_amount, currency, 'succeeded', $4, $4, id, 'chargeback', 'rail'"+
			" FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), paid, skipped, a.clock.Now())
		require.NoError(t, err)
		output, err := hostCLIResult(t, a.world, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "unposted chargeback: payment="+skipped.String())
		require.NotContains(t, output, "payment="+paid.String(), "the usage payment itself was posted")
	})

	t.Run("OR-J41-S34/a-refund-after-an-early-end-takes-the-forfeit-first", func(t *testing.T) {
		start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		pass := oneOffPrice(t, a, "pass", 9_000_000, ptr(720))
		invoice := buy(t, a, provider, "pass", []billing.InvoiceLineParams{{PriceID: pass}}, 9_000_000)
		line, access := invoice.Lines[0].ID.UUID(), *invoice.Lines[0].ProductAccessID
		sched := scheduleOfLine(t, a.world, line)

		// Revoked five days in: the pass forfeits the 7.50 it did not earn.
		revoked := start.Add(120 * time.Hour)
		a.clock.Advance(revoked.Sub(a.clock.Now()))
		require.NoError(t, a.alpha.RevokeProductAccess(t.Context(), access, billing.RevokeProductAccessParams{Reason: "Ended by the merchant"}))
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, revoked.Add(time.Hour))
		require.EqualValues(t, 7_500_000, forfeited(t, a.world, sched.ID))

		// 3.00 refunded: the service is over, so it is contra, out of the
		// forfeit first.
		_, err := a.alpha.RefundPayment(t.Context(), "after-forfeit", *invoice.Payment.PaymentID, billing.RefundPaymentParams{Amount: 3_000_000, Reason: "Partial refund"})
		require.NoError(t, err)
		reduced := scheduleOfLine(t, a.world, line)
		require.EqualValues(t, 6_000_000, reduced.Funded)
		require.EqualValues(t, 6_000_000, reduced.Recognized)
		require.EqualValues(t, 4_500_000, forfeited(t, a.world, sched.ID))

		// The revocation undone (no product path undoes one): it reinstates
		// what is left of the forfeit, 4.50, not the 5.00 above what was
		// earned at the new price.
		undone := revoked.Add(24 * time.Hour)
		a.clock.Advance(undone.Sub(a.clock.Now()))
		_, err = a.pool.Exec(t.Context(), "UPDATE "+booksTable(a.world, "product_access")+" SET revoked_at=NULL, revoke_reason=NULL WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), access.UUID())
		require.NoError(t, err)
		passAt(t, a, fleet, undone.Add(time.Hour))
		back := glQuery(t, a.world, "t.revenue_schedule_id=$2 AND t.operation='reinstatement'", sched.ID)
		require.Len(t, back, 1)
		require.EqualValues(t, 4_500_000, back[0].Amount)
		require.Zero(t, forfeited(t, a.world, sched.ID))
	})

	t.Run("OR-J41-S35/a-recovery-naming-no-chargeback-restores-the-oldest-and-never-fails", func(t *testing.T) {
		m := newMixedSale(t)
		a := m.a
		payment := m.buy(t, "unnamed-recovery")
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		service := payments.NewPaymentService(database, a.clock)
		now := a.clock.Now().UTC().Truncate(time.Microsecond)

		// Two chargebacks of 5.00, each half the credit line. The rail
		// reports the later first, so the earlier has the later id.
		late, err := service.RecordObservedRefund(ctx, payment.UUID(), "cb-late", 5_000_000, payments.ReversalChargeback, now, nil)
		require.NoError(t, err)
		early, err := service.RecordObservedRefund(ctx, payment.UUID(), "cb-early", 5_000_000, payments.ReversalChargeback, now.Add(-time.Hour), nil)
		require.NoError(t, err)
		require.Equal(t, [2]int64{5_000_000, 12_500_000}, lotShare(t, a.world, late.ID))
		require.Equal(t, [2]int64{5_000_000, 12_500_000}, lotShare(t, a.world, early.ID))
		balance(t, a.alpha, m.customer, 0, 0, 0)

		// A won dispute's recovery that names neither commits, and restores
		// the one that occurred first.
		original, err := service.GetByID(ctx, payment.UUID())
		require.NoError(t, err)
		kind := payments.ReversalDisputeReversal
		recovery := uuid.New()
		require.NoError(t, service.Create(ctx, &models.Payment{
			ID: recovery, CustomerID: original.CustomerID, InvoiceID: original.InvoiceID, RefundedPaymentID: &original.ID, Rail: original.Rail, PspID: original.PspID,
			TransactionID: "won-unnamed", Amount: 5_000_000, ListAmount: original.ListAmount, Currency: original.Currency, ReversalKind: &kind,
			Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail, PurchasedAt: a.clock.Now(), CreatedAt: a.clock.Now(),
		}))
		balance(t, a.alpha, m.customer, 12_500_000, 0, 12_500_000)
		rows, err := a.pool.Query(t.Context(), "SELECT DISTINCT source FROM "+booksTable(a.world, "ledger_transfers")+" WHERE merchant_id=$1 AND operation='purchased_credit_refund' AND source_id=$2",
			a.alpha.MerchantID().UUID(), recovery.String())
		require.NoError(t, err)
		sources, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		require.NotEmpty(t, sources)
		for _, source := range sources {
			require.Contains(t, source, early.ID.String())
		}
		require.EqualValues(t, 5_000_000, cashMoved(t, a.world, recovery.String()))
		booksAudited(t, a.world)
	})

	t.Run("OR-J41-S36/a-provider-discount-never-reaches-a-credit-line", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		item, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "item", DisplayName: "Item", Ownership: catalog.OwnershipConsumable})
		require.NoError(t, err)
		_, err = a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: item.ID, Key: "item", UnitAmount: 90_000_000, Currency: "USD"})
		require.NoError(t, err)
		pack, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "pack", DisplayName: "Pack", Ownership: catalog.OwnershipConsumable,
			CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: ptr(int64(25_000_000))}})
		require.NoError(t, err)
		_, err = a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: pack.ID, Key: "pack", UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		itemPrice, packPrice := priceUUID(t, a.world, item.ID.UUID()), priceUUID(t, a.world, pack.ID.UUID())
		customer := uuid.MustParse(a.alice.ID)
		psp := worldStripePSP(t, a)
		now := a.clock.Now().UTC().Truncate(time.Microsecond)

		// A provider bill: the item and the pack, less 10.00 of discount, paid
		// 90.00. The pack's lot is priced at its own 10.00, so the discount is
		// the item's alone.
		paid := recordBill(t, a.world, observed.Bill{PSPID: psp, CustomerID: customer, Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "provider_purchase", SourceKey: "discounted-mix",
			Currency: "USD", Total: 90_000_000, Lines: []observed.Line{
				{PriceID: &itemPrice, ProductID: ptr(item.ID.UUID()), Description: "Item", Quantity: ptr(1), UnitAmount: 90_000_000, Amount: 90_000_000},
				{PriceID: &packPrice, ProductID: ptr(pack.ID.UUID()), Description: "Pack", Quantity: ptr(1), UnitAmount: 10_000_000, Amount: 10_000_000},
				{Source: "provider_adjustment", Description: "Stripe invoice discount", Amount: -10_000_000}},
			Payment: &models.Payment{CustomerID: customer, Rail: models.RailStripe, TransactionID: "ch_discounted_mix", Amount: 90_000_000, ListAmount: 90_000_000, Currency: "USD",
				Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
			Settled: true, SettlementSource: "cash"}, now)
		require.True(t, paid.Completed)
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.ApplyInvoice(ctx, database.NewWithPgxTx(tx), paid.Invoice.ID, nil)
		}))
		scheduled := invoiceSchedules(t, a.world, paid.Invoice.ID)
		require.Len(t, scheduled, 1, "the item's: the pack's line is its lot's")
		require.EqualValues(t, 80_000_000, scheduled[0].Funded)
		require.EqualValues(t, 10_000_000, unappliedCash(t, a.world, "USD"), "the pack's 10.00 waits for its lot")
	})

	t.Run("OR-J41-S37/an-unbooked-unit-packs-refund-withdraws-its-units-and-books-its-cash-against-the-payment", func(t *testing.T) {
		s, p := unitPaymentWorld(t, 0)
		a, w := s.a, s.a.world
		// A pack bought at checkout counts GOLD and opens its accounts.
		gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"})
		s.buy(t, gold["usd"], 20_000_000, "booked-gold")
		bought := lots(t, a.alpha, s.alice, "GOLD")[0]

		// Another 400 GOLD, bought before the credit books by a payment the
		// books received: its units deposited, its price never deferred.
		shop := &creditShop{a: a, p: p, customer: s.alice}
		payment := shop.cardPayment(t, 20_000_000, models.MoneyMovementRail, "unbooked-gold")
		lot := uuid.New()
		table := booksTable(w, "credit_grants")
		tx, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(t.Context(), "ALTER TABLE "+table+" DISABLE TRIGGER credit_grants_require_books")
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "INSERT INTO "+table+" (merchant_id, id, customer_id, account_id, unit, units, funding_source, paid_amount, paid_currency, source, source_id, invoker, starts_at, payment_id)"+
			" SELECT merchant_id, $3, customer_id, account_id, unit, 400, 'purchase', 20000000, 'USD', 'pack_purchase', $5, invoker, starts_at, $4 FROM "+table+" WHERE merchant_id = $1 AND id = $2",
			w.alpha.MerchantID().UUID(), bought.ID.UUID(), lot, payment, payment.String())
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "ALTER TABLE "+table+" ENABLE TRIGGER credit_grants_require_books")
		require.NoError(t, err)
		require.NoError(t, tx.Commit(t.Context()))
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			_, err := ledger.New(q, w.alpha.MerchantID().UUID()).DepositFrom(ctx, ledger.UnitsSold, s.alice.UUID(), "GOLD", 400,
				ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: lot.String()}, lot)
			return err
		}))
		unitBalance(t, a.alpha, s.alice, "GOLD", 800, 0, 800)

		// Refunded whole: its 400 GOLD leave at once, its share is recorded,
		// and its $20 leaves the payment's unapplied cash.
		ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
		refund, err := payments.NewPaymentService(configuredDB(t, w), w.clock).Refund(ctx, payment, "unbooked-gold-refund", 20_000_000, payments.ReversalRefund)
		require.NoError(t, err)
		unitBalance(t, a.alpha, s.alice, "GOLD", 400, 0, 400)
		require.Equal(t, [2]int64{20_000_000, 400}, lotShare(t, w, refund.ID))
		require.EqualValues(t, 400, systemBalance(t, a, "GOLD", ledger.UnitsRefunded))
		legs := glFor(t, w, "payment", refund.ID.String())
		require.Len(t, legs, 1)
		require.Equal(t, "cash_returned", legs[0].Type)
		require.EqualValues(t, 20_000_000, legs[0].Amount)
		require.Zero(t, unappliedCash(t, w, "USD"))
		found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.credit.unbooked_unit_pack"})
		require.NoError(t, err)
		require.Len(t, found.Items, 1)
		require.Equal(t, "credit_grant:"+lot.String(), found.Items[0].SubjectKey)
		booksAudited(t, w)
	})
}

// lotShare is the cash and units of a lot a reversal took or a recovery
// restored, as decided when it settled.
func lotShare(t *testing.T, w *world, reversal uuid.UUID) [2]int64 {
	t.Helper()
	var cash, units int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT cash, units FROM "+booksTable(w, "credit_reversal_shares")+" WHERE merchant_id=$1 AND reversal_id=$2",
		w.alpha.MerchantID().UUID(), reversal).Scan(&cash, &units))
	return [2]int64{cash, units}
}

// applyLotReversal passes a settled reversal to the lot's poster again, as
// the next reversal of its payment does.
func applyLotReversal(t *testing.T, w *world, payment, reversal uuid.UUID) bool {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	posted, err := purchasedcredits.New(database, w.clock).ApplyReversal(merchant.WithID(t.Context(), w.alpha.MerchantID()), payment, reversal, nil)
	require.NoError(t, err)
	return posted
}

// forfeited is what of a schedule's revenue an early end forfeited and no
// undone end has taken back.
func forfeited(t *testing.T, w *world, schedule uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT forfeited FROM "+booksTable(w, "revenue_schedules")+" WHERE merchant_id=$1 AND id=$2",
		w.alpha.MerchantID().UUID(), schedule).Scan(&n))
	return n
}

// configuredDB is a database a scenario opens itself, reading the world's
// declared merchant configuration as the runtime binds it: a provider
// refund's access policy is read there.
func configuredDB(t *testing.T, w *world) *db.DB {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	database.SetMerchantConfig(declaredConfig{w: w})
	return database
}

// declaredConfig serves the world's merchant declarations.
type declaredConfig struct{ w *world }

func (c declaredConfig) declaration(id billing.MerchantID) billing.MerchantDeclaration {
	for _, m := range c.w.cfg.Merchants {
		if id == c.w.alpha.MerchantID() && m.Slug == "alpha" || c.w.beta != nil && id == c.w.beta.MerchantID() && m.Slug == "beta" {
			return m
		}
	}
	return billing.MerchantDeclaration{}
}

func (c declaredConfig) MerchantSettings(_ context.Context, id billing.MerchantID) (string, billing.MerchantSettings, error) {
	d := c.declaration(id)
	return d.DisplayName, d.MerchantSettings, nil
}

func (c declaredConfig) MerchantMessages(_ context.Context, id billing.MerchantID) (billing.MessageSettings, error) {
	if d := c.declaration(id); d.Messages != nil {
		return *d.Messages, nil
	}
	return billing.MessageSettings{}, nil
}

func (c declaredConfig) StoredCurrencies(_ context.Context, id billing.MerchantID) ([]billing.MerchantCurrency, error) {
	return c.declaration(id).Currencies, nil
}

// worldStripePSP is the payment world's Stripe PSP.
func worldStripePSP(t *testing.T, a *authorityWorld) uuid.UUID {
	t.Helper()
	var psp uuid.UUID
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(a.world, "psps")+" WHERE merchant_id=$1 AND rail='stripe'", a.alpha.MerchantID().UUID()).Scan(&psp))
	return psp
}

// cashMoved is what a reversal's legs, its credit lot's and its own, moved
// on cash accounts.
func cashMoved(t *testing.T, w *world, source string) int64 {
	t.Helper()
	var moved int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(t.amount), 0) FROM "+booksTable(w, "ledger_transfers")+" t JOIN "+
		booksTable(w, "ledger_accounts")+" d ON d.merchant_id=t.merchant_id AND d.id=t.debit_account_id JOIN "+booksTable(w, "ledger_accounts")+
		" c ON c.merchant_id=t.merchant_id AND c.id=t.credit_account_id WHERE t.merchant_id=$1 AND t.book='general' AND t.source_id=$2"+
		" AND (d.account_type IN ('cash_clearing', 'external_receipts') OR c.account_type IN ('cash_clearing', 'external_receipts'))",
		w.alpha.MerchantID().UUID(), source).Scan(&moved))
	return moved
}
