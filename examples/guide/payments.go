package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// RefundInFull returns a payment through its PSP and ends the access it bought.
func RefundInFull(ctx context.Context, shop *openrails.Client, payment billing.PaymentID, key string) (*billing.Payment, error) {
	// guide:refund
	return shop.RefundPayment(ctx, payment, billing.RefundPaymentParams{
		IdempotencyKey: key, // one per refund decision; a retry with it refunds once
		Full:           true,
		Reason:         "requested_by_customer",
		RevokeAccess:   true,
	})
	// guide:end
}
