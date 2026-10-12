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
			declareDynamicMeter(t, client, "remittance-proof", "units")
			const owed = int64(50_000_000)
			_, err := client.UpdateCustomer(ctx, payer.cid(), billing.UpdateCustomerParams{
				CreditLimits:   []billing.CreditLimit{{Currency: "USD", Amount: 2 * owed}},
				InvoiceProfile: catalog.Value(billing.InvoiceProfile{CollectionMethod: billing.CollectSendInvoice, NetTermsDays: 30}),
			})
			require.NoError(t, err)
			invoice := func() *billing.Order {
				_, err := recordUsage(ctx, client, billing.RecordUsageParams{Dimensions: map[string]int64{"units": 1}, CustomerID: payer.cid(), Invoker: payer.id, Currency: "USD", EventType: "remittance-proof", Amount: new(int64(owed)), Source: "test", SourceID: uuid.NewString()})
				require.NoError(t, err)
				w.advance(time.Minute)
				job, err := w.jobs.Insert(ctx, invoicePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
				w.waitJob(job.Job.ID)
				invoices, err := client.ListOrders(ctx, billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: payer.cid()})
				require.NoError(t, err)
				var selected *billing.Order
				for i := range invoices.Items {
					if invoices.Items[i].Usage.AmountDue > 0 {
						require.Nil(t, selected)
						selected = &invoices.Items[i]
					}
				}
				require.NotNil(t, selected)
				require.Equal(t, owed, selected.Usage.AmountDue)
				read, err := client.GetOrder(ctx, selected.ID)
				require.NoError(t, err)
				return read
			}
			first := invoice()
			amount := owed
			if partial {
				amount = 40_000_000
			}
			params := billing.CreatePaymentParams{OrderID: first.ID, Amount: amount, TransactionID: "bank-remittance"}
			type result struct {
				payment *billing.Payment
				err     error
			}
			results := make(chan result, 2)
			for range 2 {
				go func() {
					payment, err := client.CreatePayment(ctx, params)
					results <- result{payment, err}
				}()
			}
			accepted, concurrent := <-results, <-results
			require.NoError(t, accepted.err)
			require.NoError(t, concurrent.err)
			require.Equal(t, accepted.payment.ID, concurrent.payment.ID, "concurrent first submissions share one payment")
			receipt := accepted.payment
			require.Equal(t, billing.ChannelManual, receipt.Channel)
			require.Equal(t, &first.ID, receipt.OrderID)
			require.Equal(t, amount, receipt.Amount)
			require.Equal(t, billing.PaymentSucceeded, receipt.Status)
			paid, err := client.GetOrder(ctx, first.ID)
			require.NoError(t, err)
			require.Equal(t, owed-amount, paid.Usage.AmountDue)
			for range 2 {
				replay, err := client.CreatePayment(ctx, params)
				require.NoError(t, err, "replay survives reduced amount_due and paid status")
				require.Equal(t, receipt.ID, replay.ID)
			}
			changed := params
			changed.Amount++
			_, err = client.CreatePayment(ctx, changed)
			requirePaymentRefusal(t, err, 422, billing.CodeIdempotencyKeyReused)
			if partial {
				_, err = client.CreatePayment(ctx, billing.CreatePaymentParams{OrderID: first.ID, Amount: owed - amount, TransactionID: "remainder"})
				require.NoError(t, err)
				replay, err := client.CreatePayment(ctx, params)
				require.NoError(t, err)
				require.Equal(t, receipt.ID, replay.ID, "an older partial payment answers on the fully paid invoice")
				paid, err = client.GetOrder(ctx, first.ID)
				require.NoError(t, err)
			}
			require.Equal(t, billing.OrderComplete, paid.Status)
			_, err = client.CreatePayment(ctx, billing.CreatePaymentParams{OrderID: first.ID, Amount: 1, TransactionID: "new-payment-after-paid"})
			requirePaymentRefusal(t, err, 409, billing.CodeOrderActionNotAllowed)
			payments, err := client.ListPayments(ctx, billing.PaymentListParams{OrderID: first.ID})
			require.NoError(t, err)
			want := 1
			if partial {
				want = 2
			}
			require.Len(t, payments.Items, want)
			// The same remittance must not settle another invoice.
			next := invoice()
			other := params
			other.OrderID = next.ID
			_, err = client.CreatePayment(ctx, other)
			requirePaymentRefusal(t, err, 422, billing.CodeIdempotencyKeyReused)
			nextAfter, err := client.GetOrder(ctx, next.ID)
			require.NoError(t, err)
			require.Equal(t, next, nextAfter)
			nextPayments, err := client.ListPayments(ctx, billing.PaymentListParams{OrderID: next.ID})
			require.NoError(t, err)
			require.Empty(t, nextPayments.Items)
			// A recorded payment is not a replacement for live merchant authorization.
			sid := uuid.NewString()
			token := w.auth.sessionToken(t, "staff", sid)
			revoked, err := openrails.NewRemoteMerchant(w.server.URL+mountPrefix, openrails.RemoteCredential{Token: func(context.Context) (string, error) { return token, nil }})
			require.NoError(t, err)
			w.auth.revoked.Store(sid, struct{}{})
			_, err = revoked.CreatePayment(ctx, params)
			var refusal *billing.StatusError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, 401, refusal.Status)
			stranger, err := w.rt.Merchant(billing.MerchantID(uuid.New()))
			require.NoError(t, err)
			_, err = stranger.CreatePayment(ctx, params)
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

func requirePaymentRefusal(t *testing.T, err error, status int, code string) {
	t.Helper()
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, status, refusal.Status)
	require.Equal(t, code, refusal.Code)
}
