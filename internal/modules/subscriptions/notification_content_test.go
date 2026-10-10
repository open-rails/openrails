package subscriptions

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
)

// Access-ended mail goes to often long-lapsed users, so its copy stays
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
		"receipt":         RenderPurchaseReceiptEmail(evil, evil, billing.NotificationData{ProductName: evil, Amount: new(int64(1_000_000)), Currency: "USD", OrderNumber: evil}, start).HTML,
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

// Emails show an amount at its currency's own scale: cents for a six-decimal
// currency, whole yen for a zero-decimal one, never native units.
func TestEmailAmountsUseTheCurrencyScale(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		amount          int64
		currency, shown string
	}{
		{19_990_000, "USD", "19.99 USD"},
		{5_000_000, "JPY", "500 JPY"},
	} {
		data := SubscriptionEmailData{Username: "alice", Amount: tc.amount, Currency: tc.currency, PeriodStart: start, PeriodEnd: start.Add(720 * time.Hour)}
		mail := &sentMail{}
		receipt := &models.NotificationQueue{EventType: models.NotificationOneOffPurchaseCompleted, CreatedAt: start,
			Data: billing.NotificationData{UserEmail: "alice@example.com", ProductName: "Credits", Amount: &tc.amount, Currency: tc.currency}}
		require.NoError(t, NewEmailService(mail, nil).SendPurchaseReceipt(context.Background(), receipt))
		require.Len(t, mail.sent, 1)
		for name, c := range map[string]EmailContent{
			"confirmation":   RenderSubscriptionConfirmationEmail("Store", data),
			"renewal":        RenderSubscriptionRenewalEmail("Store", data),
			"payment failed": RenderPaymentFailedEmail("Store", "", data),
			"receipt":        {HTML: mail.sent[0].HTML, Plain: mail.sent[0].Text},
		} {
			for _, body := range []string{c.HTML, c.Plain} {
				require.Contains(t, body, tc.shown, name)
				require.NotContains(t, body, strconv.FormatInt(tc.amount, 10), name)
			}
		}
	}
}

type sentMail struct{ sent []config.Email }

func (m *sentMail) Send(_ context.Context, e config.Email) error {
	m.sent = append(m.sent, e)
	return nil
}

func (*sentMail) CheckHealth(context.Context) error { return nil }
