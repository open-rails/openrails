package subscriptions

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// The notices of a usage order, of arrears delinquency and of a scheduled
// price change. Each is rendered from the notice's own data; billingURL is
// the merchant's page where the customer pays, changes a card or cancels.
var _ = template.Must(emailHTML.Parse(`
{{define "order_due"}}
<h2>Your bill: {{.Amount}}</h2>
<p>Hi {{.Username}},</p>
{{if .Automatic}}<p>We'll charge <strong>{{.Amount}}</strong> for your usage to your card on file on <strong>{{.Due}}</strong>. You don't need to do anything.</p>
{{else if .Invoice}}<p>Your bill of <strong>{{.Amount}}</strong> for your usage is due on <strong>{{.Due}}</strong>. Please pay it by then.</p>
{{template "button" .Button}}
{{else}}<p>Your bill of <strong>{{.Amount}}</strong> for your usage is due on <strong>{{.Due}}</strong>. View your billing account for payment details.</p>
{{template "button" .Button}}{{end}}
{{if .Automatic}}{{template "how_to_cancel" .}}{{end}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "order_overdue"}}
<h2>Your bill is overdue</h2>
<p>Hi {{.Username}},</p>
<p>Your bill of <strong>{{.Amount}}</strong> was due on <strong>{{.Due}}</strong> and is still unpaid. Please pay it to keep using {{.Store}} without interruption.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "order_card_failed"}}
<h2>We couldn't charge your card</h2>
<p>Hi {{.Username}},</p>
<p>We tried to charge <strong>{{.Amount}}</strong> for your usage to your card on file, but the payment didn't go through.</p>
<p>{{if .Next}}We'll try again on <strong>{{.Next}}</strong>. {{end}}To be safe, please check or update your card.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "order_card_update"}}
<h2>Your card needs updating</h2>
<p>Hi {{.Username}},</p>
<p>Your bank declined the card on file for your usage bill of <strong>{{.Amount}}</strong>, and it won't work if we try again, so we've stopped charging it.</p>
<p>Add or update a card and pay the bill to settle it.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "order_collection_stopped"}}
<h2>We stopped charging your card</h2>
<p>Hi {{.Username}},</p>
<p>{{.Reason}}</p>
<p><strong>{{.Amount}}</strong> for your usage is still owed. Please pay it with another card.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "account_delinquent"}}
<h2>Your account is past due</h2>
<p>Hi {{.Username}},</p>
<p>{{.Amount}} for your usage has been overdue since <strong>{{.Since}}</strong>. Until it is paid, new usage is paused.</p>
{{template "button" .Button}}
<p>The {{.Store}} Team</p>
{{end}}

{{define "account_delinquency_cleared"}}
<h2>Your account is in good standing</h2>
<p>Hi {{.Username}},</p>
<p>Thank you: your overdue bills are settled, and your usage is no longer paused.</p>
<p>The {{.Store}} Team</p>
{{end}}

{{define "price_change"}}
<h2>{{.Title}}</h2>
<p>Hi {{.Username}},</p>
<p>{{.Change}}</p>
<ul>
	<li><strong>From:</strong> {{.Old}}</li>
	<li><strong>To:</strong> {{.New}}</li>
	<li><strong>Starting:</strong> {{.Effective}}</li>
</ul>
<p>Nothing changes before then. If you'd rather not continue, cancel before {{.Effective}}.</p>
{{template "how_to_cancel" .}}
<p>The {{.Store}} Team</p>
{{end}}
`))

func noticeName(username string) string {
	if name := strings.TrimSpace(username); name != "" {
		return name
	}
	return "there"
}

func noticeDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("January 2, 2006")
}

func noticeAmount(amount *int64, currency string) string {
	if amount == nil {
		return ""
	}
	return moneyutil.FormatAmount(*amount, currency)
}

// plainNotice is a notice's plain-text body: its lines, then the sign-off.
func plainNotice(title, username string, lines []string, storeName string) string {
	return title + "\n\nHi " + username + ",\n\n" + strings.Join(lines, "\n\n") + "\n\nThe " + storeName + " Team\n"
}

func linkLine(label, url string) string {
	if url == "" {
		return ""
	}
	return label + ": " + url
}

func nonEmpty(lines ...string) []string {
	out := lines[:0]
	for _, line := range lines {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// RenderOrderDueEmail is a billed usage order: charged to the card on file on
// its due date (with how to cancel that agreement), or the customer pays it.
func RenderOrderDueEmail(storeName, billingURL, username string, data billing.NotificationData) EmailContent {
	name, amount, due := noticeName(username), noticeAmount(data.AmountDue, data.Currency), noticeDate(data.DueAt)
	automatic := data.CollectionMethod == billing.CollectChargeAutomatically
	invoiced := data.CollectionMethod == billing.CollectSendInvoice
	button := emailButton{billingURL, "View bill"}
	const what = "charges for your usage"
	var lines []string
	if automatic {
		lines = []string{fmt.Sprintf("We'll charge %s for your usage to your card on file on %s. You don't need to do anything.", amount, due),
			cancelPlain(storeName, billingURL, what)}
	} else if invoiced {
		button.Label = "Pay now"
		lines = nonEmpty(fmt.Sprintf("Your bill of %s for your usage is due on %s. Please pay it by then.", amount, due), linkLine("Pay it here", billingURL))
	} else {
		// Older queued notices have no collection method. Do not promise a
		// card charge or ask for manual payment when the notice cannot say.
		lines = nonEmpty(fmt.Sprintf("Your bill of %s for your usage is due on %s. View your billing account for payment details.", amount, due), linkLine("View your bill", billingURL))
	}
	return EmailContent{
		Subject: fmt.Sprintf("Your %s bill: %s due %s", storeName, amount, due),
		HTML: renderEmailHTML("order_due", emailFields{"Username": name, "Amount": amount, "Due": due, "Automatic": automatic, "Invoice": invoiced,
			"Button": button, "CancelURL": billingURL, "CancelWhat": what, "Store": storeName}),
		Plain: plainNotice("Your bill: "+amount, name, lines, storeName),
	}
}

// RenderOrderOverdueEmail is a usage order still owed past its due date.
func RenderOrderOverdueEmail(storeName, billingURL, username string, data billing.NotificationData) EmailContent {
	name, amount, due := noticeName(username), noticeAmount(data.AmountDue, data.Currency), noticeDate(data.DueAt)
	return EmailContent{
		Subject: fmt.Sprintf("Your %s bill is overdue", storeName),
		HTML: renderEmailHTML("order_overdue", emailFields{"Username": name, "Amount": amount, "Due": due,
			"Button": emailButton{billingURL, "Pay now"}, "Store": storeName}),
		Plain: plainNotice("Your bill is overdue", name, nonEmpty(
			fmt.Sprintf("Your bill of %s was due on %s and is still unpaid. Please pay it to keep using %s without interruption.", amount, due, storeName),
			linkLine("Pay it here", billingURL)), storeName),
	}
}

// RenderOrderCardFailedEmail is a declined charge of a usage order that
// collection retries.
func RenderOrderCardFailedEmail(storeName, billingURL, username string, data billing.NotificationData) EmailContent {
	name, amount, next := noticeName(username), noticeAmount(data.AmountDue, data.Currency), noticeDate(data.NextAttemptAt)
	retry := "To be safe, please check or update your card."
	if next != "" {
		retry = fmt.Sprintf("We'll try again on %s. %s", next, retry)
	}
	return EmailContent{
		Subject: fmt.Sprintf("We couldn't charge your card for your %s bill", storeName),
		HTML: renderEmailHTML("order_card_failed", emailFields{"Username": name, "Amount": amount, "Next": next,
			"Button": emailButton{billingURL, "Update payment method"}, "Store": storeName}),
		Plain: plainNotice("We couldn't charge your card", name, nonEmpty(
			fmt.Sprintf("We tried to charge %s for your usage to your card on file, but the payment didn't go through.", amount),
			retry, linkLine("Update your card here", billingURL)), storeName),
	}
}

// RenderOrderCardUpdateEmail is a usage order's card the issuer says will
// not work again: charging it stopped until the customer acts.
func RenderOrderCardUpdateEmail(storeName, billingURL, username string, data billing.NotificationData) EmailContent {
	name, amount := noticeName(username), noticeAmount(data.AmountDue, data.Currency)
	return EmailContent{
		Subject: fmt.Sprintf("Please update your card for your %s bill", storeName),
		HTML: renderEmailHTML("order_card_update", emailFields{"Username": name, "Amount": amount,
			"Button": emailButton{billingURL, "Update payment method"}, "Store": storeName}),
		Plain: plainNotice("Your card needs updating", name, nonEmpty(
			fmt.Sprintf("Your bank declined the card on file for your usage bill of %s, and it won't work if we try again, so we've stopped charging it.", amount),
			"Add or update a card and pay the bill to settle it.", linkLine("Update your card here", billingURL)), storeName),
	}
}

// RenderOrderCollectionStoppedEmail is a usage order whose collection ended:
// the retries ran out, or the issuer withdrew the card. It is still owed.
func RenderOrderCollectionStoppedEmail(storeName, billingURL, username string, data billing.NotificationData) EmailContent {
	name, amount := noticeName(username), noticeAmount(data.AmountDue, data.Currency)
	reason := "We tried to charge your card on file several times and couldn't, so we've stopped trying."
	if data.Reason == "non_recoverable" {
		reason = "Your bank told us your card on file can no longer be charged, so we've stopped trying."
	}
	return EmailContent{
		Subject: fmt.Sprintf("We stopped charging your card for your %s bill", storeName),
		HTML: renderEmailHTML("order_collection_stopped", emailFields{"Username": name, "Reason": reason, "Amount": amount,
			"Button": emailButton{billingURL, "Pay now"}, "Store": storeName}),
		Plain: plainNotice("We stopped charging your card", name, nonEmpty(reason,
			fmt.Sprintf("%s for your usage is still owed. Please pay it with another card.", amount), linkLine("Pay it here", billingURL)), storeName),
	}
}

// RenderAccountDelinquentEmail is a customer past the merchant's grace: new
// usage in the currency is paused until the overdue bills are paid.
func RenderAccountDelinquentEmail(storeName, billingURL, username string, data billing.NotificationData) EmailContent {
	name, amount, since := noticeName(username), noticeAmount(data.OverdueAmount, data.Currency), noticeDate(data.OverdueStartedAt)
	return EmailContent{
		Subject: fmt.Sprintf("Your %s account is past due", storeName),
		HTML: renderEmailHTML("account_delinquent", emailFields{"Username": name, "Amount": amount, "Since": since,
			"Button": emailButton{billingURL, "Pay now"}, "Store": storeName}),
		Plain: plainNotice("Your account is past due", name, nonEmpty(
			fmt.Sprintf("%s for your usage has been overdue since %s. Until it is paid, new usage is paused.", amount, since),
			linkLine("Pay it here", billingURL)), storeName),
	}
}

// RenderAccountDelinquencyClearedEmail is a delinquent customer whose overdue
// bills are settled.
func RenderAccountDelinquencyClearedEmail(storeName, username string) EmailContent {
	name := noticeName(username)
	return EmailContent{
		Subject: fmt.Sprintf("Your %s account is in good standing", storeName),
		HTML:    renderEmailHTML("account_delinquency_cleared", emailFields{"Username": name, "Store": storeName}),
		Plain: plainNotice("Your account is in good standing", name,
			[]string{"Thank you: your overdue bills are settled, and your usage is no longer paused."}, storeName),
	}
}

// RenderPriceChangeEmail is the advance notice of a subscription's scheduled
// price change, or of its move to another plan (toProduct set), with how to
// cancel the subscription before it applies.
func RenderPriceChangeEmail(storeName, billingURL, username, product string, data billing.NotificationData) EmailContent {
	name, effective := noticeName(username), noticeDate(data.EffectiveAt)
	product = subscriptionProductName(storeName, product)
	old, next := noticeAmount(data.OldAmount, data.Currency), noticeAmount(data.NewAmount, data.Currency)
	title := fmt.Sprintf("Your %s price is changing", product)
	change := fmt.Sprintf("The price of your %s subscription changes on %s.", product, effective)
	fromLine, toLine := old, next
	if data.ToProductName != "" {
		title = fmt.Sprintf("Your %s plan is changing", product)
		change = fmt.Sprintf("Your %s subscription moves to %s on %s.", product, data.ToProductName, effective)
		fromLine, toLine = product+", "+old, data.ToProductName+", "+next
	}
	const what = "this subscription"
	return EmailContent{
		Subject: title,
		HTML: renderEmailHTML("price_change", emailFields{"Title": title, "Username": name, "Change": change, "Old": fromLine, "New": toLine,
			"Effective": effective, "CancelURL": billingURL, "CancelWhat": what, "Store": storeName}),
		Plain: plainNotice(title, name, []string{change,
			fmt.Sprintf("- From: %s\n- To: %s\n- Starting: %s", fromLine, toLine, effective),
			fmt.Sprintf("Nothing changes before then. If you'd rather not continue, cancel before %s.", effective),
			cancelPlain(storeName, billingURL, what)}, storeName),
	}
}
