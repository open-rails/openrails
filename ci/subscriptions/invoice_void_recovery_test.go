//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestNMIVoidCannotCompleteRecoveryWithUnresolvedInvoiceMoney(t *testing.T) {
	t.Parallel()
	for _, recorded := range []bool{true, false} {
		name := "retained_paid_invoice"
		if !recorded {
			name = "missing_invoice_allocation"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			method := c.saveCard("nmi", visa)
			invoice := observedInvoice(t, w, c, 50_000_000)
			w.refreshProviders()
			w.settleCollectionScans()
			mid, psp := w.client[embedded].MerchantID(), w.psp["nmi"]
			var completedBefore time.Time
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT watermark_at FROM billing.psp_refresh_watermarks WHERE merchant_id=$1 AND psp_id=$2 AND event_domain='completed_events'`), mid.UUID(), psp.UUID()).Scan(&completedBefore))
			refreshHeld := func() {
				t.Helper()
				// Ordinary worker, intentionally incomplete financial observation.
				job, err := w.jobs.Insert(t.Context(), refreshMerchant{MerchantID: mid.UUID()}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					row, err := w.jobs.JobGet(t.Context(), job.Job.ID)
					return err == nil && row.State == rivertype.JobStateRetryable
				}, 20*time.Second, 20*time.Millisecond, "unresolved money prevents refresh completion")
			}
			var transaction string
			if recorded {
				answer := payNMIInvoice(t.Context(), w, c, invoice, method, "before-void")
				require.NoError(t, answer.err)
				require.Equal(t, http.StatusOK, answer.status, string(answer.body))
				transaction = w.nmi.ledger("")[0].ID
			} else {
				sale := w.nmi.AddSale(nmimock.Sale{OrderID: uuid.NewString(), OrderDescription: "invoice " + uuid.NewString(), Vault: w.vaultOf(method), Amount: "50.00"})
				transaction = sale.TransactionID
				w.advance(10 * time.Minute)
				refreshHeld()
				var status string
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.reconciliation_findings WHERE merchant_id=$1 AND psp_id=$2 AND finding_type='pull.charge.missing' AND subject_key=$3`), mid.UUID(), psp.UUID(), transaction).Scan(&status))
				require.Equal(t, "requires_review", status)
			}
			if recorded {
				w.waive("evidenced", "the provider voids a recorded invoice payment; the reversal stays held for review")
			}
			w.nmi.Void(transaction) // dashboard action at the local provider fixture
			w.advance(10 * time.Minute)
			refreshHeld()
			var status string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.reconciliation_findings WHERE merchant_id=$1 AND psp_id=$2 AND finding_type='pull.reversal.unlinked' AND subject_key=$3`), mid.UUID(), psp.UUID(), transaction).Scan(&status))
			require.Equal(t, "requires_review", status)
			if !recorded {
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.reconciliation_findings WHERE merchant_id=$1 AND psp_id=$2 AND finding_type='pull.charge.missing' AND subject_key=$3`), mid.UUID(), psp.UUID(), transaction).Scan(&status))
				require.Equal(t, "fixed", status, "old charge finding closes only while the reversal remains held")
			}
			var completedAfter time.Time
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT watermark_at FROM billing.psp_refresh_watermarks WHERE merchant_id=$1 AND psp_id=$2 AND event_domain='completed_events'`), mid.UUID(), psp.UUID()).Scan(&completedAfter))
			require.True(t, completedBefore.Equal(completedAfter), "void did not advance financial completion")
			ctx := merchant.WithID(t.Context(), mid)
			require.ErrorIs(t, providerrecovery.CheckPSP(ctx, engine.Graph(w.rt).Runtime.DB, mid.UUID(), psp.UUID(), w.clock.Now()), providerrecovery.ErrPending)
			current, err := w.client[remote].GetInvoice(t.Context(), invoice)
			require.NoError(t, err)
			if recorded {
				require.Equal(t, billing.InvoicePaid, current.Status, "review did not invent an accounting reversal")
			} else {
				require.Equal(t, int64(50_000_000), current.AmountDue, "void granted no payment or credit")
			}
			var refunds int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE amount<0 OR refunded_payment_id IS NOT NULL`)).Scan(&refunds))
			require.Zero(t, refunds)
			want := 0
			if recorded {
				want = 1
			}
			require.Len(t, w.nmi.Attempts(), want, "observation made no provider charge")
		})
	}
}
