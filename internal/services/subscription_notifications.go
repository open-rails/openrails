package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func (s *EmailService) storeName() string {
	if s == nil || s.store == nil {
		return "My Store"
	}
	name := strings.TrimSpace(s.store.Name)
	if name == "" {
		return "My Store"
	}
	return name
}

func (s *EmailService) storeCustomerPortalURL() string {
	if s == nil || s.store == nil {
		return ""
	}
	return strings.TrimSpace(s.store.CustomerPortalURL)
}

// SendSubscriptionConfirmation sends a confirmation email when subscription is created
func (s *EmailService) SendSubscriptionConfirmation(ctx context.Context, data subscriptions.SubscriptionEmailData) error {
	storeName := s.storeName()
	premiumName := strings.TrimSpace(data.ProductName)
	if premiumName == "" {
		premiumName = storeName + " Premium"
	}

	subject := fmt.Sprintf("Welcome to %s! Your subscription is confirmed", premiumName)
	amountLine := moneyutil.FormatDisplay(data.Amount, data.Currency)

	htmlContent := fmt.Sprintf(`
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
		data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.PaymentMethod, storeName)

	plainContent := fmt.Sprintf(`
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
		data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.PaymentMethod, storeName)

	return s.SendEmail(ctx, data.UserEmail, subject, htmlContent, plainContent)
}

// SendSubscriptionRenewal sends notification when subscription is renewed
func (s *EmailService) SendSubscriptionRenewal(ctx context.Context, data subscriptions.SubscriptionEmailData) error {
	storeName := s.storeName()
	premiumName := strings.TrimSpace(data.ProductName)
	if premiumName == "" {
		premiumName = storeName + " Premium"
	}

	subject := fmt.Sprintf("Your %s subscription has been renewed", premiumName)
	amountLine := moneyutil.FormatDisplay(data.Amount, data.Currency)

	htmlContent := fmt.Sprintf(`
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
		data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.TransactionID, storeName)

	plainContent := fmt.Sprintf(`
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
		data.PeriodStart.Format("Jan 2, 2006"), data.PeriodEnd.Format("Jan 2, 2006"), data.TransactionID, storeName)

	return s.SendEmail(ctx, data.UserEmail, subject, htmlContent, plainContent)
}

// SendSubscriptionCancellation sends notification when subscription is cancelled
func (s *EmailService) SendSubscriptionCancellation(ctx context.Context, data subscriptions.SubscriptionEmailData, reason subscriptions.PremiumEndReason) error {
	periodEnd := data.PeriodEnd.Format("Jan 2, 2006")
	storeName := s.storeName()
	premiumName := strings.TrimSpace(data.ProductName)
	if premiumName == "" {
		premiumName = storeName + " Premium"
	}

	subject := fmt.Sprintf("Your %s subscription has been cancelled", premiumName)
	reasonBlurb := "We've cancelled your membership as requested."
	footer := "You can resubscribe at any time. We'd love to see you back!"

	switch reason {
	case subscriptions.PremiumEndReasonChargeback:
		subject = fmt.Sprintf("Your %s subscription has been terminated", premiumName)
		reasonBlurb = "We received a dispute on your most recent payment, so the membership has been closed for now."
		footer = "If this was unexpected, please reach out to support so we can help restore your access."
	case subscriptions.PremiumEndReasonRefund:
		subject = fmt.Sprintf("Your %s subscription was refunded", premiumName)
		reasonBlurb = "We've processed your refund and closed the associated premium membership."
	case subscriptions.PremiumEndReasonAdmin:
		subject = fmt.Sprintf("Your %s subscription was cancelled", premiumName)
		reasonBlurb = "Our support team closed this subscription."
	case subscriptions.PremiumEndReasonProcessor:
		subject = fmt.Sprintf("Your %s subscription was cancelled", premiumName)
		reasonBlurb = "Your payment provider confirmed this cancellation, so we’ve closed the membership."
	}

	htmlContent := fmt.Sprintf(`
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
		`, subject, data.Username, reasonBlurb, data.SubscriptionID, periodEnd, periodEnd, footer, storeName)

	plainContent := fmt.Sprintf(`
		%s
		
		Hi %s,
		
		%s
		
		Subscription ID: %s
		Premium access available until: %s
		
		You'll continue to enjoy premium access until %s.
		
			%s
			The %s Team
		`, subject, data.Username, reasonBlurb, data.SubscriptionID, periodEnd, periodEnd, footer, storeName)

	return s.SendEmail(ctx, data.UserEmail, subject, htmlContent, plainContent)
}

func (s *EmailService) SendSubscriptionExpired(ctx context.Context, data subscriptions.SubscriptionEmailData) error {
	periodEnd := data.PeriodEnd.Format("Jan 2, 2006")
	storeName := s.storeName()
	premiumName := strings.TrimSpace(data.ProductName)
	if premiumName == "" {
		premiumName = storeName + " Premium"
	}
	customerPortalURL := s.storeCustomerPortalURL()
	subject := fmt.Sprintf("Your %s access has expired", premiumName)

	linkHTML := ""
	linkText := ""
	if customerPortalURL != "" {
		linkHTML = fmt.Sprintf(`<p><a href="%s" style="display:inline-block;padding:10px 18px;background:#6c4ad0;color:#ffffff;text-decoration:none;border-radius:4px;">Manage billing settings</a></p>`, customerPortalURL)
		linkText = fmt.Sprintf("\n\t\tUpdate your payment method anytime to restart your membership: %s\n", customerPortalURL)
	}

	htmlContent := fmt.Sprintf(`
		<h2>Your Premium Access Has Expired</h2>
		<p>Hi %s,</p>
		<p>We tried to renew your %s subscription several times but couldn’t complete the payment. Your access ended on <strong>%s</strong>.</p>
		<p>If you’d like to jump back in, update your payment method and restart your membership any time.</p>
		%s
		<p>The %s Team</p>
	`, data.Username, premiumName, periodEnd, linkHTML, storeName)

	plainContent := fmt.Sprintf(`
		Your Premium Access Has Expired
		
		Hi %s,
		
		We tried to renew your %s subscription several times but couldn’t complete the payment. Your access ended on %s.
		
		%s
		
		The %s Team
	`, data.Username, premiumName, periodEnd, strings.TrimSpace(linkText), storeName)

	return s.SendEmail(ctx, data.UserEmail, subject, htmlContent, plainContent)
}

// sendPaymentFailed sends notification when subscription payment fails (internal, takes SubscriptionEmailData)
func (s *EmailService) sendPaymentFailed(ctx context.Context, data subscriptions.SubscriptionEmailData) error {
	amountLine := moneyutil.FormatDisplay(data.Amount, data.Currency)
	storeName := s.storeName()
	premiumName := strings.TrimSpace(data.ProductName)
	if premiumName == "" {
		premiumName = storeName + " Premium"
	}
	customerPortalURL := s.storeCustomerPortalURL()
	subject := fmt.Sprintf("We couldn’t renew your %s subscription", premiumName)

	linkHTML := ""
	linkText := ""
	if customerPortalURL != "" {
		linkHTML = fmt.Sprintf(`<p><a href="%s" style="display:inline-block;padding:10px 18px;background:#6c4ad0;color:#ffffff;text-decoration:none;border-radius:4px;">Update payment method</a></p>`, customerPortalURL)
		linkText = fmt.Sprintf("Update your payment details here to avoid losing access: %s", customerPortalURL)
	}

	htmlContent := fmt.Sprintf(`
		<h2>Payment Attempt Unsuccessful</h2>
		<p>Hi %s,</p>
		<p>We just tried to renew your %s subscription but the payment didn’t go through.</p>
		<ul>
			<li><strong>Subscription ID:</strong> %s</li>
			<li><strong>Amount:</strong> %s</li>
			<li><strong>Payment method:</strong> %s</li>
		</ul>
		<p>Your premium access stays active while we retry automatically. To be safe, please take a moment to update your payment details.</p>
		%s
		<p>If payment continues to fail, your membership will expire.</p>
		<p>The %s Team</p>
	`, data.Username, premiumName, data.SubscriptionID, amountLine, data.PaymentMethod, linkHTML, storeName)

	plainContent := fmt.Sprintf(`
		We couldn’t renew your %s subscription
		
		Hi %s,
		
		We just tried to renew your %s subscription but the payment didn’t go through.
		Subscription ID: %s
		Amount: %s
		Payment method: %s
		
		Your premium stays active while we retry automatically. %s
		
		If payment continues to fail, your membership will expire.
		
		The %s Team
	`, premiumName, data.Username, premiumName, data.SubscriptionID, amountLine, data.PaymentMethod, linkText, storeName)

	return s.SendEmail(ctx, data.UserEmail, subject, htmlContent, plainContent)
}

// SendEntitlementExpiration sends notification when an entitlement is expiring soon
func (s *EmailService) SendEntitlementExpiration(ctx context.Context, userEmail, username, entitlementName string, expiresAt time.Time) error {
	subject := fmt.Sprintf("Your %s access expires soon", entitlementName)
	storeName := s.storeName()

	daysUntilExpiry := int(time.Until(expiresAt).Hours() / 24)

	htmlContent := fmt.Sprintf(`
		<h2>Access Expiring Soon</h2>
		<p>Hi %s,</p>
		<p>This is a reminder that your <strong>%s</strong> access will expire in %d days on <strong>%s</strong>.</p>
		
		<p>To continue enjoying premium features, please renew your subscription before the expiration date.</p>
		<p>Thank you for being a valued member!</p>
		<p>The %s Team</p>
    `, username, entitlementName, daysUntilExpiry, expiresAt.Format("January 2, 2006"), storeName)

	plainContent := fmt.Sprintf(`
		Access Expiring Soon
		
		Hi %s,
		
		Your %s access will expire in %d days on %s.
		
		To continue enjoying premium features, please renew your subscription before the expiration date.
		
		Thank you for being a valued member!
		The %s Team
	    `, username, entitlementName, daysUntilExpiry, expiresAt.Format("January 2, 2006"), storeName)

	return s.SendEmail(ctx, userEmail, subject, htmlContent, plainContent)
}
