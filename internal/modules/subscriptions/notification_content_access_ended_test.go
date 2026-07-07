package subscriptions

import (
	"strings"
	"testing"
	"time"
)

func TestRenderAccessEndedEmail(t *testing.T) {
	endedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	t.Run("with signup url", func(t *testing.T) {
		c := RenderAccessEndedEmail("Doujins", "https://example.com/signup", "alice", endedAt)
		if c.Subject != "Your Doujins premium access has ended" {
			t.Fatalf("subject = %q", c.Subject)
		}
		if !strings.Contains(c.HTML, "Hi alice,") {
			t.Errorf("HTML missing greeting: %q", c.HTML)
		}
		if !strings.Contains(c.HTML, "Jul 4, 2026") {
			t.Errorf("HTML missing ended date: %q", c.HTML)
		}
		if !strings.Contains(c.HTML, `href="https://example.com/signup"`) || !strings.Contains(c.HTML, "Sign up again") {
			t.Errorf("HTML missing signup CTA: %q", c.HTML)
		}
		if !strings.Contains(c.Plain, "https://example.com/signup") {
			t.Errorf("Plain missing signup URL: %q", c.Plain)
		}
		// Neutral copy: never charge/dunning language (often long-lapsed users).
		for _, banned := range []string{"charge", "payment", "renew"} {
			if strings.Contains(strings.ToLower(c.HTML), banned) {
				t.Errorf("HTML contains %q — copy must stay neutral", banned)
			}
		}
	})

	t.Run("without signup url", func(t *testing.T) {
		c := RenderAccessEndedEmail("Doujins", "", "", endedAt)
		if strings.Contains(c.HTML, "href=") || strings.Contains(c.HTML, "Sign up again</a>") {
			t.Errorf("HTML must not render CTA without a URL: %q", c.HTML)
		}
		if !strings.Contains(c.HTML, "Hi there,") {
			t.Errorf("HTML missing fallback greeting: %q", c.HTML)
		}
	})
}

func TestParsePremiumEndReasonAccessEnded(t *testing.T) {
	if got := ParsePremiumEndReason("access_ended"); got != PremiumEndReasonAccessEnded {
		t.Fatalf("ParsePremiumEndReason(access_ended) = %q", got)
	}
	if got := ParsePremiumEndReason(string(PremiumEndReasonAccessEnded)); got != PremiumEndReasonAccessEnded {
		t.Fatalf("round-trip = %q", got)
	}
	if got := ParsePremiumEndReason("bogus"); got != PremiumEndReasonUnknown {
		t.Fatalf("unknown fallback = %q", got)
	}
}
