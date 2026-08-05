package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
