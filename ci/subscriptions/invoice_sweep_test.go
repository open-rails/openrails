//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/money"
)

// monthlyInvoicePass is the monthly pass: last period's statements.
type monthlyInvoicePass struct {
	FinalizePreviousMonth bool `json:"finalize_previous_month"`
}

func (monthlyInvoicePass) Kind() string { return "openrails.invoice" }

// The monthly pass bills payers active in the period, not every payer on
// file: a payer last active six months ago, or first active after the period
// closed, gets no usage order.
func TestMonthlyInvoicesCoverActivePayersOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[embedded]
	active, dormant, newcomer := w.newCustomer(), w.newCustomer(), w.newCustomer()
	run := w.clock.Now().Add(31 * 24 * time.Hour)
	from, to, err := money.PreviousBillingPeriod(run, run, money.BillingBoundaryFixedInterval)
	require.NoError(t, err)
	activity := from.Add(time.Hour)
	for c, at := range map[*customer]time.Time{active: activity, dormant: from.AddDate(0, -6, 0), newcomer: to.Add(time.Hour)} {
		_, err := client.UpdateCustomer(ctx, c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 10_000_000}}})
		require.NoError(t, err)
		_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "sweep", Amount: 5_000_000, Source: "e2e", SourceID: uuid.NewString()})
		require.NoError(t, err)
		// The owed accrual takes the database's clock; move it onto the world's.
		tx, err := w.pool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica")
		require.NoError(t, err)
		_, err = tx.Exec(ctx, w.q(`UPDATE billing.ledger_transfers SET created_at = $2 WHERE customer_id = $1`), c.cid().UUID(), at)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, w.q(`UPDATE billing.owed_items SET accrued_at = $2 WHERE customer_id = $1`), c.cid().UUID(), at)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
	}

	w.advanceTo(run)
	res, err := w.jobs.Insert(ctx, monthlyInvoicePass{FinalizePreviousMonth: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(res.Job.ID)

	invoices, err := client.ListOrders(ctx, billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: active.cid()})
	require.NoError(t, err)
	require.Len(t, invoices.Items, 1)
	inv := invoices.Items[0]
	require.True(t, inv.Usage.PeriodStartsAt.Equal(from) && inv.Usage.PeriodEndsAt.Equal(to), "%v-%v", inv.Usage.PeriodStartsAt, inv.Usage.PeriodEndsAt)
	require.Equal(t, int64(5_000_000), inv.Total)
	for _, c := range []*customer{dormant, newcomer} {
		invoices, err = client.ListOrders(ctx, billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c.cid()})
		require.NoError(t, err)
		require.Empty(t, invoices.Items)
	}
}
