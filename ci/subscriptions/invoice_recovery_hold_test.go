//go:build e2e && integration

package subscriptions_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/providerrecovery"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestInvoicePaymentsProceedWithoutBulkProviderHistory(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, c, 50_000_000)
	blockHistory := func() {
		w.nmi.Intercept(func(r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/query.php") {
				return false
			}
			form, _ := url.ParseQuery(readBody(r))
			return form.Get("start_date") != "" // exact invoice preflight still reads
		}, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("history unavailable")), Request: r}, nil
		})
	}
	blockHistory()
	t.Cleanup(w.nmi.ClearIntercepts)
	w.advance(5 * 24 * time.Hour) // bulk history is unavailable; exact payment checks still work
	answer := payNMIInvoice(t.Context(), w, c, invoice, method, "customer-recovery")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusOK, answer.status, string(answer.body))
	require.Len(t, w.nmi.Attempts(), 1, "stale bulk coverage cannot block the requested payment")
	var operations int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='order_collection'`)).Scan(&operations))
	require.Equal(t, 1, operations, "the ordinary payment has one canonical operation")
	w.nmi.ClearIntercepts()
	w.refreshProviders()
	retry := payNMIInvoice(t.Context(), w, c, invoice, method, "customer-recovery")
	require.NoError(t, retry.err)
	require.Equal(t, http.StatusOK, retry.status, string(retry.body))
	require.Len(t, w.nmi.Attempts(), 1)

	// A real decline still needs the customer's recurring agreement. Once the
	// card is selected for USD invoices, bulk-refresh failure does not prevent
	// the merchant-initiated retry.
	second := observedInvoice(t, w, c, 50_000_000)
	w.nmi.SetDecline(visa.Last4, "202")
	declined := payNMIInvoice(t.Context(), w, c, second, method, "declined")
	require.NoError(t, declined.err)
	require.Equal(t, http.StatusPaymentRequired, declined.status, string(declined.body))
	w.nmi.SetDecline(visa.Last4, "")
	c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": method})
	blockHistory()
	w.advance(5 * 24 * time.Hour)
	w.settleCollectionScans()
	var nextRetry *time.Time
	var failures int
	var previousState, previousReason string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT next_collection_attempt_at, collection_failure_count
        FROM billing.orders WHERE id=$1`), second.UUID()).Scan(&nextRetry, &failures))
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status,coalesce(last_failure_reason,'') FROM billing.provider_intents
        WHERE intent_type='order_collection' AND payload->>'order_id'=$1 ORDER BY created_at DESC LIMIT 1`), second.UUID().String()).Scan(&previousState, &previousReason))
	t.Logf("before scheduled retry: previous_intent=%s reason=%s failures=%d retry_at=%v now=%v", previousState, previousReason, failures, nextRetry, w.clock.Now())
	require.Equal(t, "failed_terminal", previousState, "the prior declined attempt is finished")
	require.Equal(t, 1, failures)
	require.NotNil(t, nextRetry)
	require.False(t, nextRetry.After(w.clock.Now()), "the declared retry is due")
	// Advancing the fake business clock does not run an hourly job. Trigger
	// the same collecting pass that production schedules, not another user pay.
	mid := w.client[embedded].MerchantID()
	job, err := w.jobs.Insert(t.Context(), riverjobs.UsageOrderArgs{MerchantID: &mid, Collect: true}, &river.InsertOpts{Queue: "billing"})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)
	w.settle()
	require.Equal(t, billing.OrderComplete, w.invoice(second).Status, "the agreed retry proceeds while bulk history is unavailable")
	require.Len(t, w.nmi.Attempts(), 3)
	w.nmi.ClearIntercepts()
	w.refreshProviders()
	w.settleCollectionScans()
	w.settle()
	require.Equal(t, billing.OrderComplete, w.invoice(second).Status, "later refresh does not change the completed payment")
	require.Len(t, w.nmi.Attempts(), 3, "each invoice has one approved sale; the decline stays an attempt")
	require.Equal(t, "merchant", w.nmi.LastSale().InitiatedBy)
	require.Len(t, w.nmi.ledger(""), 2)
}

// Refresh cadence is observability, never permission to pay an invoice.
func TestMissedRefreshCycleDoesNotBlockInvoicePayment(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	w.advance(5 * 24 * time.Hour) // an established book
	w.refreshProviders()
	w.settleRefreshes() // the startup refresh too, or its coverage lands later
	refreshed := w.clock.Now()

	onSchedule := observedInvoice(t, w, c, 50_000_000)
	w.advance(refreshed.Add(providerrecovery.RefreshInterval + 30*time.Minute).Sub(w.clock.Now()))
	answer := payNMIInvoice(t.Context(), w, c, onSchedule, method, "on-schedule")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusOK, answer.status, "the next refresh running late does not pause billing: %s", answer.body)
	require.Len(t, w.nmi.Attempts(), 1)

	missed := observedInvoice(t, w, c, 50_000_000)
	w.advance(refreshed.Add(2*providerrecovery.RefreshInterval + 30*time.Minute).Sub(w.clock.Now()))
	answer = payNMIInvoice(t.Context(), w, c, missed, method, "missed-cycle")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusOK, answer.status, "a missed refresh cycle does not hold the book: %s", answer.body)
	require.Len(t, w.nmi.Attempts(), 2)
}
