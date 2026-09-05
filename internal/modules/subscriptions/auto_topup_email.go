package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"html"
)

// SendAutoTopupDisabled uses the ordinary customer directory and email worker.
// It does not require a subscription: prepaid balances may be standalone.
func (s *EmailService) SendAutoTopupDisabled(ctx context.Context, customerID, currency string) error {
	username, email, err := s.getUserEmail(ctx, customerID)
	if errors.Is(err, errUserEmailUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	if email == "" {
		return nil
	}
	content := RenderAutoTopupDisabledEmail(s.storeName(ctx), username, currency)
	return s.SendEmail(ctx, email, content.Subject, content.HTML, content.Plain)
}

func RenderAutoTopupDisabledEmail(storeName, username, currency string) EmailContent {
	plain := fmt.Sprintf("Hi %s,\n\nAutomatic top-ups for your %s balance have been disabled after repeated payment declines. Your existing balance is unchanged. Review your saved payment method and explicitly enable automatic top-ups when you are ready.\n\n%s", username, currency, storeName)
	return EmailContent{Subject: "Automatic top-ups disabled", Plain: plain, HTML: "<p>" + html.EscapeString(plain) + "</p>"}
}
