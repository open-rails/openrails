package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// RefundPayment refunds a charge through its rail. The returned refund has
// Status "succeeded" once the provider confirmed it, or "pending" while the
// durable refund operation is parked (provider write gates) or reconciling an
// uncertain provider outcome; it settles without another call. A provider
// refusal is a StatusError and leaves no refund recorded. idempotencyKey is
// required: a retry with it returns the same refund and never refunds twice;
// the same key with other terms is ErrIdempotencyKeyReused.
func (c *Client) RefundPayment(ctx context.Context, idempotencyKey string, id billing.PaymentID, params billing.RefundPaymentParams) (*billing.Payment, error) {
	payment, err := requireTypedID("payment_id", id)
	if err != nil {
		return nil, err
	}
	if params.Full == (params.Amount != 0) || params.Amount < 0 {
		return nil, invalidErr("exactly one of a positive amount or full is required")
	}
	var out billing.Payment
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/admin/payments/"+payment+"/refunds", params, &out, idempotent(idempotencyKey)); err != nil {
		return nil, err
	}
	return &out, nil
}
