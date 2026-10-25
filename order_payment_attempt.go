package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// GetOrderPaymentAttempt reads one retained purchase payment attempt by its
// original Idempotency-Key. It never retries or performs a provider write.
func (c *Client) GetOrderPaymentAttempt(ctx context.Context, id billing.OrderID, key string, requestOptions ...RequestOption) (*billing.OrderPaymentAttempt, error) {
	order, err := requireTypedID("order_id", id)
	if err != nil {
		return nil, err
	}
	key, err = requiredIdempotencyKey(key, requestOptions)
	if err != nil {
		return nil, err
	}
	var out billing.OrderPaymentAttempt
	if err = c.doWithHeaders(ctx, http.MethodGet, "/v1/app/orders/"+order+"/payment-attempt", nil, &out, http.Header{"Idempotency-Key": {key}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
