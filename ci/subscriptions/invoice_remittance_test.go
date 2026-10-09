//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Usage and the ordinary invoice worker create the debt. Every remittance is
// submitted through the public remote Client; no financial row is inserted by
// this test and no payment provider is needed.
func TestInvoiceRemittanceReplay(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "full"
		if partial {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			ctx, client, payer := t.Context(), w.client[remote], w.newCustomer()
			const owed = int64(50_000_000)
			_, err := client.UpdateCustomer(ctx, payer.cid(), billing.UpdateCustomerParams{
				CreditLimits:   []billing.CreditLimit{{Currency: "USD", Amount: 2 * owed}},
				InvoiceProfile: catalog.Value(billing.InvoiceProfile{CollectionMethod: billing.CollectSendInvoice, NetTermsDays: 30}),
			})
			require.NoError(t, err)
			invoice := func() *billing.Invoice {
				_, err := recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: payer.cid(), Invoker: payer.id, Currency: "USD", EventType: "remittance-proof", Amount: owed, Source: "test", SourceID: uuid.NewString()})
				require.NoError(t, err)
				w.advance(time.Minute)
				job, err := w.jobs.Insert(ctx, invoicePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
				w.waitJob(job.Job.ID)
				invoices, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: payer.cid()})
				require.NoError(t, err)
				var selected *billing.Invoice
				for i := range invoices.Items {
					if invoices.Items[i].AmountDue > 0 {
						require.Nil(t, selected)
						selected = &invoices.Items[i]
					}
				}
				require.NotNil(t, selected)
				require.Equal(t, owed, selected.AmountDue)
				read, err := client.GetInvoice(ctx, selected.ID)
				require.NoError(t, err)
				return read
			}
			first := invoice()
			amount := owed
			if partial {
				amount = 40_000_000
			}
			params := billing.CreateInvoicePaymentParams{Amount: amount, Reference: "bank-remittance"}
			type result struct {
				invoice *billing.Invoice
				err     error
			}
			results := make(chan result, 2)
			for range 2 {
				go func() {
					invoice, err := client.CreateInvoicePayment(ctx, first.ID, params)
					results <- result{invoice, err}
				}()
			}
			accepted, concurrent := <-results, <-results
			require.NoError(t, accepted.err)
			require.NoError(t, concurrent.err)
			require.Equal(t, accepted.invoice, concurrent.invoice, "concurrent first submissions share one receipt")
			paid := accepted.invoice
			require.Equal(t, owed-amount, paid.AmountDue)
			payments, err := client.ListInvoicePayments(ctx, first.ID, billing.InvoicePaymentListParams{})
			require.NoError(t, err)
			require.Len(t, payments.Items, 1)
			receipt := payments.Items[0]
			for range 2 {
				replay, err := client.CreateInvoicePayment(ctx, first.ID, params)
				require.NoError(t, err, "replay survives reduced amount_due and paid status")
				require.Equal(t, paid, replay)
			}
			_, err = client.CreateInvoicePayment(ctx, first.ID, billing.CreateInvoicePaymentParams{Amount: amount + 1, Reference: params.Reference})
			requireInvoiceRefusal(t, err, billing.CodeInvoicePaymentReferenceUsed)
			if partial {
				paid, err = client.CreateInvoicePayment(ctx, first.ID, billing.CreateInvoicePaymentParams{Amount: owed - amount, Reference: "remainder"})
				require.NoError(t, err)
				replay, err := client.CreateInvoicePayment(ctx, first.ID, params)
				require.NoError(t, err)
				require.Equal(t, paid, replay, "an older partial receipt answers the current fully paid invoice")
			}
			require.Equal(t, billing.InvoicePaid, paid.Status)
			_, err = client.CreateInvoicePayment(ctx, first.ID, billing.CreateInvoicePaymentParams{Amount: 1, Reference: "new-payment-after-paid"})
			requireInvoiceRefusal(t, err, billing.CodeInvoiceActionNotAllowed)
			payments, err = client.ListInvoicePayments(ctx, first.ID, billing.InvoicePaymentListParams{})
			require.NoError(t, err)
			want := 1
			if partial {
				want = 2
			}
			require.Len(t, payments.Items, want)
			require.Contains(t, payments.Items, receipt, "the original receipt remains unchanged")
			// The same coordinate must not settle another invoice for this payer.
			next := invoice()
			_, err = client.CreateInvoicePayment(ctx, next.ID, params)
			requireInvoiceRefusal(t, err, billing.CodeInvoicePaymentReferenceUsed)
			nextAfter, err := client.GetInvoice(ctx, next.ID)
			require.NoError(t, err)
			require.Equal(t, next, nextAfter)
			nextPayments, err := client.ListInvoicePayments(ctx, next.ID, billing.InvoicePaymentListParams{})
			require.NoError(t, err)
			require.Empty(t, nextPayments.Items)
			// A receipt is not a replacement for live merchant authorization.
			sid := uuid.NewString()
			token := w.auth.sessionToken(t, "staff", sid)
			revoked, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(w.slug), openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
			require.NoError(t, err)
			w.auth.revoked.Store(sid, struct{}{})
			_, err = revoked.CreateInvoicePayment(ctx, first.ID, params)
			var refusal *billing.StatusError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, 401, refusal.Status)
			_, err = client.CreateInvoicePayment(ctx, first.ID, params, openrails.ForMerchantID(billing.MerchantID(uuid.New())))
			require.Error(t, err, "the original invoice is not accessible in another book")
			// Ledger counts and sums prove no replay/mismatch moved money again.
			var transfers int
			var settled int64
			require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*),COALESCE(sum(amount),0) FROM billing.ledger_transfers WHERE customer_id=$1 AND operation='manual_invoice_payment'`), payer.cid().UUID()).Scan(&transfers, &settled))
			require.Equal(t, want, transfers)
			require.Equal(t, owed, settled)
			require.Empty(t, w.attempts(payer.id), "recording external remittances creates no provider attempt")
		})
	}
}

func requireInvoiceRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, 409, refusal.Status)
	require.Equal(t, code, refusal.Code)
}
