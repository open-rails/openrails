package intents

import (
	"context"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// #696 refund leg — handler classification + verify. WIRE PROVISIONAL: these
// exercise the modeled request/response shape against a fake DataLink server (no
// live traffic, no DB); the success->finalize path needs a DB and is covered by
// the integration suite.

func ccbillRefundPayload() RefundPayload {
	p := testRefundPayload()
	p.ProviderTarget = "sub_123"       // CCBill subscriptionId
	p.ProviderTransactionID = "TX_999" // original transaction id
	return p
}

// ccbillRefundTestRails is the armed rail state (#788) the refund handler
// resolves its DataLink client from.
func ccbillRefundTestRails() railresolve.FixedSet {
	return railresolve.FixedSet{"ccbill": {
		Rail:      models.RailCCBill,
		AccountID: "900100-0000",
		CCBill:    &config.CCBillRailConfig{DataLinkUsername: "dluser", DataLinkPassword: "dlpass"},
	}}
}

func newCCBillRefundHandler(baseURL string, cfg *config.Config) *CCBillRefundHandler {
	h := NewCCBillRefundHandler(nil, cfg, ccbillRefundTestRails(), nil)
	h.DataLinkBaseURL = baseURL
	return h
}

// ccbillFakeSMS serves a scripted body and records hits.
func ccbillFakeSMS(t *testing.T, body string) (*CCBillRefundHandler, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return newCCBillRefundHandler(srv.URL, nil), &hits
}

func TestCCBillRefundIdempotencyKey(t *testing.T) {
	assert.Equal(t, "ccbill_refund:sub_1:tx_1:500", CCBillRefundIdempotencyKey(" sub_1 ", " tx_1 ", 500))
	assert.Equal(t,
		CCBillRefundIdempotencyKey("sub_1", "tx_1", 500),
		CCBillRefundIdempotencyKey("sub_1", "tx_1", 500))
	assert.NotEqual(t,
		CCBillRefundIdempotencyKey("sub_1", "tx_1", 500),
		CCBillRefundIdempotencyKey("sub_1", "tx_2", 500),
		"a different transaction is a different logical refund")
	assert.NotEqual(t,
		CCBillRefundIdempotencyKey("sub_1", "tx_1", 500),
		CCBillRefundIdempotencyKey("sub_1", "tx_1", 600),
		"a different amount is a different logical refund")
}

func TestRefundIntentForRoutesCCBill(t *testing.T) {
	ccbillPayment := &models.Payment{Rail: models.RailCCBill, TransactionID: "TX_999"}
	typ, provider, key, err := RefundIntentFor(ccbillPayment, "sub_123", 500, "ignored for ccbill")
	require.NoError(t, err)
	assert.Equal(t, TypeCCBillRefund, typ)
	assert.Equal(t, "ccbill", provider)
	// providerTarget is the subscriptionId; the transaction id comes off the payment.
	assert.Equal(t, CCBillRefundIdempotencyKey("sub_123", "TX_999", 500), key)
}

func TestCCBillRefundExecuteParksBeforeProviderTraffic(t *testing.T) {
	intent := refundIntent(t, TypeCCBillRefund, ccbillRefundPayload())

	t.Run("unarmed rail parks", func(t *testing.T) {
		h := NewCCBillRefundHandler(nil, nil, nil, nil)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "not configured")
	})

	t.Run("read-only client parks", func(t *testing.T) {
		cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureSandbox}
		h := newCCBillRefundHandler("https://datalink.ccbill.com", cfg)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "mode=readonly")
	})
}

func TestCCBillRefundExecuteRequiresTransactionID(t *testing.T) {
	h, _ := ccbillFakeSMS(t, `<results>1</results>`)
	payload := ccbillRefundPayload()
	payload.ProviderTransactionID = ""
	out := h.Execute(context.Background(), refundIntent(t, TypeCCBillRefund, payload))
	assert.Equal(t, OutcomeTerminal, out.Class)
	assert.Contains(t, out.Reason, "transaction id")
}

// ccbillDenialMaxAttempts bounds -7 (OVERLOADED denial) clean retries before
// terminal — never infinite retry as if it were pure auth.
func TestCCBillDenialExhausted(t *testing.T) {
	assert.False(t, ccbillDenialExhausted(1), "first attempt retries")
	assert.False(t, ccbillDenialExhausted(ccbillDenialMaxAttempts-1))
	assert.True(t, ccbillDenialExhausted(ccbillDenialMaxAttempts), "budget spent -> terminal")
	assert.True(t, ccbillDenialExhausted(ccbillDenialMaxAttempts+5))
}

func TestCCBillRefundExecuteClassification(t *testing.T) {
	// -7 denial below the retry budget: BOUNDED retry (did not execute), NOT the
	// old misleading "auth rejected" (verified: -7 also = not-refundable/too-old).
	t.Run("denial (-7) below budget is bounded retry", func(t *testing.T) {
		h, _ := ccbillFakeSMS(t, `<results>-7</results>`)
		out := h.Execute(context.Background(), refundIntent(t, TypeCCBillRefund, ccbillRefundPayload()))
		assert.Equal(t, OutcomeRetryable, out.Class)
		assert.Contains(t, out.Reason, "denied (-7)")
		assert.Contains(t, out.Reason, "bounded retry")
		assert.NotContains(t, out.Reason, "auth rejected", "reason must not misrepresent -7 as pure auth")
	})

	// Definite reject (results=0): may mean already-refunded -> verify, never decline.
	t.Run("definite reject is ambiguous", func(t *testing.T) {
		h, _ := ccbillFakeSMS(t, `<results>0</results>`)
		out := h.Execute(context.Background(), refundIntent(t, TypeCCBillRefund, ccbillRefundPayload()))
		assert.Equal(t, OutcomeAmbiguous, out.Class)
		assert.Contains(t, out.Reason, "outcome unknown")
	})

	// Transport failure after send: MAY have moved money -> ambiguous.
	t.Run("transport failure is ambiguous", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(srv.Close)
		h := newCCBillRefundHandler(srv.URL, nil)
		out := h.Execute(context.Background(), refundIntent(t, TypeCCBillRefund, ccbillRefundPayload()))
		assert.Equal(t, OutcomeAmbiguous, out.Class)
	})
}

// Reclaimed lease (attempts > 1): pre-send verify must read counters and refuse
// to resend unless they are known-zero.
func TestCCBillRefundExecuteReclaimPreSendVerify(t *testing.T) {
	t.Run("counters nonzero -> do not resend (ambiguous)", func(t *testing.T) {
		h, hits := ccbillFakeSMS(t, `<results><refundsIssued>1</refundsIssued><voidsIssued>0</voidsIssued><subscriptionStatus>2</subscriptionStatus></results>`)
		intent := refundIntent(t, TypeCCBillRefund, ccbillRefundPayload())
		intent.Attempts = 2
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeAmbiguous, out.Class)
		assert.Contains(t, out.Reason, "not resending")
		assert.Equal(t, 1, *hits, "only the verify read, no refund resend")
	})

	t.Run("pre-send read fails -> ambiguous", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		h := newCCBillRefundHandler(srv.URL, nil)
		intent := refundIntent(t, TypeCCBillRefund, ccbillRefundPayload())
		intent.Attempts = 2
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeAmbiguous, out.Class)
		assert.Contains(t, out.Reason, "pre-send verification read failed")
	})
}

func TestCCBillRefundVerify(t *testing.T) {
	t.Run("counters known-zero -> verified not executed (retryable)", func(t *testing.T) {
		h, _ := ccbillFakeSMS(t, `<results><refundsIssued>0</refundsIssued><voidsIssued>0</voidsIssued><subscriptionStatus>2</subscriptionStatus></results>`)
		out := h.Verify(context.Background(), refundIntent(t, TypeCCBillRefund, ccbillRefundPayload()))
		assert.Equal(t, OutcomeRetryable, out.Class)
		assert.Contains(t, out.Reason, "verified not executed")
	})

	t.Run("counters nonzero -> cannot attribute (ambiguous, operator resolves)", func(t *testing.T) {
		h, _ := ccbillFakeSMS(t, `<results><refundsIssued>1</refundsIssued><voidsIssued>0</voidsIssued><subscriptionStatus>2</subscriptionStatus></results>`)
		out := h.Verify(context.Background(), refundIntent(t, TypeCCBillRefund, ccbillRefundPayload()))
		assert.Equal(t, OutcomeAmbiguous, out.Class)
		assert.Contains(t, out.Reason, "operator must confirm")
	})

	t.Run("counters unknown (omitted) -> ambiguous", func(t *testing.T) {
		h, _ := ccbillFakeSMS(t, `<results><returnsIssued>1</returnsIssued><subscriptionStatus>0</subscriptionStatus></results>`)
		out := h.Verify(context.Background(), refundIntent(t, TypeCCBillRefund, ccbillRefundPayload()))
		assert.Equal(t, OutcomeAmbiguous, out.Class)
	})

	t.Run("read failure stays ambiguous", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		h := newCCBillRefundHandler(srv.URL, nil)
		out := h.Verify(context.Background(), refundIntent(t, TypeCCBillRefund, ccbillRefundPayload()))
		assert.Equal(t, OutcomeAmbiguous, out.Class)
		assert.Contains(t, out.Reason, "provider read failed")
	})
}
