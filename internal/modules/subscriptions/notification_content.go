package subscriptions

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// periodInstant renders a boundary of this period (with time of day for
// periods shorter than two days).
func (d SubscriptionEmailData) periodInstant(t time.Time) string {
	return cadence.FormatInstant(t, d.PeriodEnd.Sub(d.PeriodStart))
}

type EmailContent struct {
	Subject string
	HTML    string
	Plain   string
}

// emailHTML holds every HTML body. html/template escapes each value by context
// (text, attribute, URL), so customer and merchant text cannot inject markup.
var emailHTML = template.Must(template.New("email").Option("missingkey=error").Parse(`
{{define "button"}}{{if .URL}}<p><a href="{{.URL}}" style="display:inline-block;padding:10px 18px;background:#6c4ad0;color:#ffffff;text-decoration:none;border-radius:4px;">{{.Label}}</a></p>{{end}}{{end}}

{{define "confirmation"}}
<h2>Welcome to {{.Premium}}!</h2>
<p>Hi {{.Username}},</p>
<p>Your subscription has been successfully activated. Thank you for your support!</p>
<h3>Subscription Details:</h3>
<ul>
	<li><strong>Subscription ID:</strong> {{.SubscriptionID}}</li>
	<li><strong>Amount:</strong> {{.Amount}}</li>
	<li><strong>Current Period:</strong> {{.PeriodStart}} to {{.PeriodEnd}}</li>
	<li><strong>Payment Method:</strong> {{.PaymentMethod}}</li>
</ul>
<p>Enjoy!</p>
<p>The {{.Store}} Team</p>
{{end}}

{{define "renewal"}}
<h2>Subscription Renewed Successfully</h2>
<p>Hi {{.Username}},</p>
<p>Your {{.Premium}} subscription has been automatically renewed. Thank you for your continued support!</p>
<h3>Renewal Details:</h3>
<ul>
	<li><strong>Subscription ID:</strong> {{.SubscriptionID}}</li>
	<li><strong>Amount Charged:</strong> {{.Amount}}</li>
	<li><strong>New Period:</strong> {{.PeriodStart}} to {{.PeriodEnd}}</li>
	<li><strong>Transaction ID:</strong> {{.TransactionID}}</li>
</ul>
<p>Your access continues uninterrupted.</p>
<p>The {{.Store}} Team</p>
{{end}}

{{define "cancellation"}}
<h2>{{.Subject}}</h2>
<p>Hi {{.Username}},</p>
<p>{{.Reason}}</p>
<ul>
	<li><strong>Subscription ID:</strong> {{.SubscriptionID}}</li>
	<li><strong>Premium access available until:</strong> {{.PeriodEnd}}</li>
</ul>
<p>You'll continue to enjoy premium access until {{.PeriodEnd}}.</p>
<p>{{.Footer}}</p>
<p>The {{.Store}} Team</p>
{{end}}

{{define "expired"}}
<h2>Your Premium Access Has Expired</h2>
<p>Hi {{.Username}},</p>
<p>We tried to renew your {{.Premium}} subscription several times but couldn't complete the payment. Your access ended on <strong>{{.PeriodEnd}}</strong>.</p>
<p>If you'd like to jump back in, update your payment method and restart your membership any time.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "access_ended"}}
<h2>Your Premium Access Has Ended</h2>
<p>Hi {{.Username}},</p>
<p>Your {{.Store}} premium access ended on <strong>{{.EndedOn}}</strong>.</p>
<p>To keep enjoying premium, sign up again — we'd love to have you back.</p>
{{template "button" .Button}}
<p>If you have any questions, just reply to this email.</p>
<p>The {{.Store}} Team</p>
{{end}}

{{define "payment_failed"}}
<h2>Payment Attempt Unsuccessful</h2>
<p>Hi {{.Username}},</p>
<p>We just tried to renew your {{.Premium}} subscription but the payment didn't go through.</p>
<ul>
	<li><strong>Subscription ID:</strong> {{.SubscriptionID}}</li>
	<li><strong>Amount:</strong> {{.Amount}}</li>
	<li><strong>Payment method:</strong> {{.PaymentMethod}}</li>
</ul>
<p>Your premium access stays active while we retry automatically. To be safe, please take a moment to update your payment details.</p>
{{template "button" .Button}}
<p>If payment continues to fail, your membership will expire.</p>
<p>The {{.Store}} Team</p>
{{end}}

{{define "method_update_required"}}
<h2>Your Payment Method Needs Updating</h2>
<p>Hi {{.Username}},</p>
<p>Your bank declined the card saved for your {{.Premium}} subscription, and it won't work on future renewals — so we've paused charging it rather than trying again.</p>
<ul>
	<li><strong>Subscription ID:</strong> {{.SubscriptionID}}</li>
	<li><strong>Payment method:</strong> {{.PaymentMethod}}</li>
</ul>
<p><strong>Your access is still active.</strong> Add or update a card and your subscription carries on as normal. If nothing changes, the subscription will eventually end.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "non_recoverable"}}
<h2>Your Subscription Has Ended</h2>
<p>Hi {{.Username}},</p>
<p>Your bank told us the card on your {{.Premium}} subscription can no longer be used for recurring payments, so we've ended the subscription rather than keep trying.</p>
<ul>
	<li><strong>Subscription ID:</strong> {{.SubscriptionID}}</li>
</ul>
<p>Nothing was changed about the payment methods saved to your account — you can review or remove them yourself at any time.</p>
<p>When you're ready, start a new subscription with a different card and you're back in.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "entitlement_expiring"}}
<h2>Access Expiring Soon</h2>
<p>Hi {{.Username}},</p>
<p>This is a reminder that your <strong>{{.Entitlement}}</strong> access will expire in {{.Remaining}} on <strong>{{.ExpiresOn}}</strong>.</p>
<p>To continue enjoying premium features, please renew your subscription before the expiration date.</p>
<p>Thank you for being a valued member!</p>
<p>The {{.Store}} Team</p>
{{end}}

{{define "purchase_receipt"}}
<h2>{{if .Solana}}Solana Payment Received{{else}}Payment Received{{end}}</h2>
<p>Hi there,</p>
<p>{{.Intro}}{{if .Solana}} This one-time Solana transaction instantly extended your premium access.{{end}}</p>
<ul>
	<li><strong>Product:</strong> {{.Product}}</li>
	<li><strong>Amount:</strong> {{.Amount}}</li>
	<li><strong>Date:</strong> {{.Date}}</li>
</ul>
<p>{{if .Solana}}Enjoy your premium benefits; no rebill will occur automatically.{{else}}Your access has been updated instantly. Enjoy!{{end}}</p>
<p>The {{.Store}} Team</p>
{{end}}
`))

type emailFields = map[string]any

type emailButton struct{ URL, Label string }

// renderEmailHTML executes a template parsed above; a failure is a programming
// error in this file, caught by the render tests.
func renderEmailHTML(name string, fields emailFields) string {
	var b strings.Builder
	if err := emailHTML.ExecuteTemplate(&b, name, fields); err != nil {
		panic(fmt.Sprintf("email template %s: %v", name, err))
	}
	return b.String()
}

func RenderSubscriptionConfirmationEmail(storeName string, data SubscriptionEmailData) EmailContent {
	premiumName := subscriptionProductName(storeName, data.ProductName)
	amountLine := moneyutil.FormatAmount(data.Amount, data.Currency)

	return EmailContent{
		Subject: fmt.Sprintf("Welcome to %s! Your subscription is confirmed", premiumName),
		HTML: renderEmailHTML("confirmation", emailFields{"Premium": premiumName, "Username": data.Username, "SubscriptionID": data.SubscriptionID, "Amount": amountLine,
			"PeriodStart": data.periodInstant(data.PeriodStart), "PeriodEnd": data.periodInstant(data.PeriodEnd), "PaymentMethod": data.PaymentMethod, "Store": storeName}),
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
			data.periodInstant(data.PeriodStart), data.periodInstant(data.PeriodEnd), data.PaymentMethod, storeName),
	}
}

func RenderSubscriptionRenewalEmail(storeName string, data SubscriptionEmailData) EmailContent {
	premiumName := subscriptionProductName(storeName, data.ProductName)
	amountLine := moneyutil.FormatAmount(data.Amount, data.Currency)

	return EmailContent{
		Subject: fmt.Sprintf("Your %s subscription has been renewed", premiumName),
		HTML: renderEmailHTML("renewal", emailFields{"Premium": premiumName, "Username": data.Username, "SubscriptionID": data.SubscriptionID, "Amount": amountLine,
			"PeriodStart": data.periodInstant(data.PeriodStart), "PeriodEnd": data.periodInstant(data.PeriodEnd), "TransactionID": data.TransactionID, "Store": storeName}),
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
			data.periodInstant(data.PeriodStart), data.periodInstant(data.PeriodEnd), data.TransactionID, storeName),
	}
}

func RenderSubscriptionCancellationEmail(storeName string, data SubscriptionEmailData, reason PremiumEndReason) EmailContent {
	periodEnd := data.periodInstant(data.PeriodEnd)
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
	case PremiumEndReasonRail:
		reasonBlurb = "Your payment provider confirmed this cancellation, so we've closed the membership."
	}

	return EmailContent{
		Subject: subject,
		HTML: renderEmailHTML("cancellation", emailFields{"Subject": subject, "Username": data.Username, "Reason": reasonBlurb, "SubscriptionID": data.SubscriptionID,
			"PeriodEnd": periodEnd, "Footer": footer, "Store": storeName}),
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
	periodEnd := data.periodInstant(data.PeriodEnd)
	premiumName := subscriptionProductName(storeName, data.ProductName)
	linkText := ""
	if customerPortalURL != "" {
		linkText = fmt.Sprintf("\n\t\tUpdate your payment method anytime to restart your membership: %s\n", customerPortalURL)
	}

	return EmailContent{
		Subject: fmt.Sprintf("Your %s access has expired", premiumName),
		HTML: renderEmailHTML("expired", emailFields{"Username": data.Username, "Premium": premiumName, "PeriodEnd": periodEnd,
			"Button": emailButton{customerPortalURL, "Manage billing settings"}, "Store": storeName}),
		Plain: fmt.Sprintf(`
			Your Premium Access Has Expired

			Hi %s,

			We tried to renew your %s subscription several times but couldn't complete the payment. Your access ended on %s.

			%s

			The %s Team
		`, data.Username, premiumName, periodEnd, strings.TrimSpace(linkText), storeName),
	}
}

// RenderAccessEndedEmail (#789) tells a customer their premium access ended.
// Neutral copy — these are often long-lapsed users, so no "we tried to charge
// you" language. A non-empty signupURL renders a "Sign up again" CTA.
func RenderAccessEndedEmail(storeName, signupURL, username string, endedAt time.Time) EmailContent {
	endedOn := endedAt.Format("Jan 2, 2006")
	name := strings.TrimSpace(username)
	if name == "" {
		name = "there"
	}
	linkText := ""
	if signupURL != "" {
		linkText = fmt.Sprintf("Sign up again any time: %s", signupURL)
	}

	return EmailContent{
		Subject: fmt.Sprintf("Your %s premium access has ended", storeName),
		HTML:    renderEmailHTML("access_ended", emailFields{"Username": name, "EndedOn": endedOn, "Button": emailButton{signupURL, "Sign up again"}, "Store": storeName}),
		Plain: fmt.Sprintf(`
			Your Premium Access Has Ended

			Hi %s,

			Your %s premium access ended on %s.

			To keep enjoying premium, sign up again — we'd love to have you back.

			%s

			If you have any questions, just reply to this email.
			The %s Team
		`, name, storeName, endedOn, linkText, storeName),
	}
}

func RenderPaymentFailedEmail(storeName, customerPortalURL string, data SubscriptionEmailData) EmailContent {
	amountLine := moneyutil.FormatAmount(data.Amount, data.Currency)
	premiumName := subscriptionProductName(storeName, data.ProductName)
	linkText := ""
	if customerPortalURL != "" {
		linkText = fmt.Sprintf("Update your payment details here to avoid losing access: %s", customerPortalURL)
	}

	return EmailContent{
		Subject: fmt.Sprintf("We couldn't renew your %s subscription", premiumName),
		HTML: renderEmailHTML("payment_failed", emailFields{"Username": data.Username, "Premium": premiumName, "SubscriptionID": data.SubscriptionID, "Amount": amountLine,
			"PaymentMethod": data.PaymentMethod, "Button": emailButton{customerPortalURL, "Update payment method"}, "Store": storeName}),
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

// RenderPaymentMethodUpdateRequiredEmail is the or#870 BUCKET 2 notice: the
// rail told us this card cannot be charged (expired, bad CVC, call issuer...),
// so we STOPPED charging it. The subscription and the customer's access are
// still live — they just need to update the card. The one email in the ladder
// that is a recovery opportunity rather than an apology or a goodbye.
func RenderPaymentMethodUpdateRequiredEmail(storeName, customerPortalURL string, data SubscriptionEmailData) EmailContent {
	premiumName := subscriptionProductName(storeName, data.ProductName)
	linkText := ""
	if customerPortalURL != "" {
		linkText = fmt.Sprintf("Update your payment method here: %s", customerPortalURL)
	}

	return EmailContent{
		Subject: fmt.Sprintf("Please update the payment method for your %s subscription", premiumName),
		HTML: renderEmailHTML("method_update_required", emailFields{"Username": data.Username, "Premium": premiumName, "SubscriptionID": data.SubscriptionID,
			"PaymentMethod": data.PaymentMethod, "Button": emailButton{customerPortalURL, "Update payment method"}, "Store": storeName}),
		Plain: fmt.Sprintf(`
			Please update the payment method for your %s subscription

			Hi %s,

			Your bank declined the card saved for your %s subscription, and it won't work on
			future renewals — so we've paused charging it rather than trying again.

			Subscription ID: %s
			Payment method: %s

			Your access is still active. Add or update a card and your subscription carries
			on as normal. If nothing changes, the subscription will eventually end.

			%s

			The %s Team
		`, premiumName, data.Username, premiumName, data.SubscriptionID, data.PaymentMethod, linkText, storeName),
	}
}

// RenderSubscriptionNonRecoverableEmail is the or#870 BUCKET 3 goodbye: the
// issuer withdrew the recurring mandate (NMI 261/262, Stripe
// revocation_of_authorization) or the instrument is permanently dead
// (pick-up/lost/stolen/fraudulent). We cancelled the schedule at the rail. We
// did NOT touch their stored payment method — only they delete that — and they
// are welcome to re-subscribe.
func RenderSubscriptionNonRecoverableEmail(storeName, checkoutURL string, data SubscriptionEmailData) EmailContent {
	premiumName := subscriptionProductName(storeName, data.ProductName)
	linkText := ""
	if checkoutURL != "" {
		linkText = fmt.Sprintf("Re-subscribe any time: %s", checkoutURL)
	}

	return EmailContent{
		Subject: fmt.Sprintf("Your %s subscription has ended", premiumName),
		HTML: renderEmailHTML("non_recoverable", emailFields{"Username": data.Username, "Premium": premiumName, "SubscriptionID": data.SubscriptionID,
			"Button": emailButton{checkoutURL, "Re-subscribe"}, "Store": storeName}),
		Plain: fmt.Sprintf(`
			Your %s subscription has ended

			Hi %s,

			Your bank told us the card on your %s subscription can no longer be used for
			recurring payments, so we've ended the subscription rather than keep trying.

			Subscription ID: %s

			Nothing was changed about the payment methods saved to your account — you can
			review or remove them yourself at any time.

			When you're ready, start a new subscription with a different card and you're back in.

			%s

			The %s Team
		`, premiumName, data.Username, premiumName, data.SubscriptionID, linkText, storeName),
	}
}
