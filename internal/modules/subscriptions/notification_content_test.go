package subscriptions

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #789: access-ended mail goes to often long-lapsed users, so its copy stays
// neutral, and the CTA exists only when there is somewhere to send them.
func TestRenderAccessEndedEmail(t *testing.T) {
	endedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	c := RenderAccessEndedEmail("Host One", "https://example.com/signup", "alice", endedAt)
	require.Equal(t, "Your Host One premium access has ended", c.Subject)
	for _, body := range []string{c.HTML, c.Plain} {
		require.Contains(t, body, "Hi alice,")
		require.Contains(t, body, "Jul 4, 2026")
		require.Contains(t, body, "https://example.com/signup")
		for _, banned := range []string{"charge", "payment", "renew"} {
			require.NotContains(t, strings.ToLower(body), banned)
		}
	}
	require.Contains(t, c.HTML, `href="https://example.com/signup"`)

	c = RenderAccessEndedEmail("Host One", "", "  ", endedAt)
	require.NotContains(t, c.HTML, "href=")
	require.NotContains(t, c.Plain, "Sign up again any time")
	require.Contains(t, c.HTML, "Hi there,")
}

// Customer- and merchant-supplied text is escaped by context in every HTML
// body, and a hostile link cannot become a script URL.
func TestEmailHTMLEscapesUserText(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>"'&`
	const evilURL = `javascript:alert(1)`
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	data := SubscriptionEmailData{Username: evil, ProductName: evil, Amount: 1999, Currency: "USD", PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), PaymentMethod: evil, TransactionID: evil}
	bodies := map[string]string{
		"confirmation":    RenderSubscriptionConfirmationEmail(evil, data).HTML,
		"renewal":         RenderSubscriptionRenewalEmail(evil, data).HTML,
		"cancellation":    RenderSubscriptionCancellationEmail(evil, data, PremiumEndReasonChargeback).HTML,
		"expired":         RenderSubscriptionExpiredEmail(evil, evilURL, data).HTML,
		"access ended":    RenderAccessEndedEmail(evil, evilURL, evil, start).HTML,
		"payment failed":  RenderPaymentFailedEmail(evil, evilURL, data).HTML,
		"update required": RenderPaymentMethodUpdateRequiredEmail(evil, evilURL, data).HTML,
		"non recoverable": RenderSubscriptionNonRecoverableEmail(evil, evilURL, data).HTML,
		"expiring":        renderEmailHTML("entitlement_expiring", emailFields{"Username": evil, "Entitlement": evil, "Remaining": "3 days", "ExpiresOn": "Jul 4", "Store": evil}),
		"receipt":         renderEmailHTML("purchase_receipt", emailFields{"Solana": true, "Intro": evil, "Product": evil, "Amount": "$1", "Date": "Jul 4", "Store": evil}),
	}
	for name, html := range bodies {
		require.NotContains(t, html, "<img", name)
		require.NotContains(t, html, evilURL, name)
		require.Contains(t, html, "&lt;img src=x onerror=alert(1)&gt;&#34;&#39;&amp;", name)
	}
	require.Contains(t, bodies["expired"], `href="#ZgotmplZ"`)
}

func TestParsePremiumEndReasonRoundTrips(t *testing.T) {
	for _, r := range []PremiumEndReason{
		PremiumEndReasonUserCancel, PremiumEndReasonExpired, PremiumEndReasonChargeback, PremiumEndReasonRefund,
		PremiumEndReasonAdmin, PremiumEndReasonRail, PremiumEndReasonAccessEnded, PremiumEndReasonNonRecoverable,
	} {
		require.Equal(t, r, ParsePremiumEndReason(string(r)))
		require.Equal(t, r, ParsePremiumEndReason(strings.ToUpper(string(r))))
	}
	require.Equal(t, PremiumEndReasonUnknown, ParsePremiumEndReason("bogus"))
	require.Equal(t, PremiumEndReasonUnknown, ParsePremiumEndReason(""))
}
