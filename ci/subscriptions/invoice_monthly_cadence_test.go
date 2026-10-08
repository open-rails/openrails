//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// A second replica cannot turn an overlapping retry into a second monthly
// batch after the first scan succeeds, even when a new invoice arrives meanwhile.
func TestInvoiceMonthlyCadenceSerializesReplicas(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	a, b := f.replicas[0], f.replicas[1]
	c := a.newCustomer()
	method := c.saveCard("nmi", visa)
	first := newNMIInvoice(f, c)
	answer := payNMIInvoice(t.Context(), a, c, first, method, "monthly-agreement")
	require.NoError(t, answer.err)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, answer.status)
	f.settle()
	c.must(http.MethodPut, "/collection-payment-method", "", map[string]any{"currency": "USD", "payment_method_id": method})
	small := func(on ...*world) billing.InvoiceID {
		id := newNMIInvoice(f, c, on...)
		_, err := a.client[remote].CreateInvoicePayment(t.Context(), id, billing.CreateInvoicePaymentParams{Amount: 90_000_000, Reference: "partial-" + id.String()})
		require.NoError(t, err)
		return id
	}
	invoice := small()
	hold := f.hold("nmi", submission("nmi"), false)
	start := func(r *world) int64 {
		job, err := r.jobs.Insert(t.Context(), riverjobs.InvoiceArgs{Collect: true, UseMonthlyFloor: true}, &river.InsertOpts{Queue: r.replica.queue})
		require.NoError(t, err)
		return job.Job.ID
	}
	firstPass := start(a)
	hold.wait()
	secondPass := start(b)
	require.Eventually(t, func() bool {
		job, err := b.jobs.JobGet(t.Context(), secondPass)
		return err == nil && job.State == rivertype.JobStateRetryable
	}, 10*time.Second, 20*time.Millisecond, "the other replica retains retryable work while the monthly scan owns its session lock")
	var completed int
	require.NoError(t, a.pool.QueryRow(t.Context(), a.q(`SELECT count(*) FROM billing.invoice_collection_cadence`)).Scan(&completed))
	require.Zero(t, completed, "an unfinished scan cannot advance the monthly watermark")
	later := small(b) // A's single-worker queue is intentionally paused at the provider.
	hold.release()
	a.waitJob(firstPass)
	_, err := b.jobs.JobRetry(t.Context(), secondPass)
	require.NoError(t, err)
	b.waitJob(secondPass)
	requireInvoicePaidOnce(f, invoice, 2, 2, 10_000_000, 1)
	unpaid, err := a.client[remote].GetInvoice(t.Context(), later)
	require.NoError(t, err)
	require.Equal(t, int64(10_000_000), unpaid.AmountDue, "the retry sees completed cadence before scanning the new invoice")
}
