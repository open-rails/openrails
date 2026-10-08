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

// The monthly pass invoices payers active in the period, not every payer on
// file: a payer last active six months ago, or first active after the period
// closed, gets no statement.
func TestMonthlyInvoicesCoverActivePayersOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[embedded]
	active, dormant, newcomer := w.newCustomer(), w.newCustomer(), w.newCustomer()
	run := w.clock.Now().Add(31 * 24 * time.Hour)
	from, to, err := money.PreviousInvoicePeriod(run, run, money.InvoiceBoundaryFixedInterval)
	require.NoError(t, err)
	activity := from.Add(time.Hour)
	for c, at := range map[*customer]time.Time{active: activity, dormant: from.AddDate(0, -6, 0), newcomer: to.Add(time.Hour)} {
		_, err := client.CreateCreditGrant(ctx, c.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 5_000_000, Source: "e2e", SourceID: uuid.NewString()})
		require.NoError(t, err)
		// Ledger rows take the database's clock; move them onto the world's.
		tx, err := w.pool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica")
		require.NoError(t, err)
		_, err = tx.Exec(ctx, w.q(`UPDATE billing.ledger_transfers SET created_at = $2 WHERE customer_id = $1`), c.cid().UUID(), at)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
	}

	w.advanceTo(run)
	res, err := w.jobs.Insert(ctx, monthlyInvoicePass{FinalizePreviousMonth: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(res.Job.ID)

	invoices, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: active.cid()})
	require.NoError(t, err)
	require.Len(t, invoices.Items, 1)
	inv := invoices.Items[0]
	require.True(t, inv.PeriodStartsAt.Equal(from) && inv.PeriodEndsAt.Equal(to), "%v-%v", inv.PeriodStartsAt, inv.PeriodEndsAt)
	require.Equal(t, int64(5_000_000), inv.DepositsTotal)
	for _, c := range []*customer{dormant, newcomer} {
		invoices, err = client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: c.cid()})
		require.NoError(t, err)
		require.Empty(t, invoices.Items)
	}
}
