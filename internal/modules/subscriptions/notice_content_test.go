package subscriptions

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Each notice renders its own data at the currency's scale, and the notices
// about a recurring agreement say how to cancel it.
func TestNoticeEmails(t *testing.T) {
	due := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	amount, old := int64(42_500_000), int64(9_990_000)
	const billingPage = "https://shop.test/billing"
	order := billing.NotificationData{OrderID: billing.OrderID(uuid.New()), AmountDue: &amount, Currency: "USD", DueAt: &due}
	automatic, invoiced, retried := order, order, order
	invoiced.CollectionMethod = billing.CollectSendInvoice
	retried.NextAttemptAt = &due
	stopped := order
	stopped.Reason = "non_recoverable"
	delinquent := billing.NotificationData{Currency: "USD", OverdueAmount: &amount, OverdueOrders: 2, OverdueStartedAt: &due}
	reprice := billing.NotificationData{OldAmount: &old, NewAmount: &amount, Currency: "USD", EffectiveAt: &due}
	plan := reprice
	plan.ToProductName = "Studio"

	cancelUsage := "To cancel charges for your usage, and with it your agreement to these charges, visit https://shop.test/billing."
	cancelPlan := "To cancel this subscription, and with it your agreement to these charges, visit https://shop.test/billing."
	for name, tc := range map[string]struct {
		content EmailContent
		says    []string
		cancel  string
	}{
		"due, charged":          {RenderOrderDueEmail("Shop", billingPage, "ada", automatic), []string{"42.50 USD", "November 5, 2026", "card on file"}, cancelUsage},
		"due, invoiced":         {RenderOrderDueEmail("Shop", billingPage, "ada", invoiced), []string{"42.50 USD", "November 5, 2026", "Please pay it by then", billingPage}, ""},
		"overdue":               {RenderOrderOverdueEmail("Shop", billingPage, "ada", order), []string{"42.50 USD", "November 5, 2026", "still unpaid", billingPage}, ""},
		"card failed":           {RenderOrderCardFailedEmail("Shop", billingPage, "ada", retried), []string{"42.50 USD", "try again on", "November 5, 2026", billingPage}, ""},
		"card update":           {RenderOrderCardUpdateEmail("Shop", billingPage, "ada", order), []string{"42.50 USD", "stopped charging it", billingPage}, ""},
		"collection stopped":    {RenderOrderCollectionStoppedEmail("Shop", billingPage, "ada", stopped), []string{"42.50 USD", "can no longer be charged", "still owed"}, ""},
		"collection exhausted":  {RenderOrderCollectionStoppedEmail("Shop", billingPage, "ada", order), []string{"several times"}, ""},
		"delinquent":            {RenderAccountDelinquentEmail("Shop", billingPage, "ada", delinquent), []string{"42.50 USD", "overdue since", "November 5, 2026", "paused"}, ""},
		"delinquency cleared":   {RenderAccountDelinquencyClearedEmail("Shop", "ada"), []string{"no longer paused"}, ""},
		"reprice scheduled":     {RenderPriceChangeEmail("Shop", billingPage, "ada", "Pro", reprice), []string{"9.99 USD", "42.50 USD", "November 5, 2026", "cancel before"}, cancelPlan},
		"plan change scheduled": {RenderPriceChangeEmail("Shop", billingPage, "ada", "Pro", plan), []string{"moves to Studio", "Studio, 42.50 USD", "Pro, 9.99 USD"}, cancelPlan},
	} {
		c := tc.content
		require.NotEmpty(t, c.Subject, name)
		for _, body := range []string{c.HTML, c.Plain} {
			require.Contains(t, body, "Hi ada,", name)
			require.Contains(t, body, "The Shop Team", name)
			require.NotContains(t, body, "42500000", name)
			for _, s := range tc.says {
				require.Contains(t, body, s, name)
			}
		}
		if tc.cancel == "" {
			require.NotContains(t, strings.ToLower(c.Plain), "to cancel", name)
			continue
		}
		require.Contains(t, c.Plain, tc.cancel, name)
		require.Contains(t, c.HTML, `visit <a href="https://shop.test/billing">`, name)
	}

	without := RenderOrderDueEmail("Shop", "", "", automatic)
	require.Contains(t, without.Plain, "To cancel charges for your usage, and with it your agreement to these charges, sign in to your account at Shop.")
	require.Contains(t, without.HTML, "sign in to your account at Shop")
	require.Contains(t, without.HTML, "Hi there,")
	require.NotContains(t, RenderOrderDueEmail("Shop", "", "ada", invoiced).HTML, "href=", "no billing page, no button")
}

// Customer- and merchant-supplied text is escaped in every notice, and a
// hostile link is never an href.
func TestNoticeEmailsEscapeUserText(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>"'&`
	const evilURL = `javascript:alert(1)`
	at, amount := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), int64(1_000_000)
	data := billing.NotificationData{AmountDue: &amount, OverdueAmount: &amount, OldAmount: &amount, NewAmount: &amount, Currency: "USD",
		DueAt: &at, EffectiveAt: &at, OverdueStartedAt: &at, ToProductName: evil}
	for name, html := range map[string]string{
		"due":       RenderOrderDueEmail(evil, evilURL, evil, data).HTML,
		"overdue":   RenderOrderOverdueEmail(evil, evilURL, evil, data).HTML,
		"failed":    RenderOrderCardFailedEmail(evil, evilURL, evil, data).HTML,
		"update":    RenderOrderCardUpdateEmail(evil, evilURL, evil, data).HTML,
		"stopped":   RenderOrderCollectionStoppedEmail(evil, evilURL, evil, data).HTML,
		"behind":    RenderAccountDelinquentEmail(evil, evilURL, evil, data).HTML,
		"cleared":   RenderAccountDelinquencyClearedEmail(evil, evil).HTML,
		"plan move": RenderPriceChangeEmail(evil, evilURL, evil, evil, data).HTML,
	} {
		require.NotContains(t, html, "<img", name)
		require.NotContains(t, html, `href="`+evilURL, name)
		require.Contains(t, html, "&lt;img src=x onerror=alert(1)&gt;&#34;&#39;&amp;", name)
	}
}
