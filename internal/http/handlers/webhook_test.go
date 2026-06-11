package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/stretchr/testify/require"

	httprequest "github.com/open-rails/openrails/internal/http/request"
)

func newTestWebhookRequest(t *testing.T, body string) (*httprequest.Request, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill", strings.NewReader(body))
	return httprequest.NewHTTP(w, req, nil), w
}

func TestReadLimitedWebhookBodyRejectsOversized(t *testing.T) {
	r, w := newTestWebhookRequest(t, strings.Repeat("a", int(maxCCBillWebhookBytes)+1))

	body, ok := readLimitedWebhookBody(r, maxCCBillWebhookBytes)
	if ok {
		t.Fatalf("expected oversized body to be rejected")
	}
	if body != nil {
		t.Fatalf("expected nil body on rejection, got %d bytes", len(body))
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestReadLimitedWebhookBodyAcceptsWithinLimit(t *testing.T) {
	payload := strings.Repeat("a", int(maxCCBillWebhookBytes)-1)
	r, w := newTestWebhookRequest(t, payload)

	body, ok := readLimitedWebhookBody(r, maxCCBillWebhookBytes)
	if !ok {
		t.Fatalf("expected body within limit to be accepted, got status %d", w.Code)
	}
	if string(body) != payload {
		t.Fatalf("expected body to round-trip, got %d bytes", len(body))
	}
}

func TestFirstPresentHeader(t *testing.T) {
	header := http.Header{}
	header.Set("X-NMI-Signature", "sig-123")
	header.Set("Webhook-Signature", "")

	got := firstPresentHeader(header, "Webhook-Signature", "X-Signature", "X-NMI-Signature")
	if got != "sig-123" {
		t.Fatalf("expected fallback signature header, got %q", got)
	}
}

func TestReadRequestBodyDoesNotConstrainWebhookSize(t *testing.T) {
	largeBody := strings.Repeat("a", (1<<20)+1)
	body, err := readRequestBody(io.NopCloser(strings.NewReader(largeBody)))
	if err != nil {
		t.Fatalf("expected oversized webhook body to read without size error: %v", err)
	}
	if string(body) != largeBody {
		t.Fatalf("expected body to round-trip, got %d bytes", len(body))
	}
}

func TestVerifyHook0Signature(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"eventType":"onramp.transaction.success","partnerUserRef":"ufs_00000000-0000-0000-0000-000000000001"}`)
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Custom", "abc")
	signature := signHook0("whsec_coinbase", body, headers, "content-type x-custom", now)

	require.NoError(t, verifyHook0Signature(body, signature, "whsec_coinbase", headers, now, 5*time.Minute))
	require.ErrorIs(t, verifyHook0Signature(body, signature, "wrong", headers, now, 5*time.Minute), webhookutil.ErrWebhookSignatureInvalid)
	require.ErrorIs(t, verifyHook0Signature(body, "", "whsec_coinbase", headers, now, 5*time.Minute), webhookutil.ErrWebhookSignatureMissing)
	require.ErrorIs(t, verifyHook0Signature(body, signature, "whsec_coinbase", headers, now.Add(10*time.Minute), 5*time.Minute), webhookutil.ErrWebhookSignatureInvalid)
}

func TestCoinbaseFundingSessionID(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"session": map[string]any{
				"partnerUserRef": "ufs_00000000-0000-0000-0000-000000000001",
			},
		},
	}
	got, ok := coinbaseFundingSessionID(payload)
	require.True(t, ok)
	require.Equal(t, "ufs_00000000-0000-0000-0000-000000000001", got)
}

func signHook0(secret string, body []byte, headers http.Header, headerNames string, now time.Time) string {
	timestamp := fmt.Sprintf("%d", now.Unix())
	values := make([]string, 0)
	for _, name := range strings.Fields(headerNames) {
		values = append(values, headers.Get(name))
	}
	signedPayload := timestamp + "." + headerNames + "." + strings.Join(values, ".") + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signedPayload))
	return "t=" + timestamp + ",h=" + headerNames + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
