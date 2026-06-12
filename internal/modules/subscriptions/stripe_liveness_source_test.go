package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStripeLivenessSubscription_ActivePaidInvoice(t *testing.T) {
	body := []byte(`{
		"id": "sub_123",
		"status": "active",
		"cancel_at_period_end": false,
		"current_period_start": 1780000000,
		"current_period_end": 1782592000,
		"latest_invoice": {
			"id": "in_9",
			"status": "paid",
			"paid": true,
			"charge": "ch_55",
			"payment_intent": "pi_44",
			"amount_paid": 999,
			"currency": "usd"
		}
	}`)
	rec, err := parseStripeLivenessSubscription(body)
	require.NoError(t, err)
	assert.True(t, rec.Found)
	assert.Equal(t, "active", rec.Status)
	assert.Equal(t, time.Unix(1782592000, 0).UTC(), rec.CurrentPeriodEnd)
	assert.True(t, rec.LatestInvoicePaid)
	// Webhook-compatible precedence: charge wins over payment_intent/invoice.
	assert.Equal(t, "ch_55", rec.LatestInvoiceTransactionID)
	assert.Equal(t, int64(999), rec.LatestInvoiceAmountPaid)
	assert.Equal(t, "usd", rec.LatestInvoiceCurrency)
}

func TestParseStripeLivenessSubscription_TransactionIDFallbacks(t *testing.T) {
	rec, err := parseStripeLivenessSubscription([]byte(`{
		"id": "sub_1", "status": "active",
		"latest_invoice": {"id": "in_1", "status": "paid", "payment_intent": "pi_2"}
	}`))
	require.NoError(t, err)
	assert.Equal(t, "pi_2", rec.LatestInvoiceTransactionID)

	rec, err = parseStripeLivenessSubscription([]byte(`{
		"id": "sub_1", "status": "active",
		"latest_invoice": {"id": "in_1", "status": "paid"}
	}`))
	require.NoError(t, err)
	assert.Equal(t, "in_1", rec.LatestInvoiceTransactionID)
}

func TestParseStripeLivenessSubscription_NoInvoice(t *testing.T) {
	rec, err := parseStripeLivenessSubscription([]byte(`{"id": "sub_1", "status": "canceled"}`))
	require.NoError(t, err)
	assert.True(t, rec.Found)
	assert.Equal(t, "canceled", rec.Status)
	assert.False(t, rec.LatestInvoicePaid)
	assert.Empty(t, rec.LatestInvoiceTransactionID)
}

func TestHTTPStripeLivenessProber_NotFoundIsAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": {"type": "invalid_request_error", "code": "resource_missing"}}`))
	}))
	t.Cleanup(srv.Close)

	prober := &HTTPStripeLivenessProber{SecretKey: "sk_test_x", BaseURL: srv.URL, HTTPClient: srv.Client()}
	rec, err := prober.ProbeSubscription(context.Background(), "sub_gone")
	require.NoError(t, err, "404 means remote absent, not transport failure")
	assert.False(t, rec.Found)
}

func TestHTTPStripeLivenessProber_FetchesExpandedSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/v1/subscriptions/sub_42", r.URL.Path)
		assert.Equal(t, "latest_invoice", r.URL.Query().Get("expand[]"))
		assert.Equal(t, "Bearer sk_test_x", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"id": "sub_42", "status": "past_due"}`))
	}))
	t.Cleanup(srv.Close)

	prober := &HTTPStripeLivenessProber{SecretKey: "sk_test_x", BaseURL: srv.URL, HTTPClient: srv.Client()}
	rec, err := prober.ProbeSubscription(context.Background(), "sub_42")
	require.NoError(t, err)
	assert.True(t, rec.Found)
	assert.Equal(t, "past_due", rec.Status)
}
