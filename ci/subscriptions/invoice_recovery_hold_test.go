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
	"github.com/stretchr/testify/require"
)

func TestInvoiceRecoveryHoldIsTemporaryOnCustomerAndMerchantHTTP(t *testing.T) {
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
	w.advance(5 * 24 * time.Hour) // deliberately no refresh: the safety hold is the subject
	answer := payNMIInvoice(t.Context(), w, c, invoice, method, "customer-recovery")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusServiceUnavailable, answer.status, string(answer.body))
	require.Contains(t, string(answer.body), `"code":"service_unavailable"`)
	require.Contains(t, string(answer.body), "billing is paused while provider recovery completes")
	require.NotContains(t, string(answer.body), w.psp["nmi"].UUID().String())
	require.NotContains(t, string(answer.body), "history unavailable")
	require.Empty(t, w.nmi.Attempts())
	var operations int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='invoice_collection'`)).Scan(&operations))
	require.Zero(t, operations, "temporary pre-admission hold created no payment operation")
	w.nmi.ClearIntercepts()
	w.refreshProviders()
	retry := payNMIInvoice(t.Context(), w, c, invoice, method, "customer-recovery")
	require.NoError(t, retry.err)
	require.Equal(t, http.StatusOK, retry.status, string(retry.body))
	require.Len(t, w.nmi.Attempts(), 1)

	// The same established temporary code applies to merchant retry after a
	// real decline. The original approved payment supplied the MIT agreement.
	second := observedInvoice(t, w, c, 50_000_000)
	w.nmi.SetDecline(visa.Last4, "202")
	declined := payNMIInvoice(t.Context(), w, c, second, method, "declined")
	require.NoError(t, declined.err)
	require.Equal(t, http.StatusPaymentRequired, declined.status, string(declined.body))
	w.nmi.SetDecline(visa.Last4, "")
	blockHistory()
	w.advance(5 * 24 * time.Hour)
	params := billing.RetryInvoiceCollectionParams{PaymentMethodID: pmid(method), IdempotencyKey: "merchant-recovery"}
	_, err := w.client[remote].RetryInvoiceCollection(t.Context(), second, params)
	requireCode(t, err, http.StatusServiceUnavailable, billing.CodeServiceUnavailable)
	require.Len(t, w.nmi.Attempts(), 2, "held retry sends no additional sale")
	w.nmi.ClearIntercepts()
	w.refreshProviders()
	paid, err := w.client[remote].RetryInvoiceCollection(t.Context(), second, params)
	require.NoError(t, err)
	require.Equal(t, billing.InvoicePaid, paid.Invoice.Status)
	require.Len(t, w.nmi.Attempts(), 3, "each invoice has one approved sale; the decline stays an attempt")
	require.Len(t, w.nmi.ledger(""), 2)
}

// A refresh's coverage ends at its start less the provider window delay, and
// the scheduler reshuffles each cycle's merchants over its stagger, so an
// account's next refresh finishes after a full interval has passed. A book
// refreshed on schedule is not stale: its customer pays while that refresh is
// still due. Only a whole missed cycle holds the book.
func TestRefreshedBookStaysPayableUntilACycleIsMissed(t *testing.T) {
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
	require.Equal(t, http.StatusServiceUnavailable, answer.status, "a missed refresh cycle holds the book: %s", answer.body)
	require.Len(t, w.nmi.Attempts(), 1)
}
