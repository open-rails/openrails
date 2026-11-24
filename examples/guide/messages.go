package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// CheckReceipt renders a receipt template before it is saved, sends it to
// the merchant's alert address, and reads a customer's delivery log.
func CheckReceipt(ctx context.Context, shop *openrails.Client, customer billing.CustomerID) error {
	// guide:messages-check
	candidate := &billing.MessageTemplate{
		Subject: "Your {{.Merchant.Name}} receipt",
		Text:    "You paid {{.Event.Amount}} for {{.Event.ProductName}}.",
	}
	preview, err := shop.PreviewMessage(ctx, billing.PreviewMessageParams{EventType: "one_off_purchase_completed", Template: candidate})
	if err != nil {
		return err // 400 message_template_invalid names what does not render
	}
	_ = preview.Subject
	if _, err := shop.CreateTestMessage(ctx, billing.CreateTestMessageParams{EventType: "one_off_purchase_completed", Template: candidate}); err != nil {
		return err // sent to alert_email only, ten an hour
	}
	log, err := shop.ListCustomerMessages(ctx, customer, billing.CustomerMessageListParams{})
	if err != nil {
		return err
	}
	for _, m := range log.Items { // newest first; no body
		_, _, _ = m.EventType, m.Status, m.LastError
	}
	return nil
	// guide:end
}
