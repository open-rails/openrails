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
	"github.com/open-rails/openrails/internal/modules/money"
)

// A period statement is paid only once the invoices that bill its usage are:
// threshold invoices billed mid-period, and its own remainder. It follows
// each payment as it lands, whichever way the money arrives (#1147).
func TestPeriodStatementFollowsItsInvoices(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// Only this scenario's passes run: a scheduled one could bill the
	// remainder below before the period closes.
	w.settleRefreshes()
	w.jobs.PeriodicJobs().Clear()
	ctx, client := t.Context(), w.client[remote]
	const threshold = int64(50_000_000) // the default invoice threshold
	const tail = int64(10_000_000)

	// Periods are fixed thirty-day windows: begin an hour into the next one.
	interval := 30 * 24 * time.Hour
	current, err := money.CurrentInvoicePeriodStart(w.clock.Now(), time.Time{}, money.InvoiceBoundaryFixedInterval)
	require.NoError(t, err)
	start := current.Add(interval)
	end := start.Add(interval)
	w.advance(start.Add(time.Hour).Sub(w.clock.Now()))

	payer := func(arrears bool) *customer {
		c := w.newCustomer()
		if arrears {
			_, err := client.UpdateCustomerSettings(ctx, []billing.UpdateCustomerSettingsParams{{CustomerID: c.cid(), CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 4 * threshold}}}})
			require.NoError(t, err)
		}
		return c
	}
	use := func(c *customer, amount int64) {
		_, err := recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "statement-proof", Amount: amount, Source: "test", SourceID: uuid.NewString()})
		require.NoError(t, err)
	}
	pass := func(args river.JobArgs) {
		w.advance(time.Minute)
		job, err := w.jobs.Insert(ctx, args, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		w.waitJob(job.Job.ID)
	}
	invoice := func(id billing.InvoiceID) *billing.Invoice {
		inv, err := client.GetInvoice(ctx, id)
		require.NoError(t, err)
		return inv
	}
	// invoices are the customer's threshold invoices, oldest first, and the
	// statement of [start, end) once it exists.
	invoices := func(c *customer) ([]billing.InvoiceID, *billing.Invoice) {
		page, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: c.cid()})
		require.NoError(t, err)
		var billed []billing.InvoiceID
		var statement *billing.Invoice
		for i := len(page.Items) - 1; i >= 0; i-- {
			inv := page.Items[i]
			require.True(t, inv.PeriodStartsAt.Equal(start), "%+v", inv)
			if inv.PeriodEndsAt.Equal(end) {
				statement = invoice(inv.ID)
				continue
			}
			billed = append(billed, inv.ID)
		}
		return billed, statement
	}
	pay := func(id billing.InvoiceID, amount int64) {
		_, err := client.CreateInvoicePayment(ctx, id, billing.CreateInvoicePaymentParams{Amount: amount, Reference: "remit-" + uuid.NewString()})
		require.NoError(t, err)
	}
	requireStatement := func(c *customer, status billing.InvoiceStatus, total, paid, due int64) *billing.Invoice {
		t.Helper()
		_, s := invoices(c)
		require.NotNil(t, s)
		require.Equal(t, []any{status, total, paid, due}, []any{s.Status, s.TotalAmount, s.AmountPaid, s.AmountDue})
		require.Equal(t, status == billing.InvoicePaid, s.PaidAt != nil)
		return s
	}

	// covered: one threshold invoice bills the whole period.
	// split: two threshold invoices, then usage below the threshold that only
	// the statement bills.
	// plain: arrears usage below the threshold; prepaid: usage paid from
	// credit as it happens. Neither has a threshold invoice.
	covered, split, plain, prepaid := payer(true), payer(true), payer(true), payer(false)
	_, err = client.CreateCreditGrant(ctx, prepaid.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 2 * tail, Source: "test", SourceID: uuid.NewString()})
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
	pass(openrails.InvoiceSweepArgs{FinalizePreviousMonth: true})

	coveredBy, _ := invoices(covered)
	require.Len(t, coveredBy, 1)
	splitBy, _ := invoices(split)
	require.Len(t, splitBy, 2)
	for _, id := range append(coveredBy, splitBy...) {
		require.NotEqual(t, billing.InvoicePaid, invoice(id).Status)
	}
	plainBy, _ := invoices(plain)
	require.Empty(t, plainBy)

	// Nothing arrived yet: no statement claims money.
	requireStatement(covered, billing.InvoiceOpen, threshold, 0, 0)
	statement := requireStatement(split, billing.InvoiceOpen, 2*threshold+tail, 0, tail)
	requireStatement(plain, billing.InvoiceOpen, tail, 0, tail)
	requireStatement(prepaid, billing.InvoicePaid, tail, tail, 0)

	// A partly paid threshold invoice is partly paid usage.
	pay(coveredBy[0], 20_000_000)
	requireStatement(covered, billing.InvoiceOpen, threshold, 20_000_000, 0)
	pay(coveredBy[0], threshold-20_000_000)
	require.Equal(t, billing.InvoicePaid, invoice(coveredBy[0]).Status)
	requireStatement(covered, billing.InvoicePaid, threshold, threshold, 0)

	// Paying the statement's own remainder leaves it waiting on its
	// threshold invoices; funding repays the oldest; a card pays the last.
	pay(statement.ID, tail)
	requireStatement(split, billing.InvoiceOpen, 2*threshold+tail, tail, 0)
	_, err = client.CreateCreditGrant(ctx, split.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: threshold, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	require.Equal(t, billing.InvoicePaid, invoice(splitBy[0]).Status)
	requireStatement(split, billing.InvoiceOpen, 2*threshold+tail, threshold+tail, 0)
	w.refreshProviders()
	w.settleCollectionScans()
	card := split.saveCard("nmi", visa)
	split.must(http.MethodPost, "/invoices/"+splitBy[1].String()+"/pay-now", "pay-"+uuid.NewString(), map[string]any{"payment_method_id": card})
	require.Equal(t, billing.InvoicePaid, invoice(splitBy[1]).Status)
	requireStatement(split, billing.InvoicePaid, 2*threshold+tail, 2*threshold+tail, 0)

	// Without a threshold invoice nothing changes: the statement is the bill.
	pay(requireStatement(plain, billing.InvoiceOpen, tail, 0, tail).ID, tail)
	requireStatement(plain, billing.InvoicePaid, tail, tail, 0)

	// What each statement says was paid is what its invoices received, and
	// the ledger holds no debt for any of them.
	for c, ids := range map[*customer][]billing.InvoiceID{covered: coveredBy, split: append(splitBy, statement.ID), plain: nil} {
		_, s := invoices(c)
		if c == plain {
			ids = []billing.InvoiceID{s.ID}
		}
		var received int64
		for _, id := range ids {
			payments, err := client.ListInvoicePayments(ctx, id, billing.PageRequest{})
			require.NoError(t, err)
			for _, p := range payments.Items {
				if p.Status == billing.InvoicePaymentSettled {
					received += p.Amount
				}
			}
		}
		require.Equal(t, s.AmountPaid, received)
		balance, err := client.GetBalance(ctx, c.cid(), "USD")
		require.NoError(t, err)
		require.Zero(t, balance.OwedAmount)
	}
}
