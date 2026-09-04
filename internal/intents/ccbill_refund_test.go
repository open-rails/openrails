package intents

import (
	"context"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

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
