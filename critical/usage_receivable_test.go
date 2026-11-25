//go:build integration

package critical_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// TestUsageReceivable is the general ledger's usage receivable: what a
// customer owes beyond its balance, accrued as it is owed and settled as the
// units book settles it.
func TestUsageReceivable(t *testing.T) {
	t.Run("OR-J40-S20/usage-beyond-the-balance-is-receivable-collected-by-card-and-by-remittance", func(t *testing.T) {
		t.Run("card", func(t *testing.T) {
			a, customer, _, _ := cardCollectedUsage(t)
			w := a.world
			require.Equal(t, [2]int64{30_000_000, 30_000_000}, booksTotal(t, w, "general", "usage_receivable"), "accrued as it posted, collected by the card")
			require.EqualValues(t, 30_000_000, glBalance(t, w, "usage_revenue"))
			require.Equal(t, [2]int64{30_000_000, 30_000_000}, booksTotal(t, w, "general", "unapplied_cash"), "the card's cash is applied to the receivable")
			require.Equal(t, [2]int64{0, 30_000_000}, booksTotal(t, w, "general", "cash_clearing"))
			owedIsReceivable(t, a, customer, 0)
			receivableAudited(t, w, false)
		})
		t.Run("remittance", func(t *testing.T) {
			a, customer, order := openUsageOrder(t)
			w := a.world
			owedIsReceivable(t, a, customer, 30_000_000)
			remit(t, a, order, 10_000_000, "bank-part")
			owedIsReceivable(t, a, customer, 20_000_000)
			remit(t, a, order, 20_000_000, "bank-rest")
			owedIsReceivable(t, a, customer, 0)
			require.Equal(t, billing.OrderComplete, oneUsageOrder(t, a, customer).Status)
			require.Equal(t, [2]int64{30_000_000, 30_000_000}, booksTotal(t, w, "general", "usage_receivable"))
			require.Equal(t, [2]int64{0, 30_000_000}, booksTotal(t, w, "general", "external_receipts"))
			require.Zero(t, glBalance(t, w, "unapplied_cash"))
			require.EqualValues(t, 30_000_000, glBalance(t, w, "usage_revenue"))
			receivableAudited(t, w, false)
		})
	})
	t.Run("OR-J40-S21/a-waiver-and-an-uncollectible-order-collected-or-waived", func(t *testing.T) {
		t.Run("waived", func(t *testing.T) {
			a, customer, order := openUsageOrder(t)
			w := a.world
			_, err := a.alpha.WaiveOrderBalance(t.Context(), order)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 0)
			require.Equal(t, [2]int64{0, 30_000_000}, booksTotal(t, w, "general", "revenue_adjustments"), "the waiver takes the revenue back")
			require.EqualValues(t, 30_000_000, glBalance(t, w, "usage_revenue"))
			require.Zero(t, glBalance(t, w, "allowance_doubtful"))
			receivableAudited(t, w, false)
		})
		t.Run("uncollectible-then-collected", func(t *testing.T) {
			a, customer, order := openUsageOrder(t)
			w := a.world
			_, err := a.alpha.MarkOrderUncollectible(t.Context(), order)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 30_000_000)
			require.Equal(t, [2]int64{0, 30_000_000}, booksTotal(t, w, "general", "bad_debt"))
			require.Equal(t, [2]int64{30_000_000, 0}, booksTotal(t, w, "general", "allowance_doubtful"))
			remit(t, a, order, 10_000_000, "bank-late-part")
			owedIsReceivable(t, a, customer, 20_000_000)
			require.EqualValues(t, 20_000_000, glBalance(t, w, "allowance_doubtful"), "collected, a third of the allowance comes back")
			require.EqualValues(t, -20_000_000, glBalance(t, w, "bad_debt"))
			remit(t, a, order, 20_000_000, "bank-late-rest")
			owedIsReceivable(t, a, customer, 0)
			require.Equal(t, [2]int64{30_000_000, 30_000_000}, booksTotal(t, w, "general", "allowance_doubtful"))
			require.Equal(t, [2]int64{30_000_000, 30_000_000}, booksTotal(t, w, "general", "bad_debt"))
			receivableAudited(t, w, false)
		})
		t.Run("uncollectible-then-waived", func(t *testing.T) {
			a, customer, order := openUsageOrder(t)
			w := a.world
			_, err := a.alpha.MarkOrderUncollectible(t.Context(), order)
			require.NoError(t, err)
			_, err = a.alpha.WaiveOrderBalance(t.Context(), order)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 0)
			require.Equal(t, [2]int64{30_000_000, 30_000_000}, booksTotal(t, w, "general", "allowance_doubtful"), "the allowance is written off")
			require.Equal(t, [2]int64{0, 30_000_000}, booksTotal(t, w, "general", "bad_debt"), "the expense stands")
			require.Equal(t, [2]int64{0, 0}, booksTotal(t, w, "general", "revenue_adjustments"))
			receivableAudited(t, w, false)
		})
	})
	t.Run("OR-J40-S22/units-owed-equal-the-receivable-after-every-path", func(t *testing.T) {
		t.Run("invoiced", func(t *testing.T) {
			a, _, customer := usageBillingWorld(t, true)
			w := a.world
			oneUsage(t, a.alpha, usageInput(customer, "every-path", 1))
			a.clock.Advance(time.Second)
			_, err := a.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "every-path", CustomerID: customer, Currency: "USD", RatePerHour: 360_000})
			require.NoError(t, err)
			fleet := quietFleet(t, w)
			a.clock.Advance(12*time.Hour + 10*time.Second)
			_, err = a.alpha.StopRunningCharge(t.Context(), "every-path", billing.StopRunningChargeParams{})
			require.NoError(t, err)
			// $10.00 of usage and the running charge's 12 h 10 s at $0.36 an
			// hour post as the order bills them.
			runUsageDue(t, fleet)
			order := oneUsageOrder(t, a, customer)
			require.EqualValues(t, 14_321_000, order.Total)
			owedIsReceivable(t, a, customer, 14_321_000)
			remit(t, a, order.ID, 4_321_000, "every-path-part")
			owedIsReceivable(t, a, customer, 10_000_000)
			_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 5_000_000, Source: "critical", SourceID: "every-path-gift"}})
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 5_000_000)
			_, err = a.alpha.MarkOrderUncollectible(t.Context(), order.ID)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 5_000_000)
			require.EqualValues(t, 5_000_000, glBalance(t, w, "allowance_doubtful"))
			remit(t, a, order.ID, 2_000_000, "every-path-late")
			owedIsReceivable(t, a, customer, 3_000_000)
			require.EqualValues(t, 3_000_000, glBalance(t, w, "allowance_doubtful"))
			_, err = a.alpha.WaiveOrderBalance(t.Context(), order.ID)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 0)
			require.Zero(t, glBalance(t, w, "allowance_doubtful"))
			receivableAudited(t, w, false)
		})
		t.Run("charged-back", func(t *testing.T) {
			// A chargeback reopens the debt; with no further usage it is billed
			// at once and collected again.
			a, customer, payment, fleet := cardCollectedUsage(t)
			w := a.world
			owedIsReceivable(t, a, customer, 0)
			ctx, service := paymentService(t, w)
			// A partial chargeback, under the $1.00 minimum invoice.
			_, err := service.Refund(ctx, payment.UUID(), "every-path-chargeback", 500_000, payments.ReversalChargeback)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 500_000)
			runUsageDue(t, fleet)
			require.Eventually(t, func() bool {
				rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
				return err == nil && len(rows.Items) == 2 && rows.Items[0].Status == billing.OrderComplete && rows.Items[1].Status == billing.OrderComplete
			}, 15*time.Second, 20*time.Millisecond, "the reopened debt is billed and collected")
			owedIsReceivable(t, a, customer, 0)
			receivableAudited(t, w, false)
		})
		t.Run("terms-withdrawn", func(t *testing.T) {
			// The merchant stops billing usage in dollars; the reopened debt
			// needs no usage terms and is billed still.
			a, customer, payment, _ := cardCollectedUsage(t)
			w := a.world
			ctx, service := paymentService(t, w)
			_, err := service.Refund(ctx, payment.UUID(), "terms-chargeback", 500_000, payments.ReversalChargeback)
			require.NoError(t, err)
			alpha := cfgMerchant(w.cfg, "alpha")
			alpha.UsageBilling = &billing.UsageBillingSettings{BillingDay: 25, Currencies: []billing.UsageBillingCurrency{{Currency: "EUR", CreditLimit: 30_000_000, MinimumInvoiceAmount: 1_000_000}}}
			setCfgMerchant(&w.cfg, "alpha", alpha)
			usage := money.NewMoneyService(configuredDB(t, w), w.clock)
			billed, err := usage.BillDueUsageAccount(ctx, customer.UUID(), "USD", w.clock.Now())
			require.NoError(t, err)
			require.True(t, billed, "the reopened debt is billed without usage terms")
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			require.NoError(t, err)
			require.Len(t, rows.Items, 2)
			require.Contains(t, []int64{rows.Items[0].Total, rows.Items[1].Total}, int64(500_000))
		})
	})
	t.Run("OR-J40-S27/owed-from-before-the-books-is-collected-as-revenue-beside-running-charges", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true)
		w := a.world
		// $2.00 owed before the books, billed with the next order.
		owedBeforeTheBooks(t, w, customer, 2_000_000, "unbooked-usage")
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			now := a.clock.Now()
			return q.InsertPendingOwedItem(ctx, gen.InsertPendingOwedItemParams{ID: uuid.New(), MerchantID: w.alpha.MerchantID().UUID(), CustomerID: customer.UUID(),
				Currency: "USD", SourceType: "owed_accrual", SourceID: "unbooked-usage", AccruedAt: now, Amount: 2_000_000, Metadata: []byte(`{}`), CreatedAt: now, UpdatedAt: now})
		}))
		_, err := a.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "beside-unbooked", CustomerID: customer, Currency: "USD", RatePerHour: 360_000})
		require.NoError(t, err)
		fleet := quietFleet(t, w)
		a.clock.Advance(12*time.Hour + 10*time.Second)
		_, err = a.alpha.StopRunningCharge(t.Context(), "beside-unbooked", billing.StopRunningChargeParams{})
		require.NoError(t, err)
		runUsageDue(t, fleet)
		order := oneUsageOrder(t, a, customer)
		require.EqualValues(t, 6_321_000, order.Total)
		require.EqualValues(t, -4_321_000, glBalance(t, w, "usage_receivable"), "the running charge's owed is receivable as it posts")
		receivableAudited(t, w, true)
		// Credit repays the receivable at face.
		_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1_000_000, Source: "critical", SourceID: "beside-unbooked-gift"}})
		require.NoError(t, err)
		require.EqualValues(t, -3_321_000, glBalance(t, w, "usage_receivable"))
		// The remittance settles the receivable; the owed from before the
		// books it also settles is revenue.
		remit(t, a, order.ID, 5_321_000, "beside-unbooked")
		require.Zero(t, glBalance(t, w, "usage_receivable"))
		require.Equal(t, []int64{3_321_000}, collected(t, w, "usage_collection"))
		require.Equal(t, []int64{2_000_000}, collected(t, w, "usage_collection_revenue"))
		require.EqualValues(t, 6_321_000, glBalance(t, w, "usage_revenue"))
		owedIsReceivable(t, a, customer, 0)
		receivableAudited(t, w, false)
	})
	t.Run("OR-J40-S28/a-chargeback-of-a-usage-payment-reopens-owed-and-its-won-dispute-credits-what-the-customer-paid-again", func(t *testing.T) {
		a, customer, payment, fleet := surplusCollectedUsage(t)
		w := a.world

		// The network takes the whole $14.33 back: the owed is reopened, the
		// change the lot holds is a loss, and nothing leaves unapplied cash.
		ctx, service := paymentService(t, w)
		chargeback, err := service.Refund(ctx, payment.UUID(), "surplus-chargeback", 14_330_000, payments.ReversalChargeback)
		require.NoError(t, err)
		owedIsReceivable(t, a, customer, 14_321_000)
		require.Equal(t, []int64{14_321_000}, glLegs(t, w, "usage_reopened"))
		require.Equal(t, [2]int64{0, 9_000}, booksTotal(t, w, "general", "chargebacks"))
		require.Zero(t, glBalance(t, w, "unapplied_cash"))
		require.Equal(t, billing.OrderComplete, oneUsageOrder(t, a, customer).Status, "the order stays as it was paid")
		// Replayed, the chargeback reopens nothing more.
		_, err = service.Refund(ctx, payment.UUID(), "surplus-chargeback", 14_330_000, payments.ReversalChargeback)
		require.NoError(t, err)
		owedIsReceivable(t, a, customer, 14_321_000)

		// The reopened debt is billed at once and the card pays it again.
		runUsageDue(t, fleet)
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 2 && rows.Items[0].Status == billing.OrderComplete && rows.Items[1].Status == billing.OrderComplete
		}, 15*time.Second, 20*time.Millisecond)
		owedIsReceivable(t, a, customer, 0)

		// Then the dispute is won: the chargeback's $0.009 loss comes back
		// first (the change lot is still the customer's), and the customer,
		// having paid twice, is credited the other $14.321 at face, with a
		// finding for the merchant.
		original, err := service.GetByID(ctx, payment.UUID())
		require.NoError(t, err)
		kind := payments.ReversalDisputeReversal
		recovery := uuid.New()
		require.NoError(t, service.Create(ctx, &models.Payment{
			ID: recovery, CustomerID: original.CustomerID, OrderID: original.OrderID, RefundedPaymentID: &original.ID, Rail: original.Rail, PspID: original.PspID,
			TransactionID: "surplus-won", Metadata: map[string]any{"reverses_payment_id": chargeback.ID.String()}, Amount: 14_330_000, ListAmount: original.ListAmount,
			Currency: original.Currency, ReversalKind: &kind, Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail,
			PurchasedAt: w.clock.Now(), CreatedAt: w.clock.Now(),
		}))
		owedIsReceivable(t, a, customer, 0)
		require.Equal(t, []int64{9_000, 14_321_000}, glLegs(t, w, "dispute_won"))
		require.Equal(t, [2]int64{9_000, 9_000}, booksTotal(t, w, "general", "chargebacks"), "the loss is reversed")
		require.Zero(t, glBalance(t, w, "unapplied_cash"), "nothing is left unapplied")
		balance(t, a.alpha, customer, 14_339_000, 0, 14_339_000)
		var credited int64
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(units), 0) FROM "+booksTable(w, "credit_grants")+" WHERE merchant_id=$1 AND source='dispute_won_credit' AND source_id=$2",
			w.alpha.MerchantID().UUID(), recovery.String()).Scan(&credited))
		require.EqualValues(t, 14_321_000, credited)
		findings, err := a.alphaRemote.ListFindings(t.Context(), billing.FindingListParams{Type: "books.dispute.customer_credit"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		receivableAudited(t, w, false)
	})
	t.Run("OR-J40-S29/a-voluntary-refund-of-a-usage-payment-leaves-owed-settled", func(t *testing.T) {
		t.Run("refunds", func(t *testing.T) {
			a, customer, payment, _ := cardCollectedUsage(t)
			w := a.world
			refund, err := a.alpha.RefundPayment(t.Context(), "usage-goodwill", payment, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Goodwill"})
			require.NoError(t, err)
			require.Equal(t, billing.PaymentSucceeded, refund.Status)
			ctx, service := paymentService(t, w)
			_, err = service.RecordObservedRefund(ctx, payment.UUID(), "usage-observed-refund", 5_000_000, payments.ReversalRefund, w.clock.Now(), nil)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 0)
			require.Empty(t, glLegs(t, w, "usage_reopened"))
			require.Equal(t, [2]int64{0, 15_000_000}, booksTotal(t, w, "general", "refunds"), "the refunds are the usage's contra revenue")
			require.Zero(t, glBalance(t, w, "unapplied_cash"))
			require.Equal(t, billing.OrderComplete, oneUsageOrder(t, a, customer).Status)
			receivableAudited(t, w, false)
		})
		t.Run("refund-then-chargeback", func(t *testing.T) {
			// $5.00 of the $14.33 is refunded, then the network takes the
			// other $9.33: only the $9.321 of owed the refund left is owed
			// again; the change is a loss.
			a, customer, payment, _ := surplusCollectedUsage(t)
			w := a.world
			refund, err := a.alpha.RefundPayment(t.Context(), "surplus-goodwill", payment, billing.RefundPaymentParams{Amount: 5_000_000, Reason: "Goodwill"})
			require.NoError(t, err)
			require.Equal(t, billing.PaymentSucceeded, refund.Status)
			ctx, service := paymentService(t, w)
			_, err = service.Refund(ctx, payment.UUID(), "after-refund-chargeback", 9_330_000, payments.ReversalChargeback)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 9_321_000)
			require.Equal(t, []int64{9_321_000}, glLegs(t, w, "usage_reopened"))
			require.Equal(t, [2]int64{0, 5_000_000}, booksTotal(t, w, "general", "refunds"))
			require.Equal(t, [2]int64{0, 9_000}, booksTotal(t, w, "general", "chargebacks"))
			require.Zero(t, glBalance(t, w, "unapplied_cash"))
			receivableAudited(t, w, false)
		})
		t.Run("refund-chargeback-then-won", func(t *testing.T) {
			// The dispute after the refund is won: it settles the reopened
			// $9.321 again and gives back the $0.009 loss; nothing is left to
			// credit.
			a, customer, payment, _ := surplusCollectedUsage(t)
			w := a.world
			_, err := a.alpha.RefundPayment(t.Context(), "won-goodwill", payment, billing.RefundPaymentParams{Amount: 5_000_000, Reason: "Goodwill"})
			require.NoError(t, err)
			ctx, service := paymentService(t, w)
			chargeback, err := service.Refund(ctx, payment.UUID(), "won-chargeback", 9_330_000, payments.ReversalChargeback)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 9_321_000)
			win(t, w, service, payment, chargeback.ID, 9_330_000, "won-recovery")
			owedIsReceivable(t, a, customer, 0)
			require.Equal(t, [2]int64{9_000, 9_000}, booksTotal(t, w, "general", "chargebacks"))
			require.Equal(t, [2]int64{0, 5_000_000}, booksTotal(t, w, "general", "refunds"))
			require.Zero(t, glBalance(t, w, "unapplied_cash"))
			balance(t, a.alpha, customer, 9_000, 0, 9_000)
			findings, err := a.alphaRemote.ListFindings(t.Context(), billing.FindingListParams{Type: "books.dispute.customer_credit"})
			require.NoError(t, err)
			require.Empty(t, findings.Items, "the customer is owed no credit")
			receivableAudited(t, w, false)
		})
		t.Run("refund-reserved-then-chargeback-then-won", func(t *testing.T) {
			// A refund reserved before the chargeback settles after it: the
			// chargeback reopened what it reopened as it posted, so its won
			// dispute settles all of it.
			a, customer, payment, _ := surplusCollectedUsage(t)
			w := a.world
			ctx, service := paymentService(t, w)
			reserved, err := service.ReserveRefund(ctx, payment.UUID(), "reserved-refund", 5_000_000, billing.RefundRequestedByCustomer, nil)
			require.NoError(t, err)
			chargeback, err := service.Refund(ctx, payment.UUID(), "reserved-chargeback", 9_330_000, payments.ReversalChargeback)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 9_330_000)
			_, err = service.CompleteRefundReservation(ctx, reserved.ID, "reserved-refund-settled", nil)
			require.NoError(t, err)
			owedIsReceivable(t, a, customer, 9_330_000)
			win(t, w, service, payment, chargeback.ID, 9_330_000, "reserved-recovery")
			owedIsReceivable(t, a, customer, 0)
			require.Equal(t, [2]int64{0, 5_000_000}, booksTotal(t, w, "general", "refunds"))
			require.Zero(t, glBalance(t, w, "unapplied_cash"))
			receivableAudited(t, w, false)
		})
	})
}

// cardCollectedUsage is alice's $30.00 of usage beyond her balance, billed
// and collected by her saved card.
func cardCollectedUsage(t *testing.T) (*authorityWorld, billing.CustomerID, billing.PaymentID, *river.Client[pgx.Tx]) {
	t.Helper()
	a, _, customer := usageBillingWorld(t, false)
	oneUsage(t, a.alpha, usageInput(customer, "card-collected", 3))
	a.clock.Advance(time.Second)
	fleet := quietFleet(t, a.world)
	runUsageDue(t, fleet)
	require.Eventually(t, func() bool {
		rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
	}, 15*time.Second, 20*time.Millisecond)
	order := oneUsageOrder(t, a, customer)
	require.NotNil(t, order.Payment.PaymentID)
	return a, customer, *order.Payment.PaymentID, fleet
}

// surplusCollectedUsage is alice's $10.00 of usage and a running charge's
// 12 h 10 s at $0.36 an hour, $14.321, which her card pays as $14.33: the
// change is a credit lot of $0.009.
func surplusCollectedUsage(t *testing.T) (*authorityWorld, billing.CustomerID, billing.PaymentID, *river.Client[pgx.Tx]) {
	t.Helper()
	a, p, customer := usageBillingWorld(t, false)
	oneUsage(t, a.alpha, usageInput(customer, "surplus", 1))
	a.clock.Advance(time.Second)
	_, err := a.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "surplus", CustomerID: customer, Currency: "USD", RatePerHour: 360_000})
	require.NoError(t, err)
	fleet := quietFleet(t, a.world)
	a.clock.Advance(12*time.Hour + 10*time.Second)
	_, err = a.alpha.StopRunningCharge(t.Context(), "surplus", billing.StopRunningChargeParams{})
	require.NoError(t, err)
	runUsageDue(t, fleet)
	require.Eventually(t, func() bool { return oneUsageOrder(t, a, customer).Status == billing.OrderComplete }, 15*time.Second, 20*time.Millisecond)
	order := oneUsageOrder(t, a, customer)
	require.EqualValues(t, 14_321_000, order.Total)
	require.NotNil(t, order.Payment.PaymentID)
	p.money(t, 1, 1433)
	balance(t, a.alpha, customer, 9_000, 0, 9_000)
	return a, customer, *order.Payment.PaymentID, fleet
}

// openUsageOrder is alice's $30.00 of usage beyond her balance on an open
// usage order she pays by invoice.
func openUsageOrder(t *testing.T) (*authorityWorld, billing.CustomerID, billing.OrderID) {
	t.Helper()
	a, _, customer := usageBillingWorld(t, true)
	oneUsage(t, a.alpha, usageInput(customer, "invoiced", 3))
	fleet := quietFleet(t, a.world)
	postUsageMonth(t, a.world, fleet)
	runUsageDue(t, fleet)
	order := oneUsageOrder(t, a, customer)
	require.Equal(t, billing.OrderOpen, order.Status)
	return a, customer, order.ID
}

// remit records money the merchant received for a usage order.
func remit(t *testing.T, a *authorityWorld, order billing.OrderID, amount int64, key string) {
	t.Helper()
	_, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order, Amount: amount, TransactionID: key})
	require.NoError(t, err)
}

// owedIsReceivable checks that the customer owes amount in the units book
// and the general ledger holds as much receivable.
func owedIsReceivable(t *testing.T, a *authorityWorld, customer billing.CustomerID, amount int64) {
	t.Helper()
	funds, err := a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
	require.NoError(t, err)
	require.Equal(t, amount, funds.OwedAmount, "units owed")
	require.Equal(t, amount, -glBalance(t, a.world, "usage_receivable"), "general-ledger receivable")
}

// receivableAudited runs ledger-audit: the books are intact, warning of a
// receivable that differs from what is owed when drift is true.
func receivableAudited(t *testing.T, w *world, drift bool) {
	t.Helper()
	out := hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String())
	require.Contains(t, out, "Ledger integrity OK")
	require.Equal(t, drift, strings.Contains(out, "receivable: customer="), out)
}

// glLegs is the amounts of the general ledger's transfers of one type, in
// the order they posted.
func glLegs(t *testing.T, w *world, typ string) []int64 {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT amount FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='general' AND transfer_type=$2 ORDER BY created_at, id",
		w.alpha.MerchantID().UUID(), typ)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	return out
}

// collected is the amounts of the usage collections at one operation.
func collected(t *testing.T, w *world, op string) []int64 {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT amount FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='general' AND transfer_type='usage_collected' AND operation=$2 ORDER BY created_at, id",
		w.alpha.MerchantID().UUID(), op)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	return out
}

// win records a won dispute's recovery of amount of a chargeback, as the
// Stripe webhook records one.
func win(t *testing.T, w *world, service *payments.PaymentService, payment billing.PaymentID, chargeback uuid.UUID, amount int64, key string) {
	t.Helper()
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	original, err := service.GetByID(ctx, payment.UUID())
	require.NoError(t, err)
	kind := payments.ReversalDisputeReversal
	require.NoError(t, service.Create(ctx, &models.Payment{
		ID: uuid.New(), CustomerID: original.CustomerID, OrderID: original.OrderID, RefundedPaymentID: &original.ID, Rail: original.Rail, PspID: original.PspID,
		TransactionID: key, Metadata: map[string]any{"reverses_payment_id": chargeback.String()}, Amount: amount, ListAmount: original.ListAmount,
		Currency: original.Currency, ReversalKind: &kind, Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail,
		PurchasedAt: w.clock.Now(), CreatedAt: w.clock.Now(),
	}))
}

// paymentService is the payments service a provider's notices record
// reversals through, bound to the world's merchant.
func paymentService(t *testing.T, w *world) (context.Context, *payments.PaymentService) {
	t.Helper()
	return merchant.WithID(t.Context(), w.alpha.MerchantID()), payments.NewPaymentService(configuredDB(t, w), w.clock)
}
