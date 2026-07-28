package handlers

import (
	"strings"
	"testing"
)

// TestStripeRelatedObjectURLIsAlwaysAPath is the SEC-24 item 5 guard.
//
// Thin-event hydration sends `Authorization: Bearer <sk_live_…>` — the
// merchant's LIVE Stripe secret key — to whatever the related-object url names.
// The previous code was `if !strings.HasPrefix(url, "http") { url = base + url }`,
// which is the inverse of a guard: an absolute URL was used verbatim. The path
// is reached only after HMAC verification, so it did not create the hole on its
// own; it escalated "webhook signing secret leaked" into "live API key
// exfiltrated to an attacker-chosen host".
func TestStripeRelatedObjectURLIsAlwaysAPath(t *testing.T) {
	const base = "https://api.stripe.com"

	refused := []struct{ name, in string }{
		{"absolute attacker url", "https://evil.example/collect"},
		{"absolute http url", "http://evil.example/collect"},
		{"protocol relative", "//evil.example/v1/charges"},
		{"credentials in authority", "https://user:pass@evil.example/x"},
		{"scheme smuggled mid-string", "x://evil.example/y"},
		{"backslash authority", "\\\\evil.example\\v1"},
		{"newline injection", "/v1/charges\nHost: evil.example"},
		{"traversal escape", "/v1/../../evil"},
		{"empty", "   "},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripeRelatedObjectURL(tc.in)
			if err == nil {
				t.Fatalf("expected refusal, got %q — the merchant's live secret key would be sent there", got)
			}
		})
	}

	accepted := map[string]string{
		"/v1/charges/ch_123":    base + "/v1/charges/ch_123",
		"v1/charges/ch_123":     base + "/v1/charges/ch_123",
		"/v1/invoices/in_1?x=1": base + "/v1/invoices/in_1?x=1",
	}
	for in, want := range accepted {
		got, err := stripeRelatedObjectURL(in)
		if err != nil {
			t.Fatalf("%q: unexpected refusal: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
		if !strings.HasPrefix(got, base+"/") {
			t.Fatalf("%q escaped the Stripe API host: %q", in, got)
		}
	}
}
