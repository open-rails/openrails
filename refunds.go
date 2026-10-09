package openrails

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// RefundPayment refunds a charge through its rail. The returned refund has
// Status "succeeded" once the provider confirmed it, or "pending" while the
// durable refund operation is parked (provider write gates) or reconciling an
// uncertain provider outcome; it settles without another call. A provider
// refusal is a StatusError and leaves no refund recorded.
func (c *Client) RefundPayment(ctx context.Context, id billing.PaymentID, params billing.RefundPaymentParams, requestOptions ...RequestOption) (*billing.Payment, error) {
	payment, err := requireTypedID("payment_id", id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	if params.Full == (params.Amount != 0) || params.Amount < 0 {
		return nil, invalidErr("exactly one of a positive amount or full is required")
	}
	var out billing.Payment
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/admin/payments/"+payment+"/refunds", params, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ArchiveProduct archives a product (never deletes it) and, per the caller's
// policy, refunds or holds for review its recent one-time purchases. A
// purchase held for review is a finding: ResolveFinding refunds it (approve)
// or keeps it (ignore).
func (c *Client) ArchiveProduct(ctx context.Context, params billing.ArchiveProductParams, requestOptions ...RequestOption) (*billing.ProductArchive, error) {
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	var out billing.ProductArchive
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/admin/catalog/product-archives", params, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
