package handlers

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

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
