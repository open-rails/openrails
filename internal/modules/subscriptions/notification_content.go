package subscriptions

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type EmailContent struct {
	Subject string
	HTML    string
	Plain   string
}

func RenderSubscriptionConfirmationEmail(storeName string, data SubscriptionEmailData) EmailContent {
	premiumName := subscriptionProductName(storeName, data.ProductName)
	amountLine := moneyutil.FormatDisplay(data.Amount, data.Currency)

	return EmailContent{
		Subject: fmt.Sprintf("Welcome to %s! Your subscription is confirmed", premiumName),
		HTML: fmt.Sprintf(`
			<h2>Welcome to %s!</h2>
			<p>Hi %s,</p>
			<p>Your subscription has been successfully activated. Thank you for your support!</p>
			<h3>Subscription Details:</h3>
			<ul>
				<li><strong>Subscription ID:</strong> %s</li>
				<li><strong>Amount:</strong> %s</li>
				<li><strong>Current Period:</strong> %s to %s</li>
				<li><strong>Payment Method:</strong> %s</li>
			</ul>
			<p>Enjoy!</p>
			<p>The %s Team</p>
		`, premiumName, data.Username, data.SubscriptionID, amountLine,
			data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.PaymentMethod, storeName),
		Plain: fmt.Sprintf(`
			Welcome to %s!

			Hi %s,

			Your subscription has been successfully activated. Thank you for your support!

			Subscription Details:
			- Subscription ID: %s
			- Amount: %s
			- Current Period: %s to %s
			- Payment Method: %s

			Enjoy!
			The %s Team
		`, premiumName, data.Username, data.SubscriptionID, amountLine,
			data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.PaymentMethod, storeName),
	}
}

func RenderSubscriptionRenewalEmail(storeName string, data SubscriptionEmailData) EmailContent {
	premiumName := subscriptionProductName(storeName, data.ProductName)
	amountLine := moneyutil.FormatDisplay(data.Amount, data.Currency)

	return EmailContent{
		Subject: fmt.Sprintf("Your %s subscription has been renewed", premiumName),
		HTML: fmt.Sprintf(`
			<h2>Subscription Renewed Successfully</h2>
			<p>Hi %s,</p>
			<p>Your %s subscription has been automatically renewed. Thank you for your continued support!</p>
			<h3>Renewal Details:</h3>
			<ul>
				<li><strong>Subscription ID:</strong> %s</li>
				<li><strong>Amount Charged:</strong> %s</li>
				<li><strong>New Period:</strong> %s to %s</li>
				<li><strong>Transaction ID:</strong> %s</li>
			</ul>
			<p>Your access continues uninterrupted.</p>
			<p>The %s Team</p>
		`, data.Username, premiumName, data.SubscriptionID, amountLine,
			data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.TransactionID, storeName),
		Plain: fmt.Sprintf(`
			Subscription Renewed Successfully

			Hi %s,

			Your %s subscription has been automatically renewed. Thank you for continued support!

			Renewal Details:
			- Subscription ID: %s
			- Amount Charged: %s
			- New Period: %s to %s
			- Transaction ID: %s

			Your access continues uninterrupted.

			The %s Team
		`, data.Username, premiumName, data.SubscriptionID, amountLine,
			data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.TransactionID, storeName),
	}
}

func RenderSubscriptionCancellationEmail(storeName string, data SubscriptionEmailData, reason PremiumEndReason) EmailContent {
	periodEnd := data.PeriodEnd.Format("Jan 2, 2006")
	premiumName := subscriptionProductName(storeName, data.ProductName)
	subject := fmt.Sprintf("Your %s subscription has been cancelled", premiumName)
	reasonBlurb := "We've cancelled your membership as requested."
	footer := "You can resubscribe at any time. We'd love to see you back!"

	switch reason {
	case PremiumEndReasonChargeback:
		subject = fmt.Sprintf("Your %s subscription has been terminated", premiumName)
		reasonBlurb = "We received a dispute on your most recent payment, so the membership has been closed for now."
		footer = "If this was unexpected, please reach out to support so we can help restore your access."
	case PremiumEndReasonRefund:
		subject = fmt.Sprintf("Your %s subscription was refunded", premiumName)
		reasonBlurb = "We've processed your refund and closed the associated premium membership."
	case PremiumEndReasonAdmin:
		reasonBlurb = "Our support team closed this subscription."
	case PremiumEndReasonProcessor:
		reasonBlurb = "Your payment provider confirmed this cancellation, so we've closed the membership."
	}

	return EmailContent{
		Subject: subject,
		HTML: fmt.Sprintf(`
			<h2>%s</h2>
			<p>Hi %s,</p>
			<p>%s</p>
			<ul>
				<li><strong>Subscription ID:</strong> %s</li>
				<li><strong>Premium access available until:</strong> %s</li>
			</ul>
			<p>You'll continue to enjoy premium access until %s.</p>
			<p>%s</p>
			<p>The %s Team</p>
		`, subject, data.Username, reasonBlurb, data.SubscriptionID, periodEnd, periodEnd, footer, storeName),
		Plain: fmt.Sprintf(`
			%s

			Hi %s,

			%s

			Subscription ID: %s
			Premium access available until: %s

			You'll continue to enjoy premium access until %s.

			%s
			The %s Team
		`, subject, data.Username, reasonBlurb, data.SubscriptionID, periodEnd, periodEnd, footer, storeName),
	}
}

func RenderSubscriptionExpiredEmail(storeName, customerPortalURL string, data SubscriptionEmailData) EmailContent {
	periodEnd := data.PeriodEnd.Format("Jan 2, 2006")
	premiumName := subscriptionProductName(storeName, data.ProductName)
	linkHTML := ""
	linkText := ""
	if customerPortalURL != "" {
		linkHTML = fmt.Sprintf(`<p><a href="%s" style="display:inline-block;padding:10px 18px;background:#6c4ad0;color:#ffffff;text-decoration:none;border-radius:4px;">Manage billing settings</a></p>`, customerPortalURL)
		linkText = fmt.Sprintf("\n\t\tUpdate your payment method anytime to restart your membership: %s\n", customerPortalURL)
	}

	return EmailContent{
		Subject: fmt.Sprintf("Your %s access has expired", premiumName),
		HTML: fmt.Sprintf(`
			<h2>Your Premium Access Has Expired</h2>
			<p>Hi %s,</p>
			<p>We tried to renew your %s subscription several times but couldn't complete the payment. Your access ended on <strong>%s</strong>.</p>
			<p>If you'd like to jump back in, update your payment method and restart your membership any time.</p>
			%s
			<p>The %s Team</p>
		`, data.Username, premiumName, periodEnd, linkHTML, storeName),
		Plain: fmt.Sprintf(`
			Your Premium Access Has Expired

			Hi %s,

			We tried to renew your %s subscription several times but couldn't complete the payment. Your access ended on %s.

			%s

			The %s Team
		`, data.Username, premiumName, periodEnd, strings.TrimSpace(linkText), storeName),
	}
}

func RenderPaymentFailedEmail(storeName, customerPortalURL string, data SubscriptionEmailData) EmailContent {
	amountLine := moneyutil.FormatDisplay(data.Amount, data.Currency)
	premiumName := subscriptionProductName(storeName, data.ProductName)
	linkHTML := ""
	linkText := ""
	if customerPortalURL != "" {
		linkHTML = fmt.Sprintf(`<p><a href="%s" style="display:inline-block;padding:10px 18px;background:#6c4ad0;color:#ffffff;text-decoration:none;border-radius:4px;">Update payment method</a></p>`, customerPortalURL)
		linkText = fmt.Sprintf("Update your payment details here to avoid losing access: %s", customerPortalURL)
	}

	return EmailContent{
		Subject: fmt.Sprintf("We couldn't renew your %s subscription", premiumName),
		HTML: fmt.Sprintf(`
			<h2>Payment Attempt Unsuccessful</h2>
			<p>Hi %s,</p>
			<p>We just tried to renew your %s subscription but the payment didn't go through.</p>
			<ul>
				<li><strong>Subscription ID:</strong> %s</li>
				<li><strong>Amount:</strong> %s</li>
				<li><strong>Payment method:</strong> %s</li>
			</ul>
			<p>Your premium access stays active while we retry automatically. To be safe, please take a moment to update your payment details.</p>
			%s
			<p>If payment continues to fail, your membership will expire.</p>
			<p>The %s Team</p>
		`, data.Username, premiumName, data.SubscriptionID, amountLine, data.PaymentMethod, linkHTML, storeName),
		Plain: fmt.Sprintf(`
			We couldn't renew your %s subscription

			Hi %s,

			We just tried to renew your %s subscription but the payment didn't go through.
			Subscription ID: %s
			Amount: %s
			Payment method: %s

			Your premium stays active while we retry automatically. %s

			If payment continues to fail, your membership will expire.

			The %s Team
		`, premiumName, data.Username, premiumName, data.SubscriptionID, amountLine, data.PaymentMethod, linkText, storeName),
	}
}

func subscriptionProductName(storeName, productName string) string {
	premiumName := strings.TrimSpace(productName)
	if premiumName == "" {
		premiumName = storeName + " Premium"
	}
	return premiumName
}
