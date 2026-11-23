package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// GetOrderPaymentAttempt reads one retained purchase payment attempt by
// idempotencyKey, its original POST /me/orders/{id}/pay Idempotency-Key. It
// does not look up create-order-with-payment or renewal/usage keys, retry or
// perform a provider write.
func (c *Client) GetOrderPaymentAttempt(ctx context.Context, idempotencyKey string, id billing.OrderID) (*billing.OrderPaymentAttempt, error) {
	order, err := requireTypedID("order_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.OrderPaymentAttempt
	if err = c.doWithHeaders(ctx, http.MethodGet, "/v1/app/orders/"+order+"/payment-attempt", nil, &out, idempotent(idempotencyKey)); err != nil {
		return nil, err
	}
	return &out, nil
}
