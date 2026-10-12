//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Each owed charge is billed once: threshold orders bill the period so far,
// the period sweep the remainder, and a period with nothing owed bills no
// order. Each order is paid by whatever reaches it, partly or whole: a
// remittance, a funded balance, the customer's card.
func TestUsageOrdersBillEachChargeOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// Only this scenario's passes run: a scheduled one could bill the
	// remainder below before the period closes.
	w.settleRefreshes()
	w.jobs.PeriodicJobs().Clear()
	ctx, client := t.Context(), w.client[remote]
	declareDynamicMeter(t, client, "usage-proof", "units")
	const threshold = int64(50_000_000) // the default collection threshold
	const tail = int64(10_000_000)

	// Periods are fixed thirty-day windows: begin an hour into the next one.
	interval := 30 * 24 * time.Hour
	current, err := money.CurrentBillingPeriodStart(w.clock.Now(), time.Time{}, money.BillingBoundaryFixedInterval)
	require.NoError(t, err)
	start := current.Add(interval)
	end := start.Add(interval)
	w.advance(start.Add(time.Hour).Sub(w.clock.Now()))

	payer := func(arrears bool) *customer {
		c := w.newCustomer()
		if arrears {
			_, err := client.UpdateCustomer(ctx, c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 4 * threshold}}})
			require.NoError(t, err)
		}
		return c
	}
	use := func(c *customer, amount int64) {
		_, err := recordUsage(ctx, client, billing.RecordUsageParams{Dimensions: map[string]int64{"units": 1}, CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "usage-proof", Amount: new(int64(amount)), Source: "test", SourceID: uuid.NewString()})
		require.NoError(t, err)
	}
	pass := func(args river.JobArgs) {
		w.advance(time.Minute)
		job, err := w.jobs.Insert(ctx, args, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		w.waitJob(job.Job.ID)
	}
	order := func(id billing.OrderID) *billing.Order {
		o, err := client.GetOrder(ctx, id)
		require.NoError(t, err)
		return o
	}
	// orders are the customer's usage orders, oldest first.
	orders := func(c *customer) []*billing.Order {
		page, err := client.ListOrders(ctx, billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c.cid()})
		require.NoError(t, err)
		var out []*billing.Order
		for i := len(page.Items) - 1; i >= 0; i-- {
			o := page.Items[i]
			require.True(t, o.Usage.PeriodStartsAt.Equal(start), "%+v", o)
			out = append(out, order(o.ID))
		}
		return out
	}
	pay := func(id billing.OrderID, amount int64) {
		_, err := client.CreatePayment(ctx, billing.CreatePaymentParams{OrderID: id, Amount: amount, TransactionID: "remit-" + uuid.NewString()})
		require.NoError(t, err)
	}
	requireOrder := func(o *billing.Order, status billing.OrderStatus, total, paid int64) {
		t.Helper()
		o = order(o.ID)
		require.Equal(t, []any{status, total, paid, total - paid}, []any{o.Status, o.Total, o.Usage.AmountPaid, o.Usage.AmountDue})
		require.Equal(t, status == billing.OrderComplete, o.Number != nil, "numbered once paid")
	}

	// covered: one threshold order bills the whole period.
	// split: two threshold orders, then usage below the threshold that only
	// the period's order bills.
	// plain: arrears usage below the threshold; prepaid: usage paid from
	// credit as it happens.
	covered, split, plain, prepaid := payer(true), payer(true), payer(true), payer(false)
	_, err = createCreditGrant(ctx, client, prepaid.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 2 * tail, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	use(covered, threshold)
	use(split, threshold)
	pass(invoicePass{})
	use(split, threshold)
	pass(invoicePass{})
	use(split, tail)
	use(plain, tail)
	use(prepaid, tail)

	w.advance(end.Add(time.Hour).Sub(w.clock.Now()))
	pass(openrails.UsageSweepArgs{FinalizePreviousMonth: true})

	coveredBy := orders(covered)
	require.Len(t, coveredBy, 1, "the threshold order billed the whole period: the sweep bills nothing more")
	require.True(t, coveredBy[0].Usage.PeriodEndsAt.Before(end))
	splitBy := orders(split)
	require.Len(t, splitBy, 3)
	require.True(t, splitBy[2].Usage.PeriodEndsAt.Equal(end), "the period's order bills the remainder")
	plainBy := orders(plain)
	require.Len(t, plainBy, 1)
	require.True(t, plainBy[0].Usage.PeriodEndsAt.Equal(end))
	require.Empty(t, orders(prepaid), "nothing owed bills no order")

	requireOrder(coveredBy[0], billing.OrderOpen, threshold, 0)
	requireOrder(splitBy[0], billing.OrderOpen, threshold, 0)
	requireOrder(splitBy[1], billing.OrderOpen, threshold, 0)
	requireOrder(splitBy[2], billing.OrderOpen, tail, 0)
	requireOrder(plainBy[0], billing.OrderOpen, tail, 0)

	// A remittance pays part of an order, then the rest.
	pay(coveredBy[0].ID, 20_000_000)
	requireOrder(coveredBy[0], billing.OrderOpen, threshold, 20_000_000)
	pay(coveredBy[0].ID, threshold-20_000_000)
	requireOrder(coveredBy[0], billing.OrderComplete, threshold, threshold)

	// Funding repays the oldest order; a card pays the next; a remittance the
	// last.
	_, err = createCreditGrant(ctx, client, split.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: threshold, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	requireOrder(splitBy[0], billing.OrderComplete, threshold, threshold)
	requireOrder(splitBy[1], billing.OrderOpen, threshold, 0)
	w.refreshProviders()
	w.settleCollectionScans()
	card := split.saveCard("nmi", visa)
	split.must(http.MethodPost, "/orders/"+splitBy[1].ID.String()+"/pay", "pay-"+uuid.NewString(), map[string]any{
		"payment": map[string]any{"payment_method_id": card}, "expected_total": "50000000"})
	requireOrder(splitBy[1], billing.OrderComplete, threshold, threshold)
	pay(splitBy[2].ID, tail)
	requireOrder(splitBy[2], billing.OrderComplete, tail, tail)
	pay(plainBy[0].ID, tail)
	requireOrder(plainBy[0], billing.OrderComplete, tail, tail)

	// What each order says was paid is what it received, as payments or
	// repayments from funding, and the ledger holds no debt for any of them.
	for c, billed := range map[*customer][]*billing.Order{covered: coveredBy, split: splitBy, plain: plainBy} {
		for _, o := range billed {
			payments, err := client.ListPayments(ctx, billing.PaymentListParams{OrderID: o.ID})
			require.NoError(t, err)
			var received int64
			for _, p := range payments.Items {
				if p.Status == billing.PaymentSucceeded {
					received += p.Amount
				}
			}
			var repaid int64
			require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT COALESCE(sum(amount), 0) FROM billing.ledger_transfers WHERE order_id = $1 AND transfer_type = 'owed_repayment'`), o.ID.UUID()).Scan(&repaid))
			require.Equal(t, order(o.ID).Usage.AmountPaid, received+repaid)
		}
		balance, err := client.GetBalance(ctx, c.cid(), "USD")
		require.NoError(t, err)
		require.Zero(t, balance.OwedAmount)
	}
}

// A meter's rate card prices every customer's usage: two customers metered on
// one card are each billed their own quantity at the card's price.
func TestEveryCustomerIsRatedByTheMeterRateCard(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.settleRefreshes()
	w.jobs.PeriodicJobs().Clear()
	ctx, client := t.Context(), w.client[remote]
	const unit = int64(2_000_000)
	key := "calls-" + uuid.NewString()[:6]
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "api-" + uuid.NewString()[:8], DisplayName: "API"})
	require.NoError(t, err)
	_, err = client.SetMeter(ctx, key, billing.SetMeterParams{Aggregation: catalog.AggregationCount, RateCard: catalog.Value(billing.MeterRateCardParams{
		ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: unit}}})})
	require.NoError(t, err)

	interval := 30 * 24 * time.Hour
	current, err := money.CurrentBillingPeriodStart(w.clock.Now(), time.Time{}, money.BillingBoundaryFixedInterval)
	require.NoError(t, err)
	start := current.Add(interval)
	end := start.Add(interval)
	w.advance(start.Add(time.Hour).Sub(w.clock.Now()))

	calls := map[*customer]int64{w.newCustomer(): 3, w.newCustomer(): 5}
	for c, n := range calls {
		_, err := client.UpdateCustomer(ctx, c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 50_000_000}}})
		require.NoError(t, err)
		for range n {
			_, err := recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: key, Source: "test", SourceID: uuid.NewString()})
			require.NoError(t, err)
		}
	}
	w.advance(end.Add(time.Hour).Sub(w.clock.Now()))
	job, err := w.jobs.Insert(ctx, openrails.UsageSweepArgs{FinalizePreviousMonth: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)

	for c, n := range calls {
		page, err := client.ListOrders(ctx, billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c.cid()})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, n*unit, page.Items[0].Total, "%d calls at the card's price", n)
	}
}
