package handlers

import (
	"net/http"
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
